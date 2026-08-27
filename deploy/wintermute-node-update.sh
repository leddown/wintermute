#!/bin/sh
# Pull the current fleet agent from the server this node already reports to.
#
# The build lives on the server: it is compiled there, from the same commit the
# server itself runs, for both architectures. This is the node's end of that.
# It reads the address and the token out of /etc/wintermute/node.env, so
# updating a host takes no arguments and does not need a token dug out of
# wherever it was written down months ago.
#
# It fetches the server's installer rather than carrying its own copy of the
# install steps. A node updating therefore runs the *current* installer, not
# whichever one shipped with the version it happens to be on, and the install
# logic has one home.
#
# An operator runs this. It is deliberately not on a timer, and the agent never
# calls it -- nothing in the reporting loop can reach it. A server that could
# replace the binary running as a service on every node, unattended, would be
# root on the whole fleet the moment it was compromised. Putting this on a
# systemd timer is that decision; take it knowingly if you take it.
set -eu

ENV_FILE=/etc/wintermute/node.env
BIN=/usr/local/bin/wintermute-node
CHECK=0
SERVER=""

die() { echo "error: $*" >&2; exit 1; }
say() { echo "==> $*"; }

usage() {
  cat <<'USAGE'
Usage: wintermute-node-update [--check] [--server <url>] [-- <installer options>]

  --check           report whether the server holds a different build, and stop.
                    Exit 0 if this node is current, 10 if an update is waiting,
                    so a loop over hosts can skip the ones with nothing to do.
  --server <url>    ask a different address than the one in node.env. Does not
                    change what is recorded there.

Anything after -- goes to the installer:

  wintermute-node-update -- --store /srv/models

With no arguments: fetch the current agent and install it, restarting the
service. node.env is left alone, so the token and this host's settings survive.
USAGE
}

while [ $# -gt 0 ]; do
  case "$1" in
    --check)   CHECK=1; shift ;;
    --server)  [ $# -ge 2 ] || die "--server needs a value"; SERVER="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    --)        shift; break ;;
    *) usage >&2; die "unknown option: $1" ;;
  esac
done

command -v curl >/dev/null 2>&1 || die "curl is required"
[ -r "$ENV_FILE" ] || die "cannot read $ENV_FILE (run this as root)"

# Last assignment wins, the same way systemd reads an EnvironmentFile.
env_value() { sed -n "s/^[[:space:]]*$1=//p" "$ENV_FILE" | tail -1; }

[ -n "$SERVER" ] || SERVER="$(env_value WINTERMUTE_SERVER)"
TOKEN="$(env_value WINTERMUTE_TOKEN)"
[ -n "$SERVER" ] || die "$ENV_FILE names no WINTERMUTE_SERVER"
[ -n "$TOKEN" ]  || die "$ENV_FILE names no WINTERMUTE_TOKEN"
SERVER="${SERVER%/}"

case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m); the server builds amd64 and arm64" ;;
esac

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

fetch() {
  curl -fsSL --retry 3 -H "Authorization: Bearer $TOKEN" \
    "$SERVER/api/v1/node-agent/$1" -o "$2" \
    || die "could not fetch $1 from $SERVER (is the token still valid, and has update.sh been run there?)"
}

# --check compares checksums rather than versions. Two builds of the same commit
# report the same -version string, and what matters here is whether the file on
# the server is the file on this disk.
if [ "$CHECK" -eq 1 ]; then
  command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required for --check"
  if [ ! -x "$BIN" ]; then
    die "$BIN is not installed here; run this without --check to install it"
  fi

  fetch SHA256SUMS "$TMP/SHA256SUMS"
  WANT="$(awk -v f="wintermute-node.$ARCH" '$2 == f || $2 == "*" f { print $1 }' "$TMP/SHA256SUMS")"
  [ -n "$WANT" ] || die "SHA256SUMS on the server names no wintermute-node.$ARCH"
  GOT="$(sha256sum "$BIN" | cut -d" " -f1)"

  if [ "$WANT" = "$GOT" ]; then
    echo "up to date: $BIN matches the build on $SERVER"
    exit 0
  fi
  echo "an update is waiting on $SERVER"
  echo "  installed: $GOT"
  echo "  server:    $WANT"
  echo "  run wintermute-node-update to take it"
  exit 10
fi

[ "$(id -u)" -eq 0 ] || die "run this as root (it replaces $BIN and restarts the service)"

say "Fetching the installer from $SERVER"
fetch install.sh "$TMP/install.sh"
# Handed the token explicitly rather than left to be read back out of node.env,
# so a --server pointing somewhere else still authenticates the same way.
sh "$TMP/install.sh" --token "$TOKEN" "$@"
