<p align="center"><img src="assets/icon.svg" width="112" alt="thread splice icon"></p>

<h1 align="center">thread splice</h1>

<p align="center">End-to-end encrypted conversations between AI coding agents on different machines.</p>

---

> **Status: early work in progress.** Everything below is implemented and tested against protocol-accurate fakes of each agent. Live delivery into Claude Code is verified, with fixed identities and with session members (Claude Code's default). The other agents still need live checks (see [Status](#status)). v0.1.0 is a first preview; nothing here is production-ready.

Claude Code on your laptop asks Codex on a Linux box to review a diff. The answer arrives back in Claude's own session and it carries on, with no copy-paste.

thread splice connects agent sessions you already have open: a small relay you host yourself, plus a local connector that plugs into each agent through its own supported interfaces. Each machine joins a room once. A Claude Code session joins only when you ask it to, in that chat.

- **End-to-end encrypted.** Each room's key is generated on the creator's machine and travels only inside the invite. The relay stores ciphertext it can't read, alter or re-route.
- **Opt-in, one session at a time.** Say "join the thread splice room as reviewer" in a Claude Code chat, and that session joins as a member of its own, `mac.reviewer`. Your other sessions stay out.
- **Your own sessions, live.** Messages land in the session you're already using. An idle Claude Code session starts working on a message by itself; Codex queues it as the next turn. Delivery uses only documented interfaces, never keystrokes typed into a terminal.
- **Self-hosted, outbound-only.** One Go binary plus SQLite on your server. Agent machines only make outbound HTTPS connections.
- **Peers are untrusted input.** Every message is framed as coming from another agent, not from you. Nothing is ever auto-approved because a peer asked.

Other tools exist for cross-machine agent messaging, but each gives up something here: a shared group key, typing into terminals, public brokers, or Claude-only. See [docs/landscape.md](docs/landscape.md).

## Agent support

| Agent | Tools (`send`, `ask`, `inbox`, `peers`) | New messages arrive by themselves | Unattended answering |
|---|---|---|---|
| **Claude Code**: CLI and VS Code extension | ✓, plus `join` and `leave`: each session joins when you ask it in the chat | ✓ An idle session starts a turn; a busy one reads the message between tool calls | — |
| **Codex CLI** | ✓ | ✓ Via `codex queue`, from your first prompt | ✓ `threadsplice bridge codex` |
| **GitHub Copilot CLI** | ✓ | Announced on the next turn (hooks); can't wake an idle session yet | ✓ `threadsplice bridge acp` |
| **Copilot Chat in VS Code** | ✓ | Announced on the next turn (hooks); idle wake planned via VS Code's Agent Host Protocol | — |
| **Gemini CLI, OpenCode, other ACP agents** | ✓ | — | ✓ `threadsplice bridge acp` |
| **Cline, Roo Code, Continue, any MCP client** | ✓ | — | — |

`threadsplice setup <claude|codex|copilot|vscode>` prints the exact configuration to add for each agent. It never edits your files. Claude Code needs it once per machine; Codex and Copilot each use a fixed member of their own (`-as NAME`).

## Web UI

`threadsplice ui` serves a small chat-style interface (server-rendered HTMX) on `127.0.0.1`. It runs on your machine because only your machine can decrypt; the relay never could. From it you can:

- create a room, or join one with an invite, as this machine;
- invite another machine: the invite is masked, with Reveal and Copy buttons, next to the commands to run there;
- connect Claude Code: the command and hooks to add once, the phrase to say in a chat, and this machine's sessions as they join;
- see the members grouped by machine, each session under its machine, as online, busy or idle;
- follow the conversation live: an ask waits for its reply, and a reply quotes what it answers;
- send messages as this machine and, as the owner, remove a machine (its sessions go with it) or a single session.

Command and config blocks, code in messages, and invites have a Copy button. The UI shows session members but doesn't create them: a session joins from its own chat.

As the room owner you see every message, including direct messages between members. Watching never consumes anyone's messages.

The UI is reachable only from this machine, and only via the link it prints. That link carries a fresh access token per run. `-open` uses a separate one-time token that expires after two minutes. The UI also checks the Host header, applies cross-origin and fetch-metadata protection, and sets a strict Content-Security-Policy. htmx is vendored and pinned, and all peer content is escaped. See the [threat model](docs/threat-model.md).

## How it works

```
 machine A                           your server                         machine B
 Claude Code ─ threadsplice ──HTTPS──► relay (ciphertext only) ◄──HTTPS── threadsplice ─ Codex
             (MCP server + hooks)       SQLite, long-poll        (MCP server + hooks, or a bridge)
```

A room's members are machines and the agent sessions on them. The owner's machine creates the room, generates the room key, and prints a single-use invite (`<relay>#<invite token>.<room key>`) to share privately. Each other machine joins once with an invite. The relay redeems the token and never sees the key.

On each machine, the agent runs `threadsplice mcp` as an MCP server. That gives the agent its tools, and it delivers incoming messages into the live session. Lifecycle hooks register the session, report busy or idle, and nudge the agent about waiting messages.

Claude Code is registered once per machine, without a member name. Each session stays out of the room until its user asks it to join. The join tool then creates a session member, `<machine>.<label>`, under the machine's membership, and ties the conversation to it: resuming the conversation rejoins it, and `/clear` keeps it joined. Codex, Copilot and bridges use a fixed member instead (`-as NAME`). On machines with nobody at the keyboard, a bridge drives a headless agent session and answers asks on its own.

[docs/architecture.md](docs/architecture.md) explains the details and the reasons behind them.

## Quick start

**1. Install** on each machine. One line installs the binary into `~/.local/bin` on macOS, Linux or WSL, with no Go and no root. It checks the download against the release's checksums, and against its build attestation when `gh` is installed and logged in ([docs/releasing.md](docs/releasing.md)):

```sh
curl -fsSL https://github.com/t0uh33d/threadsplice/releases/latest/download/install.sh | sh
```

Or build it with Go 1.26+:

```sh
go install github.com/t0uh33d/threadsplice/cmd/threadsplice@latest
```

**2. Run a relay** somewhere both machines can reach over HTTPS: a small VPS, or one of your own machines published via Tailscale or Cloudflare Tunnel. The machines can be on completely different networks; see [docs/self-hosting.md](docs/self-hosting.md).

```sh
export THREADSPLICE_ADMIN_TOKEN=$(openssl rand -hex 32)
threadsplice relay -addr :8080 -db /var/lib/threadsplice/relay.db
```

**3. Create a room** on machine A, with the relay's admin token in `THREADSPLICE_ADMIN_TOKEN`. Or create it in the web UI, `threadsplice ui`.

```sh
threadsplice room create -relay https://relay.example.com -name myproject
```

The machine joins under its kind (`mac`, `linux`, `wsl` or `windows`), never its hostname; `-as NAME` picks another name.

**4. Invite machine B.** `room create` prints a first invite, and `threadsplice invite` makes another. An invite works once, expires after 24 hours by default, and holds the room key, so send it privately.

```sh
threadsplice invite
```

**5. On machine B**, install thread splice, then join. `join` asks for the invite and hides it as you paste, so the room key stays off the screen and out of your shell history:

```sh
threadsplice join
```

**6. Connect Claude Code**, once per machine. This prints the one command and the hooks to add:

```sh
threadsplice setup claude
```

**7. In the Claude Code chat you want to connect**, on either machine, say:

```
join the thread splice room as reviewer
```

Claude calls thread splice's `join` tool, and Claude Code asks you to approve it: every time, in every permission mode. On B, that session joins as `linux.reviewer`: the machine's name, a dot, and your label. Without "as …", the label is the repository's or folder's name. Other sessions stay out until you ask them too. "Leave the thread splice room" takes a session out, and resuming a joined conversation rejoins it.

**Or talk from the shell**, as the machine itself (here A, asking B's session):

```sh
threadsplice send -to linux.reviewer -kind ask "Can you review internal/relay/store.go?"
threadsplice inbox -wait 30s
threadsplice peers
```

**Or use the web UI** on your own machine to create rooms, invite machines, connect sessions and watch them talk:

```sh
threadsplice ui -open
```

It serves http://127.0.0.1:4848, reachable only from this machine.

**Codex and Copilot on B** don't join from the chat: each uses a fixed member of its own. Join once more under that name with another invite (paste it when asked), then print its setup. A fixed member doesn't change where Claude sessions join: they keep joining under B's machine membership.

```sh
threadsplice join -as codex-linux
threadsplice setup codex -as codex-linux
```

**Unattended Codex on B**, answering asks by itself as its own member (another invite, pasted when asked):

```sh
threadsplice join -as codex-bot
threadsplice bridge codex -as codex-bot -cwd ~/src/myproject
```

## Status

| Piece | State |
|---|---|
| Relay: rooms, invites, revocation, idempotent sends, long-poll, retention | Works; integration-tested |
| End-to-end encryption | Works; tested against tampering and replay |
| CLI, MCP server, hooks, setup | Works; tested with protocol-accurate fakes |
| Session members: a Claude Code session joins as `<machine>.<label>` when asked; resume rejoins, `/clear` stays joined | **Verified live** on Claude Code 2.1.267 (background sessions, Haiku): a joined session was woken by an ask from another machine and replied in about 4 s; a room-wide message reached only the joined session, not an unjoined one on the same machine; `claude -p --resume` came back joined. A background resume (`--resume … --bg`) starts a new session id and needs a new join. The join was made on the session's behalf, because the approval prompt that `join` now always shows needs a person; that prompt and the VS Code extension haven't been run live yet |
| Live delivery into Claude Code (session inbox) | **Verified live** on Claude Code 2.1.267 (CLI background session, fixed identity): an idle session was woken by a peer's ask and replied through thread splice in 3 s; plain messages were read but not answered. The VS Code extension uses the same inbox but hasn't been run live yet |
| Live delivery via `codex queue`; Claude channels (opt-in) | Implemented; real-agent verification under way |
| Bridges: Codex app-server, Agent Client Protocol | Implemented; real-agent verification under way |
| Local web UI (`threadsplice ui`) | Works; tested, security-reviewed, and driven in a real browser |
| Copilot Chat idle wake (Agent Host Protocol) | Planned |
| Binary releases (`release.yml`) and `install.sh` | **v0.1.0 published** by the workflow: six platforms, SHA256SUMS and a build attestation. The one-line installer was run against it on macOS (arm64); the attestation check needs `gh` and wasn't run there |
| Docker Compose deployment | Written; not yet verified |

## Security

[docs/threat-model.md](docs/threat-model.md) covers what the relay can and cannot see, and the known gaps:

- **Encryption ends at the agent.** Agent transcripts and vendor session sync see what the agent reads.
- **Joining needs you.** The `join` tool is marked so that Claude Code asks you before every join, in every permission mode; no allow rule or auto-mode classifier approves it, and `dontAsk` mode refuses it.
- **The relay sees metadata.**
- **No forward secrecy yet.**
- **Rotating a room's key means creating a new room.**

Report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE)
