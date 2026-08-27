#!/usr/bin/env bash
# First-time bare-metal setup for the wintermute server on this machine:
# checks prerequisites, detects a local model backend if one is running,
# creates the service user, writes the env file and backends.json, builds and
# installs both binaries, applies migrations, registers the first client
# tokens, and installs the systemd unit.
#
# Safe to re-run — an existing user, env file, backends.json, database or
# client is left alone rather than recreated. Client tokens are shown once at
# the end and stored only as hashes; nothing else records them, so capture them
# when they appear.
set -euo pipefail

SERVICE_NAME="${WINTERMUTE_SERVICE_NAME:-wintermuted}"
SERVICE_USER="${WINTERMUTE_SERVICE_USER:-wintermute}"
ENV_FILE="${WINTERMUTE_ENV_FILE:-/etc/wintermute/wintermute.env}"
BACKENDS_FILE="${WINTERMUTE_BACKENDS:-/etc/wintermute/backends.json}"
STATE_DIR="${WINTERMUTE_STATE_DIR:-/var/lib/wintermute}"
BIN_DIR="${WINTERMUTE_BIN_DIR:-/usr/local/bin}"
SERVER_BIN="$BIN_DIR/wintermuted"
CLIENT_BIN="$BIN_DIR/wintermute"
# Where the built fleet agent is left for new hosts to install from, over HTTP,
# with the token they were issued. Under the state directory because it is
# build output the server serves, not configuration anyone edits.
AGENT_DIR="${WINTERMUTE_NODE_AGENT_DIR:-$STATE_DIR/node-agent}"
# Not the binary's own default of :8080 — that is llama-server's port, and on a
# host serving its own models the two collide.
LISTEN_ADDR="${WINTERMUTE_ADDR:-:8088}"
DB_PATH="$STATE_DIR/wintermute.db"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> Checking prerequisites"
if ! command -v go >/dev/null 2>&1; then
  echo "error: go is not installed. Install Go 1.25.12+ (https://go.dev/dl/) and re-run." >&2
  exit 1
fi
echo "    $(go version)"

if ! command -v systemctl >/dev/null 2>&1; then
  echo "error: systemd is required; this script installs a systemd unit." >&2
  exit 1
fi

if ! command -v curl >/dev/null 2>&1; then
  echo "    warning: curl not found — backend detection below is skipped, and"
  echo "             update.sh cannot report backend health. sudo apt install curl"
fi

# The hardware report shells out to nvidia-smi. Without it the server still
# runs; every VRAM fit estimate just comes back unknown, which looks like a
# broken feature rather than a missing tool.
if ! command -v nvidia-smi >/dev/null 2>&1; then
  echo "    note: nvidia-smi not found — GET /api/v1/system will report no GPU,"
  echo "          and model fit estimates will have no VRAM to measure against."
else
  echo "    $(nvidia-smi --query-gpu=name,memory.total --format=csv,noheader 2>/dev/null | head -1)"
fi

# Not required, but update.sh's per-backend report needs it.
if ! command -v jq >/dev/null 2>&1; then
  echo "    note: jq not found — update.sh will skip its per-backend report."
  echo "          sudo apt install jq"
fi

# A listener already on the chosen port means the service will fail to start,
# and the failure lands minutes later in journalctl rather than here.
PORT="${LISTEN_ADDR##*:}"
if command -v ss >/dev/null 2>&1 && ss -ltn "sport = :$PORT" 2>/dev/null | grep -q LISTEN; then
  echo "    warning: something is already listening on port $PORT."
  echo "             wintermuted will fail to bind. Set WINTERMUTE_ADDR and re-run,"
  echo "             or stop whatever holds the port."
fi

echo "==> Detecting a local model backend"
# Probing beats asking: whichever of these is already running is almost
# certainly the one this host is meant to use. 401/403 still proves a server is
# there and speaking — it just wants the API key that goes in the env file.
probe() {
  command -v curl >/dev/null 2>&1 || return 1
  local code
  code="$(curl -o /dev/null -s -m 3 -w '%{http_code}' "$1" 2>/dev/null || true)"
  case "$code" in
    200 | 401 | 403) return 0 ;;
    *) return 1 ;;
  esac
}

DETECTED_KIND=""
DETECTED_URL=""
if probe "http://127.0.0.1:11434/v1/models"; then
  DETECTED_KIND="ollama"
  DETECTED_URL="http://127.0.0.1:11434/v1"
  echo "    found an Ollama server at $DETECTED_URL"
elif probe "http://127.0.0.1:8080/v1/models"; then
  DETECTED_KIND="llamacpp"
  DETECTED_URL="http://127.0.0.1:8080/v1"
  echo "    found an OpenAI-compatible server at $DETECTED_URL"
else
  DETECTED_KIND="llamacpp"
  DETECTED_URL="http://127.0.0.1:8080/v1"
  echo "    none running — writing a starter entry for $DETECTED_URL."
  echo "    The server starts anyway and records the backend unreachable; see"
  echo "    docs/local-models.md to set one up, then: sudo systemctl restart $SERVICE_NAME"
fi

echo "==> Ensuring system user '$SERVICE_USER' exists"
if ! id "$SERVICE_USER" >/dev/null 2>&1; then
  sudo useradd --system --no-create-home --shell /usr/sbin/nologin "$SERVICE_USER"
  echo "    created system user '$SERVICE_USER'"
else
  echo "    system user '$SERVICE_USER' already exists"
fi

echo "==> Creating state directory $STATE_DIR"
# StateDirectory= in the unit would create this on first start, but migrations
# and client registration below run before the service has ever started.
sudo mkdir -p "$STATE_DIR"
sudo chown "$SERVICE_USER":"$SERVICE_USER" "$STATE_DIR"
sudo chmod 700 "$STATE_DIR"

echo "==> Writing environment file at $ENV_FILE"
sudo mkdir -p "$(dirname "$ENV_FILE")"
if [ ! -f "$ENV_FILE" ]; then
  sudo tee "$ENV_FILE" >/dev/null <<EOF_ENV
# Written by scripts/setup.sh. See deploy/wintermute.env.example for what each
# of these does and why. This is a systemd EnvironmentFile, not a shell script:
# values are taken literally, so do not quote them.

# Without this the server looks for ./backends.json relative to its working
# directory, does not find one, and falls back to a single backend built from
# the environment — so the file below would be silently ignored.
WINTERMUTE_BACKENDS=$BACKENDS_FILE

WINTERMUTE_ADDR=$LISTEN_ADDR
WINTERMUTE_DB=$DB_PATH

WINTERMUTE_LLM_TIMEOUT=10m
WINTERMUTE_LLM_MAX_TOKENS=16000
WINTERMUTE_MAX_TOOL_ITERATIONS=12

# Referenced by name from backends.json. Fill in whichever your backends use.
LLAMA_API_KEY=
ANTHROPIC_API_KEY=

HUGGINGFACE_TOKEN=

# Fleet telemetry, in its own database file. Remote hosts report here, and it is
# what lets a model be judged against the machine that would actually run it
# rather than against this one.
WINTERMUTE_METRICS_DB=$STATE_DIR/metrics.db

# The built agent a new host installs from, over HTTP with its own token:
#
#   curl -fsSL -H "Authorization: Bearer \$TOKEN" SERVER/api/v1/node-agent/install.sh | sudo sh -s -- --token "\$TOKEN"
#
# Filled by this script and by update.sh. Unset it to turn the install endpoints
# off entirely.
WINTERMUTE_NODE_AGENT_DIR=$AGENT_DIR
EOF_ENV
  echo "    wrote $ENV_FILE"
else
  echo "    $ENV_FILE already exists, leaving it alone"
fi
sudo chown "$SERVICE_USER":"$SERVICE_USER" "$ENV_FILE"
sudo chmod 600 "$ENV_FILE"

echo "==> Writing backend declaration at $BACKENDS_FILE"
if [ ! -f "$BACKENDS_FILE" ]; then
  sudo tee "$BACKENDS_FILE" >/dev/null <<EOF_BACKENDS
{
  "default": "local",
  "backends": [
    {
      "name": "local",
      "kind": "$DETECTED_KIND",
      "base_url": "$DETECTED_URL",
      "api_key_env": "LLAMA_API_KEY",
      "model": ""
    }
  ]
}
EOF_BACKENDS
  # No secrets live here — keys are referenced by variable name — so it is
  # readable, which is what lets update.sh report on it without sudo.
  sudo chmod 644 "$BACKENDS_FILE"
  echo "    wrote $BACKENDS_FILE (kind: $DETECTED_KIND)"
  echo
  echo "    Leave \"model\" empty only if the backend serves exactly one model."
  echo "    To add Claude as a per-conversation alternative, see docs/backends.md."
else
  echo "    $BACKENDS_FILE already exists, leaving it alone"
fi

echo "==> Building wintermuted and wintermute"
# Build as the invoking user so the build uses their module cache and
# credentials, then install with sudo, since $BIN_DIR is not user-writable.
BUILD_DIR="$(mktemp -d)"
trap 'rm -rf "$BUILD_DIR"' EXIT
(cd "$REPO_ROOT" && go build -o "$BUILD_DIR/wintermuted" ./cmd/wintermuted)
(cd "$REPO_ROOT" && go build -o "$BUILD_DIR/wintermute" ./cmd/wintermute)
sudo install -m 0755 "$BUILD_DIR/wintermuted" "$SERVER_BIN"
sudo install -m 0755 "$BUILD_DIR/wintermute" "$CLIENT_BIN"
echo "    installed $SERVER_BIN and $CLIENT_BIN"

# The fleet agent, for both architectures a home fleet is likely to hold. No
# cgo anywhere in this tree, so cross-compiling is just a pair of env vars —
# the same property that makes the Windows client a single build.
echo "==> Building the fleet agent for remote hosts"
(cd "$REPO_ROOT" && GOOS=linux GOARCH=amd64 go build -o "$BUILD_DIR/wintermute-node.amd64" ./cmd/wintermute-node)
(cd "$REPO_ROOT" && GOOS=linux GOARCH=arm64 go build -o "$BUILD_DIR/wintermute-node.arm64" ./cmd/wintermute-node)
sudo mkdir -p "$AGENT_DIR"
sudo install -m 0644 "$BUILD_DIR/wintermute-node.amd64" "$AGENT_DIR/wintermute-node.amd64"
sudo install -m 0644 "$BUILD_DIR/wintermute-node.arm64" "$AGENT_DIR/wintermute-node.arm64"
sudo install -m 0644 "$REPO_ROOT/deploy/wintermute-node.service" "$AGENT_DIR/wintermute-node.service"
sudo install -m 0644 "$REPO_ROOT/deploy/wintermute-node.env.example" "$AGENT_DIR/node.env.example"
(cd "$AGENT_DIR" && sudo sh -c 'sha256sum wintermute-node.amd64 wintermute-node.arm64 wintermute-node.service node.env.example > SHA256SUMS')
sudo chmod 0644 "$AGENT_DIR/SHA256SUMS"
echo "    built linux/amd64 and linux/arm64 into $AGENT_DIR"

echo "==> Applying database migrations"
# As the service user, so the database and its -wal/-shm files end up owned by
# the account that has to write them. Run as root, they would be root-owned and
# the service would fail on its first write with a permission error that points
# nowhere useful. Migrations need only WINTERMUTE_DB, not the whole config.
sudo -u "$SERVICE_USER" env WINTERMUTE_DB="$DB_PATH" "$SERVER_BIN" -migrate-only

echo "==> Registering client tokens"
EXISTING="$(sudo -u "$SERVICE_USER" env WINTERMUTE_DB="$DB_PATH" "$SERVER_BIN" -list-clients 2>/dev/null | tail -n +2 || true)"
if [ -z "$EXISTING" ]; then
  HARNESS_OUT="$(sudo -u "$SERVICE_USER" env WINTERMUTE_DB="$DB_PATH" "$SERVER_BIN" -add-client desktop -kind harness)"
  BROWSER_OUT="$(sudo -u "$SERVICE_USER" env WINTERMUTE_DB="$DB_PATH" "$SERVER_BIN" -add-client browser -kind browser)"
  HARNESS_TOKEN="$(printf '%s\n' "$HARNESS_OUT" | grep -oE 'wm_[A-Za-z0-9_-]+' | head -1)"
  BROWSER_TOKEN="$(printf '%s\n' "$BROWSER_OUT" | grep -oE 'wm_[A-Za-z0-9_-]+' | head -1)"
  echo "    registered 'desktop' (harness) and 'browser'"
else
  echo "    clients already registered, leaving them alone:"
  printf '%s\n' "$EXISTING" | sed 's/^/      /'
fi

echo "==> Installing systemd unit"
sudo cp "$REPO_ROOT/deploy/wintermuted.service" "/etc/systemd/system/$SERVICE_NAME.service"
sudo sed -i \
  -e "s|^User=.*|User=$SERVICE_USER|" \
  -e "s|^Group=.*|Group=$SERVICE_USER|" \
  -e "s|^EnvironmentFile=.*|EnvironmentFile=$ENV_FILE|" \
  -e "s|^ExecStart=.*|ExecStart=$SERVER_BIN|" \
  -e "s|^WorkingDirectory=.*|WorkingDirectory=$STATE_DIR|" \
  "/etc/systemd/system/$SERVICE_NAME.service"
sudo systemctl daemon-reload
sudo systemctl enable "$SERVICE_NAME"
echo "    installed and enabled $SERVICE_NAME.service"

if [ -n "${HARNESS_TOKEN:-}" ] || [ -n "${BROWSER_TOKEN:-}" ]; then
  echo
  echo "==> Client tokens (shown once here only — save them now)"
  echo "    Stored only as hashes. If you lose one, revoke and re-add the client."
  [ -n "${HARNESS_TOKEN:-}" ] && echo "    desktop harness: $HARNESS_TOKEN"
  [ -n "${BROWSER_TOKEN:-}" ] && echo "    browser UI:      $BROWSER_TOKEN"
fi

# The listen address may be :8088 rather than the 8080 in the README, so print
# the URL that actually applies rather than making anyone work it out.
HOST_URL="http://localhost${LISTEN_ADDR}"

echo
echo "Setup complete. Next steps:"
echo "  1. Start the service:  sudo systemctl start $SERVICE_NAME"
echo "  2. Check it came up:   sudo systemctl status $SERVICE_NAME"
echo "  3. Tail logs:          sudo journalctl -u $SERVICE_NAME -f"
echo "  4. Open the UI at $HOST_URL and paste the browser token."
echo
echo "  For the desktop harness on this machine:"
echo "     $CLIENT_BIN -init"
echo "     then set server_url to $HOST_URL and paste the harness token,"
echo "     or export WINTERMUTE_SERVER=$HOST_URL and WINTERMUTE_TOKEN=... instead."
echo
echo "  Redeploy after a code change with ./update.sh"
echo
