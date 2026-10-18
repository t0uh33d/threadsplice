package main

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/t0uh33d/threadsplice/internal/client"
	"github.com/t0uh33d/threadsplice/internal/hooks"
)

func init() {
	extra["setup"] = cmdSetup
}

// cmdSetup prints the configuration that connects an agent to thread splice.
// It never writes the agent's files: the user reviews the config and adds it.
func cmdSetup(_ context.Context, args []string) error {
	fs := flags("setup")
	as := fs.String("as", "", "identity the agent uses")
	pos, err := parseMixed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 || !slices.Contains(hooks.SetupTargets, pos[0]) {
		return usageError("usage: threadsplice setup <" + strings.Join(hooks.SetupTargets, "|") + "> [-as NAME]")
	}
	id, err := client.Load(*as) // resolves the default identity and validates the name
	if err != nil {
		return err
	}
	text, _ := hooks.Setup(pos[0], id.Alias)
	fmt.Print(text)
	if exe, err := os.Executable(); err == nil {
		fmt.Printf("\nThese commands run threadsplice from the agent's PATH. If the agent can't find it there, use the full path: %s\n", exe)
	}
	if home := os.Getenv("THREADSPLICE_HOME"); home != "" {
		// Agents started from a desktop launcher or a long-lived daemon don't see the shell's variables.
		fmt.Printf("THREADSPLICE_HOME is set (%s): the agent must run with the same value, or its hooks and MCP server won't find this identity.\n", home)
	}
	return nil
}
