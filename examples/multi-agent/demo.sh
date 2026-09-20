#!/usr/bin/env bash
# Multi-agent demo (roadmap §23): one machine, one toolchain, three agent
# sessions, zero runtime collisions.
#
# Works without Unity installed: the demo registers three `command` tools
# standing in for Unity workloads. With a real Unity install, swap the
# requirement forms shown in the comments.
set -euo pipefail

ARH="${ARH:-arh}"
STATE="$(mktemp -d)"
export ARH_STATE_DIR="$STATE"
CONFIG="$STATE/config.yaml"

cat > "$CONFIG" <<YAML
runtime:
  state_dir: $STATE
  max_sessions: 8
tools:
  gameplay-compiler:
    type: command
    executable: /bin/sh
  shader-validator:
    type: command
    executable: /bin/sh
  android-builder:
    type: command
    executable: /bin/sh
resources:
  gpu:
    mode: exclusive
  unity-license:
    mode: exclusive
  build-slot:
    mode: capacity
    capacity: 2
environment:
  isolate_home: true
  isolate_tmp: true
YAML
export ARH_CONFIG="$CONFIG"

echo "== state dir: $STATE"
echo "== starting daemon"
"$ARH" daemon start -d

cleanup() {
  "$ARH" daemon stop >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo
echo "== Agent A: gameplay code -> EditMode tests"
SESS_A=$("$ARH" session create --name agent-a | sed -n 's/^session \([^ ]*\) created.*/\1/p')
"$ARH" lease acquire --session "$SESS_A" --resource unity-license
"$ARH" exec gameplay-compiler.run --session "$SESS_A" -- \
  args="-c;echo [agent-a] compiling gameplay; sleep 1; echo [agent-a] tests: 12 passed" >/dev/null

echo "== Agent B: shader -> GPU lease -> PlayMode validation"
SESS_B=$("$ARH" session create --name agent-b | sed -n 's/^session \([^ ]*\) created.*/\1/p')
# Exclusive GPU: with A holding nothing on gpu this is instant; if another
# session held it, --wait would queue FIFO.
"$ARH" lease acquire --session "$SESS_B" --resource gpu --wait
"$ARH" exec shader-validator.run --session "$SESS_B" -- \
  args="-c;echo [agent-b] validating shaders on GPU; sleep 1" >/dev/null

echo "== Agent C: android plugin -> player build"
SESS_C=$("$ARH" session create --name agent-c | sed -n 's/^session \([^ ]*\) created.*/\1/p')
"$ARH" exec android-builder.run --session "$SESS_C" -- \
  args="-c;echo [agent-c] building Android player; sleep 1; echo [agent-c] build ok" >/dev/null

echo
echo "== status"
"$ARH" status
echo
echo "== leases (who held what)"
"$ARH" lease list
echo
echo "== runs"
"$ARH" run list
echo
echo "== session A logs"
"$ARH" logs "$("$ARH" run list --session "$SESS_A" | awk 'NR==2{print $1}')" --name stdout.log

echo
echo "== teardown: kill all three sessions"
for s in "$SESS_A" "$SESS_B" "$SESS_C"; do
  "$ARH" session kill "$s" --reason "demo done"
done

echo
echo "== post-mortem"
"$ARH" session list
"$ARH" lease list
"$ARH" port list
echo "3 agents | 1 machine | 1 shared toolchain | 0 runtime collisions"
