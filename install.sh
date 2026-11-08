#!/bin/sh
# Installs skopos: the release binary for this machine, checked against the
# release's checksums.txt (and its Sigstore signature when cosign is
# installed), into ~/.local/bin. Run it again to upgrade.
#
#   curl -fsSL https://github.com/martinsuchenak/skopos/releases/latest/download/install.sh | sh
#
# Options (or the environment variables after them):
#   --version X.Y.Z   a release instead of the latest      SKOPOS_VERSION
#   --dir DIR         where the binary goes                SKOPOS_INSTALL_DIR (default ~/.local/bin)
#
# A worker as well: --worker sets one up after the install, with
# `skopos worker init` (it asks for what's missing at a terminal), then
# `worker check` and, when that passes, `worker install` (the service):
#   curl -fsSL .../install.sh | sh -s -- --worker --server-url https://skopos.example.com \
#       --workspace github.com/you/repo [--repo PATH] [--clone] [--push github|gitea|none] [--no-service]
# Unattended (no terminal): SKOPOS_API_KEY and GH_TOKEN come from the
# environment, never from arguments. The config is ~/.config/skopos/skopos-config.toml.
#
# A worker running this binary upgrades itself to the server's release (the
# directory is the worker's own, so it can replace the file). Nothing else to
# set up: auto_upgrade is on unless the worker config turns it off.
set -eu

REPO="martinsuchenak/skopos"
# SKOPOS_DOWNLOAD_URL: a mirror of the GitHub releases (the same layout,
# .../v<version>/<asset>), like the worker's upgrade_url. Also for tests.
DOWNLOAD_URL="${SKOPOS_DOWNLOAD_URL:-https://github.com/$REPO/releases/download}"
VERSION="${SKOPOS_VERSION:-}"
INSTALL_DIR="${SKOPOS_INSTALL_DIR:-$HOME/.local/bin}"
WORKER=0
SERVICE=1
CONFIG="${SKOPOS_CONFIG:-$HOME/.config/skopos/skopos-config.toml}"
INIT_ARGS="" # quoted, for eval: `skopos worker init` arguments

say() { printf '%s\n' "$*"; }
fail() { printf 'skopos install: %s\n' "$*" >&2; exit 1; }
# quote: one shell word, safe to eval.
quote() { printf "'%s'" "$(printf '%s' "$1" | sed "s/'/'\\\\''/g")"; }
init_arg() { INIT_ARGS="$INIT_ARGS $(quote "$1") $(quote "$2")"; }

while [ $# -gt 0 ]; do
	case "$1" in
	--version) [ $# -ge 2 ] || fail "--version needs a value"; VERSION="$2"; shift 2 ;;
	--version=*) VERSION="${1#*=}"; shift ;;
	--dir) [ $# -ge 2 ] || fail "--dir needs a value"; INSTALL_DIR="$2"; shift 2 ;;
	--dir=*) INSTALL_DIR="${1#*=}"; shift ;;
	--worker) WORKER=1; shift ;;
	--no-service) SERVICE=0; shift ;;
	--clone) INIT_ARGS="$INIT_ARGS --clone"; shift ;;
	--server-url | --workspace | --repo | --push)
		[ $# -ge 2 ] || fail "$1 needs a value"
		WORKER=1; init_arg "$1" "$2"; shift 2 ;;
	--server-url=* | --workspace=* | --repo=* | --push=*)
		WORKER=1; init_arg "${1%%=*}" "${1#*=}"; shift ;;
	-h | --help) sed -n '2,28p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//'; exit 0 ;;
	*) fail "unknown option: $1 (--version, --dir, --worker, --server-url, --workspace, --repo, --clone, --push, --no-service)" ;;
	esac
done
VERSION="${VERSION#v}"

# --- this machine ---
case "$(uname -s)" in
Linux) OS=linux ;;
Darwin) OS=darwin ;;
*) fail "$(uname -s) isn't supported by this script; download a release from https://github.com/$REPO/releases" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) ARCH=amd64 ;;
arm64 | aarch64) ARCH=arm64 ;;
*) fail "unsupported CPU: $(uname -m)" ;;
esac
# An Intel shell on Apple silicon (Rosetta) still gets the native binary.
if [ "$OS" = darwin ] && [ "$ARCH" = amd64 ] && [ "$(sysctl -n sysctl.proc_translated 2>/dev/null || echo 0)" = 1 ]; then
	ARCH=arm64
fi
ASSET="skopos_${OS}_${ARCH}"

# --- tools ---
if command -v curl >/dev/null 2>&1; then
	fetch() { curl -fsSL --retry 3 -o "$2" "$1"; }
elif command -v wget >/dev/null 2>&1; then
	fetch() { wget -q -O "$2" "$1"; }
else
	fail "needs curl or wget"
fi
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
	fail "needs sha256sum or shasum to check the download"
fi

TMP="$(mktemp -d 2>/dev/null || mktemp -d -t skopos-install)"
trap 'rm -rf "$TMP"' EXIT INT TERM

# --- which release ---
# The latest is read once, so the binary and its checksums come from the
# same release even if one is published meanwhile.
if [ -z "$VERSION" ]; then
	fetch "https://api.github.com/repos/$REPO/releases/latest" "$TMP/latest.json" ||
		fail "couldn't ask GitHub for the latest release (set SKOPOS_VERSION to skip that)"
	VERSION="$(sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"v\{0,1\}\([^"]*\)".*/\1/p' "$TMP/latest.json" | head -n 1)"
	[ -n "$VERSION" ] || fail "couldn't read the latest release's version"
fi
case "$VERSION" in
*[!0-9.]* | "") fail "not a release version: $VERSION" ;;
esac
BASE="$DOWNLOAD_URL/v$VERSION"

# --- download and check ---
say "Downloading skopos $VERSION ($OS/$ARCH)..."
fetch "$BASE/checksums.txt" "$TMP/checksums.txt" || fail "no checksums.txt for v$VERSION at $BASE"
fetch "$BASE/$ASSET" "$TMP/skopos" || fail "no $ASSET in v$VERSION"

if command -v cosign >/dev/null 2>&1; then
	if fetch "$BASE/checksums.txt.sigstore.json" "$TMP/checksums.txt.sigstore.json" 2>/dev/null; then
		cosign verify-blob --bundle "$TMP/checksums.txt.sigstore.json" \
			--certificate-identity-regexp "^https://github.com/$REPO/" \
			--certificate-oidc-issuer https://token.actions.githubusercontent.com \
			"$TMP/checksums.txt" >/dev/null 2>&1 || fail "checksums.txt doesn't carry the release's signature"
		say "Signature: verified (cosign)"
	else
		fail "v$VERSION has no checksums.txt.sigstore.json to verify with cosign"
	fi
fi

WANT="$(awk -v a="$ASSET" '$2 == a || $2 == "*" a { print $1; exit }' "$TMP/checksums.txt")"
[ -n "$WANT" ] || fail "checksums.txt of v$VERSION lists no $ASSET"
GOT="$(sha256 "$TMP/skopos")"
[ "$GOT" = "$WANT" ] || fail "$ASSET doesn't match its checksum (expected $WANT, got $GOT)"
say "Checksum: verified"

chmod 755 "$TMP/skopos"
"$TMP/skopos" --version 2>&1 | grep -q "$VERSION" || fail "the downloaded binary doesn't run as $VERSION"

# --- install ---
mkdir -p "$INSTALL_DIR" || fail "can't create $INSTALL_DIR"
DEST="$INSTALL_DIR/skopos"
# A rename within the directory: a running worker keeps its old file, and the
# new one appears whole.
cp "$TMP/skopos" "$INSTALL_DIR/.skopos-install.$$" || fail "can't write to $INSTALL_DIR"
mv -f "$INSTALL_DIR/.skopos-install.$$" "$DEST" || { rm -f "$INSTALL_DIR/.skopos-install.$$"; fail "can't replace $DEST"; }
say "Installed: $DEST ($("$DEST" --version 2>&1 | head -n 1))"

# --- what to do next ---
case ":$PATH:" in
*":$INSTALL_DIR:"*)
	FOUND="$(command -v skopos 2>/dev/null || true)"
	if [ -n "$FOUND" ] && [ "$FOUND" != "$DEST" ]; then
		say ""
		say "Note: $FOUND comes first on your PATH, not $DEST."
		case "$FOUND" in
		*/homebrew/* | */linuxbrew/* | /usr/local/bin/*) say "If it's the Homebrew cask, remove it: brew uninstall --cask skopos" ;;
		esac
	fi
	;;
*)
	case "${SHELL:-}" in
	*/zsh) RC="$HOME/.zshrc" ;;
	*/bash) RC="$HOME/.bashrc" ;;
	*/fish) RC="$HOME/.config/fish/config.fish" ;;
	*) RC="your shell's startup file" ;;
	esac
	say ""
	say "$INSTALL_DIR isn't on your PATH. Add it to $RC:"
	case "${SHELL:-}" in
	*/fish) say "  fish_add_path $INSTALL_DIR" ;;
	*) say "  export PATH=\"$INSTALL_DIR:\$PATH\"" ;;
	esac
	;;
esac

# --- a worker ---
if [ "$WORKER" = 1 ]; then
	say ""
	mkdir -p "$(dirname "$CONFIG")" || fail "can't create $(dirname "$CONFIG")"
	if grep -q '^\[worker\]' "$CONFIG" 2>/dev/null; then
		say "Worker config: $CONFIG already has a [worker] section, kept as it is."
	else
		say "Setting up the worker ($CONFIG)..."
		# At a terminal, init asks for what's missing; a pipe from curl is
		# this script's stdin, so the questions read the terminal.
		if (exec </dev/tty) 2>/dev/null; then
			eval "\"\$DEST\" worker init --config \"\$CONFIG\" $INIT_ARGS" </dev/tty || fail "worker init failed"
		else
			eval "\"\$DEST\" worker init --config \"\$CONFIG\" $INIT_ARGS" </dev/null || fail "worker init failed"
		fi
	fi
	say ""
	if "$DEST" worker check --config "$CONFIG"; then
		if [ "$SERVICE" = 1 ]; then
			"$DEST" worker install --config "$CONFIG" || fail "worker install failed"
			# The service now runs $DEST: nothing for the check below to report.
			exit 0
		fi
		say "Start it as a service: $DEST worker install --config $CONFIG"
	else
		say ""
		say "The worker isn't ready yet: fix what's listed above in $CONFIG"
		say "(secrets in $(dirname "$CONFIG")/skopos-worker.env), then:"
		say "  $DEST worker check --config $CONFIG && $DEST worker install --config $CONFIG"
	fi
	exit 0
fi

# A worker service installed from another binary (Homebrew, an older place)
# keeps running that one until it is installed again.
for unit in "$HOME/Library/LaunchAgents/skopos-worker.plist" \
	"$HOME/.config/systemd/user/skopos-worker.service" \
	"/etc/systemd/system/skopos-worker.service"; do
	if [ -f "$unit" ] && ! grep -q "$DEST" "$unit" 2>/dev/null; then
		say ""
		say "The worker service ($unit) runs another skopos binary."
		say "Point it at this one: $DEST worker install   (with the flags you installed it with)"
	fi
done
