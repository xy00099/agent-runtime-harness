# Agent Runtime Harness — ROADMAP

> A local runtime broker that lets multiple AI coding agents safely share stateful development tools, devices, caches, licenses, and hardware.

## 1. Project Goal

Build a **local execution harness / runtime broker for AI coding agents**.

The project is NOT primarily a filesystem sandbox and NOT a coding agent itself.

Its core responsibility is to coordinate access to expensive, stateful, host-installed development tools such as:

- Unity
- Unreal Engine
- Xcode
- Android SDK / emulator
- iOS Simulator
- Blender
- compilers and build systems
- GPUs
- physical devices
- devkits
- licensed SDKs

Multiple agents should be able to work on independent code workspaces while safely sharing the same host machine and tool installations.

The runtime should provide:

- isolated execution sessions
- process ownership and cleanup
- resource leasing
- tool discovery
- controlled host-tool execution
- cache sharing
- artifact and log collection
- policy enforcement
- a thin MCP interface for agents

The core idea:

```text
Worktree isolates code.
Harness isolates execution.
MCP exposes controlled capabilities.
```

## 2. Problem Statement

Modern coding agents already handle static source isolation reasonably well using git worktrees, cloned workspaces, containers, or temporary directories.

The harder problem appears when agents need to execute stateful development tools.

Example:

```text
Agent A -> Unity project A
Agent B -> Unity project B
Agent C -> Unity project C
```

All three may need the same installed Unity editor:

```text
/Applications/Unity/Hub/Editor/6000.x/Unity.app
```

But they should NOT share mutable runtime state such as project Library directories, HOME, TMP, preferences, PID files, logs, ports, sockets, temporary build files, simulator state, or device ownership.

They may also compete for scarce resources such as Unity licenses, GPUs, physical devices, simulators, console devkits, exclusive TCP ports, and limited build slots.

Without a coordinating runtime, multiple agents can kill each other's processes, corrupt shared caches, race for ports, hold stale licenses, leave orphan processes, mutate host configuration, contend for devices, interfere with GUI tools, or produce nondeterministic test results.

This project solves that execution-layer coordination problem.

## 3. Project Positioning

Do NOT position this project as:

- Docker for AI agents
- another agent sandbox
- Unity MCP server
- multi-agent coding UI
- git worktree manager

Position it as:

> A host-native development runtime and resource broker for AI coding agents.

The runtime is responsible for safely mediating access to host tools.

MCP is only one frontend. Future frontends may include CLI, HTTP API, Python SDK, IDE integration, and CI runner integration.

## 4. High-Level Architecture

```text
                AI Coding Agents
        Claude / Codex / Gemini / Other
                       |
                       | MCP
                       v
              +-------------------+
              |   MCP Frontend    |
              |-------------------|
              | runtime.create    |
              | tool.execute      |
              | lease.acquire     |
              | session.inspect   |
              +---------+---------+
                        |
                        | RPC / internal API
                        v
            +-------------------------+
            |     Runtime Daemon      |
            |-------------------------|
            | Session Manager         |
            | Tool Registry           |
            | Process Supervisor      |
            | Resource Scheduler      |
            | Lease Manager           |
            | Environment Manager     |
            | Cache Manager           |
            | Artifact Collector      |
            | Policy Engine           |
            +-----------+-------------+
                        |
          +-------------+-------------+
          |             |             |
          v             v             v
       Unity          Xcode       Android SDK
       GPU           Simulator      Devices
       Unreal        Keychain       Emulator
```

## 5. Core Concepts

### 5.1 Tool

A host-installed executable or toolchain.

```yaml
tool:
  id: unity-6000.3
  type: unity
  executable: /Applications/Unity/Hub/Editor/6000.3/Unity.app/Contents/MacOS/Unity
  version: 6000.3
```

Tools should be treated as immutable shared resources whenever possible.

Examples include Unity, Unreal Engine, Xcode, Android SDK, JDK, and compiler toolchains.

### 5.2 Execution Session

The central abstraction of the project.

Each agent task executes inside a session.

```text
ExecutionSession
├── id
├── owner
├── workspace
├── environment
├── temp namespace
├── process group
├── tool leases
├── resource leases
├── cache namespace
├── logs
├── artifacts
└── lifecycle state
```

A session owns every process and resource created on behalf of an agent. When the session ends, the runtime must be able to clean up everything it owns.

### 5.3 Lease

A lease represents temporary ownership or permission to use a resource.

Examples:

```text
unity-license
gpu-0
ios-simulator-01
android-device-02
tcp-port-8100
physical-iphone-01
build-slot-01
```

Lease lifecycle:

```text
available
   |
acquire
   |
leased
   |
renew / release / expire
   |
available
```

Leases must survive agent crashes through daemon-side tracking.

### 5.4 Shared Resource

Safe to reuse across sessions:

- tool binaries
- SDK installations
- read-only assets
- package downloads
- build cache
- Unity Accelerator
- compiler cache

### 5.5 Isolated State

Must be session- or workspace-specific:

- HOME
- TMPDIR
- editor preferences
- process state
- sockets
- logs
- local config
- mutable project metadata
- PID files
- local IPC endpoints

### 5.6 Scarce Resource

Requires scheduling or explicit leasing:

- GPU
- Unity license
- simulator
- physical device
- devkit
- exclusive port
- limited build slot

## 6. Initial Scope

The first release should support:

- macOS first
- git worktree workspace model
- process supervision
- isolated HOME/TMP
- tool registry
- resource leases
- port allocation
- Unity adapter
- CLI frontend
- MCP frontend
- logs and artifact collection
- crash cleanup

Do NOT begin with full VM or container isolation. The first goal is host-native coordination.

## 7. Repository Structure

```text
agent-runtime/
├── README.md
├── ROADMAP.md
├── LICENSE
├── CMakeLists.txt
│
├── core/
│   ├── session/
│   ├── process/
│   ├── leases/
│   ├── scheduler/
│   ├── registry/
│   ├── environment/
│   ├── artifacts/
│   └── policy/
│
├── daemon/
│   ├── server/
│   ├── rpc/
│   └── persistence/
│
├── adapters/
│   ├── unity/
│   ├── xcode/
│   └── android/
│
├── frontends/
│   ├── cli/
│   └── mcp/
│
├── examples/
│   ├── unity/
│   └── multi-agent/
│
├── tests/
│   ├── unit/
│   ├── integration/
│   └── stress/
│
├── schemas/
│   ├── tools.schema.json
│   └── runtime.schema.json
│
└── docs/
    ├── architecture.md
    ├── tool-adapters.md
    ├── leases.md
    ├── sessions.md
    └── mcp.md
```

Exact language choice is implementation-dependent.

Recommended options:

- daemon/core: Rust, Go, or C++
- MCP frontend: any language with mature MCP support
- configuration: YAML or TOML
- RPC: local socket first

Prefer a language that makes process supervision and concurrency reliable.

## 8. Phase 0 — Architecture Spike

### Goal

Prove that multiple isolated execution sessions can launch the same host-installed executable without interfering with one another.

### Deliverables

Implement a prototype that:

- creates three execution sessions
- assigns each a unique HOME, TMPDIR, workspace, and process group
- launches a simple test executable from all three
- captures logs independently
- kills all child processes when a session terminates

### Acceptance Criteria

```text
Session A cannot see Session B temp files.
Session A cannot kill Session B processes through the runtime API.
Destroying Session A does not affect Session B.
All orphan children from Session A are terminated.
```

No Unity yet.

## 9. Phase 1 — Runtime Daemon

### Goal

Create the persistent daemon responsible for sessions and process ownership.

### SessionManager

Operations:

```text
createSession()
getSession()
listSessions()
terminateSession()
cleanupSession()
```

Session states:

```text
CREATED
PREPARING
READY
RUNNING
STOPPING
STOPPED
FAILED
```

### ProcessSupervisor

Must track:

- PID
- process group
- parent/child relationship
- start time
- stdout
- stderr
- exit code
- termination reason

Required behavior:

- graceful shutdown
- timeout
- forced kill
- recursive process cleanup
- orphan detection

### Acceptance Criteria

A daemon restart must not leave unmanaged processes.

On startup, the daemon should detect stale session metadata and clean up or reconcile orphaned processes.

## 10. Phase 2 — Tool Registry

### Goal

Represent host-installed development tools as managed resources.

Example configuration:

```yaml
tools:
  unity-6000:
    type: unity
    version: "6000.3"
    executable: /Applications/Unity/Hub/Editor/6000.3/Unity.app/Contents/MacOS/Unity

  java-17:
    type: java
    version: "17"
    executable: /Library/Java/JavaVirtualMachines/jdk-17/bin/java
```

### Required Features

- static configuration
- tool detection
- executable validation
- version query
- tool capability metadata
- per-tool environment variables

Commands:

```bash
runtime tool list
runtime tool inspect unity-6000
runtime tool doctor unity-6000
```

### Acceptance Criteria

The daemon can resolve:

```text
requires unity >= 6000.0
```

into a concrete installed tool.

## 11. Phase 3 — Resource Lease Manager

### Goal

Create generic resource coordination.

Resources should support:

```text
exclusive lease
shared lease
capacity-based lease
```

Examples:

```yaml
resources:
  gpu:
    mode: capacity
    capacity: 2

  unity-license:
    mode: exclusive

  ios-device-01:
    mode: exclusive
```

### Lease API

```text
acquire(resource, session)
renew(lease)
release(lease)
list()
```

Required properties:

```text
lease_id
resource_id
session_id
created_at
expires_at
state
```

### Important Behaviors

- timeout
- cancellation
- waiting queue
- cleanup on session death
- cleanup on daemon restart
- fair scheduling

### Acceptance Criteria

```text
Session A -> acquire device
Session B -> wait
Session A -> terminate
Session B -> automatically acquire
```

## 12. Phase 4 — Port and IPC Isolation

### Goal

Prevent agents from accidentally fighting over ports and local endpoints.

Provide:

```text
allocatePort()
allocatePortRange()
allocateSocketPath()
```

Example:

```bash
runtime port acquire --session abc
# => 42137
```

Expose resolved values as environment variables:

```text
AGENT_RUNTIME_PORT_DEBUG=42137
```

### Acceptance Criteria

Parallel sessions never receive the same exclusive port.

Ports are released automatically when the owning session terminates.

## 13. Phase 5 — Environment Isolation

### Goal

Provide lightweight host-native isolation.

Per-session:

```text
HOME
TMPDIR
XDG_CONFIG_HOME
XDG_CACHE_HOME
XDG_STATE_HOME
runtime-specific environment variables
```

Also support:

- allowlisted inherited variables
- secret redaction
- per-tool environment overrides

Example:

```yaml
environment:
  inherit:
    - PATH
    - LANG

  isolate:
    home: true
    temp: true

  deny:
    - AWS_SECRET_ACCESS_KEY
    - PROD_TOKEN
```

### Important

Do NOT claim full security isolation.

This is execution-state isolation, not a replacement for a VM security boundary.

## 14. Phase 6 — Workspace Integration

### Goal

Integrate git worktrees with runtime sessions.

Commands:

```bash
runtime workspace create --repo . --branch agent/foo
runtime session create --workspace <id>
```

Runtime responsibilities:

- create worktree
- assign to session
- prevent accidental cross-session deletion
- expose workspace path to tools
- cleanup when requested

Workspace lifetime should be separate from session lifetime.

```text
workspace survives
session is disposable
```

## 15. Phase 7 — Unity Adapter v0

### Goal

Use Unity as the first real heavy-tool integration.

The adapter should support:

```text
unity.version
unity.compile
unity.test.editmode
unity.test.playmode
unity.build
unity.run.batch
```

Example:

```bash
runtime exec unity.test \
  --session agent-42 \
  --mode editmode
```

Adapter responsibilities:

1. resolve Unity version
2. validate workspace
3. acquire required leases
4. prepare environment
5. allocate logs
6. launch Unity
7. monitor process
8. capture exit status
9. collect test reports
10. release resources

### Initial Unity Resources

Model at least:

```text
unity-process-slot
unity-license
gpu
```

Even if some are configured as shared.

## 16. Phase 8 — Unity Cache Strategy

### Goal

Avoid duplicating expensive generated state unnecessarily.

Keep per-workspace mutable state isolated.

Potential shared resources:

- Unity Accelerator
- package cache
- downloaded packages
- compiler cache

Do NOT directly share writable `Library/` directories across workspaces.

Support runtime configuration such as:

```yaml
unity:
  accelerator:
    enabled: true
    endpoint: 127.0.0.1:10080
```

Measure:

```text
cold import time
warm import time
disk usage
concurrent import behavior
```

Provide benchmark results in docs.

## 17. Phase 9 — Artifact Collection

### Goal

Every execution produces structured output.

```text
runs/
└── run-01/
    ├── metadata.json
    ├── stdout.log
    ├── stderr.log
    ├── unity.log
    ├── test-results.xml
    ├── screenshots/
    └── artifacts/
```

Common metadata:

```json
{
  "session": "agent-42",
  "tool": "unity-6000",
  "command": "test.editmode",
  "started_at": "...",
  "duration_ms": 12345,
  "exit_code": 0
}
```

Artifact model should be tool-agnostic.

## 18. Phase 10 — CLI Frontend

### Goal

Create a human-debuggable interface before MCP.

Core commands:

```bash
runtime daemon start

runtime session create
runtime session list
runtime session inspect
runtime session kill

runtime tool list
runtime tool doctor

runtime resource list
runtime lease list

runtime exec unity.test
runtime exec unity.build

runtime logs <run>
runtime artifacts <run>
```

The CLI should use the same daemon API as MCP.

No duplicated execution logic.

## 19. Phase 11 — MCP Frontend

### Goal

Expose controlled capabilities to coding agents.

MCP must remain thin.

Example MCP tools:

```text
runtime_create_session
runtime_close_session

runtime_list_tools
runtime_execute_tool

runtime_list_resources
runtime_acquire_resource
runtime_release_resource

runtime_get_logs
runtime_get_artifacts

unity_run_tests
unity_build
```

Avoid exposing raw unrestricted host-shell access through this MCP server.

The intended model is:

```text
Agent
  |
  | high-level request
  v
MCP
  |
  v
Runtime daemon
  |
  v
Policy + leases + supervision
```

## 20. Phase 12 — Policy Engine

### Goal

Ensure agents can only manipulate resources they own or are explicitly allowed to access.

Example policy:

```yaml
policies:
  - subject: agent
    allow:
      - session.create
      - tool.execute
      - own_process.kill
      - lease.acquire

    deny:
      - host_process.kill
      - tool.install.modify
      - other_session.modify
```

Important rule:

```text
Session A must never be allowed to kill Session B processes through the runtime API.
```

Possible future policy dimensions:

- tool allowlist
- resource allowlist
- max runtime
- max memory
- max concurrent processes
- max GPU slots
- filesystem roots
- network access

## 21. Phase 13 — Crash Recovery

### Goal

Make runtime ownership reliable.

Persist enough metadata to recover after daemon restart.

On daemon startup:

```text
load session state
scan owned processes
validate leases
reconcile resources
cleanup stale state
```

Use OS-native process identity carefully.

Do not rely only on PID because PIDs can be reused.

Store additional process identity metadata such as:

- process start time
- parent relationship
- command hash

## 22. Phase 14 — Observability

### Goal

Make multi-agent execution understandable.

Expose:

```text
sessions
running processes
resource leases
wait queues
tool executions
failures
cleanup events
```

CLI example:

```text
SESSION      TOOL       STATE      RESOURCE
agent-41     unity      running    gpu-0
agent-42     unity      waiting    gpu-0
agent-43     xcode      running    ios-sim-3
```

Optional later UI:

```text
Runtime Dashboard
├ Sessions
├ Tools
├ Resources
├ Wait Queue
└ Runs
```

Do not build dashboard before runtime core is stable.

## 23. Phase 15 — Multi-Agent Demo

Create the killer demo.

One machine. One Unity installation. Three coding-agent sessions.

```text
Agent A
-> modifies gameplay code
-> runs EditMode tests

Agent B
-> modifies shader
-> requests GPU
-> runs PlayMode validation

Agent C
-> modifies Android plugin
-> builds Android player
```

Runtime:

```text
Unity binary shared
workspace isolated
HOME isolated
TMP isolated
processes isolated
leases coordinated
logs separated
cache reused
```

README should show:

```text
3 agents
1 machine
1 Unity install
0 runtime collisions
```

## 24. v0.1 Scope

A v0.1 release is complete when all of the following work:

```text
[ ] persistent daemon
[ ] execution sessions
[ ] process supervision
[ ] isolated HOME/TMP
[ ] tool registry
[ ] generic resource leases
[ ] port allocation
[ ] workspace integration
[ ] Unity adapter
[ ] Unity test execution
[ ] Unity build execution
[ ] log collection
[ ] artifact collection
[ ] CLI
[ ] MCP frontend
[ ] session cleanup
[ ] daemon crash recovery
[ ] multi-agent Unity demo
```

## 25. Explicit Non-Goals for v0.1

Do NOT implement:

- full VM isolation
- Docker orchestration
- Kubernetes integration
- Windows support
- Linux support
- Unreal integration
- Xcode integration
- Android emulator management
- remote execution
- cloud scheduler
- multi-machine cluster
- full GUI dashboard
- arbitrary agent orchestration
- LLM inference
- agent planning
- prompt management
- source-code editing
- git merge automation
- generic CI platform

Keep v0.1 focused on local runtime coordination.

## 26. v0.2

Add a second major toolchain to validate the abstraction.

Preferred candidates:

```text
Xcode + iOS Simulator
or
Android SDK + emulator
```

Required features:

- simulator/device leases
- tool-specific resource discovery
- parallel test jobs
- artifact capture

If the abstraction only works for Unity, the design is too Unity-specific.

## 27. v0.3

Add advanced resource scheduling.

Potential features:

- lease priorities
- resource quotas
- weighted fairness
- job queues
- preemption
- reservation
- multi-resource transactions

Example:

```text
job requires:
  Unity
  GPU
  Android device
```

The scheduler should acquire the required set atomically or wait.

## 28. v0.4

Add remote execution.

```text
Agent
  |
Runtime Controller
  |
+---------+---------+
|                   |
Mac Worker       Windows Worker
```

Workers expose:

- tools
- resources
- device inventory
- available capacity

Do not add this before single-machine semantics are correct.

## 29. v1.0 Target

A stable v1.0 should provide:

- stable session API
- stable tool adapter API
- stable resource lease API
- reliable crash cleanup
- deterministic ownership model
- multiple production-quality adapters
- MCP + CLI
- local and remote execution
- documentation for third-party adapters

## 30. Tool Adapter Interface

Adapters should not directly own global runtime state.

Conceptual interface:

```text
ToolAdapter

detect()
validate()
capabilities()

prepare(session, request)
resources(request)
launch(session, request)
collect(run)
cleanup(run)
```

Example Unity adapter:

```text
UnityAdapter

detect Unity versions
resolve project path
request license/GPU resources
construct batchmode command
parse test results
collect Editor.log
```

## 31. Resource Provider Interface

Host resources should be pluggable.

```text
ResourceProvider

discover()
status()
acquire()
release()
healthcheck()
```

Examples:

```text
GPUProvider
PortProvider
IOSSimulatorProvider
AndroidDeviceProvider
LicenseProvider
```

## 32. Security Model

Be precise in documentation.

v0.x provides:

> controlled execution and state isolation

It does NOT necessarily provide:

> hostile-code security isolation

If an agent receives arbitrary shell access under the same OS user, it may still bypass the runtime.

True hostile isolation requires stronger primitives such as:

- separate OS users
- OS sandbox mechanisms
- containers
- VMs
- namespaces
- capability restrictions

The architecture should allow stronger isolation backends later.

## 33. Testing Strategy

### Unit Tests

Cover:

- session lifecycle
- lease lifecycle
- scheduler
- process state
- config parsing
- policy decisions

### Integration Tests

Run real subprocesses.

Test:

```text
process cleanup
session termination
port reuse
lease timeout
daemon restart
orphan recovery
```

### Stress Tests

Simulate:

```text
100 sessions
1000 lease requests
rapid create/destroy
daemon crash
resource contention
```

### Unity Integration Tests

At least:

```text
compile success
compile failure
EditMode test pass
EditMode test fail
PlayMode timeout
build success
build failure
forced session termination
```

## 34. Definition of Done for Every Runtime Feature

A feature is NOT done unless it includes:

- implementation
- unit tests
- failure-path tests
- cleanup behavior
- structured logs
- documentation
- CLI observability
- daemon restart considerations

Runtime infrastructure must treat failure handling as first-class behavior.

## 35. Implementation Order for AI Coding Agents

AI agents working on this repository should implement in this order:

```text
1. core data models
2. session manager
3. process supervisor
4. local daemon
5. CLI
6. tool registry
7. lease manager
8. port provider
9. environment isolation
10. workspace integration
11. generic adapter interface
12. Unity adapter
13. artifacts/logging
14. crash recovery
15. policy engine
16. MCP frontend
17. multi-agent demo
```

Do NOT start from the MCP server.

The daemon and runtime ownership model are the product.

## 36. Architectural Principles

1. MCP is a protocol adapter, not the core.
2. All resource ownership belongs to sessions.
3. Every acquired resource must have automatic cleanup.
4. Shared immutable state is good; shared mutable state is dangerous.
5. Prefer host-native tools over duplicating heavyweight toolchains.
6. Adapters translate tool semantics; they should not reimplement scheduling.
7. Agent crashes must not leak runtime state indefinitely.
8. Everything important must be observable from CLI.
9. Do not optimize for distributed execution before local semantics are correct.
10. Do not confuse lightweight state isolation with security isolation.

## 37. Example User Flow

```bash
runtime daemon start

runtime workspace create \
  --repo ~/game \
  --branch agent/payment-fix

runtime session create \
  --workspace payment-fix

runtime exec unity.test \
  --session sess-42 \
  --mode edit

runtime artifacts sess-42

runtime session close sess-42
```

MCP equivalent:

```text
create_session(workspace="payment-fix")

run_unity_tests(
    session="sess-42",
    mode="edit"
)

get_artifacts(session="sess-42")

close_session(session="sess-42")
```

## 38. Example Runtime Configuration

```yaml
runtime:
  state_dir: ~/.agent-runtime
  max_sessions: 8

tools:
  unity-6000:
    type: unity
    version: "6000.3"
    executable: /Applications/Unity/Hub/Editor/6000.3/Unity.app/Contents/MacOS/Unity

resources:
  gpu:
    provider: local-gpu
    capacity: 1

  unity-editor-slot:
    provider: semaphore
    capacity: 3

environment:
  isolate_home: true
  isolate_tmp: true

policy:
  max_processes_per_session: 32
  max_runtime_minutes: 60
```

## 39. Suggested README One-Liner

> Agent Runtime Harness is a local execution and resource broker that lets multiple AI coding agents safely share heavyweight development tools, devices, caches, licenses, and hardware.

Alternative:

> Run many coding agents on one development machine without letting Unity, Xcode, devices, ports, or background processes collide.

## 40. Success Criteria

The project is successful when a user can run:

```text
multiple agents
+
multiple isolated workspaces
+
one shared host toolchain
```

and obtain:

```text
no cross-session process interference
no resource collisions
automatic cleanup
reused expensive caches
structured logs
controlled tool execution
```

The first convincing demonstration should be:

```text
One Mac
One Unity installation
Three isolated agent sessions
Three simultaneous development tasks
Shared caches
Coordinated resources
Independent logs and artifacts
Clean teardown
```

That is the v0.1 product story.
