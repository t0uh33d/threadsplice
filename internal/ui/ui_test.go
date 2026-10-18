package ui

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"html"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/t0uh33d/threadsplice/internal/api"
	"github.com/t0uh33d/threadsplice/internal/client"
	"github.com/t0uh33d/threadsplice/internal/relay"
)

const admin = "test-admin-token-0123456789"

// env is a real relay plus the UI on loopback, in a fresh THREADSPLICE_HOME,
// and a browser-like client that has opened the UI's link.
type env struct {
	t      *testing.T
	relay  string
	ui     string
	token  string
	c      *http.Client
	bodies []string // every UI response body, for the secret checks
}

func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func newEnv(t *testing.T) *env {
	t.Helper()
	t.Setenv("THREADSPLICE_HOME", t.TempDir())
	t.Setenv("THREADSPLICE_RELAY", "")
	rs, err := relay.New(relay.Config{DBPath: filepath.Join(t.TempDir(), "relay.db"), AdminToken: admin, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	rts := httptest.NewServer(rs.Handler())
	t.Cleanup(func() { rts.Close(); rs.Close() })

	e := &env{t: t, relay: rts.URL, token: rand.Text()}
	uts := httptest.NewUnstartedServer(nil)
	h, err := New(Options{Token: e.token, Addr: uts.Listener.Addr().String(), Version: "test"})
	if err != nil {
		t.Fatal(err)
	}
	uts.Config.Handler = h
	uts.Start()
	t.Cleanup(uts.Close)
	e.ui = uts.URL
	jar, _ := cookiejar.New(nil)
	e.c = &http.Client{Jar: jar, CheckRedirect: noRedirect}
	if r := e.get("/?t=" + e.token); r.code != http.StatusSeeOther {
		t.Fatalf("opening the link: %d", r.code)
	}
	return e
}

type resp struct {
	code int
	body string
	h    http.Header
}

// do sends a request the way the signed-in browser would; hdr overrides
// headers (an empty value removes one) and "Host".
func (e *env) do(method, path string, form url.Values, hdr map[string]string) resp {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, e.ui+path, body)
	if err != nil {
		e.t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method != http.MethodGet {
		req.Header.Set("Origin", e.ui)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	for k, v := range hdr {
		switch {
		case k == "Host":
			req.Host = v
		case v == "":
			req.Header.Del(k)
		default:
			req.Header.Set(k, v)
		}
	}
	r, err := e.c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	e.bodies = append(e.bodies, string(b))
	return resp{r.StatusCode, string(b), r.Header}
}

func (e *env) get(path string) resp { return e.do(http.MethodGet, path, nil, nil) }

// hx sends an htmx request.
func (e *env) hx(method, path string, form url.Values) resp {
	return e.do(method, path, form, map[string]string{"HX-Request": "true"})
}

func must(t *testing.T, r resp, code int, want ...string) {
	t.Helper()
	if r.code != code {
		t.Fatalf("status %d, want %d\n%s", r.code, code, r.body)
	}
	for _, w := range want {
		if !strings.Contains(r.body, w) {
			t.Errorf("response lacks %q\n%s", w, r.body)
		}
	}
}

func mustNot(t *testing.T, r resp, bad ...string) {
	t.Helper()
	for _, b := range bad {
		if strings.Contains(r.body, b) {
			t.Errorf("response contains %q\n%s", b, r.body)
		}
	}
}

var inviteRe = regexp.MustCompile(`id="invite-value"[^>]*value="([^"]+)"`)

func inviteIn(t *testing.T, body string) string {
	t.Helper()
	m := inviteRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no invite in\n%s", body)
	}
	return html.UnescapeString(m[1])
}

func agent(t *testing.T, alias string) *client.Agent {
	t.Helper()
	a, err := client.OpenAgent(alias)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func checkHeaders(t *testing.T, h http.Header) {
	t.Helper()
	for k, v := range map[string]string{"Content-Security-Policy": csp, "X-Content-Type-Options": "nosniff",
		"Referrer-Policy": "no-referrer", "Cache-Control": "no-store"} {
		if got := h.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

func TestAccessControl(t *testing.T) {
	e := newEnv(t)
	anon := &http.Client{CheckRedirect: noRedirect}
	anonGet := func(path string) (*http.Response, string) {
		t.Helper()
		r, err := anon.Get(e.ui + path)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return r, string(b)
	}

	// Without the cookie: 401, for static files too.
	for _, p := range []string{"/", "/r/alice", "/static/ui.js", "/static/htmx-2.0.11.min.js", "/favicon.svg", "/?t=WRONGWRONGWRONGWRONG"} {
		r, body := anonGet(p)
		if r.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "threadsplice ui") {
			t.Errorf("GET %s without the cookie: %d\n%s", p, r.StatusCode, body)
		}
		checkHeaders(t, r.Header)
	}

	// The link sets the cookie and redirects to the same URL minus the token.
	r, _ := anonGet("/setup/claude?as=x&t=" + e.token)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/setup/claude?as=x" {
		t.Fatalf("link: %d, Location %q", r.StatusCode, r.Header.Get("Location"))
	}
	var c *http.Cookie
	for _, x := range r.Cookies() {
		if x.Name == "ts_ui" {
			c = x
		}
	}
	if c == nil || c.Value != e.token || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Path != "/" {
		t.Fatalf("cookie = %+v", c)
	}

	// Signed in: pages and assets, with the security headers.
	home := e.get("/")
	must(t, home, http.StatusOK, "Let your agents talk", `<meta name="htmx-config"`, `"allowEval":false`, `"allowScriptTags":false`)
	checkHeaders(t, home.h)
	if strings.Contains(home.body, "style=") || strings.Contains(home.body, "<script>") {
		t.Error("page has inline style or script, which the CSP blocks")
	}
	for p, ct := range map[string]string{"/static/ui.js": "text/javascript", "/static/style.css": "text/css",
		"/static/htmx-2.0.11.min.js": "text/javascript", "/favicon.svg": "image/svg+xml"} {
		r := e.get(p)
		if r.code != http.StatusOK || !strings.HasPrefix(r.h.Get("Content-Type"), ct) {
			t.Errorf("GET %s: %d %q", p, r.code, r.h.Get("Content-Type"))
		}
	}
	if r := e.get("/static/htmx.LICENSE"); r.code != http.StatusNotFound {
		t.Errorf("non-asset served: %d", r.code)
	}

	// DNS rebinding: another Host is refused, cookie or not.
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(e.ui, "http://"))
	r403 := e.do(http.MethodGet, "/", nil, map[string]string{"Host": "evil.example:" + port})
	if r403.code != http.StatusForbidden {
		t.Errorf("foreign Host: %d", r403.code)
	}
	checkHeaders(t, r403.h)
	// localhost is a different cookie host, so it takes the link once too.
	local := map[string]string{"Host": "localhost:" + port}
	if r := e.do(http.MethodGet, "/?t="+e.token, nil, local); r.code != http.StatusSeeOther {
		t.Errorf("link via localhost: %d", r.code)
	}
	if r := e.do(http.MethodGet, "/", nil, local); r.code != http.StatusOK {
		t.Errorf("localhost Host: %d", r.code)
	}

	// Cross-origin posts are refused even with the cookie; same-origin ones get through.
	form := url.Values{"invite": {"not an invite"}, "name": {"bob"}}
	for _, h := range []map[string]string{{"Sec-Fetch-Site": "cross-site"}, {"Sec-Fetch-Site": "same-site"},
		{"Sec-Fetch-Site": "", "Origin": "http://evil.example"}} {
		if r := e.do(http.MethodPost, "/join", form, h); r.code != http.StatusForbidden {
			t.Errorf("cross-origin POST %v: %d", h, r.code)
		}
	}
	must(t, e.do(http.MethodPost, "/join", form, nil), http.StatusUnprocessableEntity, "invalid invite")

	// Other origins (another localhost port is the same site) get top-level
	// navigation only: no fetches, even ones that carry the cookie.
	if r := e.do(http.MethodGet, "/r/alice/feed?after=0", nil, map[string]string{"Sec-Fetch-Site": "same-site", "Sec-Fetch-Mode": "cors"}); r.code != http.StatusForbidden {
		t.Errorf("same-site fetch: %d", r.code)
	}
	if r := e.do(http.MethodGet, "/", nil, map[string]string{"Sec-Fetch-Site": "cross-site", "Sec-Fetch-Mode": "navigate", "Sec-Fetch-Dest": "document"}); r.code != http.StatusOK {
		t.Errorf("navigation from another site: %d", r.code)
	}

	// Oversized bodies: htmx forms keep their text and get a notice; plain ones a page.
	big := url.Values{"text": {strings.Repeat("x", maxBody)}, "kind": {"message"}}
	tooBig := e.hx(http.MethodPost, "/r/alice/send", big)
	must(t, tooBig, http.StatusRequestEntityTooLarge, `id="notice"`, `hx-swap-oob="true"`, "limited to 256 KiB")
	if tooBig.h.Get("HX-Reswap") != "none" {
		t.Errorf("HX-Reswap = %q", tooBig.h.Get("HX-Reswap"))
	}
	must(t, e.do(http.MethodPost, "/rooms", big, nil), http.StatusRequestEntityTooLarge, "<h1>Request Entity Too Large</h1>")
}

func TestCheckAddr(t *testing.T) {
	for addr, ok := range map[string]bool{"127.0.0.1:4848": true, "localhost:4848": true, "[::1]:4848": true, "127.0.0.2:0": true,
		":4848": false, "0.0.0.0:4848": false, "[::]:4848": false, "192.168.1.5:4848": false, "example.com:4848": false, "127.0.0.1": false} {
		if err := CheckAddr(addr); (err == nil) != ok {
			t.Errorf("CheckAddr(%q) = %v, want ok=%v", addr, err, ok)
		}
	}
}

// TestRoomFlow walks the UI end to end against a real relay: create, join,
// add an agent, watch, send, invite and revoke.
func TestRoomFlow(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()

	// Create a room through the form; the room page shows its first invite once.
	r := e.do(http.MethodPost, "/rooms", url.Values{"relay": {e.relay}, "room": {"proj"}, "name": {"alice"}, "admin": {admin}, "ttl": {"24h"}}, nil)
	if r.code != http.StatusSeeOther || r.h.Get("Location") != "/r/alice" {
		t.Fatalf("create: %d %q\n%s", r.code, r.h.Get("Location"), r.body)
	}
	r = e.get("/r/alice")
	must(t, r, http.StatusOK, "<h1>proj</h1>", "you are <strong>alice</strong>", "the relay sees only ciphertext",
		"You see every message in this room, including direct messages between participants.", "Invite someone", "Single use")
	first := inviteIn(t, r.body)
	mustNot(t, e.get("/r/alice"), "invite-value")
	alice := agent(t, "alice")
	if !alice.ID.Owner || alice.ID.RoomName != "proj" {
		t.Fatalf("alice = %+v", alice.ID)
	}

	// Join with it through the form, as another machine would.
	r = e.do(http.MethodPost, "/join", url.Values{"invite": {first}, "name": {"bob"}}, nil)
	if r.code != http.StatusSeeOther || r.h.Get("Location") != "/r/bob" {
		t.Fatalf("join: %d %q\n%s", r.code, r.h.Get("Location"), r.body)
	}
	r = e.get("/r/bob")
	must(t, r, http.StatusOK, "you are <strong>bob</strong>", "You see messages to you, to the room, and your own.")
	mustNot(t, r, "Invite someone", "Add an agent")
	bob := agent(t, "bob")

	// Add an agent on this machine: a 0600 identity file and the setup hint.
	r = e.hx(http.MethodPost, "/r/alice/agents", url.Values{"name": {"carol"}})
	must(t, r, http.StatusOK, "threadsplice setup claude -as carol", `href="/setup/codex?as=carol"`, `hx-swap-oob="innerHTML:#nav"`, `href="/r/carol"`)
	carol := agent(t, "carol")
	if carol.ID.RoomID != alice.ID.RoomID || carol.ID.Owner || carol.ID.RoomKey != alice.ID.RoomKey || carol.ID.Name != "carol" {
		t.Fatalf("carol = %+v", carol.ID)
	}
	fi, err := os.Stat(filepath.Join(os.Getenv("THREADSPLICE_HOME"), "identities", "carol.json"))
	if err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("identity file: %v %v", fi.Mode(), err)
	}
	must(t, e.hx(http.MethodPost, "/r/alice/agents", url.Values{"name": {"carol"}}), http.StatusUnprocessableEntity, "already taken")
	must(t, e.hx(http.MethodPost, "/r/bob/agents", url.Values{"name": {"dave"}}), http.StatusForbidden, "Only the room owner")

	must(t, e.get("/setup/claude?as=carol"), http.StatusOK, "claude mcp add --scope user threadsplice -- threadsplice mcp -as carol", `aria-current="page">claude<`)
	for _, p := range []string{"/setup/emacs?as=carol", "/setup/claude?as=nobody", "/r/nobody", "/r/nobody/feed?after=0"} {
		if r := e.get(p); r.code != http.StatusNotFound {
			t.Errorf("GET %s: %d", p, r.code)
		}
	}

	// Traffic: bob sends carol something hostile, alice (through the form)
	// asks carol, carol answers the room, and someone sends a forgery.
	const hostile = `<script>alert(1)</script><img src=x onerror=alert(2)> hx-get="/r/alice/agents" hx-trigger="load"`
	const escaped = `&lt;script&gt;alert(1)&lt;/script&gt;&lt;img src=x onerror=alert(2)&gt; hx-get=&#34;/r/alice/agents&#34; hx-trigger=&#34;load&#34;`
	if _, err := bob.Send(ctx, "carol", api.Payload{Kind: api.KindAsk, Text: hostile}); err != nil {
		t.Fatal(err)
	}
	r = e.hx(http.MethodPost, "/r/alice/send", url.Values{"to": {carol.ID.ParticipantID}, "kind": {"ask"}, "text": {"for carol only\r\nline two"}})
	must(t, r, http.StatusOK, `id="compose"`, "Sent.", "(Ctrl+Enter sends)\" autofocus></textarea>")
	if r.h.Get("HX-Trigger") != "ts-sent" {
		t.Errorf("HX-Trigger = %q", r.h.Get("HX-Trigger"))
	}
	must(t, e.hx(http.MethodPost, "/r/alice/send", url.Values{"to": {""}, "kind": {"ask"}, "text": {"  "}}), http.StatusUnprocessableEntity, "Write something")
	got, err := carol.Inbox(ctx, 0)
	if err != nil || len(got) != 2 || got[0].Text != hostile || got[1].FromName != "alice" || got[1].Kind != api.KindAsk || got[1].Text != "for carol only\nline two" {
		t.Fatalf("carol's inbox = %+v, %v", got, err)
	}
	if _, err := carol.Send(ctx, "", api.Payload{Kind: api.KindReply, Text: "hello room", ReplyTo: got[1].ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Relay.Send(ctx, bob.ID.RoomID, api.SendRequest{ID: "forged1", Sealed: []byte{1, 2, 3}}); err != nil {
		t.Fatal(err)
	}

	// The owner sees the whole room, DMs between others included; peer text stays text.
	r = e.hx(http.MethodGet, "/r/alice/feed?after=0", nil)
	must(t, r, http.StatusOK, `hx-swap-oob="beforeend:#feed"`, escaped, "for carol only\nline two", "hello room",
		`href="#m-`+got[1].ID+`"`, "Couldn't decrypt this message", `class="badge kind">ask<`, `class="badge kind">reply<`)
	mustNot(t, r, "<script>alert", "<img src=x", `hx-get="/r/alice/agents"`, `hx-trigger="load"`)
	cursor := regexp.MustCompile(`hx-get="/r/alice/feed\?after=(\d+)"`).FindStringSubmatch(r.body)
	if cursor == nil || cursor[1] == "0" {
		t.Fatalf("poller cursor: %v", cursor)
	}
	r = e.hx(http.MethodGet, "/r/alice/feed?after="+cursor[1], nil)
	must(t, r, http.StatusOK, `hx-get="/r/alice/feed?after=`+cursor[1]+`"`)
	mustNot(t, r, "beforeend:#feed")
	must(t, e.get("/r/alice"), http.StatusOK, escaped, "for carol only", "hello room")

	// Anyone else sees what they sent and the room, never others' DMs.
	r = e.hx(http.MethodGet, "/r/bob/feed?after=0", nil)
	must(t, r, http.StatusOK, escaped, "hello room")
	mustNot(t, r, "for carol only", "<script>alert")

	// Invites: a real one for the owner, none for anyone else.
	r = e.hx(http.MethodPost, "/r/alice/invite", url.Values{"ttl": {"1h"}})
	must(t, r, http.StatusOK, "contains the room key", "threadsplice join -as &lt;name&gt; &#39;"+e.relay+"#")
	relayURL, tok, key, err := client.ParseInvite(inviteIn(t, r.body))
	if err != nil || relayURL != e.relay || key.String() != alice.ID.RoomKey {
		t.Fatalf("invite: %q %v %v", relayURL, key.String() == alice.ID.RoomKey, err)
	}
	if _, err := client.New(relayURL, "").Join(ctx, tok, "dave"); err != nil {
		t.Fatalf("redeeming the invite: %v", err)
	}
	must(t, e.hx(http.MethodPost, "/r/bob/invite", url.Values{"ttl": {"1h"}}), http.StatusForbidden, "Only the room owner")

	// Revoking: only the owner, and never itself.
	must(t, e.hx(http.MethodGet, "/r/alice/participants", nil), http.StatusOK, "(you)", `class="badge owner"`, "Revoke", `value="`+bob.ID.ParticipantID+`"`)
	mustNot(t, e.hx(http.MethodGet, "/r/carol/participants", nil), "Revoke")
	must(t, e.hx(http.MethodPost, "/r/bob/revoke", url.Values{"pid": {carol.ID.ParticipantID}}), http.StatusForbidden, "Only the room owner")
	must(t, e.hx(http.MethodPost, "/r/alice/revoke", url.Values{"pid": {alice.ID.ParticipantID}}), http.StatusUnprocessableEntity)
	if _, err := carol.Peers(ctx); err != nil {
		t.Fatalf("carol lost access after a refused revoke: %v", err)
	}
	r = e.hx(http.MethodPost, "/r/alice/revoke", url.Values{"pid": {bob.ID.ParticipantID}})
	must(t, r, http.StatusOK, `id="participants"`, `class="peer revoked"`)
	var se *client.StatusError
	if _, err := bob.Peers(ctx); !errors.As(err, &se) || se.Code != http.StatusUnauthorized {
		t.Fatalf("bob after revocation: %v", err)
	}

	// No response carried the admin token or a participant token, and only
	// invites carried the room key.
	for _, b := range e.bodies {
		for _, secret := range []string{admin, alice.ID.Token, bob.ID.Token, carol.ID.Token} {
			if strings.Contains(b, secret) {
				t.Fatalf("a response contains a secret:\n%s", b)
			}
		}
		if strings.Contains(b, alice.ID.RoomKey) && !strings.Contains(b, `id="invite-value"`) {
			t.Fatalf("a response other than an invite contains the room key:\n%s", b)
		}
	}
}

func TestCreateRoomErrors(t *testing.T) {
	e := newEnv(t)
	// A relay that echoes the token it was given.
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(api.Error{Error: "bad token " + strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")})
	}))
	defer echo.Close()
	for _, f := range []url.Values{
		{"relay": {e.relay}, "room": {"proj"}, "name": {"alice"}, "admin": {"wrong-admin-token-0123456789"}, "ttl": {"24h"}},
		{"relay": {echo.URL}, "room": {"proj"}, "name": {"alice"}, "admin": {admin}, "ttl": {"24h"}},
		{"relay": {e.relay}, "room": {"bad name"}, "name": {"alice"}, "admin": {admin}, "ttl": {"24h"}},
		{"relay": {"http://relay.example.com"}, "room": {"proj"}, "name": {"alice"}, "admin": {admin}, "ttl": {"24h"}},
		{"relay": {e.relay}, "room": {"proj"}, "name": {"alice"}, "admin": {"tooShortXYZ"}, "ttl": {"24h"}},
	} {
		r := e.do(http.MethodPost, "/rooms", f, nil)
		must(t, r, http.StatusUnprocessableEntity, `class="error"`, `value="`+f.Get("relay")+`"`)
		mustNot(t, r, f.Get("admin"))
	}
	must(t, e.do(http.MethodPost, "/rooms", url.Values{"relay": {echo.URL}, "room": {"proj"}, "name": {"alice"}, "admin": {admin}, "ttl": {"24h"}}, nil),
		http.StatusUnprocessableEntity, "bad token [redacted]")
	if names, _ := client.List(); len(names) != 0 {
		t.Fatalf("identities saved after failures: %v", names)
	}

	// A second room under the same name gets its own local identity name.
	for _, c := range [][2]string{{"proj", "/r/alice"}, {"other", "/r/alice-other"}} {
		r := e.do(http.MethodPost, "/rooms", url.Values{"relay": {e.relay}, "room": {c[0]}, "name": {"alice"}, "admin": {admin}, "ttl": {"none"}}, nil)
		if r.code != http.StatusSeeOther || r.h.Get("Location") != c[1] {
			t.Fatalf("create %s: %d %q\n%s", c[0], r.code, r.h.Get("Location"), r.body)
		}
	}
	r := e.get("/r/alice-other")
	must(t, r, http.StatusOK, "you are <strong>alice</strong>", "(identity <code>alice-other</code>)")
	mustNot(t, r, "invite-value")
}

// TestWatching covers what a room page shows while it watches: a non-owner's
// view read through the local owner (so the page leaves that identity's
// presence alone), the cursor moving past other people's DMs, code blocks,
// and the hooks ui.js uses to keep focus and recipients in step.
func TestWatching(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	r := e.do(http.MethodPost, "/rooms", url.Values{"relay": {e.relay}, "room": {"proj"}, "name": {"alice"}, "admin": {admin}, "ttl": {"none"}}, nil)
	if r.code != http.StatusSeeOther {
		t.Fatalf("create: %d\n%s", r.code, r.body)
	}
	for _, n := range []string{"bob", "carol"} {
		must(t, e.hx(http.MethodPost, "/r/alice/agents", url.Values{"name": {n}}), http.StatusOK, "threadsplice setup claude -as "+n)
	}
	alice, bob, carol := agent(t, "alice"), agent(t, "bob"), agent(t, "carol")

	// Every relay call counts as presence: carol's page must not make her look online.
	must(t, e.get("/r/carol"), http.StatusOK, "you are <strong>carol</strong>")
	must(t, e.hx(http.MethodGet, "/r/carol/participants", nil), http.StatusOK, "carol <span class=\"muted\">(you)</span>")
	must(t, e.hx(http.MethodGet, "/r/carol/feed?after=0", nil), http.StatusOK)
	ps, err := alice.Peers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Name == "carol" && (p.Online || !p.LastSeen.IsZero()) {
			t.Errorf("watching carol made her look online: %+v", p)
		}
	}

	// A DM between two others moves carol's cursor without showing her anything.
	if _, err := bob.Send(ctx, "alice", api.Payload{Kind: api.KindMessage, Text: "private to alice"}); err != nil {
		t.Fatal(err)
	}
	r = e.hx(http.MethodGet, "/r/carol/feed?after=0", nil)
	must(t, r, http.StatusOK, `hx-get="/r/carol/feed?after=1"`)
	mustNot(t, r, "private to alice", "beforeend:#feed")

	// Fenced code is a monospace block, fences included, and still only text.
	code := "Patch:\n```go\nif a < b {\n\treturn \"<b>\"\n}\n```\nThat's all."
	if _, err := alice.Send(ctx, "", api.Payload{Kind: api.KindMessage, Text: code}); err != nil {
		t.Fatal(err)
	}
	must(t, e.hx(http.MethodGet, "/r/carol/feed?after=1", nil), http.StatusOK,
		"<div class=\"text\">Patch:\n</div><pre class=\"code\">```go\nif a &lt; b {\n\treturn &#34;&lt;b&gt;&#34;\n}\n```\n</pre><div class=\"text\">That&#39;s all.</div>")

	// Who can be written to, for the compose form; ids on revoke buttons, so
	// keyboard focus survives the panel's refresh.
	r = e.hx(http.MethodGet, "/r/alice/participants", nil)
	must(t, r, http.StatusOK, `data-to="`+bob.ID.ParticipantID+`" data-name="bob"`, `id="revoke-`+carol.ID.ParticipantID+`"`)
	mustNot(t, r, `data-to="`+alice.ID.ParticipantID+`"`)

	// Without its owner on this machine, an identity reads for itself.
	if err := os.Remove(filepath.Join(os.Getenv("THREADSPLICE_HOME"), "identities", "alice.json")); err != nil {
		t.Fatal(err)
	}
	r = e.hx(http.MethodGet, "/r/carol/feed?after=0", nil)
	must(t, r, http.StatusOK, "That&#39;s all.")
	mustNot(t, r, "private to alice")

	// The URL a failed form leaves behind leads back to the form, and the
	// error comes into view.
	if r := e.get("/rooms"); r.code != http.StatusSeeOther || r.h.Get("Location") != "/#create" {
		t.Errorf("GET /rooms: %d %q", r.code, r.h.Get("Location"))
	}
	must(t, e.do(http.MethodPost, "/join", url.Values{"invite": {"https://relay.example.com#nokey"}, "name": {"dave"}}, nil),
		http.StatusUnprocessableEntity, "missing token or room key. Paste the invite again", `tabindex="-1" autofocus`)
}

func TestBlocks(t *testing.T) {
	for in, want := range map[string][]block{
		"":                         nil,
		"plain\ntext":              {{"plain\ntext", false}},
		"a\n```\nx\n```\nb":        {{"a\n", false}, {"```\nx\n```\n", true}, {"b", false}},
		"```go\nunclosed":          {{"```go\nunclosed", true}},
		"  ```\nindented\n  ```":   {{"  ```\nindented\n  ```", true}},
		"inline ```not a fence```": {{"inline ```not a fence```", false}},
	} {
		if got := blocks(in); !slices.Equal(got, want) {
			t.Errorf("blocks(%q) = %+v, want %+v", in, got, want)
		}
	}
}
