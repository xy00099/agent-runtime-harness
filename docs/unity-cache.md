# Unity Cache Strategy (Phase 8)

Goal: avoid duplicating expensive generated state while keeping
per-workspace mutable state isolated.

## The rule

**Never directly share a writable `Library/` directory across workspaces.**
`Library/` holds import artifacts, `Artifacts/` hashes, temp importer state —
concurrent editors corrupt it. Each workspace gets its own `Library/`.

## What IS safe to share

- the Unity **editor binary** (immutable install)
- **package cache** (UPM downloads): read-mostly, keyed by package+version
- **Unity Accelerator** cache: the whole point of Accelerator — it serves
  imported artifacts over the network with per-client isolation
- compiler caches that are content-addressed

## Configuration

```yaml
unity:
  cache:
    accelerator_enabled: true
    accelerator_host: 127.0.0.1     # Accelerator default port 10080
    shared_package_cache: ~/.agent-runtime/caches/upm
```

When the accelerator is enabled, the adapter sets `UNITY_CACHE_SERVER` for
Unity batch runs so import artifacts are fetched from the shared cache
instead of re-imported per workspace.

## What to measure (benchmark template)

```text
cold import time       first import of a project with an empty Library/
warm import time       same project, Library/ removed, accelerator primed
disk usage             per-workspace Library/ size vs accelerator size
concurrent imports     3 sessions importing simultaneously; verify no
                       corruption and sane wall time
```

Run it like:

```bash
arh session create --dir <unity-project-clone> --name cold
time arh exec "unity >= 6000.0".compile --session <id>
```

Results depend heavily on project size and disk speed — publish yours in
this doc when you have them. The structural guarantees (isolated Library/,
shared immutable binary, opt-in accelerator) do not depend on the numbers.
