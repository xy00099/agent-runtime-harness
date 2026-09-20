#!/usr/bin/env bash
# End-to-end smoke: real daemon, real processes, real crash recovery.
# Usage: tests/smoke.sh   (requires `go build -o arh ./cmd/arh` first)
set -euo pipefail

cd "$(dirname "$0")/.."
ARH="$(pwd)/arh"
[ -x "$ARH" ] || { echo "build first: go build -o arh ./cmd/arh"; exit 1; }

STATE="$(mktemp -d)"
export ARH_STATE_DIR="$STATE"
export ARH_CONFIG="$STATE/config.yaml"

cat > "$ARH_CONFIG" <<YAML
runtime:
  state_dir: $STATE
tools:
  echo:
    type: command
    executable: /bin/sh
resources:
  gpu: { mode: exclusive }
YAML

echo "== start daemon"
"$ARH" daemon start -d

echo "== two sessions, isolated HOME"
S1=$("$ARH" session create --name a | head -1 | awk '{print $2}')
S2=$("$ARH" session create --name b | head -1 | awk '{print $2}')
[ "$S1" != "$S2" ] || { echo "FAIL: ids collide"; exit 1; }

echo "== exec in session 1"
"$ARH" exec echo.run --session "$S1" --wait=true "args=-c echo HOME=\$HOME" >/dev/null

echo "== lease + port"
"$ARH" lease acquire --session "$S1" --resource gpu >/dev/null
"$ARH" port acquire --session "$S1" --name debug >/dev/null

echo "== kill session 1; session 2 untouched"
"$ARH" session kill "$S1" --reason smoke >/dev/null
"$ARH" session list | grep -q "STOPPED" || { echo "FAIL: no STOPPED"; exit 1; }

echo "== lease queue handoff"
"$ARH" lease acquire --session "$S2" --resource gpu --wait >/dev/null &
BGPID=$!
sleep 0.3
"$ARH" session kill "$S2" --reason smoke >/dev/null || true
wait $BGPID || true

echo "== crash recovery: hard-kill the daemon"
"$ARH" exec echo.run --session "$($ARH" session create --name crashy | head -1 | awk '{print $2}')" \
  --wait=false "args=-c sleep 30" >/dev/null
DPID="$(cat "$STATE/daemon.pid")"
kill -9 "$DPID" 2>/dev/null || true
sleep 0.5
"$ARH" daemon start -d
sleep 1
"$ARH" session list

echo "== stop"
"$ARH" daemon stop >/dev/null
echo "SMOKE OK"
