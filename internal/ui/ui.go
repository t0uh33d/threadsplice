// Package ui serves thread splice's local web UI: create and join rooms,
// invite participants, and watch the live conversation between agents.
//
// It runs on the user's machine, listens on loopback only, and acts as this
// machine's identities: messages are decrypted here, and the plaintext goes
// nowhere but the browser on this machine. Every request needs the per-run
// access token (a cookie set from the ?t= link the CLI prints). Host checks
// stop DNS rebinding, CrossOriginProtection stops cross-origin posts, and a
// strict CSP plus html/template escaping keep peer-supplied text inert.
package ui

import (
	"bytes"
	"cmp"
	"context"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/t0uh33d/threadsplice/internal/api"
	"github.com/t0uh33d/threadsplice/internal/client"
	"github.com/t0uh33d/threadsplice/internal/e2ee"
	"github.com/t0uh33d/threadsplice/internal/hooks"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static/*.js static/*.css
var staticFiles embed.FS

const (
	cookieName  = "ts_ui"
	maxBody     = 256 << 10
	maxText     = 96 << 10 // as `threadsplice send -`
	minAdmin    = 16       // the relay refuses shorter admin tokens
	relayBudget = 30 * time.Second
	csp         = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"
	nameRule    = "Names are 1-32 characters of letters, digits, '.', '_' and '-', starting with a letter or digit."
)

var (
	tokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`) // invite tokens as the relay mints them
	relayID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)   // participant and message ids as the relay mints and accepts them
)

// Options configures the UI.
type Options struct {
	Token   string // per-run access token (crypto/rand.Text)
	Launch  string // optional one-time token for a link handed to a browser's command line
	Addr    string // host:port the UI listens on; requests must name it, or localhost, as Host
	Version string
}

// launchTTL bounds how long an unused Launch token stays valid.
const launchTTL = 2 * time.Minute

type server struct {
	token   []byte
	hosts   map[string]bool
	version string

	mu       sync.Mutex
	flash    map[string]inviteView // alias+room -> first invite of a room just created, shown once
	launch   []byte                // Options.Launch until spent
	launchBy time.Time
}

// New returns the UI's HTTP handler.
func New(opt Options) (http.Handler, error) {
	if len(opt.Token) < 16 || opt.Launch != "" && len(opt.Launch) < 16 {
		return nil, errors.New("ui: access tokens must be at least 16 characters")
	}
	host, port, err := net.SplitHostPort(opt.Addr)
	if err != nil {
		return nil, fmt.Errorf("ui: listen address: %w", err)
	}
	s := &server{token: []byte(opt.Token), version: cmp.Or(opt.Version, "dev"), flash: map[string]inviteView{}, hosts: map[string]bool{}}
	if opt.Launch != "" {
		s.launch, s.launchBy = []byte(opt.Launch), time.Now().Add(launchTTL)
	}
	for _, h := range []string{host, "localhost", "127.0.0.1", "::1"} {
		hp := strings.ToLower(net.JoinHostPort(h, port))
		s.hosts[hp] = true
		if port == "80" { // browsers leave the default port out of Host
			s.hosts[strings.TrimSuffix(hp, ":80")] = true
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", s.home)
	mux.HandleFunc("POST /rooms", s.createRoom)
	mux.HandleFunc("POST /join", s.joinRoom)
	// A form that failed leaves its POST URL in the address bar; going there gets the form.
	mux.Handle("GET /rooms", http.RedirectHandler("/#create", http.StatusSeeOther))
	mux.Handle("GET /join", http.RedirectHandler("/#join", http.StatusSeeOther))
	mux.HandleFunc("GET /r/{alias}", s.room)
	mux.HandleFunc("GET /r/{alias}/feed", s.feed)
	mux.HandleFunc("GET /r/{alias}/participants", s.participants)
	mux.HandleFunc("POST /r/{alias}/send", s.send)
	mux.HandleFunc("POST /r/{alias}/invite", s.invite)
	mux.HandleFunc("POST /r/{alias}/agents", s.addAgent)
	mux.HandleFunc("POST /r/{alias}/revoke", s.revoke)
	mux.HandleFunc("GET /setup/{target}", s.setup)
	mux.HandleFunc("GET /static/{file}", s.static)
	mux.HandleFunc("GET /favicon.svg", s.favicon)
	return s.guard(http.NewCrossOriginProtection().Handler(mux)), nil
}

// Serve runs the UI on ln, which must be a loopback listener, until ctx is
// cancelled, then shuts down gracefully.
func Serve(ctx context.Context, ln net.Listener, opt Options) error {
	opt.Addr = ln.Addr().String()
	if err := CheckAddr(opt.Addr); err != nil {
		return err
	}
	h, err := New(opt)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		return srv.Close()
	}
	return nil
}

// CheckAddr accepts only loopback listen addresses (127.0.0.0/8, ::1 or
// localhost): the UI shows decrypted messages and acts as this machine's
// identities, so it never listens on a network interface.
func CheckAddr(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address %q: %v", addr, err)
	}
	if ip := net.ParseIP(host); host == "localhost" || ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("%q is not a loopback address; the UI only listens on this machine (e.g. 127.0.0.1:4848)", addr)
}

// guard runs before everything: security headers, the Host check (DNS
// rebinding), fetch metadata, a body cap and the access token.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin") // a page that opened this one can't steer it later
		h.Set("X-Frame-Options", "DENY")                   // frame-ancestors, for browsers without it
		h.Set("Cache-Control", "no-store")
		if !s.hosts[strings.ToLower(r.Host)] {
			http.Error(w, "403 forbidden: unexpected Host header", http.StatusForbidden)
			return
		}
		// Other origins get top-level navigation only. Other localhost ports are
		// the same site, so SameSite=Strict alone would still send them the cookie.
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" && !navigation(r) {
			http.Error(w, "403 forbidden: cross-origin request", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		q := r.URL.Query()
		if t := q.Get("t"); t != "" && r.Method == http.MethodGet && (s.valid(t) || s.spend(t)) {
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: string(s.token), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			q.Del("t") // keep the token out of history and bookmarks
			u := url.URL{Path: path.Clean("/" + r.URL.Path), RawQuery: q.Encode()}
			http.Redirect(w, r, u.String(), http.StatusSeeOther)
			return
		}
		if !s.signedIn(r) {
			s.render(w, http.StatusUnauthorized, "problem", "unauthorized", nil)
			return
		}
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				s.badForm(w, r, err)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) valid(t string) bool { return subtle.ConstantTimeCompare([]byte(t), s.token) == 1 }

// signedIn accepts any ts_ui cookie that holds the token: a page on another
// localhost port can plant more of them (cookies ignore ports), and browsers
// send the one with the longest path first.
func (s *server) signedIn(r *http.Request) bool {
	return slices.ContainsFunc(r.CookiesNamed(cookieName), func(c *http.Cookie) bool { return s.valid(c.Value) })
}

// spend reports whether t is the one-time launch token, which then stops
// working. The -open link carries it, because a browser can keep the URL it
// was started with on its command line, where other local users can read it.
func (s *server) spend(t string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.launch == nil || time.Now().After(s.launchBy) || subtle.ConstantTimeCompare([]byte(t), s.launch) != 1 {
		return false
	}
	s.launch = nil
	return true
}

// navigation reports whether r is a plain top-level page load: not a fetch,
// a frame, or a prefetch or prerender, any of which another origin could
// start without the user seeing this page.
func navigation(r *http.Request) bool {
	return r.Method == http.MethodGet && r.Header.Get("Sec-Fetch-Mode") == "navigate" &&
		r.Header.Get("Sec-Fetch-Dest") == "document" && r.Header.Get("Sec-Purpose") == ""
}

// redact removes secrets from text a page shows. The relay already holds the
// tokens a request sent it, but a misbehaving one could echo them back, and
// pages never show them.
func redact(msg string, secrets ...string) string {
	for _, x := range secrets {
		if x != "" {
			msg = strings.ReplaceAll(msg, x, "[redacted]")
		}
	}
	return msg
}

// badForm answers a POST body that didn't parse, usually one over maxBody.
// An htmx form keeps what the user typed: only a notice is swapped in.
func (s *server) badForm(w http.ResponseWriter, r *http.Request, err error) {
	code, msg := http.StatusBadRequest, "That form couldn't be read."
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		code, msg = http.StatusRequestEntityTooLarge, fmt.Sprintf("Too large to send: requests are limited to %d KiB.", maxBody>>10)
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Reswap", "none")
		s.render(w, code, "problem", "notice", msg)
		return
	}
	s.fail(w, r, code, msg)
}

// ---- rendering ----

var funcs = template.FuncMap{"short": short, "ago": ago, "clock": clock, "kind": kindClass, "state": stateClass, "blocks": blocks}

// pages holds one template set per page; each defines "main" for the layout,
// and room's also holds the fragments htmx swaps in.
var pages = parsePages("home", "room", "setup", "problem")

func parsePages(names ...string) map[string]*template.Template {
	layout := template.Must(template.New("").Funcs(funcs).ParseFS(templateFiles, "templates/layout.html"))
	m := map[string]*template.Template{}
	for _, n := range names {
		m[n] = template.Must(template.Must(layout.Clone()).ParseFS(templateFiles, "templates/"+n+".html"))
	}
	return m
}

// render executes template name from page set p, buffered so a failure
// never sends half a page.
func (s *server) render(w http.ResponseWriter, code int, p, name string, data any) {
	var b bytes.Buffer
	if err := pages[p].ExecuteTemplate(&b, name, data); err != nil {
		log.Printf("threadsplice ui: rendering %s/%s: %v", p, name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	w.Write(b.Bytes())
}

// fail reports an error: as a fragment to htmx, else as a page.
func (s *server) fail(w http.ResponseWriter, r *http.Request, code int, msg string) {
	if r.Header.Get("HX-Request") == "true" {
		s.render(w, code, "problem", "error", msg)
		return
	}
	s.render(w, code, "problem", "layout", problemPage{frame: s.frame(http.StatusText(code), ""), Message: msg})
}

// frame is what every full page shares: title, version and the sidebar.
type frame struct {
	Title, Version, Current string
	Nav                     []navRoom
}

type problemPage struct {
	frame
	Message string
}

func (s *server) frame(title, current string) frame {
	return frame{Title: title, Version: s.version, Current: current, Nav: nav()}
}

type navRoom struct {
	Name, Relay, URL string
	Members          []navMember
}

type navMember struct {
	Alias, Name   string
	Owner, Broken bool
}

// nav groups this machine's identities by room for the sidebar.
func nav() []navRoom {
	aliases, _ := client.List()
	var rooms []navRoom
	at := map[string]int{}
	for _, a := range aliases {
		id, err := client.Load(a)
		key, m := id.Relay+"\x00"+id.RoomID, navMember{Alias: a, Name: id.Name, Owner: id.Owner}
		if err != nil {
			key, m = "\x00", navMember{Alias: a, Name: a, Broken: true}
			id = client.Identity{RoomName: "Unreadable identities"}
		}
		i, ok := at[key]
		if !ok {
			i, at[key] = len(rooms), len(rooms)
			rooms = append(rooms, navRoom{Name: id.RoomName, Relay: relayHost(id.Relay), URL: id.Relay})
		}
		rooms[i].Members = append(rooms[i].Members, m)
	}
	slices.SortFunc(rooms, func(a, b navRoom) int { return cmp.Or(cmp.Compare(a.Name, b.Name), cmp.Compare(a.Relay, b.Relay)) })
	for _, r := range rooms { // owner first
		slices.SortFunc(r.Members, func(a, b navMember) int {
			switch {
			case a.Owner && !b.Owner:
				return -1
			case b.Owner && !a.Owner:
				return 1
			}
			return cmp.Compare(a.Name, b.Name)
		})
	}
	return rooms
}

func relayHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

// short abbreviates a message id for display.
func short(id string) string {
	if r := []rune(id); len(r) > 8 {
		return string(r[:8])
	}
	return id
}

// ago renders a relative time such as "5m ago".
func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	switch d := time.Since(t); {
	case d < 10*time.Second:
		return "just now"
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s ago"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m ago"
	case d < 48*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h ago"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d ago"
	}
}

// clock renders a message time in local time: "15:04" today, else with the date.
func clock(t time.Time) string {
	t, now := t.Local(), time.Now()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}

// block is a run of message text; Code marks a ``` fenced block, fences
// included, so the page still shows exactly what was sent.
type block struct {
	Text string
	Code bool
}

// blocks splits message text at ``` fences, so code shows in monospace.
// ponytail: backtick fences at the start of a line only; no other markdown.
func blocks(s string) []block {
	var out []block
	start, at, code := 0, 0, false
	for line := range strings.Lines(s) {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), "```") {
			end := at // an opening fence ends the text before it
			if code {
				end = at + len(line) // a closing fence ends its block
			}
			if end > start {
				out = append(out, block{s[start:end], code})
			}
			start, code = end, !code
		}
		at += len(line)
	}
	if start < len(s) {
		out = append(out, block{s[start:], code})
	}
	return out
}

// kindClass and stateClass map peer- and relay-supplied values to fixed CSS classes.
func kindClass(k string) string {
	switch k {
	case api.KindAsk, api.KindReply, api.KindMessage:
		return k
	}
	return "other"
}

func stateClass(s string) string {
	switch s {
	case "idle", "busy", "away":
		return s
	}
	return "other"
}

// ---- home: create and join ----

type ttlOption struct {
	Value, Label string
	Dur          time.Duration
}

// ttls are the invite lifetimes on offer (the relay caps them at 7 days).
// "none" is only for the create form: no first invite.
var ttls = []ttlOption{{"1h", "Valid for 1 hour", time.Hour}, {"24h", "Valid for 24 hours", 24 * time.Hour},
	{"72h", "Valid for 3 days", 72 * time.Hour}, {"168h", "Valid for 7 days", 168 * time.Hour}, {"none", "No invite yet", 0}}

func ttlFor(v string) (time.Duration, bool) {
	i := slices.IndexFunc(ttls, func(o ttlOption) bool { return o.Value == v })
	if i < 0 {
		return 0, false
	}
	return ttls[i].Dur, true
}

// createForm and joinForm are what the forms re-render with. Neither holds
// a secret: the admin token and the invite are never sent back.
type createForm struct{ Relay, Room, Name, TTL, Err string }
type joinForm struct{ Name, Err string }

type homePage struct {
	frame
	Create createForm
	Join   joinForm
	TTLs   []ttlOption
}

func (s *server) home(w http.ResponseWriter, r *http.Request) {
	s.renderHome(w, http.StatusOK, createForm{}, joinForm{})
}

func (s *server) renderHome(w http.ResponseWriter, code int, c createForm, j joinForm) {
	p := homePage{frame: s.frame("Rooms", ""), Create: c, Join: j, TTLs: ttls}
	if p.Create.Relay == "" {
		p.Create.Relay = os.Getenv("THREADSPLICE_RELAY")
		if p.Create.Relay == "" && len(p.Nav) > 0 {
			p.Create.Relay = p.Nav[0].URL
		}
	}
	if p.Create.Name == "" {
		// The human's own name: agents get theirs when they're added.
		if n := cmp.Or(os.Getenv("USER"), os.Getenv("USERNAME"), os.Getenv("LOGNAME")); api.ValidName(n) {
			p.Create.Name = n
		}
	}
	p.Create.TTL = cmp.Or(p.Create.TTL, "24h")
	s.render(w, code, "home", "layout", p)
}

// createRoom does what `threadsplice room create` does: the relay creates the
// room, the room key is generated here, and the identity is saved locally.
func (s *server) createRoom(w http.ResponseWriter, r *http.Request) {
	f := createForm{Relay: strings.TrimRight(strings.TrimSpace(r.PostFormValue("relay")), "/"), Room: strings.TrimSpace(r.PostFormValue("room")),
		Name: strings.TrimSpace(r.PostFormValue("name")), TTL: r.PostFormValue("ttl")}
	admin := r.PostFormValue("admin") // used for this one request: never stored, logged or rendered
	ttl, ttlOK := ttlFor(f.TTL)
	switch {
	case f.Relay == "" || f.Room == "" || f.Name == "" || admin == "":
		f.Err = "Fill in every field."
	case !api.ValidName(f.Room) || !api.ValidName(f.Name):
		f.Err = nameRule
	case len(admin) < minAdmin:
		f.Err = fmt.Sprintf("That isn't the relay's admin token: relays require at least %d characters.", minAdmin)
	case !ttlOK:
		f.Err = "Choose how long the first invite stays valid."
	default:
		if err := client.CheckRelayURL(f.Relay); err != nil {
			f.Err = err.Error()
		}
	}
	if f.Err == "" {
		ctx, cancel := relayCtx(r)
		defer cancel()
		m, err := client.New(f.Relay, admin).CreateRoom(ctx, f.Room, f.Name)
		if err == nil {
			key := e2ee.NewKey() // generated here; the relay never sees it
			id := client.Identity{Relay: f.Relay, RoomID: m.RoomID, RoomName: m.RoomName, ParticipantID: m.ParticipantID,
				Name: f.Name, Owner: true, Token: m.Token, RoomKey: key.String()}
			if id, err = saveNew(id); err == nil {
				if ttl > 0 {
					inv, err := mintInvite(ctx, id, ttl)
					if err != nil {
						inv.Err = "The room is ready, but its first invite failed: " + redact(err.Error(), admin, id.Token)
					}
					s.mu.Lock()
					s.flash[flashKey(id)] = inv
					s.mu.Unlock()
				}
				http.Redirect(w, r, "/r/"+id.Alias, http.StatusSeeOther)
				return
			}
			err = fmt.Errorf("the relay created the room, but it couldn't be saved on this machine: %w", err)
		}
		f.Err = redact(err.Error(), admin) // a relay must not be able to make this page echo the token back
	}
	s.renderHome(w, http.StatusUnprocessableEntity, f, joinForm{})
}

// flashKey ties a first invite to its room as well as the local name, which
// a later identity could reuse.
func flashKey(id client.Identity) string { return id.Alias + "\x00" + id.Relay + "\x00" + id.RoomID }

// joinRoom does what `threadsplice join` does: redeem the invite token at the
// relay, keep the room key from the invite here.
func (s *server) joinRoom(w http.ResponseWriter, r *http.Request) {
	f := joinForm{Name: strings.TrimSpace(r.PostFormValue("name"))}
	relayURL, token, key, err := client.ParseInvite(r.PostFormValue("invite"))
	switch {
	case !api.ValidName(f.Name):
		f.Err = nameRule
	case err != nil:
		f.Err = err.Error()
	default:
		if err := client.CheckRelayURL(relayURL); err != nil {
			f.Err = err.Error()
		}
	}
	if f.Err == "" {
		ctx, cancel := relayCtx(r)
		defer cancel()
		m, err := client.New(relayURL, "").Join(ctx, token, f.Name) // the key stays local
		if err == nil {
			id := client.Identity{Relay: relayURL, RoomID: m.RoomID, RoomName: m.RoomName, ParticipantID: m.ParticipantID,
				Name: f.Name, Token: m.Token, RoomKey: key.String()}
			if id, err = saveNew(id); err == nil {
				http.Redirect(w, r, "/r/"+id.Alias, http.StatusSeeOther)
				return
			}
		}
		f.Err = redact(err.Error(), token)
	}
	f.Err = strings.TrimRight(f.Err, ".") + ". Paste the invite again: it contains the room key, so this page never shows it back."
	s.renderHome(w, http.StatusUnprocessableEntity, createForm{}, f)
}

var saveMu sync.Mutex

// saveNew stores a new identity under the first free local name: the
// participant name, else <name>-<room>, then numbered variants. Peers only
// see the participant name; the local name is what -as selects.
func saveNew(id client.Identity) (client.Identity, error) {
	// Between picking a free name and saving it, no other request may pick
	// the same one: client.Save refuses a taken name (os.Link), which would
	// turn the race into a spurious "already exists" error.
	saveMu.Lock()
	defer saveMu.Unlock()
	taken, err := client.List()
	if err != nil {
		return id, err
	}
	base := id.Name + "-" + id.RoomName
	base = base[:min(len(base), 60)]
	cands := []string{id.Name, base}
	for i := 2; i <= 9; i++ {
		cands = append(cands, base+"-"+strconv.Itoa(i))
	}
	for _, c := range cands {
		// EqualFold: on case-insensitive disks (macOS, Windows) Alice.json is alice.json.
		if client.ValidAlias(c) && !slices.ContainsFunc(taken, func(t string) bool { return strings.EqualFold(t, c) }) {
			id.Alias = c
			return id, client.Save(id)
		}
	}
	return id, errors.New("no free identity name on this machine: remove an unused identity first")
}

// ---- setup and static files ----

type setupPage struct {
	frame
	Target, Alias, Text, Exe, Home string
	Targets, Aliases               []string
}

// setup shows what `threadsplice setup <target> -as <alias>` prints.
func (s *server) setup(w http.ResponseWriter, r *http.Request) {
	target := r.PathValue("target")
	if !slices.Contains(hooks.SetupTargets, target) {
		s.fail(w, r, http.StatusNotFound, "Unknown agent. Setup covers: "+strings.Join(hooks.SetupTargets, ", ")+".")
		return
	}
	p := setupPage{frame: s.frame("Connect an agent", ""), Target: target, Targets: hooks.SetupTargets}
	p.Aliases, _ = client.List()
	if as := r.URL.Query().Get("as"); as != "" {
		if !client.ValidAlias(as) || !slices.Contains(p.Aliases, as) {
			s.fail(w, r, http.StatusNotFound, "No such identity on this machine.")
			return
		}
		p.Alias, p.Current = as, as
		p.Text, _ = hooks.Setup(target, as)
		p.Exe, _ = os.Executable()
		p.Home = os.Getenv("THREADSPLICE_HOME")
	}
	s.render(w, http.StatusOK, "setup", "layout", p)
}

// staticTypes is fixed rather than mime.TypeByExtension: the Windows registry
// can map .js to text/plain, which nosniff then refuses to run.
var staticTypes = map[string]string{".js": "text/javascript; charset=utf-8", ".css": "text/css; charset=utf-8"}

func (s *server) static(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	ct, ok := staticTypes[path.Ext(name)]
	b, err := staticFiles.ReadFile("static/" + name)
	if !ok || err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Write(b)
}

// favicon serves the header's mark, which is self-contained SVG.
func (s *server) favicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	pages["problem"].ExecuteTemplate(w, "mark", nil)
}

func relayCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), relayBudget)
}
