# Architecture

## The one-sentence version

The daemon owns every process, lease, port and artifact; frontends (CLI, MCP)
are thin protocol adapters over a JSON-RPC surface; adapters (command, unity)
translate tool semantics but never own global runtime state.

## Layers

```text
frontends (cli, mcp)            protocol only, zero execution logic
        |
rpc (JSON-RPC 2.0)              method table, wire views
        |
daemon.Service                  the facade: policy + wiring + recovery
        |
managers (session, supervisor, lease, ports, registry, runs, workspace)
        |
proc (platform primitives)      process groups (unix) / job objects (windows)
```

Dependency rule: managers never import the daemon; the daemon imports
managers. Adapters receive a `Runtime` bundle (leases / exec / ports / log)
instead of the service object, so adapters stay portable and testable.

## Sessions

A session is the ownership root. Everything created on behalf of an agent —
processes, leases, ports, run directories — points back to exactly one
session ID. Termination is a fixed pipeline:

1. `STOPPING` state
2. kill every supervised process tree (grace period, then force)
3. release all leases (queued waiters are granted automatically)
4. release all ports (held listeners closed)
5. finish non-terminal runs as `KILLED`
6. `STOPPED` state; remove the session runtime dir

The pipeline is idempotent: terminating a terminal session is a no-op.

## Process supervision

Launch path (`internal/supervisor`):

- stdout/stderr are opened as files *before* `Start` (no pipe deadlocks)
- the child goes through `proc.Start`, which applies the platform attribute:
  - unix: `setpgid` — the child leads its own process group
  - windows: `CREATE_NEW_PROCESS_GROUP` + assignment to a global Job Object
    with `JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE`
- the supervisor records `PID` + **process start-time identity** and a hash
  of the command line

Kill path:

- `KillTree` signals the whole process group (unix) or `taskkill /T /F`
  (windows), then waits out the grace period and force-kills stragglers
- the Job Object is the backstop: even if the daemon itself is killed, the
  OS closes the job handle and reaps every tree

PID-reuse defense (roadmap §21): recovery only reaps a persisted PID when the
recorded start time still matches the live process. A reused PID will not
match and is left alone — we may miss reaping one orphan, but we never kill
an innocent process.

## Leases

Single-mutex serialized state machine:

```text
available --acquire--> LEASED --release/expire--> available
                                (FIFO waiters granted head-first)
```

Modes: `exclusive` (capacity 1), `shared` (effectively unlimited),
`capacity` (N). Waiters are queued FIFO; a release/expire/session-death
grants the queue head. Waiters owned by a dying session are cancelled so
they cannot linger.

Persistence happens after every mutation, **outside** the manager lock
(snapshot under lock, save after unlock) — an earlier version deadlocked by
persisting under the lock.

## Ports

Allocation binds a loopback listener on `:0`, keeps the listener open for
the lifetime of the allocation (so the port cannot be stolen), and records
it in the table. Release closes the listener and drops the row. Because the
OS never hands the same bound port twice, parallel sessions can never
receive the same exclusive port.

## Recovery (daemon restart)

`Service.Recover` runs at startup:

1. reload persisted leases / ports / sessions / workspaces / runs
2. sessions found in non-terminal states were interrupted by the previous
   daemon exit → run the termination pipeline for each
3. reap leases/ports whose sessions no longer exist
4. reconcile persisted process records: kill still-alive orphans (identity
   checked), skip reused PIDs
5. finish non-terminal runs as `KILLED`
6. persist the reconciled picture

## Policy

`policy.Engine.Evaluate(subject, capability)`:

- a small set of capabilities is *always denied* (`host_process.kill`,
  `tool.install`, `tool.modify`, `other_session.modify`) — they are not
  exposed through the RPC surface at all
- explicit rules match by longest subject prefix; deny beats allow
- unruly subjects fall back to a built-in allowlist (local single-user
  posture; `daemon.admin` is denied by default)

Ownership is enforced beyond capabilities: releasing a lease requires the
calling session to own it; killing processes only ever touches the caller's
own session's trees.

## Wire contract

JSON-RPC 2.0, newline-delimited, over a unix socket (named pipe on Windows)
or TCP. Method names: `session.*`, `tool.*`, `resource.list`, `lease.*`,
`port.*`, `workspace.*`, `run.*`, `describe`. See `internal/rpc/methods.go`
for the authoritative wire views.
