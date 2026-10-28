package relay_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/t0uh33d/threadsplice/internal/api"
	"github.com/t0uh33d/threadsplice/internal/client"
	"github.com/t0uh33d/threadsplice/internal/e2ee"
	"github.com/t0uh33d/threadsplice/internal/relay"
	_ "modernc.org/sqlite"
)

const admin = "test-admin-token-0123456789"

func startRelay(t *testing.T, db string, perMinute int) *httptest.Server {
	t.Helper()
	return startRelayConfig(t, relay.Config{DBPath: db, AdminToken: admin, SendPerMinute: perMinute})
}

func startRelayConfig(t *testing.T, cfg relay.Config) *httptest.Server {
	t.Helper()
	s, err := relay.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); s.Close() })
	return ts
}

// rawDB opens a relay database directly, next to any relay using it.
func rawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+(&url.URL{Path: path}).EscapedPath()+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// room creates a room owned by `owner` and joins the others, returning agents in order.
func room(t *testing.T, base, name, owner string, others ...string) []*client.Agent {
	t.Helper()
	ctx := context.Background()
	m, err := client.New(base, admin).CreateRoom(ctx, name, owner)
	if err != nil {
		t.Fatal(err)
	}
	key := e2ee.NewKey()
	agents := []*client.Agent{mustAgent(t, client.Identity{Alias: owner, Relay: base, RoomID: m.RoomID, RoomName: name,
		ParticipantID: m.ParticipantID, Name: owner, Owner: true, Token: m.Token, RoomKey: key.String()})}
	for _, n := range others {
		inv, err := agents[0].Relay.CreateInvite(ctx, m.RoomID, time.Hour, 1)
		if err != nil {
			t.Fatal(err)
		}
		j, err := client.New(base, "").Join(ctx, inv.Token, n)
		if err != nil {
			t.Fatal(err)
		}
		agents = append(agents, mustAgent(t, client.Identity{Alias: n, Relay: base, RoomID: j.RoomID, RoomName: name,
			ParticipantID: j.ParticipantID, Name: n, Token: j.Token, RoomKey: key.String()}))
	}
	return agents
}

func mustAgent(t *testing.T, id client.Identity) *client.Agent {
	t.Helper()
	a, err := client.NewAgent(id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func status(err error) int {
	var se *client.StatusError
	if errors.As(err, &se) {
		return se.Code
	}
	return 0
}

func TestRelayEndToEnd(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "relay.db")
	ts := startRelay(t, db, 0)

	if _, err := client.New(ts.URL, "wrong-token-wrong-token").CreateRoom(ctx, "x", "y"); status(err) != 401 {
		t.Fatalf("room creation without admin token: %v", err)
	}

	ag := room(t, ts.URL, "proj", "alice", "bob", "carol")
	alice, bob, carol := ag[0], ag[1], ag[2]

	// Only the owner invites.
	if _, err := bob.Relay.CreateInvite(ctx, bob.ID.RoomID, time.Hour, 1); status(err) != 403 {
		t.Fatalf("non-owner invite: %v", err)
	}

	// DM reaches only its recipient; broadcasts reach everyone else; nobody gets their own.
	secret := "the deploy key is in vault, not the repo"
	if _, err := alice.Send(ctx, "bob", api.Payload{Kind: api.KindAsk, Text: secret}); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Send(ctx, "", api.Payload{Kind: api.KindMessage, Text: "hello room"}); err != nil {
		t.Fatal(err)
	}
	got := func(a *client.Agent) []string {
		msgs, err := a.Inbox(ctx, 0)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range msgs {
			if m.Err != nil {
				t.Fatalf("%s: undecryptable message: %v", a.ID.Name, m.Err)
			}
			out = append(out, m.FromName+":"+m.Text)
		}
		return out
	}
	if g := got(bob); len(g) != 1 || g[0] != "alice:"+secret {
		t.Fatalf("bob inbox = %q", g)
	}
	if g := got(carol); len(g) != 1 || g[0] != "bob:hello room" {
		t.Fatalf("carol inbox = %q (must not see alice->bob DM)", g)
	}
	if g := got(alice); len(g) != 1 || g[0] != "bob:hello room" {
		t.Fatalf("alice inbox = %q", g)
	}
	if g := got(bob); len(g) != 0 {
		t.Fatalf("bob inbox after ack = %q", g)
	}

	// The relay database never holds plaintext.
	for _, f := range []string{db, db + "-wal"} {
		if b, err := os.ReadFile(f); err == nil && bytes.Contains(b, []byte(secret)) {
			t.Fatalf("plaintext found in %s", f)
		}
	}

	// Retries with the same id are deduplicated.
	req := api.SendRequest{ID: "retry-1", Sealed: []byte{1, 2, 3}}
	r1, err := alice.Relay.Send(ctx, alice.ID.RoomID, req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := alice.Relay.Send(ctx, alice.ID.RoomID, req)
	if err != nil || r2.Seq != r1.Seq || !r2.Duplicate {
		t.Fatalf("duplicate send: %+v %+v %v", r1, r2, err)
	}

	// A long-poll wakes promptly when a message lands.
	bob.Inbox(ctx, 0)
	done := make(chan time.Duration, 1)
	start := time.Now()
	go func() {
		msgs, _, _ := bob.Fetch(ctx, -1, 10*time.Second, "idle")
		if len(msgs) == 1 {
			done <- time.Since(start)
		}
		close(done)
	}()
	time.Sleep(200 * time.Millisecond)
	alice.Send(ctx, "bob", api.Payload{Kind: api.KindMessage, Text: "ping"})
	if d, ok := <-done; !ok || d > 3*time.Second {
		t.Fatalf("long-poll did not wake (ok=%v, %v)", ok, d)
	}
	// ...and returns empty after its wait elapses.
	start = time.Now()
	if msgs, _, err := carol.Fetch(ctx, 1<<40, time.Second, ""); err != nil || len(msgs) != 0 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("empty long-poll: %d msgs, %v, %v", len(msgs), err, time.Since(start))
	}

	// Presence: bob reported state=idle on its poll.
	ps, err := alice.Peers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Name == "bob" && (p.State != "idle" || !p.Online) {
			t.Fatalf("bob presence = %+v", p)
		}
	}

	// Cross-room isolation: a credential works only in its own room.
	other := room(t, ts.URL, "other", "dave", "erin")
	if _, err := bob.Relay.Participants(ctx, other[0].ID.RoomID); status(err) != 404 {
		t.Fatalf("cross-room read: %v", err)
	}
	if _, err := bob.Relay.Send(ctx, bob.ID.RoomID, api.SendRequest{ID: "x1", To: other[1].ID.ParticipantID, Sealed: []byte{1}}); status(err) != 404 {
		t.Fatalf("cross-room recipient: %v", err)
	}

	// Revocation is immediate.
	if err := alice.Relay.Revoke(ctx, alice.ID.RoomID, bob.ID.ParticipantID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := bob.Fetch(ctx, -1, 0, ""); status(err) != 401 {
		t.Fatalf("revoked participant still reads: %v", err)
	}
	if _, err := alice.Send(ctx, "bob", api.Payload{Text: "x"}); err == nil {
		t.Fatal("sent to revoked participant")
	}
}

// The owner can watch the whole room, DMs between others included, without
// consuming anyone's messages; nobody else can.
func TestOwnerRoomLog(t *testing.T) {
	ctx := context.Background()
	ts := startRelay(t, filepath.Join(t.TempDir(), "relay.db"), 0)
	ag := room(t, ts.URL, "proj", "human", "claude", "codex")
	human, claude, codex := ag[0], ag[1], ag[2]

	if _, err := claude.Send(ctx, "codex", api.Payload{Kind: api.KindAsk, Text: "private review request"}); err != nil {
		t.Fatal(err)
	}
	msgs, last, err := human.Log(ctx, 0, 0)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "private review request" || msgs[0].FromName != "claude" || msgs[0].ToName != "codex" || last != msgs[0].Seq {
		t.Fatalf("owner log = %+v, last %d, %v", msgs, last, err)
	}
	// A non-owner's history has what it sent and received, never others' DMs.
	if _, err := codex.Send(ctx, "", api.Payload{Kind: api.KindMessage, Text: "hello room"}); err != nil {
		t.Fatal(err)
	}
	human.Send(ctx, "claude", api.Payload{Kind: api.KindMessage, Text: "for claude only"})
	hist, _, err := codex.Log(ctx, 0, 0)
	var texts []string
	for _, m := range hist {
		texts = append(texts, m.Text)
	}
	if err != nil || strings.Join(texts, "|") != "private review request|hello room" {
		t.Fatalf("codex history = %q, %v (want what it got and sent, not human->claude)", texts, err)
	}
	if msgs, _, _ = human.Log(ctx, 0, 0); len(msgs) != 3 {
		t.Fatalf("owner sees %d messages, want all 3", len(msgs))
	}
	last = msgs[len(msgs)-1].Seq
	if _, err := human.Relay.Messages(ctx, human.ID.RoomID, -1, 0, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := human.Relay.RoomLog(ctx, human.ID.RoomID, -1, 0); err != nil { // clamps to 0
		t.Fatal(err)
	}
	// Watching must not have consumed codex's message.
	if got, err := codex.Inbox(ctx, 0); err != nil || len(got) != 1 || got[0].Text != "private review request" {
		t.Fatalf("codex inbox after owner watched = %+v, %v", got, err)
	}
	// A watcher long-polls the log too.
	go func() {
		time.Sleep(200 * time.Millisecond)
		codex.Send(ctx, "claude", api.Payload{Kind: api.KindReply, Text: "lgtm"})
	}()
	start := time.Now()
	msgs, _, err = human.Log(ctx, last, 5*time.Second)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "lgtm" || time.Since(start) > 3*time.Second {
		t.Fatalf("log long-poll = %+v, %v after %v", msgs, err, time.Since(start))
	}
}

// A participant who joins late must not get the retained history as a flood
// of new messages, but can still read it.
func TestLateJoinerStartsAtCursor(t *testing.T) {
	ctx := context.Background()
	ts := startRelay(t, filepath.Join(t.TempDir(), "relay.db"), 0)
	ag := room(t, ts.URL, "proj", "alice", "bob")
	for i := range 3 {
		if _, err := ag[0].Send(ctx, "", api.Payload{Text: fmt.Sprint("old ", i)}); err != nil {
			t.Fatal(err)
		}
	}
	ag[1].Inbox(ctx, 0) // bob consumes them
	inv, err := ag[0].Relay.CreateInvite(ctx, ag[0].ID.RoomID, time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	j, err := client.New(ts.URL, "").Join(ctx, inv.Token, "carol")
	if err != nil {
		t.Fatal(err)
	}
	carol := mustAgent(t, client.Identity{Relay: ts.URL, RoomID: j.RoomID, RoomName: "proj", ParticipantID: j.ParticipantID, Name: "carol", Token: j.Token, RoomKey: ag[0].ID.RoomKey})
	if msgs, err := carol.Inbox(ctx, 0); err != nil || len(msgs) != 0 {
		t.Fatalf("late joiner inbox = %d messages, %v (want none)", len(msgs), err)
	}
	if hist, _, err := carol.Log(ctx, 0, 0); err != nil || len(hist) != 3 {
		t.Fatalf("late joiner history = %d, %v (want the 3 retained broadcasts)", len(hist), err)
	}
	ag[0].Send(ctx, "", api.Payload{Text: "new"})
	if msgs, err := carol.Inbox(ctx, 0); err != nil || len(msgs) != 1 || msgs[0].Text != "new" {
		t.Fatalf("late joiner after a new message = %+v, %v", msgs, err)
	}
}

func TestInvites(t *testing.T) {
	ctx := context.Background()
	ts := startRelay(t, filepath.Join(t.TempDir(), "relay.db"), 0)
	owner := room(t, ts.URL, "proj", "alice")[0]
	anon := client.New(ts.URL, "")

	inv, err := owner.Relay.CreateInvite(ctx, owner.ID.RoomID, time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := anon.Join(ctx, inv.Token, "alice"); status(err) != 409 {
		t.Fatalf("duplicate name: %v", err)
	}
	if _, err := anon.Join(ctx, inv.Token, "bad name!"); status(err) != 400 {
		t.Fatalf("invalid name: %v", err)
	}
	if _, err := anon.Join(ctx, inv.Token, "bob"); err != nil {
		t.Fatalf("invite consumed by a failed join: %v", err)
	}
	if _, err := anon.Join(ctx, inv.Token, "bob2"); status(err) != 403 {
		t.Fatalf("invite reused: %v", err)
	}
	if _, err := anon.Join(ctx, "not-a-real-invite", "zed"); status(err) != 403 {
		t.Fatalf("bogus invite: %v", err)
	}
	short, err := owner.Relay.CreateInvite(ctx, owner.ID.RoomID, time.Second, 1)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := anon.Join(ctx, short.Token, "late"); status(err) != 403 {
		t.Fatalf("expired invite: %v", err)
	}

	// A fleet invite admits its number of joins, then no more; a name clash
	// doesn't spend one.
	fleet, err := owner.Relay.CreateInvite(ctx, owner.ID.RoomID, time.Hour, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := anon.Join(ctx, fleet.Token, "worker-1"); err != nil {
		t.Fatalf("first use: %v", err)
	}
	if _, err := anon.Join(ctx, fleet.Token, "worker-1"); status(err) != 409 {
		t.Fatalf("name clash: %v", err)
	}
	if _, err := anon.Join(ctx, fleet.Token, "worker-2"); err != nil {
		t.Fatalf("second use: %v", err)
	}
	if _, err := anon.Join(ctx, fleet.Token, "worker-3"); status(err) != 403 {
		t.Fatalf("third use of a 2-use invite: %v", err)
	}
	for _, uses := range []int{-1, 101} {
		body := strings.NewReader(fmt.Sprintf(`{"uses": %d}`, uses))
		req, _ := http.NewRequest("POST", ts.URL+"/v1/rooms/"+owner.ID.RoomID+"/invites", body)
		req.Header.Set("Authorization", "Bearer "+owner.ID.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 400 {
			t.Errorf("invite with %d uses: status %d, want 400", uses, resp.StatusCode)
		}
	}
}

func TestLimits(t *testing.T) {
	ctx := context.Background()
	ts := startRelay(t, filepath.Join(t.TempDir(), "relay.db"), 3)
	a := room(t, ts.URL, "proj", "alice", "bob")[0]
	if _, err := a.Relay.Send(ctx, a.ID.RoomID, api.SendRequest{ID: "big", Sealed: make([]byte, 129<<10)}); status(err) != 413 {
		t.Fatalf("oversized message: %v", err)
	}
	for i := range 4 {
		_, err := a.Send(ctx, "", api.Payload{Text: "x"})
		if i < 3 && err != nil {
			t.Fatal(err)
		}
		if i == 3 && status(err) != 429 {
			t.Fatalf("rate limit not enforced: %v", err)
		}
	}
}

func TestDurableAcrossRestart(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "relay.db")
	ts := startRelay(t, db, 0)
	ag := room(t, ts.URL, "proj", "alice", "bob")
	if _, err := ag[0].Send(ctx, "bob", api.Payload{Kind: api.KindAsk, Text: "survive a restart"}); err != nil {
		t.Fatal(err)
	}
	ts.Close()

	ts2 := startRelay(t, db, 0) // same database, new process-equivalent
	bob := ag[1]
	bob.Relay = client.New(ts2.URL, bob.ID.Token)
	msgs, err := bob.Inbox(ctx, 0)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "survive a restart" {
		t.Fatalf("after restart: %+v %v", msgs, err)
	}
}

// childReply is the answer to a create-member request: a membership or an error.
type childReply struct {
	api.Membership
	Error string `json:"error"`
}

// createChild asks the relay, as machine, for the session member name with
// token hash hash. It returns the HTTP status, the only place where a retry's
// 200 and a new member's 201 differ.
func createChild(t *testing.T, machine *client.Agent, name, hash string) (int, childReply) {
	t.Helper()
	body, _ := json.Marshal(api.CreateChildRequest{Name: name, TokenHash: hash})
	req, err := http.NewRequest("POST", machine.ID.Relay+"/v1/rooms/"+machine.ID.RoomID+"/participants", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+machine.ID.Token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r childReply
	json.NewDecoder(resp.Body).Decode(&r)
	return resp.StatusCode, r
}

// member creates the session member name of machine through the client API.
func member(t *testing.T, machine *client.Agent, name string) *client.Agent {
	t.Helper()
	tok := client.NewToken()
	m, err := machine.Relay.CreateChild(context.Background(), machine.ID.RoomID, name, client.TokenHash(tok))
	if err != nil {
		t.Fatal(err)
	}
	if m.RoomID != machine.ID.RoomID || m.RoomName != machine.ID.RoomName || m.Token != "" {
		t.Fatalf("member %s: membership %+v", name, m)
	}
	return mustAgent(t, client.Identity{Alias: machine.ID.Alias + "/" + name, Relay: machine.ID.Relay, RoomID: m.RoomID,
		RoomName: m.RoomName, ParticipantID: m.ParticipantID, Name: name, Parent: machine.ID.ParticipantID,
		ParentAlias: machine.ID.Alias, Token: tok, RoomKey: machine.ID.RoomKey})
}

// Session members: a machine membership creates them under its own name, one
// level deep, and whatever binds the machine binds them too.
func TestChildren(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "relay.db")
	ts := startRelayConfig(t, relay.Config{DBPath: db, AdminToken: admin, MembersPerMinute: 100}) // room for the cap
	ag := room(t, ts.URL, "proj", "alice", "mac", "pc")
	alice, mac, pc := ag[0], ag[1], ag[2]
	if _, err := alice.Send(ctx, "", api.Payload{Text: "before any session joined"}); err != nil {
		t.Fatal(err)
	}

	// A member's name is its machine's name, a dot and a label; its token hash is lowercase hex.
	hash := client.TokenHash("some other token")
	for _, name := range []string{"mac", "mac.", "pc.reviewer", "macx.reviewer", "reviewer", "mac.bad name", "mac." + strings.Repeat("x", 29)} {
		if code, r := createChild(t, mac, name, hash); code != 400 || !strings.Contains(r.Error, `"mac."`) {
			t.Errorf("name %q: HTTP %d %q, want 400 stating the rule", name, code, r.Error)
		}
	}
	for _, h := range []string{"", strings.Repeat("A", 64), hash[:63], hash + "0", strings.Repeat("g", 64)} {
		if code, _ := createChild(t, mac, "mac.reviewer", h); code != 400 {
			t.Errorf("token hash %q: HTTP %d, want 400", h, code)
		}
	}

	// Creating is idempotent on the token hash: a retry gets the same member.
	tok := client.NewToken()
	code, r := createChild(t, mac, "mac.reviewer", client.TokenHash(tok))
	if code != 201 || r.ParticipantID == "" || r.RoomID != mac.ID.RoomID || r.RoomName != "proj" || r.Token != "" {
		t.Fatalf("create = %d %+v", code, r)
	}
	if code, again := createChild(t, mac, "mac.reviewer", client.TokenHash(tok)); code != 200 || again.Membership != r.Membership {
		t.Fatalf("retry = %d %+v, want 200 and %+v", code, again, r.Membership)
	}
	if code, _ := createChild(t, mac, "mac.reviewer", hash); code != 409 {
		t.Fatalf("taken name, other token: HTTP %d, want 409", code)
	}
	if code, _ := createChild(t, mac, "mac.copy", client.TokenHash(alice.ID.Token)); code != 409 {
		t.Fatalf("someone else's token: HTTP %d, want 409", code)
	}
	reviewer := mustAgent(t, client.Identity{Alias: "mac/mac.reviewer", Relay: ts.URL, RoomID: r.RoomID, RoomName: "proj",
		ParticipantID: r.ParticipantID, Name: "mac.reviewer", Parent: mac.ID.ParticipantID, ParentAlias: "mac", Token: tok, RoomKey: mac.ID.RoomKey})
	if _, err := reviewer.Relay.CreateChild(ctx, reviewer.ID.RoomID, "mac.reviewer.sub", hash); status(err) != 403 {
		t.Fatalf("member creating a member: %v", err)
	}

	// A member starts at the room's head, and its direct messages are its own.
	if msgs, err := reviewer.Inbox(ctx, 0); err != nil || len(msgs) != 0 {
		t.Fatalf("new member's inbox = %d messages, %v (want none of the history)", len(msgs), err)
	}
	if _, err := alice.Send(ctx, "mac.reviewer", api.Payload{Kind: api.KindAsk, Text: "review this"}); err != nil {
		t.Fatal(err)
	}
	if msgs, err := reviewer.Inbox(ctx, 0); err != nil || len(msgs) != 1 || msgs[0].Text != "review this" || msgs[0].FromName != "alice" {
		t.Fatalf("member's inbox = %+v, %v", msgs, err)
	}
	if msgs, err := mac.Inbox(ctx, 0); err != nil || len(msgs) != 1 || msgs[0].Text != "before any session joined" {
		t.Fatalf("machine's inbox = %+v, %v (want the broadcast, not its member's DM)", msgs, err)
	}

	// Members aren't owners: no invites, no revoking, no whole-room log.
	if _, err := reviewer.Relay.CreateInvite(ctx, reviewer.ID.RoomID, time.Hour, 1); status(err) != 403 {
		t.Fatalf("member invites: %v", err)
	}
	if err := reviewer.Relay.Revoke(ctx, reviewer.ID.RoomID, pc.ID.ParticipantID); status(err) != 403 {
		t.Fatalf("member revokes: %v", err)
	}
	if _, err := alice.Send(ctx, "pc", api.Payload{Text: "for pc only"}); err != nil {
		t.Fatal(err)
	}
	envs, err := reviewer.Relay.RoomLog(ctx, reviewer.ID.RoomID, 0, 0)
	if err != nil || len(envs) != 2 { // the broadcast and its own DM
		t.Fatalf("member's all=1 view = %d envelopes, %v", len(envs), err)
	}
	for _, e := range envs {
		if e.To != "" && e.To != reviewer.ID.ParticipantID {
			t.Fatalf("member read a DM to %s", e.To)
		}
	}

	// A top-level member's name and a dot are its sessions' namespace.
	anon := client.New(ts.URL, "")
	invite := func() string {
		inv, err := alice.Relay.CreateInvite(ctx, alice.ID.RoomID, time.Hour, 1)
		if err != nil {
			t.Fatal(err)
		}
		return inv.Token
	}
	inv := invite()
	for _, name := range []string{"mac.impostor", "mac.reviewer.x"} {
		if _, err := anon.Join(ctx, inv, name); status(err) != 409 || !strings.Contains(err.Error(), `belong to member "mac"`) {
			t.Fatalf("join as %q: %v", name, err)
		}
	}
	if _, err := anon.Join(ctx, inv, "macbook"); err != nil { // the refusals left the invite usable
		t.Fatalf("join as macbook: %v", err)
	}
	if _, err := anon.Join(ctx, invite(), "team.a"); err != nil {
		t.Fatalf("join as team.a: %v", err)
	}
	if _, err := anon.Join(ctx, invite(), "team.a.b"); status(err) != 409 || !strings.Contains(err.Error(), `belong to member "team.a"`) {
		t.Fatalf("join under a dotted name: %v", err)
	}

	// The participants list names each member's machine.
	ps, err := alice.Peers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		want := ""
		if p.Name == "mac.reviewer" {
			want = mac.ID.ParticipantID
		}
		if p.Parent != want {
			t.Errorf("%s: parent %q, want %q", p.Name, p.Parent, want)
		}
	}

	// At most 32 live members a machine; a retry still answers, and revoking one frees its slot.
	for i := 2; i <= 32; i++ {
		if code, r := createChild(t, mac, fmt.Sprint("mac.s", i), client.TokenHash(client.NewToken())); code != 201 {
			t.Fatalf("member %d: HTTP %d %q", i, code, r.Error)
		}
	}
	if code, r := createChild(t, mac, "mac.s33", client.TokenHash(client.NewToken())); code != 409 || !strings.Contains(r.Error, "already has 32 session members") {
		t.Fatalf("33rd member: HTTP %d %q", code, r.Error)
	}
	if code, _ := createChild(t, mac, "mac.reviewer", client.TokenHash(tok)); code != 200 {
		t.Fatalf("retry at the cap: HTTP %d", code)
	}
	if ps, err = alice.Peers(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Name == "mac.s2" {
			if err := alice.Relay.Revoke(ctx, alice.ID.RoomID, p.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if code, r := createChild(t, mac, "mac.s33", client.TokenHash(client.NewToken())); code != 201 {
		t.Fatalf("member after revoking one: HTTP %d %q", code, r.Error)
	}

	// Revoking a machine revokes its members at once, ending a member's waiting long-poll.
	polled := make(chan error, 1)
	go func() {
		_, _, err := reviewer.Fetch(ctx, -1, 10*time.Second, "")
		polled <- err
	}()
	time.Sleep(200 * time.Millisecond)
	if err := alice.Relay.Revoke(ctx, alice.ID.RoomID, mac.ID.ParticipantID); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-polled:
		if status(err) != 401 {
			t.Fatalf("member's long-poll after its machine was revoked: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("member's long-poll still waiting after its machine was revoked")
	}
	if _, err := reviewer.Send(ctx, "", api.Payload{Text: "still here?"}); status(err) != 401 {
		t.Fatalf("revoked member sends: %v", err)
	}
	if ps, err = alice.Peers(ctx); err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if strings.HasPrefix(p.Name, "mac.") && !p.Revoked {
			t.Errorf("%s outlived its machine's revocation", p.Name)
		}
	}

	// Defence in depth: a member whose machine is revoked is refused even without the cascade.
	px := member(t, pc, "pc.x")
	if _, err := rawDB(t, db).Exec(`UPDATE participants SET revoked_at = 1 WHERE id = ?`, pc.ID.ParticipantID); err != nil {
		t.Fatal(err)
	}
	if _, err := px.Peers(ctx); status(err) != 401 {
		t.Fatalf("member of a revoked machine: %v", err)
	}

	// A machine's members share its send budget, and creating members has a limit of its own.
	ts2 := startRelayConfig(t, relay.Config{DBPath: filepath.Join(t.TempDir(), "relay.db"), AdminToken: admin, SendPerMinute: 3})
	ag2 := room(t, ts2.URL, "proj", "alice", "mac")
	mac2 := ag2[1]
	sub := member(t, mac2, "mac.a")
	for _, a := range []*client.Agent{mac2, sub, sub} {
		if _, err := a.Send(ctx, "", api.Payload{Text: "x"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, a := range []*client.Agent{mac2, sub} {
		if _, err := a.Send(ctx, "", api.Payload{Text: "x"}); status(err) != 429 {
			t.Fatalf("%s past its machine's budget: %v", a.ID.Name, err)
		}
	}
	if _, err := ag2[0].Send(ctx, "", api.Payload{Text: "x"}); err != nil {
		t.Fatalf("another machine's budget: %v", err)
	}
	for i := 2; i <= 10; i++ { // mac.a was the first of 10 a minute
		if code, r := createChild(t, mac2, fmt.Sprint("mac.b", i), client.TokenHash(client.NewToken())); code != 201 {
			t.Fatalf("member %d: HTTP %d %q", i, code, r.Error)
		}
	}
	if code, _ := createChild(t, mac2, "mac.c", client.TokenHash(client.NewToken())); code != 429 {
		t.Fatalf("11th member in a minute: HTTP %d, want 429", code)
	}
	// The retry of a request whose answer was lost costs nothing: past the
	// limit it still gets its member, so the client keeps its token.
	if code, r := createChild(t, mac2, "mac.a", client.TokenHash(sub.ID.Token)); code != 200 || r.ParticipantID != sub.ID.ParticipantID {
		t.Fatalf("retry past the limit: HTTP %d %+v, want 200 and %s", code, r, sub.ID.ParticipantID)
	}
	if code, _ := createChild(t, mac2, "mac.a", client.TokenHash(client.NewToken())); code != 429 {
		t.Fatalf("taken name past the limit: HTTP %d, want 429", code)
	}
}

// Whoever joined first, and in whatever letter case, nobody passes as another
// member or as one of another member's sessions.
func TestNamespaces(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "relay.db")
	ts := startRelayConfig(t, relay.Config{DBPath: db, AdminToken: admin, MembersPerMinute: 100})
	alice := room(t, ts.URL, "proj", "alice")[0]
	join := func(name string) (*client.Agent, error) {
		t.Helper()
		inv, err := alice.Relay.CreateInvite(ctx, alice.ID.RoomID, time.Hour, 1)
		if err != nil {
			t.Fatal(err)
		}
		j, err := client.New(ts.URL, "").Join(ctx, inv.Token, name)
		if err != nil {
			return nil, err
		}
		return mustAgent(t, client.Identity{Alias: name, Relay: ts.URL, RoomID: j.RoomID, RoomName: "proj", ParticipantID: j.ParticipantID,
			Name: name, Token: j.Token, RoomKey: alice.ID.RoomKey}), nil
	}
	refused := func(name, want string) {
		t.Helper()
		if _, err := join(name); status(err) != 409 || !strings.Contains(err.Error(), want) {
			t.Errorf("join as %q: %v, want a 409 saying %s", name, err, want)
		}
	}

	// A dotted name that joined first keeps the name it would pass as a session of.
	if _, err := join("mac.reviewer"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mac", "MAC", "Mac"} {
		refused(name, `would make member "mac.reviewer", already in this room, look like one of its sessions`)
	}
	// Letter case tells neither names nor namespaces apart.
	pc, err := join("pc")
	if err != nil {
		t.Fatal(err)
	}
	refused("pc", `name "pc" is already taken in this room`)
	refused("PC", `name "PC" is already taken in this room, as "pc"`)
	refused("Alice", `as "alice"`)
	for _, name := range []string{"pc.reviewer", "Pc.reviewer", "PC.repo"} {
		refused(name, `belong to member "pc"'s sessions`)
	}
	pc2, err := join("pc-2") // where a defaulted name moves on to
	if err != nil {
		t.Fatal(err)
	}
	if code, r := createChild(t, pc, "pc.repo", client.TokenHash(client.NewToken())); code != 201 {
		t.Fatalf("pc.repo: HTTP %d %q", code, r.Error)
	}
	if code, r := createChild(t, pc, "pc.Repo", client.TokenHash(client.NewToken())); code != 409 || !strings.Contains(r.Error, `already taken in this room, as "pc.repo"`) {
		t.Errorf("pc.Repo next to pc.repo: HTTP %d %q", code, r.Error)
	}

	// Rooms from before these rules may hold names that clash already: no
	// member there creates a session that would pass as another member's.
	legacy := func(pid, name string) *client.Agent {
		t.Helper()
		tok := client.NewToken()
		h := sha256.Sum256([]byte(tok))
		if _, err := rawDB(t, db).Exec(`INSERT INTO participants (id, room_id, name, token_hash, created_at) VALUES (?, ?, ?, ?, ?)`,
			pid, alice.ID.RoomID, name, h[:], time.Now().UnixMilli()); err != nil {
			t.Fatal(err)
		}
		return mustAgent(t, client.Identity{Alias: name, Relay: ts.URL, RoomID: alice.ID.RoomID, RoomName: "proj", ParticipantID: pid,
			Name: name, Token: tok, RoomKey: alice.ID.RoomKey})
	}
	studio := legacy("LEGACY1", "pc.studio")
	if code, r := createChild(t, pc, "pc.studio.reviewer", client.TokenHash(client.NewToken())); code != 409 || !strings.Contains(r.Error, `belong to member "pc.studio"'s sessions`) {
		t.Errorf("pc creating a name in pc.studio's namespace: HTTP %d %q", code, r.Error)
	}
	if code, r := createChild(t, studio, "pc.studio.x", client.TokenHash(client.NewToken())); code != 201 {
		t.Errorf("pc.studio creating its own member: HTTP %d %q", code, r.Error)
	}
	upper := legacy("LEGACY2", "PC-2")
	for _, c := range []struct {
		machine     *client.Agent
		name, owner string
	}{{pc2, "pc-2.x", "PC-2"}, {upper, "PC-2.x", "pc-2"}} {
		if code, r := createChild(t, c.machine, c.name, client.TokenHash(client.NewToken())); code != 409 || !strings.Contains(r.Error, `belong to member "`+c.owner+`"'s sessions`) {
			t.Errorf("%s next to %s: HTTP %d %q", c.name, c.owner, code, r.Error)
		}
	}
}

// A machine the owner revokes while its request for a member is under way
// gets no member: one created then would have escaped the cascade.
func TestCreateChildRacesRevoke(t *testing.T) {
	ctx := context.Background()
	ts := startRelay(t, filepath.Join(t.TempDir(), "relay.db"), 0)
	ag := room(t, ts.URL, "proj", "alice", "mac")
	alice, mac := ag[0], ag[1]
	body, w := io.Pipe()
	req, err := http.NewRequest("POST", ts.URL+"/v1/rooms/"+mac.ID.RoomID+"/participants", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+mac.ID.Token)
	answer := make(chan int, 1)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			answer <- 0
			return
		}
		resp.Body.Close()
		answer <- resp.StatusCode
	}()
	w.Write([]byte(`{"name":"mac.late",`)) // sends the headers: the relay authenticates mac and waits for the rest
	seen := func() bool {
		ps, err := alice.Peers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range ps {
			if p.ID == mac.ID.ParticipantID {
				return !p.LastSeen.IsZero()
			}
		}
		return false
	}
	for deadline := time.Now().Add(5 * time.Second); !seen(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the relay never authenticated the request")
		}
	}
	if err := alice.Relay.Revoke(ctx, alice.ID.RoomID, mac.ID.ParticipantID); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(w, `"token_hash":%q}`, client.TokenHash(client.NewToken()))
	w.Close()
	if code := <-answer; code != 401 {
		t.Errorf("creating a member as a machine revoked meanwhile: HTTP %d, want 401", code)
	}
	ps, err := alice.Peers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Name == "mac.late" {
			t.Errorf("the relay created %+v under a revoked machine", p)
		}
	}
}

// v1Schema is the schema v1 relays created, before session members.
const v1Schema = `
CREATE TABLE rooms (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	created_at INTEGER NOT NULL
);
CREATE TABLE participants (
	id         TEXT PRIMARY KEY,
	room_id    TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
	name       TEXT NOT NULL,
	token_hash BLOB NOT NULL UNIQUE,
	owner      INTEGER NOT NULL DEFAULT 0,
	state      TEXT NOT NULL DEFAULT '',
	last_seen  INTEGER NOT NULL DEFAULT 0,
	acked_seq  INTEGER NOT NULL DEFAULT 0,
	revoked_at INTEGER,
	created_at INTEGER NOT NULL,
	UNIQUE (room_id, name),
	UNIQUE (room_id, id)
);
CREATE TABLE invites (
	token_hash BLOB PRIMARY KEY,
	room_id    TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
	created_by TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at    INTEGER,
	FOREIGN KEY (room_id, created_by) REFERENCES participants (room_id, id) ON DELETE CASCADE
);
CREATE TABLE messages (
	seq          INTEGER PRIMARY KEY AUTOINCREMENT,
	room_id      TEXT NOT NULL REFERENCES rooms (id) ON DELETE CASCADE,
	id           TEXT NOT NULL,
	sender_id    TEXT NOT NULL,
	recipient_id TEXT,
	sealed       BLOB NOT NULL,
	created_at   INTEGER NOT NULL,
	UNIQUE (sender_id, id),
	FOREIGN KEY (room_id, sender_id) REFERENCES participants (room_id, id) ON DELETE CASCADE,
	FOREIGN KEY (room_id, recipient_id) REFERENCES participants (room_id, id) ON DELETE CASCADE
);
CREATE INDEX messages_room_seq ON messages (room_id, seq);
PRAGMA user_version = 1;
`

// v2Additions is what v2 relays added to v1Schema: session members.
const v2Additions = `
CREATE TABLE children (
	room_id   TEXT NOT NULL,
	child_id  TEXT PRIMARY KEY,
	parent_id TEXT NOT NULL,
	CHECK (child_id <> parent_id),
	FOREIGN KEY (room_id, child_id)  REFERENCES participants (room_id, id) ON DELETE CASCADE,
	FOREIGN KEY (room_id, parent_id) REFERENCES participants (room_id, id) ON DELETE CASCADE
);
CREATE INDEX children_parent ON children (parent_id);
CREATE TRIGGER children_one_level BEFORE INSERT ON children
WHEN EXISTS (SELECT 1 FROM children WHERE child_id = NEW.parent_id)
	OR EXISTS (SELECT 1 FROM children WHERE parent_id = NEW.child_id)
BEGIN SELECT RAISE(ABORT, 'session members are one level deep'); END;
PRAGMA user_version = 2;
`

// A v1 or v2 database migrates when a relay opens it: its data survives,
// user_version becomes 3, members stay one level deep, and an invite made
// before the upgrade still admits exactly one join. A fresh database starts
// at v3, and a newer one is refused.
func TestMigrate(t *testing.T) {
	t.Run("v1", func(t *testing.T) { testMigrate(t, v1Schema) })
	t.Run("v2", func(t *testing.T) { testMigrate(t, v1Schema+v2Additions) })

	var v int
	fresh := filepath.Join(t.TempDir(), "fresh.db")
	startRelay(t, fresh, 0)
	if err := rawDB(t, fresh).QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 3 {
		t.Fatalf("fresh database's user_version = %d, %v; want 3", v, err)
	}
	newer := filepath.Join(t.TempDir(), "newer.db")
	if _, err := rawDB(t, newer).Exec("PRAGMA user_version = 4"); err != nil {
		t.Fatal(err)
	}
	if _, err := relay.New(relay.Config{DBPath: newer, AdminToken: admin}); err == nil || !strings.Contains(err.Error(), "newer than this relay (v3)") {
		t.Fatalf("opening a v4 database: %v", err)
	}
}

func testMigrate(t *testing.T, ddl string) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.db")
	old := rawDB(t, path)
	if _, err := old.Exec(ddl); err != nil {
		t.Fatal(err)
	}
	key := e2ee.NewKey()
	hash := func(tok string) []byte { h := sha256.Sum256([]byte(tok)); return h[:] }
	sealed := key.Seal(e2ee.Header{Room: "R1", From: "P1", To: "P2", ID: "m1"}, []byte(`{"kind":"ask","text":"sent before the upgrade"}`))
	now := time.Now().UnixMilli()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO rooms (id, name, created_at) VALUES ('R1', 'proj', ?)`, []any{now}},
		{`INSERT INTO participants (id, room_id, name, token_hash, owner, created_at) VALUES ('P1', 'R1', 'alice', ?, 1, ?)`, []any{hash("alice-token"), now}},
		{`INSERT INTO participants (id, room_id, name, token_hash, created_at) VALUES ('P2', 'R1', 'bob', ?, ?)`, []any{hash("bob-token"), now}},
		{`INSERT INTO messages (room_id, id, sender_id, recipient_id, sealed, created_at) VALUES ('R1', 'm1', 'P1', 'P2', ?, ?)`, []any{sealed, now}},
		{`INSERT INTO invites (token_hash, room_id, created_by, expires_at) VALUES (?, 'R1', 'P1', ?)`, []any{hash("old-invite"), now + time.Hour.Milliseconds()}},
	} {
		if _, err := old.Exec(q.sql, q.args...); err != nil {
			t.Fatal(err)
		}
	}
	old.Close()

	ts := startRelay(t, path, 0)
	id := func(pid, name, tok string) client.Identity {
		return client.Identity{Alias: name, Relay: ts.URL, RoomID: "R1", RoomName: "proj", ParticipantID: pid, Name: name,
			Owner: pid == "P1", Token: tok, RoomKey: key.String()}
	}
	alice, bob := mustAgent(t, id("P1", "alice", "alice-token")), mustAgent(t, id("P2", "bob", "bob-token"))
	if msgs, err := bob.Inbox(ctx, 0); err != nil || len(msgs) != 1 || msgs[0].Text != "sent before the upgrade" || msgs[0].FromName != "alice" {
		t.Fatalf("bob's inbox after the upgrade = %+v, %v", msgs, err)
	}
	sub := member(t, bob, "bob.reviewer")
	ps, err := alice.Peers(ctx)
	if err != nil || len(ps) != 3 {
		t.Fatalf("participants after the upgrade = %+v, %v", ps, err)
	}
	for _, p := range ps {
		if (p.Name == "bob.reviewer") != (p.Parent == "P2") {
			t.Errorf("%s: parent %q", p.Name, p.Parent)
		}
	}
	anon := client.New(ts.URL, "")
	if _, err := anon.Join(ctx, "old-invite", "carol"); err != nil {
		t.Fatalf("an invite from before the upgrade: %v", err)
	}
	if _, err := anon.Join(ctx, "old-invite", "dave"); status(err) != 403 {
		t.Fatalf("an invite from before the upgrade, used again: %v", err)
	}

	db := rawDB(t, path)
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 3 {
		t.Fatalf("user_version = %d, %v; want 3", v, err)
	}
	for _, pair := range [][]any{
		{"P1", sub.ID.ParticipantID}, // a member under a member
		{"P2", "P1"},                 // a machine with members of its own under another
	} {
		if _, err := db.Exec(`INSERT INTO children (room_id, child_id, parent_id) VALUES ('R1', ?, ?)`, pair...); err == nil || !strings.Contains(err.Error(), "one level deep") {
			t.Errorf("child %v: %v, want the one-level trigger", pair, err)
		}
	}
}
