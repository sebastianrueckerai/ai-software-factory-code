#!/usr/bin/env bash
# scripts/console-lab/run.sh [out] serves the console over its test fakes
# (TestLab in internal/console/lab_test.go) and screenshots every scenario
# in both themes into out, by default /tmp/claude-1000/console-lab/shots;
# then runs the structural check (check.js) and stops the lab. Run from
# anywhere; no database, cluster or network.
set -euo pipefail
repo=$(cd "$(dirname "$0")/../.." && pwd)
export CONSOLE_LAB_DIR=${CONSOLE_LAB_DIR:-/tmp/claude-1000/console-lab}
export CONSOLE_LAB_ADDR=${CONSOLE_LAB_ADDR:-127.0.0.1:8090}
export OUT=${1:-$CONSOLE_LAB_DIR/shots}
export BASE=http://$CONSOLE_LAB_ADDR
export NODE_PATH=${NODE_PATH:-/tmp/claude-1000/pw/node_modules}
export PLAYWRIGHT_BROWSERS_PATH=${PLAYWRIGHT_BROWSERS_PATH:-$HOME/.cache/ms-playwright}

mkdir -p "$CONSOLE_LAB_DIR"
rm -f "$CONSOLE_LAB_DIR/cookie.txt" "$CONSOLE_LAB_DIR/stop"
log=$CONSOLE_LAB_DIR/lab.log
(cd "$repo" && CONSOLE_LAB=1 exec go test -v -count=1 -run '^TestLab$' -timeout 0 ./internal/console/) >"$log" 2>&1 &
lab=$!
stop() {
  touch "$CONSOLE_LAB_DIR/stop"
  wait "$lab" || { echo "the lab failed; see $log" >&2; cat "$log" >&2; }
}
trap stop EXIT

for _ in $(seq 1 240); do
  [ -f "$CONSOLE_LAB_DIR/cookie.txt" ] && break
  kill -0 "$lab" 2>/dev/null || { cat "$log" >&2; exit 1; }
  sleep 0.5
done
[ -f "$CONSOLE_LAB_DIR/cookie.txt" ] || { echo "the lab did not start within 2 minutes; see $log" >&2; exit 1; }
node "$repo/scripts/console-lab/shoot.js"
# the structural check of the design round, after the shots so a red check still leaves them
node "$repo/scripts/console-lab/check.js"
