# Contributing

Thanks for helping. thread splice is small on purpose; please keep it that way.

## Build and test

```sh
go build ./cmd/threadsplice
go vet ./...
go test -race ./...
```

Go 1.26 or newer is required. With the default `GOTOOLCHAIN=auto`, the `go` command downloads the toolchain pinned in `go.mod`. CI runs the same commands on Linux, macOS and Windows, plus `gofmt` and `govulncheck`.

## Ground rules

- **Stdlib first.** The relay and connector have exactly one runtime dependency (`modernc.org/sqlite`). A new dependency needs a reason that a few lines of code can't cover.
- **Supported interfaces only.** Agent integrations use documented hooks, MCP, app-server/ACP/AHP protocols, or official CLIs. Never use terminal keystroke injection, screen scraping, or reading another tool's private files.
- **Peer content is untrusted.** It reaches a model only through `client.Envelope`. It never appears in hook output or other high-authority channels, and nothing ever auto-approves a tool because a peer asked.
- **No secrets in logs.** Log message ids and kinds, never bodies, tokens or room keys.
- **Tests come with logic.** Fake the external agent process (helper process or `io.Pipe`). Never spend real model tokens in tests.
- **Honest docs.** Say what is verified and what isn't. A provider feature is "supported" only once it has been exercised end to end.

## Reporting security issues

Privately, please. See [SECURITY.md](SECURITY.md).

## Conduct

Be kind and assume good faith. This project follows the [Contributor Covenant 2.1](https://www.contributor-covenant.org/version/2/1/code_of_conduct/). Report problems to the maintainer ([@t0uh33d](https://github.com/t0uh33d)) through GitHub.
