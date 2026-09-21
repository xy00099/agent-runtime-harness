# Agent Runtime Harness

> A local execution and resource broker that lets multiple AI coding agents
> safely share heavyweight development tools, devices, caches, licenses and
> hardware.

**Run many coding agents on one development machine — without letting Unity,
Xcode, devices, ports or background processes collide.**

```
Worktree isolates code.
Harness isolates execution.
MCP exposes controlled capabilities.
```

[![CI](https://github.com/xy00099/agent-runtime-harness/actions/workflows/ci.yml/badge.svg)](https://github.com/xy00099/agent-runtime-harness/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/xy00099/agent-runtime-harness.svg)](https://pkg.go.dev/github.com/xy00099/agent-runtime-harness)

---

## Why

Modern coding agents handle source isolation well (git worktrees, cloned
workspaces, containers). The hard problem starts when agents must run
**stateful host tools**:

```text
Agent A → Unity project A
Agent B → Unity project B
Agent C → Unity project C          ← all three need the SAME installed editor
```

Without a coordinating runtime, agents kill each other's processes, corrupt
shared caches, race for ports, hold stale licenses, leak orphan processes,
mutate host config and produce nondeterministic test results.

Agent Runtime Harness fixes the **execution layer**: it is *not* Docker for
agents, *not* another sandbox, *not* a worktree manager, *not* a coding UI.
It is the runtime that mediates access to the machine.

## What it does

| Capability | How |
|---|---|
| **Execution sessions** | Every agent task runs in a session owning its processes, leases, ports and runs. Session dies → everything it owns is cleaned up. |
| **Process supervision** | Children launch into their own process group (unix) / kill-on-close Job Object (Windows). Recursive tree kill, timeouts, PID-reuse-safe identity (start time). |
| **Isolated HOME/TMP/XDG** | Per-session `HOME`, `TMPDIR`, `XDG_*` from an allowlisted environment; deny list for secrets. |
| **Resource leases** | `exclusive` / `shared` / `capacity` modes, FIFO wait queues, TTL expiry, automatic handoff when a session dies. |
| **Port allocation** | Exclusive TCP/UDP ports held by a live listener so nobody else can take them; released with the session. |
| **Tool registry** | Host tools as managed resources: config + auto-detection (Unity Hub), executable validation (`doctor`), version constraint resolution (`unity >= 6000.0`). |
| **Unity adapter** | Batchmode `-runTests` / `-executeMethod` / `-buildTarget` runs, editor log capture, NUnit3 test report parsing. |
| **Android adapter** | SDK/device discovery, signed APK builds (aapt2+d8+apksigner, no Gradle), session-isolated AVDs, supervised emulator boots, exclusive `adb:<serial>` device leases, screenshots. |
| **Generic command adapter** | Any host executable under the same supervision, leases and logging. |
| **Artifacts & logs** | Every run produces `runs/<id>/{metadata.json,stdout.log,stderr.log,log.txt,artifacts/}`. |
| **Crash recovery** | Daemon restart reloads persisted state, reconciles orphaned processes (start-time identity), releases leases/ports of dead sessions. |
| **Policy engine** | Capability allow/deny per caller subject; hard invariants (a session can never kill another session's processes). |
| **CLI + MCP** | Same daemon API through both frontends. MCP is a thin protocol adapter — no raw host-shell passthrough. |

## Quickstart

```bash
# 1. build (or: go install github.com/xy00099/agent-runtime-harness/cmd/arh@latest)
go build -o arh ./cmd/arh

# 2. start the daemon (detached)
./arh daemon start -d

# 3. register your first tool — edit ~/.agent-runtime/config.yaml:
#
#    tools:
#      my-tool:                 # any id you like
#        type: command          # run any executable
#        executable: /usr/bin/make
#
#    On Windows name the binary arh.exe (go build -o arh.exe ./cmd/arh).
#
#    Unity needs no executable (auto-detects the Hub); Android needs only
#    ANDROID_HOME/JAVA_HOME env — see the Configuration section below.

# 4. verify the registry picked it up
./arh tool list

# 5. one session per task
./arh session create --name agent-a --dir ~/projects/game
./arh session create --name agent-b --dir ~/projects/game-2

# give each session a private port
./arh port acquire --session sess-000001 --name debug
# => port 42137
#    export AGENT_RUNTIME_PORT_DEBUG=42137

# run a host tool under supervision (blocking). Pass tool arguments as
# --arg key=value (repeatable; values are word-split):
./arh exec my-tool.run --session sess-000001 --arg args=--version
# e.g. Windows cmd:  --arg "args=/c echo hi"   -> runs: cmd /c "echo hi"

# exclusive resource with a queue
./arh lease acquire --session sess-000001 --resource unity-license --wait

# observe everything
./arh status
./arh session procs sess-000001
./arh run list --session sess-000001
./arh logs run-000001 --name stdout.log
./arh artifacts run-000001

# clean teardown
./arh session kill sess-000001 --reason "task done"
```

### Workspaces (git worktrees)

```bash
./arh workspace create --repo ~/game --branch agent/payment-fix
# => workspace payment-fix at ~/.agent-runtime/worktrees/payment-fix-ws-000001

./arh session create --workspace payment-fix
./arh exec "unity >= 6000.0".test.editmode --session sess-000003
```

Workspace lifetime is separate from session lifetime: **workspaces survive,
sessions are disposable.**

## Use with AI agents (zero pasting)

The repo ships `AGENTS.md` — the agent-rules convention Claude Code, Codex
and friends pick up **automatically** from the repo root. Anyone who clones
this repo and opens it with an AI agent gets the correct workflow (sessions
→ tool runs → artifacts → close) with nothing to copy anywhere.

For the runtime to be reachable over MCP, add ONE server entry to your
client config (`claude_desktop_config.json` / Cursor mcp.json / …):

```json
{ "mcpServers": { "agent-runtime": { "command": "arh", "args": ["mcp"] } } }
```

Then the agent can do, end to end:

```text
runtime_create_session(name="payment-fix")
unity_run_tests(session="sess-1", mode="editmode")
android_build_apk(session="sess-1", src="app", package="com.x.y")
runtime_get_logs(run="run-000007")
runtime_get_artifacts(run="run-000007")
runtime_close_session(session="sess-1")
```

The MCP surface is thin and policy-checked — no raw host shell is exposed.
Agent behavior rules (when to lease, when to close, never kill processes)
live in [`AGENTS.md`](AGENTS.md); the MCP tool list is in [`docs/mcp.md`](docs/mcp.md).

## Configuration

`~/.agent-runtime/config.yaml` (or `ARH_CONFIG=…`):

```yaml
runtime:
  state_dir: ~/.agent-runtime
  max_sessions: 8

tools:
  unity-6000:
    type: unity
    version: "6000.3"
    executable: /Applications/Unity/Hub/Editor/6000.3/Unity.app/Contents/MacOS/Unity
  my-cmd-tool:
    type: command
    executable: /usr/local/bin/some-tool
    args: ["--verbose"]

resources:
  gpu:
    mode: exclusive            # exclusive | shared | capacity
  unity-editor-slot:
    mode: capacity
    capacity: 3
  build-slot:
    mode: capacity
    capacity: 2

environment:
  isolate_home: true
  isolate_tmp: true
  inherit: [PATH, LANG, LC_*]
  deny: [AWS_SECRET_ACCESS_KEY, PROD_TOKEN]

policy:
  max_processes_per_session: 32
  max_runtime_minutes: 60
  rules:
    - subject: mcp-restricted
      allow: [session.create, tool.list]
      deny: ["*"]

unity:
  default_version: ">= 6000.0"
  cache:
    accelerator_enabled: false
    accelerator_host: 127.0.0.1
    shared_package_cache: ~/.agent-runtime/caches/upm
```

## MCP tool surface

`arh mcp` serves the same daemon over stdio (see *Use with AI agents*
above for the one-line client config). Thin, policy-checked — never raw
shell:

```text
runtime_create_session      runtime_close_session      runtime_list_sessions
runtime_get_session         runtime_list_tools         runtime_execute_tool
runtime_list_resources      runtime_acquire_resource   runtime_release_resource
runtime_list_leases         runtime_allocate_port      runtime_get_logs
runtime_get_artifacts       runtime_list_runs
unity_run_tests             unity_build
android_list_devices        android_build_apk          android_run_tests
android_screenshot
```

Full tool schemas: [`docs/mcp.md`](docs/mcp.md). Agent behavior rules:
[`AGENTS.md`](AGENTS.md).

## Architecture

```text
                AI Coding Agents
        Claude / Codex / Gemini / Other
                       |
          CLI          |          MCP (stdio)
                       |
              +--------+--------+
              |  RPC (JSON 2.0) |   unix socket / named pipe, TCP fallback
              +--------+--------+
                       |
              +-------------------+
              |   Runtime Daemon  |
              |-------------------|
              | Session Manager   |  lifecycle, isolated dirs
              | Process Supervisor|  tree kill, identity, logs
              | Lease Manager     |  exclusive/shared/capacity, FIFO
              | Port Manager      |  held listeners
              | Tool Registry     |  config + detection, version resolve
              | Adapter Host      |  command / unity
              | Run Collector     |  artifacts, metadata
              | Policy Engine     |  capability allow/deny
              | Recovery          |  orphan reconcile, lease reap
              +-------------------+
                       |
        Unity        Xcode        Android SDK       GPU
        Simulators   Emulators    Devices           Licenses
```

Principles (from `ROADMAP.md`):

1. MCP is a protocol adapter, not the core.
2. All resource ownership belongs to sessions.
3. Every acquired resource has automatic cleanup.
4. Shared immutable state is good; shared mutable state is dangerous.
5. Agent crashes must not leak runtime state indefinitely.
6. Everything important is observable from the CLI.

## Android (v0.2)

The second toolchain validates the abstraction — same daemon, same lease
manager, same supervisor, only a new adapter:

```bash
# register the SDK (binaries derive from ANDROID_HOME; no executable needed)
# tools: { android-sdk: { type: android, env: { ANDROID_HOME: ..., JAVA_HOME: ... } } }

arh exec android-sdk.targets --session sess-1
arh exec android-sdk.build.apk --session sess-1 -- src=myproject package=com.example.app
arh lease acquire --session sess-1 --resource adb:emulator-5574   # exclusive device lease
arh exec android-sdk.install --session sess-1 -- serial=emulator-5574 apk=app.apk
arh exec android-sdk.screenshot --session sess-1 -- serial=emulator-5574
```

Device commands lease `adb:<serial>` **exclusively**: two sessions can never
touch the same device concurrently; a session death hands the serial to the
next waiter automatically. AVDs are created under the session's isolated
`ANDROID_AVD_HOME` (never `~/.android`), and emulator boots are supervised
services that die with the session. See `docs/android.md`.

## The multi-agent demo

```bash
cd examples/multi-agent
./demo.sh          # or demo.ps1 on Windows
```

One machine. One Unity installation. Three agent sessions.

```text
Agent A  modifies gameplay code   → EditMode tests
Agent B  modifies a shader        → GPU lease → PlayMode validation
Agent C  modifies Android plugin  → Android player build

Unity binary shared    workspace isolated     HOME isolated
TMP isolated           processes isolated     leases coordinated
logs separated         caches reused          clean teardown
```

README target: **3 agents, 1 machine, 1 Unity install, 0 runtime collisions.**

## Security model

v0.x provides **controlled execution and state isolation**. It does **not**
provide hostile-code security isolation: an agent with arbitrary shell access
under the same OS user can bypass the runtime. True hostile isolation needs
separate OS users, OS sandboxes, containers or VMs — the architecture allows
stronger isolation backends later. See `docs/security.md`.

## Repository layout

```text
AGENTS.md           agent rules — auto-loaded by coding agents, zero pasting
cmd/arh/            daemon + CLI + MCP entrypoint
internal/model/     core data model
internal/config/    YAML config + defaults
internal/store/     atomic JSON persistence
internal/session/   session lifecycle
internal/supervisor/ process supervision
internal/proc/      platform primitives (process groups / job objects)
internal/lease/     resource leases
internal/ports/     port allocation
internal/registry/  tool registry + version resolution
internal/env/       environment isolation
internal/workspace/ git worktrees
internal/adapter/   adapter contract + command + unity + android adapters
internal/runs/      run records + artifacts
internal/policy/    capability policy
internal/daemon/    the service facade + recovery
internal/rpc/       JSON-RPC transport + method table
internal/cli/       CLI frontend
internal/mcp/       MCP frontend
schemas/            JSON schemas for tool/runtime config
examples/           unity + multi-agent demos
docs/               architecture, sessions, leases, adapters, MCP, security
tests/              cross-cutting integration entrypoints
```

## Development

```bash
go build ./...
go vet ./... && go vet -tags integration ./...
go test ./...                              # unit tests
go test -tags integration ./internal/...   # real subprocesses, kills, recovery
```

Roadmap: `ROADMAP.md` (phases 0–15, v0.1 → v1.0). Status: **v0.2 complete** —
v0.1 (daemon, sessions, supervision, leases, ports, registry, command +
Unity adapters, artifacts, recovery, policy, CLI, MCP, multi-agent demo)
plus the Android adapter (device leases, SDK discovery, parallel test jobs,
artifact capture).

## License

Apache-2.0. See `LICENSE`.
