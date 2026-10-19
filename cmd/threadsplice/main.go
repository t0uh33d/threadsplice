// Command threadsplice lets AI coding-agent sessions on different machines talk
// to each other through a self-hosted, end-to-end encrypted relay.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/t0uh33d/threadsplice/internal/api"
	"github.com/t0uh33d/threadsplice/internal/client"
	"github.com/t0uh33d/threadsplice/internal/e2ee"
	"github.com/t0uh33d/threadsplice/internal/relay"
)

const usage = `threadsplice — end-to-end encrypted messaging between AI coding agents on different machines.

Relay (run on your server, behind TLS):
  threadsplice relay [-addr :8080] [-db threadsplice.db]     needs THREADSPLICE_ADMIN_TOKEN

Rooms and identities:
  threadsplice room create -relay URL -name ROOM [-as NAME]  create a room, print an invite
  threadsplice invite [-as NAME]                             print a new single-use invite (owner)
  threadsplice join [-as NAME] [INVITE]                      join a room from another machine; type or
                                                             paste the invite when asked, or pipe it in
  threadsplice revoke [-as NAME] PARTICIPANT                 remove a participant (owner)
  threadsplice identities                                    list identities and session members here
  threadsplice sessions                                      list Claude Code sessions that joined a room

Web UI (local, loopback only):
  threadsplice ui [-addr 127.0.0.1:4848] [-open]             rooms, invites and the live conversation

Messaging:
  threadsplice peers [-as NAME] [-json]                      who is in the room, online, busy
  threadsplice send [-as NAME] [-to PEER] [-kind ask] TEXT   send (TEXT "-" reads stdin)
  threadsplice inbox [-as NAME] [-wait 30s] [-json]          print and acknowledge new messages

Agent integration:
  threadsplice setup <claude|codex|copilot|vscode> [-as NAME]   print the config that connects an agent
  threadsplice mcp [-as NAME] [-deliver auto|claude|codex|channel|hold]
                                                             MCP server (stdio): tools + live delivery
  threadsplice hook <claude|codex|copilot> [event] [-as NAME]   lifecycle hook handler (JSON on stdin)
  threadsplice bridge codex [-as NAME] [-cwd DIR] ...        headless Codex answering the room
  threadsplice bridge acp [-as NAME] -- AGENT [ARGS...]      headless ACP agent (Copilot, Gemini, OpenCode...)

Identity selection: -as NAME, else $THREADSPLICE_AS, else the only identity. A session member is
MACHINE/MEMBER (see identities). room create and join name this machine's membership after its
kind (mac, linux, wsl, windows) unless -as says otherwise.
Claude Code without -as joins rooms one session at a time: see threadsplice setup claude.
Exit codes: 0 ok, 1 error, 2 usage.
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:])
	stop()
	var ue usageError
	switch {
	case err == nil:
	case errors.As(err, &ue):
		fmt.Fprintf(os.Stderr, "threadsplice: %v\nRun 'threadsplice help' for usage.\n", err)
		os.Exit(2)
	default:
		fmt.Fprintln(os.Stderr, "threadsplice:", err)
		os.Exit(1)
	}
}

type usageError string

func (e usageError) Error() string { return string(e) }

// subcommands registered by other files (mcp, hook, bridge).
var extra = map[string]func(context.Context, []string) error{}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return usageError("no command given")
	}
	cmd, args := args[0], args[1:]
	switch cmd {
	case "relay":
		return cmdRelay(ctx, args)
	case "room":
		if len(args) == 0 || args[0] != "create" {
			return usageError("usage: threadsplice room create -relay URL -name ROOM [-as NAME]")
		}
		return cmdRoomCreate(ctx, args[1:])
	case "invite":
		return cmdInvite(ctx, args)
	case "join":
		return cmdJoin(ctx, args)
	case "revoke":
		return cmdRevoke(ctx, args)
	case "identities":
		return cmdIdentities()
	case "sessions":
		return cmdSessions(args)
	case "peers":
		return cmdPeers(ctx, args)
	case "send":
		return cmdSend(ctx, args)
	case "inbox":
		return cmdInbox(ctx, args)
	case "version", "--version", "-version":
		fmt.Println("threadsplice", version())
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	}
	if f, ok := extra[cmd]; ok {
		return f(ctx, args)
	}
	return usageError(fmt.Sprintf("unknown command %q", cmd))
}

func version() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		return bi.Main.Version
	}
	return "dev"
}

// flags returns a FlagSet whose parse errors become usage errors.
func flags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		return usageError(fmt.Sprintf("%s: %v", fs.Name(), err))
	}
	return nil
}

func cmdRelay(ctx context.Context, args []string) error {
	fs := flags("relay")
	addr := fs.String("addr", envOr("THREADSPLICE_ADDR", ":8080"), "listen address")
	db := fs.String("db", envOr("THREADSPLICE_DB", "threadsplice.db"), "SQLite database path")
	retention := fs.Duration("retention", 30*24*time.Hour, "delete messages older than this")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *retention < time.Hour {
		return usageError("relay: -retention must be at least 1h")
	}
	return relay.Run(ctx, *addr, relay.Config{
		DBPath:     *db,
		AdminToken: os.Getenv("THREADSPLICE_ADMIN_TOKEN"),
		Retention:  *retention,
		Log:        slog.New(slog.NewJSONHandler(os.Stderr, nil)),
	})
}

func cmdRoomCreate(ctx context.Context, args []string) error {
	fs := flags("room create")
	relayURL := fs.String("relay", os.Getenv("THREADSPLICE_RELAY"), "relay base URL, e.g. https://relay.example.com")
	name := fs.String("name", "", "room name")
	as := fs.String("as", "", "this machine's participant name in the room (default "+client.DefaultMachineName()+")")
	adminToken := fs.String("admin-token", "", "relay admin token (default $THREADSPLICE_ADMIN_TOKEN)")
	ttl := fs.Duration("invite-ttl", 24*time.Hour, "how long the printed invite stays valid")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *relayURL == "" || *name == "" {
		return usageError("room create needs -relay and -name")
	}
	if err := client.CheckRelayURL(*relayURL); err != nil {
		return err
	}
	if *adminToken == "" {
		*adminToken = os.Getenv("THREADSPLICE_ADMIN_TOKEN")
	}
	owner, alias := *as, *as
	if owner == "" {
		owner = client.DefaultMachineName()
		var err error
		if alias, err = localName(owner, *name); err != nil {
			return err
		}
	} else if _, err := client.Load(owner); err == nil {
		return fmt.Errorf("identity %q already exists on this machine; pick another -as name", owner)
	}
	m, err := client.New(*relayURL, *adminToken).CreateRoom(ctx, *name, owner)
	if err != nil {
		return err
	}
	if !api.ValidName(m.RoomName) { // it reaches models later; the relay must not slip text in
		return fmt.Errorf("the relay returned an invalid room name")
	}
	key := e2ee.NewKey() // generated here; the relay never sees it
	id := client.Identity{Alias: alias, Relay: strings.TrimRight(*relayURL, "/"), RoomID: m.RoomID, RoomName: m.RoomName,
		ParticipantID: m.ParticipantID, Name: owner, Owner: true, Machine: true, Token: m.Token, RoomKey: key.String()}
	if err := client.Save(id); err != nil {
		return err
	}
	fmt.Printf("Created room %q; you are %q%s.\n", m.RoomName, owner, savedAs(id))
	if err := printInvite(ctx, id, *ttl); err != nil {
		return err
	}
	fmt.Println(nextStep)
	return nil
}

// nextStep follows room create and join.
const nextStep = "Next, connect Claude Code on this machine (once): threadsplice setup claude"

// localName picks the local name of a new machine membership whose name the
// user didn't choose: its participant name, or, when this machine already
// uses that for another room, <name>-<room>, then -2 … -9, as the web UI does.
// -as selects by local name; peers see the participant name.
func localName(name, room string) (string, error) {
	taken, err := client.List()
	if err != nil {
		return "", err
	}
	base := name + "-" + room
	base = base[:min(len(base), 60)]
	cands := []string{name, base}
	for i := 2; i <= 9; i++ {
		cands = append(cands, base+"-"+strconv.Itoa(i))
	}
	for _, c := range cands {
		// EqualFold: on case-insensitive disks (macOS, Windows) Mac.json is mac.json.
		if client.ValidAlias(c) && !slices.ContainsFunc(taken, func(t string) bool { return strings.EqualFold(t, c) }) {
			return c, nil
		}
	}
	return "", fmt.Errorf("this machine already has identities named %q, %q and %q-2 … -9: pass -as", name, base, base)
}

// savedAs mentions an identity's local name when it isn't its participant name.
func savedAs(id client.Identity) string {
	if id.Alias == id.Name {
		return ""
	}
	return fmt.Sprintf(" (saved here as %q: use -as %s)", id.Alias, id.Alias)
}

func cmdInvite(ctx context.Context, args []string) error {
	fs := flags("invite")
	as := fs.String("as", "", "identity")
	ttl := fs.Duration("ttl", 24*time.Hour, "invite lifetime (max 168h)")
	if err := parse(fs, args); err != nil {
		return err
	}
	id, err := client.Load(*as)
	if err != nil {
		return err
	}
	return printInvite(ctx, id, *ttl)
}

func printInvite(ctx context.Context, id client.Identity, ttl time.Duration) error {
	key, err := id.Key()
	if err != nil {
		return err
	}
	inv, err := client.New(id.Relay, id.Token).CreateInvite(ctx, id.RoomID, ttl)
	if err != nil {
		return err
	}
	// The user pastes the token elsewhere, possibly into a shell: a relay must
	// not be able to slip shell syntax into it.
	if !inviteToken.MatchString(inv.Token) {
		return fmt.Errorf("the relay returned a malformed invite token")
	}
	// The invite is printed on its own, and the other machine reads it at a
	// prompt: in a command line it would land in shell history and the
	// process list.
	fmt.Printf(`
Invite (single use, expires %s). It contains the room key: share it privately.

  %s

On the other machine:
  curl -fsSL https://github.com/t0uh33d/threadsplice/releases/latest/download/install.sh | sh
  threadsplice join          # paste the invite when asked
(or: go install github.com/t0uh33d/threadsplice/cmd/threadsplice@latest)

`, inv.ExpiresAt.Local().Format(time.RFC1123), client.FormatInvite(id.Relay, inv.Token, key))
	return nil
}

// inviteToken is the shape of the invite tokens the relay mints (base64url).
var inviteToken = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

const joinUsage = "usage: threadsplice join [-as NAME] [INVITE] (without INVITE, type or paste it when asked, or pipe it in)"

func cmdJoin(ctx context.Context, args []string) error {
	fs := flags("join")
	as := fs.String("as", "", "this machine's participant name in the room (default "+client.DefaultMachineName()+")")
	pos, err := parseMixed(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return usageError(joinUsage)
	}
	invite := ""
	if len(pos) == 1 {
		invite = pos[0]
	} else if invite, err = readInvite(ctx, os.Stdin, os.Stderr); err != nil {
		return err
	}
	relayURL, token, key, err := client.ParseInvite(invite)
	if err != nil {
		return err
	}
	if err := client.CheckRelayURL(relayURL); err != nil {
		return err
	}
	name, defaulted := *as, *as == ""
	if defaulted {
		name = client.DefaultMachineName()
	} else if _, err := client.Load(name); err == nil {
		return fmt.Errorf("identity %q already exists on this machine; pick another -as name", name)
	}
	c := client.New(relayURL, "") // the key stays local
	m, name, err := joinAs(name, defaulted, func(name string) (api.Membership, error) { return c.Join(ctx, token, name) })
	if err != nil {
		return err
	}
	if !api.ValidName(m.RoomName) { // it reaches models later; the relay must not slip text in
		return fmt.Errorf("the relay returned an invalid room name")
	}
	alias := name
	if defaulted {
		if alias, err = localName(name, m.RoomName); err != nil {
			return err
		}
	}
	id := client.Identity{Alias: alias, Relay: relayURL, RoomID: m.RoomID, RoomName: m.RoomName,
		ParticipantID: m.ParticipantID, Name: name, Machine: true, Token: m.Token, RoomKey: key.String()}
	if err := client.Save(id); err != nil {
		return err
	}
	fmt.Printf("Joined room %q as %q%s.\n", m.RoomName, name, savedAs(id))
	fmt.Println(nextStep)
	return nil
}

// joinAs redeems an invite as name through join. A name the user didn't
// choose moves on to name-2 … name-9 while another member has it; a chosen
// one doesn't. It returns the name the relay took.
func joinAs(name string, defaulted bool, join func(name string) (api.Membership, error)) (api.Membership, string, error) {
	names := []string{name}
	if defaulted {
		for i := 2; i <= 9; i++ {
			names = append(names, name+"-"+strconv.Itoa(i))
		}
	}
	var m api.Membership
	var err error
	for _, n := range names {
		m, err = join(n)
		if se := (*client.StatusError)(nil); !errors.As(err, &se) || se.Code != http.StatusConflict {
			return m, n, err
		}
	}
	if defaulted {
		err = fmt.Errorf("%q and %q-2 … -9 are all taken in this room, so pick a name with -as (%w)", name, name, err)
	}
	return m, name, err
}

func cmdRevoke(ctx context.Context, args []string) error {
	fs := flags("revoke")
	as := fs.String("as", "", "identity (must be the room owner)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return usageError("usage: threadsplice revoke [-as NAME] PARTICIPANT")
	}
	a, err := client.OpenAgent(*as)
	if err != nil {
		return err
	}
	ps, err := a.Peers(ctx)
	if err != nil {
		return err
	}
	for _, p := range ps {
		if p.Name == fs.Arg(0) || p.ID == fs.Arg(0) {
			if err := a.Relay.Revoke(ctx, a.ID.RoomID, p.ID); err != nil {
				return err
			}
			fmt.Printf("Revoked %q. Their access ends now; anything they already received stays readable to them.\n", p.Name)
			members := 0
			for _, c := range ps {
				if c.Parent == p.ID && !c.Revoked {
					members++
				}
			}
			if members > 0 { // the relay revokes a machine's session members with it
				fmt.Printf("Its %d session member(s) are revoked with it.\n", members)
			}
			return nil
		}
	}
	return fmt.Errorf("no participant %q in room %q", fs.Arg(0), a.ID.RoomName)
}

// cmdIdentities lists the identities on this machine, each machine
// membership with its session members indented under it.
func cmdIdentities() error {
	names, err := client.List()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "IDENTITY\tROOM\tNAME\tRELAY")
	for _, n := range names {
		id, err := client.Load(n)
		if err != nil {
			fmt.Fprintf(w, "%s\t(unreadable: %v)\t\t\n", n, err)
			continue
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", n, id.RoomName, id.Name, id.Relay)
		kids, err := client.ListChildren(n)
		if err != nil {
			fmt.Fprintf(w, "  %s/…\t(unreadable: %v)\t\t\n", n, err)
			continue
		}
		for _, k := range kids {
			name := k.Name
			if k.ParticipantID == "" {
				name += " (unfinished)" // the relay never confirmed it; the next join finishes or drops it
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", k.Alias, k.RoomName, name, k.Relay)
		}
	}
	return w.Flush()
}

// cmdSessions lists the Claude Code sessions on this machine that joined a
// room as a member of their own: live while a Claude Code process shows the
// conversation, stale otherwise (resuming it rejoins).
func cmdSessions(args []string) error {
	if len(args) > 0 {
		return usageError("usage: threadsplice sessions")
	}
	bs, err := client.Bindings()
	if err != nil {
		return err
	}
	if len(bs) == 0 {
		fmt.Println("No Claude Code session on this machine has joined a room. To join one, say in it: join the thread splice room")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "MEMBER\tROOM\tSESSION\tSTATUS")
	for _, b := range bs {
		room := "?"
		if id, err := client.LoadChild(b.Machine, b.Child); err == nil {
			room = id.RoomName
		}
		status := "stale"
		if b.Live() {
			status = "live"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", b.Child, room, b.SessionID[:min(8, len(b.SessionID))], status)
	}
	return w.Flush()
}

func cmdPeers(ctx context.Context, args []string) error {
	fs := flags("peers")
	as := fs.String("as", "", "identity")
	asJSON := fs.Bool("json", false, "machine-readable output")
	if err := parse(fs, args); err != nil {
		return err
	}
	a, err := client.OpenAgent(*as)
	if err != nil {
		return err
	}
	ps, err := a.Peers(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(os.Stdout).Encode(ps)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSTATUS\tSTATE\tLAST SEEN")
	for _, p := range ps {
		status := "offline"
		switch {
		case p.Revoked:
			status = "revoked"
		case p.Online:
			status = "online"
		}
		name := p.Name
		if p.ID == a.ID.ParticipantID {
			name += " (you)"
		}
		seen := "never"
		if !p.LastSeen.IsZero() {
			seen = time.Since(p.LastSeen).Round(time.Second).String() + " ago"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", name, status, orDash(p.State), seen)
	}
	return w.Flush()
}

func cmdSend(ctx context.Context, args []string) error {
	fs := flags("send")
	as := fs.String("as", "", "identity")
	to := fs.String("to", "", "recipient participant name (default: whole room)")
	kind := fs.String("kind", api.KindMessage, "message, ask (expects a reply) or reply")
	replyTo := fs.String("reply-to", "", "id of the message this answers")
	if err := parse(fs, args); err != nil {
		return err
	}
	// Go's flag package stops at the first non-flag: `send hi -to bob` would
	// otherwise send "hi -to bob" to the whole room.
	for _, a := range fs.Args() {
		name, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if strings.HasPrefix(a, "-") && fs.Lookup(name) != nil {
			return usageError(fmt.Sprintf("send: flag %s must come before the message text", a))
		}
	}
	text := strings.Join(fs.Args(), " ")
	if text == "-" {
		const limit = 96 << 10
		b, err := io.ReadAll(io.LimitReader(os.Stdin, limit+1))
		if err != nil {
			return err
		}
		if len(b) > limit {
			return fmt.Errorf("message on stdin is larger than %d KiB", limit>>10)
		}
		text = string(b)
	}
	if strings.TrimSpace(text) == "" {
		return usageError("usage: threadsplice send [-to PEER] [-kind ask] TEXT")
	}
	if *kind != api.KindMessage && *kind != api.KindAsk && *kind != api.KindReply {
		return usageError("-kind must be message, ask or reply")
	}
	a, err := client.OpenAgent(*as)
	if err != nil {
		return err
	}
	id, err := a.Send(ctx, *to, api.Payload{Kind: *kind, Text: text, ReplyTo: *replyTo})
	if err != nil {
		return err
	}
	fmt.Println(id)
	return nil
}

func cmdInbox(ctx context.Context, args []string) error {
	fs := flags("inbox")
	as := fs.String("as", "", "identity")
	wait := fs.Duration("wait", 0, "block up to this long for a message (max 30s per poll)")
	asJSON := fs.Bool("json", false, "one JSON object per message")
	if err := parse(fs, args); err != nil {
		return err
	}
	a, err := client.OpenAgent(*as)
	if err != nil {
		return err
	}
	deadline := time.Now().Add(*wait)
	for {
		msgs, err := a.Inbox(ctx, min(time.Until(deadline), 30*time.Second))
		if err != nil {
			return err
		}
		if len(msgs) > 0 || !time.Now().Before(deadline) {
			if *asJSON {
				enc := json.NewEncoder(os.Stdout)
				for _, m := range msgs {
					enc.Encode(jsonMessage(m))
				}
				return nil
			}
			fmt.Print(client.Format(a.ID.RoomName, msgs))
			return nil
		}
	}
}

func jsonMessage(m client.Message) map[string]any {
	out := map[string]any{"seq": m.Seq, "id": m.ID, "from": m.FromName, "from_id": m.From, "direct": m.Direct,
		"at": m.At, "kind": m.Kind, "text": m.Text, "reply_to": m.ReplyTo, "depth": m.Depth}
	if m.Err != nil {
		out["error"] = m.Err.Error()
	}
	return out
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
