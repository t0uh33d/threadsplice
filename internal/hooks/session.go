package hooks

import (
	"context"
	"errors"
	"io"
	"os"
	"strconv"

	"github.com/t0uh33d/threadsplice/internal/client"
)

// sessionHook handles a Claude Code hook in session mode, where each session
// joins a room as a member of its own only when its user asks (see
// internal/mcp's join tool, and client.Binding). It records the conversation
// the Claude Code process shows, moves a binding along when /clear replaces
// the conversation, and then acts as the session's member, exactly as the
// hooks of a fixed identity do. A session that hasn't joined gets no output and
// costs no relay call: hook output is high-authority and costs tokens.
func sessionHook(ctx context.Context, ev string, p input, out io.Writer) error {
	sid := p.SessionID
	if !client.ValidSessionID(sid) {
		return errors.New("the hook input has no valid session id")
	}
	pid := claudePID()
	switch ev {
	case "SessionStart":
		// /clear replaced the conversation prev in this process. The new one
		// gets prev's binding before it becomes the one the process shows, and
		// prev loses it only after: at every moment the conversation shown is
		// bound, so the session stays joined and its member never looks free.
		prev, _ := client.Current(pid)
		var moved bool
		var moveErr error
		if p.Source == "clear" && prev != "" && prev != sid {
			moved, moveErr = follow(prev, sid, pid)
		}
		if err := client.SetCurrent(pid, sid); err != nil {
			return err
		}
		if moveErr != nil {
			return moveErr
		}
		if moved {
			if err := client.DeleteBinding(prev); err != nil {
				return err
			}
		}
	case "SessionEnd":
		// /clear ends the conversation, not the process: the SessionStart
		// that follows looks up the conversation it replaces.
		if p.Reason != "clear" {
			defer client.ClearCurrent(pid)
		}
	}
	b, ok, err := client.LoadBinding(sid)
	if err != nil || !ok || client.InUse(b.Machine, b.Child, sid) {
		return err // not joined, or another live session holds the member
	}
	id, err := client.LoadMember(b.Machine, b.Child)
	if err != nil {
		return err
	}
	a, err := client.NewAgent(id)
	if err != nil {
		return err
	}
	o, err := handle(ctx, a, "claude", ev, p)
	if err != nil || o == nil {
		return err
	}
	return write(out, o)
}

// follow gives sid, the conversation that replaced prev in the same Claude
// Code process (/clear), prev's binding, unless sid has one of its own. It
// reports whether it did; prev's binding is the caller's to delete.
func follow(prev, sid string, pid int) (bool, error) {
	b, ok, err := client.LoadBinding(prev)
	if err != nil || !ok {
		return false, err
	}
	if _, taken, err := client.LoadBinding(sid); err != nil || taken {
		return false, err
	}
	b.SessionID, b.PID = sid, pid
	if err := client.SaveBinding(b); err != nil {
		return false, err
	}
	return true, nil
}

// claudePID is the Claude Code process the hook runs for: $CLAUDE_PID, else
// the hook's parent. The MCP server, a direct child, uses its parent too.
func claudePID() int {
	if pid, err := strconv.Atoi(os.Getenv("CLAUDE_PID")); err == nil && pid > 0 {
		return pid
	}
	return os.Getppid()
}
