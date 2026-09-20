# Leases

Generic resource coordination (roadmap §11 / Phase 3).

## Modes

```yaml
resources:
  gpu:              { mode: exclusive }        # one holder
  unity-license:    { mode: exclusive }
  build-slot:       { mode: capacity, capacity: 2 }
  unity-process-slot: { mode: capacity, capacity: 2 }
  some-counter:     { mode: shared }           # unlimited holders
```

## Lifecycle

```text
             acquire                release / expire
available ───────────► LEASED ───────────────────────► available
                          │  ▲
                 queue    │  └─ auto-grant next waiter (FIFO)
session B waits ──────────┘
```

- **acquire** blocks (or queues with `wait: true`); a caller that does not
  want to queue gets a 30s ceiling by default
- **TTL**: leases with `ttl_seconds` expire on the sweep tick (15s) or when
  re-checked after a daemon restart
- **release** grants the queue head immediately
- **session death** releases everything the session held and cancels its
  queued waiters

## Fairness

One FIFO queue per resource. Head-of-queue is granted as soon as capacity
allows; nobody can jump the queue. Capacity modes admit multiple holders up
to N; exclusive is capacity 1.

## Persistence

Every mutation persists `leases.json` (snapshot under the lock, write after
unlock). On daemon restart:

- active leases whose session still exists and is non-terminal → the session
  was interrupted → termination pipeline releases them
- active leases whose session is gone entirely → reaped immediately
- expired-by-time leases → marked EXPIRED and freed

## The acceptance scenario

```text
Session A → acquire device      (LEASED)
Session B → wait                (queued)
Session A → terminate           (pipeline: release)
Session B → automatically acquire   (queue head granted)
```

Covered by `TestLeaseHandoffOnSessionDeath` in
`internal/daemon/service_test.go`.

## CLI

```bash
arh resource list
arh lease acquire --session sess-1 --resource gpu --ttl 600 --wait
arh lease list --session sess-1
arh lease release --session sess-1 --lease lease-000001
```

## RPC

```json
{"method": "lease.acquire", "params": {
  "owner": "cli:alice", "session_id": "sess-1",
  "resource_id": "gpu", "ttl_seconds": 600, "wait": true
}}
```

MCP: `runtime_acquire_resource` / `runtime_release_resource` /
`runtime_list_leases` / `runtime_list_resources`.
