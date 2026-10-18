package relay_test

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/t0uh33d/threadsplice/internal/api"
	"github.com/t0uh33d/threadsplice/internal/client"
	"github.com/t0uh33d/threadsplice/internal/e2ee"
	"github.com/t0uh33d/threadsplice/internal/relay"
)

const admin = "test-admin-token-0123456789"

func startRelay(t *testing.T, db string, perMinute int) *httptest.Server {
	t.Helper()
	s, err := relay.New(relay.Config{DBPath: db, AdminToken: admin, SendPerMinute: perMinute})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() { ts.Close(); s.Close() })
	return ts
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
		inv, err := agents[0].Relay.CreateInvite(ctx, m.RoomID, time.Hour)
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
	if _, err := bob.Relay.CreateInvite(ctx, bob.ID.RoomID, time.Hour); status(err) != 403 {
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

func TestInvites(t *testing.T) {
	ctx := context.Background()
	ts := startRelay(t, filepath.Join(t.TempDir(), "relay.db"), 0)
	owner := room(t, ts.URL, "proj", "alice")[0]
	anon := client.New(ts.URL, "")

	inv, err := owner.Relay.CreateInvite(ctx, owner.ID.RoomID, time.Hour)
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
	short, err := owner.Relay.CreateInvite(ctx, owner.ID.RoomID, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond)
	if _, err := anon.Join(ctx, short.Token, "late"); status(err) != 403 {
		t.Fatalf("expired invite: %v", err)
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
