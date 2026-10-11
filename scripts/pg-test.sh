#!/usr/bin/env bash
# scripts/pg-test.sh <command...> runs the command beside a throwaway
# PostgreSQL 17 server: a temporary directory holds its data and its only
# listener, a unix socket; FACTORYD_TEST_DATABASE_URL and
# GATEWAY_TEST_DATABASE_URL point at the server's postgres database, whose
# superuser postgres needs no password. The server stops and the directory
# goes on every exit; the exit status is the command's. initdb and postgres
# refuse root: run as root, the script runs the server as nobody.
set -euo pipefail
if [ $# -eq 0 ]; then
  echo "usage: $0 <command...>" >&2
  exit 2
fi
PATH=/usr/lib/postgresql/17/bin:$PATH

cleanup() {
  local status=$?
  set +e
  [ -f "$dir/data/postmaster.pid" ] && "${as[@]}" pg_ctl -D "$dir/data" -m immediate stop >/dev/null
  rm -r "$dir"
  exit "$status"
}
as=()
dir=$(mktemp -d)
trap cleanup EXIT
if [ "$(id -u)" -eq 0 ]; then
  chown nobody "$dir"
  as=(runuser -u nobody --)
fi

"${as[@]}" initdb -D "$dir/data" -U postgres -A trust -E UTF8 --locale=C -N >/dev/null
cat >>"$dir/data/postgresql.conf" <<EOF
listen_addresses = ''
unix_socket_directories = '$dir'
fsync = off
EOF
if ! "${as[@]}" pg_ctl -D "$dir/data" -l "$dir/server.log" -w start >/dev/null; then
  cat "$dir/server.log" >&2
  exit 1
fi
echo "pg-test.sh: $(postgres --version) on $dir" >&2

export FACTORYD_TEST_DATABASE_URL="postgres:///postgres?host=$dir&user=postgres"
export GATEWAY_TEST_DATABASE_URL=$FACTORYD_TEST_DATABASE_URL
"$@"
