# Live testing against Claude Code

The test suite fakes every agent. This script checks delivery into a real Claude Code session: a background session (`claude --bg`, Claude Code 2.1.267 or later) joins a room on a loopback relay, and a scripted peer asks it a question. It spends a few model turns; Haiku keeps them cheap.

Run it with bash from the repository root, all in one go. When an agent runs it (for example through Claude Code's Bash tool), it has to be a single call, because shell variables don't survive between calls.

```bash
set -euo pipefail
T=$(mktemp -d)
go build -o "$T/threadsplice" ./cmd/threadsplice
# Keep a parent Claude Code session's inbox out of the test.
unset CLAUDE_CODE_MESSAGING_SOCKET CLAUDE_CODE_MESSAGING_TOKEN CLAUDE_CODE_ENTRYPOINT
export THREADSPLICE_HOME=$T/home THREADSPLICE_ADMIN_TOKEN=test-admin-token-0123456789
"$T/threadsplice" relay -addr 127.0.0.1:18100 -db "$T/relay.db" 2> "$T/relay.log" &
RELAY=$!
until curl -fsS http://127.0.0.1:18100/healthz >/dev/null 2>&1; do sleep 0.2; done
invite=$("$T/threadsplice" room create -relay http://127.0.0.1:18100 -name demo -as owner | grep -o "'http[^']*'" | tr -d "'")
"$T/threadsplice" join -as agent "$invite"
cat > "$T/mcp.json" <<EOF
{"mcpServers": {"threadsplice": {"command": "$T/threadsplice", "args": ["mcp", "-as", "agent"], "env": {"THREADSPLICE_HOME": "$T/home"}}}}
EOF
cd "$T"
# The prompt goes first: --allowedTools takes every argument after it.
claude "Reply 'ready'. Answer kind=ask messages with the threadsplice send tool." --bg -n ts-live-test --model haiku \
  --strict-mcp-config --mcp-config "$T/mcp.json" --permission-mode dontAsk \
  --allowedTools mcp__threadsplice__send mcp__threadsplice__inbox mcp__threadsplice__peers mcp__threadsplice__ask
"$T/threadsplice" send -as owner -to agent -kind ask "What is 17 * 23?"
"$T/threadsplice" inbox -as owner -wait 60s   # expect the reply 391, threaded to the ask
echo "relay pid $RELAY, files in $T"
```

The session is limited to the four thread splice tools. To exercise the hooks as well, add `--settings` with the hooks that `threadsplice setup claude -as agent` prints, written with the absolute path to `$T/threadsplice`. To watch the room, run `THREADSPLICE_HOME=$T/home $T/threadsplice ui`.

Clean up afterwards, including after a failed step. `claude agents --json` lists the session id.

```bash
claude stop <id> && claude rm <id>
kill <relay pid>; rm -rf <files dir>
```
