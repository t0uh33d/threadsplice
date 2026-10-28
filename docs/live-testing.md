# Live testing against Claude Code

The test suite fakes every agent. These scripts check delivery into a real Claude Code session: a background session (`claude --bg`, Claude Code 2.1.267 or later) joins a room on a loopback relay, and a scripted peer asks it a question. Each spends a few model turns; Haiku keeps them cheap.

Run one with bash from the repository root, all in one go. When an agent runs it (for example through Claude Code's Bash tool), it has to be a single call, because shell variables don't survive between calls.

## Fixed identity

The session's MCP server acts as a fixed identity, `-as agent`, as Codex and Copilot do.

```bash
set -euo pipefail
T=$(mktemp -d)
go build -o "$T/threadsplice" ./cmd/threadsplice
# Keep a parent Claude Code session's inbox and session out of the test.
unset CLAUDE_CODE_MESSAGING_SOCKET CLAUDE_CODE_MESSAGING_TOKEN CLAUDE_CODE_SESSION_ID CLAUDE_PID CLAUDE_CODE_ENTRYPOINT THREADSPLICE_AS
export THREADSPLICE_HOME=$T/home THREADSPLICE_ADMIN_TOKEN=test-admin-token-0123456789
"$T/threadsplice" relay -addr 127.0.0.1:18100 -db "$T/relay.db" 2> "$T/relay.log" &
RELAY=$!
until curl -fsS http://127.0.0.1:18100/healthz >/dev/null 2>&1; do sleep 0.2; done
"$T/threadsplice" room create -relay http://127.0.0.1:18100 -name demo -as owner > "$T/room.txt"
# The invite is the indented line that starts with the relay URL; join reads it from stdin.
awk '/^  http/ {print $1}' "$T/room.txt" | "$T/threadsplice" join -as agent
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

The session is limited to the four thread splice tools.

## Session mode

The MCP server runs without `-as`, as `threadsplice setup claude` registers it. A real `join` always waits for a person: the tool is marked so that Claude Code asks before every join, and `dontAsk` mode refuses it. So the script joins on the background session's behalf, the way its `join` tool would after you approved it. It drives `threadsplice mcp` once with the session's own id, and the session's MCP server picks up the binding within a couple of seconds. `claude --bg` picks its own session id (it ignores `--session-id`), so the script reads it from `claude agents --json`.

The session's machine gets a home of its own, as a second machine would.

```bash
set -euo pipefail
T=$(mktemp -d)
go build -o "$T/threadsplice" ./cmd/threadsplice
# Keep a parent Claude Code session's inbox and session out of the test.
unset CLAUDE_CODE_MESSAGING_SOCKET CLAUDE_CODE_MESSAGING_TOKEN CLAUDE_CODE_SESSION_ID CLAUDE_PID CLAUDE_CODE_ENTRYPOINT THREADSPLICE_AS
export THREADSPLICE_HOME=$T/home THREADSPLICE_ADMIN_TOKEN=test-admin-token-0123456789
"$T/threadsplice" relay -addr 127.0.0.1:18100 -db "$T/relay.db" 2> "$T/relay.log" &
RELAY=$!
until curl -fsS http://127.0.0.1:18100/healthz >/dev/null 2>&1; do sleep 0.2; done
"$T/threadsplice" room create -relay http://127.0.0.1:18100 -name demo -as owner > "$T/room.txt"
awk '/^  http/ {print $1}' "$T/room.txt" | THREADSPLICE_HOME=$T/agent "$T/threadsplice" join -as agent
cat > "$T/mcp.json" <<EOF
{"mcpServers": {"threadsplice": {"command": "$T/threadsplice", "args": ["mcp"], "env": {"THREADSPLICE_HOME": "$T/agent"}}}}
EOF
mkdir -p "$T/repo/.git" && cd "$T/repo"
# The prompt goes first: --allowedTools takes every argument after it.
claude "Reply 'ready'. Answer kind=ask messages with the threadsplice send tool." --bg -n ts-live-session --model haiku \
  --strict-mcp-config --mcp-config "$T/mcp.json" --setting-sources project --permission-mode dontAsk \
  --allowedTools mcp__threadsplice__send mcp__threadsplice__inbox mcp__threadsplice__peers mcp__threadsplice__ask
sleep 10
SID=$(claude agents --json | python3 -c 'import json,sys; print([a["sessionId"] for a in json.load(sys.stdin) if a.get("name") == "ts-live-session"][0])')
# Join on the session's behalf: what its join tool does once you approve it.
CLAUDE_CODE_SESSION_ID=$SID CLAUDE_PROJECT_DIR=$T/repo THREADSPLICE_HOME=$T/agent python3 -c '
import json, subprocess, sys
p = subprocess.Popen([sys.argv[1], "mcp"], stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True)
def call(i, method, params):
    p.stdin.write(json.dumps({"jsonrpc": "2.0", "id": i, "method": method, "params": params}) + "\n"); p.stdin.flush()
    while (m := json.loads(p.stdout.readline())).get("id") != i: pass
    return m
call(1, "initialize", {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "claude-code"}})
print(call(2, "tools/call", {"name": "join", "arguments": {"name": "tester"}})["result"]["content"][0]["text"].splitlines()[0])
p.stdin.close(); p.wait()' "$T/threadsplice"
sleep 3
"$T/threadsplice" send -as owner -to agent.tester -kind ask "What is 17 * 23?"
"$T/threadsplice" inbox -as owner -wait 60s   # expect the reply 391, threaded to the ask
echo "relay pid $RELAY, files in $T"
```

The agent home's only membership is `agent`, so the session joins as `agent.tester`. `THREADSPLICE_HOME=$T/agent $T/threadsplice sessions` lists the binding and whether it's live. `--setting-sources project` keeps your own `~/.claude/settings.json`, and any thread splice hooks in it, out of the test session.

Verified this way on 2026-10-19 with Claude Code 2.1.267:
- **Delivery to a joined session:** a background Haiku session joined as `mac.reviewer`, was woken by an ask from another machine's home, and replied through thread splice in about 4 s.
- **Unjoined sessions stay out:** a room-wide message reached the joined session only. An unjoined session in the same machine home got nothing and didn't appear among the members.
- **Resume:** `claude -p --resume <id>` came back joined without calling `join`, read the waiting ask from its inbox and replied.
- **Background resume:** `claude --resume <id> --bg` starts a new session id, so that resumed session isn't joined.

## Hooks, the UI and cleanup

To exercise the hooks as well, add `--settings` with the hooks that `threadsplice setup claude -as agent` prints (for session mode, `threadsplice setup claude`). Write each command with the binary's absolute path, after the `THREADSPLICE_HOME` the MCP config sets: `THREADSPLICE_HOME=$T/home $T/threadsplice hook claude -as agent`, or in session mode `THREADSPLICE_HOME=$T/agent $T/threadsplice hook claude`. To watch the room, run `THREADSPLICE_HOME=$T/home $T/threadsplice ui`.

Clean up afterwards, including after a failed step. `claude agents --json` lists the session id.

```bash
claude stop <id> && claude rm <id>
kill <relay pid>; rm -rf <files dir>
```

Background sessions also leave their transcripts in `~/.claude/projects/`, in a folder named after the session's working directory with `/` turned into `-`. Delete it with its own command, from outside the files dir: Claude Code's safety check refuses an `rm -rf` that includes the current working directory, and refuses the whole command with it.
