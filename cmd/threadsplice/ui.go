package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"

	"github.com/t0uh33d/threadsplice/internal/ui"
)

func init() {
	extra["ui"] = cmdUI
}

// cmdUI serves the local web UI (package ui) on a loopback address until
// interrupted. Each run prints a link with a fresh access token.
func cmdUI(ctx context.Context, args []string) error {
	fs := flags("ui")
	addr := fs.String("addr", "127.0.0.1:4848", "listen address (loopback only)")
	open := fs.Bool("open", false, "also open a one-time link in the default browser")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return usageError("usage: threadsplice ui [-addr 127.0.0.1:4848] [-open]")
	}
	if err := ui.CheckAddr(*addr); err != nil {
		return usageError("ui: " + err.Error())
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	if err := ui.CheckAddr(ln.Addr().String()); err != nil { // "localhost" may resolve to anything
		ln.Close()
		return err
	}
	opt := ui.Options{Token: rand.Text(), Version: version()}
	link := "http://" + ln.Addr().String() + "/?t="
	fmt.Println("thread splice UI: " + link + opt.Token)
	if *open {
		// The browser gets a link that works once: a browser started with a
		// URL can keep it on its command line, where other local users can
		// read it (/proc/<pid>/cmdline on Linux).
		opt.Launch = rand.Text()
		if err := openBrowser(link + opt.Launch); err != nil {
			fmt.Fprintln(os.Stderr, "threadsplice ui: couldn't open a browser:", err)
		}
	}
	return ui.Serve(ctx, ln, opt)
}

func openBrowser(link string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", link)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", link)
	default:
		cmd = exec.Command("xdg-open", link)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap it
	return nil
}
