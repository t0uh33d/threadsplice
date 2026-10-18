package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/t0uh33d/threadsplice/internal/hooks"
)

func init() {
	extra["hook"] = cmdHook
}

// cmdHook runs one agent lifecycle hook (see package hooks). It exits 0 with
// nothing on stdout whenever something is wrong, usage errors included: exit
// code 2 means "block" to Claude Code and Codex.
func cmdHook(ctx context.Context, args []string) error {
	fs := flags("hook")
	as := fs.String("as", "", "identity")
	pos, err := parseMixed(fs, args)
	if err == nil && (len(pos) == 0 || len(pos) > 2) {
		err = usageError("usage: threadsplice hook <claude|codex|copilot> [event] [-as NAME]")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "threadsplice hook:", err)
		return nil
	}
	event := ""
	if len(pos) == 2 {
		event = pos[1]
	}
	hooks.Hook(ctx, pos[0], event, *as, os.Stdin, os.Stdout, os.Stderr)
	return nil
}

// parseMixed parses fs from args in which flags and positional arguments may
// be interleaved (hook claude -as NAME Stop), and returns the positional ones.
func parseMixed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := parse(fs, args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos, args = append(pos, fs.Arg(0)), fs.Args()[1:]
	}
}
