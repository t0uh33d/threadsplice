package ui

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/t0uh33d/threadsplice/internal/api"
	"github.com/t0uh33d/threadsplice/internal/client"
	"github.com/t0uh33d/threadsplice/internal/e2ee"
)

// rawHTTP sends a hand-written request, for what net/http's client won't
// send (no Host, HTTP/1.0, odd paths), and returns the response's status line
// and headers.
func rawHTTP(t *testing.T, addr, req string) (status string, header http.Header) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	fmt.Fprint(c, req)
	br := bufio.NewReader(c)
	status, _ = br.ReadString('\n')
	header = http.Header{}
	for {
		l, err := br.ReadString('\n')
		if err != nil || strings.TrimSpace(l) == "" {
			break
		}
		k, v, _ := strings.Cut(strings.TrimSpace(l), ":")
		header.Add(k, strings.TrimSpace(v))
	}
	return strings.TrimSpace(status), header
}

func TestHostVariants(t *testing.T) {
	e := newEnv(t)
	addr := strings.TrimPrefix(e.ui, "http://")
	_, port, _ := net.SplitHostPort(addr)
	// DNS rebinding and near misses: only the exact loopback names pass.
	for _, h := range []string{"localhost.:" + port, "127.0.0.1.:" + port, "0.0.0.0:" + port, "[::ffff:127.0.0.1]:" + port,
		"127.1:" + port, "[0:0:0:0:0:0:0:1]:" + port, "127.0.0.1", "localhost", "127.0.0.1:0" + port, "evil.example",
		"evil.example:" + port, "127.0.0.1.nip.io:" + port, "user@127.0.0.1:" + port} {
		if r := e.do(http.MethodGet, "/", nil, map[string]string{"Host": h}); r.code != http.StatusForbidden {
			t.Errorf("Host %q: %d", h, r.code)
		}
	}
	for _, h := range []string{"LOCALHOST:" + port, "[::1]:" + port} {
		if r := e.do(http.MethodGet, "/", nil, map[string]string{"Host": h}); r.code == http.StatusForbidden {
			t.Errorf("Host %q refused", h)
		}
	}
	// No Host at all (HTTP/1.0), or an empty one.
	for _, req := range []string{"GET / HTTP/1.0\r\nCookie: ts_ui=" + e.token + "\r\n\r\n",
		"GET / HTTP/1.1\r\nHost: \r\nCookie: ts_ui=" + e.token + "\r\nConnection: close\r\n\r\n"} {
		if st, _ := rawHTTP(t, addr, req); !strings.Contains(st, " 403 ") {
			t.Errorf("%q: %s", req, st)
		}
	}
	// Absolute-form request targets: Go takes Host from the URL.
	if st, _ := rawHTTP(t, addr, "GET http://evil.example/ HTTP/1.1\r\nHost: "+addr+"\r\nCookie: ts_ui="+e.token+"\r\nConnection: close\r\n\r\n"); !strings.Contains(st, " 403 ") {
		t.Errorf("absolute-form foreign URL: %s", st)
	}
}

// On port 80 browsers send Host without a port; those must pass, others not.
func TestHostPort80(t *testing.T) {
	h, err := New(Options{Token: "0123456789abcdef0123", Addr: "127.0.0.1:80"})
	if err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]int{"127.0.0.1": 401, "localhost": 401, "[::1]": 401, "127.0.0.1:80": 401, "evil.example": 403, "localhost:8080": 403} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("Host %q on port 80: %d, want %d", host, w.Code, want)
		}
	}
}

func TestCheckAddrVariants(t *testing.T) {
	for addr, ok := range map[string]bool{"[::ffff:127.0.0.1]:4848": true, "127.255.255.254:4848": true,
		"localhost.:4848": false, "LOCALHOST:4848": false, "[::1%lo0]:4848": false, "[fe80::1]:4848": false, "0177.0.0.1:4848": false,
		"127.0.0.1:4848:1": false, "[::]:0": false, "0.0.0.0:0": false, "169.254.1.1:4848": false} {
		if err := CheckAddr(addr); (err == nil) != ok {
			t.Errorf("CheckAddr(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}

type fakeListener struct{ addr net.Addr }

func (l fakeListener) Accept() (net.Conn, error) { select {} }
func (l fakeListener) Close() error              { return nil }
func (l fakeListener) Addr() net.Addr            { return l.addr }

// Serve itself refuses a listener on a network interface, whoever calls it.
func TestServeRefusesNonLoopback(t *testing.T) {
	ln := fakeListener{&net.TCPAddr{IP: net.ParseIP("192.168.1.5"), Port: 4848}}
	errc := make(chan error, 1)
	go func() { errc <- Serve(context.Background(), ln, Options{Token: "0123456789abcdef0123"}) }()
	select {
	case err := <-errc:
		if err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Fatalf("Serve on 192.168.1.5: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve is serving on 192.168.1.5")
	}
}

func TestTokenHandling(t *testing.T) {
	e := newEnv(t)
	addr := strings.TrimPrefix(e.ui, "http://")
	get := func(path, hdr string) (string, http.Header) {
		return rawHTTP(t, addr, "GET "+path+" HTTP/1.1\r\nHost: "+addr+"\r\n"+hdr+"Connection: close\r\n\r\n")
	}
	// Cookie tossing: a page on another localhost port can plant extra ts_ui
	// cookies with longer paths, which browsers send first.
	if st, _ := get("/r/x", "Cookie: ts_ui=planted; ts_ui="+e.token+"\r\n"); !strings.Contains(st, " 404 ") {
		t.Errorf("planted cookie first: %s", st)
	}
	// Nothing else stands in for the cookie.
	for _, hdr := range []string{"Authorization: Bearer " + e.token + "\r\n", "Cookie: ts=" + e.token + "\r\n",
		"Cookie: ts_ui=" + e.token[:len(e.token)-1] + "\r\n", "Cookie: ts_ui=" + strings.ToLower(e.token) + "\r\n", "X-Token: " + e.token + "\r\n"} {
		if st, _ := get("/", hdr); !strings.Contains(st, " 401 ") {
			t.Errorf("%q: %s", hdr, st)
		}
	}
	// The link only works as a GET, and never leaves the token in a Location.
	for _, m := range []string{"HEAD", "POST"} {
		st, h := rawHTTP(t, addr, m+" /?t="+e.token+" HTTP/1.1\r\nHost: "+addr+"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		if !strings.Contains(st, " 401 ") || h.Get("Set-Cookie") != "" {
			t.Errorf("%s with the link: %s %v", m, st, h)
		}
	}
	for _, p := range []string{"/?t=" + e.token, "//evil.example/?t=" + e.token, "/%5Cevil.example?t=" + e.token,
		"/r/x?t=" + e.token + "&t=" + e.token, "/setup/claude?t=" + e.token + "&as=x%0d%0aSet-Cookie:a=b"} {
		st, h := get(p, "")
		loc := h.Get("Location")
		if !strings.Contains(st, " 303 ") || strings.Contains(loc, e.token) || !strings.HasPrefix(loc, "/") || strings.HasPrefix(loc, "//") ||
			strings.ContainsAny(loc, "\\\r\n") || len(h.Values("Set-Cookie")) != 1 {
			t.Errorf("link %q: %s Location %q Set-Cookie %q", p, st, loc, h.Values("Set-Cookie"))
		}
	}
	// No page ever repeats the token.
	for _, b := range e.bodies {
		if strings.Contains(b, e.token) {
			t.Fatalf("a response body contains the access token:\n%s", b)
		}
	}
}

// The -open link works once, signs the browser in with the real token, and
// doesn't let anything else in.
func TestLaunchToken(t *testing.T) {
	const token, launch = "TOKENTOKENTOKENTOKEN", "LAUNCHLAUNCHLAUNCHLA"
	h, err := New(Options{Token: token, Launch: launch, Addr: "127.0.0.1:4848"})
	if err != nil {
		t.Fatal(err)
	}
	try := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Host = "127.0.0.1:4848"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := try(http.MethodGet, "/?t=WRONGWRONGWRONGWRONG"); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong launch token: %d", w.Code)
	}
	if w := try(http.MethodHead, "/?t="+launch); w.Code != http.StatusUnauthorized {
		t.Fatalf("HEAD spent the launch token: %d", w.Code)
	}
	w := try(http.MethodGet, "/?t="+launch)
	if c := w.Result().Cookies(); w.Code != http.StatusSeeOther || len(c) != 1 || c[0].Value != token {
		t.Fatalf("launch link: %d %v", w.Code, c)
	}
	if w := try(http.MethodGet, "/?t="+launch); w.Code != http.StatusUnauthorized {
		t.Fatalf("launch token worked twice: %d", w.Code)
	}
	if w := try(http.MethodGet, "/?t="+token); w.Code != http.StatusSeeOther {
		t.Fatalf("printed link after the launch: %d", w.Code)
	}
	if _, err := New(Options{Token: token, Launch: "short", Addr: "127.0.0.1:4848"}); err == nil {
		t.Fatal("short launch token accepted")
	}
}

func TestFetchMetadata(t *testing.T) {
	e := newEnv(t)
	// Another origin (another localhost port is the same site) may only
	// navigate a tab here: no frames, embeds, prefetches or fetches.
	for _, h := range []map[string]string{
		{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "iframe"},
		{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "embed"},
		{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "object"},
		{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document", "Sec-Purpose": "prefetch;prerender"},
		{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate"},
		{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "image"},
		{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "no-cors", "Sec-Fetch-Dest": "script"},
		{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"},
	} {
		if r := e.do(http.MethodGet, "/", nil, h); r.code != http.StatusForbidden {
			t.Errorf("GET %v: %d", h, r.code)
		}
	}
	for _, h := range []map[string]string{
		{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"},
		{"Sec-Fetch-Site": "none", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"},
		{"Sec-Fetch-Site": "same-origin", "Sec-Fetch-Mode": "cors", "Sec-Fetch-Dest": "empty"},
	} {
		if r := e.do(http.MethodGet, "/", nil, h); r.code != http.StatusOK {
			t.Errorf("GET %v: %d", h, r.code)
		}
	}
	// Posts from elsewhere, including a top-level form post and an opaque origin.
	form := url.Values{"invite": {"x"}, "name": {"bob"}}
	for _, h := range []map[string]string{
		{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"},
		{"Sec-Fetch-Site": "", "Origin": "null"},
		{"Sec-Fetch-Site": "", "Origin": "http://localhost:1"},
	} {
		if r := e.do(http.MethodPost, "/join", form, h); r.code != http.StatusForbidden {
			t.Errorf("POST %v: %d", h, r.code)
		}
	}
	// Every response, refusals included, forbids framing and keeps its opener out.
	for _, r := range []resp{e.get("/"), e.do(http.MethodGet, "/", nil, map[string]string{"Host": "evil.example"}),
		e.do(http.MethodGet, "/", nil, map[string]string{"Cookie": ""})} {
		if r.h.Get("Cross-Origin-Opener-Policy") != "same-origin" || r.h.Get("X-Frame-Options") != "DENY" ||
			!strings.Contains(r.h.Get("Content-Security-Policy"), "frame-ancestors 'none'") {
			t.Errorf("%d: framing/opener headers missing: %v", r.code, r.h)
		}
	}
}

// Nothing changes state over GET, and only urlencoded bodies are read.
func TestStateChangesNeedPost(t *testing.T) {
	e := newEnv(t)
	r := e.do(http.MethodPost, "/rooms", url.Values{"relay": {e.relay}, "room": {"proj"}, "name": {"alice"}, "admin": {admin}, "ttl": {"none"}}, nil)
	if r.code != http.StatusSeeOther {
		t.Fatalf("create: %d", r.code)
	}
	for _, p := range []string{"/r/alice/send?to=&kind=ask&text=hi", "/r/alice/invite?ttl=1h", "/r/alice/revoke?pid=x"} {
		if r := e.get(p); r.code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s: %d", p, r.code)
		}
	}
	// The old one-click "add an agent on this machine" is gone for good.
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		if r := e.do(m, "/r/alice/agents", url.Values{"name": {"mallory"}}, map[string]string{"HX-Request": "true"}); r.code != http.StatusNotFound && r.code != http.StatusMethodNotAllowed {
			t.Errorf("%s /r/alice/agents: %d", m, r.code)
		}
	}
	// Query parameters never stand in for the form.
	must(t, e.hx(http.MethodPost, "/r/alice/send?kind=message&text=mallory", url.Values{}), http.StatusUnprocessableEntity, "Write something")
	for _, ct := range []string{"text/plain", "multipart/form-data; boundary=B", "application/json"} {
		body := "kind=message&text=mallory"
		if strings.HasPrefix(ct, "multipart") {
			body = "--B\r\nContent-Disposition: form-data; name=\"text\"\r\n\r\nmallory\r\n--B--\r\n"
		}
		req, _ := http.NewRequest(http.MethodPost, e.ui+"/r/alice/send", strings.NewReader(body))
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
		req.Header.Set("HX-Request", "true")
		res, err := e.c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("POST as %s: %d", ct, res.StatusCode)
		}
	}
	if names, _ := client.List(); len(names) != 1 {
		t.Fatalf("identities after GETs and odd bodies: %v", names)
	}
	a := agent(t, "alice")
	if ps, err := a.Peers(context.Background()); err != nil || len(ps) != 1 {
		t.Fatalf("participants after GETs and odd bodies: %v %v", ps, err)
	}
	if msgs, _, err := a.Log(context.Background(), 0, 0); err != nil || len(msgs) != 0 {
		t.Fatalf("messages after GETs and odd bodies: %v %v", msgs, err)
	}
}

func TestTraversal(t *testing.T) {
	e := newEnv(t)
	addr := strings.TrimPrefix(e.ui, "http://")
	if err := os.WriteFile(filepath.Join(os.Getenv("THREADSPLICE_HOME"), "secret.json"), []byte(`{"token":"leak"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/static/..%2Fui.go", "/static/%2e%2e%2fui.go", "/static/htmx.LICENSE", "/static/", "/static/ui.js%00.css",
		"/static/..%2Ftemplates%2Flayout.html", "/templates/layout.html", "/STATIC/ui.js", "/r/..%2Fsecret", "/r/%2e%2e", "/r/..",
		"/r/ALICE", "/r/secret", "/setup/..%2Fclaude", "/setup/claude?as=..%2Fsecret", "/setup/claude?as=secret",
		"/r/..%2Fsecret/connect", "/r/secret/invites", "/r/secret/sessions"} {
		st, _ := rawHTTP(t, addr, "GET "+p+" HTTP/1.1\r\nHost: "+addr+"\r\nCookie: ts_ui="+e.token+"\r\nConnection: close\r\n\r\n")
		if !strings.Contains(st, " 404 ") && !strings.Contains(st, " 307 ") && !strings.Contains(st, " 301 ") {
			t.Errorf("GET %s: %s", p, st)
		}
	}
}

// hostileRelay answers the UI with attacker-chosen metadata everywhere it
// can: ids, names, states, room names, kinds, reply ids, invite tokens and
// error text, and echoes whatever bearer token it was sent.
func hostileRelay(t *testing.T, key e2ee.Key, room string) *httptest.Server {
	t.Helper()
	const bidi = "‮"
	evil := `X1"'><img src=x onerror=alert(1)><script>alert(2)</script> hx-get="/r/alice/agents" hx-trigger="load"`
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusInternalServerError, api.Error{Error: evil + " for " + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")})
	}
	mux.HandleFunc("GET /v1/rooms/{room}/participants", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusOK, []api.Participant{
			{ID: "ME", Name: "alice", Owner: true, Online: true, State: "idle"},
			{ID: evil + bidi, Name: evil + bidi, State: evil, Online: true, LastSeen: time.Now()},
			{ID: bidi + "BOB", Name: "bob", State: "busy"},
			// Session members: one genuine, the rest claiming machines they don't belong to.
			{ID: "KID1", Name: "alice.rev", Parent: "ME", Online: true, State: "busy"},
			{ID: "KID2", Name: evil + bidi, Parent: "ME", Online: true},
			{ID: "KID3", Name: "alice.x", Parent: evil + bidi},
			{ID: "KID4", Name: "bob.y", Parent: "ME"},
			{ID: "KID5", Name: "alice.rev.deep", Parent: "KID1"},
			{ID: "KID6", Name: "alice.self", Parent: "KID6"},
			{ID: "KID7", Name: "alice.gone", Parent: "NOBODY"},
		})
	})
	mux.HandleFunc("GET /v1/rooms/{room}/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("after") != "0" {
			reply(w, http.StatusOK, api.MessagesResponse{Messages: []api.Envelope{}})
			return
		}
		seal := func(from, to, id string, p api.Payload) []byte {
			b, _ := json.Marshal(p)
			return key.Seal(e2ee.Header{Room: room, From: from, To: to, ID: id}, b)
		}
		now := time.Now()
		reply(w, http.StatusOK, api.MessagesResponse{Messages: []api.Envelope{
			{Seq: 1, ID: evil + bidi, From: evil + bidi, To: evil, Sealed: []byte{1, 2, 3}, CreatedAt: now},
			{Seq: 2, ID: evil + bidi, From: bidi + "BOB", Sealed: seal(bidi+"BOB", "", evil+bidi, api.Payload{Kind: evil, Text: "decrypts fine", ReplyTo: evil, Depth: 3}), CreatedAt: now},
			{Seq: 3, ID: "OK1", From: "alice (you)" + bidi, To: "ME", Sealed: seal("alice (you)"+bidi, "ME", "OK1", api.Payload{Kind: api.KindReply, Text: "spoofed sender", ReplyTo: "OK0"}), CreatedAt: now},
			{Seq: 4, ID: "feed", From: "ME", Sealed: seal("ME", "", "feed", api.Payload{Kind: api.KindAsk, Text: "id collision"}), CreatedAt: now},
			{Seq: 5, ID: "K1", From: "KID1", Sealed: seal("KID1", "", "K1", api.Payload{Kind: api.KindReply, Text: "from a real session", ReplyTo: "feed"}), CreatedAt: now},
			{Seq: 6, ID: "K3", From: "KID3", Sealed: seal("KID3", "", "K3", api.Payload{Kind: api.KindMessage, Text: "from a fake session"}), CreatedAt: now},
			{Seq: 7, ID: "K4", From: "KID4", Sealed: seal("KID4", "", "K4", api.Payload{Kind: api.KindMessage, Text: "from a misnamed session"}), CreatedAt: now},
		}})
	})
	mux.HandleFunc("POST /v1/rooms/{room}/messages", fail)
	mux.HandleFunc("DELETE /v1/rooms/{room}/participants/{pid}", fail)
	mux.HandleFunc("POST /v1/rooms/{room}/invites", func(w http.ResponseWriter, r *http.Request) {
		reply(w, http.StatusCreated, api.Invite{Token: "abcdefghijklmnop'; touch /tmp/pwned #", ExpiresAt: time.Now().Add(time.Hour)})
	})
	mux.HandleFunc("POST /v1/join", func(w http.ResponseWriter, r *http.Request) {
		var req api.JoinRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Name == "echo" {
			reply(w, http.StatusForbidden, api.Error{Error: "spent invite " + req.Invite})
			return
		}
		reply(w, http.StatusCreated, api.Membership{RoomID: room, RoomName: "joined" + evil, ParticipantID: evil, Token: "JOINEDTOKEN0123456789"})
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

var (
	tmplTagRe = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9]*)`)
	tmplAttRe = regexp.MustCompile(`\s([a-zA-Z][a-zA-Z0-9:-]*)=`)
	tagRe     = regexp.MustCompile(`<([a-zA-Z][a-zA-Z0-9]*)((?:\s[^>]*)?)>`)
	attrRe    = regexp.MustCompile(`\s([^\s="'<>/]+)(?:="([^"]*)")?`)
)

// markup lists the elements and attributes the templates write themselves.
func markup(t *testing.T) (tags, attrs map[string]bool) {
	t.Helper()
	tags, attrs = map[string]bool{}, map[string]bool{"selected": true, "required": true, "readonly": true, "hidden": true, "autofocus": true, "defer": true, "checked": true,
		"data-copy": true, "data-close": true, "data-close-drawer": true, "data-drawer": true, "data-initial": true, "data-quote": true,
		"data-grow": true, "data-machine-name": true, "data-machine-echo": true, "": true}
	files, err := templateFiles.ReadDir("templates")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		b, _ := templateFiles.ReadFile("templates/" + f.Name())
		for _, m := range tmplTagRe.FindAllStringSubmatch(string(b), -1) {
			tags[strings.ToLower(m[1])] = true
		}
		for _, m := range tmplAttRe.FindAllStringSubmatch(string(b), -1) {
			attrs[strings.ToLower(m[1])] = true
		}
	}
	return tags, attrs
}

var htmxValue = map[string]*regexp.Regexp{
	"hx-get":     regexp.MustCompile(`^/r/[A-Za-z0-9._-]+/(feed\?after=\d+|participants|sessions)$`),
	"hx-post":    regexp.MustCompile(`^/r/[A-Za-z0-9._-]+/(send|invite|revoke)$`),
	"hx-trigger": regexp.MustCompile(`^(every 2s, ts-sent from:body|every 5s|every 3s)$`),
	"src":        regexp.MustCompile(`^/static/(htmx-2\.0\.11\.min|ui)\.js$`),
	"href":       regexp.MustCompile(`^(/[^:]*|#[A-Za-z][A-Za-z0-9_-]*|https://github\.com/t0uh33d/threadsplice/[A-Za-z0-9/._-]*)$`),
	// ui.js looks these up by id or compares them; they carry relay ids only after validation.
	"data-dialog": regexp.MustCompile(`^rm-[A-Za-z0-9_-]{1,64}$`),
	"data-re":     regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`),
	"data-g":      regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}\|[A-Za-z0-9_-]{0,64}$`),
	"data-t":      regexp.MustCompile(`^-?\d+$`),
}

// inert fails unless body, rendered from hostile input, holds only the
// markup the templates write, with htmx and URL attributes shaped as they
// write them, and no bidi override in its visible text.
func inert(t *testing.T, what, body string) {
	t.Helper()
	tags, attrs := markup(t)
	for _, m := range tagRe.FindAllStringSubmatch(body, -1) {
		if tag := strings.ToLower(m[1]); !tags[tag] {
			t.Errorf("%s: element <%s> the templates never write", what, tag)
		}
		for _, a := range attrRe.FindAllStringSubmatch(m[2], -1) {
			name := strings.ToLower(a[1])
			if !attrs[name] {
				t.Errorf("%s: attribute %q in <%s%s>", what, name, m[1], m[2])
			}
			if re, ok := htmxValue[name]; ok && !re.MatchString(a[2]) && !strings.HasPrefix(a[2], "/setup/") {
				t.Errorf("%s: %s=%q", what, name, a[2])
			}
		}
	}
	if text := tagRe.ReplaceAllString(body, " "); strings.ContainsRune(text, '‮') {
		t.Errorf("%s: a bidi override reached visible text", what)
	}
}

// TestHostileRelay renders everything a misbehaving relay controls and checks
// it stays inert, that sender labels and ids are not taken as-is, and that
// tokens it echoes back are not shown.
func TestHostileRelay(t *testing.T) {
	e := newEnv(t)
	key := e2ee.NewKey()
	hs := hostileRelay(t, key, "ROOM")
	const myToken = "MYBEARERTOKEN0123456789"
	if err := client.Save(client.Identity{Alias: "alice", Relay: hs.URL, RoomID: "ROOM", RoomName: `proj"><img src=x>`, ParticipantID: "ME",
		Name: "alice", Owner: true, Token: myToken, RoomKey: key.String()}); err != nil {
		t.Fatal(err)
	}
	var seen []resp
	check := func(what string, r resp) resp {
		t.Helper()
		inert(t, what, r.body)
		seen = append(seen, r)
		return r
	}
	page := check("room page", e.get("/r/alice"))
	must(t, page, http.StatusOK, "decrypts fine", "spoofed sender", "Couldn't decrypt this message", "id collision", `id="m-OK1"`, `id="m-feed"`)
	mustNot(t, page, "alice (you)", `id="m-X1`, `id="m-"`)
	feed := check("feed", e.hx(http.MethodGet, "/r/alice/feed?after=0", nil))
	must(t, feed, http.StatusOK, "unknown", `class="msg k-message"`, "depth 3",
		`id="m-feed" class="msg k-ask mine answered"`, "“id collision”", `<span class="on">on alice</span>`)
	if strings.Contains(feed.body, `href="#m-X1`) {
		t.Error("a hostile reply id became a link")
	}
	// Only the session member whose machine is real, and named for it, is shown as one.
	if n := strings.Count(feed.body, `class="on"`); n != 1 || strings.Count(feed.body, `class="tag agent"`) != 1 {
		t.Errorf("%d messages shown as from a session member, want 1", n)
	}
	members := check("participants", e.hx(http.MethodGet, "/r/alice/participants", nil))
	must(t, members, http.StatusOK, `data-sessions="1"`, `data-count="10"`)
	if strings.Count(members.body, `<ul class="kids"`) != 1 || !strings.Contains(members.body, `data-to="KID1" data-name="alice.rev"`) {
		t.Errorf("session members grouped wrong:\n%s", members.body)
	}
	sessions := check("sessions", e.hx(http.MethodGet, "/r/alice/sessions", nil))
	must(t, sessions, http.StatusOK, "alice.rev")
	mustNot(t, sessions, "alice.x", "bob.y", "alice.self", "alice.gone", "alice.rev.deep", "invalid-name")
	check("connect", e.get("/r/alice/connect"))
	check("invites", e.get("/r/alice/invites"))
	check("home", e.get("/"))
	check("setup", e.get("/setup/claude?as=alice"))

	// Relay errors are text, and the bearer token they echo is redacted.
	for _, c := range []struct{ path, form string }{{"/r/alice/send", "to=&kind=ask&text=x"}, {"/r/alice/revoke", "pid=BOB"}} {
		f, _ := url.ParseQuery(c.form)
		r := check(c.path, e.hx(http.MethodPost, c.path, f))
		must(t, r, http.StatusBadGateway, "for [redacted]")
	}
	// An invite token that would break out of the quoted join command is refused.
	r := check("invite", e.hx(http.MethodPost, "/r/alice/invite", url.Values{"ttl": {"1h"}}))
	must(t, r, http.StatusBadGateway, "malformed invite token")
	mustNot(t, r, "touch /tmp/pwned")

	// Joining through it: the room name it makes up stays text; an invite
	// token it echoes is redacted.
	inv := client.FormatInvite(hs.URL, "INVITETOKEN0123456789", key)
	r = check("join echo", e.do(http.MethodPost, "/join", url.Values{"invite": {inv}, "name": {"echo"}}, nil))
	must(t, r, http.StatusUnprocessableEntity, "spent invite [redacted]")
	r = e.do(http.MethodPost, "/join", url.Values{"invite": {inv}, "name": {"bob"}}, nil)
	if r.code != http.StatusSeeOther {
		t.Fatalf("join: %d\n%s", r.code, r.body)
	}
	check("joined room", e.get("/r/bob"))

	for _, r := range append(seen, e.bodiesAsResps()...) {
		for _, secret := range []string{myToken, "JOINEDTOKEN0123456789", "INVITETOKEN0123456789", key.String()} {
			if strings.Contains(r.body, secret) {
				t.Fatalf("a response shows a secret (%s):\n%s", secret[:6], r.body)
			}
		}
	}
}

func (e *env) bodiesAsResps() []resp {
	out := make([]resp, len(e.bodies))
	for i, b := range e.bodies {
		out[i] = resp{body: b}
	}
	return out
}

// A room's first invite shows on that room's page only, even if its local
// name is later reused for another room.
func TestFlashStaysWithItsRoom(t *testing.T) {
	e := newEnv(t)
	create := func(room, ttl string) {
		t.Helper()
		r := e.do(http.MethodPost, "/rooms", url.Values{"relay": {e.relay}, "room": {room}, "name": {"alice"}, "admin": {admin}, "ttl": {ttl}}, nil)
		if r.code != http.StatusSeeOther || r.h.Get("Location") != "/r/alice" {
			t.Fatalf("create %s: %d %q", room, r.code, r.h.Get("Location"))
		}
	}
	create("first", "24h") // its invite waits to be shown on /r/alice
	if err := os.Remove(filepath.Join(os.Getenv("THREADSPLICE_HOME"), "identities", "alice.json")); err != nil {
		t.Fatal(err)
	}
	create("second", "none")
	r := e.get("/r/alice")
	must(t, r, http.StatusOK, `<h1 id="room-title">second</h1>`)
	mustNot(t, r, "invite-value")
}

// Concurrent requests that save an identity under the same name each get
// their own file; none replaces another's (and so loses its token).
func TestSaveNewConcurrent(t *testing.T) {
	t.Setenv("THREADSPLICE_HOME", t.TempDir())
	key := e2ee.NewKey().String()
	aliases := make([]string, 8)
	var wg sync.WaitGroup
	for i := range aliases {
		wg.Go(func() {
			id, err := saveNew(client.Identity{Relay: "https://relay.example", RoomID: "R", RoomName: "proj", ParticipantID: fmt.Sprint("P", i),
				Name: "alice", Token: fmt.Sprint("T", i), RoomKey: key})
			if err != nil {
				t.Error(err)
			}
			aliases[i] = id.Alias
		})
	}
	wg.Wait()
	seen := map[string]bool{}
	for i, a := range aliases {
		if id, err := client.Load(a); seen[a] || err != nil || id.Token != fmt.Sprint("T", i) {
			t.Fatalf("identity %d saved as %q (twice: %v): %+v %v", i, a, seen[a], id, err)
		}
		seen[a] = true
	}
	// A name that differs only in case is taken too: on macOS and Windows it
	// is the same file.
	if err := client.Save(client.Identity{Alias: "Bob", Relay: "https://relay.example", RoomID: "R", Name: "Bob", RoomKey: key}); err != nil {
		t.Fatal(err)
	}
	if id, err := saveNew(client.Identity{Relay: "https://relay.example", RoomID: "R2", RoomName: "proj", Name: "bob", RoomKey: key}); err != nil || id.Alias != "bob-proj" {
		t.Fatalf("bob next to Bob: %q %v", id.Alias, err)
	}
}
