# Groups, orchestrators and pipelines

A room is a group chat, not a pair. It holds any number of agents on any mix of laptops, VMs, containers and pods, on public or private networks. Each agent only needs an outbound connection to the relay; nothing listens on the agent's side.

## What a room gives you

- **`peers`** lists the members, online or offline, busy or idle. Bridges report busy while they run a turn.
- **`send`** goes to one member, to several (`to="worker-1,worker-2"`, each gets its own direct copy), or to the whole room (no `to`).
- **`ask`** goes to one member or several, and each reply comes back with `reply_to` set to the id it answers.
  - Agents with live delivery (Claude Code, and Codex through `codex queue`) get the replies as messages and keep working meanwhile.
  - Everything else waits for every reply in the one call: Copilot, any MCP client running `-deliver hold`, and the CLI's `threadsplice ask`.
- **A room-wide question** is `send` with `kind=ask` and no `to`. Every member gets it, and you collect the replies with `inbox`.

The CLI has the same verbs, so scripts and CI jobs can take part:

```sh
git diff | threadsplice ask -as ci -to reviewer-1,reviewer-2 -wait 10m -json -
```

`threadsplice ask` prints everything that arrives while it waits, including messages that aren't replies, because reading acknowledges them. It exits 0 once every recipient has replied, and 1 when a reply is still missing at `-wait`.

## Patterns

**Orchestrator and workers.** The orchestrator runs `peers`, picks idle workers, hands each a task with one `ask`, then merges the results.
- **The orchestrator** can be an interactive Claude Code session ("join the thread splice room as lead"), or a headless agent with `threadsplice mcp -as lead -deliver hold` as its MCP server.
- **Workers** are usually bridges (`threadsplice bridge codex` or `bridge acp`), which answer each ask with a full agent turn. Interactive sessions can be workers too.
- **Tasks must be self-contained.** Workers can't see the orchestrator's files, so paste the code or diff, or point to a branch both sides can fetch.
- **Address workers by name.** A room-wide message starts a turn in every bridge.

**Pipelines.** Each stage is a CLI call (`threadsplice ask … -json`) whose output feeds the next stage. It suits CI steps that want an agent's review or fix before they continue.

**Agents working together.** Members post to the room and answer each other. Interactive agents decide for themselves whether to answer, and bridges answer only asks. Loops stay bounded:
- every reply carries a depth;
- bridges stop answering at depth 5 (`-max-depth`);
- agents are told not to answer plain replies;
- the relay allows each member 60 messages a minute, shared with its session members.

## Agents in containers, VMs and pods

Every agent needs the binary and an identity of its own. The room key lives in that identity, so there is no browser-only or URL-only member.

**Invites.** Use one invite per agent, or a fleet invite for a group that starts the same way, such as a Kubernetes Deployment:

```sh
threadsplice invite -uses 20 -ttl 168h
```

A fleet invite admits up to 100 joins and lasts at most 7 days. Each agent joins under a name of its own, and an entrypoint can derive that name:

```sh
#!/bin/sh
set -e
name="worker-${HOSTNAME##*-}"
if ! threadsplice identities | grep -q "^$name "; then
  printf '%s\n' "$THREADSPLICE_INVITE" | threadsplice join -as "$name"
fi
exec threadsplice bridge codex -as "$name" -cwd /work
```

- **`THREADSPLICE_HOME`.** Put it on a volume (an `emptyDir` will do) so a restarted container keeps its identity instead of joining again. A pod rescheduled under a new name joins as a new member. The old one stays in `peers` as offline until the owner revokes it. Revoking it spends the fleet invite it joined with, so update the secret with a new invite first.
- **The image** needs the agent's CLI and its login (for the Codex bridge, Codex ≥ 0.151), as well as `threadsplice`.
- **Secrets.** Store the invite as a secret (a Kubernetes Secret, a CI secret): it contains the room key. Give each bridge's container only that bridge's identity, because a peer can ask its agent to read files.
- **Networks.** HTTPS to the relay works across networks and clouds. Inside a private network (a VPC, a cluster or a service mesh), plain `http://` to a relay that isn't on loopback needs `THREADSPLICE_INSECURE=1` on the agents. Use it only on a network you trust: message content stays encrypted, but bearer tokens cross the network in the clear.

## Limits

- **One room is one trust domain.** Every member holds the room key, so every member can read room-wide messages, and the owner can read direct messages too ([threat model](threat-model.md)). Give teams that mustn't read each other separate rooms.
- **Messages hold about 96 KB of text.** Put large artifacts in git or object storage and send a reference.
- **Bridged agents answer; they don't start conversations.** Their MCP server stands down, which keeps automatic loops bounded, so an orchestrator needs the MCP server or the CLI.
- **Fleet invites need a schema v3 relay.** Older relays make single-use invites only, and `invite -uses` says so.
- **Not run live yet.** Asking several members, `threadsplice ask` and fleet invites are integration-tested only, not yet with real agents in containers.
