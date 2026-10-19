package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/t0uh33d/threadsplice/internal/api"
	"github.com/t0uh33d/threadsplice/internal/client"
	"github.com/t0uh33d/threadsplice/internal/relay"
)

// TestMain fences the environment: these tests write identities, and may run
// inside a Claude Code session.
func TestMain(m *testing.M) {
	for _, k := range []string{"CLAUDE_CODE_MESSAGING_SOCKET", "CLAUDE_CODE_MESSAGING_TOKEN", "CLAUDE_CODE_SESSION_ID", "CLAUDE_PID",
		"CLAUDE_CODE_ENTRYPOINT", "CLAUDE_PROJECT_DIR", "THREADSPLICE_AS", "THREADSPLICE_ADMIN_TOKEN", "THREADSPLICE_RELAY"} {
		os.Unsetenv(k)
	}
	home, err := os.MkdirTemp("", "threadsplice-test")
	if err != nil {
		panic(err)
	}
	os.Setenv("THREADSPLICE_HOME", home)
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// An invite that isn't on the command line is read from a pipe as is, with
// no prompt; only a terminal gets one.
func TestReadInvite(t *testing.T) {
	ctx := context.Background()
	const inv = "https://relay.example.com#tok.key"
	var prompt bytes.Buffer
	if got, err := readInvite(ctx, strings.NewReader("  "+inv+" \r\nsomething else\n"), &prompt); got != inv || err != nil {
		t.Errorf("from a reader: %q, %v", got, err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	w.WriteString(inv) // no newline: the pipe just ends
	w.Close()
	if got, err := readInvite(ctx, r, &prompt); got != inv || err != nil {
		t.Errorf("from a pipe: %q, %v", got, err)
	}
	r.Close()
	if prompt.Len() != 0 {
		t.Errorf("prompted without a terminal: %q", prompt.String())
	}
	var ue usageError
	if _, err := readInvite(ctx, strings.NewReader("\n"), &prompt); !errors.As(err, &ue) || !strings.Contains(err.Error(), "or pipe it in") {
		t.Errorf("nothing piped: %v", err)
	}
	if got, _ := readInvite(ctx, strings.NewReader(strings.Repeat("x", 8<<10)), &prompt); len(got) != 4<<10 {
		t.Errorf("read %d bytes of a huge line", len(got))
	}
	if !strings.Contains(usage, "type or\n") || !strings.Contains(usage, "paste the invite when asked, or pipe it in") {
		t.Error("the usage text doesn't say the invite can be pasted or piped")
	}
}

// At a terminal, the prompt says the invite isn't shown only once echo is
// off; where it can't be turned off, it says the invite will show.
func TestReadInviteTerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no /dev/tty on Windows")
	}
	const inv = "https://relay.example.com#tok.key"
	stdin, err := os.Open(os.DevNull) // a character device, so readInvite takes it for a terminal
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	oldTTY, oldHide := openTTY, hideInput
	t.Cleanup(func() { openTTY, hideInput = oldTTY, oldHide })
	for _, hides := range []bool{false, true} {
		r, w, err := os.Pipe() // stands in for the terminal
		if err != nil {
			t.Fatal(err)
		}
		w.WriteString(inv + "\n")
		w.Close()
		openTTY = func() (*os.File, error) { return r, nil }
		restored := false
		hideInput = func(tty *os.File) func() {
			if tty != r || !hides {
				return nil // as without stty
			}
			return func() { restored = true }
		}
		var prompt bytes.Buffer
		got, err := readInvite(context.Background(), stdin, &prompt)
		if got != inv || err != nil {
			t.Errorf("echo off %v: read %q, %v", hides, got, err)
		}
		switch p := prompt.String(); {
		case hides && (p != "Paste the invite (it isn't shown): \n" || !restored):
			t.Errorf("echo off: prompt %q, echo restored %v", p, restored)
		case !hides && (p != visiblePrompt || strings.Contains(p, "isn't shown")):
			t.Errorf("echo on: prompt %q", p)
		}
	}
}

// A default name another member has moves on to -2 … -9; a chosen one, or
// another error, stops at once.
func TestJoinAs(t *testing.T) {
	taken := map[string]bool{"mac": true, "mac-2": true}
	var tried []string
	join := func(name string) (api.Membership, error) {
		tried = append(tried, name)
		switch {
		case name == "denied":
			return api.Membership{}, &client.StatusError{Code: http.StatusForbidden, Msg: "invite is invalid"}
		case taken[name]:
			return api.Membership{}, &client.StatusError{Code: http.StatusConflict, Msg: "taken"}
		}
		return api.Membership{RoomName: "proj"}, nil
	}
	if _, name, err := joinAs("mac", true, join); name != "mac-3" || err != nil || !slices.Equal(tried, []string{"mac", "mac-2", "mac-3"}) {
		t.Errorf("default name: %q, %v after %q", name, err, tried)
	}
	tried = nil
	if _, _, err := joinAs("mac", false, join); err == nil || !slices.Equal(tried, []string{"mac"}) {
		t.Errorf("chosen name: %v after %q", err, tried)
	}
	tried = nil
	if _, _, err := joinAs("denied", true, join); err == nil || !slices.Equal(tried, []string{"denied"}) {
		t.Errorf("a refused invite: %v after %q", err, tried)
	}
	for i := 3; i <= 9; i++ {
		taken["mac-"+string(rune('0'+i))] = true
	}
	if _, _, err := joinAs("mac", true, join); err == nil || !strings.Contains(err.Error(), "pick a name with -as") {
		t.Errorf("every default name taken: %v", err)
	}
}

// capture runs f with stdout going to a pipe and returns what it printed.
func capture(t *testing.T, f func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(r)
		done <- b
	}()
	ferr := f()
	w.Close()
	os.Stdout = old
	out := string(<-done)
	if ferr != nil {
		t.Fatalf("%v\n%s", ferr, out)
	}
	return out
}

// withStdin runs f with s on stdin, as when it's piped in.
func withStdin(t *testing.T, s string, f func() error) error {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	in, err := os.Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	old := os.Stdin
	os.Stdin = in
	defer func() { os.Stdin = old }()
	return f()
}

// room create and join without -as name this machine's membership after its
// kind and mark it as the machine's; the invite they print travels without a
// command line; identities and sessions show session members.
func TestRoomCreateAndJoin(t *testing.T) {
	ctx := context.Background()
	const admin = "test-admin-token-0123456789"
	rs, err := relay.New(relay.Config{DBPath: filepath.Join(t.TempDir(), "r.db"), AdminToken: admin, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(rs.Handler())
	t.Cleanup(func() { ts.Close(); rs.Close() })
	machine := client.DefaultMachineName()

	t.Setenv("THREADSPLICE_HOME", t.TempDir())
	out := capture(t, func() error {
		return run(ctx, []string{"room", "create", "-relay", ts.URL, "-name", "proj", "-admin-token", admin})
	})
	var invite string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "  "+ts.URL+"#") {
			invite = strings.TrimSpace(l)
		}
	}
	// A test binary isn't a release's build, so go install comes first: the
	// installer script exists only once a release does.
	for _, want := range []string{`Created room "proj"; you are "` + machine + `".`,
		"On the other machine, install thread splice, then join (it asks for the invite):\n" +
			"  go install github.com/t0uh33d/threadsplice/cmd/threadsplice@latest\n  threadsplice join\n" +
			"Without Go (from the first release on), install it with:\n" +
			"  curl -fsSL https://github.com/t0uh33d/threadsplice/releases/latest/download/install.sh | sh\n", nextStep} {
		if !strings.Contains(out, want) {
			t.Errorf("room create's output lacks %q:\n%s", want, out)
		}
	}
	if invite == "" || strings.Contains(out, "threadsplice join -as") {
		t.Fatalf("room create's output:\n%s", out)
	}
	// Pasted into zsh, whose default has no interactive comments, a "# …"
	// after a command would become its arguments.
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "#") && !strings.HasPrefix(l, "  "+ts.URL+"#") {
			t.Errorf("a printed line has a comment: %q", l)
		}
	}
	if id, err := client.Load(machine); err != nil || !id.Machine || !id.Owner || id.Name != machine {
		t.Errorf("the owner's membership = %+v, %v", id, err)
	}
	// Another room: this machine has the local name already.
	out = capture(t, func() error {
		return run(ctx, []string{"room", "create", "-relay", ts.URL, "-name", "ops", "-admin-token", admin})
	})
	if want := `you are "` + machine + `" (saved here as "` + machine + `-ops": use -as ` + machine + `-ops).`; !strings.Contains(out, want) {
		t.Errorf("second room: want %q in\n%s", want, out)
	}
	if id, err := client.Load(machine + "-ops"); err != nil || id.Name != machine || id.RoomName != "ops" || !id.Machine {
		t.Errorf("second room's membership = %+v, %v", id, err)
	}
	ownerHome := os.Getenv("THREADSPLICE_HOME")

	// The other machine, of the same kind, joins with the invite piped in.
	t.Setenv("THREADSPLICE_HOME", t.TempDir())
	out = capture(t, func() error { return withStdin(t, invite+"\n", func() error { return run(ctx, []string{"join"}) }) })
	if want := `Joined room "proj" as "` + machine + `-2".` + "\n" + nextStep + "\n"; out != want {
		t.Errorf("join's output:\n got %q\nwant %q", out, want)
	}
	if id, err := client.Load(machine + "-2"); err != nil || !id.Machine || id.Owner || id.Name != machine+"-2" || id.RoomName != "proj" {
		t.Errorf("the joined membership = %+v, %v", id, err)
	}
	// -as may follow the invite; the relay refuses an invite used already.
	var ue usageError
	if err := run(ctx, []string{"join", invite, "-as", "carol"}); err == nil || errors.As(err, &ue) || !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("joining with a used invite: %v", err)
	}
	// join -as makes a fixed identity for one agent, not a second machine
	// membership: Claude Code sessions keep a single one to join under.
	joinHome := os.Getenv("THREADSPLICE_HOME")
	t.Setenv("THREADSPLICE_HOME", ownerHome)
	out = capture(t, func() error { return run(ctx, []string{"invite", "-as", machine}) })
	t.Setenv("THREADSPLICE_HOME", joinHome)
	var second string
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "  "+ts.URL+"#") {
			second = strings.TrimSpace(l)
		}
	}
	out = capture(t, func() error {
		return withStdin(t, second+"\n", func() error { return run(ctx, []string{"join", "-as", "codex-" + machine}) })
	})
	if want := `Joined room "proj" as "codex-` + machine + `".` + "\n" + fmt.Sprintf(fixedStep, "codex-"+machine); out != want {
		t.Errorf("join -as's output:\n got %q\nwant %q", out, want)
	}
	if id, err := client.Load("codex-" + machine); err != nil || id.Machine || id.Name != "codex-"+machine {
		t.Errorf("the fixed identity = %+v, %v", id, err)
	}
	if ms, err := client.MachineMemberships("proj", ""); err != nil || len(ms) != 1 || ms[0].Alias != machine+"-2" {
		t.Errorf("memberships sessions join under: %+v, %v", ms, err)
	}

	// identities shows session members under their machine, sessions the
	// sessions that joined.
	t.Setenv("THREADSPLICE_HOME", ownerHome)
	mac, err := client.Load(machine)
	if err != nil {
		t.Fatal(err)
	}
	tok := client.NewToken()
	m, err := client.New(mac.Relay, mac.Token).CreateChild(ctx, mac.RoomID, machine+".repo", client.TokenHash(tok))
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SaveChild(client.Identity{Relay: mac.Relay, RoomID: mac.RoomID, RoomName: "proj", ParticipantID: m.ParticipantID,
		Name: machine + ".repo", Parent: mac.ParticipantID, ParentAlias: machine, Token: tok, RoomKey: mac.RoomKey}); err != nil {
		t.Fatal(err)
	}
	out = capture(t, func() error { return run(ctx, []string{"identities"}) })
	if want := "\n  " + machine + "/" + machine + ".repo "; !strings.Contains(out, want) || strings.Index(out, want) < strings.Index(out, "\n"+machine+" ") {
		t.Errorf("identities: want %q under %q in\n%s", want, machine, out)
	}
	if out := capture(t, func() error { return run(ctx, []string{"sessions"}) }); !strings.HasPrefix(out, "No Claude Code session on this machine has joined a room.") {
		t.Errorf("sessions without any: %s", out)
	}
	if err := client.SetCurrent(os.Getpid(), "0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	for sid, pid := range map[string]int{"0123456789abcdef": os.Getpid(), "fedcba9876543210": os.Getpid()} {
		if err := client.SaveBinding(client.Binding{SessionID: sid, Machine: machine, Child: machine + ".repo", PID: pid}); err != nil {
			t.Fatal(err)
		}
	}
	out = capture(t, func() error { return run(ctx, []string{"sessions"}) })
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 || strings.Fields(lines[0])[0] != "MEMBER" ||
		!slices.Equal(strings.Fields(lines[1]), []string{machine + ".repo", "proj", "01234567", "live"}) ||
		!slices.Equal(strings.Fields(lines[2]), []string{machine + ".repo", "proj", "fedcba98", "stale"}) {
		t.Errorf("sessions:\n%s", out)
	}

	// revoke says when a machine's session members go with it.
	out = capture(t, func() error { return run(ctx, []string{"revoke", "-as", machine, machine + "-2"}) })
	if !strings.Contains(out, `Revoked "`+machine+`-2".`) || strings.Contains(out, "session member") {
		t.Errorf("revoke without members: %s", out)
	}
	out = capture(t, func() error { return run(ctx, []string{"revoke", "-as", machine, machine + ".repo"}) })
	if !strings.Contains(out, `Revoked "`+machine+`.repo".`) {
		t.Errorf("revoke a session member: %s", out)
	}
}

// A release's build points the other machine to the installer script first;
// any other build to go install, as the script exists only once a release does.
func TestInstallSteps(t *testing.T) {
	for released, first := range map[bool]string{true: installScript, false: goInstall} {
		got := installSteps(released)
		if !strings.HasPrefix(got, "  "+first+"\n  threadsplice join\n") || strings.Count(got, "\n  ") != 2 || strings.Contains(got, "#") {
			t.Errorf("released %v:\n%s", released, got)
		}
	}
}

// Usage errors for the commands that changed.
func TestUsage(t *testing.T) {
	ctx := context.Background()
	var ue usageError
	for _, args := range [][]string{
		{"room", "create", "-relay", "https://relay.example.com"},
		{"join", "a", "b"},
		{"sessions", "extra"},
	} {
		if err := run(ctx, args); !errors.As(err, &ue) {
			t.Errorf("%q: %v, want a usage error", args, err)
		}
	}
}
