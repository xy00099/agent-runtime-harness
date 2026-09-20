# Sessions

The session is the central abstraction (roadmap §5.2). This document is the
behavioral contract.

## Layout

Each session materializes under `<state>/sessions/<id>/`:

```text
sessions/sess-000001/
├── home/          isolated HOME (HOME / USERPROFILE / APPDATA)
├── tmp/           isolated TMP / TEMP / TMPDIR
└── xdg/
    ├── config/    XDG_CONFIG_HOME
    ├── cache/     XDG_CACHE_HOME
    └── state/
        └── log/   XDG_STATE_HOME/log
```

Runs live elsewhere (`<state>/runs/<id>/`) so they survive session cleanup —
logs and artifacts are history, not session state.

## States

```text
CREATED → PREPARING → READY → RUNNING → STOPPING → STOPPED
                                              ↘ FAILED
```

- `READY`: dirs materialized, session usable
- `RUNNING`: at least one supervised process launched
- `STOPPED` / `FAILED`: terminal; idempotent cleanup; records persist for
  observability until pruned

## Ownership rules

- every lease row carries `session_id`; a session can only release its own
- every supervised process carries `session_id`; kill paths only ever touch
  the caller's session
- every port allocation carries `session_id`
- a session bound to a workspace records it; workspaces refuse removal while
  a live session uses them

## Guarantees

| Guarantee | Mechanism |
|---|---|
| Session A cannot see session B temp files | isolated TMPDIR trees |
| Session A cannot kill session B processes | supervisor keys by session; no cross-session kill API |
| Destroying A does not affect B | per-session kill pipeline; shared state is only read-mostly config |
| All orphan children of A are terminated | process group / job object tree kill + recovery reconcile |
| Resources held by A are freed when A dies | termination pipeline releases leases/ports; waiters auto-granted |
| A crashed agent does not leak state | daemon-side tracking survives agent death; TTL sweep expires stale leases |
| A crashed daemon does not leak processes | windows job objects; unix recovery kills orphans by identity |

## Environment

`env.Manager.Build` composes the child environment:

1. allowlisted inheritance from the daemon environment (exact names or
   `PREFIX_*` globs)
2. denylisted variables dropped (`AWS_SECRET_ACCESS_KEY`, …)
3. isolated roots override `HOME`, `TMPDIR`, `XDG_*` (originals preserved
   as `*_ORIG` for tools that explicitly need the real user home)
4. static `set:` entries applied last (never overriding the deny list)

This is **execution-state isolation**, not a security boundary — see
`docs/security.md`.
