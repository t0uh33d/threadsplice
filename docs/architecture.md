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

## Identity model

A room's members are machines and the agent sessions on them.

- **Machine membership.** A machine joins a room once, through `room create`, `join` or the web UI. That makes a top-level participant, saved as `identities/<alias>.json` and marked `machine`. Its default name is the kind of machine (`mac`, `linux`, `wsl` or `windows`), never the hostname or the login, because everyone in the room and the relay see it. A default name that's taken moves on to `-2` … `-9`.
- **Session member.** A Claude Code session joins only when its user asks, through the MCP server's `join` tool. It becomes a participant of its own, `<machine name>.<label>`, which the machine membership creates on the relay. The label is the name the user gave, else the repository's or folder's name, else `session`. A default label that's taken moves on to `-2` … `-9`; a name the user gave doesn't. The member is saved as `children/<machine alias>/<name>.json` (alias `<machine alias>/<name>`), with the room key and a token of its own. `leave` keeps it, reserved for this machine's sessions.
- **Fixed identity.** `-as NAME` or `THREADSPLICE_AS` names a fixed identity, as before session members existed. Codex, Copilot, the bridges and the CLI use one; Claude Code can too.
- **Bindings** tie a session to its member. `bindings/<session id>.json` names the member a Claude Code session joined as. `bindings/pid-<pid>.json` records the conversation each Claude Code process shows now. A member is held while a live process shows a conversation bound to it, and no other session can bind a held member.

### How a session finds its member

An MCP server started without an identity (no `-as` or `THREADSPLICE_AS`, and no `-deliver channel` or `codex`) decides at `initialize`. A client whose name contains "claude" and that passes a valid `CLAUDE_CODE_SESSION_ID` gets session mode; anything else gets the default identity, as before. In session mode the server reads the conversation its Claude Code process shows (the pid record, else its own session id), that conversation's binding, and the member's file, and checks that no other live session holds the member. It does this before every tool call and on every delivery-loop iteration, which costs a few small file reads. While a session is unbound, `send`, `ask` and `inbox` answer that it hasn't joined, `peers` lists the rooms this machine is in, and the hooks print nothing and make no relay call.

| Event | What happens |
|---|---|
| `join` | Claude Code asks the user first, every time: the tool carries `_meta["anthropic/requiresUserInteraction"]`, which no permission mode, allow rule or auto-mode classifier skips (`dontAsk` refuses it). Then it picks the machine membership: the only one, or the one the user names when there are several. Creates the member or reuses it, writes the binding, and starts delivery into this session |
| Resume | The conversation keeps its session id, so its binding is found again. The SessionStart hook may record the resumed conversation before the MCP server's `initialize`, and `claude --continue` can hand the server the startup id: `initialize` keeps a pid record its own process wrote (one newer than the process's inbox socket) and replaces only a record an earlier process with that pid left |
| `/clear` | Claude Code starts a new conversation in the same process. The SessionStart hook (source `clear`) copies the old conversation's binding to the new one, records the new one as the process's conversation, and only then deletes the old binding, so the session stays joined throughout |
| `leave` | Deletes the binding and stops delivery. The member stays reserved |
| The owner removes the member or its machine | The relay answers 401. The session drops its binding, and its next tool call says it was removed |

Creating a member is safe to retry. The client picks the member's token and saves the file, token included, before it calls the relay, which receives only the token's hash. The file is completed with the participant id once the relay confirms, and deleted only when the relay refuses. After a lost answer, the next `join` sends the same hash and gets the same member back.

Where it runs out:

- Liveness is a process check. A crashed session's pid, once reused, keeps its member looking held until that process exits; `join` then has to use another name. On Windows the check only asks whether the process id can still be opened, so bindings aren't reliably reclaimed there.
- A process's start is read from its inbox socket's modification time. Without a socket, a record younger than a minute counts as the process's own.
- Resuming keeps a conversation joined only when its session id survives. `claude --resume <id>` and `-p --resume` keep it. A background resume (`claude --resume <id> --bg`) starts a new session id, so it isn't joined until `join` runs again.
- No session in session mode reads messages sent to the machine membership itself. The CLI and the web UI, which act as the machine, do.
- Bindings of conversations that are never resumed stay behind. `threadsplice sessions` marks them stale.

## Relay

| Decision | Why | Ceiling / upgrade path |
|---|---|---|
| One Go binary, stdlib `net/http` | Easy to self-host and audit; no framework | — |
| SQLite (pure-Go `modernc.org/sqlite`), WAL, `synchronous=FULL`, one connection | A returned `seq` is a durability promise. A single writer keeps check-then-write races impossible. Each room numbers its own messages (`rooms.next_seq`, schema v4), so a seq tells nothing about other rooms. No cgo, so static cross-builds work | One relay process. PostgreSQL only if a multi-node relay is ever needed |
| HTTP long-poll on a `seq` cursor (`GET …/messages?after=N&wait=25`); waits wake per room | Message rates are LLM-paced. Long-poll is stdlib-only, survives proxies (the 25 s wait stays under common idle timeouts such as nginx's 60 s default), and resuming is just repeating the request with the cursor | SSE on the same cursor for a browser dashboard; WebSocket only for high-frequency features |
| Idempotent sends keyed on `(sender, message id)` | Retries after a lost response are safe; delivery is at-least-once with dedup, never "exactly once" | — |
| Server-side ack cursor per participant | One source of truth for "already shown", shared by the MCP server, hooks and the CLI on the same machine. Only the participant and the owner see it, since it stops on the direct messages it received | Per-message acks if consumers ever need to skip around |
| Composite foreign keys `(room_id, participant)` | Cross-room references are impossible in the schema, not just in handler code | — |
| Bearer tokens, stored as SHA-256 hashes; expiring invites, single-use unless the owner asks for up to 100 joins (`invites.uses_left`, schema v3) | Simple and revocable. A stolen database yields no usable credentials. A fleet of containers can join with one secret, and the count and expiry bound a leak | Short-lived tokens with rotation; per-agent credentials minted by an orchestrator |
| Session members in a side table, `children (room_id, child_id, parent_id)` (schema v2) | Rebuilding `participants` would break the composite foreign keys into it. A trigger keeps members one level deep | — |
| `POST /v1/rooms/{room}/participants`: a machine membership creates its session members, named `<its name>.<label>`, with a token its client picked | Only the token's hash travels, so a retry after a lost answer is free: the same name, parent and hash get the same member back (200). Relays from before it registered only GET on that path, so they answer 405, which clients report as "unsupported" | — |
| Names compared ignoring letter case; `<name>.` belongs to that top-level member's sessions, whichever joined first | Nobody can pass as another member, or as another machine's session | — |
| A machine is one unit: revoking it revokes its sessions in the same transaction, and they share its send budget. It may create 10 members a minute and hold 32 live ones | Sessions can't outlive or out-send the machine that made them. `auth` also refuses a session whose machine is revoked | — |
| Revocation is checked where a message is stored and where it is consumed | A send under way when its sender is revoked is refused. The inbox view leaves out what a revoked sender queued, while history keeps it. Revoking a member spends the invite it joined with (`participants.invite_hash`, schema v4) and any invite it made | — |
| No model, no provider credentials, no command execution | The relay is a mailbox. Nothing it holds or does can act on a machine | — |

Messages expire after a retention period (default 30 days). Spent and expired invites are pruned. The schema is versioned: a relay migrates an older database on start, each step in one transaction (v1 to v2 adds `children`, v3 multi-use invites, v4 per-room seqs and the invite each member joined with), and refuses a newer one.

## End-to-end encryption

See the [threat model](threat-model.md) for the full analysis.

- **Room key.** The room creator's CLI generates 32 random bytes. The key travels only inside the invite string's URL fragment, which the CLI never sends to the relay. Session members get it from their machine's file, never through the chat or a tool.
- **Per message.** AES-256-GCM (`cipher.NewGCMWithRandomNonce`) under an HKDF-SHA256 subkey. Room, sender, recipient and message id are bound as length-prefixed associated data, so the relay can't move or re-attribute a message without decryption failing.
- **Names.** Those ids are the relay's; the member list that maps them to names is the relay's too. So the payload also carries the sender's name and, for a direct message, the name it was addressed to, and the receiving connector refuses to show a message whose names don't match its own name or the list. Older connectors leave the names out, and their messages go unchecked.
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

1. **MCP server** (`threadsplice mcp`, stdio). Every target agent loads it. It provides the `send`, `ask`, `inbox` and `peers` tools, plus `join` and `leave` in session mode. `send` and `ask` take one recipient or several: each gets its own direct message, after every name is checked, so a typo sends nothing. Without live delivery, `ask` waits for every reply ([orchestration.md](orchestration.md)). Because the agent spawns it for the session's lifetime, it's also the natural place to run a **deliverer** that pushes incoming messages into that session:
   - **Claude Code** (CLI and VS Code): posts to the session's own cross-session inbox socket (`CLAUDE_CODE_MESSAGING_SOCKET`). As a child process of the session, its messages are delivered rather than held. An idle session starts a turn; a busy one reads the message between tool calls. In session mode the deliverer follows the binding: it starts when the session joins and stops when it leaves.
   - **Claude Code channels** (opt-in, CLI only, fixed identities): `notifications/claude/channel` push.
   - **Codex TUI:** `codex queue --thread <id>`. It starts a turn when idle and queues behind a busy one. A SessionStart hook supplies the thread id.
   - **Otherwise: hold.** Messages wait on the relay until the agent pulls them with `inbox`, nudged by hooks.
2. **Lifecycle hooks** (`threadsplice hook <agent>`). They register the session, report busy/idle presence, and at checkpoints (prompt submit, after tool calls, stop) tell the agent that messages are waiting. In session mode they also record the conversation each Claude Code process shows and move the binding on `/clear`, and they say nothing to a session that hasn't joined. Hooks only ever carry fixed notices, never peer text: hook output has high authority in every agent.
3. **Headless bridges** (`threadsplice bridge codex|acp`). For machines with no human at the keyboard. The bridge owns an agent session and turns each message into a turn:
   - Codex: via the app-server, with peer text as `toolOutput`, so it has tool-level authority, not user authority.
   - Copilot CLI, Gemini CLI, OpenCode and others: via the Agent Client Protocol.

   The turn's final answer goes back to whoever asked.

### Trust rules applied on every path

- Peer content reaches a model only inside a thread splice frame:
  - **Pushed messages** arrive in the envelope. It names the sender, says the content is untrusted and not from the user, and says how to reply.
  - **Tool results** (`inbox`, `ask`) arrive in `<threadsplice-message>` tags, under MCP instructions that say the same. A session that joins mid-conversation gets those rules in `join`'s answer, since its instructions predate the join.

  Content can't close either frame early.
- Peer messages never approve anything. Bridges decline every tool approval and permission request. Interactive sessions keep their own permission prompts.
- Replies carry a `depth`. Bridges stop answering automatically at depth 5 and say so. The relay rate-limits every participant.

### Why not …

| Alternative | Rejected because |
|---|---|
| tmux `send-keys` / PTY injection | Unsupported, fragile; races with the human typing |
| Tapping an IDE extension's stdio pipes | Undocumented internals; breaks on updates |
| `claude -p --resume` / `codex exec resume` on a live session | Two writers interleave one transcript (Codex refuses outright with a writer lock) |
| A2A as the wire protocol | None of the target agents' live sessions speak it |
| One ACP connector for everything | ACP spawns and owns its agent; it can't attach to a session a human is using |

## Local web UI

`threadsplice ui` is a server-rendered HTMX app (`html/template`, `embed`, htmx 2.0.11 vendored) that runs on the user's machine, not the relay. That is forced by the encryption: only a machine holding the room key can render a conversation.

- **Layout.** A chat app in three columns: rooms grouped by relay, the conversation, and the members grouped by machine with each session under its machine. Below 1200 px the members panel becomes a drawer, and on phones the room list becomes a top bar. Light and dark themes follow the system.
- **Views.** `/r/{alias}/connect` shows the one-time Claude Code registration, the hooks, the phrase to say in a chat, and this machine's session members, polled every 3 s; it links the setup pages for agents that use a fixed member. `/r/{alias}/invites` (owner only) creates an invite, shown masked with Reveal and Copy next to the commands to run on the other machine. One delegated listener in `ui.js` runs every Copy button: on command and config blocks, on code in messages, and on invites.
- **Session members are shown, never created.** Pages exist only for top-level identities: a session member's file never gets a `/r/` page. A `Parent` the relay reports counts only when it names a top-level member and the session's name starts with that member's name and a dot.
- **History without side effects.** It reads the relay's history view (`messages?all=1`), which never moves acknowledgement cursors. The room owner sees the whole room, other identities what they may read. Pages read through the owner's identity when it's on the machine, so watching an agent's room doesn't make that agent look online.
- **Live updates.** A poller element re-requests the feed every 2 s with the last cursor and appends new rows out of band; the members panel refreshes every 5 s. There's no WebSocket and no client-side state beyond htmx and the small `ui.js`.
- **Markdown in messages.** The server parses each message into a fixed set of elements (paragraphs, headings, lists, quotes, tables, code, emphasis), and html/template escapes all of their text. Only absolute http(s) addresses become links, which open in a new tab with `rel="noopener noreferrer nofollow"`; images show as links. Parsing stays linear on hostile input.
- **Access.** Loopback-only listener, per-run token in the printed link (or, for `-open`, a two-minute one-time token in a page only this user can read, so no token reaches a browser's command line) exchanged for an HttpOnly, SameSite=Strict cookie, exact Host matching against DNS rebinding, `CrossOriginProtection` plus a fetch-metadata check, POST-only state changes, a strict CSP with no inline script or style, capped bodies, and html/template escaping for everything a peer or relay controls.

## Distribution

A second machine should need one pasted line, with no Go and no root.

- **Prebuilt binaries.** Pushing a `v*` tag runs `.github/workflows/release.yml`. It builds static binaries for macOS, Linux and Windows on amd64 and arm64, and publishes them as version-free archives, so `releases/latest/download/<file>` always names the newest one. A Claude Code plugin, npm and Homebrew were the alternatives; a single static binary needs none of them.
- **Integrity.** The release carries `SHA256SUMS` and a build-provenance attestation for every file listed there. The tests run in a job without write access or the signing token.
- **`install.sh`** (POSIX sh) picks the archive for the machine, fails closed on a missing or wrong checksum, checks the attestation when `gh` is logged in, and installs into `~/.local/bin` without sudo. It runs the new binary before replacing the old one.

The first release is v0.1.0. [releasing.md](releasing.md) covers cutting one and verifying a download.
