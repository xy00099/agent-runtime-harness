// Package policy implements the policy engine (roadmap §20, §31 of model).
//
// Rules are evaluated per caller identity (subject). Every capability the
// daemon exposes has a name; a call is allowed only if some rule allows it
// for the caller and no rule denies it. The hard invariants (roadmap §20):
//
//   - A session can never kill another session's processes.
//   - Host tools can never be installed or modified through the runtime API.
//   - Default posture is deny-with-minimal-allow for unknown subjects.
package policy

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xy00099/agent-runtime-harness/internal/config"
	"github.com/xy00099/agent-runtime-harness/internal/model"
)

// Engine evaluates policy decisions.
type Engine struct {
	cfg    *config.Config
	rules  []config.PolicyRule
	sorted bool
}

// New builds an engine from config.
func New(cfg *config.Config) *Engine {
	return &Engine{cfg: cfg, rules: cfg.Policy.Rules}
}

// Capability names used across the daemon.
const (
	CapSessionCreate   = "session.create"
	CapSessionList     = "session.list"
	CapSessionGet      = "session.get"
	CapSessionKill     = "session.kill" // own sessions only, enforced separately
	CapExecTool        = "tool.execute"
	CapToolList        = "tool.list"
	CapToolInspect     = "tool.inspect"
	CapLeaseAcquire    = "lease.acquire"
	CapLeaseRelease    = "lease.release"
	CapLeaseList       = "lease.list"
	CapResourceList    = "resource.list"
	CapPortAllocate    = "port.allocate"
	CapPortRelease     = "port.release"
	CapWorkspaceCreate = "workspace.create"
	CapWorkspaceList   = "workspace.list"
	CapWorkspaceRemove = "workspace.remove" // own workspaces only, enforced separately
	CapRunList         = "run.list"
	CapRunGet          = "run.get"
	CapLogRead         = "log.read"
	CapDaemonAdmin     = "daemon.admin" // stop daemon
)

// DenyError is a policy denial with a user-facing reason.
type DenyError struct {
	Subject    string
	Capability string
	Reason     string
}

func (e *DenyError) Error() string {
	return fmt.Sprintf("policy: denied %s for subject %q: %s", e.Capability, e.Subject, e.Reason)
}

// defaultAllow is the built-in baseline for subjects without explicit rules.
// Roadmap §20 example policy, generalized to local single-user operation.
var defaultAllow = map[string]bool{
	CapSessionCreate: true, CapSessionList: true, CapSessionGet: true,
	CapSessionKill: true, CapExecTool: true, CapToolList: true, CapToolInspect: true,
	CapLeaseAcquire: true, CapLeaseRelease: true, CapLeaseList: true,
	CapResourceList: true, CapPortAllocate: true, CapPortRelease: true,
	CapWorkspaceCreate: true, CapWorkspaceList: true, CapWorkspaceRemove: true,
	CapRunList: true, CapRunGet: true, CapLogRead: true,
	CapDaemonAdmin: false,
}

// hostDangerous is always denied regardless of rules.
var alwaysDeny = map[string]bool{
	"host_process.kill":    true,
	"tool.install":         true,
	"tool.modify":          true,
	"other_session.modify": true,
}

// Evaluate reports whether subject may use capability.
// extra carries per-call context (e.g. session ownership) for deny rules
// that need it; currently unused but reserved.
func (e *Engine) Evaluate(subject, capability string) error {
	if alwaysDeny[capability] {
		return &DenyError{Subject: subject, Capability: capability, Reason: "capability is reserved and never exposed through the runtime API"}
	}
	// Longest matching subject prefix wins; "*" matches everything.
	best := -1
	var chosen *config.PolicyRule
	for i := range e.rules {
		r := &e.rules[i]
		if r.Subject == "*" || strings.HasPrefix(subject, strings.TrimSuffix(r.Subject, "*")) {
			if len(r.Subject) > best {
				best, chosen = len(r.Subject), r
			}
		}
	}
	if chosen != nil {
		for _, d := range chosen.Deny {
			if d == capability || d == "*" {
				return &DenyError{Subject: subject, Capability: capability,
					Reason: fmt.Sprintf("denied by rule for subject %q", chosen.Subject)}
			}
		}
		for _, a := range chosen.Allow {
			if a == capability || a == "*" {
				return nil
			}
		}
		// Explicit rule exists but does not allow: deny.
		return &DenyError{Subject: subject, Capability: capability,
			Reason: fmt.Sprintf("rule for subject %q does not allow this capability", chosen.Subject)}
	}
	// Baseline for unruly subjects.
	if defaultAllow[capability] {
		return nil
	}
	return &DenyError{Subject: subject, Capability: capability,
		Reason: "not in default allowlist; add an explicit policy rule"}
}

// MaxProcessesPerSession returns the configured process cap.
func (e *Engine) MaxProcessesPerSession() int { return e.cfg.Policy.MaxProcessesPerSession }

// MaxRuntimeMinutes returns the per-run default timeout in minutes.
func (e *Engine) MaxRuntimeMinutes() int { return e.cfg.Policy.MaxRuntimeMinutes }

// MaxConcurrentRuns returns the concurrent run cap.
func (e *Engine) MaxConcurrentRuns() int { return e.cfg.Policy.MaxConcurrentRuns }

// Describe renders the effective policy for CLI output.
func (e *Engine) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "limits:\n")
	fmt.Fprintf(&b, "  max_processes_per_session: %d\n", e.cfg.Policy.MaxProcessesPerSession)
	fmt.Fprintf(&b, "  max_runtime_minutes: %d\n", e.cfg.Policy.MaxRuntimeMinutes)
	fmt.Fprintf(&b, "  max_concurrent_runs: %d\n", e.cfg.Policy.MaxConcurrentRuns)
	if len(e.rules) == 0 {
		b.WriteString("rules: (built-in default allowlist)\n")
		return b.String()
	}
	subjects := make([]string, 0, len(e.rules))
	for _, r := range e.rules {
		subjects = append(subjects, r.Subject)
	}
	sort.Strings(subjects)
	b.WriteString("rules:\n")
	for _, r := range e.rules {
		fmt.Fprintf(&b, "  - subject: %s\n      allow: %v\n      deny:  %v\n", r.Subject, r.Allow, r.Deny)
	}
	return b.String()
}

// ValidateExecDir enforces that a run's working directory stays inside
// allowed roots: the session workspace, session runtime dir, or temp dir.
// Roadmap §20 "filesystem roots" dimension, kept intentionally simple.
func (e *Engine) ValidateExecDir(session *model.Session, dir string) error {
	if dir == "" {
		return nil
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("resolve dir: %w", err)
	}
	allowed := []string{session.WorkspaceDir, session.RuntimeDir, session.TempDir, session.HomeDir}
	for _, root := range allowed {
		if root == "" {
			continue
		}
		r, err := filepath.Abs(root)
		if err != nil {
			continue
		}
		if abs == r || strings.HasPrefix(abs, r+string(filepath.Separator)) {
			return nil
		}
	}
	return &DenyError{Subject: session.Owner, Capability: CapExecTool,
		Reason: fmt.Sprintf("working directory %s is outside the session roots (workspace/runtime/temp/home)", abs)}
}
