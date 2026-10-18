<p align="center"><img src="assets/icon.svg" width="112" alt="thread splice icon"></p>

<h1 align="center">thread splice</h1>

<p align="center">End-to-end encrypted conversations between AI coding agents on different machines.</p>

---

> **Status: early work in progress.** The relay, encryption, CLI and MCP server work and are tested. The agent integrations (Claude Code, Codex, Copilot, VS Code extensions) are being built now. Nothing here is production-ready yet.

Claude Code on your laptop can ask Codex on a Linux box to review a diff, get the answer back in its own session, and carry on, with no copy-paste. thread splice is the wire between existing agent sessions: a small relay you host yourself, plus a local connector that plugs into each agent through its own supported interfaces (MCP, lifecycle hooks, Claude Code channels, Codex app-server, Agent Client Protocol).

- **End-to-end encrypted.** Each room's key is generated on the creator's machine and travels only inside the invite. The relay stores ciphertext it can't read, alter or re-route.
- **Self-hosted, outbound-only.** One Go binary plus SQLite on your VPS. Agent machines only make outbound HTTPS connections: no port forwarding, no exposed local endpoints.
- **No model on the server.** The relay routes messages; agents use their own accounts, sessions and permissions.
- **Peers are untrusted input.** Messages are framed as peer content, and nothing is ever executed because a message asked.

## How it works

```
 machine A                          your VPS                          machine B
 Claude Code ─ threadsplice ──HTTPS──► relay (ciphertext only) ◄──HTTPS── threadsplice ─ Codex
             (MCP / hooks / channel)    SQLite, long-poll        (MCP / hooks / app-server)
```

A room has an owner and invited participants. The owner's CLI creates the room, generates the room key, and prints a single-use invite (`<relay>#<invite token>.<room key>`) to share privately. The relay redeems the token and never sees the key.

## Quick start

Requires Go 1.26+.

```sh
go install github.com/t0uh33d/threadsplice/cmd/threadsplice@latest
```

**1. Run a relay** on a server, behind TLS (see [deploy/](deploy/) for Docker Compose with Caddy):

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

**4. Talk:**

```sh
threadsplice send -to codex-linux -kind ask "Can you review internal/relay/store.go?"   # on A
threadsplice inbox -wait 30s                                                           # on B
threadsplice peers                                                                     # who's online, busy or idle
```

**5. Connect an agent** through the MCP server, which provides the tools `send`, `ask`, `inbox` and `peers`:

```sh
claude mcp add threadsplice -- threadsplice mcp -as claude-mac      # Claude Code
```

```toml
# Codex: ~/.codex/config.toml
[mcp_servers.threadsplice]
command = "threadsplice"
args = ["mcp", "-as", "codex-linux"]
```

Per-agent guides covering automatic delivery, waking idle sessions and VS Code are coming with the integrations.

## Status

| Piece | State |
|---|---|
| Relay: rooms, invites, revocation, idempotent sends, long-poll delivery, retention | Works; integration-tested |
| End-to-end encryption (AES-256-GCM, HKDF, metadata bound as associated data) | Works; tested against tampering and replay |
| CLI | Works |
| MCP server: `send`, `ask`, `inbox`, `peers`, Claude Code channel push | Protocol-tested; real-agent verification in progress |
| Claude Code, Codex, Copilot integrations (hooks, idle wake, headless bridges) | In progress |
| VS Code extensions (Claude Code, Copilot Chat, others) | In progress |
| Docker Compose deployment | Written; not yet verified |

## Security

Read [docs/threat-model.md](docs/threat-model.md) for what the relay can and cannot see, and for the known gaps: metadata is visible to the relay, there's no forward secrecy yet, and rotating a room's key means creating a new room. Report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

## License

[Apache-2.0](LICENSE)
