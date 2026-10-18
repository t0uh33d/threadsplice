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
	"os"
	"os/signal"
	"runtime/debug"
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
  threadsplice room create -relay URL -name ROOM -as NAME    create a room, print an invite
  threadsplice invite [-as NAME]                             print a new single-use invite (owner)
  threadsplice join -as NAME INVITE                          join a room from another machine
  threadsplice revoke [-as NAME] PARTICIPANT                 remove a participant (owner)
  threadsplice identities                                    list identities on this machine

Messaging:
  threadsplice peers [-as NAME] [-json]                      who is in the room, online, busy
  threadsplice send [-as NAME] [-to PEER] [-kind ask] TEXT   send (TEXT "-" reads stdin)
  threadsplice inbox [-as NAME] [-wait 30s] [-json]          print and acknowledge new messages

Agent integration:
  threadsplice mcp [-as NAME] [-channel]                     MCP server (stdio) for Claude Code, Codex, Copilot...
  threadsplice hook <event> [-as NAME]                       lifecycle hook handler (see docs)
  threadsplice bridge <codex|acp> ...                        drive a headless agent session from the room

Identity selection: -as NAME, else $THREADSPLICE_AS, else the only identity.
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
			return usageError("usage: threadsplice room create -relay URL -name ROOM -as NAME")
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
	as := fs.String("as", "", "your participant name in the room")
	adminToken := fs.String("admin-token", "", "relay admin token (default $THREADSPLICE_ADMIN_TOKEN)")
	ttl := fs.Duration("invite-ttl", 24*time.Hour, "how long the printed invite stays valid")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *relayURL == "" || *name == "" || *as == "" {
		return usageError("room create needs -relay, -name and -as")
	}
	if err := client.CheckRelayURL(*relayURL); err != nil {
		return err
	}
	if *adminToken == "" {
		*adminToken = os.Getenv("THREADSPLICE_ADMIN_TOKEN")
	}
	if _, err := client.Load(*as); err == nil {
		return fmt.Errorf("identity %q already exists on this machine; pick another -as name", *as)
	}
	m, err := client.New(*relayURL, *adminToken).CreateRoom(ctx, *name, *as)
	if err != nil {
		return err
	}
	key := e2ee.NewKey() // generated here; the relay never sees it
	id := client.Identity{Alias: *as, Relay: strings.TrimRight(*relayURL, "/"), RoomID: m.RoomID, RoomName: m.RoomName,
		ParticipantID: m.ParticipantID, Name: *as, Owner: true, Token: m.Token, RoomKey: key.String()}
	if err := client.Save(id, false); err != nil {
		return err
	}
	fmt.Printf("Created room %q; you are %q.\n", m.RoomName, *as)
	return printInvite(ctx, id, *ttl)
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
	fmt.Printf(`
Invite (single use, expires %s). It contains the room key: share it privately.

  threadsplice join -as <name> '%s'

`, inv.ExpiresAt.Local().Format(time.RFC1123), client.FormatInvite(id.Relay, inv.Token, key))
	return nil
}

func cmdJoin(ctx context.Context, args []string) error {
	fs := flags("join")
	as := fs.String("as", "", "your participant name in the room")
	if err := parse(fs, args); err != nil {
		return err
	}
	if *as == "" || fs.NArg() != 1 {
		return usageError("usage: threadsplice join -as NAME INVITE")
	}
	relayURL, token, key, err := client.ParseInvite(fs.Arg(0))
	if err != nil {
		return err
	}
	if err := client.CheckRelayURL(relayURL); err != nil {
		return err
	}
	if _, err := client.Load(*as); err == nil {
		return fmt.Errorf("identity %q already exists on this machine; pick another -as name", *as)
	}
	m, err := client.New(relayURL, "").Join(ctx, token, *as) // the key stays local
	if err != nil {
		return err
	}
	id := client.Identity{Alias: *as, Relay: relayURL, RoomID: m.RoomID, RoomName: m.RoomName,
		ParticipantID: m.ParticipantID, Name: *as, Token: m.Token, RoomKey: key.String()}
	if err := client.Save(id, false); err != nil {
		return err
	}
	fmt.Printf("Joined room %q as %q.\n", m.RoomName, *as)
	return nil
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
			return nil
		}
	}
	return fmt.Errorf("no participant %q in room %q", fs.Arg(0), a.ID.RoomName)
}

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
	text := strings.Join(fs.Args(), " ")
	if text == "-" {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, 96<<10))
		if err != nil {
			return err
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
