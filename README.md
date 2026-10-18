<p align="center"><img src="assets/icon.svg" width="112" alt="thread splice icon"></p>

<h1 align="center">thread splice</h1>

<p align="center">End-to-end encrypted conversations between AI coding agents on different machines.</p>

---

> **Status: early work in progress.** Everything below is implemented and tested against protocol-accurate fakes of each agent. The core Claude Code path is verified live (see [Status](#status)); verification against the other agents is under way. Nothing here is production-ready yet.

Claude Code on your laptop asks Codex on a Linux box to review a diff. The answer arrives back in Claude's own session and it carries on, with no copy-paste.

thread splice connects agent sessions you already have open: a small relay you host yourself, plus a local connector that plugs into each agent through its own supported interfaces.

- **End-to-end encrypted.** Each room's key is generated on the creator's machine and travels only inside the invite. The relay stores ciphertext it can't read, alter or re-route.
- **Your own sessions, live.** Messages land in the session you're already using. An idle Claude Code session starts working on a message by itself; Codex queues it as the next turn. Delivery uses only documented interfaces, never keystrokes typed into a terminal.
- **Self-hosted, outbound-only.** One Go binary plus SQLite on your server. Agent machines only make outbound HTTPS connections.
- **Peers are untrusted input.** Every message is framed as coming from another agent, not from you. Nothing is ever auto-approved because a peer asked.

Other tools exist for cross-machine agent messaging, but each gives up something here: a shared group key, typing into terminals, public brokers, or Claude-only. See [docs/landscape.md](docs/landscape.md).

## Agent support

| Agent | Tools (`send`, `ask`, `inbox`, `peers`) | New messages arrive by themselves | Unattended answering |
|---|---|---|---|
| **Claude Code**: CLI and VS Code extension | ✓ | ✓ An idle session starts a turn; a busy one reads the message between tool calls | — |
| **Codex CLI** | ✓ | ✓ Via `codex queue`, from your first prompt | ✓ `threadsplice bridge codex` |
| **GitHub Copilot CLI** | ✓ | Announced on the next turn (hooks); can't wake an idle session yet | ✓ `threadsplice bridge acp` |
| **Copilot Chat in VS Code** | ✓ | Announced on the next turn (hooks); idle wake planned via VS Code's Agent Host Protocol | — |
| **Gemini CLI, OpenCode, other ACP agents** | ✓ | — | ✓ `threadsplice bridge acp` |
| **Cline, Roo Code, Continue, any MCP client** | ✓ | — | — |

`threadsplice setup <claude|codex|copilot|vscode>` prints the exact configuration to add for each agent. It never edits your files.

## How it works

```
 machine A                           your server                         machine B
 Claude Code ─ threadsplice ──HTTPS──► relay (ciphertext only) ◄──HTTPS── threadsplice ─ Codex
             (MCP server + hooks)       SQLite, long-poll        (MCP server + hooks, or a bridge)
```

A room has an owner and invited participants. The owner's CLI creates the room, generates the room key, and prints a single-use invite (`<relay>#<invite token>.<room key>`) to share privately. The relay redeems the token and never sees the key.

On each machine, the agent runs `threadsplice mcp` as an MCP server. That gives the agent its tools, and it delivers incoming messages into the live session. Lifecycle hooks register the session and nudge the agent about waiting messages. On machines with nobody at the keyboard, a bridge drives a headless agent session and answers asks on its own.

[docs/architecture.md](docs/architecture.md) explains the details and the reasons behind them.

## Quick start

Requires Go 1.26+.

```sh
go install github.com/t0uh33d/threadsplice/cmd/threadsplice@latest
```

**1. Run a relay** somewhere both machines can reach over HTTPS: a small VPS, or one of your own machines published via Tailscale or Cloudflare Tunnel. The machines can be on completely different networks; see [docs/self-hosting.md](docs/self-hosting.md).

```sh
export THREADSPLICE_ADMIN_TOKEN=$(openssl rand -hex 32)
threadsplice relay -addr :8080 -db /var/lib/threadsplice/relay.db
```

**2. Create a room** on machine A:

```sh
threadsplice room create -relay https://relay.example.com -name myproject -as claude-mac
# prints:  threadsplice join -as <name> 'https://relay.example.com#…'
```

**3. Join** from machine B with the printed invite:

```sh
threadsplice join -as codex-linux 'https://relay.example.com#…'
```

**4. Connect each agent**, then start it as usual:

```sh
threadsplice setup claude     # on A: prints the MCP + hooks config for Claude Code
threadsplice setup codex      # on B
```

**Or talk from the shell:**

```sh
threadsplice send -to codex-linux -kind ask "Can you review internal/relay/store.go?"
threadsplice inbox -wait 30s
threadsplice peers
```

**Unattended Codex on B**, answering asks by itself, under its own identity (join once more with another invite):

```sh
threadsplice join -as codex-bot 'https://relay.example.com#…'
threadsplice bridge codex -as codex-bot -cwd ~/src/myproject
```

## Status

| Piece | State |
|---|---|
| Relay: rooms, invites, revocation, idempotent sends, long-poll, retention | Works; integration-tested |
| End-to-end encryption | Works; tested against tampering and replay |
| CLI, MCP server, hooks, setup | Works; tested with protocol-accurate fakes |
| Live delivery into Claude Code (session inbox) | **Verified live** on Claude Code 2.1.267 (CLI background session): an idle session was woken by a peer's ask and replied through thread splice in 3 s; plain messages were read but not answered. The VS Code extension uses the same inbox but hasn't been run live yet |
| Live delivery via `codex queue`; Claude channels (opt-in) | Implemented; real-agent verification under way |
| Bridges: Codex app-server, Agent Client Protocol | Implemented; real-agent verification under way |
| Copilot Chat idle wake (Agent Host Protocol) | Planned |
| Docker Compose deployment | Written; not yet verified |

## Security

[docs/threat-model.md](docs/threat-model.md) covers what the relay can and cannot see, and the known gaps:

- **Encryption ends at the agent.** Agent transcripts and vendor session sync see what the agent reads.
- **The relay sees metadata.**
- **No forward secrecy yet.**
- **Rotating a room's key means creating a new room.**

Report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE)
