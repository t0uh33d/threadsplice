package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/t0uh33d/threadsplice/internal/client"
)

// Tuning; variables so tests can shrink them.
var (
	codexBin     = "codex"
	claudePace   = 2 * time.Second  // Claude Code's sustained rate per sender (see claudePush)
	retryDelay   = client.Backoff   // before retrying a failed hand-off
	maxFailures  = 8                // consecutive failed hand-offs (about a minute) before falling back to hold
	holdRecheck  = 2 * time.Second  // how often a holding deliverer looks for its session again
	sessionSlack = 10 * time.Second // clock slack when telling this session's record from an earlier one's
)

// headless are the CLAUDE_CODE_ENTRYPOINT values of Claude Code runs nobody
// watches: claude -p, the Agent SDK, GitHub Actions.
// ponytail: a hint, not a guarantee; a run started from inside another session
// inherits that session's value.
var headless = map[string]bool{"sdk-cli": true, "sdk-ts": true, "sdk-py": true, "claude-code-github-action": true}

var errHandOff = errors.New("hand-off failed")

// A deliverer pushes framed messages into the live agent session.
type deliverer struct {
	name   string                                               // "claude" or "codex"
	target func() string                                        // where to deliver right now; "" holds
	push   func(ctx context.Context, target, text string) error // nil: the session has it
	pace   time.Duration                                        // pause after each delivery
}

// setDelivery resolves Options.Deliver for the MCP client that introduced
// itself as clientName, and returns the mode. auto takes Claude Code's inbox
// socket under an interactive Claude Code, codex queue under Codex, and
// otherwise holds messages for the inbox tool; session mode has rules of its
// own (sessionDelivery). A repeated initialize keeps the first answer: the
// delivery loop started with it.
func (s *server) setDelivery(clientName string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mode != "" {
		return s.mode
	}
	mode, name := s.opt.Deliver, strings.ToLower(clientName)
	auto := mode == "" || mode == "auto"
	switch {
	case s.sess != nil:
		s.mode = s.sessionDelivery(auto)
		return s.mode
	case s.agent == nil: // the default identity didn't open: nothing to deliver
		s.mode = "hold"
		return s.mode
	}
	if auto {
		mode = "hold"
		switch {
		// Any agent started inside a Claude session inherits the socket variable;
		// only Claude Code itself ("claude-code") may deliver into that session.
		// Not into a headless run, though: those often run unattended with
		// permission prompts off, and Claude Code delivers its own child's
		// messages even then (it holds other senders' for approval).
		case os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET") != "" && strings.Contains(name, "claude") && !headless[os.Getenv("CLAUDE_CODE_ENTRYPOINT")]:
			mode = "claude"
		case strings.Contains(name, "codex"): // Codex says "codex-mcp-client"
			mode = "codex"
		}
	}
	var d *deliverer
	var err error
	switch mode {
	case "claude":
		d, err = s.claudeDeliverer()
	case "codex":
		d, err = s.codexDeliverer()
	}
	if err != nil {
		if !auto {
			s.opt.Log.Warn("live delivery unavailable; holding messages for the inbox tool", "deliver", mode, "err", err)
		}
		mode = "hold"
	}
	s.mode, s.d = mode, d
	return mode
}

// sessionDelivery is setDelivery for session mode, where bindings already
// guarantee that one live session acts as a member: the bound member's
// messages go to this Claude Code process's own inbox socket, and the hooks'
// session records don't come into it. Channels stay with fixed identities.
func (s *server) sessionDelivery(auto bool) string {
	ok := runtime.GOOS != "windows" && s.sess.socket != "" // see claudeDeliverer for Windows
	switch {
	case ok && (s.opt.Deliver == "claude" || auto && !headless[os.Getenv("CLAUDE_CODE_ENTRYPOINT")]):
		return "claude"
	case s.opt.Deliver == "claude":
		s.opt.Log.Warn("live delivery unavailable; holding messages for the inbox tool", "deliver", "claude", "err", "no inbox socket")
	}
	return "hold"
}

// live returns the delivery mode in effect: "hold" while a deliverer has
// nowhere to deliver (Codex before its session registers, Claude Code while
// another session of this identity gets the messages) or once it gave up.
func (s *server) live() string {
	s.mu.Lock()
	mode, d := s.mode, s.d
	s.mu.Unlock()
	if mode != "hold" && d != nil && d.target() == "" {
		return "hold"
	}
	return mode
}

// claudeDeliverer posts to the session's cross-session inbox socket. This server
// is a child of the Claude Code session, so it inherits the socket path and
// token, and Claude Code delivers its own children's messages instead of
// holding them for approval.
//
// Sessions that share an identity (one registration for all projects) each run
// this server; pushing into all of them would show every message in each. So
// a session holds while the SessionStart hook's record names another session
// that is still listening: the one that registered last gets the messages.
// ponytail: without the hook there's no record, and every session gets them.
func (s *server) claudeDeliverer() (*deliverer, error) {
	if runtime.GOOS == "windows" { // ponytail: the Windows inbox is a named pipe; not dialed yet
		return nil, errors.New("the Windows inbox of Claude Code is a named pipe, which is not supported yet")
	}
	socket, token := os.Getenv("CLAUDE_CODE_MESSAGING_SOCKET"), os.Getenv("CLAUDE_CODE_MESSAGING_TOKEN")
	return &deliverer{
		name: "claude",
		target: func() string {
			si, ok, _ := client.LoadSession(s.agent.ID.Alias)
			ok = ok && si.Agent == "claude" && si.Socket != ""
			switch {
			case socket == "" && ok:
				return si.Socket
			case ok && si.Socket != socket && listening(si.Socket):
				return "" // another session's turn
			}
			return socket
		},
		push: func(ctx context.Context, socket, text string) error { return claudePush(ctx, socket, token, text) },
		pace: claudePace,
	}, nil
}

// codexDeliverer queues into the Codex thread that the session's SessionStart
// hook recorded. Codex runs that hook at the session's first turn, after it
// started this server, so an older record is an earlier session's: queuing
// there would strand the message in a thread nobody is looking at.
//
// The server latches onto the first fresh thread it sees. Codex keeps a thread
// you left (and its MCP servers) loaded for a while, so a server outliving its
// session must not follow the record into the next session's thread: once the
// record names another thread, this server holds and the newer one delivers.
func (s *server) codexDeliverer() (*deliverer, error) {
	bin, err := codexPath()
	if err != nil {
		return nil, err
	}
	var mu sync.Mutex
	var latched string
	return &deliverer{
		name: "codex",
		target: func() string {
			si, ok, _ := client.LoadSession(s.agent.ID.Alias)
			if !ok || si.Agent != "codex" || si.UpdatedAt.Before(s.startedAt.Add(-sessionSlack)) {
				return ""
			}
			mu.Lock()
			defer mu.Unlock()
			if latched == "" {
				latched = si.SessionID // for Codex, the root thread id
			}
			if si.SessionID != latched {
				return ""
			}
			return latched
		},
		push: func(ctx context.Context, thread, text string) error { return codexQueue(ctx, bin, thread, text) },
	}, nil
}

// deliver hands each of m's incoming messages to the session, one at a time
// and in order, acknowledging it only once the session has it (at-least-once).
// After maxFailures failed hand-offs in a row it falls back to hold for good,
// leaving the message in flight for the inbox tool.
func (s *server) deliver(ctx context.Context, m *member, d *deliverer) {
	var fails, polls int
	more, holding := false, false
	for ctx.Err() == nil {
		target := d.target()
		if target == "" {
			if !holding {
				s.opt.Log.Info("holding messages: no session to deliver into", "via", d.name)
				holding = true
			}
			sleep(ctx, holdRecheck)
			continue
		}
		holding = false
		if !more {
			// Wait for something to arrive without holding recvMu, so inbox calls
			// never queue behind a 25 s poll. This only wakes us; handOff fetches.
			envs, err := m.Relay.Messages(ctx, m.ID.RoomID, -1, 25*time.Second, "")
			if err != nil {
				if ctx.Err() != nil || s.gone(m, err) {
					return
				}
				polls++
				s.opt.Log.Warn("relay poll failed", "err", err, "attempt", polls)
				sleep(ctx, client.Backoff(polls))
				continue
			}
			polls = 0
			if len(envs) == 0 {
				continue
			}
			if target = d.target(); target == "" { // the session may have re-registered meanwhile
				continue
			}
		}
		var err error
		more, err = s.handOff(ctx, m, d, target)
		switch {
		case ctx.Err() != nil, s.gone(m, err):
			return
		case errors.Is(err, errHandOff):
			if fails++; fails >= maxFailures {
				s.opt.Log.Warn("live delivery keeps failing; holding messages for the inbox tool", "via", d.name, "err", err)
				s.mu.Lock()
				s.mode = "hold"
				s.mu.Unlock()
				return
			}
			s.opt.Log.Info("retrying", "via", d.name, "attempt", fails, "err", err)
			sleep(ctx, retryDelay(fails))
		case err != nil:
			polls++
			s.opt.Log.Warn("relay request failed", "err", err, "attempt", polls)
			sleep(ctx, client.Backoff(polls))
		default:
			fails, polls = 0, 0
			sleep(ctx, d.pace)
		}
	}
}

// handOff pushes the oldest message waiting for m and acknowledges it once
// pushed; more reports whether others are still waiting. It holds m.recvMu,
// so the inbox tool and the deliverer never both present a message. A message
// whose push fails stays in m.pending, where the next try (or the inbox tool)
// picks it up: the client drops repeat fetches of a message it has already
// seen, so it can't simply be fetched again.
func (s *server) handOff(ctx context.Context, m *member, d *deliverer, target string) (more bool, err error) {
	m.recvMu.Lock()
	defer m.recvMu.Unlock()
	if len(m.pending) == 0 {
		msgs, last, err := m.Fetch(ctx, -1, 0, "")
		if err != nil || len(msgs) == 0 {
			if err == nil && last > 0 { // only replays the client dropped: move past them
				err = m.Ack(ctx, last)
			}
			return false, err
		}
		s.remember(msgs)
		m.pending = msgs
	}
	msg := m.pending[0]
	if msg.Err != nil {
		// No content for the model, and a turn would cost tokens: log it and move on, as bridges do.
		s.opt.Log.Warn("skipping undecryptable message", "id", msg.ID, "from", msg.FromName)
	} else if err := d.push(ctx, target, client.Envelope(m.ID.RoomName, msg, false)); err != nil {
		return true, fmt.Errorf("%w: %w", errHandOff, err)
	} else {
		s.opt.Log.Info("delivered", "id", msg.ID, "kind", msg.Kind, "via", d.name)
	}
	m.pending = m.pending[1:]
	// The session has it now: record that even when delivery is being stopped
	// (leave and rebinding cancel ctx, possibly right after the push), or the
	// next join would deliver it again.
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return len(m.pending) > 0, m.Ack(actx, msg.Seq)
}

// inboxLine is one line on Claude Code's inbox socket.
type inboxLine struct {
	Type     string        `json:"type"`
	Token    string        `json:"token,omitempty"`
	Message  *inboxMessage `json:"message,omitempty"`
	Priority string        `json:"priority,omitempty"`
}

type inboxMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// claudePush posts text to a Claude Code session's inbox socket: the auth line
// if there is a token, then one user message. Priority "next" means Claude reads
// it between tool calls when busy and starts a turn when idle ("now" would
// abort the running turn).
// ponytail: the socket sends no receipt, so a message Claude Code drops still
// counts as delivered: one its inbound settings refuse or hold, one over its
// per-sender budget (30 back to back, then one per 2 s, which claudePace keeps
// under; Anthropic can tighten it remotely), or one past the 50 it queues while
// a long tool call runs.
func claudePush(ctx context.Context, socket, token, text string) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b) // one line per Encode
	enc.SetEscapeHTML(false)
	if token != "" {
		enc.Encode(inboxLine{Type: "auth", Token: token})
	}
	enc.Encode(inboxLine{Type: "user", Message: &inboxMessage{Role: "user", Content: text}, Priority: "next"})
	c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	if err != nil {
		return err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(b.Bytes()); err != nil {
		return err
	}
	return c.Close()
}

// listening reports whether something accepts connections on a unix socket.
// Claude Code removes its socket when it exits; a crash leaves one nobody accepts on.
func listening(socket string) bool {
	c, err := net.DialTimeout("unix", socket, time.Second)
	if err == nil {
		c.Close()
	}
	return err == nil
}

// codexPath finds the codex CLI. A Windows .cmd/.bat shim (what npm installs) is
// refused: cmd.exe re-parses a batch file's arguments, so message text could
// run commands.
func codexPath() (string, error) {
	p, err := exec.LookPath(codexBin)
	if err != nil {
		return "", err
	}
	if ext := strings.ToLower(filepath.Ext(p)); runtime.GOOS == "windows" && (ext == ".cmd" || ext == ".bat") {
		return "", fmt.Errorf("%s is a batch file, which would let cmd.exe re-parse message text", p)
	}
	return p, nil
}

// codexQueue queues text into a Codex thread with the stable `codex queue` CLI
// (exit 0: queued). Codex starts a turn with it if the session is idle, else
// after the running turn (after an interrupted turn it waits until another turn
// ends). Queued text has user-level authority in Codex, so the
// envelope's untrusted-input framing is all that marks it as a peer's: never
// queue peer text without it. The output is discarded, never logged, in case it
// echoes the message.
// ponytail: the envelope travels in argv (codex queue reads it from nowhere
// else), so other local users can see the decrypted text in the process list
// while codex queue runs, and a message over ~128 KB (Linux) or ~32 KB
// (Windows) fails here and ends up with the inbox tool.
func codexQueue(ctx context.Context, bin, thread, text string) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	// argv can't carry NUL: one in a peer's text would fail every retry.
	text = strings.ReplaceAll(text, "\x00", "\uFFFD")
	return exec.CommandContext(ctx, bin, "queue", "--thread", thread, "--message", text).Run()
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
