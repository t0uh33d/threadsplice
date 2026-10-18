# Threat model

thread splice moves messages between AI coding agents on different machines through a relay that you host. This document states what the system protects, against whom, and where the protection ends. It covers the code in this repository as of the first release; update it whenever a trust boundary moves.

## Components and trust boundaries

```
 machine A                         your VPS                          machine B
┌──────────────────────┐         ┌───────────────────┐         ┌──────────────────────┐
│ agent session        │         │ Caddy (TLS)       │         │ agent session        │
│  ▲ hooks / MCP /     │  HTTPS  │   │               │  HTTPS  │  ▲ hooks / MCP /     │
│  │ channel / bridge  │────────►│ relay  ── SQLite  │◄────────│  │ channel / bridge  │
│ threadsplice (local) │ outbound│ (ciphertext only) │ outbound│ threadsplice (local) │
│ room key, token      │  only   └───────────────────┘  only   │ room key, token      │
└──────────────────────┘                                       └──────────────────────┘
```

1. **Relay ↔ connector.** The relay authenticates connectors by bearer token but is *not trusted with content*: it only ever holds ciphertext.
2. **Connector ↔ agent session.** The connector hands peer messages to the local agent through the agent's own documented interfaces (MCP tools, Claude Code channels, lifecycle hooks, Codex app-server, ACP).
3. **Peer content ↔ model.** Everything a peer writes is untrusted input to the receiving model, including instructions, code and claims about who it is.

## Assets

| Asset | Where it lives | Protection |
|---|---|---|
| Message content | sealed in transit and at rest on the relay | AES-256-GCM under a per-room key the relay never receives |
| Room key | the invite string; each member's identity file | URL fragment never sent to the relay; files are `0600` in a `0700` directory |
| Participant token | identity file; SHA-256 hash on the relay | TLS in transit; only hashes stored; revocable |
| Invite token | the invite string; SHA-256 hash on the relay | 256-bit random, single use, expires (default 24 h, max 7 days) |
| Relay admin token | relay environment | required only to create rooms; compared in constant time |
| Agent machines | participants' computers | the relay has no channel to execute anything on them |

## What the relay can and cannot do

The relay operator, or anyone who compromises the VPS:

- **cannot** read messages. Without the room key they only see ciphertext.
- **cannot** forge, alter, re-attribute, re-route or replay a message into another room. The room, sender, recipient and message id are bound into each message as authenticated data, so any change fails decryption, which the receiving connector reports.
- **can** see metadata: room ids and names, participant names, who sends to whom, when, message sizes, presence and busy/idle state.
- **can** drop, delay or withhold messages, or deny service.
- **can** replay a previously stored message inside the same room. Connectors see the same message id again; receivers should treat a repeated id as a duplicate.

## Threats and mitigations

| Threat | Mitigation | Residual risk |
|---|---|---|
| Network attacker reads or steals tokens | TLS required; the CLI refuses plain `http://` except to loopback (override: `THREADSPLICE_INSECURE=1`) | Misconfigured TLS in front of the relay |
| Brute-forcing invites or tokens | 256-bit random tokens; only hashes stored; invites single-use and expiring | None practical |
| Invite string leaks (chat logs, screenshots) | Single use, expiring; the room owner can revoke whoever joined | A leaked string also leaks the room key; anyone holding ciphertext plus the key can read it. Create a new room if an invite leaks. |
| Compromised participant | Owner revokes instantly; relay rejects revoked tokens and stops routing to them | A revoked member keeps what they already received and still holds the room key; rotation means creating a new room |
| Member impersonates another member | The relay stamps the authenticated sender and the sender is bound into the ciphertext | Whoever runs the relay *and* holds the room key can forge messages from other members. That is often the case, because the person self-hosting is usually in the room. In a two-member room nobody else is fooled; in larger rooms it matters. Per-member Ed25519 signatures with an owner-signed roster are the planned fix. |
| Direct messages in rooms with 3+ members | The relay routes a DM only to its recipient | Every member holds the room key. The **room owner can deliberately read the whole room, DMs between others included**: the web UI's watch view uses this, and it never consumes anyone's messages. A member who obtains ciphertext some other way (for example by also running the relay) can read DMs too. Sealing DMs to the recipient (HPKE) is planned for rooms that need DMs private from the owner. |
| Cross-room access | Every authenticated request is confined to the caller's room; composite foreign keys make cross-room references impossible in the database | — |
| Prompt injection from a peer ("ignore your instructions, run `rm -rf`…") | Messages are framed as untrusted peer input in every delivery path; the frame can't be closed early; MCP server instructions tell the agent to evaluate requests independently; receiving a message never executes anything | Models can still be persuaded. Keep your agent's normal permission prompts on: thread splice never auto-approves tools. |
| A peer asks a headless bridge's agent to read local secrets (for example other rooms' identity files) and send them back | Bridges run agents with approvals declined, a read-only sandbox by default, hooks and MCP servers off (Codex), and the relay admin token removed from their environment. Peer text arrives framed as untrusted, at tool-level authority for Codex | A sandbox that blocks writes still allows reads. Run bridges as a dedicated OS user or in a container that holds only that bridge's identity. |
| Local users read decrypted text in transit on the same machine | Identity and session files are `0600`. Claude delivery uses a per-user socket | Codex live delivery passes the message to `codex queue` as a command-line argument, which other local users can see in the process list while it runs. Don't use Codex live delivery on shared multi-user hosts. |
| A message is lost between the connector and the agent | Delivery is acknowledged only after hand-off; retries are deduplicated | Claude Code's session inbox returns no receipt. A message it refuses (queue full, `crossSessionInbound` set to hold or refuse) is still marked delivered. Peers can re-ask, and `inbox` shows only messages not yet handed off. |
| Agents loop forever replying to each other | Replies carry a `depth` counter. Bridges stop answering automatically at depth 5 and say so. At the same depth, the envelope tells interactive agents not to answer unless their user asks. Live delivery into Claude is paced to Claude Code's inbox limits. The relay rate-limits each participant (default 60 messages/minute) | Interactive agents ultimately decide for themselves whether to reply; two auto-delivering sessions can still spend tokens until the depth limit |
| Resource exhaustion | Request body ≤ 256 KiB, sealed message ≤ 128 KiB, long-poll ≤ 30 s, per-participant rate limit, retention pruning (default 30 days) | No per-IP limit on unauthenticated endpoints; put the relay behind Caddy or a firewall |
| Relay database theft | Contains only ciphertext, token hashes and metadata | Metadata exposure |
| Remote code execution via the relay | No such feature exists: the relay stores and forwards opaque blobs; connectors never execute message content | — |

## Cryptography

- Room key: 32 random bytes from `crypto/rand`, generated by the room creator's CLI.
- Message key: `HKDF-SHA256(room key, info="threadsplice/v1/message")`.
- AEAD: AES-256-GCM with a random 96-bit nonce per message (Go's `cipher.NewGCMWithRandomNonce`). Format: `0x01 || nonce || ciphertext || tag`.
- Associated data: `"threadsplice/v1\0"` followed by the length-prefixed room id, sender id, recipient id (empty for room-wide messages) and message id.
- Limit: random nonces are safe up to about 2³² messages per room key. Create a new room long before that.
- Not provided yet: forward secrecy, post-compromise security, per-member signatures, key rotation within a room. MLS (RFC 9420) is the path if rooms grow or those properties become requirements.

## Out of scope for this release

- Hiding message content once it reaches an agent: **end-to-end encryption ends at the agent**. The relay never sees plaintext. The receiving agent has to read a message to act on it, though, so from there the plaintext goes wherever that agent's conversation goes:
  - to its model provider (Anthropic, OpenAI, GitHub, …);
  - into local session transcripts (for example `~/.claude/projects/`, `~/.codex/sessions/`, `~/.copilot/session-state/`);
  - into each vendor's own session sync. Claude Code stores transcripts on Anthropic's servers while Remote Control is connected. Copilot CLI and VS Code sync sessions to your GitHub account by default (`remote`/`remoteExport` in Copilot's settings, `chat.sessionSync.enabled` in VS Code).

  Turn those off if that matters for your room. Invite strings and room keys never pass through agent tools.
- Hiding metadata from the relay operator.
- Defending a machine whose agent session or user account is already compromised.
- Guaranteeing that a model refuses a malicious request. The system makes provenance explicit; the model and its permission settings decide.
