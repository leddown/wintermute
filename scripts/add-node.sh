#!/usr/bin/env bash
# Add a host to the fleet: issue its token, then print the one command to run
# on it.
#
# Two halves of the job live on two machines and this script only does the half
# that is here. It registers the node against the database the *service* reads —
# which is the trap clients.sh exists to avoid, and the reason this delegates to
# it rather than calling the binary itself — and then prints the installer line
# for the operator to run on the host.
#
# It does not reach out and install anything. That is not a limitation to be
# fixed later: the server never touches a host uninvited, and a server that
# could put a binary on every machine in the fleet would be root on all of them
# the moment it was compromised. See internal/api/nodeagent.go.
#
# What it does do is check, before issuing anything, that the host will find an
# agent to download when it asks — an unset WINTERMUTE_NODE_AGENT_DIR or an
# update.sh that was never run is the usual way this fails, and it fails on the
# far machine, at the end, in front of an operator who has already used the
# token once.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CLIENTS="$HERE/clients.sh"
ENV_FILE="${WINTERMUTE_ENV_FILE:-/etc/wintermute/wintermute.env}"

SERVER=""
STORE=""
RUNTIME=""
NAME=""

usage() {
  cat <<'EOF'
Usage: add-node.sh <name> [options]

  <name>              how this machine is identified everywhere afterwards: in
                      the Fleet view, in model assignments, and in the "node"
                      field of a backend declaration. Never taken from the
                      hostname the agent reports — a node that could name
                      itself could write telemetry attributed to another one.

  --server <url>      the address the node will reach this server on. Guessed
                      from the system hostname and WINTERMUTE_ADDR when it is
                      not given, which is a guess worth checking.
  --store <path>      the host will also hold weights, kept here.
  --runtime <name>    what serves models there: ollama or llamacpp.
                      Only meaningful with --store.

Overrides: WINTERMUTE_ENV_FILE, WINTERMUTE_DB, WINTERMUTE_BIN,
WINTERMUTE_SERVICE_USER — all passed through to clients.sh.
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
  -h | --help | help)
    usage
    exit 0
    ;;
  --server)
    [[ $# -ge 2 ]] || { echo "error: --server needs a value" >&2; exit 2; }
    SERVER="$2"
    shift 2
    ;;
  --store)
    [[ $# -ge 2 ]] || { echo "error: --store needs a value" >&2; exit 2; }
    STORE="$2"
    shift 2
    ;;
  --runtime)
    [[ $# -ge 2 ]] || { echo "error: --runtime needs a value" >&2; exit 2; }
    RUNTIME="$2"
    shift 2
    ;;
  -*)
    echo "error: unknown option '$1'" >&2
    usage >&2
    exit 2
    ;;
  *)
    if [[ -n $NAME ]]; then
      echo "error: only one name, got '$NAME' and '$1'" >&2
      exit 2
    fi
    NAME="$1"
    shift
    ;;
  esac
done

if [[ -z $NAME ]]; then
  usage >&2
  exit 2
fi

# The server takes any non-empty name. This is stricter on purpose: the name
# ends up in a JSON backend declaration, in a URL path, and in whatever the
# operator types next, and a machine identified by something with a quote or a
# space in it is a bad afternoon later. Refused before a token exists, so a
# rejected name costs nothing to retry.
if [[ ! $NAME =~ ^[A-Za-z0-9][A-Za-z0-9._-]*$ ]]; then
  echo "error: '$NAME' is not a usable node name." >&2
  echo "       Letters, digits, dot, dash and underscore; starting with a letter or digit." >&2
  exit 2
fi

if [[ $EUID -ne 0 ]]; then
  echo "error: run with sudo — the database is owned by the service user" >&2
  exit 1
fi

if [[ ! -x $CLIENTS ]]; then
  echo "error: $CLIENTS not found or not executable." >&2
  exit 1
fi

# env_value reads one setting out of the service's env file. Last one wins, the
# same way systemd reads it.
env_value() {
  [[ -r $ENV_FILE ]] || return 0
  sed -n "s/^[[:space:]]*$1=//p" "$ENV_FILE" | tail -1
}

# ---- will the node find an agent to download? ----

AGENT_DIR="${WINTERMUTE_NODE_AGENT_DIR:-$(env_value WINTERMUTE_NODE_AGENT_DIR)}"
if [[ -z $AGENT_DIR ]]; then
  echo "error: WINTERMUTE_NODE_AGENT_DIR is not set in $ENV_FILE." >&2
  echo "       Without it the server serves no agent and the install command below" >&2
  echo "       would fail on the host with a 503. Add it and run update.sh:" >&2
  echo >&2
  echo "         WINTERMUTE_NODE_AGENT_DIR=/var/lib/wintermute/node-agent" >&2
  exit 1
fi

missing=()
for f in wintermute-node.amd64 wintermute-node.arm64 wintermute-node.service \
  node.env.example SHA256SUMS; do
  [[ -f "$AGENT_DIR/$f" ]] || missing+=("$f")
done
if [[ ${#missing[@]} -gt 0 ]]; then
  echo "error: $AGENT_DIR is missing ${missing[*]}." >&2
  echo "       Run update.sh on this server to build the agent, then try again." >&2
  exit 1
fi

# ---- where will the node reach this server? ----

if [[ -z $SERVER ]]; then
  ADDR="$(env_value WINTERMUTE_ADDR)"
  PORT="${ADDR##*:}"
  [[ -n $PORT && $PORT =~ ^[0-9]+$ ]] || PORT=8080
  HOSTNAME_FQ="$(hostname -f 2>/dev/null || hostname)"
  # http, not https: a server behind a reverse proxy is the case that needs the
  # override, and guessing https for one that is not produces an installer that
  # cannot reach the machine that served it. Better to be plainly wrong in the
  # direction the operator will notice.
  SERVER="http://$HOSTNAME_FQ:$PORT"
  GUESSED=1
else
  GUESSED=0
fi

if [[ ! $SERVER =~ ^https?://[A-Za-z0-9._-]+(:[0-9]{1,5})?$ ]]; then
  echo "error: --server must be http:// or https:// with a plain host and optional port." >&2
  echo "       Got: $SERVER" >&2
  echo "       The address is written into a script that runs as root on the node," >&2
  echo "       and the server refuses anything it cannot write there safely." >&2
  exit 1
fi

# ---- issue the token ----

# clients.sh writes which database it used to stderr and the token block to
# stdout, so this keeps the operator's view of the former while capturing the
# latter.
OUTPUT="$("$CLIENTS" add "$NAME" node)"
TOKEN="$(printf '%s\n' "$OUTPUT" | grep -o 'wm_[A-Za-z0-9_-]\+' | head -1)"
if [[ -z $TOKEN ]]; then
  echo "error: registered '$NAME' but could not find the token in the output:" >&2
  printf '%s\n' "$OUTPUT" >&2
  echo >&2
  echo "       The token is shown once. Revoke and retry:" >&2
  echo "         sudo $CLIENTS revoke $NAME" >&2
  exit 1
fi

EXTRA=""
[[ -n $STORE ]] && EXTRA=" --store $STORE"
[[ -n $RUNTIME ]] && EXTRA="$EXTRA --runtime $RUNTIME"

cat <<EOF

Registered node "$NAME".

Run this on that machine:

  curl -fsSL -H "Authorization: Bearer $TOKEN" \\
    $SERVER/api/v1/node-agent/install.sh \\
    | sudo sh -s -- --token "$TOKEN"$EXTRA

EOF

if [[ $GUESSED -eq 1 ]]; then
  cat <<EOF
The address above was guessed from this machine's hostname and WINTERMUTE_ADDR.
Check the node can reach it; pass --server if it cannot.

EOF
fi

cat <<EOF
That line carries a credential — it is this node's token, shown once and stored
here only as a hash. If it gets away from you, revoke and start again:

  sudo $CLIENTS revoke $NAME

The installer fetches the agent this server built, creates the service user,
writes /etc/wintermute/node.env, installs the systemd unit and starts it.
Re-run the same command later to update; the token can be left off then.

Afterwards, on the host:

  systemctl status wintermute-node     is it running
  journalctl -u wintermute-node -f     what it is saying

And here, if that machine serves models, name it in the backend declaration so
fit verdicts are computed against its hardware:

  { "name": "...", "kind": "llamacpp", "base_url": "...", "node": "$NAME" }
EOF
