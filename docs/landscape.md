# Landscape

What already exists for letting AI coding agents talk to each other, and where thread splice fits. Surveyed 2026-10-18 by reading the projects' source, not just their READMEs; star counts and versions are from that date.

## Summary

Cross-machine agent messaging does exist, in two forms:

- **Claude Code's own cross-session messaging.** `SendMessage`/`ListAgents` reach your other Claude sessions, on other machines too via Remote Control. It is Claude-only, limited to your own claude.ai account, and routed through Anthropic's servers without end-to-end encryption.
- **Young third-party tools** (hcom, a2acast, agent-talk, Agent Relay, Paseo, goose roam and others). Each gives up at least two of the properties below.

thread splice aims to be the one that has all five:

1. attaches to sessions you already have open, rather than launching and wrapping its own;
2. works across Claude Code, Codex and Copilot;
3. delivers only through each agent's supported interfaces, never by typing into a terminal (PTY/tmux injection);
4. encrypts end to end with vetted primitives, so the relay never sees content;
5. needs only outbound connections to a relay you host yourself.

## Closest prior art

| Project | Transport | How messages enter a session | Existing sessions | E2EE | Wakes idle | Gives up |
|---|---|---|---|---|---|---|
| [hcom](https://github.com/aannoo/hcom) (MIT, Rust, 579★) | MQTT; public brokers by default | Hooks mid-turn. When idle, a PTY wrapper types the message in | Only if launched via `hcom <tool>` | One shared key for all devices | Yes | Typing into terminals; shared key; public brokers |
| [a2acast](https://github.com/husker/a2acast) (MIT, Python, 8★) | ntfy topics | Claude `asyncRewake` hooks; Codex Stop hook plus a supervisor; Copilot via MCP sampling | Yes | Shared mesh key; home-built cipher | Mostly | Crypto quality; single maintainer |
| [agent-talk](https://github.com/xhluca/agent-talk) (MIT, 194★) | retalk relay | Claude plugin monitors; Codex hooks plus an app-server nudge | Yes | Per-peer keys (not audited) | Claude, Codex | Copilot is pull-only; young |
| Claude Code cross-session messaging (proprietary) | Anthropic servers | Native; an idle session starts a turn | Yes | No | Yes | Claude-only, same account, not self-hostable |
| [Agent Relay](https://github.com/AgentWorkforce/relay) (Apache-2.0, 887★) | WebSocket relay | Pastes into terminals; wraps the agents | No | No (relay sees plaintext) | Yes | Wraps sessions; terminal pasting; plaintext |
| [Paseo](https://github.com/getpaseo/paseo) (Apache-2.0, 21.3k★) | NaCl-box E2EE relay | Its daemon drives agents (Agent SDK, app-server, ACP) | No, only agents it launched | Yes | n/a | An orchestrator, not a bridge between your sessions |
| [goose roam](https://github.com/aaif-goose/goose) (Apache-2.0) | iroh QUIC | ACP; a fresh agent per connection | No | Yes (QUIC-TLS, Ed25519 allowlist) | n/a | goose only; experimental |

Local-only tools on one machine: [claude-peers-mcp](https://github.com/louislva/claude-peers-mcp), [agents-connector](https://github.com/Aldenysq/agents-connector) (wakes agents with tmux `send-keys`), [cross-agent_mcp](https://github.com/whooperlove/cross-agent_mcp) (taps extension stdio pipes; Copilot via VS Code's Agent Host Protocol), Claude Code agent teams, claude-squad, ccmanager, coder/agentapi.

[MCP Agent Mail](https://github.com/Dicklesworthstone/mcp_agent_mail) has a rich mailbox model, but its license adds a rider that excludes OpenAI, Anthropic and their agents. It is not OSI-approved and its code can't be reused here.

## Protocols considered

| Protocol | Used? | Why |
|---|---|---|
| MCP (stdio) | Yes | Every target agent loads MCP servers. Gives the `send`, `ask`, `inbox` and `peers` tools, and the per-session process that delivers messages. |
| Claude Code channels (`notifications/claude/channel`) | Opt-in | The official push contract, but a research preview: CLI-only for custom servers, behind flags and org policy. |
| Claude Code cross-session inbox (`CLAUDE_CODE_MESSAGING_SOCKET`) | Yes | Documented per-session socket. Our MCP server is a child of the session, so its messages are delivered, not held. Works in the VS Code extension. |
| Codex app-server | Yes | The supported programmatic interface. Peer messages go in as `toolOutput`, which has tool-level authority, below the user's. |
| `codex queue` | Yes | Stable CLI for queueing a message into a live Codex session. |
| Agent Client Protocol (ACP) | Yes, headless | Drives Copilot CLI, Gemini CLI, OpenCode, and Claude or Codex through adapters, in sessions the bridge owns. It can't attach to a session a human is typing in. |
| Agent Host Protocol (AHP) | Later | Microsoft's open protocol for VS Code agent sessions; the likely path into live VS Code chat. Discovery of the in-editor host isn't documented yet. |
| A2A | Not now | Of our targets only Gemini CLI speaks it, and v1.0 renamed the core methods. It may become an optional façade later. |

## What we took, and what we didn't

Concepts adopted, as independent implementations; no code was copied:

- **From Claude Code's own messaging:** a peer message is never consent; delivery between tool calls when busy and as a new turn when idle; loop and burst limits.
- **From cross-agent_mcp:** an envelope with provenance, reply-to and a hop budget.
- **From agents-connector:** the ask/reply schema; busy/idle presence driven by hooks.
- **From the Codex SDK's `ExternalMessage`:** remote content enters at tool-level authority, not user authority.

Approaches rejected:

- typing into terminals (PTY, tmux, bracketed paste);
- tapping an IDE extension's stdio pipes;
- resuming a live session headlessly;
- reading host-private databases or registries;
- shared group keys and home-built ciphers;
- public message brokers.
