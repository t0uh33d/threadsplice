package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"html"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
	open := fs.Bool("open", false, "also sign the default browser in, once")
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
		// No token goes on a command line, where other local users can read
		// it (/proc/<pid>/cmdline on Linux): the browser opens a page only
		// this user can read, which sends it on with a link that works once.
		opt.Launch = rand.Text()
		page, err := launchPage(link + opt.Launch)
		if page != "" {
			defer os.RemoveAll(filepath.Dir(page))
		}
		if err == nil {
			err = openBrowser(page)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "threadsplice ui: couldn't open a browser:", err)
		} else {
			fmt.Println("Opening your browser. If it doesn't sign in (a Snap or Flatpak browser can't read the page it gets), use the link above.")
		}
	}
	return ui.Serve(ctx, ln, opt)
}

// launchPage writes a page that sends a browser on to link, in a new
// directory only this user can read, and returns its path.
// ponytail: a sandboxed browser (Snap, Flatpak) may not see the temp
// directory; the printed link still works there.
func launchPage(link string) (string, error) {
	dir, err := os.MkdirTemp("", "threadsplice-ui-") // 0700
	if err != nil {
		return "", err
	}
	page, esc := filepath.Join(dir, "open.html"), html.EscapeString(link)
	return page, os.WriteFile(page, []byte(`<!doctype html><meta charset="utf-8"><meta http-equiv="refresh" content="0;url=`+esc+
		`"><title>thread splice</title><a href="`+esc+`">Open thread splice</a>`+"\n"), 0o600)
}

// openBrowser opens target, a page's path, in the default browser. A variable so tests can see what it gets.
var openBrowser = func(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	go cmd.Wait() // reap it
	return nil
}
