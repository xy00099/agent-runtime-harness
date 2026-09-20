# MCP Frontend

MCP is a protocol adapter, not the core (roadmap §19). The daemon is the
product; MCP just exposes controlled slices of it.

## Running

```bash
arh mcp        # stdio server; dials the daemon at the configured endpoint
```

Client config (Claude Code / Claude Desktop / any MCP client):

```json
{
  "mcpServers": {
    "agent-runtime": {
      "command": "/usr/local/bin/arh",
      "args": ["mcp"]
    }
  }
}
```

The daemon must be running (`arh daemon start -d`). `arh mcp` fails fast
with a dial error otherwise.

## Tools

| Tool | Maps to | Notes |
|---|---|---|
| `runtime_list_tools` | `tool.list` | |
| `runtime_list_resources` | `resource.list` | with live usage/queue depth |
| `runtime_create_session` | `session.create` | isolated HOME/TMP/XDG |
| `runtime_close_session` | `session.terminate` | full cleanup pipeline |
| `runtime_list_sessions` | `session.list` | |
| `runtime_get_session` | `session.get` | state, dirs, processes |
| `runtime_acquire_resource` | `lease.acquire` | `wait: true` queues FIFO |
| `runtime_release_resource` | `lease.release` | ownership enforced |
| `runtime_list_leases` | `lease.list` | |
| `runtime_allocate_port` | `port.allocate` | exclusive, session-scoped |
| `runtime_execute_tool` | `run.execute` | supervised + leased + logged |
| `unity_run_tests` | `run.execute` | `test.editmode` / `test.playmode` |
| `unity_build` | `run.execute` | `build` with target+method |
| `runtime_get_logs` | `run.logs` | log.txt / stdout / stderr / unity |
| `runtime_get_artifacts` | `run.artifacts` | test reports, builds |
| `runtime_list_runs` | `run.list` | |

Protocol version: `2025-06-18`. Initialize → tools/list → tools/call.
Tool-execution failures come back as `isError: true` tool results (not
protocol errors), so agents can read and react to them.

## What is deliberately NOT exposed

- raw host shell
- host process kill (only session-scoped termination)
- tool install/modify
- touching another session's resources

`daemon.admin` is denied for MCP subjects by default; policy rules can
tighten further per subject prefix (`mcp-restricted` in the example config).

## Intended model

```text
Agent
  |  high-level request ("run editmode tests in workspace payment-fix")
  v
MCP  ── thin, schema'd, policy-checked
  v
Runtime daemon
  |
  v
Policy + leases + supervision + artifacts
```

An agent that needs arbitrary shell access should get it from its own
harness with its own sandbox — not through this server. The value here is
*coordinated, observable* execution, not another shell.
