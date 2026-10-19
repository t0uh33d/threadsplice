package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// The terminal, as readInvite reaches it; variables so tests can stand in.
var (
	openTTY   = func() (*os.File, error) { return os.OpenFile("/dev/tty", os.O_RDWR, 0) }
	hideInput = sttyHide
)

// visiblePrompt asks for an invite where it can't be kept off the screen.
const visiblePrompt = "Paste the invite (this terminal will show it; pipe it in to keep it off the screen): "

// readInvite reads an invite that wasn't given as an argument, which keeps it
// out of shell history and the process list: typed or pasted at the terminal
// with echo off, or piped in on in. prompt is where the prompt goes (stderr,
// as stdout may be redirected).
func readInvite(ctx context.Context, in io.Reader, prompt io.Writer) (string, error) {
	f, ok := in.(*os.File)
	if !ok || !isTerminal(f) {
		return readLine(in)
	}
	tty, err := openTTY()
	if err != nil {
		// ponytail: no /dev/tty or stty on Windows, so the invite shows as it's
		// typed there. Read the console in raw mode if that matters.
		fmt.Fprint(prompt, visiblePrompt)
		return readLineCtx(ctx, f)
	}
	defer tty.Close()
	// Echo goes off first: the prompt says the invite isn't shown only when
	// that worked (it doesn't without stty, for one).
	restore := hideInput(tty)
	if restore == nil {
		fmt.Fprint(prompt, visiblePrompt)
		return readLineCtx(ctx, tty)
	}
	defer restore()
	fmt.Fprint(prompt, "Paste the invite (it isn't shown): ")
	defer fmt.Fprintln(prompt) // the Enter that ends it isn't echoed either
	return readLineCtx(ctx, tty)
}

// isTerminal reports whether f is a terminal (a character device).
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// sttyHide turns off the terminal's echo and returns what turns it back on,
// or nil when it couldn't.
func sttyHide(tty *os.File) func() {
	stty := func(args ...string) ([]byte, error) {
		cmd := exec.Command("stty", args...)
		cmd.Stdin = tty // stty works on the terminal on its stdin
		return cmd.Output()
	}
	saved, err := stty("-g")
	if err != nil {
		return nil
	}
	if _, err := stty("-echo"); err != nil {
		return nil
	}
	return func() { stty(strings.TrimSpace(string(saved))) }
}

// readLineCtx is readLine that gives up when ctx ends (Ctrl-C), so the
// deferred restore turns the echo back on.
func readLineCtx(ctx context.Context, r io.Reader) (string, error) {
	type line struct {
		s   string
		err error
	}
	ch := make(chan line, 1)
	go func() {
		s, err := readLine(r)
		ch <- line{s, err}
	}()
	select {
	case l := <-ch:
		return l.s, l.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// readLine reads the first line of r (at most 4 KiB), trimmed.
func readLine(r io.Reader) (string, error) {
	s, err := bufio.NewReader(io.LimitReader(r, 4<<10)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if s = strings.TrimSpace(s); s == "" {
		return "", usageError(joinUsage)
	}
	return s, nil
}
