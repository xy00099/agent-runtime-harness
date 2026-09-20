# Security Model

Be precise about what this protects against. (Roadmap §32.)

## What v0.x provides

**Controlled execution and state isolation:**

- session-scoped `HOME` / `TMPDIR` / `XDG_*` so tools do not fight over
  preferences, caches and temp files
- exclusive resource leases so agents do not race for devices, licenses,
  GPUs or ports
- supervised process trees that are always cleaned up, even across daemon
  crashes
- a policy layer that refuses cross-session manipulation and never exposes
  host-level process management through the API
- structured logs and artifacts per run

## What v0.x does NOT provide

**Hostile-code security isolation.** If an agent receives arbitrary shell
access under the same OS user, it can:

- read and write any file the OS user can
- spawn processes outside the supervisor (nothing forces agents through the
  runtime)
- kill other processes with OS-level tools
- modify host configuration

The runtime's guarantees apply to what goes *through it*. It is a
**coordinator**, not a jailer.

## True hostile isolation requires

- separate OS users per agent
- OS sandbox mechanisms (macOS sandbox-exec / Seatbelt, Windows AppContainer)
- containers / VMs
- namespaces and capability restrictions

The architecture is deliberately shaped so a stronger isolation backend can
slot in later: every launch already flows through a single chokepoint
(`proc.Start` with its platform attribute), and sessions already own an
isolated environment spec. Replacing "own process group" with "namespace'd
container" is a backend change, not an API change.

## Operational notes

- the RPC endpoint is a local socket (named pipe on Windows) or loopback
  TCP; it is not a network service and should not be exposed remotely
- secrets: the environment deny list (`AWS_SECRET_ACCESS_KEY`, …) drops
  variables before children see them; the redact list masks values in any
  structured log output — this is hygiene, not confidentiality
- pidfiles and state live under `~/.agent-runtime` with default user
  permissions
