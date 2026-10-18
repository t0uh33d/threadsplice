# Releasing

Pushing a `vX.Y.Z` tag runs [`.github/workflows/release.yml`](../.github/workflows/release.yml), which builds and publishes the release. Tag a commit on `main` that CI has passed:

```sh
git tag v0.1.0
git push origin v0.1.0
```

The workflow runs `go vet` and the race tests, then cross-compiles. It stops if the Linux binary's `threadsplice version` isn't exactly the tag: Go stamps the version from git, so use a full semver tag such as `v0.1.0`. Then it writes `SHA256SUMS`, attests build provenance for every file listed there, and creates the GitHub release with generated notes.

To try it without publishing anything, run the workflow by hand (**Actions → release → Run workflow**). A manual run builds and attests, but skips the release. Pick a tag under "Use workflow from" to include the version check.

## What a release contains

| File | For |
|---|---|
| `threadsplice_darwin_amd64.tar.gz`, `threadsplice_darwin_arm64.tar.gz` | macOS |
| `threadsplice_linux_amd64.tar.gz`, `threadsplice_linux_arm64.tar.gz` | Linux, including WSL |
| `threadsplice_windows_amd64.zip`, `threadsplice_windows_arm64.zip` | Windows |
| `install.sh` | The installer |
| `SHA256SUMS` | SHA-256 of each file above, in `sha256sum` format |

Each archive holds the binary, `LICENSE` and `README.md`. The binaries are static (`CGO_ENABLED=0`) and built with `-trimpath`. The file names carry no version, so `https://github.com/t0uh33d/threadsplice/releases/latest/download/<file>` always fetches the newest release. That's the URL the installer uses.

`install.sh` checks the archive against `SHA256SUMS`, and against its attestation when `gh` is installed and logged in. The script's header lists its settings, such as `THREADSPLICE_VERSION` and `THREADSPLICE_REQUIRE_ATTESTATION=1`.

## Verifying a download

Put the archive and `SHA256SUMS` in one directory, then:

```sh
sha256sum -c --ignore-missing SHA256SUMS     # macOS: shasum -a 256 -c --ignore-missing SHA256SUMS
gh attestation verify threadsplice_linux_amd64.tar.gz --repo t0uh33d/threadsplice
```

The checksum shows that the file matches the release's `SHA256SUMS`. The attestation shows more: it's signed through Sigstore with the release workflow's identity, and it names the commit that was built. Replacing an archive and its `SHA256SUMS` line on the release page can't fake that.

## Immutable releases

The maintainer should turn on GitHub's immutable releases: in the repository's **Settings**, under **Releases**, select **Enable release immutability**. It covers releases published after it's turned on. A published release's assets then can't be added, changed or deleted, and its tag can't be moved or deleted, so a leaked token can't swap a binary under an existing version. `gh release create` uploads the assets to a draft and publishes it afterwards, so the workflow keeps working with the setting on.
