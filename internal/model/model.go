// Package model defines the core data model of the Agent Runtime Harness.
//
// These types are pure data: no behavior, no I/O. They are shared by the
// daemon, the persistence layer, the RPC wire and both frontends (CLI, MCP).
package model

import "time"

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// SessionState is the lifecycle state of an execution session.
type SessionState string

const (
	SessionCreated   SessionState = "CREATED"
	SessionPreparing SessionState = "PREPARING"
	SessionReady     SessionState = "READY"
	SessionRunning   SessionState = "RUNNING" // has at least one live process
	SessionStopping  SessionState = "STOPPING"
	SessionStopped   SessionState = "STOPPED"
	SessionFailed    SessionState = "FAILED"
)

// Terminal reports whether the state is a terminal session state.
func (s SessionState) Terminal() bool {
	return s == SessionStopped || s == SessionFailed
}

// Session is the central abstraction: every process, lease, port and run
// created on behalf of an agent is owned by exactly one session.
type Session struct {
	ID           string            `json:"id"`
	Name         string            `json:"name,omitempty"`
	Owner        string            `json:"owner"` // caller identity, e.g. "cli:alice" or "mcp:agent-1"
	WorkspaceID  string            `json:"workspace_id,omitempty"`
	WorkspaceDir string            `json:"workspace_dir,omitempty"` // absolute path (worktree or plain dir)
	State        SessionState      `json:"state"`
	CreatedAt    time.Time         `json:"created_at"`
	StartedAt    *time.Time        `json:"started_at,omitempty"`
	StoppedAt    *time.Time        `json:"stopped_at,omitempty"`
	Failure      string            `json:"failure,omitempty"`
	HomeDir      string            `json:"home_dir"`      // isolated HOME
	TempDir      string            `json:"temp_dir"`      // isolated TMPDIR
	RuntimeDir   string            `json:"runtime_dir"`   // state/sessions/<id>: env dirs, pidfiles, socket paths
	Env          map[string]string `json:"env,omitempty"` // informational snapshot of overrides
}

// ---------------------------------------------------------------------------
// Processes
// ---------------------------------------------------------------------------

// ProcInfo is the supervisor's record for one supervised process tree.
type ProcInfo struct {
	PID        int        `json:"pid"`
	PPID       int        `json:"ppid"`
	StartTime  uint64     `json:"start_time"` // OS-native process start identity (see proc.Identity)
	Exe        string     `json:"exe"`
	Cmdline    []string   `json:"cmdline"`
	SessionID  string     `json:"session_id"`
	RunID      string     `json:"run_id,omitempty"`
	Label      string     `json:"label"` // e.g. "unity:test.editmode"
	StartedAt  time.Time  `json:"started_at"`
	ExitedAt   *time.Time `json:"exited_at,omitempty"`
	ExitCode   *int       `json:"exit_code,omitempty"`
	Killed     bool       `json:"killed,omitempty"`
	KillReason string     `json:"kill_reason,omitempty"`
	CmdHash    string     `json:"cmd_hash,omitempty"` // hash of cmdline, for crash-recovery identity
}

// ---------------------------------------------------------------------------
// Resources and leases
// ---------------------------------------------------------------------------

// LeaseMode describes how a resource can be shared.
type LeaseMode string

const (
	// ModeExclusive allows exactly one holder at a time.
	ModeExclusive LeaseMode = "exclusive"
	// ModeShared allows unlimited concurrent holders (best-effort).
	ModeShared LeaseMode = "shared"
	// ModeCapacity allows up to Capacity concurrent holders.
	ModeCapacity LeaseMode = "capacity"
)

// Resource is a schedulable host resource (GPU, license, device, port, slot).
type Resource struct {
	ID       string            `json:"id"`
	Mode     LeaseMode         `json:"mode"`
	Capacity int               `json:"capacity,omitempty"`
	Provider string            `json:"provider,omitempty"`
	Meta     map[string]string `json:"meta,omitempty"`
}

// LeaseState is the lifecycle state of a lease.
type LeaseState string

const (
	LeaseActive   LeaseState = "LEASED"
	LeaseReleased LeaseState = "RELEASED"
	LeaseExpired  LeaseState = "EXPIRED"
)

// Lease is a temporary ownership grant of a resource to a session.
type Lease struct {
	ID         string     `json:"id"`
	ResourceID string     `json:"resource_id"`
	SessionID  string     `json:"session_id"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  time.Time  `json:"expires_at,omitempty"` // zero = no expiry
	State      LeaseState `json:"state"`
	ReleasedAt *time.Time `json:"released_at,omitempty"`
}

// ---------------------------------------------------------------------------
// Tools
// ---------------------------------------------------------------------------

// Tool is a host-installed executable managed by the registry.
type Tool struct {
	ID             string            `json:"id"`
	Type           string            `json:"type"` // unity | command | generic | ...
	Version        string            `json:"version,omitempty"`
	Executable     string            `json:"executable"`
	VersionCommand []string          `json:"version_command,omitempty"` // args appended to exe; empty = skip
	Args           []string          `json:"args,omitempty"`            // prefix args (command adapter)
	Env            map[string]string `json:"env,omitempty"`
	Capabilities   []string          `json:"capabilities,omitempty"`
	Source         string            `json:"source"` // config | detected
	Disabled       bool              `json:"disabled,omitempty"`
	Meta           map[string]string `json:"meta,omitempty"`
}

// ---------------------------------------------------------------------------
// Runs (tool executions)
// ---------------------------------------------------------------------------

// RunStatus is the lifecycle state of a tool execution.
type RunStatus string

const (
	RunWaitingResources RunStatus = "WAITING_RESOURCES"
	RunQueued           RunStatus = "QUEUED"
	RunRunning          RunStatus = "RUNNING"
	RunSucceeded        RunStatus = "SUCCEEDED"
	RunFailed           RunStatus = "FAILED"
	RunTimeout          RunStatus = "TIMEOUT"
	RunKilled           RunStatus = "KILLED"
	RunError            RunStatus = "ERROR"
)

// Terminal reports whether the run reached a terminal state.
func (r RunStatus) Terminal() bool {
	switch r {
	case RunSucceeded, RunFailed, RunTimeout, RunKilled, RunError:
		return true
	}
	return false
}

// Run is one tool execution inside a session.
type Run struct {
	ID            string            `json:"id"`
	SessionID     string            `json:"session_id"`
	ToolID        string            `json:"tool_id"`
	ToolType      string            `json:"tool_type"`
	Command       string            `json:"command"`
	Args          []string          `json:"args,omitempty"`
	Status        RunStatus         `json:"status"`
	StartedAt     time.Time         `json:"started_at"`
	FinishedAt    *time.Time        `json:"finished_at,omitempty"`
	DurationMS    int64             `json:"duration_ms"`
	ExitCode      *int              `json:"exit_code,omitempty"`
	Dir           string            `json:"dir"`              // runs/<id>/ artifact root
	Leases        []string          `json:"leases,omitempty"` // lease IDs held for this run
	Env           map[string]string `json:"env,omitempty"`    // notable env snapshot
	Error         string            `json:"error,omitempty"`
	PID           int               `json:"pid,omitempty"`
	ProcStartTime uint64            `json:"proc_start_time,omitempty"`
	Attempt       int               `json:"attempt,omitempty"`
}

// TestSummary is the parsed result of a test run (Unity or otherwise).
type TestSummary struct {
	Suite        string `json:"suite,omitempty"`
	Total        int    `json:"total"`
	Passed       int    `json:"passed"`
	Failed       int    `json:"failed"`
	Skipped      int    `json:"skipped"`
	Inconclusive int    `json:"inconclusive"`
	DurationMS   int64  `json:"duration_ms"`
}

// ---------------------------------------------------------------------------
// Workspaces
// ---------------------------------------------------------------------------

// Workspace is a git worktree (or plain directory) bound to sessions.
// Workspace lifetime is independent of session lifetime.
type Workspace struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Repo          string    `json:"repo"`
	Branch        string    `json:"branch,omitempty"`
	Path          string    `json:"path"`
	CreatedAt     time.Time `json:"created_at"`
	LastSessionID string    `json:"last_session_id,omitempty"`
	Plain         bool      `json:"plain,omitempty"` // not a git worktree, just a directory
}
