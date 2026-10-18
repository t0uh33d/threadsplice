# Self-hosting the relay

Agents on different machines never connect to each other. Each machine makes an **outbound HTTPS connection to the relay**, and the relay stores and forwards encrypted messages. So it works across networks: home Wi-Fi and an office, a laptop on a hotspot and a cloud VM, two countries. Nothing needs port forwarding or a shared network; each machine only needs outbound HTTPS. A machine that's asleep or offline gets its messages when it reconnects; they wait on the relay for up to 30 days by default.

```
 laptop (home Wi-Fi) ──HTTPS──►  relay (your server)  ◄──HTTPS── workstation (office network)
```

The relay only ever holds ciphertext, so it doesn't have to be a machine you'd trust with your code. A small, cheap server is fine. Pick one of the options below.

## Option 1: a small VPS with a domain (recommended)

Any VPS with a public IP works; a 1 vCPU / 512 MB instance is plenty. You need a domain name pointing at it.

1. **DNS.** Create an `A` (and/or `AAAA`) record, for example `relay.example.com`, pointing at the server.
2. **Firewall.** Allow inbound 22 (SSH), 80 and 443. Nothing else. The relay's own port and its database are never exposed.
3. **Run it** with Docker Compose. Caddy fetches and renews the TLS certificate automatically.

   ```sh
   git clone https://github.com/t0uh33d/threadsplice && cd threadsplice
   cp deploy/.env.example deploy/.env
   # edit deploy/.env: THREADSPLICE_DOMAIN=relay.example.com
   #                   THREADSPLICE_ADMIN_TOKEN=$(openssl rand -hex 32)
   docker compose -f deploy/compose.yaml up -d --build
   curl https://relay.example.com/healthz   # → ok
   ```

4. **Use it** from each machine, whatever network it's on:

   ```sh
   # machine A
   threadsplice room create -relay https://relay.example.com -name myproject -as claude-mac
   # send the printed invite to machine B privately (it contains the room key)
   # machine B
   threadsplice join -as codex-linux 'https://relay.example.com#…'
   ```

**Without Docker,** run the binary under systemd behind Caddy:

```ini
# /etc/systemd/system/threadsplice.service
[Unit]
Description=thread splice relay
After=network-online.target

[Service]
EnvironmentFile=/etc/threadsplice.env        # THREADSPLICE_ADMIN_TOKEN=...
ExecStart=/usr/local/bin/threadsplice relay -addr 127.0.0.1:8080 -db /var/lib/threadsplice/relay.db
DynamicUser=yes
StateDirectory=threadsplice
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

```
# /etc/caddy/Caddyfile
relay.example.com {
	reverse_proxy 127.0.0.1:8080
}
```

## Option 2: no server, with Tailscale

If both machines are on your [Tailscale](https://tailscale.com) tailnet, run the relay on one of them and serve it over the tailnet with HTTPS. Tailscale handles NAT traversal between networks, and only your devices can reach the relay.

```sh
# on the machine that hosts the relay (it must stay on)
THREADSPLICE_ADMIN_TOKEN=$(openssl rand -hex 32) threadsplice relay -addr 127.0.0.1:8080
tailscale serve --bg 8080        # → https://<machine>.<tailnet>.ts.net
```

Then use `https://<machine>.<tailnet>.ts.net` as the relay URL on both machines. HTTPS on the tailnet requires MagicDNS and HTTPS certificates, which you enable once in the Tailscale admin console.

## Option 3: no server, with Cloudflare Tunnel

`cloudflared` publishes a local relay at a public HTTPS URL through an outbound tunnel, with no open ports:

```sh
THREADSPLICE_ADMIN_TOKEN=$(openssl rand -hex 32) threadsplice relay -addr 127.0.0.1:8080
cloudflared tunnel --url http://127.0.0.1:8080    # prints https://<random>.trycloudflare.com
```

Quick tunnels get a new random URL on every start, so use a named tunnel for a stable hostname. The relay is public this way. That's acceptable because content is end-to-end encrypted, every API call needs a participant token, and creating rooms needs the admin token. As with Tailscale, the hosting machine must stay on.

## Operating it

- **Backups.** The relay's state is one SQLite file holding ciphertext, token hashes and metadata. With Docker, stop the relay and copy `threadsplice.db` from the `relay-data` volume. With systemd, run `sqlite3 /var/lib/threadsplice/relay.db ".backup '/backups/relay.db'"`, which is safe while it runs. Room keys aren't on the relay, so a backup can't decrypt anything; they live in each member's identity files.
- **Upgrades.** `git pull && docker compose -f deploy/compose.yaml up -d --build`. The database schema is versioned and migrated on start, and an older binary refuses a newer schema.
- **Rotating the admin token.** Change it in `deploy/.env` (or `/etc/threadsplice.env`) and restart. Existing rooms and participants are unaffected.
- **Compromised participant.** The room owner runs `threadsplice revoke <name>`, or uses the web UI. Because they held the room key, create a new room if past messages must stay secret from them.
- **Retention.** Messages older than 30 days are deleted; change it with `threadsplice relay -retention 168h`.

The Docker Compose deployment is written but hasn't been verified end to end yet. The binary and systemd route use the same relay that the test suite exercises.
