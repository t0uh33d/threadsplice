#!/bin/sh
# Installs thread splice from a GitHub release, with no Go and no root:
#
#   curl -fsSL https://github.com/t0uh33d/threadsplice/releases/latest/download/install.sh | sh
#
# Add "-s -- join" after sh to run threadsplice join (it asks for the invite)
# straight after installing.
#
# It downloads the archive for this OS and CPU, checks it against the release's
# SHA256SUMS (and its build attestation, when gh is installed and logged in),
# and copies the binary to ~/.local/bin. Optional settings:
#
#   THREADSPLICE_VERSION=v0.1.0          a release tag instead of latest
#   THREADSPLICE_INSTALL_DIR=DIR         where the binary goes (default ~/.local/bin)
#   THREADSPLICE_REQUIRE_ATTESTATION=1   stop unless gh verifies the attestation
#   THREADSPLICE_DOWNLOAD_BASE=URL       the releases URL (tests use file://)
set -eu

repo=t0uh33d/threadsplice

say() { printf '%s\n' "$@"; }

die() {
	printf 'threadsplice install: %s\n' "$1" >&2
	exit 1
}

# fetch URL writes the file at URL to stdout.
fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO- "$1"
	else
		die "this needs curl or wget"
	fi
}

# sha256 FILE prints the file's SHA-256 in hex.
sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum <"$1" | awk '{ print $1 }'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 <"$1" | awk '{ print $1 }'
	elif command -v openssl >/dev/null 2>&1; then
		openssl dgst -sha256 <"$1" | awk '{ print $NF }'
	else
		die "this needs sha256sum, shasum or openssl to check the download"
	fi
}

# main runs from the last line, so a download cut short runs nothing.
main() {
	join=false
	if [ $# -gt 0 ]; then
		[ "$1" = join ] || die "unknown argument $1; the only one is join"
		join=true
		shift # anything after join goes to threadsplice join
	fi

	case $(uname -s) in
	Darwin) os=darwin ;;
	Linux) os=linux ;;
	*) die "this installer is for macOS and Linux. On Windows, use WSL, or download the Windows zip from https://github.com/$repo/releases" ;;
	esac
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "there is no build for $(uname -m), only for amd64 and arm64" ;;
	esac
	# A shell under Rosetta reports x86_64 on Apple silicon; install the native build.
	if [ "$os" = darwin ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null)" = 1 ]; then
		arch=arm64
	fi

	version=${THREADSPLICE_VERSION:-latest}
	base=${THREADSPLICE_DOWNLOAD_BASE:-https://github.com/$repo/releases}
	case $version in
	latest) url=$base/latest/download ;;
	v[0-9]*) url=$base/download/$version ;;
	*) die "THREADSPLICE_VERSION must be a release tag such as v0.1.0, or latest" ;;
	esac
	dir=${THREADSPLICE_INSTALL_DIR:-$HOME/.local/bin}
	bin=$dir/threadsplice
	new=$dir/.threadsplice.$$ # the new binary until it's renamed into place

	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"; rm -f "$new"' EXIT
	trap 'exit 1' HUP INT TERM

	archive=threadsplice_${os}_${arch}.tar.gz
	say "Downloading $archive ($version)"
	fetch "$url/$archive" >"$tmp/$archive" || die "could not download $url/$archive"
	fetch "$url/SHA256SUMS" >"$tmp/SHA256SUMS" || die "could not download $url/SHA256SUMS"

	# Fail closed: a missing entry stops here, and so does a different hash.
	# Lines read "<hex>  <file>", or "<hex> *<file>" in binary mode.
	want=$(awk -v f="$archive" '$2 == f || $2 == "*" f { print $1; exit }' "$tmp/SHA256SUMS")
	[ -n "$want" ] || die "SHA256SUMS has no entry for $archive"
	got=$(sha256 "$tmp/$archive")
	[ "$got" = "$want" ] || die "checksum mismatch for $archive: SHA256SUMS says $want, the download is $got"
	say "Checksum OK."

	# The checksum shows the archive matches SHA256SUMS. The attestation shows
	# that this repository's release workflow built it.
	require=${THREADSPLICE_REQUIRE_ATTESTATION:-0}
	if ! command -v gh >/dev/null 2>&1 || ! gh auth status --hostname github.com >/dev/null 2>&1; then
		[ "$require" != 1 ] || die "THREADSPLICE_REQUIRE_ATTESTATION=1 needs gh, installed and logged in"
		say "Note: the build attestation wasn't checked; that needs gh, installed and logged in."
	elif out=$(gh attestation verify "$tmp/$archive" --repo "$repo" 2>&1); then
		say "Attestation OK."
	elif [ "$require" = 1 ]; then
		die "gh attestation verify failed: $out"
	else
		say "Note: gh attestation verify failed, so only the checksum was checked:" "$out"
	fi

	tar -xzf "$tmp/$archive" -C "$tmp" threadsplice || die "could not unpack $archive"
	mkdir -p "$dir"
	# Copy beside the old binary, then rename over it. The rename is atomic, and
	# a threadsplice that's running (an MCP server, say) keeps its old file.
	cp "$tmp/threadsplice" "$new"
	chmod 755 "$new"
	mv -f "$new" "$bin"
	v=$("$bin" version) || die "installed $bin, but it doesn't run"
	say "Installed $v to $bin"

	cmd=$bin
	case :$PATH: in
	*:"$dir":*) cmd=threadsplice ;;
	*) say "" "$dir isn't on your PATH. To add it, put this line in your shell's startup file (~/.zshrc, ~/.bashrc):" "  export PATH=\"$dir:\$PATH\"" ;;
	esac
	say "" "Next steps:" \
		"  $cmd join            # paste the invite when asked" \
		"  $cmd setup claude    # print the config for Claude Code (or codex, copilot, vscode)"

	[ "$join" = true ] || return 0
	# Under curl | sh, stdin is this script, so join has to read the terminal.
	if (: </dev/tty) 2>/dev/null; then
		say "" "Running $cmd join now."
		rm -rf "$tmp" # exec skips the EXIT trap
		exec "$bin" join "$@" </dev/tty
	fi
	say "" "There's no terminal to read the invite from, so run $cmd join yourself."
}

main "$@"
