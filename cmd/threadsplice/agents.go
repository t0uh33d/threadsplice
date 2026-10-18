package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/t0uh33d/threadsplice/internal/client"
	"github.com/t0uh33d/threadsplice/internal/mcp"
)

func init() {
	extra["mcp"] = cmdMCP
}

func cmdMCP(ctx context.Context, args []string) error {
	fs := flags("mcp")
	as := fs.String("as", "", "identity")
	channel := fs.Bool("channel", false, "push incoming messages as Claude Code channel events (start claude with this server as a channel)")
	if err := parse(fs, args); err != nil {
		return err
	}
	a, err := client.OpenAgent(*as)
	if err != nil {
		return err
	}
	// stdout carries the protocol; diagnostics go to stderr.
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	return mcp.Serve(ctx, a, os.Stdin, os.Stdout, mcp.Options{Channel: *channel, Version: version(), Log: log})
}
