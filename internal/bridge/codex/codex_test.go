package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
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

// The test binary doubles as a fake `codex app-server` for TestProcess.
func TestMain(m *testing.M) {
	if os.Getenv("THREADSPLICE_FAKE_CODEX") == "1" {
		os.Exit(fakeCodex())
	}
	os.Exit(m.Run())
}

// fakeCodex speaks just enough app-server to start a thread, then exits with
// status 3 on the first turn/start, like a crash mid-turn.
func fakeCodex() int {
	if a := strings.Join(os.Args[1:], " "); a != "-c features.hooks=false app-server --listen stdio://" || os.Getenv("THREADSPLICE_BRIDGE") != "1" || os.Getenv("CODEX_HOME") != os.Getenv("THREADSPLICE_FAKE_CODEX_HOME") {
		fmt.Fprintln(os.Stderr, "fake codex: unexpected arguments:", a)
		return 2
	}
	enc := json.NewEncoder(os.Stdout)
	sc := bufio.NewScanner(os.Stdin)
	for sc.Scan() {
		var m struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		json.Unmarshal(sc.Bytes(), &m)
		switch m.Method {
		case "initialize":
			enc.Encode(map[string]any{"id": m.ID, "result": map[string]any{"userAgent": "threadsplice/0.163.0"}})
		case "thread/start":
			enc.Encode(map[string]any{"id": m.ID, "result": map[string]any{"thread": map[string]any{"id": "thr_proc"}}})
		case "turn/start":
			return 3
		}
	}
	return 0 // stdin closed: shut down
}

// fake plays app-server over in-process pipes; the test scripts it step by step.
type fake struct {
	t   *testing.T
	out io.WriteCloser      // read by the session as the server's stdout
	in  chan map[string]any // lines the session wrote, decoded
}

type started struct {
	s   *Session
	err error
}

// startFake starts a session against a fake. The test serves the handshake,
// then collects the result from the channel (see session).
func startFake(t *testing.T, opt Options) (*fake, <-chan started) {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	f := &fake{t: t, out: outW, in: make(chan map[string]any, 100)}
	go func() {
		defer close(f.in)
		defer outW.Close() // stdin closed: the server exits
		br := bufio.NewReader(inR)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				var m map[string]any
				if err := json.Unmarshal(line, &m); err != nil {
					t.Errorf("session wrote invalid JSON %q: %v", line, err)
				} else if _, ok := m["jsonrpc"]; ok {
					t.Errorf("session sent a jsonrpc field: %s", line)
				}
				f.in <- m
			}
			if err != nil {
				return
			}
		}
	}()
	ch := make(chan started, 1)
	go func() {
		s, err := start(context.Background(), nil, inW, outR, opt)
		ch <- started{s, err}
	}()
	return f, ch
}

// session waits for a successful start and closes the session with the test.
func (f *fake) session(ch <-chan started) *Session {
	f.t.Helper()
	r := await(f.t, ch)
	if r.err != nil {
		f.t.Fatal(r.err)
	}
	f.t.Cleanup(r.s.Close)
	return r.s
}

func (f *fake) next() map[string]any {
	f.t.Helper()
	select {
	case m, ok := <-f.in:
		if !ok {
			f.t.Fatal("session closed its stdin")
		}
		return m
	case <-time.After(10 * time.Second):
		f.t.Fatal("timed out waiting for the session")
		return nil
	}
}

// request returns the session's next message, which must be a method request.
func (f *fake) request(method string) map[string]any {
	f.t.Helper()
	m := f.next()
	if m["method"] != method || m["id"] == nil {
		f.t.Fatalf("got %v, want a %s request", m, method)
	}
	return m
}

func (f *fake) send(m map[string]any) {
	f.t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		f.t.Fatal(err)
	}
	if _, err := f.out.Write(append(b, '\n')); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fake) reply(req map[string]any, result any) {
	f.send(map[string]any{"id": req["id"], "result": result})
}

func (f *fake) fail(req map[string]any, code int, msg string) {
	f.send(map[string]any{"id": req["id"], "error": map[string]any{"code": code, "message": msg}})
}

func (f *fake) notify(method string, params map[string]any) {
	f.send(map[string]any{"method": method, "params": params, "emittedAtMs": 1})
}

// serveInit answers initialize and checks the initialized notification.
func (f *fake) serveInit() map[string]any {
	f.t.Helper()
	init := f.request("initialize")
	f.reply(init, map[string]any{"userAgent": "threadsplice/0.163.0", "codexHome": "/tmp/codex", "platformFamily": "unix", "platformOs": "linux"})
	if n := f.next(); n["method"] != "initialized" || n["id"] != nil || n["params"] != nil {
		f.t.Fatalf("want the initialized notification, got %v", n)
	}
	return init
}

// handshake serves a whole startup that creates thread thr_1.
func (f *fake) handshake() {
	f.t.Helper()
	f.serveInit()
	f.reply(f.request("thread/start"), map[string]any{"thread": map[string]any{"id": "thr_1"}})
}

func turnResp(id string) map[string]any {
	return map[string]any{"turn": map[string]any{"id": id, "status": "inProgress", "items": []any{}, "itemsView": "notLoaded"}}
}

func agentMsg(text string, phase any) map[string]any {
	return map[string]any{"type": "agentMessage", "id": "msg_" + text, "text": text, "phase": phase}
}

type result struct {
	text string
	err  error
}

func turn(s *Session, prompt string) <-chan result {
	ch := make(chan result, 1)
	go func() {
		text, err := s.Turn(context.Background(), prompt)
		ch <- result{text, err}
	}()
	return ch
}

func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
		var zero T
		return zero
	}
}

// get digs into decoded JSON: get(m, "params", "clientInfo", "name").
func get(v any, path ...string) any {
	for _, k := range path {
		m, _ := v.(map[string]any)
		v = m[k]
	}
	return v
}

func TestSession(t *testing.T) {
	f, ch := startFake(t, Options{Cwd: "/work", Sandbox: "workspace-write", Version: "test"})

	init := f.serveInit()
	if get(init, "params", "clientInfo", "name") != "threadsplice" || get(init, "params", "clientInfo", "version") != "test" {
		t.Errorf("clientInfo = %v", get(init, "params", "clientInfo"))
	}
	if get(init, "params", "capabilities", "experimentalApi") != false {
		t.Errorf("experimentalApi must be false: %v", get(init, "params", "capabilities"))
	}
	if optOut, _ := get(init, "params", "capabilities", "optOutNotificationMethods").([]any); !slices.Contains(optOut, any("item/agentMessage/delta")) {
		t.Errorf("optOutNotificationMethods = %v", optOut)
	}
	ts := f.request("thread/start")
	if get(ts, "params", "approvalPolicy") != "never" || get(ts, "params", "sandbox") != "workspace-write" || get(ts, "params", "cwd") != "/work" {
		t.Errorf("thread/start params = %v", ts["params"])
	}
	if di, _ := get(ts, "params", "developerInstructions").(string); !strings.Contains(di, "untrusted") || !strings.Contains(di, `"threadsplice" tool`) {
		t.Errorf("developerInstructions lack the peer policy: %q", di)
	}
	f.reply(ts, map[string]any{"thread": map[string]any{"id": "thr_1", "status": map[string]any{"type": "idle"}}})
	s := f.session(ch)
	if s.ThreadID() != "thr_1" {
		t.Fatalf("ThreadID = %q", s.ThreadID())
	}

	// Turn 1: peer text goes in as toolOutput, never as user input.
	done := turn(s, "what is 2+2")
	req := f.request("turn/start")
	if in, ok := get(req, "params", "input").([]any); !ok || len(in) != 0 {
		t.Errorf("input = %v, want []", get(req, "params", "input"))
	}
	if get(req, "params", "threadId") != "thr_1" || get(req, "params", "toolOutput", "name") != "threadsplice" || get(req, "params", "toolOutput", "output") != "what is 2+2" {
		t.Errorf("turn/start params = %v", req["params"])
	}
	f.reply(req, turnResp("turn_1"))

	// Server requests mid-turn are answered at once and never approved.
	for i, c := range []struct {
		method string
		want   map[string]any // result; nil: a JSON-RPC error
	}{
		{"item/commandExecution/requestApproval", map[string]any{"decision": "decline"}},
		{"item/fileChange/requestApproval", map[string]any{"decision": "decline"}},
		{"item/permissions/requestApproval", map[string]any{"permissions": map[string]any{}, "scope": "turn"}},
		{"mcpServer/elicitation/request", map[string]any{"action": "decline", "content": nil, "_meta": nil}},
		{"execCommandApproval", map[string]any{"decision": map[string]any{"denied": map[string]any{"rejection": declined}}}},
		{"applyPatchApproval", map[string]any{"decision": map[string]any{"denied": map[string]any{"rejection": declined}}}},
		{"item/tool/requestUserInput", nil},
		{"item/tool/call", nil},
		{"account/chatgptAuthTokens/refresh", nil},
		{"attestation/generate", nil},
		{"currentTime/read", nil},
		{"some/future/request", nil},
	} {
		id := float64(100 + i)
		f.send(map[string]any{"id": id, "method": c.method, "params": map[string]any{"threadId": "thr_1", "turnId": "turn_1", "itemId": "x"}})
		r := f.next()
		switch {
		case r["id"] != id || r["method"] != nil:
			t.Errorf("%s: want a response to id %v, got %v", c.method, id, r)
		case c.want == nil && get(r, "error", "code") != float64(codeUnsupported):
			t.Errorf("%s: want error -32000, got %v", c.method, r)
		case c.want != nil && !reflect.DeepEqual(r["result"], c.want):
			t.Errorf("%s: result = %v, want %v", c.method, r["result"], c.want)
		}
	}

	f.notify("turn/completed", map[string]any{"threadId": "thr_1", "turn": map[string]any{ // late, from an older turn
		"id": "turn_0", "status": "completed", "items": []any{agentMsg("stale", "final_answer")}}})
	f.notify("item/completed", map[string]any{"threadId": "thr_1", "turnId": "turn_1", "item": agentMsg("let me think", "commentary")})
	f.notify("turn/completed", map[string]any{"threadId": "thr_1", "turn": map[string]any{
		"id": "turn_1", "status": "completed", "itemsView": "summary", "items": []any{agentMsg("4", "final_answer")}}})
	if r := await(t, done); r.err != nil || r.text != "4" {
		t.Fatalf("turn 1 = %q, %v; want the answer from turn/completed", r.text, r.err)
	}

	// Turn 2 fails; its notification overtakes the turn/start response.
	done = turn(s, "explode")
	req = f.request("turn/start")
	f.notify("turn/completed", map[string]any{"threadId": "thr_1", "turn": map[string]any{
		"id": "turn_2", "status": "failed", "items": []any{}, "error": map[string]any{"message": "model overloaded"}}})
	f.reply(req, turnResp("turn_2"))
	if r := await(t, done); r.err == nil || !strings.Contains(r.err.Error(), "model overloaded") {
		t.Fatalf("failed turn = %q, %v; want its error", r.text, r.err)
	}
}

func TestRetryAndFailClosed(t *testing.T) {
	f, ch := startFake(t, Options{Sandbox: "read-only"})
	f.handshake()
	s := f.session(ch)

	done := turn(s, "hi")
	f.fail(f.request("turn/start"), codeOverloaded, "Server overloaded; retry later.")
	req := f.request("turn/start") // retried as is
	if get(req, "params", "toolOutput", "output") != "hi" {
		t.Errorf("retry params = %v", req["params"])
	}
	f.reply(req, turnResp("turn_1"))
	f.notify("turn/completed", map[string]any{"threadId": "thr_1", "turn": map[string]any{
		"id": "turn_1", "status": "completed", "items": []any{agentMsg("hello", nil)}}})
	if r := await(t, done); r.err != nil || r.text != "hello" {
		t.Fatalf("turn = %q, %v", r.text, r.err)
	}

	// A Codex that refuses toolOutput gets an error, never peer text as user input.
	done = turn(s, "again")
	f.fail(f.request("turn/start"), -32600, "Invalid request: unknown field `toolOutput`")
	if r := await(t, done); r.err == nil || !strings.Contains(r.err.Error(), "upgrade") {
		t.Fatalf("turn = %q, %v; want an upgrade error", r.text, r.err)
	}
	done = turn(s, "third")
	if req := f.request("turn/start"); get(req, "params", "toolOutput", "output") != "third" || len(get(req, "params", "input").([]any)) != 0 {
		t.Errorf("later turn params = %v (must stay toolOutput with empty input)", req["params"])
	}
}

func TestTurnTimeout(t *testing.T) {
	f, ch := startFake(t, Options{Sandbox: "read-only", TurnTimeout: 500 * time.Millisecond})
	f.handshake()
	s := f.session(ch)

	done := turn(s, "take your time")
	f.reply(f.request("turn/start"), turnResp("turn_1"))
	intr := f.request("turn/interrupt")
	if get(intr, "params", "threadId") != "thr_1" || get(intr, "params", "turnId") != "turn_1" {
		t.Errorf("turn/interrupt params = %v", intr["params"])
	}
	f.reply(intr, map[string]any{})
	if r := await(t, done); r.err == nil || !strings.Contains(r.err.Error(), "timed out") {
		t.Fatalf("turn = %q, %v; want a timeout", r.text, r.err)
	}
}

func TestResume(t *testing.T) {
	// A thread another process holds can't be resumed: the error says why, and
	// no new thread is started in its place.
	f, ch := startFake(t, Options{ThreadID: "thr_9", Sandbox: "read-only"})
	f.serveInit()
	req := f.request("thread/resume")
	if get(req, "params", "threadId") != "thr_9" || get(req, "params", "excludeTurns") != true ||
		get(req, "params", "approvalPolicy") != "never" || get(req, "params", "sandbox") != "read-only" {
		t.Errorf("thread/resume params = %v", req["params"])
	}
	f.fail(req, -32600, "thread thr_9 already has an active writer")
	r := await(t, ch)
	if !errors.Is(r.err, ErrResume) || !strings.Contains(r.err.Error(), "active writer") || !strings.Contains(r.err.Error(), "thr_9") {
		t.Fatalf("start = %v, want a resume error", r.err)
	}
	select {
	case m, ok := <-f.in:
		if ok {
			t.Fatalf("session sent %v after the failed resume", m["method"])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("session did not shut down after the failed resume")
	}

	f, ch = startFake(t, Options{ThreadID: "thr_9", Sandbox: "read-only"})
	f.serveInit()
	f.reply(f.request("thread/resume"), map[string]any{"thread": map[string]any{"id": "thr_9"}})
	if s := f.session(ch); s.ThreadID() != "thr_9" {
		t.Fatalf("ThreadID = %q", s.ThreadID())
	}
}

func TestProcess(t *testing.T) {
	bin, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("THREADSPLICE_FAKE_CODEX", "1")
	home := t.TempDir()
	t.Setenv("THREADSPLICE_FAKE_CODEX_HOME", home) // the fake checks it got exactly this CODEX_HOME
	t.Setenv("CODEX_HOME", "/somewhere/else")      // ...overriding the user's
	ctx := context.Background()

	s, err := Start(ctx, Options{Bin: bin, Cwd: t.TempDir(), Home: home})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.ThreadID() != "thr_proc" {
		t.Fatalf("ThreadID = %q", s.ThreadID())
	}
	if _, err := s.Turn(ctx, "hello"); err == nil || !strings.Contains(err.Error(), "exit status 3") {
		t.Fatalf("Turn = %v; want the process exit", err)
	}
	select {
	case <-s.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("Done not closed after the process exited")
	}
	if s.Err() == nil {
		t.Error("Err is nil after the process exited")
	}

	// Close ends a healthy app-server by closing its stdin.
	s2, err := Start(ctx, Options{Bin: bin, Home: home})
	if err != nil {
		t.Fatal(err)
	}
	if s2.Err() != nil {
		t.Fatalf("Err = %v while running", s2.Err())
	}
	s2.Close()
	select {
	case <-s2.Done():
	default:
		t.Fatal("Done not closed after Close")
	}

	// A relative -codex path means relative to us, not to the thread's directory.
	t.Chdir(filepath.Dir(bin))
	s3, err := Start(ctx, Options{Bin: "." + string(filepath.Separator) + filepath.Base(bin), Cwd: t.TempDir(), Home: home})
	if err != nil {
		t.Fatalf("Start with a relative binary path: %v", err)
	}
	s3.Close()

	if _, err := Start(ctx, Options{Bin: bin, Sandbox: "danger-full-access"}); err == nil {
		t.Fatal("Start accepted danger-full-access")
	}
}

func TestBridgeEndToEnd(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	const admin = "test-admin-token-0123456789"
	rs, err := relay.New(relay.Config{DBPath: filepath.Join(t.TempDir(), "r.db"), AdminToken: admin})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(rs.Handler())
	defer func() { ts.Close(); rs.Close() }()
	m, err := client.New(ts.URL, admin).CreateRoom(ctx, "proj", "asker")
	if err != nil {
		t.Fatal(err)
	}
	key := e2ee.NewKey().String()
	asker, _ := client.NewAgent(client.Identity{Relay: ts.URL, RoomID: m.RoomID, RoomName: "proj", ParticipantID: m.ParticipantID, Name: "asker", Owner: true, Token: m.Token, RoomKey: key})
	inv, err := asker.Relay.CreateInvite(ctx, m.RoomID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	j, err := client.New(ts.URL, "").Join(ctx, inv.Token, "codex")
	if err != nil {
		t.Fatal(err)
	}
	bot, _ := client.NewAgent(client.Identity{Relay: ts.URL, RoomID: j.RoomID, RoomName: "proj", ParticipantID: j.ParticipantID, Name: "codex", Token: j.Token, RoomKey: key})

	f, ch := startFake(t, Options{Sandbox: "read-only"})
	f.handshake()
	s := f.session(ch)
	ran := make(chan error, 1)
	go func() { ran <- bridge.Run(ctx, bot, s, bridge.Options{}) }()

	ask, err := asker.Send(ctx, "codex", api.Payload{Kind: api.KindAsk, Text: "what is 2+2"})
	if err != nil {
		t.Fatal(err)
	}
	req := f.request("turn/start")
	prompt, _ := get(req, "params", "toolOutput", "output").(string)
	if !strings.Contains(prompt, "what is 2+2") || !strings.Contains(prompt, "untrusted input") || !strings.Contains(prompt, `sent back to "asker" automatically`) {
		t.Errorf("turn prompt is not the framed peer message:\n%s", prompt)
	}
	f.reply(req, turnResp("turn_1"))
	f.notify("item/completed", map[string]any{"threadId": "thr_1", "turnId": "turn_1", "item": agentMsg("4", "final_answer")})
	f.notify("turn/completed", map[string]any{"threadId": "thr_1", "turn": map[string]any{
		"id": "turn_1", "status": "completed", "items": []any{}, "itemsView": "notLoaded"}})

	var reply client.Message
	for deadline := time.Now().Add(10 * time.Second); reply.ID == "" && time.Now().Before(deadline); {
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
	if reply.Kind != api.KindReply || reply.Text != "4" || reply.Depth != 1 {
		t.Fatalf("reply = %+v", reply)
	}
	cancel()
	if err := await(t, ran); !errors.Is(err, context.Canceled) {
		t.Errorf("Run returned %v", err)
	}
}

// hookStdin is a session stdin that runs a hook on every write.
type hookStdin func([]byte)

func (h hookStdin) Write(b []byte) (int, error) { h(b); return len(b), nil }
func (hookStdin) Close() error                  { return nil }

func TestAnswerThenExit(t *testing.T) {
	// Codex can answer and exit before the caller gets to look: the answer must
	// win over the exit, every time.
	for range 100 {
		s := &Session{opt: Options{Log: slog.New(slog.DiscardHandler)}, pending: map[int64]chan message{},
			wake: make(chan struct{}, 1), done: make(chan struct{})}
		s.stdin = hookStdin(func([]byte) {
			s.dispatch([]byte(`{"id":1,"result":{"ok":true}}`))
			s.err = errors.New("codex app-server exited")
			close(s.done)
		})
		if res, err := s.call(context.Background(), "turn/start", nil); err != nil || string(res) != `{"ok":true}` {
			t.Fatalf("call = %s, %v; want the answer sent before the exit", res, err)
		}
	}

	// The same through a whole turn whose last event lands just before the exit.
	f, ch := startFake(t, Options{Sandbox: "read-only"})
	f.handshake()
	s := f.session(ch)
	done := turn(s, "q")
	f.reply(f.request("turn/start"), turnResp("turn_1"))
	f.notify("turn/completed", map[string]any{"threadId": "thr_1", "turn": map[string]any{
		"id": "turn_1", "status": "completed", "items": []any{agentMsg("answer", "final_answer")}}})
	f.out.Close()
	if r := await(t, done); r.err != nil || r.text != "answer" {
		t.Fatalf("turn = %q, %v; want the answer sent before the exit", r.text, r.err)
	}
}

func TestCancel(t *testing.T) {
	// A caller giving up mid-turn interrupts the turn and gets its own error back.
	f, ch := startFake(t, Options{Sandbox: "read-only"})
	f.handshake()
	s := f.session(ch)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan result, 1)
	go func() {
		text, err := s.Turn(ctx, "q")
		done <- result{text, err}
	}()
	f.reply(f.request("turn/start"), turnResp("turn_1"))
	f.notify("item/completed", map[string]any{"threadId": "thr_1", "turnId": "turn_1", "item": agentMsg("working on it", "commentary")})
	cancel()
	intr := f.request("turn/interrupt")
	if get(intr, "params", "turnId") != "turn_1" {
		t.Errorf("turn/interrupt params = %v", intr["params"])
	}
	f.reply(intr, map[string]any{})
	if r := await(t, done); !errors.Is(r.err, context.Canceled) {
		t.Fatalf("turn = %q, %v; want context.Canceled", r.text, r.err)
	}
}

func TestUnexpectedInput(t *testing.T) {
	// Junk on the pipe and answers nobody asked for are skipped, not fatal.
	f, ch := startFake(t, Options{Sandbox: "read-only"})
	init := f.request("initialize")
	if _, err := f.out.Write([]byte("not json\n[1,2]\n\n{\"id\":\"zz\",\"result\":{}}\n{\"id\":999,\"result\":{}}\n")); err != nil {
		t.Fatal(err)
	}
	f.reply(init, map[string]any{"userAgent": "threadsplice/0.163.0"})
	f.next() // initialized
	f.reply(f.request("thread/start"), map[string]any{"thread": map[string]any{"id": "thr_1"}})
	f.session(ch)

	// A server request id is echoed as sent, string or number.
	f.send(map[string]any{"id": "req-1", "method": "item/fileChange/requestApproval", "params": map[string]any{}})
	if r := f.next(); r["id"] != "req-1" || get(r, "result", "decision") != "decline" {
		t.Fatalf("answer to a string-id request = %v", r)
	}
}

func TestOldCodex(t *testing.T) {
	// A Codex that predates toolOutput would ignore it and run an empty turn;
	// the only fallback would give peer text user authority, so Start refuses.
	f, ch := startFake(t, Options{Sandbox: "read-only"})
	f.reply(f.request("initialize"), map[string]any{"userAgent": "threadsplice/0.150.2 (Ubuntu 24.4.0; x86_64) xterm-256color (threadsplice; dev)"})
	if r := await(t, ch); r.err == nil || !strings.Contains(r.err.Error(), "upgrade to Codex 0.151") {
		t.Fatalf("Start with Codex 0.150 = %v, want an upgrade error", r.err)
	}

	for ua, want := range map[string][]int{
		"threadsplice/0.163.0 (Mac OS 15.1.0; arm64) xterm": {0, 163, 0},
		"codex_cli_rs/0.164.0-alpha.3 (Windows 11; x86_64)": {0, 164, 0},
		"threadsplice/0.0.0 (Linux 6.8; aarch64)":           nil,
		"threadsplice/1.2 (Linux)":                          nil,
		"":                                                  nil,
	} {
		if got := codexVersion(ua); !slices.Equal(got, want) {
			t.Errorf("codexVersion(%q) = %v, want %v", ua, got, want)
		}
	}
}
