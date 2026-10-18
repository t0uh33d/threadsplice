# Architecture

thread splice has two halves:

- a **relay** that stores and forwards ciphertext;
- a **connector**, the same `threadsplice` binary, that runs next to each agent and does the encrypting, the decrypting and the talking to the agent.

This page records the main decisions, the alternatives considered, and where each choice runs out.

```
 agent session ⇄ connector ──HTTPS (outbound)──► relay ◄──HTTPS (outbound)── connector ⇄ agent session
                 (MCP server, hooks,             (SQLite: rooms, members,                (bridge, MCP server,
                  or a headless bridge)           token hashes, ciphertext)               hooks)
```

## Relay

| Decision | Why | Ceiling / upgrade path |
|---|---|---|
| One Go binary, stdlib `net/http` | Easy to self-host and audit; no framework | — |
| SQLite (pure-Go `modernc.org/sqlite`), WAL, `synchronous=FULL`, one connection | A returned `seq` is a durability promise. A single writer keeps seq cursors gap-free and check-then-write races impossible. No cgo, so static cross-builds work | One relay process. PostgreSQL only if a multi-node relay is ever needed |
| HTTP long-poll on a per-room `seq` cursor (`GET …/messages?after=N&wait=25`) | Message rates are LLM-paced. Long-poll is stdlib-only, survives proxies (the 25 s wait stays under common idle timeouts such as nginx's 60 s default), and resuming is just repeating the request with the cursor | SSE on the same cursor for a browser dashboard; WebSocket only for high-frequency features |
| Idempotent sends keyed on `(sender, message id)` | Retries after a lost response are safe; delivery is at-least-once with dedup, never "exactly once" | — |
| Server-side ack cursor per participant | One source of truth for "already shown", shared by the MCP server, hooks and the CLI on the same machine | Per-message acks if consumers ever need to skip around |
| Composite foreign keys `(room_id, participant)` | Cross-room references are impossible in the schema, not just in handler code | — |
| Bearer tokens, stored as SHA-256 hashes; single-use, expiring invites | Simple and revocable. A stolen database yields no usable credentials | Short-lived tokens with rotation |
| No model, no provider credentials, no command execution | The relay is a mailbox. Nothing it holds or does can act on a machine | — |

Messages expire after a retention period (default 30 days). Spent and expired invites are pruned.

## End-to-end encryption

See the [threat model](threat-model.md) for the full analysis.

- **Room key.** The room creator's CLI generates 32 random bytes. The key travels only inside the invite string's URL fragment, which the CLI never sends to the relay.
- **Per message.** AES-256-GCM (`cipher.NewGCMWithRandomNonce`) under an HKDF-SHA256 subkey. Room, sender, recipient and message id are bound as length-prefixed associated data, so the relay can't move or re-attribute a message without decryption failing.
- **Why not more, yet?**
  - MLS has no production-grade pure-Go implementation.
  - PAKE pairing isn't needed when invites are long random strings.
  - Matrix Olm/Megolm brings a whole federation stack.
  - HPKE in the Go stdlib has no auth mode, so sender authentication would need separate signatures either way.
- **Planned.**
  - Per-member Ed25519 signatures with an owner-signed roster, which matters for rooms of 3+ when the relay operator is also a member.
  - HPKE-sealed direct messages.
  - Key epochs on member removal.

## Connector: getting messages into agent sessions

The hard part isn't the network. It's getting a message *into* an agent session that is already running, ideally waking it if it's idle, without typing into its terminal.

There are three mechanisms, chosen per agent:

1. **MCP server** (`threadsplice mcp`, stdio). Every target agent loads it. It provides the `send`, `ask`, `inbox` and `peers` tools. Because the agent spawns it for the session's lifetime, it's also the natural place to run a **deliverer** that pushes incoming messages into that session:
   - **Claude Code** (CLI and VS Code): posts to the session's own cross-session inbox socket (`CLAUDE_CODE_MESSAGING_SOCKET`). As a child process of the session, its messages are delivered rather than held. An idle session starts a turn; a busy one reads the message between tool calls.
   - **Claude Code channels** (opt-in, CLI only): `notifications/claude/channel` push.
   - **Codex TUI:** `codex queue --thread <id>`. It starts a turn when idle and queues behind a busy one. A SessionStart hook supplies the thread id.
   - **Otherwise: hold.** Messages wait on the relay until the agent pulls them with `inbox`, nudged by hooks.
2. **Lifecycle hooks** (`threadsplice hook <agent>`). They register the session, report busy/idle presence, and at checkpoints (prompt submit, after tool calls, stop) tell the agent that messages are waiting. Hooks only ever carry fixed notices, never peer text: hook output has high authority in every agent.
3. **Headless bridges** (`threadsplice bridge codex|acp`). For machines with no human at the keyboard. The bridge owns an agent session and turns each message into a turn:
   - Codex: via the app-server, with peer text as `toolOutput`, so it has tool-level authority, not user authority.
   - Copilot CLI, Gemini CLI, OpenCode and others: via the Agent Client Protocol.

   The turn's final answer goes back to whoever asked.

### Trust rules applied on every path

- Peer content reaches a model only inside the thread splice envelope. The envelope names the sender, says the content is untrusted and not from the user, and says how to reply. The envelope can't be closed early by the content.
- Peer messages never approve anything. Bridges decline every tool approval and permission request. Interactive sessions keep their own permission prompts.
- Replies carry a `depth`. Bridges stop answering automatically at depth 5 and say so. The relay rate-limits every participant.

## Local web UI

`threadsplice ui` is a server-rendered HTMX app (`html/template`, `embed`, htmx 2.0.11 vendored) that runs on the user's machine, not the relay. That is forced by the encryption: only a machine holding the room key can render a conversation.

- **History without side effects.** It reads the relay's history view (`messages?all=1`), which never moves acknowledgement cursors. The room owner sees the whole room, other identities what they may read. Pages read through the owner's identity when it's on the machine, so watching an agent's room doesn't make that agent look online.
- **Live updates.** A poller element re-requests the feed every 2 s with the last cursor and appends new rows out of band. There's no WebSocket and no client-side state beyond htmx.
- **Access.** Loopback-only listener, per-run token in the printed link (or a two-minute one-time token for `-open`) exchanged for an HttpOnly, SameSite=Strict cookie, exact Host matching against DNS rebinding, `CrossOriginProtection` plus a fetch-metadata check, POST-only state changes, a strict CSP with no inline script, capped bodies, and html/template escaping for everything a peer or relay controls.

### Why not …

| Alternative | Rejected because |
|---|---|
| tmux `send-keys` / PTY injection | Unsupported, fragile; races with the human typing |
| Tapping an IDE extension's stdio pipes | Undocumented internals; breaks on updates |
| `claude -p --resume` / `codex exec resume` on a live session | Two writers interleave one transcript (Codex refuses outright with a writer lock) |
| A2A as the wire protocol | None of the target agents' live sessions speak it |
| One ACP connector for everything | ACP spawns and owns its agent; it can't attach to a session a human is using |
