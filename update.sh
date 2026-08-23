#!/usr/bin/env bash
# Redeploys wintermute after a code change: pulls the latest commit, rebuilds
# both binaries, applies any pending migrations, and restarts the service. Run
# it on the host after pulling or pushing changes, or any time the working tree
# has commits the running service doesn't.
#
# Assumes scripts/setup.sh has already been run once, so the service user, env
# file, backends.json and systemd unit all exist.
set -euo pipefail

SERVICE_NAME="${WINTERMUTE_SERVICE_NAME:-wintermuted}"
SERVICE_USER="${WINTERMUTE_SERVICE_USER:-wintermute}"
ENV_FILE="${WINTERMUTE_ENV_FILE:-/etc/wintermute/wintermute.env}"
BIN_DIR="${WINTERMUTE_BIN_DIR:-/usr/local/bin}"
SERVER_BIN="$BIN_DIR/wintermuted"
CLIENT_BIN="$BIN_DIR/wintermute"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ ! -f "$ENV_FILE" ]; then
  echo "error: $ENV_FILE not found. Run scripts/setup.sh first." >&2
  exit 1
fi

# Reads one key from the env file, which is mode 600 and owned by the service
# user — hence sudo. A missing key is not an error here; the checks below
# report an unset value themselves, with the consequence attached.
env_value() {
  local raw
  raw="$(sudo grep -E "^$1=" "$ENV_FILE" 2>/dev/null | tail -1 | cut -d= -f2- || true)"
  # systemd strips one layer of surrounding quotes, so the value the service
  # actually sees is the unquoted one. Match that, or a quoted address turns
  # into a URL nothing answers on.
  case "$raw" in
    '"'*'"')
      raw="${raw#\"}"
      raw="${raw%\"}"
      ;;
    "'"*"'")
      raw="${raw#\'}"
      raw="${raw%\'}"
      ;;
  esac
  printf '%s\n' "$raw"
}

echo "==> Pulling latest changes"
# A working tree with no upstream is a normal state for this repo, and it is
# not a reason to refuse to rebuild what is already checked out.
if git -C "$REPO_ROOT" rev-parse --abbrev-ref --symbolic-full-name '@{upstream}' >/dev/null 2>&1; then
  git -C "$REPO_ROOT" pull --ff-only
else
  BRANCH="$(git -C "$REPO_ROOT" rev-parse --abbrev-ref HEAD)"
  echo "    branch '$BRANCH' has no upstream — skipping pull, building what is checked out"
fi
echo "    at $(git -C "$REPO_ROOT" rev-parse --short HEAD) $(git -C "$REPO_ROOT" log -1 --pretty=%s)"

echo "==> Building wintermuted and wintermute"
BUILD_DIR="$(mktemp -d)"
trap 'rm -rf "$BUILD_DIR"' EXIT
(cd "$REPO_ROOT" && go build -o "$BUILD_DIR/wintermuted" ./cmd/wintermuted)
(cd "$REPO_ROOT" && go build -o "$BUILD_DIR/wintermute" ./cmd/wintermute)
sudo install -m 0755 "$BUILD_DIR/wintermuted" "$SERVER_BIN"
sudo install -m 0755 "$BUILD_DIR/wintermute" "$CLIENT_BIN"
echo "    installed $SERVER_BIN and $CLIENT_BIN"

echo "==> Applying database migrations"
# As the service user: run as root, the database and its -wal/-shm files end up
# root-owned and the service fails on its first write.
DB_PATH="$(env_value WINTERMUTE_DB)"
if [ -z "$DB_PATH" ]; then
  echo "error: WINTERMUTE_DB is not set in $ENV_FILE." >&2
  exit 1
fi
sudo -u "$SERVICE_USER" env WINTERMUTE_DB="$DB_PATH" "$SERVER_BIN" -migrate-only

echo "==> Restarting $SERVICE_NAME"
sudo systemctl restart "$SERVICE_NAME"
sudo systemctl status "$SERVICE_NAME" --no-pager || true

# Everything below is reported on every update because none of it is visible
# otherwise. A wintermute that cannot reach its models still starts, still
# serves the UI, and still answers — it just fails at the first turn, and the
# reason is in a log nobody is tailing.
echo
echo "==> Post-restart checks"

LISTEN_ADDR="$(env_value WINTERMUTE_ADDR)"
LISTEN_ADDR="${LISTEN_ADDR:-:8080}"
PORT="${LISTEN_ADDR##*:}"
BASE_URL="http://127.0.0.1:$PORT"

if command -v curl >/dev/null 2>&1; then
  # The service takes a moment to bind, and probing instantly reports a
  # failure that fixes itself a second later.
  for _ in 1 2 3 4 5 6 7 8 9 10; do
    if curl -fsS -m 2 "$BASE_URL/api/v1/health" >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  if curl -fsS -m 2 "$BASE_URL/api/v1/health" >/dev/null 2>&1; then
    echo "    ✓ answering on $BASE_URL"
  else
    echo "    ✗ not answering on $BASE_URL/api/v1/health"
    echo "      sudo journalctl -u $SERVICE_NAME -n 50 --no-pager"
  fi
else
  echo "    • curl not installed, skipping the health check"
fi

# The single most consequential line in the env file. Unset, the server looks
# for ./backends.json in its working directory, doesn't find one, and quietly
# builds one backend from the environment instead — so a carefully written
# multi-backend file sits on disk doing nothing, and the only symptom is that
# the extra backends have vanished.
BACKENDS_FILE="$(env_value WINTERMUTE_BACKENDS)"
if [ -z "$BACKENDS_FILE" ]; then
  echo "    ✗ WINTERMUTE_BACKENDS is unset in $ENV_FILE."
  echo "      The server is falling back to a single backend built from the"
  echo "      environment, and any backends.json you wrote is being ignored."
  echo "      Add: WINTERMUTE_BACKENDS=/etc/wintermute/backends.json"
elif [ ! -f "$BACKENDS_FILE" ]; then
  echo "    ✗ WINTERMUTE_BACKENDS points at $BACKENDS_FILE, which does not exist."
fi

echo
echo "==> Model backends"

if [ -z "$BACKENDS_FILE" ] || [ ! -f "$BACKENDS_FILE" ]; then
  echo "    • no backend file to check"
elif ! command -v jq >/dev/null 2>&1 || ! command -v curl >/dev/null 2>&1; then
  echo "    • jq and curl are needed for this report. sudo apt install jq curl"
else
  # 401/403 still proves a server is there and speaking; it just wants the API
  # key. Treating that as unreachable would send people hunting a network fault
  # that doesn't exist.
  reachable() {
    local code
    code="$(curl -o /dev/null -s -m 3 -w '%{http_code}' "$1/models" 2>/dev/null || true)"
    case "$code" in
      200 | 401 | 403) return 0 ;;
      *) return 1 ;;
    esac
  }

  while IFS=$'\t' read -r name kind base_url key_env; do
    [ -n "$name" ] || continue
    case "$kind" in
      anthropic)
        # Nothing to probe: the failure mode here is a missing key, and that
        # only shows up on the first turn that uses this backend.
        key_env="${key_env:-ANTHROPIC_API_KEY}"
        if [ -z "$(env_value "$key_env")" ]; then
          echo "    ✗ $name (anthropic): $key_env is empty in $ENV_FILE — every turn on"
          echo "      this backend will fail. It is also the only backend kind that"
          echo "      sends your transcript off the local network."
        else
          echo "    ✓ $name (anthropic): key present. Note this backend leaves the network."
        fi
        ;;
      *)
        if [ -z "$base_url" ]; then
          echo "    ✗ $name ($kind): no base_url declared"
        elif reachable "$base_url"; then
          echo "    ✓ $name ($kind): reachable at $base_url"
        else
          echo "    ✗ $name ($kind): NOT reachable at $base_url"
          echo "      Turns pinned to it fail. Check the server is running and that the"
          echo "      URL includes the /v1 suffix."
        fi
        ;;
    esac
  done < <(jq -r '.backends[] | [.name, .kind, (.base_url // ""), (.api_key_env // "")] | @tsv' "$BACKENDS_FILE")

fi

echo
echo "Update complete."
