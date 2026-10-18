package ui

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/t0uh33d/threadsplice/internal/api"
	"github.com/t0uh33d/threadsplice/internal/client"
	"github.com/t0uh33d/threadsplice/internal/hooks"
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
	Peers   peersView
	Compose composeView
	Flash   *inviteView
	TTLs    []ttlOption
	Targets []string
}

// agent loads the identity the {alias} path segment names and binds it to its
// relay. On failure it has answered the request already. Unlike client.Load,
// it never falls back to a default identity.
func (s *server) agent(w http.ResponseWriter, r *http.Request) (*client.Agent, bool) {
	alias := r.PathValue("alias")
	names, err := client.List()
	if err != nil || !client.ValidAlias(alias) || !slices.Contains(names, alias) {
		s.fail(w, r, http.StatusNotFound, "No such identity on this machine.")
		return nil, false
	}
	a, err := client.OpenAgent(alias)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, err.Error())
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
	p := roomPage{frame: s.frame(v.Room, v.Alias), Room: v, Peers: peers(ctx, rd, v), TTLs: ttls, Targets: hooks.SetupTargets}
	p.Feed = history(ctx, rd, v, 0)
	p.Compose = composeView{Room: v, Peers: p.Peers.recipients()}
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
	At                                     time.Time
	Depth                                  int
	Mine, ToMe                             bool
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

// history reads the room log after seq `after` through r (see reader) —
// read-only, nothing is acknowledged, so agents' inboxes are untouched — and
// keeps what v may see: the whole room for the owner, else what v may read
// plus what v sent.
func history(ctx context.Context, r *client.Agent, v roomView, after int64) feedView {
	f := feedView{Room: v, Last: after}
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
	if m.Err != nil {
		r.Err = m.Err.Error()
		return r
	}
	r.Kind, r.Text, r.ReplyTo, r.Depth = cmp.Or(m.Kind, api.KindMessage), m.Text, m.ReplyTo, m.Depth
	return r
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
	s.render(w, http.StatusOK, "room", "feed", history(ctx, reader(a), viewOf(a.ID), after))
}

// ---- participants ----

type peer struct {
	ID, Name, State             string
	Seen                        time.Time
	Online, Owner, You, Revoked bool
}

type peersView struct {
	Room  roomView
	Peers []peer
	Err   string
}

func peers(ctx context.Context, a *client.Agent, v roomView) peersView {
	p := peersView{Room: v}
	ps, err := a.Peers(ctx)
	if err != nil {
		p.Err = "Couldn't load participants: " + redact(err.Error(), a.ID.Token) + "."
		return p
	}
	for _, x := range ps {
		p.Peers = append(p.Peers, peer{ID: x.ID, Name: shown(x.Name), State: x.State, Seen: x.LastSeen,
			Online: x.Online, Owner: x.Owner, You: x.ID == v.PID, Revoked: x.Revoked})
	}
	return p
}

// recipients are the participants a message can go to.
func (p peersView) recipients() []peer {
	var out []peer
	for _, x := range p.Peers {
		if !x.You && !x.Revoked {
			out = append(out, x)
		}
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

// revoke removes a participant (owner only) and answers with the refreshed panel.
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
		code, msg = http.StatusForbidden, "Only the room owner can revoke participants."
	case pid == "" || pid == a.ID.ParticipantID:
		code, msg = http.StatusUnprocessableEntity, "Choose another participant to revoke."
	default:
		if err := a.Relay.Revoke(ctx, a.ID.RoomID, pid); err != nil {
			code, msg = http.StatusBadGateway, "Not revoked: "+redact(err.Error(), a.ID.Token)
		}
	}
	p := peers(ctx, a, viewOf(a.ID))
	p.Err = cmp.Or(msg, p.Err)
	s.render(w, code, "room", "participants", p)
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
	c := composeView{Room: viewOf(a.ID), To: r.PostFormValue("to"), Kind: r.PostFormValue("kind"),
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
			c.Text, c.Sent = "", true
			w.Header().Set("HX-Trigger", "ts-sent")
		}
	}
	c.Peers = peers(ctx, reader(a), c.Room).recipients()
	s.render(w, code, "room", "compose", c)
}

// ---- owner tools ----

type inviteView struct{ Invite, Command, Expires, Err string }

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
	if !tokenRe.MatchString(inv.Token) { // it ends up in a command line the user pastes
		return inviteView{}, errors.New("the relay returned a malformed invite token")
	}
	s := client.FormatInvite(id.Relay, inv.Token, key)
	return inviteView{Invite: s, Command: "threadsplice join -as <name> " + shellQuote(s),
		Expires: inv.ExpiresAt.Local().Format("Mon Jan 2 15:04")}, nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

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
	s.render(w, http.StatusOK, "room", "invite", inv)
}

type agentView struct {
	Name, Alias string
	Targets     []string
	Frame       frame // refreshes the sidebar
}

// addAgent creates an invite and redeems it here as a new identity, exactly
// as `threadsplice join` would on this machine.
func (s *server) addAgent(w http.ResponseWriter, r *http.Request) {
	a, ok := s.agent(w, r)
	if !ok {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	switch {
	case !a.ID.Owner:
		s.fail(w, r, http.StatusForbidden, "Only the room owner can add participants.")
		return
	case !api.ValidName(name):
		s.fail(w, r, http.StatusUnprocessableEntity, nameRule)
		return
	}
	if err := client.CheckRelayURL(a.ID.Relay); err != nil {
		s.fail(w, r, http.StatusUnprocessableEntity, err.Error())
		return
	}
	ctx, cancel := relayCtx(r)
	defer cancel()
	ps, err := a.Peers(ctx)
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "Couldn't reach the relay: "+redact(err.Error(), a.ID.Token))
		return
	}
	if slices.ContainsFunc(ps, func(p api.Participant) bool { return p.Name == name }) {
		s.fail(w, r, http.StatusUnprocessableEntity, fmt.Sprintf("%q is already taken in this room.", name))
		return
	}
	// Redeemed at once, so it only needs to live a minute.
	inv, err := a.Relay.CreateInvite(ctx, a.ID.RoomID, time.Minute)
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "No invite: "+redact(err.Error(), a.ID.Token))
		return
	}
	m, err := client.New(a.ID.Relay, "").Join(ctx, inv.Token, name)
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "Couldn't join: "+redact(err.Error(), a.ID.Token, inv.Token))
		return
	}
	id, err := saveNew(client.Identity{Relay: a.ID.Relay, RoomID: m.RoomID, RoomName: m.RoomName, ParticipantID: m.ParticipantID,
		Name: name, Token: m.Token, RoomKey: a.ID.RoomKey})
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, fmt.Sprintf("%s joined the room, but its identity couldn't be saved: %v", name, err))
		return
	}
	s.render(w, http.StatusOK, "room", "agent", agentView{Name: name, Alias: id.Alias, Targets: hooks.SetupTargets, Frame: s.frame("", a.ID.Alias)})
}
