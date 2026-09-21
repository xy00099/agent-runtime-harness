# Agent rules for Agent Runtime Harness (arh)

> Coding agents: read this file before running any host tool.
> It is loaded automatically (AGENTS.md convention). No other setup needed.

## The one rule

**Host tools (Unity, Android SDK, compilers, emulators) never run through a
raw shell. They run through the `arh` daemon** — via the MCP tools below, or
the `arh` CLI. The daemon handles leases, supervision, timeouts, logging and
cleanup. Running tools directly defeats all of that.

## MCP tool flow — always in this order

```text
1. runtime_create_session        one session per task; name it after the task
2. runtime_execute_tool          every tool run goes through here
   ├─ Unity tests:    unity_run_tests(session, mode="editmode"|"playmode")
   ├─ Unity build:    unity_build(session, method="CIScript.BuildPlayer", target="Android")
   ├─ Android build:  android_build_apk(session, src=<dir>, package=<id>)
   ├─ Android tests:  android_run_tests(session, serial=<adb-serial>, cmd=<am instrument ...>)
   └─ anything else:  runtime_execute_tool(session, tool_id=<id>, command=<cmd>)
3. runtime_get_logs / runtime_get_artifacts     read REAL results from disk
4. runtime_close_session         ALWAYS, even on failure
```

## Rules

- **One session per task**, not per command. Retries stay in the same session.
- **Never acquire leases by hand** for a single tool run — tool runs lease
  what they need (`gpu`, `unity-license`, `adb:<serial>`) automatically.
  Hand-acquire (`runtime_acquire_resource`) only to hold a device across
  *multiple* runs.
- Exclusive resources queue FIFO. If `runtime_list_resources` shows
  `waiting > 0`, blocking is **correct behavior** — do not work around it.
- Tool failures return `isError: true` results: read the error, fix the
  cause, retry. Do not spawn a new session to retry.
- **Never kill processes yourself.** `runtime_close_session` kills the whole
  supervised tree, releases leases and ports. If a session wedged, close it.
- Read results from `runtime_get_logs` / `runtime_get_artifacts` — never
  guess outcomes from memory.
- If the MCP server errors with a dial failure: the daemon is not running.
  Tell the user to run `arh daemon start -d`.

## CLI equivalents (for debugging, same daemon)

```bash
arh status                                   # sessions / resources / queues
arh session list && arh session procs ID     # who is running what
arh run list --session ID                    # history
arh logs RUN --name stdout.log --tail 100    # any run output
arh artifacts RUN                            # test reports, APKs, screenshots
```
