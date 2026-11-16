#!/usr/bin/env bash
# Start a stock, disposable native PostgreSQL cluster as the current OS user.
# Use a normal unprivileged user. This intentionally refuses root execution.
set -euo pipefail

if [[ $(id -u) -eq 0 ]]; then
  printf '%s\n' 'Run this launcher as an unprivileged OS user, or supply a database in a normal container/VM.' >&2
  exit 1
fi

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
pg_bin=${PG_BIN:-}
pg_port=${PGPORT:-55440}
python_bin=${PYTHON:-python3}
if [[ ! $pg_port =~ ^[0-9]+$ ]] || (( pg_port < 1024 || pg_port > 65535 )); then
  printf '%s\n' 'PGPORT must be an unprivileged TCP port between 1024 and 65535.' >&2
  exit 1
fi
if [[ -n $pg_bin ]]; then
  initdb_bin=$pg_bin/initdb
  pg_ctl_bin=$pg_bin/pg_ctl
else
  initdb_bin=$(command -v initdb)
  pg_ctl_bin=$(command -v pg_ctl)
fi
if [[ ! -x $initdb_bin || ! -x $pg_ctl_bin ]]; then
  printf '%s\n' 'Install native PostgreSQL server tools or set PG_BIN to their bin directory.' >&2
  exit 1
fi

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/publisher-postgres.XXXXXX")
mkdir -- "$work_dir/socket" "$work_dir/evidence"
started=0
cleanup() {
  if [[ $started -eq 1 ]]; then
    "$pg_ctl_bin" -D "$work_dir/data" -m fast -w stop >> "$work_dir/postgres.log" 2>&1 || true
  fi
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Fail early if another process owns the chosen loopback port. PostgreSQL also
# fails its own bind if the port is taken between this check and startup.
"$python_bin" - "$pg_port" <<'PY'
import socket, sys
with socket.socket() as sock:
    sock.bind(("127.0.0.1", int(sys.argv[1])))
PY
"$initdb_bin" -D "$work_dir/data" -A trust --username=publisher --encoding=UTF8 --locale=C > "$work_dir/initdb.log" 2>&1
started=1
"$pg_ctl_bin" -D "$work_dir/data" -l "$work_dir/postgres.log" -o "-h 127.0.0.1 -p $pg_port -k $work_dir/socket" -w start
unset TEST_PGLITE
export TEST_DATABASE_URL="postgres://publisher@127.0.0.1:$pg_port/postgres?sslmode=disable"
export VERIFY_OUTPUT_DIR=${VERIFY_OUTPUT_DIR:-$work_dir/evidence}
if [[ $# -eq 0 ]]; then
  "$script_dir/native-postgres.sh"
else
  "$@"
fi
printf 'Disposable PostgreSQL logs: %s\n' "$work_dir"
