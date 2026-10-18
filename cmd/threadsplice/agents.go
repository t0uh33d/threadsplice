package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/t0uh33d/threadsplice/internal/client"
	"github.com/t0uh33d/threadsplice/internal/mcp"
)

func init() {
	extra["mcp"] = cmdMCP
}

func cmdMCP(ctx context.Context, args []string) error {
	fs := flags("mcp")
	as := fs.String("as", "", "identity")
	deliver := fs.String("deliver", "auto", "how incoming messages reach the session: auto, claude (Claude Code inbox socket), codex (codex queue), channel (Claude Code channel events) or hold (inbox tool only)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if os.Getenv(client.BridgeEnv) != "" {
		// A bridged agent loaded the user's global MCP config: serving the
		// interactive identity here would let the bridge's session read it.
		return fmt.Errorf("not available inside a thread splice bridge session (%s is set)", client.BridgeEnv)
	}
	modes := strings.Join(mcp.Modes, "|")
	switch {
	case fs.NArg() > 0:
		return usageError("usage: threadsplice mcp [-as NAME] [-deliver " + modes + "]")
	case !slices.Contains(mcp.Modes, *deliver):
		return usageError(fmt.Sprintf("mcp: -deliver must be one of %s, not %q", modes, *deliver))
	}
	a, err := client.OpenAgent(*as)
	if err != nil {
		return err
	}
	// stdout carries the protocol; diagnostics go to stderr.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return mcp.Serve(ctx, a, os.Stdin, os.Stdout, mcp.Options{Deliver: *deliver, Version: version(), Log: log})
}
