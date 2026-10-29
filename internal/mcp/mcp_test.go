package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/t0uh33d/threadsplice/internal/api"
	"github.com/t0uh33d/threadsplice/internal/client"
	"github.com/t0uh33d/threadsplice/internal/e2ee"
	"github.com/t0uh33d/threadsplice/internal/relay"
)

// pair returns two agents in one room on a fresh relay.
func pair(t *testing.T) (alice, bob *client.Agent) {
	t.Helper()
	ctx := context.Background()
	const admin = "test-admin-token-0123456789"
	rs, err := relay.New(relay.Config{DBPath: filepath.Join(t.TempDir(), "r.db"), AdminToken: admin})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(rs.Handler())
	t.Cleanup(func() { ts.Close(); rs.Close() })
	m, err := client.New(ts.URL, admin).CreateRoom(ctx, "proj", "alice")
	if err != nil {
		t.Fatal(err)
	}
	key := e2ee.NewKey().String()
	alice, _ = client.NewAgent(client.Identity{Alias: "alice", Relay: ts.URL, RoomID: m.RoomID, RoomName: "proj", ParticipantID: m.ParticipantID, Name: "alice", Owner: true, Token: m.Token, RoomKey: key})
	inv, err := alice.Relay.CreateInvite(ctx, m.RoomID, time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	j, err := client.New(ts.URL, "").Join(ctx, inv.Token, "bob")
	if err != nil {
		t.Fatal(err)
	}
	bob, _ = client.NewAgent(client.Identity{Alias: "bob", Relay: ts.URL, RoomID: j.RoomID, RoomName: "proj", ParticipantID: j.ParticipantID, Name: "bob", Token: j.Token, RoomKey: key})
	return alice, bob
}

// joinRoom adds a member called name to owner's room.
func joinRoom(t *testing.T, owner *client.Agent, name string) *client.Agent {
	t.Helper()
	ctx := context.Background()
	inv, err := owner.Relay.CreateInvite(ctx, owner.ID.RoomID, time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	j, err := client.New(owner.ID.Relay, "").Join(ctx, inv.Token, name)
	if err != nil {
		t.Fatal(err)
	}
	a, err := client.NewAgent(client.Identity{Alias: name, Relay: owner.ID.Relay, RoomID: j.RoomID, RoomName: owner.ID.RoomName,
		ParticipantID: j.ParticipantID, Name: name, Token: j.Token, RoomKey: owner.ID.RoomKey})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

type session struct {
	t     *testing.T
	in    io.Writer
	lines chan map[string]any
	id    int
	logs  *syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *syncBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *syncBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

// start serves a on an in-process pipe with delivery mode deliver. The server
// has fully stopped, delivery loop included, once the test's cleanup ran.
func start(t *testing.T, a *client.Agent, deliver string) *session {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	logs := &syncBuf{}
	opt := Options{Deliver: deliver, Version: "test", Log: slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	done := make(chan struct{})
	go func() {
		if err := Serve(ctx, a, inR, outW, opt); err != nil {
			t.Errorf("Serve: %v", err)
		}
		outW.Close()
		close(done)
	}()
	t.Cleanup(func() { cancel(); inW.Close(); <-done })
	s := &session{t: t, in: inW, lines: make(chan map[string]any, 64), logs: logs}
	go func() {
		sc := bufio.NewScanner(outR)
		sc.Buffer(nil, 16<<20)
		for sc.Scan() {
			var m map[string]any
			if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
				t.Errorf("server wrote invalid JSON: %q", sc.Text())
			}
			s.lines <- m
		}
		close(s.lines)
	}()
	return s
}

func (s *session) write(m map[string]any) {
	m["jsonrpc"] = "2.0"
	b, _ := json.Marshal(m)
	s.in.Write(append(b, '\n'))
}

// call sends a request and returns its response, skipping interleaved notifications.
func (s *session) call(method string, params any) map[string]any {
	s.t.Helper()
	s.id++
	s.write(map[string]any{"id": s.id, "method": method, "params": params})
	for {
		m := s.next(10 * time.Second)
		if id, ok := m["id"].(float64); ok && int(id) == s.id {
			return m
		}
	}
}

func (s *session) next(d time.Duration) map[string]any {
	s.t.Helper()
	select {
	case m, ok := <-s.lines:
		if !ok {
			s.t.Fatal("server closed its output")
		}
		return m
	case <-time.After(d):
		s.t.Fatal("timed out waiting for server output")
		return nil
	}
}

// initialize runs the MCP handshake as the client clientName and returns the
// initialize result.
func (s *session) initialize(clientName string) map[string]any {
	s.t.Helper()
	res := s.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": clientName, "version": "x"}})
	s.write(map[string]any{"method": "notifications/initialized"})
	return res["result"].(map[string]any)
}

// tool calls a tool and returns its text, failing the test on a tool error.
func (s *session) tool(name string, args map[string]any) string {
	s.t.Helper()
	return toolText(s.t, s.call("tools/call", map[string]any{"name": name, "arguments": args}))
}

func toolText(t *testing.T, resp map[string]any) string {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", resp)
	}
	if res["isError"] == true {
		t.Fatalf("tool error: %v", res)
	}
	return res["content"].([]any)[0].(map[string]any)["text"].(string)
}

// An orchestrator hands several agents one task: each gets a direct copy,
// and ask (without live delivery) waits for every reply. A misspelt name
// sends nothing.
func TestAskSeveral(t *testing.T) {
	ctx := context.Background()
	alice, bob := pair(t)
	carol := joinRoom(t, alice, "carol")
	s := start(t, alice, "hold")
	s.call("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	answer := func(a *client.Agent, text string) {
		msgs, _ := a.Inbox(ctx, 10*time.Second)
		if len(msgs) == 1 && msgs[0].Direct && msgs[0].Kind == api.KindAsk {
			a.Send(ctx, "alice", api.Payload{Kind: api.KindReply, ReplyTo: msgs[0].ID, Text: text})
		}
	}

	go answer(bob, "bob: LGTM")
	go answer(carol, "carol: one nit")
	out := s.tool("ask", map[string]any{"to": "bob, carol,bob", "text": "review?", "wait_seconds": 10})
	if !strings.Contains(out, "bob: LGTM") || !strings.Contains(out, "carol: one nit") || strings.Contains(out, "No reply") || strings.Count(out, "(id ") != 2 {
		t.Fatalf("ask two: %s", out)
	}
	if out := s.toolErr("ask", map[string]any{"to": "bob,carl", "text": "typo"}); !strings.Contains(out, `"carl"`) {
		t.Fatalf("ask with a misspelt name: %s", out)
	}
	go answer(bob, "bob again")
	out = s.tool("ask", map[string]any{"to": "bob,carol", "text": "still there?", "wait_seconds": 2})
	if !strings.Contains(out, "bob again") || !strings.Contains(out, `No reply within 2s from "carol"`) {
		t.Fatalf("ask with a reply missing: %s", out)
	}
	if msgs, err := carol.Inbox(ctx, 0); err != nil || len(msgs) != 1 || msgs[0].Text != "still there?" {
		t.Fatalf("carol's inbox (the misspelt ask must not be in it): %+v, %v", msgs, err)
	}
	out = s.tool("send", map[string]any{"to": "bob,carol", "text": "thanks both"})
	if !strings.HasPrefix(out, `sent to "bob" (id `) || !strings.Contains(out, `, "carol" (id `) {
		t.Fatalf("send to two: %s", out)
	}
	for _, a := range []*client.Agent{bob, carol} {
		if msgs, err := a.Inbox(ctx, 5*time.Second); err != nil || len(msgs) != 1 || msgs[0].Text != "thanks both" || !msgs[0].Direct {
			t.Fatalf("%s's copy: %+v, %v", a.ID.Name, msgs, err)
		}
	}
}

func TestChannelSession(t *testing.T) {
	ctx := context.Background()
	alice, bob := pair(t)
	s := start(t, alice, "channel")

	init := s.call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "claude-code", "version": "x"}})
	res := init["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" {
		t.Fatalf("version not echoed: %v", res["protocolVersion"])
	}
	if _, ok := res["capabilities"].(map[string]any)["experimental"].(map[string]any)["claude/channel"]; !ok {
		t.Fatalf("channel capability missing: %v", res["capabilities"])
	}
	if !strings.Contains(res["instructions"].(string), `"alice"`) {
		t.Fatalf("instructions: %v", res["instructions"])
	}
	s.write(map[string]any{"method": "notifications/initialized"})

	if tools := s.call("tools/list", map[string]any{})["result"].(map[string]any)["tools"].([]any); len(tools) != 4 {
		t.Fatalf("tools/list returned %d tools", len(tools))
	}
	if e := s.call("bogus/method", nil)["error"].(map[string]any); e["code"].(float64) != -32601 {
		t.Fatalf("unknown method: %v", e)
	}
	if _, ok := s.call("ping", nil)["result"].(map[string]any); !ok {
		t.Fatal("ping")
	}

	// An incoming ask is pushed into the session as a channel event...
	askID, err := bob.Send(ctx, "alice", api.Payload{Kind: api.KindAsk, Text: "is store.go safe?", Depth: 2})
	if err != nil {
		t.Fatal(err)
	}
	ev := s.next(10 * time.Second)
	if ev["method"] != "notifications/claude/channel" {
		t.Fatalf("expected channel event, got %v", ev)
	}
	p := ev["params"].(map[string]any)
	meta := p["meta"].(map[string]any)
	if p["content"] != "is store.go safe?" || meta["from"] != "bob" || meta["kind"] != "ask" || meta["id"] != askID {
		t.Fatalf("channel event = %v", p)
	}

	// ...and answering it carries depth+1 for loop prevention.
	out := toolText(t, s.call("tools/call", map[string]any{"name": "send", "arguments": map[string]any{"to": "bob", "kind": "reply", "reply_to": askID, "text": "yes"}}))
	if !strings.Contains(out, "sent") {
		t.Fatalf("send: %s", out)
	}
	msgs, err := bob.Inbox(ctx, 5*time.Second)
	if err != nil || len(msgs) != 1 || msgs[0].ReplyTo != askID || msgs[0].Depth != 3 || msgs[0].Text != "yes" {
		t.Fatalf("bob got %+v %v", msgs, err)
	}
}

func TestToolSession(t *testing.T) {
	ctx := context.Background()
	alice, bob := pair(t)
	s := start(t, alice, "hold")
	s.call("initialize", map[string]any{"protocolVersion": "1999-01-01"}) // unknown version: server offers its newest
	s.write(map[string]any{"method": "notifications/initialized"})

	if out := toolText(t, s.call("tools/call", map[string]any{"name": "inbox", "arguments": map[string]any{}})); out != "No new messages." {
		t.Fatalf("empty inbox: %q", out)
	}

	// ask blocks until the peer replies.
	go func() {
		msgs, _ := bob.Inbox(ctx, 10*time.Second)
		if len(msgs) == 1 {
			bob.Send(ctx, "alice", api.Payload{Kind: api.KindReply, ReplyTo: msgs[0].ID, Text: "LGTM"})
		}
	}()
	out := toolText(t, s.call("tools/call", map[string]any{"name": "ask", "arguments": map[string]any{"to": "bob", "text": "review?", "wait_seconds": 10}}))
	if !strings.Contains(out, "LGTM") || strings.Contains(out, "No reply") {
		t.Fatalf("ask: %s", out)
	}

	resp := s.call("tools/call", map[string]any{"name": "send", "arguments": map[string]any{"to": "nobody", "text": "x"}})
	if resp["result"].(map[string]any)["isError"] != true {
		t.Fatalf("send to unknown peer should be a tool error: %v", resp)
	}
	if out := toolText(t, s.call("tools/call", map[string]any{"name": "peers", "arguments": map[string]any{}})); !strings.Contains(out, "bob") {
		t.Fatalf("peers: %s", out)
	}
}

// ask counts an answer only from the member it asked: the owner, who reads
// every direct message, can't end another member's wait with its own reply.
func TestAskNeedsTheRecipient(t *testing.T) {
	ctx := context.Background()
	alice, bob := pair(t)
	carol := joinRoom(t, alice, "carol")
	s := start(t, bob, "hold")
	s.call("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	go func() { // alice, the owner, answers bob's ask to carol in her stead
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			msgs, _, _ := alice.Log(ctx, 0, 0)
			for _, m := range msgs {
				if m.Kind == api.KindAsk && m.To == carol.ID.ParticipantID {
					alice.Send(ctx, "bob", api.Payload{Kind: api.KindReply, ReplyTo: m.ID, Text: "forged answer"})
					return
				}
			}
		}
	}()
	out := s.tool("ask", map[string]any{"to": "carol", "text": "ready?", "wait_seconds": 2})
	if !strings.Contains(out, "forged answer") || !strings.Contains(out, `No reply within 2s from "carol"`) {
		t.Fatalf("ask answered by someone else: %s", out)
	}
}

// A reused message id can only end a chain early, and a depth out of range
// counts as a chain that has used its budget up.
func TestDepthsOnlyGrow(t *testing.T) {
	ctx := context.Background()
	alice, bob := pair(t)
	carol := joinRoom(t, alice, "carol")
	s := start(t, alice, "hold")
	s.call("initialize", map[string]any{"protocolVersion": "2025-06-18"})
	bob.SendID(ctx, "M1", "alice", api.Payload{Kind: api.KindAsk, Text: "deep", Depth: 3})
	carol.SendID(ctx, "M1", "alice", api.Payload{Kind: api.KindAsk, Text: "same id, shallow"})
	bob.SendID(ctx, "M2", "alice", api.Payload{Kind: api.KindAsk, Text: "hostile", Depth: -1})
	if out := s.tool("inbox", map[string]any{"wait_seconds": 5}); strings.Count(out, "<threadsplice-message") != 3 {
		t.Fatalf("inbox: %s", out)
	}
	for id, want := range map[string]int{"M1": 4, "M2": client.MaxAutoDepth} {
		s.tool("send", map[string]any{"to": "bob", "kind": "reply", "reply_to": id, "text": "re " + id})
		msgs, err := bob.Inbox(ctx, 5*time.Second)
		if err != nil || len(msgs) != 1 || msgs[0].Depth < want || id == "M1" && msgs[0].Depth != want {
			t.Errorf("reply to %s: %+v, %v (want depth %d)", id, msgs, err, want)
		}
	}
}

// In channel mode, a message that doesn't decrypt is skipped, not pushed: it
// has no content for the model, and its sender and id are the relay's.
func TestChannelSkipsUndecryptable(t *testing.T) {
	ctx := context.Background()
	alice, bob := pair(t)
	s := start(t, alice, "channel")
	s.initialize("claude-code")
	if _, err := bob.Relay.Send(ctx, bob.ID.RoomID, api.SendRequest{ID: "forged", To: alice.ID.ParticipantID, Sealed: []byte("not sealed")}); err != nil {
		t.Fatal(err)
	}
	if _, err := bob.Send(ctx, "alice", api.Payload{Kind: api.KindMessage, Text: "genuine"}); err != nil {
		t.Fatal(err)
	}
	ev := s.next(10 * time.Second)
	if p, _ := ev["params"].(map[string]any); ev["method"] != "notifications/claude/channel" || p["content"] != "genuine" {
		t.Fatalf("first channel event: %v", ev)
	}
}
