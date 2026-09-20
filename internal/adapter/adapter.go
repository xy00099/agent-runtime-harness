// Package adapter defines the tool-agnostic adapter contract
// (roadmap §30). Adapters translate tool semantics; they never own global
// runtime state — the daemon provides sessions, leases, ports and policy.
package adapter

import (
	"context"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/model"
)

// ExecRequest describes one supervised child process to run on behalf of
// an adapter. The daemon launches it via the supervisor with the session's
// isolated environment, writes stdout/stderr under runDir, and enforces
// the timeout.
type ExecRequest struct {
	SessionID string
	RunID     string
	Label     string // e.g. "unity:test.editmode"
	Exe       string
	Args      []string
	Env       []string // full child env (session env + adapter additions)
	Dir       string
	RunDir    string // run artifact root; stdout.log/stderr.log land here
	Timeout   time.Duration
}

// ExecResult is the supervised outcome.
type ExecResult struct {
	ExitCode *int
	Status   model.RunStatus // SUCCEEDED / FAILED / TIMEOUT / KILLED / ERROR
	Duration time.Duration
	PID      int
	Note     string
}

// Runtime is the bundle of daemon capabilities an adapter may use.
type Runtime interface {
	// AcquireLease blocks until resourceID is leased to the session.
	// ttl 0 = no expiry. The lease is auto-released at session end.
	AcquireLease(ctx context.Context, sessionID, resourceID string, ttl time.Duration) (*model.Lease, error)
	// AllocatePort reserves an exclusive port for the session; released at
	// session end.
	AllocatePort(sessionID, protocol, name string) (int, error)
	// Exec launches one supervised child process and waits for it.
	Exec(ctx context.Context, req ExecRequest) (*ExecResult, error)
	// WorkspaceDir returns the session workspace path ("" if none).
	WorkspaceDir(sessionID string) string
	// Log writes a structured line into the run log (run dir log.txt).
	Log(runID, level, msg string)
}

// Request is a tool execution request resolved against a session.
type Request struct {
	Session *model.Session
	// Tool is the resolved registry entry.
	Tool *model.Tool
	// Command is the adapter subcommand, e.g. "test.editmode", "build".
	Command string
	// Args are free-form arguments from the caller (CLI flags / MCP args).
	Args map[string]any
	// SessionEnv is the session's isolated environment (base for children).
	SessionEnv []string
}

// Result is what an adapter produces for one execution.
type Result struct {
	ExitCode   *int
	Status     model.RunStatus
	Summary    *model.TestSummary
	ExtraFiles map[string]string // label -> path inside the run dir
	Note       string
}

// ToolAdapter is implemented by every tool type adapter.
type ToolAdapter interface {
	// Type is the tool type this adapter serves ("unity", "command").
	Type() string
	// Capabilities lists subcommands, e.g. "test.editmode".
	Capabilities() []string
	// Prepare validates the request and returns required resource IDs.
	Prepare(ctx context.Context, req *Request) (needed []string, err error)
	// Launch performs the execution through the Runtime bundle.
	Launch(ctx context.Context, req *Request, rt Runtime, runDir string) (*Result, error)
}
