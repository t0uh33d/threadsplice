# thread splice

End-to-end encrypted messaging between AI coding-agent sessions (Claude Code, Codex, Copilot, ACP agents) on different machines. One Go binary is both the **relay** (stores and forwards ciphertext) and the **connector** (CLI, MCP server, hooks, headless bridges, local web UI). Read [docs/architecture.md](docs/architecture.md) and [docs/threat-model.md](docs/threat-model.md) before you move a trust boundary. [CONTRIBUTING.md](CONTRIBUTING.md) has the ground rules.

## Commands

```sh
go build ./cmd/threadsplice
go vet ./...
go test -race -count=1 ./...
"$(go env GOROOT)/bin/gofmt" -l .   # must print nothing; a plain `gofmt` may come from an older local Go
```

CI runs vet, the race tests and `go build -trimpath` on Linux, macOS and Windows. gofmt runs on Linux only, and `govulncheck` is a separate Linux job. The `install` job runs `install.sh` against a local copy of a release, on Linux and macOS. Job logs need admin rights, so when a test fails, the test job also copies the failing lines into the run's annotations (`GET /repos/t0uh33d/threadsplice/check-runs/<job id>/annotations`). That step pipes `go test` through `tee`, so keep its explicit `shell: bash`, which is what gives the pipe `pipefail`.

## Layout

| Path | Role |
|---|---|
| `cmd/threadsplice/` | CLI. `main.go` holds the core subcommands, `sessions` included; `tty.go` reads an invite at a hidden prompt. `mcp` (in `agents.go`), `setup`, `hook`, `ui` and `bridge` (`bridge.go`, which dispatches to `bridge_codex.go` and `bridge_acp.go`) register into `extra` from their own files |
| `internal/api` | Wire types and `ValidName` |
| `internal/e2ee` | Room key, seal/open |
| `internal/relay` | HTTP API and SQLite store (schema v3) |
| `internal/client` | Relay client; identity and session files; session members (`children.go`), bindings (`bindings.go`), the memberships a session can join under (`machines.go`), `DefaultMachineName` (`machinename.go`); `Agent` (seal/open, replay dedupe); the `Envelope` and `Format` frames |
| `internal/mcp` | stdio MCP server; session mode and the `join`/`leave` tools (`session.go`); live deliverers: Claude inbox socket, `codex queue`, channel push, or hold |
| `internal/hooks` | Lifecycle hooks (session mode in `session.go`), and the `setup` text for each agent |
| `internal/bridge` | Headless bridges: `codex` (app-server) and `acp` |
| `internal/ui` | Local HTMX web UI: home, room, and the Connect (`connect.html`) and Invite (`invites.html`) views |
| `.github/workflows/release.yml`, `install.sh` | Binary releases on a `v*` tag, and the installer ([docs/releasing.md](docs/releasing.md)) |
| `deploy/`, `Dockerfile` | Caddy and relay via Docker Compose. Not verified end to end |

## Invariants

- **The relay never sees plaintext or room keys.** The key lives only in the invite's URL fragment and in identity and session-member files (`0600` in a `0700` directory). `client.Save` never overwrites, because it creates the file with `os.Link`.
- **Peer text reaches a model only inside an untrusted-input frame.**
  - Pushed into a session (Claude inbox, `codex queue`, bridge turns): `client.Envelope`. Every envelope starts with `client.EnvelopePrefix`, which Codex's UserPromptSubmit hook relies on.
  - In `inbox`/`ask` results and in the CLI: `client.Format`'s `<threadsplice-message>` tags.
  - In channel mode: channel events.

  Peer text never goes into hook output, MCP `instructions` or any other high-authority channel. `Format` escapes `</threadsplice-message` in any letter case and spacing (`frameClose`); escape the closing tag of any new frame you add the same way.
- **Hooks print fixed notices, counts, and `api.ValidName`-checked, quoted names.** Never message text. `threadsplice hook` must exit 0 with empty stdout on every failure, because exit 2 (also a Go panic's exit code) means "block" to Claude Code and Codex.
- **`mcp` and `hook` own stdout** (JSON-RPC, and hook output the agent reads with high authority). Log to stderr, and never hand `os.Stdout` to a child process.
- **Validate what the relay supplies before a model or a shell sees it.**
  - Names: `api.ValidName`.
  - States and `reply_to` ids: `peerState` and `messageID` in `client/agent.go`.
  - Invite tokens: `inviteToken` in `cmd/threadsplice/main.go`.
  - Message and sender ids are only `%q`-quoted, so validate any new field.
- **Nothing auto-approves because a peer asked.** Bridges decline every approval and permission request.
- **Delivery.**
  - Pushes (deliverers, channel push, bridges) are at-least-once: they acknowledge only after hand-off.
  - Once a push succeeds, acknowledge on a context the deliverer's shutdown can't cancel. `leave` and rebinding cancel it, sometimes right after the push, and a lost ack means the next join delivers the message again (`TestHandOffAcksWhenStopped`).
  - Pulls through `Agent.Inbox` (the `inbox` tool, `ask` waits, the CLI) acknowledge on read, a deliberate at-most-once.
  - Acks are a cumulative cursor, and `Agent.Fetch` returns each id only once per `Agent`. So hand off in seq order, keep unshown messages in `pending` (`requeue`), and call `remember` to record depths.
  - Sends are idempotent on (sender, id): a retry reuses its id.
- **Loops are bounded.**
  - Replies and follow-up asks sent through the MCP tools or a bridge carry a `depth`.
  - Bridges stop at `-max-depth` (default `client.MaxAutoDepth`, 5). At that depth, the envelope tells interactive agents not to answer.
  - The relay rate-limits each participant.
- **Bridges run agents with `bridge.ChildEnv()`.** It drops `THREADSPLICE_ADMIN_TOKEN` and sets `THREADSPLICE_BRIDGE=1`, which makes the hooks and the MCP server stand down.
- **The web UI stays locked down** (`server.guard`, `security_test.go`). Never loosen its CSP. Inline `<script>`, `style=` attributes, `hx-on:*` and `js:` values are all blocked, and htmx runs with `allowEval: false`. Put behaviour in `static/ui.js` and styles in `static/style.css`. No test runs a browser, so blocked markup passes CI and silently does nothing.
- **Logs carry no tokens, room keys or message text.**

## Compatibility

- **Relays and connectors upgrade separately.** The relay decodes requests with `DisallowUnknownFields`, so a new request field must be `omitempty` and sent only when used.
- **The relay's `schema` const applies only to an empty database.** A schema change needs a migration case in `openStore` and a `user_version` bump. Tests always start from a fresh database, so they won't catch a missing migration.
- **`Identity.Alias` can differ from `Identity.Name`.** The alias is the file name, what `-as` selects, and the key for session and bridge files; the name is what peers see. The UI and the CLI's default names save clashes as `<name>-<room>[-N]`. A session member's alias is `<parent alias>/<name>`.
- **`POST /v1/rooms/{room}/participants`** (session members) answers 405 on relays from before it, which registered only GET there. The client maps that to `client.ErrChildrenUnsupported`.

## Gotchas

- **Claude Code delivery** posts to the session's inbox socket (`CLAUDE_CODE_MESSAGING_SOCKET`/`_TOKEN`). The MCP server inherits it because it's the session's child.
  - Under `-deliver auto`, the headless entrypoints `sdk-cli`, `sdk-ts`, `sdk-py` and `claude-code-github-action` hold. Native Windows (a named pipe) always holds.
  - The VS Code extension reports `claude-vscode` and has the socket.
  - Deliveries are paced 2 s apart (`claudePace`). The socket gives no receipt.
- **Session mode** (Claude Code without `-as`; see the identity model in [docs/architecture.md](docs/architecture.md)).
  - It's decided at `initialize`: a client whose name contains "claude", with a valid `CLAUDE_CODE_SESSION_ID`. `-as`, `THREADSPLICE_AS` and `-deliver channel|codex` keep a fixed identity; anything else gets the default identity.
  - Bindings are keyed by the Claude session id (`bindings/<id>.json`). `bindings/pid-<pid>.json` records the conversation each Claude Code process shows, because the MCP server keeps the session id it was spawned with.
  - Claude Code doesn't document the order of SessionStart and the MCP server's `initialize`, and `claude --continue` can hand the server the startup session id. At `initialize` the server keeps a pid record its own process wrote (newer than the process's inbox socket) and replaces only an earlier process's. It re-reads the binding before every tool call and, while it delivers live, every 2 s.
  - `join` carries `_meta["anthropic/requiresUserInteraction"]`: Claude Code asks the user for every join in every mode, and `dontAsk` refuses it. Background tests pre-join on the session's behalf (docs/live-testing.md).
  - `threadsplice mcp` must be Claude Code's direct child: it takes the Claude Code pid from `os.Getppid()`, while hooks use `$CLAUDE_PID`. A wrapper in between (a script that doesn't `exec`, npx) splits its pid record from the hooks'. Put variables in the MCP config's `env` instead.
  - Never prune `bindings/<session id>.json` by age: a binding is how `claude --resume` rejoins, however old the conversation. Only `pid-<pid>.json` records of dead processes are reclaimed.
  - SessionEnd(reason `clear`) fires before SessionStart(source `clear`). SessionEnd must keep the pid record, or `/clear` can't move the binding.
  - Session-member files live in `children/<parent alias>/`, outside `identities/`, so `List` and `Load("")` never see them. They are never overwritten: `SaveChild` writes the token before the relay call (pending, no participant id) and completes the file once. `DeleteChild` removes only a pending one.
- **Fence the environment in tests**, as `internal/mcp`'s `TestMain` does: a temp `THREADSPLICE_HOME`, `THREADSPLICE_AS` empty, and `CLAUDE_CODE_MESSAGING_*`, `CLAUDE_CODE_SESSION_ID`, `CLAUDE_PID`, `CLAUDE_PROJECT_DIR` and `CLAUDE_CODE_ENTRYPOINT` unset. Run from inside a Claude session, an unfenced test can post into that session, bind to it, or touch real identities.
- **Codex.**
  - `-c mcp_servers={}` doesn't disable MCP servers. The bridge runs Codex under its own `CODEX_HOME`, which holds only a symlink to `auth.json`.
  - Peer text enters as `toolOutput`, which needs Codex ≥ 0.151. The bridge fails closed when Codex reports an older version or rejects the field.
  - `maxWaitSeconds` (240) must stay under the `tool_timeout_sec = 300` that `setup codex` writes; Codex's documented default is 60 s, and a call it abandons loses the messages it already fetched.
- **The relay's long-poll cap** (`maxWait`, 30 s) must stay under the client's 60 s HTTP timeout and under proxy read timeouts.
- **`seq` is relay-wide,** so it has gaps within a room. Participants resume from their server-side `acked_seq`.
- **Setup text** replaces every literal `NAME` with the alias, so keep that word out of the prose. `TestSetup` matches phrases that must not wrap across lines. Vendor config formats there follow the vendors' docs as of October 2026; re-check those docs before changing them. The UI's Connect view has its own copy of the session-mode hooks (`claudeHooks` in `internal/ui/room.go`), and its chat phrase (`joinPhrase`) repeats how `join` picks a membership (`client.MachineMemberships`): keep each pair in step.

## Conventions

- **Stdlib first.** The only direct dependency is `modernc.org/sqlite` (pure Go). Don't add another for what a few lines can do.
- **Mark deliberate simplifications with `// ponytail:` comments,** naming the ceiling and the upgrade path.
- **Tests fake the external agent.** The test binary re-runs itself as the fake (for example `-fake-acp-agent`), or the test uses `io.Pipe`. Tests never spend model tokens.
- **Keep docs in step with the code.** Update the README Status table, and `docs/threat-model.md` when a boundary moves, in the same change.
- **Naming.** Prose says "thread splice"; the binary and module say `threadsplice`.
- **No `# comment` after a command people paste** (docs, setup text, CLI and installer output). zsh, macOS's default shell, passes the comment's words as arguments. Put the hint on its own line; in the UI, Copy skips `.cmt`.
- **Never commit** `deploy/.env` or `*.db` files (both gitignored). The same goes for identity files and `.github/hooks/threadsplice.json`, which are not gitignored, so keep `THREADSPLICE_HOME` outside the repo.

Live tests against a real Claude Code session spend model tokens; see [docs/live-testing.md](docs/live-testing.md).
