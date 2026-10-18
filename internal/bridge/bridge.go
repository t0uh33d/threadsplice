// Package bridge connects a headless agent session to a thread splice room:
// every incoming message becomes a turn in that session, and the session's
// final answer to an ask goes back to the asker. Provider packages (Codex
// app-server, ACP) implement Session.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/t0uh33d/threadsplice/internal/api"
	"github.com/t0uh33d/threadsplice/internal/client"
)

// Session is an agent session the bridge drives, one turn at a time.
type Session interface {
	// Turn delivers prompt (an already framed peer message) as one turn and
	// returns the agent's final answer once the turn ends.
	Turn(ctx context.Context, prompt string) (string, error)
}

// DefaultMaxDepth stops automatic ask/reply chains after five exchanges.
const DefaultMaxDepth = client.MaxAutoDepth

// maxAnswer keeps a reply under the relay's 128 KiB sealed-message limit after
// JSON escaping and sealing overhead.
const maxAnswer = 64 << 10

// ChildEnv is the environment for an agent a bridge drives: ours, minus the
// relay admin token, plus client.BridgeEnv so this binary's hooks and MCP
// server stand down inside the bridge's session.
func ChildEnv() []string {
	env := slices.DeleteFunc(os.Environ(), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return strings.EqualFold(k, "THREADSPLICE_ADMIN_TOKEN") || strings.EqualFold(k, client.BridgeEnv)
	})
	return append(env, client.BridgeEnv+"=1")
}

// Options configures Run.
type Options struct {
	MaxDepth int // asks at or beyond this depth get a refusal instead of a turn
	Log      *slog.Logger
}

// Run serves the room until ctx ends. Messages are handled one at a time, in
// order; each is acknowledged once handled, so a crash re-delivers at most the
// message in flight.
func Run(ctx context.Context, a *client.Agent, s Session, opt Options) error {
	if opt.MaxDepth <= 0 {
		opt.MaxDepth = DefaultMaxDepth
	}
	if opt.Log == nil {
		opt.Log = slog.New(slog.DiscardHandler)
	}
	opt.Log.Info("bridge ready", "room", a.ID.RoomName, "as", a.ID.Name)
	for attempt := 0; ctx.Err() == nil; {
		msgs, last, err := a.Fetch(ctx, -1, 25*time.Second, "idle")
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			attempt++
			opt.Log.Warn("relay poll failed", "err", err, "attempt", attempt)
			sleep(ctx, client.Backoff(attempt))
			continue
		}
		attempt = 0
		for _, m := range msgs {
			if err := handle(ctx, a, s, m, opt); err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				opt.Log.Error("message not handled", "id", m.ID, "from", m.FromName, "err", err)
			}
			if err := a.Ack(ctx, m.Seq); err != nil && ctx.Err() == nil {
				opt.Log.Warn("ack failed; message may be handled again", "id", m.ID, "err", err)
			}
		}
		if last > 0 { // also moves past replays Fetch dropped
			a.Ack(ctx, last)
		}
	}
	return ctx.Err()
}

func handle(ctx context.Context, a *client.Agent, s Session, m client.Message, opt Options) error {
	if m.Err != nil {
		return fmt.Errorf("undecryptable message: %w", m.Err)
	}
	ask := m.Kind == api.KindAsk
	reply := func(text string) error {
		_, err := a.Send(ctx, m.From, api.Payload{Kind: api.KindReply, ReplyTo: m.ID, Text: text, Depth: m.Depth + 1})
		return err
	}
	if ask && m.Depth+1 > opt.MaxDepth {
		opt.Log.Warn("loop budget exhausted; not answering", "id", m.ID, "from", m.FromName, "depth", m.Depth)
		return reply(fmt.Sprintf("[thread splice bridge] Not answered automatically: this exchange reached the automatic-reply limit (%d). A human needs to take a look.", opt.MaxDepth))
	}
	opt.Log.Info("turn", "id", m.ID, "from", m.FromName, "kind", m.Kind)
	a.Relay.SetState(ctx, a.ID.RoomID, "busy")
	answer, err := s.Turn(ctx, client.Envelope(a.ID.RoomName, m, true))
	a.Relay.SetState(ctx, a.ID.RoomID, "idle")
	if err != nil && ctx.Err() == nil {
		opt.Log.Warn("turn failed", "id", m.ID, "err", err)
	}
	if !ask || ctx.Err() != nil {
		return err
	}
	switch {
	case err != nil:
		reason := err.Error()
		if len(reason) > 300 { // agent errors can carry local detail; peers get the gist
			reason = strings.ToValidUTF8(reason[:300], "") + "…"
		}
		answer = "[thread splice bridge] The agent could not complete this turn: " + reason
	case strings.TrimSpace(answer) == "":
		answer = "[thread splice bridge] The agent finished the turn without a text answer."
	case len(answer) > maxAnswer:
		answer = strings.ToValidUTF8(answer[:maxAnswer], "") + "\n[thread splice bridge] Answer truncated to fit the relay's message size limit."
	}
	return reply(answer)
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
