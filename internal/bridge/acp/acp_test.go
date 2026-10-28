package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/t0uh33d/threadsplice/internal/api"
	"github.com/t0uh33d/threadsplice/internal/bridge"
	"github.com/t0uh33d/threadsplice/internal/client"
	"github.com/t0uh33d/threadsplice/internal/e2ee"
	"github.com/t0uh33d/threadsplice/internal/relay"
)

// The test binary doubles as a fake ACP agent, so the tests go through a real
// process: Start runs `<test binary> -fake-acp-agent TRANSCRIPT [FLAG...]`.
func TestMain(m *testing.M) {
	if len(os.Args) > 2 && os.Args[1] == "-fake-acp-agent" {
		fakeAgent(os.Args[2], os.Args[3:])
	}
	// A -race fake agent would otherwise linger a second at every exit.
	os.Setenv("GORACE", strings.TrimSpace("atexit_sleep_ms=0 "+os.Getenv("GORACE")))
	os.Exit(m.Run())
}

// fakeAgent speaks just enough ACP for these tests and appends every line the
// client sends to transcript. What a turn does depends on an @word in the
// prompt (never part of an envelope's own text).
// Flags: "auth" fails session/new with auth_required; "noload" denies loadSession.
func fakeAgent(transcript string, flags []string) {
	rec, err := os.OpenFile(transcript, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Exit(2)
	}
	in := bufio.NewReader(os.Stdin)
	enc := json.NewEncoder(os.Stdout)
	read := func() map[string]any {
		line, err := in.ReadBytes('\n')
		if err != nil {
			os.Exit(0) // stdin closed: the client wants us gone
		}
		rec.Write(line)
		var m map[string]any
		json.Unmarshal(line, &m)
		return m
	}
	send := func(m map[string]any) {
		m["jsonrpc"] = "2.0"
		enc.Encode(m)
	}
	update := func(sid string, u map[string]any) {
		send(map[string]any{"method": "session/update", "params": map[string]any{"sessionId": sid, "update": u}})
	}
	say := func(sid, text string) {
		update(sid, map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": text}})
	}
	n := 0
	ask := func(method string, params map[string]any) { // a request to the client; waits for its answer
		n++
		id := fmt.Sprintf("agent-%d", n)
		send(map[string]any{"id": id, "method": method, "params": params})
		for read()["id"] != id {
		}
	}
	opts := func(kinds ...string) []any {
		var out []any
		for _, k := range kinds {
			out = append(out, map[string]any{"optionId": strings.ReplaceAll(k, "_", "-"), "name": k, "kind": k})
		}
		return out
	}
	for {
		m := read()
		p, _ := m["params"].(map[string]any)
		result := func(r any) { send(map[string]any{"id": m["id"], "result": r}) }
		switch m["method"] {
		case "initialize":
			result(map[string]any{"protocolVersion": 1, "agentCapabilities": map[string]any{"loadSession": !slices.Contains(flags, "noload")},
				"agentInfo": map[string]any{"name": "fake", "version": "1"}, "authMethods": []any{}})
		case "session/new":
			if slices.Contains(flags, "auth") {
				send(map[string]any{"id": m["id"], "error": map[string]any{"code": -32000, "message": "Authentication required"}})
				continue
			}
			result(map[string]any{"sessionId": "sess-new"})
			update("sess-new", map[string]any{"sessionUpdate": "available_commands_update", "availableCommands": []any{}})
		case "session/load":
			sid := p["sessionId"].(string)
			update(sid, map[string]any{"sessionUpdate": "user_message_chunk", "content": map[string]any{"type": "text", "text": "old question"}})
			say(sid, "old answer")
			result(map[string]any{})
		case "session/prompt":
			sid := p["sessionId"].(string)
			text := p["prompt"].([]any)[0].(map[string]any)["text"].(string)
			switch {
			case strings.Contains(text, "@tool"):
				say(sid, "Let me run that.")
				update(sid, map[string]any{"sessionUpdate": "tool_call", "toolCallId": "call-1", "title": "rm -rf /", "kind": "execute", "status": "pending"})
				tool := map[string]any{"toolCallId": "call-1"}
				ask("session/request_permission", map[string]any{"sessionId": sid, "toolCall": tool,
					"options": opts("allow_once", "allow_always", "reject_always", "reject_once")})
				ask("session/request_permission", map[string]any{"sessionId": sid, "toolCall": tool, "options": opts("allow_once")})
				ask("fs/read_text_file", map[string]any{"sessionId": sid, "path": "/etc/passwd"})
				ask("terminal/create", map[string]any{"sessionId": sid, "command": "sh"})
				update(sid, map[string]any{"sessionUpdate": "tool_call_update", "toolCallId": "call-1", "status": "failed"})
				say(sid, "The answer")
				say(sid, " is 4.")
				result(map[string]any{"stopReason": "end_turn"})
			case strings.Contains(text, "@refuse"):
				say(sid, "No.")
				result(map[string]any{"stopReason": "refusal"})
			case strings.Contains(text, "@hang"): // until cancelled
				for read()["method"] != "session/cancel" {
				}
				ask("session/request_permission", map[string]any{"sessionId": sid, "toolCall": map[string]any{"toolCallId": "call-2"},
					"options": opts("allow_once", "reject_once")})
				result(map[string]any{"stopReason": "cancelled"})
			case strings.Contains(text, "@wedged"): // never ends the cancelled turn
				for read()["method"] != "session/cancel" {
				}
				for {
					read()
				}
			case strings.Contains(text, "@noise"):
				os.Stdout.WriteString("Debugger listening on ws://127.0.0.1:9229\n") // not ACP: skipped
				update("sess-other", map[string]any{"sessionUpdate": "agent_message_chunk", "content": map[string]any{"type": "text", "text": "not ours"}})
				update(sid, map[string]any{"sessionUpdate": "agent_thought_chunk", "content": map[string]any{"type": "text", "text": "hmm"}})
				say(sid, "ok")
				result(map[string]any{"stopReason": "end_turn"})
			case strings.Contains(text, "@stop="): // ends the turn with the stop reason that follows
				say(sid, "partial")
				result(map[string]any{"stopReason": strings.Fields(text[strings.Index(text, "@stop=")+len("@stop="):])[0]})
			case strings.Contains(text, "@stubborn"): // ignores the cancel for a while
				for read()["method"] != "session/cancel" {
				}
				time.Sleep(time.Second)
				say(sid, "late words")
				result(map[string]any{"stopReason": "end_turn"})
			case strings.Contains(text, "@die"):
				os.Exit(3)
			default:
				say(sid, "Hello")
				say(sid, ", world")
				result(map[string]any{"stopReason": "end_turn"})
			}
		}
	}
}

// fake returns Options that run the fake agent in a fresh directory, and a
// function that reads back every message the client sent it.
func fake(t *testing.T, flags ...string) (Options, func() []map[string]any) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript")
	opt := Options{Command: append([]string{os.Args[0], "-fake-acp-agent", path}, flags...), Cwd: dir, Stderr: os.Stderr}
	return opt, func() []map[string]any {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("client sent invalid JSON: %q", line)
			}
			if m["jsonrpc"] != "2.0" {
				t.Errorf("client sent a message without jsonrpc 2.0: %s", line)
			}
			out = append(out, m)
		}
		return out
	}
}

func open(t *testing.T, opt Options) *Session {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Start(ctx, opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close) // before t.TempDir's cleanup: Windows won't remove a live process's cwd
	return s
}

// sent returns the first message with the given method, or the client's
// answer to the agent request with the given id.
func sent(t *testing.T, msgs []map[string]any, methodOrID string) map[string]any {
	t.Helper()
	for _, m := range msgs {
		if m["method"] == methodOrID || (m["method"] == nil && m["id"] == methodOrID) {
			return m
		}
	}
	t.Fatalf("client never sent %s", methodOrID)
	return nil
}

func compact(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestStart(t *testing.T) {
	opt, transcript := fake(t)
	opt.Cwd = "." // made absolute before it reaches the agent
	s := open(t, opt)
	if s.ID() != "sess-new" {
		t.Errorf("ID = %q, want the agent's new session", s.ID())
	}
	msgs := transcript()
	if msgs[0]["method"] != "initialize" {
		t.Fatalf("first message = %v, want initialize", msgs[0])
	}
	p := msgs[0]["params"].(map[string]any)
	if p["protocolVersion"] != 1.0 {
		t.Errorf("protocolVersion = %v", p["protocolVersion"])
	}
	if caps := compact(p["clientCapabilities"]); caps != `{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false}` {
		t.Errorf("clientCapabilities = %s, want nothing offered", caps)
	}
	n := sent(t, msgs, "session/new")["params"].(map[string]any)
	wd, _ := os.Getwd()
	if cwd, _ := n["cwd"].(string); !filepath.IsAbs(cwd) || cwd != wd {
		t.Errorf("session/new cwd = %v, want %s", n["cwd"], wd)
	}
	if servers := compact(n["mcpServers"]); servers != "[]" {
		t.Errorf("mcpServers = %s, want []", servers)
	}
}

func TestTurnAnswersAfterLastToolCallAndDeclinesEverything(t *testing.T) {
	opt, transcript := fake(t)
	s := open(t, opt)
	answer, err := s.Turn(context.Background(), "please @tool")
	if err != nil || answer != "The answer is 4." {
		t.Fatalf("Turn = %q, %v; want only the text after the last tool call", answer, err)
	}
	msgs := transcript()
	p := sent(t, msgs, "session/prompt")["params"].(map[string]any)
	if p["sessionId"] != "sess-new" || compact(p["prompt"]) != `[{"text":"please @tool","type":"text"}]` {
		t.Errorf("session/prompt params = %s", compact(p))
	}
	for id, want := range map[string]string{
		"agent-1": `{"outcome":{"optionId":"reject-once","outcome":"selected"}}`, // reject_once beats reject_always; never allow
		"agent-2": `{"outcome":{"outcome":"cancelled"}}`,                         // no way to say no
	} {
		if got := compact(sent(t, msgs, id)["result"]); got != want {
			t.Errorf("permission answer %s = %s, want %s", id, got, want)
		}
	}
	for _, id := range []string{"agent-3", "agent-4"} { // fs/read_text_file, terminal/create
		if e, _ := sent(t, msgs, id)["error"].(map[string]any); e["code"] != -32601.0 {
			t.Errorf("answer %s = %v, want method not found", id, sent(t, msgs, id))
		}
	}
}

func TestRefusalIsAnError(t *testing.T) {
	opt, _ := fake(t)
	s := open(t, opt)
	if answer, err := s.Turn(context.Background(), "@refuse this"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("Turn = %q, %v; want a refusal error", answer, err)
	}
}

func TestTurnTimeoutCancels(t *testing.T) {
	opt, transcript := fake(t)
	opt.TurnTimeout = 200 * time.Millisecond
	s := open(t, opt)
	if _, err := s.Turn(context.Background(), "@hang"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Turn err = %v, want a timeout", err)
	}
	msgs := transcript()
	c := sent(t, msgs, "session/cancel")
	if _, isRequest := c["id"]; isRequest || compact(c["params"]) != `{"sessionId":"sess-new"}` {
		t.Errorf("cancel = %s, want a session/cancel notification", compact(c))
	}
	// ACP: once the client cancels, its answer to the turn's permission requests is "cancelled".
	if got := compact(sent(t, msgs, "agent-1")["result"]); got != `{"outcome":{"outcome":"cancelled"}}` {
		t.Errorf("permission answer after session/cancel = %s, want the cancelled outcome", got)
	}
	if answer, err := s.Turn(context.Background(), "hi"); err != nil || answer != "Hello, world" {
		t.Errorf("next Turn = %q, %v", answer, err)
	}
}

// An agent that never ends a cancelled turn is killed rather than left
// refusing every turn for good.
func TestStuckAgentIsKilled(t *testing.T) {
	defer func(c, s time.Duration) { cancelGrace, stuckGrace = c, s }(cancelGrace, stuckGrace)
	cancelGrace, stuckGrace = 100*time.Millisecond, 300*time.Millisecond
	opt, _ := fake(t)
	opt.TurnTimeout = 100 * time.Millisecond
	s := open(t, opt)
	if _, err := s.Turn(context.Background(), "@wedged"); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Turn err = %v, want a timeout", err)
	}
	if _, err := s.Turn(context.Background(), "hi"); err == nil || !strings.Contains(err.Error(), "still finishing") {
		t.Fatalf("Turn during the cancelled one: err = %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("the stuck agent is still running")
	}
}

func TestTurnWithEndedContextSendsNothing(t *testing.T) {
	opt, transcript := fake(t)
	s := open(t, opt)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.Turn(ctx, "hi"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Turn err = %v, want context.Canceled", err)
	}
	for _, m := range transcript() {
		if m["method"] == "session/prompt" || m["method"] == "session/cancel" {
			t.Errorf("sent %v for a turn whose context had already ended", m["method"])
		}
	}
}

func TestStopReasons(t *testing.T) {
	opt, _ := fake(t)
	s := open(t, opt)
	for reason, ok := range map[string]bool{"max_tokens": true, "max_turn_requests": true, "cancelled": false, "error": false} {
		answer, err := s.Turn(context.Background(), "@stop="+reason)
		if ok && (err != nil || answer != "partial") {
			t.Errorf("%s: Turn = %q, %v; want what the agent said", reason, answer, err)
		}
		if !ok && (err == nil || answer != "") {
			t.Errorf("%s: Turn = %q, %v; want an error", reason, answer, err)
		}
	}
}

func TestIgnoresNoiseOtherSessionsAndThoughts(t *testing.T) {
	opt, _ := fake(t)
	s := open(t, opt)
	if answer, err := s.Turn(context.Background(), "@noise"); err != nil || answer != "ok" {
		t.Fatalf("Turn = %q, %v; want only this session's message text", answer, err)
	}
}

// A relative agent path means the one in our directory, not one in the
// session's directory, where the agent runs.
func TestRelativeCommand(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Skip(err)
	}
	t.Chdir(filepath.Dir(exe))
	opt, _ := fake(t)
	opt.Command[0] = "." + string(filepath.Separator) + filepath.Base(exe)
	s := open(t, opt)
	if answer, err := s.Turn(context.Background(), "hi"); err != nil || answer != "Hello, world" {
		t.Fatalf("Turn = %q, %v", answer, err)
	}
}

// An agent that ignores session/cancel keeps its session off limits until it
// answers, so its late words can't end up in the next peer's reply.
func TestNoTurnUntilCancelledTurnEnds(t *testing.T) {
	defer func(g time.Duration) { cancelGrace = g }(cancelGrace)
	cancelGrace = 100 * time.Millisecond
	opt, _ := fake(t)
	opt.TurnTimeout = 100 * time.Millisecond
	s := open(t, opt)
	if _, err := s.Turn(context.Background(), "@stubborn"); err == nil {
		t.Fatal("stubborn turn did not time out")
	}
	if _, err := s.Turn(context.Background(), "hi"); err == nil || !strings.Contains(err.Error(), "still finishing") {
		t.Fatalf("Turn during the cancelled one: err = %v", err)
	}
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		answer, err := s.Turn(context.Background(), "hi")
		if err != nil && strings.Contains(err.Error(), "still finishing") {
			continue
		}
		if err != nil || answer != "Hello, world" {
			t.Fatalf("Turn = %q, %v", answer, err)
		}
		return
	}
	t.Fatal("session never became usable again")
}

func TestAgentExitFailsTurn(t *testing.T) {
	opt, _ := fake(t)
	opt.TurnTimeout = time.Minute // a hang would show up as a timeout, not an exit
	s := open(t, opt)
	if _, err := s.Turn(context.Background(), "@die"); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("Turn err = %v, want the agent's exit", err)
	}
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done not closed after the agent exited")
	}
	if s.Err() == nil {
		t.Error("Err = nil after the agent exited")
	}
	if _, err := s.Turn(context.Background(), "hi"); err == nil {
		t.Error("Turn on an exited agent succeeded")
	}
}

func TestLoadSessionIgnoresReplay(t *testing.T) {
	opt, transcript := fake(t)
	opt.SessionID = "sess-old"
	s := open(t, opt)
	if s.ID() != "sess-old" {
		t.Fatalf("ID = %q, want the loaded session", s.ID())
	}
	if answer, err := s.Turn(context.Background(), "hi"); err != nil || answer != "Hello, world" {
		t.Fatalf("Turn = %q, %v; replayed history must not leak into it", answer, err)
	}
	msgs := transcript()
	l := sent(t, msgs, "session/load")["params"].(map[string]any)
	if l["sessionId"] != "sess-old" || l["cwd"] != opt.Cwd || compact(l["mcpServers"]) != "[]" {
		t.Errorf("session/load params = %s", compact(l))
	}
	for _, m := range msgs {
		if m["method"] == "session/new" {
			t.Error("session/new sent although the load worked")
		}
	}
}

func TestNoLoadCapabilityStartsNewSession(t *testing.T) {
	opt, transcript := fake(t, "noload")
	opt.SessionID = "sess-old"
	s := open(t, opt)
	if s.ID() != "sess-new" {
		t.Fatalf("ID = %q, want a new session", s.ID())
	}
	for _, m := range transcript() {
		if m["method"] == "session/load" {
			t.Error("session/load sent to an agent without loadSession")
		}
	}
}

func TestAuthRequired(t *testing.T) {
	opt, _ := fake(t, "auth")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := Start(ctx, opt)
	if err == nil {
		s.Close()
		t.Fatal("Start succeeded although the agent wants a login")
	}
	if !errors.Is(err, errAuth) || !strings.Contains(err.Error(), "own CLI") {
		t.Fatalf("err = %v, want advice to log in with the agent's CLI", err)
	}
}

// pair returns two agents in one room on a fresh relay.
func pair(t *testing.T) (asker, bot *client.Agent) {
	t.Helper()
	ctx := context.Background()
	const admin = "test-admin-token-0123456789"
	rs, err := relay.New(relay.Config{DBPath: filepath.Join(t.TempDir(), "r.db"), AdminToken: admin})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(rs.Handler())
	t.Cleanup(func() { ts.Close(); rs.Close() })
	m, err := client.New(ts.URL, admin).CreateRoom(ctx, "proj", "asker")
	if err != nil {
		t.Fatal(err)
	}
	key := e2ee.NewKey().String()
	asker, _ = client.NewAgent(client.Identity{Relay: ts.URL, RoomID: m.RoomID, RoomName: "proj", ParticipantID: m.ParticipantID, Name: "asker", Owner: true, Token: m.Token, RoomKey: key})
	inv, err := asker.Relay.CreateInvite(ctx, m.RoomID, time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	j, err := client.New(ts.URL, "").Join(ctx, inv.Token, "bot")
	if err != nil {
		t.Fatal(err)
	}
	bot, _ = client.NewAgent(client.Identity{Relay: ts.URL, RoomID: j.RoomID, RoomName: "proj", ParticipantID: j.ParticipantID, Name: "bot", Token: j.Token, RoomKey: key})
	return asker, bot
}

func TestBridgeEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	asker, bot := pair(t)
	opt, transcript := fake(t)
	s := open(t, opt)
	done := make(chan error, 1)
	go func() { done <- bridge.Run(ctx, bot, s, bridge.Options{}) }()

	ask, err := asker.Send(ctx, "bot", api.Payload{Kind: api.KindAsk, Text: "what is 2+2?"})
	if err != nil {
		t.Fatal(err)
	}
	var reply client.Message
	for deadline := time.Now().Add(15 * time.Second); reply.ID == "" && time.Now().Before(deadline); {
		msgs, err := asker.Inbox(ctx, 2*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range msgs {
			if m.ReplyTo == ask {
				reply = m
			}
		}
	}
	if reply.Kind != api.KindReply || reply.Text != "Hello, world" || reply.Depth != 1 {
		t.Fatalf("reply = %+v", reply)
	}
	prompt := sent(t, transcript(), "session/prompt")["params"].(map[string]any)["prompt"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(prompt, "untrusted input") || !strings.Contains(prompt, "what is 2+2?") {
		t.Errorf("the agent got the peer message without its envelope:\n%s", prompt)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v", err)
	}
}
