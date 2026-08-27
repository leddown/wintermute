#!/usr/bin/env bash
# Manage wintermute client tokens: list, issue, revoke.
#
# `wintermuted` now resolves the database out of the service's env file itself
# and hands the -wal/-shm sidecars back after running under sudo, so the flags
# are safe to type directly — see cmd/wintermuted/database.go. This script is
# no longer load-bearing for that.
#
# It stays because it is still the better front door: it refuses a relative
# path and a missing file outright rather than opening what it is given, names
# which database it used, and runs as the service user instead of repairing
# ownership afterwards. Belt as well as braces, on the path that hands out
# credentials.
set -euo pipefail

SERVICE_USER="${WINTERMUTE_SERVICE_USER:-wintermute}"
ENV_FILE="${WINTERMUTE_ENV_FILE:-/etc/wintermute/wintermute.env}"
BIN="${WINTERMUTE_BIN:-/usr/local/bin/wintermuted}"

usage() {
  cat <<'EOF'
Usage: clients.sh <command> [args]

  list                      show registered clients, when created, last seen
  add <name> [kind]         register a client and print its token once
                            kind: harness (default), browser or node
  revoke <name>             remove a client; its token stops working at once
  delete <name>             alias for revoke

The browser UI wants a token like any other client. Kind is recorded for the
audit trail and nothing else, so a harness token will log the UI in just fine —
use `browser` anyway so `list` stays readable.

A fleet node authenticates as a client too, and the name given here is how that
machine is identified everywhere afterwards. `add-node.sh` is the shorter road:
it issues the token and prints the one command to run on the host.

Overrides: WINTERMUTE_ENV_FILE, WINTERMUTE_DB, WINTERMUTE_BIN,
WINTERMUTE_SERVICE_USER.
EOF
}

if [[ $# -eq 0 ]]; then
  usage
  exit 2
fi

COMMAND="$1"
shift

case "$COMMAND" in
-h | --help | help)
  usage
  exit 0
  ;;
list | add | revoke | delete) ;;
*)
  echo "error: unknown command '$COMMAND'" >&2
  usage >&2
  exit 2
  ;;
esac

if [[ $EUID -ne 0 ]]; then
  echo "error: run with sudo — the database is owned by the '$SERVICE_USER' user" >&2
  exit 1
fi

if [[ ! -x $BIN ]]; then
  echo "error: $BIN not found or not executable. Set WINTERMUTE_BIN." >&2
  exit 1
fi

# The env file is the authority on which database the server reads. Guessing
# here would reintroduce exactly the bug this script exists to prevent, so an
# explicit WINTERMUTE_DB wins, then the env file, and there is no third guess.
if [[ -n ${WINTERMUTE_DB:-} ]]; then
  DB="$WINTERMUTE_DB"
  DB_SOURCE="WINTERMUTE_DB in the environment"
elif [[ -r $ENV_FILE ]]; then
  DB="$(sed -n 's/^[[:space:]]*WINTERMUTE_DB=//p' "$ENV_FILE" | tail -1)"
  DB_SOURCE="$ENV_FILE"
  if [[ -z $DB ]]; then
    echo "error: $ENV_FILE sets no WINTERMUTE_DB." >&2
    echo "       Add it, or pass WINTERMUTE_DB=/path/to/wintermute.db." >&2
    exit 1
  fi
else
  echo "error: cannot read $ENV_FILE and WINTERMUTE_DB is unset." >&2
  echo "       Set WINTERMUTE_ENV_FILE or WINTERMUTE_DB." >&2
  exit 1
fi

if [[ $DB != /* ]]; then
  echo "error: WINTERMUTE_DB ($DB) is a relative path, resolved from '$DB_SOURCE'." >&2
  echo "       That is the trap this script exists to avoid. Use an absolute path." >&2
  exit 1
fi

# A missing file is not an error to SQLite — store.Open would create an empty
# one and every token issued into it would be invalid against the real server.
if [[ ! -f $DB ]]; then
  echo "error: no database at $DB (from $DB_SOURCE)." >&2
  echo "       Refusing to create one: a fresh database would take tokens and" >&2
  echo "       reject every one of them. Check the path, or run scripts/setup.sh." >&2
  exit 1
fi

if ! id "$SERVICE_USER" >/dev/null 2>&1; then
  echo "error: no such user '$SERVICE_USER'. Set WINTERMUTE_SERVICE_USER." >&2
  exit 1
fi

# Run as the service user, never as root: SQLite writes -wal and -shm beside the
# database, and root-owned sidecars leave the service unable to write to it.
run_as_service() {
  sudo -u "$SERVICE_USER" env WINTERMUTE_DB="$DB" "$BIN" "$@"
}

echo "database: $DB (from $DB_SOURCE)" >&2

case "$COMMAND" in
list)
  run_as_service -list-clients
  ;;

add)
  if [[ $# -lt 1 ]]; then
    echo "error: add needs a client name" >&2
    exit 2
  fi
  NAME="$1"
  KIND="${2:-harness}"
  if [[ $KIND != harness && $KIND != browser && $KIND != node ]]; then
    echo "error: kind must be 'harness', 'browser' or 'node', got '$KIND'" >&2
    exit 2
  fi
  run_as_service -add-client "$NAME" -kind "$KIND"
  ;;

revoke | delete)
  if [[ $# -lt 1 ]]; then
    echo "error: $COMMAND needs a client name" >&2
    exit 2
  fi
  NAME="$1"
  # Revoking deletes the row, so the name becomes free for reuse. There is no
  # undo and the old token cannot be recovered — it was only ever stored hashed.
  read -r -p "Revoke client '$NAME' from $DB? [y/N] " reply
  case "$reply" in
  y | Y | yes | YES) ;;
  *)
    echo "aborted"
    exit 1
    ;;
  esac
  run_as_service -revoke-client "$NAME"
  ;;
esac
