package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/t0uh33d/threadsplice/internal/bridge"
	"github.com/t0uh33d/threadsplice/internal/bridge/acp"
	"github.com/t0uh33d/threadsplice/internal/client"
)

func init() {
	bridges["acp"] = cmdBridgeACP
}

const acpUsage = `usage: threadsplice bridge acp [-as NAME] [-cwd DIR] [-session ID | -new] [-max-depth N] [-turn-timeout 15m] -- AGENT [ARGS...]
  e.g.  -- copilot --acp --stdio --no-remote-export   (Copilot syncs sessions to GitHub by default; see copilot --help)
        -- gemini --acp
        -- opencode acp`

// cmdBridgeACP serves the room from a headless Agent Client Protocol session:
// every message becomes a turn, and the answer to an ask goes back to the
// asker. The session id is saved per identity, so a restarted bridge resumes
// the same session when the agent, its arguments and the directory match.
func cmdBridgeACP(ctx context.Context, args []string) error {
	fs := flags("bridge acp")
	as := fs.String("as", "", "identity")
	cwd := fs.String("cwd", "", "the agent session's working directory (default: the current one)")
	sessionID := fs.String("session", "", "load this agent session")
	fresh := fs.Bool("new", false, "start a new agent session instead of resuming the saved one")
	maxDepth := fs.Int("max-depth", bridge.DefaultMaxDepth, "stop automatic replies at this exchange depth")
	turnTimeout := fs.Duration("turn-timeout", acp.DefaultTurnTimeout, "cancel a turn that runs longer than this")
	dash := slices.Index(args, "--")
	if dash < 0 || dash == len(args)-1 {
		return usageError(acpUsage)
	}
	if err := parse(fs, args[:dash]); err != nil {
		return err
	}
	switch {
	case fs.NArg() > 0:
		return usageError(acpUsage)
	case *fresh && *sessionID != "":
		return usageError("-new and -session are mutually exclusive")
	case *maxDepth < 1:
		return usageError("-max-depth must be at least 1")
	case *turnTimeout <= 0:
		return usageError("-turn-timeout must be positive")
	}
	command := args[dash+1:]
	a, err := client.OpenAgent(*as)
	if err != nil {
		return err
	}
	dir, err := filepath.Abs(*cwd)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	statePath, err := acpStatePath(a.ID.Alias)
	if err != nil {
		return err
	}
	resume := *sessionID
	if st, ok := loadACPState(statePath); ok && resume == "" && !*fresh && slices.Equal(st.Command, command) && st.Cwd == dir {
		resume = st.SessionID
	}

	// Setup includes session/load, which replays the whole history: allow it a while.
	startCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	s, err := acp.Start(startCtx, acp.Options{Command: command, Cwd: dir, SessionID: resume,
		TurnTimeout: *turnTimeout, Version: version(), Stderr: os.Stderr, Log: log})
	cancel()
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return nil // interrupted during setup
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w (is %q an ACP agent? it did not finish setting up within 2 minutes)", err, command[0])
	default:
		return err
	}
	defer s.Close()
	if *sessionID != "" && s.ID() != *sessionID {
		return fmt.Errorf("%s did not load session %q (see the log above); run without -session to start a new one", command[0], *sessionID)
	}
	if err := saveACPState(statePath, acpState{Command: command, Cwd: dir, SessionID: s.ID()}); err != nil {
		log.Warn("session not saved; the next run starts a new one", "err", err)
	}
	fmt.Printf("ACP session %s\n", s.ID())

	runCtx, stop := context.WithCancel(ctx) // ends when we're interrupted or the agent exits
	defer stop()
	go func() {
		select {
		case <-s.Done():
			stop()
		case <-runCtx.Done():
		}
	}()
	bridge.Run(runCtx, a, s, bridge.Options{MaxDepth: *maxDepth, Log: log})
	if ctx.Err() != nil {
		return nil // interrupted: a clean stop
	}
	return s.Err()
}

// acpState is what `bridge acp` remembers per identity between runs.
type acpState struct {
	Command   []string `json:"command"`
	Cwd       string   `json:"cwd"`
	SessionID string   `json:"session_id"`
}

func acpStatePath(alias string) (string, error) {
	home, err := client.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "bridges", alias+".acp.json"), nil // alias was validated by client.Load
}

// loadACPState reads saved state; a missing or unreadable file means none.
func loadACPState(path string) (acpState, bool) {
	var st acpState
	b, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(b, &st) != nil {
		return acpState{}, false
	}
	return st, st.SessionID != ""
}

// ponytail: a plain write, not temp+rename: a torn write costs at most a new session.
func saveACPState(path string, st acpState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
