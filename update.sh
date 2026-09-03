#!/usr/bin/env bash
# Redeploys wintermute after a code change: pulls the latest commit, rebuilds
# both binaries, applies any pending migrations, and restarts the service. Run
# it on the host after pulling or pushing changes, or any time the working tree
# has commits the running service doesn't.
#
# Assumes scripts/setup.sh has already been run once, so the service user, env
# file, backends.json and systemd unit all exist.
set -euo pipefail

# The test gate. It runs before anything is built or installed, because the
# point is to not replace a working server with a broken one.
#
# --skip-tests exists for the case the gate would otherwise make worse: the
# service is already down and this deploy is the fix. It is a deliberate word
# to type, not a default, and the script says loudly when it is used.
RUN_TESTS=1
for arg in "$@"; do
  case "$arg" in
    --skip-tests) RUN_TESTS=0 ;;
    -h|--help)
      echo "usage: ./update.sh [--skip-tests]"
      echo
      echo "  Rebuilds and reinstalls wintermute, then restarts the service."
      echo "  First it confirms the model repository's drive is mounted, since a"
      echo "  unit held to that mount turns the restart into a passphrase prompt"
      echo "  with no console to answer it. Then it checks the tree: gofmt, go"
      echo "  vet, the whole suite under the race detector, the Windows client"
      echo "  build and the app.js parse. Around forty seconds on a development"
      echo "  box, and a couple of times that on a slow one."
      echo
      echo "  --skip-tests omits the checks on the tree. It is for restoring a"
      echo "  server that is already down, not for saving time. The drive is"
      echo "  confirmed either way: a deploy that cannot reach it hangs rather"
      echo "  than failing, whichever way this was invoked."
      exit 0
      ;;
    *)
      echo "error: unknown argument '$arg' (try --help)" >&2
      exit 1
      ;;
  esac
done

SERVICE_NAME="${WINTERMUTE_SERVICE_NAME:-wintermuted}"
SERVICE_USER="${WINTERMUTE_SERVICE_USER:-wintermute}"
ENV_FILE="${WINTERMUTE_ENV_FILE:-/etc/wintermute/wintermute.env}"
BIN_DIR="${WINTERMUTE_BIN_DIR:-/usr/local/bin}"
SERVER_BIN="$BIN_DIR/wintermuted"
CLIENT_BIN="$BIN_DIR/wintermute"
# Where the built fleet agent is left for new hosts to install from. It has to
# match WINTERMUTE_NODE_AGENT_DIR in the env file, and the check below says so
# when it does not.
AGENT_DIR="${WINTERMUTE_NODE_AGENT_DIR:-/var/lib/wintermute/node-agent}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ ! -f "$ENV_FILE" ]; then
  echo "error: $ENV_FILE not found. Run scripts/setup.sh first." >&2
  exit 1
fi

# Reads one key from the env file, which is mode 600 and owned by the service
# user — hence sudo. A missing key is not an error here; the checks below
# report an unset value themselves, with the consequence attached.
#
# The question this answers is "what does the service actually see", so it
# reads the file the way systemd does rather than the way a shell would:
# whitespace before the name is allowed, the last assignment of a key wins, and
# a '#' partway along a line is part of the value rather than a comment — only
# one in the first column starts one. Being cleverer than systemd here would
# report a value the server is not running with, which is worse than reporting
# nothing at all.
env_value() {
  local raw
  raw="$(sudo grep -E "^[[:space:]]*$1=" "$ENV_FILE" 2>/dev/null | tail -1 | cut -d= -f2- || true)"
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

# The model repository is usually the one part of this installation on a drive
# of its own, and that drive is checked here — before the test gate, long
# before the restart — because of what its absence does to `systemctl restart`
# rather than what it does to the server.
#
# A unit carrying RequiresMountsFor= for the repository pulls that mount in
# when the service starts. On an encrypted volume the mount pulls in
# systemd-cryptsetup, which asks for the passphrase; a deploy has no console
# attached, so the request goes to a queue only `systemd-tty-ask-password-agent`
# reads and the restart sits there waiting for someone who does not know to run
# it. That is the difference between a deploy that fails and a deploy that
# hangs, and it costs one stat to tell in advance.
#
# The marker file is the test rather than findmnt, because it is the test the
# server itself applies: an absent drive leaves its mount point behind as an
# ordinary empty directory, and only the marker tells the two apart.
echo "==> Model repository"
MODEL_REPO="$(env_value WINTERMUTE_MODEL_REPO)"
if [ -z "$MODEL_REPO" ]; then
  echo "    • not configured, nothing to check"
else
  # RequiresMountsFor= names mount points, so the repository is normally a
  # directory inside one rather than the path itself.
  REQUIRED_MOUNTS="$(systemctl show "$SERVICE_NAME" -p RequiresMountsFor --value 2>/dev/null || true)"
  REPO_MOUNT=""
  for required in $REQUIRED_MOUNTS; do
    case "$MODEL_REPO/" in "${required%/}"/*) REPO_MOUNT="${required%/}" ;; esac
  done

  if sudo test -e "$MODEL_REPO/.wintermute-repo"; then
    echo "    ✓ $MODEL_REPO is mounted and initialised"
  elif [ -n "$REPO_MOUNT" ]; then
    {
      echo "error: the repository drive is not mounted — $MODEL_REPO carries no"
      echo "       .wintermute-repo marker, which is how the server itself tells an"
      echo "       absent drive from its empty mount point."
      echo
      echo "       $SERVICE_NAME.service has RequiresMountsFor=$REPO_MOUNT, so the"
      echo "       restart this script ends with would try to mount it. On an"
      echo "       encrypted volume that is a passphrase prompt with no console to"
      echo "       answer it, and the deploy hangs rather than failing. Refusing"
      echo "       before anything is built."
      echo
      echo "       Mount it, then run this again:"
      echo "         sudo mount $REPO_MOUNT"
      echo "       If an unlock is already queued and waiting for an answer:"
      echo "         sudo systemd-tty-ask-password-agent"
      echo "       A drive that should never ask on a deploy wants a key file in"
      echo "       /etc/crypttab instead of 'none'."
    } >&2
    exit 1
  else
    echo "    ✗ $MODEL_REPO carries no .wintermute-repo marker, so it is either an"
    echo "      unmounted drive or a directory that has never been initialised from"
    echo "      Admin → Repository. The server will still start; every download and"
    echo "      conversion will refuse."
  fi
fi

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

if [ "$RUN_TESTS" -eq 1 ]; then
  echo "==> Checking the tree before it replaces a running server"
  # gofmt first: it is instant, and a tree that is not formatted is a tree
  # somebody has not finished with.
  if ! unformatted="$(cd "$REPO_ROOT" && gofmt -l .)"; then
    echo "error: gofmt could not be run over $REPO_ROOT" >&2
    exit 1
  fi
  if [ -n "$unformatted" ]; then
    echo "error: these files are not gofmt'd, refusing to deploy:" >&2
    printf '%s\n' "$unformatted" | sed 's/^/    /' >&2
    echo "    run: gofmt -w ." >&2
    exit 1
  fi
  echo "    gofmt clean"

  (cd "$REPO_ROOT" && go vet ./...)
  echo "    go vet clean"

  # The whole suite, under the race detector, in one pass.
  #
  # The detector used to run on five hand-picked packages after a plain pass
  # over everything, because the whole tree under -race took about five minutes
  # and a gate slow enough to be resented is a gate that gets skipped. Almost
  # all of that was tests migrating throwaway databases and waiting on the disk
  # to flush them; now that they do not (see internal/store/storetest), the
  # whole tree under -race costs a few seconds more than those five packages
  # did. There is no longer a reason to choose.
  #
  # The plain pass went with the choice. It ran the same tests over the same
  # packages, so it was a strict subset of this one — nothing in the tree is
  # behind a `race` build tag, and if something ever is, put it back.
  (cd "$REPO_ROOT" && go test -race ./...)
  echo "    tests pass, no data races"

  # The client is cross-compiled for machines this one is not. A deploy that
  # breaks that build is only discovered when somebody tries to build it.
  (cd "$REPO_ROOT" && GOOS=windows GOARCH=amd64 go build -o /dev/null ./cmd/wintermute)
  echo "    windows client cross-compiles"

  # The browser UI is embedded in the server binary, so a syntax error in it
  # ships as a blank page rather than a build failure.
  if command -v node >/dev/null 2>&1; then
    node --check "$REPO_ROOT/internal/web/static/app.js"
    echo "    app.js parses"
  else
    echo "    node not installed — skipping the app.js syntax check"
  fi
else
  echo "==> Skipping checks (--skip-tests)"
  echo "    Deploying an unverified tree. This is the right call only when the"
  echo "    service is already down and this deploy is the fix."
fi

echo "==> Building wintermuted and wintermute"
BUILD_DIR="$(mktemp -d)"
trap 'rm -rf "$BUILD_DIR"' EXIT
(cd "$REPO_ROOT" && go build -o "$BUILD_DIR/wintermuted" ./cmd/wintermuted)
(cd "$REPO_ROOT" && go build -o "$BUILD_DIR/wintermute" ./cmd/wintermute)
sudo install -m 0755 "$BUILD_DIR/wintermuted" "$SERVER_BIN"
sudo install -m 0755 "$BUILD_DIR/wintermute" "$CLIENT_BIN"
echo "    installed $SERVER_BIN and $CLIENT_BIN"

# Built here rather than fetched from anywhere: the agent a node installs is
# compiled from the same commit as the server it will report to, on the one
# machine that is guaranteed to have the toolchain. Both architectures, because
# a home fleet is usually a mix of an x86 box with the GPU and an ARM board or
# two, and cross-compiling costs nothing with no cgo in the tree.
echo "==> Building the fleet agent for new nodes"
build_agent() {
  local arch="$1"
  (cd "$REPO_ROOT" && GOOS=linux GOARCH="$arch" go build -o "$BUILD_DIR/wintermute-node.$arch" ./cmd/wintermute-node)
}
build_agent amd64
build_agent arm64
sudo mkdir -p "$AGENT_DIR"
sudo install -m 0644 "$BUILD_DIR/wintermute-node.amd64" "$AGENT_DIR/wintermute-node.amd64"
sudo install -m 0644 "$BUILD_DIR/wintermute-node.arm64" "$AGENT_DIR/wintermute-node.arm64"
sudo install -m 0644 "$REPO_ROOT/deploy/wintermute-node.service" "$AGENT_DIR/wintermute-node.service"
sudo install -m 0644 "$REPO_ROOT/deploy/wintermute-node.env.example" "$AGENT_DIR/node.env.example"
# The node's end of the update. It goes out with the binary so a host that has
# the agent can pull the next one without a token being found again.
sudo install -m 0644 "$REPO_ROOT/deploy/wintermute-node-update.sh" "$AGENT_DIR/wintermute-node-update.sh"
# Names relative to the directory, so the installer's awk match is on the file
# name alone rather than on wherever this happened to build.
(cd "$AGENT_DIR" && sudo sh -c 'sha256sum wintermute-node.amd64 wintermute-node.arm64 wintermute-node.service wintermute-node-update.sh node.env.example > SHA256SUMS')
sudo chmod 0644 "$AGENT_DIR/SHA256SUMS"
echo "    built linux/amd64 and linux/arm64 into $AGENT_DIR"

CONFIGURED_AGENT_DIR="$(env_value WINTERMUTE_NODE_AGENT_DIR)"
if [ -z "$CONFIGURED_AGENT_DIR" ]; then
  echo "    note: WINTERMUTE_NODE_AGENT_DIR is not set in $ENV_FILE, so the server"
  echo "          will not offer the install script. Add:"
  echo "            WINTERMUTE_NODE_AGENT_DIR=$AGENT_DIR"
elif [ "$CONFIGURED_AGENT_DIR" != "$AGENT_DIR" ]; then
  echo "    warning: built into $AGENT_DIR but the server reads $CONFIGURED_AGENT_DIR;"
  echo "             new nodes would install a stale agent. Set WINTERMUTE_NODE_AGENT_DIR"
  echo "             to match, or re-run with WINTERMUTE_NODE_AGENT_DIR=$CONFIGURED_AGENT_DIR"
fi

echo "==> Checking the installed unit against the tree"
# Nothing here rewrites /etc/systemd/system/$SERVICE_NAME.service, and that is
# deliberate: the installed unit belongs to the machine. It carries the lines
# the tree cannot know — ReadWritePaths= for a model repository on its own
# drive, RequiresMountsFor= for that drive, an ambient capability for a
# privileged port. scripts/setup.sh writes it once, and a redeploy that copied
# the tree's version over the top would drop every one of them silently.
#
# The price of that is that an edit to deploy/wintermuted.service reaches this
# host by no route at all, and until now nothing said so. So the two are
# compared and the difference is printed, in both directions.
#
# Comments, blank lines and the six directives setup.sh fills in per host are
# dropped first: they differ by construction, and a diff that is always noisy
# is a diff nobody reads.
unit_directives() {
  sed -E -e 's/[[:space:]]+$//' \
         -e '/^[[:space:]]*[#;]/d' \
         -e '/^[[:space:]]*$/d' \
         -e '/^(User|Group|EnvironmentFile|ExecStart|WorkingDirectory|StateDirectory)=/d' \
    | sort
}

INSTALLED_UNIT="/etc/systemd/system/$SERVICE_NAME.service"
if [ ! -r "$INSTALLED_UNIT" ]; then
  echo "    • $INSTALLED_UNIT is not readable, so there is nothing to compare."
else
  UNIT_DIFF="$(diff <(unit_directives <"$REPO_ROOT/deploy/wintermuted.service") \
                    <(unit_directives <"$INSTALLED_UNIT") || true)"
  if [ -z "$UNIT_DIFF" ]; then
    echo "    ✓ installed unit matches deploy/wintermuted.service"
  else
    echo "    • the installed unit and deploy/wintermuted.service have diverged."
    echo "      '<' is in the tree and not on this host; '>' is on this host and"
    echo "      not in the tree, which is usually a line this machine needs."
    printf '%s\n' "$UNIT_DIFF" | sed 's/^/        /'
    echo "      Neither is applied automatically. To take a line from the tree,"
    echo "      add it to $INSTALLED_UNIT by hand — copying the file over would"
    echo "      lose the host's own lines — then:"
    echo "        sudo systemctl daemon-reload"
  fi
fi

echo
echo "==> Applying database migrations"
# As the service user: run as root, the database and its -wal/-shm files end up
# root-owned and the service fails on its first write.
DB_PATH="$(env_value WINTERMUTE_DB)"
if [ -z "$DB_PATH" ]; then
  echo "error: WINTERMUTE_DB is not set in $ENV_FILE." >&2
  exit 1
fi
# Stopped first, rather than migrated under the running server and restarted
# afterwards. The old binary was serving from this database with statements
# prepared against the old schema, and leaving it there while the schema
# changed was a race with nothing holding it — one that has been harmless only
# because every migration so far has been additive. The cost is the
# migration's own duration added to the downtime, which is what the guarantee
# is worth.
sudo systemctl stop "$SERVICE_NAME"
if ! sudo -u "$SERVICE_USER" env WINTERMUTE_DB="$DB_PATH" "$SERVER_BIN" -migrate-only; then
  {
    echo "error: migrations failed, and $SERVICE_NAME is stopped."
    echo "       It stays stopped: a server answering against a half-applied"
    echo "       schema is worse than one that is down and says why."
    echo "       The new binary is already installed, so going back means going"
    echo "       back in the tree:"
    echo "         git -C $REPO_ROOT checkout <last good commit>"
    echo "         ./update.sh --skip-tests"
  } >&2
  exit 1
fi

echo "==> Starting $SERVICE_NAME"
sudo systemctl start "$SERVICE_NAME"
sudo systemctl status "$SERVICE_NAME" --no-pager || true

# Everything below is reported on every update because none of it is visible
# otherwise. A wintermute that cannot reach its models still starts, still
# serves the UI, and still answers — it just fails at the first turn, and the
# reason is in a log nobody is tailing.
echo
echo "==> Post-restart checks"

# Everything reported below is advisory but this: a backend that is down is not
# the deploy's fault and failing over it would only teach people to ignore the
# exit status. A server that does not answer is the one thing this script
# exists to produce, and until now it left nothing in the exit status at all —
# so anything running update.sh from a chain of commands could not tell a
# deploy from a rollback.
DEPLOY_OK=1

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
    DEPLOY_OK=0
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
        if [ -n "$(env_value "$key_env")" ]; then
          echo "    ✓ $name (anthropic): key present. Note this backend leaves the network."
        elif sudo systemctl cat "$SERVICE_NAME" 2>/dev/null |
             grep -qE "^Environment=(.*[[:space:]\"'])?$key_env="; then
          # systemctl cat includes drop-ins, which is where a key kept out of
          # the env file usually lives. Worth the second look: the env file is
          # not the only way in, and telling an operator their key is missing
          # when the service has it sends them hunting a fault that is not there.
          echo "    ✓ $name (anthropic): key set in a unit drop-in rather than"
          echo "      $ENV_FILE. Note this backend leaves the network."
        else
          echo "    ✗ $name (anthropic): $key_env is set neither in $ENV_FILE nor in a"
          echo "      unit drop-in, so unless it arrives as a systemd credential every"
          echo "      turn on this backend will fail. It is also the only backend kind"
          echo "      that sends your transcript off the local network."
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
if [ "$DEPLOY_OK" -eq 1 ]; then
  echo "Update complete."
else
  echo "Update finished, but $SERVICE_NAME is not answering — see above." >&2
  exit 1
fi
