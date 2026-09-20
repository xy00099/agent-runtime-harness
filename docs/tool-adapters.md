# Tool Adapters

The adapter contract (roadmap §30). Adapters translate tool semantics; they
never own global runtime state — scheduling, leases, supervision and policy
belong to the daemon.

## Interface

```go
type ToolAdapter interface {
    Type() string
    Capabilities() []string
    Prepare(ctx, req) (neededResources []string, err error)
    Launch(ctx, req, rt Runtime, runDir string) (*Result, error)
}
```

`Runtime` is the capability bundle the daemon hands the adapter:

```go
AcquireLease(ctx, sessionID, resourceID, ttl)   // auto-released at session end
AllocatePort(sessionID, protocol, name)          // auto-released at session end
Exec(ctx, ExecRequest) (*ExecResult, error)      // supervised, timeout-enforced
WorkspaceDir(sessionID) string
Log(runID, level, msg)
```

`Exec` gives the adapter a supervised child: stdout/stderr to the run dir,
start-time identity recorded, policy timeout enforced, tree kill on timeout.

## Built-in adapters

### command (type: `command`)

Runs any host executable under supervision with the session environment.

```yaml
tools:
  my-tool:
    type: command
    executable: /usr/local/bin/my-tool
    args: ["--verbose"]          # prefix args
    env: { MY_TOOL_MODE: "ci" }  # per-tool overrides (win over session env)
```

Commands: `run` (positional args via `args.args`), `version`
(`version_command`), `custom`.

### unity (type: `unity`)

Batchmode runs:

```text
Unity -batchmode -quit -nographics \
      -projectPath <workspace> \
      -logFile <runDir>/unity.log \
      [-runTests -testPlatform EditMode -testResults <runDir>/test-results.xml] \
      [-executeMethod CIScript.BuildPlayer -buildTarget Android]
```

Commands and the resources they need:

| Command | Resources |
|---|---|
| `version` | — |
| `compile` | `unity-process-slot` |
| `test.editmode` | `unity-process-slot`, `unity-license` |
| `test.playmode` | `unity-process-slot`, `unity-license`, `gpu` |
| `build` | `unity-process-slot`, `unity-license` |
| `run.batch` | `unity-process-slot`, `unity-license` |

Test reports (NUnit3 XML) are parsed into `{total, passed, failed, skipped,
inconclusive, duration_ms}` and collected into `artifacts/test-results`.

Editor resolution: config `tools:` entries → `unity.editors:` map → Unity Hub
auto-detection (`/Applications/Unity/Hub/Editor`,
`%LOCALAPPDATA%\Programs\Unity\Hub\Editor`). Requirements resolve through the
registry: `"unity >= 6000.0"` → highest installed version that satisfies.

## Adding an adapter

1. implement `ToolAdapter` in `internal/adapter/<type>/`
2. register it in `daemon.registerAdapters`
3. declare default resources in config `Normalize()` if the type needs them
4. add tests: prepare-validation, launch happy path, launch failure path,
   resource release on failure

Adapter responsibilities checklist (roadmap §15):

```text
resolve version → validate workspace → acquire leases → prepare environment
→ allocate logs → launch → monitor → capture exit → collect reports → release
```

The daemon already handles: lease lifecycle, supervision, timeouts, run
records, artifacts, recovery. Do not reimplement any of those in an adapter.
