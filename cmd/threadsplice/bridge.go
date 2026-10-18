package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// bridges are headless agent drivers, registered by their own files.
var bridges = map[string]func(context.Context, []string) error{}

func init() {
	extra["bridge"] = func(ctx context.Context, args []string) error {
		var names []string
		for n := range bridges {
			names = append(names, n)
		}
		slices.Sort(names)
		if len(args) == 0 || bridges[args[0]] == nil {
			return usageError(fmt.Sprintf("usage: threadsplice bridge <%s> [flags]", strings.Join(names, "|")))
		}
		return bridges[args[0]](ctx, args[1:])
	}
}
