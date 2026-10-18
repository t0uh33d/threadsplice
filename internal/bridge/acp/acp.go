// Package acp drives an Agent Client Protocol agent as a bridge.Session. It
// spawns the agent — GitHub Copilot CLI (`copilot --acp --stdio`), Gemini CLI
// (`gemini --acp`), OpenCode (`opencode acp`), Claude Code or Codex through
// Zed's adapters — opens one session, and runs one prompt turn per message.
//
// The bridge offers the agent nothing back: no file system, no terminal, no MCP
// servers, and it declines every permission request, because its turns are
// started by remote peers, not by the agent's user.
//
// ponytail: hand-rolled JSON-RPC, like package mcp. We send four requests and
// one notification and answer one kind of request; an SDK would be most of
// the code here.
package acp

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/t0uh33d/threadsplice/internal/bridge"
)

// DefaultTurnTimeout bounds a turn when Options.TurnTimeout is not set.
const DefaultTurnTimeout = 15 * time.Minute

const (
	protocolVersion  = 1      // the ACP major version spoken here
	codeAuthRequired = -32000 // ACP error: the agent wants a login first

	stopGrace  = 5 * time.Second // the agent's time to exit once its stdin closes
	drainGrace = 2 * time.Second // reading on after the agent exits, for its last lines
)

// cancelGrace is how long a cancelled turn may take to wind down before the
// session refuses new turns, and stuckGrace how much longer the agent gets
// before it is killed for never ending it (vars for tests).
var cancelGrace, stuckGrace = 10 * time.Second, time.Minute

// Options configures Start.
type Options struct {
	Command     []string      // the agent and its arguments, e.g. {"copilot", "--acp", "--stdio"}
	Cwd         string        // the session's working directory; default: the current one
	SessionID   string        // load this session if the agent can; otherwise a new one is created
	TurnTimeout time.Duration // cancel turns that run longer; default DefaultTurnTimeout
	Version     string        // reported to the agent in clientInfo
	Stderr      io.Writer     // receives the agent's stderr; nil discards it
	Log         *slog.Logger
}

// Session is a running ACP agent with one open session. Turns run one at a time.
type Session struct {
	cmd     *exec.Cmd
	name    string // the agent command's base name, for messages
	id      string // ACP session id; set before Start returns
	timeout time.Duration
	log     *slog.Logger

	wmu sync.Mutex // one message at a time on the agent's stdin
	in  io.WriteCloser
	enc *json.Encoder

	mu         sync.Mutex
	nextID     int64
	pending    map[string]chan response // our requests awaiting an answer, by JSON-RPC id
	turn       *strings.Builder         // the running turn's answer so far; nil between turns
	cancelling bool                     // session/cancel was sent for the running turn
	unfinished string                   // a cancelled prompt the agent has not answered yet

	readDone chan struct{} // the agent's stdout is exhausted
	done     chan struct{} // the agent has exited; err says how
	err      error
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	result json.RawMessage
	err    *rpcError
}

var errAuth = errors.New("acp: the agent needs you to log in")

// Start spawns the agent and opens the session. With opt.SessionID set it
// loads that session (session/load, ignoring the history the agent replays)
// when the agent supports it; otherwise, or if loading fails, it creates a new
// one — compare ID with opt.SessionID to tell. ctx bounds this setup only: the
// agent then runs until Close, or until it exits by itself.
func Start(ctx context.Context, opt Options) (*Session, error) {
	if len(opt.Command) == 0 || opt.Command[0] == "" {
		return nil, errors.New("acp: no agent command")
	}
	cwd, err := filepath.Abs(opt.Cwd)
	if err != nil {
		return nil, err
	}
	if opt.TurnTimeout <= 0 {
		opt.TurnTimeout = DefaultTurnTimeout
	}
	if opt.Log == nil {
		opt.Log = slog.New(slog.DiscardHandler)
	}
	bin := opt.Command[0]
	if filepath.Base(bin) != bin { // a path, not a name to look up: exec would resolve it in cmd.Dir, the session's directory
		if bin, err = filepath.Abs(bin); err != nil {
			return nil, err
		}
	}
	cmd := exec.Command(bin, opt.Command[1:]...)
	cmd.Dir = cwd
	cmd.Stderr = opt.Stderr
	cmd.Env = bridge.ChildEnv()
	cmd.WaitDelay = drainGrace
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	// Our own pipe, not StdoutPipe: Wait must not close it under the reader, so
	// the agent's exit is seen even if something it started keeps stdout open.
	out, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = w
	err = cmd.Start()
	w.Close()
	if err != nil {
		out.Close()
		return nil, fmt.Errorf("acp: starting the agent: %w", err)
	}
	s := &Session{cmd: cmd, name: filepath.Base(opt.Command[0]), timeout: opt.TurnTimeout, log: opt.Log,
		in: in, enc: json.NewEncoder(in), pending: map[string]chan response{},
		readDone: make(chan struct{}), done: make(chan struct{})}
	go s.read(out)
	go s.wait(out)
	if err := s.open(ctx, cwd, opt.SessionID, opt.Version); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// ID is the ACP session id, for loading the session again later.
func (s *Session) ID() string { return s.id }

// Done is closed once the agent has exited.
func (s *Session) Done() <-chan struct{} { return s.done }

// Err says how the agent exited; it is nil until Done is closed.
func (s *Session) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

// Close stops the agent: it closes the agent's stdin, which asks it to exit,
// and kills it if it is still running a few seconds later.
// ponytail: the kill reaches the agent process only, not processes it started.
func (s *Session) Close() {
	s.in.Close()
	select {
	case <-s.done:
	case <-time.After(stopGrace):
		s.cmd.Process.Kill()
		<-s.done
	}
}

// open negotiates the protocol and opens the session.
func (s *Session) open(ctx context.Context, cwd, load, version string) error {
	var info struct {
		ProtocolVersion   int `json:"protocolVersion"`
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
		AgentInfo struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"agentInfo"`
	}
	err := s.call(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"clientCapabilities": map[string]any{ // nothing: peer-driven turns get no file system or terminal from us
			"fs":       map[string]bool{"readTextFile": false, "writeTextFile": false},
			"terminal": false,
		},
		"clientInfo": map[string]string{"name": "threadsplice", "title": "thread splice", "version": cmp.Or(version, "dev")},
	}, &info)
	if err != nil {
		return err
	}
	if info.ProtocolVersion != protocolVersion {
		return fmt.Errorf("acp: %s speaks ACP version %d; thread splice speaks version %d", s.name, info.ProtocolVersion, protocolVersion)
	}
	params := map[string]any{"cwd": cwd, "mcpServers": []any{}}
	id := ""
	switch {
	case load == "":
	case !info.AgentCapabilities.LoadSession:
		s.log.Warn("the agent cannot load sessions; starting a new one", "session", load)
	default:
		params["sessionId"] = load
		err := s.call(ctx, "session/load", params, nil)
		if err == nil {
			id = load
			break
		}
		if errors.Is(err, errAuth) || ctx.Err() != nil {
			return err
		}
		s.log.Warn("could not load the session; starting a new one", "session", load, "err", err)
		delete(params, "sessionId")
	}
	if id == "" {
		var created struct {
			SessionID string `json:"sessionId"`
		}
		if err := s.call(ctx, "session/new", params, &created); err != nil {
			return err
		}
		if id = created.SessionID; id == "" {
			return errors.New("acp: session/new returned no session id")
		}
	}
	s.mu.Lock()
	s.id = id
	s.mu.Unlock()
	s.log.Info("agent session ready", "agent", info.AgentInfo.Name, "agent_version", info.AgentInfo.Version, "session", id, "loaded", id == load)
	return nil
}

// Turn sends prompt as one user message and returns the agent's answer: the
// text it says after its last tool call. A turn that outlives the turn
// timeout, or whose ctx ends, is cancelled with session/cancel.
func (s *Session) Turn(ctx context.Context, prompt string) (string, error) {
	if err := ctx.Err(); err != nil { // don't start a model turn only to cancel it
		return "", err
	}
	s.mu.Lock()
	switch {
	case s.turn != nil:
		s.mu.Unlock()
		return "", errors.New("acp: a turn is already running")
	case s.unfinished != "": // its late updates would land in this turn's answer
		s.mu.Unlock()
		return "", errors.New("acp: the agent is still finishing a cancelled turn")
	}
	answer := &strings.Builder{}
	s.turn = answer
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.turn, s.cancelling = nil, false
		s.mu.Unlock()
	}()

	tctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	id, ch, err := s.request("session/prompt", map[string]any{
		"sessionId": s.id,
		"prompt":    []map[string]string{{"type": "text", "text": prompt}},
	})
	if err != nil {
		return "", err
	}
	r, err := s.await(tctx, ch)
	if err != nil {
		if s.Err() != nil { // the agent exited
			return "", s.Err()
		}
		return "", s.cancel(ctx, id, ch, err)
	}
	var res struct {
		StopReason string `json:"stopReason"`
	}
	if err := s.result("session/prompt", r, &res); err != nil {
		return "", err
	}
	s.mu.Lock()
	text := answer.String()
	s.mu.Unlock()
	switch res.StopReason {
	case "end_turn":
		return text, nil
	case "max_tokens", "max_turn_requests":
		s.log.Warn("the agent cut the turn short; sending what it said", "session", s.id, "stop_reason", res.StopReason)
		return text, nil
	case "refusal":
		return "", errors.New("acp: the agent refused to answer")
	case "cancelled":
		return "", errors.New("acp: the turn was cancelled")
	}
	return "", fmt.Errorf("acp: the turn ended with stop reason %q", res.StopReason)
}

// cancel stops the prompt turn id (session/cancel) and gives the agent a
// moment to wind it down. An agent that takes longer gets no new turn until it
// answers, and is killed if it still hasn't stuckGrace later. It returns the
// error the turn ends with.
func (s *Session) cancel(ctx context.Context, id string, ch chan response, why error) error {
	s.log.Warn("cancelling the turn", "session", s.id, "reason", why)
	s.mu.Lock()
	s.cancelling = true
	s.mu.Unlock()
	s.notify("session/cancel", map[string]string{"sessionId": s.id})
	wctx, stop := context.WithTimeout(context.Background(), cancelGrace)
	defer stop()
	if _, err := s.await(wctx, ch); err != nil && wctx.Err() != nil {
		s.mu.Lock()
		if _, unanswered := s.pending[id]; unanswered {
			delete(s.pending, id)
			s.unfinished = id
			time.AfterFunc(stuckGrace, func() { s.giveUp(id) })
		}
		s.mu.Unlock()
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("acp: the turn timed out after %s", s.timeout)
}

// giveUp kills an agent that still hasn't ended the cancelled prompt turn id:
// its session would refuse every turn from now on, and a dead agent at least
// ends the bridge, so whoever runs it can see why and restart it.
func (s *Session) giveUp(id string) {
	s.mu.Lock()
	stuck := s.unfinished == id
	s.mu.Unlock()
	if stuck {
		s.log.Error("the agent never ended a cancelled turn; stopping it", "session", s.id)
		s.cmd.Process.Kill()
	}
}

// call sends a request and decodes its result into out.
func (s *Session) call(ctx context.Context, method string, params, out any) error {
	id, ch, err := s.request(method, params)
	if err != nil {
		return err
	}
	r, err := s.await(ctx, ch)
	if err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return fmt.Errorf("acp: %s: %w", method, err)
	}
	return s.result(method, r, out)
}

func (s *Session) request(method string, params any) (string, chan response, error) {
	if err := s.Err(); err != nil {
		return "", nil, err
	}
	s.mu.Lock()
	s.nextID++
	id := strconv.FormatInt(s.nextID, 10)
	ch := make(chan response, 1)
	s.pending[id] = ch
	s.mu.Unlock()
	if err := s.write(message{ID: json.RawMessage(id), Method: method, Params: mustJSON(params)}); err != nil {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		return "", nil, err
	}
	return id, ch, nil
}

// await waits for a response, failing as soon as the agent exits or ctx ends.
func (s *Session) await(ctx context.Context, ch chan response) (response, error) {
	select {
	case r := <-ch:
		return r, nil
	case <-s.done:
		select {
		case r := <-ch: // it answered just before exiting
			return r, nil
		default:
			return response{}, s.Err()
		}
	case <-ctx.Done():
		return response{}, ctx.Err()
	}
}

func (s *Session) result(method string, r response, out any) error {
	if e := r.err; e != nil {
		if e.Code == codeAuthRequired {
			return fmt.Errorf("%w: %s answered %q; log in with %s's own CLI first (thread splice never logs in for you), then restart the bridge", errAuth, s.name, e.Message, s.name)
		}
		return fmt.Errorf("acp: %s: %s (code %d)", method, e.Message, e.Code)
	}
	if out == nil || len(r.result) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.result, out); err != nil {
		return fmt.Errorf("acp: %s: bad result: %w", method, err)
	}
	return nil
}

func (s *Session) notify(method string, params any) {
	if err := s.write(message{Method: method, Params: mustJSON(params)}); err != nil {
		s.log.Warn("notification not sent", "method", method, "err", err)
	}
}

func (s *Session) reply(id json.RawMessage, result any, e *rpcError) {
	m := message{ID: id, Error: e}
	if e == nil {
		m.Result = mustJSON(result)
	}
	if err := s.write(m); err != nil {
		s.log.Warn("reply not sent", "err", err)
	}
}

func (s *Session) write(m message) error {
	m.JSONRPC = "2.0"
	s.wmu.Lock()
	defer s.wmu.Unlock()
	if err := s.enc.Encode(m); err != nil { // Encode writes exactly one line
		return fmt.Errorf("acp: writing to %s: %w", s.name, err)
	}
	return nil
}

// read feeds the agent's stdout to dispatch, one message per line.
// ponytail: no line length cap; the agent is a local program the user chose to run.
func (s *Session) read(stdout io.Reader) {
	r := bufio.NewReader(stdout)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			s.dispatch(line)
		}
		if err != nil {
			break
		}
	}
	close(s.readDone)
	select { // without stdout the session is over
	case <-s.done:
	case <-time.After(stopGrace):
		s.cmd.Process.Kill()
	}
}

// wait reaps the agent and ends the session.
func (s *Session) wait(stdout *os.File) {
	err := s.cmd.Wait()
	select {
	case <-s.readDone:
	case <-time.After(drainGrace): // something the agent started still holds its stdout
	}
	stdout.Close()
	if err == nil {
		err = errors.New("exit status 0")
	}
	s.err = fmt.Errorf("%s exited (%v)", s.name, err)
	close(s.done)
}

// dispatch handles one message from the agent. It runs on the reader
// goroutine in arrival order, so a turn has all of its updates by the time
// its response arrives.
func (s *Session) dispatch(line []byte) {
	var m message
	if err := json.Unmarshal(line, &m); err != nil {
		s.log.Warn("ignoring an unreadable line from the agent", "bytes", len(line))
		return
	}
	switch {
	case m.Method == "": // a response to one of our requests
		id := string(m.ID)
		s.mu.Lock()
		if ch, ok := s.pending[id]; ok {
			delete(s.pending, id)
			ch <- response{m.Result, m.Error}
		} else if id == s.unfinished {
			s.unfinished = ""
		}
		s.mu.Unlock()
	case m.ID == nil:
		if m.Method == "session/update" {
			s.update(m.Params)
		}
	default:
		s.serve(m)
	}
}

// update collects the running turn's answer. Updates between turns, such as
// the history replayed by session/load, are ignored.
func (s *Session) update(raw json.RawMessage) {
	var p struct {
		SessionID string `json:"sessionId"`
		Update    struct {
			Kind    string          `json:"sessionUpdate"`
			Content json.RawMessage `json:"content"`
		} `json:"update"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.turn == nil || p.SessionID != s.id {
		return
	}
	switch p.Update.Kind {
	case "agent_message_chunk":
		var c struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(p.Update.Content, &c) == nil && c.Type == "text" {
			s.turn.WriteString(c.Text)
		}
	case "tool_call":
		// ponytail: the answer is all text after the last tool call, not the
		// commentary before it; a subagent's text streamed as the agent's own counts too.
		s.turn.Reset()
	}
}

type option struct {
	OptionID string `json:"optionId"`
	Kind     string `json:"kind"`
}

// serve answers a request from the agent. The only one it can act on is a
// permission request, and the answer is always no: a remote peer started
// this turn, and peers never approve anything. The no is the cancelled outcome
// when no option refuses, or outside a live turn (ACP requires it once
// session/cancel is sent).
func (s *Session) serve(m message) {
	if m.Method != "session/request_permission" { // fs/*, terminal/* and the rest: none offered
		s.log.Info("refused an agent request", "method", m.Method)
		s.reply(m.ID, nil, &rpcError{Code: -32601, Message: "method not found: " + m.Method})
		return
	}
	var p struct {
		ToolCall struct {
			ID   string `json:"toolCallId"`
			Kind string `json:"kind"`
		} `json:"toolCall"`
		Options []option `json:"options"`
	}
	json.Unmarshal(m.Params, &p)
	s.mu.Lock()
	live := s.turn != nil && !s.cancelling
	s.mu.Unlock()
	outcome := map[string]string{"outcome": "cancelled"}
	if id, ok := decline(p.Options); ok && live {
		outcome = map[string]string{"outcome": "selected", "optionId": id}
	}
	s.log.Info("declined a tool permission request", "tool_call", p.ToolCall.ID, "tool_kind", p.ToolCall.Kind, "outcome", outcome["outcome"])
	s.reply(m.ID, map[string]any{"outcome": outcome}, nil)
}

// decline picks the option that refuses just this once, else the one that
// refuses for good.
func decline(opts []option) (string, bool) {
	for _, kind := range []string{"reject_once", "reject_always"} {
		for _, o := range opts {
			if o.Kind == kind {
				return o.OptionID, true
			}
		}
	}
	return "", false
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
