package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/t0uh33d/threadsplice/internal/bridge"
	"github.com/t0uh33d/threadsplice/internal/bridge/codex"
	"github.com/t0uh33d/threadsplice/internal/client"
)

func init() {
	bridges["codex"] = cmdBridgeCodex
}

// cmdBridgeCodex answers the room from a headless Codex thread (codex app-server).
func cmdBridgeCodex(ctx context.Context, args []string) error {
	fs := flags("bridge codex")
	as := fs.String("as", "", "identity")
	cwd := fs.String("cwd", ".", "working directory of the Codex thread")
	sandbox := fs.String("sandbox", "read-only", "Codex sandbox: read-only or workspace-write")
	thread := fs.String("thread", "", "resume this Codex thread id (default: the one this bridge used last)")
	fresh := fs.Bool("new", false, "start a new Codex thread instead of resuming the last one")
	maxDepth := fs.Int("max-depth", bridge.DefaultMaxDepth, "stop answering automatic ask/reply chains at this depth")
	turnTimeout := fs.Duration("turn-timeout", codex.DefaultTurnTimeout, "interrupt a turn that runs longer than this")
	bin := fs.String("codex", "codex", "codex executable")
	if err := parse(fs, args); err != nil {
		return err
	}
	switch {
	case fs.NArg() > 0:
		return usageError("usage: threadsplice bridge codex [-as NAME] [-cwd DIR] [-sandbox read-only|workspace-write] [-thread ID | -new] [-max-depth N] [-turn-timeout 15m] [-codex PATH]")
	case *sandbox != "read-only" && *sandbox != "workspace-write": // never danger-full-access for a remote peer
		return usageError("-sandbox must be read-only or workspace-write")
	case *fresh && *thread != "":
		return usageError("-new and -thread are mutually exclusive")
	case *maxDepth < 1:
		return usageError("-max-depth must be at least 1")
	case *turnTimeout <= 0:
		return usageError("-turn-timeout must be positive")
	}
	a, err := client.OpenAgent(*as)
	if err != nil {
		return err
	}
	state, err := codexStatePath(a.ID.Alias)
	if err != nil {
		return err
	}
	if *thread == "" && !*fresh {
		if *thread, err = loadCodexThread(state); err != nil {
			return err
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	startCtx, cancelStart := context.WithTimeout(ctx, 2*time.Minute) // a resume reloads the thread's history
	s, err := codex.Start(startCtx, codex.Options{Bin: *bin, Cwd: *cwd, Sandbox: *sandbox, ThreadID: *thread,
		TurnTimeout: *turnTimeout, Version: version(), Log: log})
	cancelStart()
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return nil // interrupted while starting
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w (is %q Codex? it did not set up a thread within 2 minutes)", err, *bin)
	case errors.Is(err, codex.ErrResume):
		return fmt.Errorf("%w (run with -new to start a fresh thread instead)", err)
	default:
		return err
	}
	defer s.Close()
	if err := saveCodexThread(state, s.ThreadID()); err != nil {
		return err
	}
	fmt.Printf("Codex thread %s\n", s.ThreadID())

	runCtx, cancel := context.WithCancel(ctx) // ends the bridge if Codex dies
	defer cancel()
	go func() {
		select {
		case <-s.Done():
			cancel()
		case <-runCtx.Done():
		}
	}()
	err = bridge.Run(runCtx, a, s, bridge.Options{MaxDepth: *maxDepth, Log: log})
	switch {
	case ctx.Err() != nil:
		return nil // stopped by a signal
	case s.Err() != nil:
		return s.Err()
	}
	return err
}

// codexState is what the bridge remembers per identity, so that a restart
// continues the same Codex conversation.
type codexState struct {
	ThreadID string `json:"thread_id"`
}

func codexStatePath(alias string) (string, error) {
	home, err := client.Home()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "bridges", alias+".codex.json"), nil // alias was validated by client.Load
}

func loadCodexThread(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	var st codexState
	if err := json.Unmarshal(b, &st); err != nil {
		return "", fmt.Errorf("%s: %w (delete it, or run with -new)", path, err)
	}
	return st.ThreadID, nil
}

// ponytail: a plain write, not temp+rename: a torn write costs at most a -new.
func saveCodexThread(path, id string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(codexState{ThreadID: id})
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
