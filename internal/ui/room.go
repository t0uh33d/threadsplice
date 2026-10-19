package ui

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/t0uh33d/threadsplice/internal/api"
	"github.com/t0uh33d/threadsplice/internal/client"
	"github.com/t0uh33d/threadsplice/internal/e2ee"
)

// roomView is all a page may show about an identity: never its token or room key.
type roomView struct {
	Alias, Room, Relay, Name, PID string
	Owner                         bool
}

func viewOf(id client.Identity) roomView {
	return roomView{Alias: id.Alias, Room: id.RoomName, Relay: relayHost(id.Relay), Name: id.Name, PID: id.ParticipantID, Owner: id.Owner}
}

type roomPage struct {
	frame
	Room    roomView
	Feed    feedView
	Members peersView
	Compose composeView
	Flash   *inviteView
	Steps   *checklist // nil once every step is done
	Phrase  string     // what to say in a Claude chat to join, for the empty state
}

// checklist is the room's getting-started steps, from real state; ui.js
// ticks them off live as members and messages arrive.
type checklist struct {
	Room                        roomView
	Members, Sessions, Messages bool
}

// agent loads the identity the {alias} path segment names and binds it to its
// relay. On failure it has answered the request already. Unlike client.Load,
// it never falls back to a default identity, and it only knows this machine's
// top-level memberships: session members never get pages of their own.
func (s *server) agent(w http.ResponseWriter, r *http.Request) (*client.Agent, bool) {
	alias := r.PathValue("alias")
	names, err := client.List()
	if err != nil || !client.ValidAlias(alias) || !slices.Contains(names, alias) {
		s.fail(w, r, http.StatusNotFound, "This machine isn't in that room, or no longer is.")
		return nil, false
	}
	a, err := client.OpenAgent(alias)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "This machine's membership in that room can't be read: "+err.Error())
		return nil, false
	}
	return a, true
}

// reader picks who reads the room for a page about identity a. Every relay
// call counts as presence, so polling as an agent's identity would make that
// agent look online, to the room and on this very page. When this machine
// also holds the room's owner, the owner reads instead (it may read the whole
// room, and the page keeps to what a may see); otherwise a reads for itself.
func reader(a *client.Agent) *client.Agent {
	if a.ID.Owner {
		return a
	}
	names, _ := client.List()
	for _, n := range names {
		id, err := client.Load(n)
		if err != nil || !id.Owner || id.RoomID != a.ID.RoomID || id.Relay != a.ID.Relay {
			continue
		}
		if o, err := client.NewAgent(id); err == nil {
			return o
		}
	}
	return a
}

func (s *server) room(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agent(w, r)
	if !ok {
		return
	}
	ctx, cancel := relayCtx(r)
	defer cancel()
	v, rd := viewOf(a.ID), reader(a)
	p := roomPage{frame: s.frame(v.Room, v.Alias), Room: v, Members: peers(ctx, rd, v)} // first: it warms rd's name cache
	p.Feed = history(ctx, rd, v, 0)
	annotate(p.Feed.Rows, p.Members.byID)
	p.Compose = composeView{Room: v, Peers: p.Members.recipients()}
	p.Phrase, _ = joinPhrase(v)
	if p.Members.Err == "" && p.Feed.Err == "" {
		c := checklist{Room: v, Members: p.Members.Others > 0, Sessions: p.Members.Sessions > 0, Messages: len(p.Feed.Rows) > 0 || p.Feed.Skipped > 0}
		if !c.Members || !c.Sessions || !c.Messages {
			p.Steps = &c
		}
	}
	if v.Owner {
		s.mu.Lock()
		if inv, ok := s.flash[flashKey(a.ID)]; ok {
			p.Flash = &inv
			delete(s.flash, flashKey(a.ID))
		}
		s.mu.Unlock()
	}
	s.render(w, http.StatusOK, "room", "layout", p)
}

// ---- conversation feed ----

type row struct {
	ID, From, To, Kind, Text, ReplyTo, Err string

	Group        string // sender and recipient ids: consecutive rows with the same Group share one header
	On           string // a session member's machine
	Agent        bool   // sent by a session member
	Initials, Av string // avatar
	Quote        string // the start of the message ReplyTo answers, when the feed has it
	Answered     bool   // an ask the feed has a reply to
	Cont         bool   // continues the group above: no header of its own
	At           time.Time
	Depth        int
	Mine, ToMe   bool
}

type feedView struct {
	Room    roomView
	Rows    []row
	Skipped int
	Last    int64 // cursor for the next poll
	Err     string
}

// maxRows caps the messages one response renders.
// ponytail: the relay has no "newest N" query, so this pages through the whole
// history after the cursor and keeps the tail. Add one if rooms grow large.
const maxRows = 500

// groupGap is how far apart a sender's messages may be and still share one
// header (ui.js uses the same gap for rows that arrive later).
const groupGap = 5 * time.Minute

// history reads the room log after seq `after` through r (see reader) —
// read-only, nothing is acknowledged, so agents' inboxes are untouched — and
// keeps what v may see: the whole room for the owner, else what v may read
// plus what v sent.
func history(ctx context.Context, r *client.Agent, v roomView, after int64) (f feedView) {
	f = feedView{Room: v, Last: after}
	defer func() { link(f.Rows) }()
	for {
		msgs, last, err := r.Log(ctx, f.Last, 0)
		if err != nil {
			f.Err = "Couldn't load messages: " + redact(err.Error(), r.ID.Token) + ". Retrying."
			return f
		}
		if last <= f.Last {
			return f
		}
		f.Last = last // also past a page that was all other people's DMs
		for _, m := range msgs {
			if !v.Owner && m.Direct && m.To != v.PID && m.From != v.PID {
				continue
			}
			f.Rows = append(f.Rows, rowOf(m, v.PID))
		}
		if n := len(f.Rows) - maxRows; n > 0 {
			f.Rows, f.Skipped = f.Rows[n:], f.Skipped+n
		}
	}
}

func rowOf(m client.Message, me string) row {
	r := row{ID: m.ID, From: shown(m.FromName), To: shown(m.ToName), At: m.At, Mine: m.From == me, ToMe: m.Direct && m.To == me}
	if !relayID.MatchString(r.ID) {
		r.ID = "" // no anchor or short id for an id the relay itself refuses
	}
	if relayID.MatchString(m.From) && (m.To == "" || relayID.MatchString(m.To)) {
		r.Group = m.From + "|" + m.To
	}
	r.Initials, r.Av = avatar(r.From, "")
	if m.Err != nil {
		r.Err = undecryptable(m.Err)
		return r
	}
	r.Kind, r.Text, r.ReplyTo, r.Depth = cmp.Or(m.Kind, api.KindMessage), m.Text, m.ReplyTo, m.Depth
	return r
}

// undecryptable says in plain words why a message can't be shown. Opening
// fails in e2ee (wrong key, changed on the way, an unknown format) or in
// decoding what decrypted.
func undecryptable(err error) string {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.Is(err, e2ee.ErrOpen):
		return "It wasn't sealed with this room's key, or it was changed on the way."
	case errors.As(err, &syntax), errors.As(err, &typ):
		return "It decrypted, but it isn't a message thread splice can show."
	}
	return "It isn't in a format this version of thread splice can read."
}

// link groups consecutive rows per sender and ties replies to what they
// answer, within one response; ui.js does the same across responses.
func link(rows []row) {
	at := map[string]int{}
	for i := range rows {
		r := &rows[i]
		if r.ReplyTo != "" {
			if j, ok := at[r.ReplyTo]; ok {
				rows[j].Answered, r.Quote = true, quote(rows[j].Text)
			}
		}
		if r.ID != "" {
			at[r.ID] = i
		}
		if i > 0 {
			p := rows[i-1]
			d := r.At.Sub(p.At)
			r.Cont = r.Err == "" && p.Err == "" && r.Group != "" && r.Group == p.Group && d < groupGap && d > -groupGap
		}
	}
}

// quote is the start of what a reply answers: the asked message's first
// block, a code block without its fences, as ui.js quotes rows that arrive later.
func quote(text string) string {
	bs := blocks(text)
	switch {
	case len(bs) == 0:
		return ""
	case bs[0].Code:
		return snippet(bs[0].Body())
	}
	return snippet(bs[0].Text)
}

// annotate adds what the member list knows about each sender: whether it is
// a session member, and on which machine.
func annotate(rows []row, members map[string]peer) {
	for i := range rows {
		from, _, _ := strings.Cut(rows[i].Group, "|")
		if p, ok := members[from]; ok && p.ParentName != "" {
			rows[i].On, rows[i].Agent = p.ParentName, true
			rows[i].Initials, rows[i].Av = avatar(rows[i].From, p.ParentName)
		}
	}
}

// shown is a participant label as pages show it. client.Agent keeps names to
// the relay's rule, but falls back to the raw participant id for anyone it
// can't name, and a misbehaving relay can make that anything: bidi overrides,
// a fake "(you)". Those are shown as "unknown".
func shown(name string) string {
	if name == "" || api.ValidName(name) || relayID.MatchString(name) {
		return name
	}
	return "unknown"
}

// feed answers the poller: new rows appended out of band, then a poller with
// the new cursor in place of the old one. Relay errors keep the cursor, so
// the next poll retries.
func (s *server) feed(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agent(w, r)
	if !ok {
		return
	}
	after, err := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	if err != nil || after < 0 {
		s.fail(w, r, http.StatusBadRequest, "after must be a non-negative integer")
		return
	}
	ctx, cancel := relayCtx(r)
	defer cancel()
	v, rd := viewOf(a.ID), reader(a)
	f := history(ctx, rd, v, after)
	if len(f.Rows) > 0 { // most polls bring nothing, and then cost one relay call
		annotate(f.Rows, peers(ctx, rd, v).byID)
	}
	s.render(w, http.StatusOK, "room", "feed", f)
}

// ---- members ----

type peer struct {
	ID, Name, State string
	Parent          string // a session member's machine membership: its participant id
	ParentName      string
	Seen            time.Time
	Online, Owner   bool
	You, Revoked    bool
	Initials, Av    string
	Sessions        []peer // a machine's active session members
	Page            string // the page's alias, for the remove form
	CanRemove       bool
}

// Status is the word next to a member's dot: colour is never the only signal.
func (p peer) Status() string {
	switch {
	case p.Revoked:
		return "removed"
	case p.Online && p.State != "":
		return p.State
	case p.Online:
		return "online"
	case p.State == "away":
		return "away"
	}
	return "offline"
}

// Dot is the CSS class of a member's status dot.
func (p peer) Dot() string {
	switch p.Status() {
	case "removed", "offline":
		return "off"
	case "busy", "away":
		return p.Status()
	}
	return "on"
}

type peersView struct {
	Room     roomView
	Machines []peer // active top-level members, this page's first, each with its session members
	Removed  []peer
	Count    int // active members
	Others   int // active members besides this page's
	Sessions int // active session members
	byID     map[string]peer
	Err      string
}

// peers lists the room's members, grouped by machine. The relay says which
// members are sessions of which machine; it isn't trusted with that either,
// so a Parent counts only when it names a top-level member of this room and
// the session's name starts with that member's name and a dot.
func peers(ctx context.Context, a *client.Agent, v roomView) peersView {
	p := peersView{Room: v, byID: map[string]peer{}}
	ps, err := a.Peers(ctx)
	if err != nil {
		p.Err = "Couldn't load members: " + redact(err.Error(), a.ID.Token) + "."
		return p
	}
	top := map[string]api.Participant{}
	for _, x := range ps {
		if x.Parent == "" {
			top[x.ID] = x
		}
	}
	var all []peer
	for _, x := range ps {
		q := peer{ID: x.ID, Name: shown(x.Name), State: x.State, Seen: x.LastSeen, Online: x.Online, Owner: x.Owner,
			You: x.ID == v.PID, Revoked: x.Revoked, Page: v.Alias}
		if m, ok := top[x.Parent]; ok && x.Parent != x.ID && relayID.MatchString(x.Parent) && api.ValidName(m.Name) && strings.HasPrefix(x.Name, m.Name+".") {
			q.Parent, q.ParentName = x.Parent, m.Name
		}
		q.Initials, q.Av = avatar(q.Name, q.ParentName)
		q.CanRemove = v.Owner && !q.You && !q.Revoked && relayID.MatchString(q.ID)
		p.byID[q.ID] = q
		all = append(all, q)
	}
	// Machines in the relay's order, this page's first; sessions under theirs.
	slices.SortStableFunc(all, func(a, b peer) int {
		switch {
		case a.You == b.You:
			return 0
		case a.You:
			return -1
		}
		return 1
	})
	at := map[string]int{}
	var kids []peer
	for _, q := range all {
		switch {
		case q.Revoked:
			p.Removed = append(p.Removed, q)
			continue
		case q.Parent != "":
			p.Sessions++
			kids = append(kids, q)
		default:
			at[q.ID] = len(p.Machines)
			p.Machines = append(p.Machines, q)
		}
		p.Count++
		if !q.You {
			p.Others++
		}
	}
	// Sessions go under their machine wherever the relay listed them; one
	// whose machine was removed (the relay should have removed it too) stands alone.
	for _, k := range kids {
		if i, ok := at[k.Parent]; ok {
			p.Machines[i].Sessions = append(p.Machines[i].Sessions, k)
		} else {
			p.Machines = append(p.Machines, k)
		}
	}
	return p
}

// recipients are the members a message can go to: everyone active but this page's.
func (p peersView) recipients() []peer {
	var out []peer
	for _, m := range p.Machines {
		if !m.You {
			out = append(out, m)
		}
		out = append(out, m.Sessions...)
	}
	return out
}

func (s *server) participants(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agent(w, r)
	if !ok {
		return
	}
	ctx, cancel := relayCtx(r)
	defer cancel()
	s.render(w, http.StatusOK, "room", "participants", peers(ctx, reader(a), viewOf(a.ID)))
}

// revoke removes a member (owner only) and answers with the refreshed panel.
// The relay removes a machine's session members with it.
func (s *server) revoke(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agent(w, r)
	if !ok {
		return
	}
	ctx, cancel := relayCtx(r)
	defer cancel()
	code, msg, pid := http.StatusOK, "", r.PostFormValue("pid")
	switch {
	case !a.ID.Owner:
		code, msg = http.StatusForbidden, "Only the room owner can remove members."
	case pid == "" || pid == a.ID.ParticipantID:
		code, msg = http.StatusUnprocessableEntity, "Choose another member to remove."
	default:
		if err := a.Relay.Revoke(ctx, a.ID.RoomID, pid); err != nil {
			code, msg = http.StatusBadGateway, "Not removed: "+redact(err.Error(), a.ID.Token)
		}
	}
	p := peers(ctx, a, viewOf(a.ID))
	p.Err = cmp.Or(msg, p.Err)
	s.render(w, code, "room", "participants", p)
}

// ---- connect an agent ----

// claudeHooks is what `threadsplice setup claude` (no -as) has Claude Code
// run: each session's hooks find its own member, or stay silent.
const claudeHooks = `{
  "hooks": {
    "SessionStart": [{"hooks": [{"type": "command", "command": "threadsplice hook claude", "timeout": 10}]}],
    "UserPromptSubmit": [{"hooks": [{"type": "command", "command": "threadsplice hook claude", "timeout": 10}]}],
    "PostToolUse": [{"hooks": [{"type": "command", "command": "threadsplice hook claude", "timeout": 10}]}],
    "Stop": [{"hooks": [{"type": "command", "command": "threadsplice hook claude", "timeout": 10}]}],
    "SessionEnd": [{"hooks": [{"type": "command", "command": "threadsplice hook claude", "timeout": 3}]}]
  }
}`

type sessionsView struct {
	Room     roomView
	Sessions []peer
	Err      string
}

// sessionsOf picks the page's own session members out of the member list.
func sessionsOf(p peersView) sessionsView {
	sv := sessionsView{Room: p.Room, Err: p.Err}
	for _, m := range p.Machines {
		if m.You {
			for _, k := range m.Sessions {
				k.CanRemove = false
				sv.Sessions = append(sv.Sessions, k)
			}
		}
	}
	return sv
}

type connectPage struct {
	frame
	Room          roomView
	Sessions      sessionsView
	Phrase, Hooks string
	NameRoom      bool // this machine is in several rooms, so the phrase names this one
	Exe, Home     string
	Targets       []string // agents that join with a fixed member instead
	Fixed         string   // a name to suggest for such a member
}

// connect shows how a Claude Code session on this machine joins the room:
// register the MCP server once, add the hooks, then ask in the chat.
func (s *server) connect(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agent(w, r)
	if !ok {
		return
	}
	ctx, cancel := relayCtx(r)
	defer cancel()
	v := viewOf(a.ID)
	p := connectPage{frame: s.frame("Connect an agent · "+v.Room, v.Alias), Room: v, Hooks: claudeHooks,
		Sessions: sessionsOf(peers(ctx, reader(a), v)), Targets: []string{"codex", "copilot", "vscode"}, Fixed: "codex-" + v.Name}
	p.Phrase, p.NameRoom = joinPhrase(v)
	p.Phrase += " as reviewer"
	if !api.ValidName(p.Fixed) { // a long machine name leaves no room for the prefix
		p.Fixed = "codex"
	}
	p.Exe, _ = os.Executable()
	p.Home = os.Getenv("THREADSPLICE_HOME")
	s.render(w, http.StatusOK, "connect", "layout", p)
}

// joinPhrase is what to say in a Claude chat to join v's room. Once this
// machine is in several rooms it names the room, as the join tool then needs
// it. The user pastes it as their own words and the room name came from the
// relay, so the name goes in only if it's a valid one.
func joinPhrase(v roomView) (phrase string, named bool) {
	if machines() > 1 && api.ValidName(v.Room) {
		return "join the thread splice room " + v.Room, true
	}
	return "join the thread splice room", false
}

// machines counts the memberships a session's join could pick from, as the
// join tool does: the machine memberships, or every top-level one when none
// is marked (files from before session members).
func machines() int {
	names, _ := client.List()
	all, marked := 0, 0
	for _, n := range names {
		if id, err := client.Load(n); err == nil {
			all++
			if id.Machine {
				marked++
			}
		}
	}
	return cmp.Or(marked, all)
}

// sessions answers the Connect view's poller: this machine's session members.
func (s *server) sessions(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agent(w, r)
	if !ok {
		return
	}
	ctx, cancel := relayCtx(r)
	defer cancel()
	s.render(w, http.StatusOK, "connect", "sessions", sessionsOf(peers(ctx, reader(a), viewOf(a.ID))))
}

// ---- compose ----

type composeView struct {
	Room                roomView
	Peers               []peer
	To, Kind, Text, Err string
	Sent                bool
}

// send posts a message as this identity. It answers with the form, cleared on
// success, and tells the poller to fetch right away.
func (s *server) send(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agent(w, r)
	if !ok {
		return
	}
	ctx, cancel := relayCtx(r)
	defer cancel()
	c := composeView{Room: viewOf(a.ID), To: r.PostFormValue("to"), Kind: cmp.Or(r.PostFormValue("kind"), api.KindMessage),
		Text: strings.ReplaceAll(r.PostFormValue("text"), "\r\n", "\n")}
	code := http.StatusOK
	switch {
	case c.Kind != api.KindMessage && c.Kind != api.KindAsk:
		code, c.Err = http.StatusUnprocessableEntity, "Kind must be message or ask."
	case strings.TrimSpace(c.Text) == "":
		code, c.Err = http.StatusUnprocessableEntity, "Write something first."
	case len(c.Text) > maxText:
		code, c.Err = http.StatusUnprocessableEntity, fmt.Sprintf("Messages are limited to %d KiB.", maxText>>10)
	default:
		if _, err := a.Send(ctx, c.To, api.Payload{Kind: c.Kind, Text: c.Text}); err != nil {
			code, c.Err = http.StatusBadGateway, "Not sent: "+redact(err.Error(), a.ID.Token)
		} else {
			c.Text, c.Kind, c.Sent = "", "", true // an ask is one message: the toggle starts over
			w.Header().Set("HX-Trigger", "ts-sent")
		}
	}
	c.Peers = peers(ctx, reader(a), c.Room).recipients()
	s.render(w, code, "room", "compose", c)
}

// ---- invites ----

type inviteView struct {
	Invite, Expires, Err string
	Focus                bool // just created on request: its Copy button takes focus
}

// mintInvite does what `threadsplice invite` does: the relay mints a
// single-use token and the room key is appended here.
func mintInvite(ctx context.Context, id client.Identity, ttl time.Duration) (inviteView, error) {
	key, err := id.Key()
	if err != nil {
		return inviteView{}, err
	}
	inv, err := client.New(id.Relay, id.Token).CreateInvite(ctx, id.RoomID, ttl)
	if err != nil {
		return inviteView{}, err
	}
	if !tokenRe.MatchString(inv.Token) { // it ends up pasted into a terminal
		return inviteView{}, errors.New("the relay returned a malformed invite token")
	}
	return inviteView{Invite: client.FormatInvite(id.Relay, inv.Token, key), Expires: inv.ExpiresAt.Local().Format("Mon Jan 2 15:04")}, nil
}

type invitesPage struct {
	frame
	Room  roomView
	TTLs  []ttlOption
	Owner string // the owner's name, for the others' explanation
}

// invites is the Invite view: the owner creates invites for other machines;
// anyone else learns who can.
func (s *server) invites(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agent(w, r)
	if !ok {
		return
	}
	v := viewOf(a.ID)
	p := invitesPage{frame: s.frame("Invite a machine · "+v.Room, v.Alias), Room: v, TTLs: ttls}
	if !v.Owner {
		ctx, cancel := relayCtx(r)
		defer cancel()
		for _, m := range peers(ctx, reader(a), v).Machines {
			if m.Owner {
				p.Owner = m.Name
			}
		}
	}
	s.render(w, http.StatusOK, "invites", "layout", p)
}

func (s *server) invite(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agent(w, r)
	if !ok {
		return
	}
	ttl, ok := ttlFor(r.PostFormValue("ttl"))
	switch {
	case !a.ID.Owner:
		s.fail(w, r, http.StatusForbidden, "Only the room owner can invite.")
		return
	case !ok || ttl == 0:
		s.fail(w, r, http.StatusUnprocessableEntity, "Choose how long the invite stays valid.")
		return
	}
	ctx, cancel := relayCtx(r)
	defer cancel()
	inv, err := mintInvite(ctx, a.ID, ttl)
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "No invite: "+redact(err.Error(), a.ID.Token))
		return
	}
	inv.Focus = true
	s.render(w, http.StatusOK, "room", "invite", inv)
}
