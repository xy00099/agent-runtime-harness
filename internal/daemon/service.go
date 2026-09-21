// Package daemon wires every runtime capability into one service object.
//
// The daemon is the product (roadmap §36): frontends are thin protocol
// adapters over this API. Nothing here knows about CLI flags, MCP or JSON.
package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/adapter"
	"github.com/xy00099/agent-runtime-harness/internal/adapter/android"
	"github.com/xy00099/agent-runtime-harness/internal/adapter/command"
	"github.com/xy00099/agent-runtime-harness/internal/adapter/unity"
	"github.com/xy00099/agent-runtime-harness/internal/config"
	"github.com/xy00099/agent-runtime-harness/internal/env"
	"github.com/xy00099/agent-runtime-harness/internal/lease"
	"github.com/xy00099/agent-runtime-harness/internal/model"
	"github.com/xy00099/agent-runtime-harness/internal/policy"
	"github.com/xy00099/agent-runtime-harness/internal/ports"
	"github.com/xy00099/agent-runtime-harness/internal/proc"
	"github.com/xy00099/agent-runtime-harness/internal/registry"
	"github.com/xy00099/agent-runtime-harness/internal/runs"
	"github.com/xy00099/agent-runtime-harness/internal/session"
	"github.com/xy00099/agent-runtime-harness/internal/store"
	"github.com/xy00099/agent-runtime-harness/internal/supervisor"
	"github.com/xy00099/agent-runtime-harness/internal/workspace"
)

// Service is the runtime daemon facade.
type Service struct {
	Cfg        *config.Config
	Policy     *policy.Engine
	EnvMgr     *env.Manager
	Registry   *registry.Registry
	Leases     *lease.Manager
	Ports      *ports.Manager
	Sessions   *session.Manager
	Workspace  *workspace.Manager
	Runs       *runs.Manager
	Supervisor *supervisor.Supervisor
	Store      *store.Store

	envSpecs map[string]*env.Spec // sessionID -> spec
	adapters map[string]adapter.ToolAdapter
	mu       sync.Mutex
	// stopOnce guards daemon shutdown.
	stopOnce sync.Once
}

// New builds the service from config, applying crash recovery.
func New(cfg *config.Config) (*Service, error) {
	if err := os.MkdirAll(cfg.Runtime.StateDir, 0o755); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	stateDir := filepath.Join(cfg.Runtime.StateDir, "state")
	st, err := store.New(stateDir)
	if err != nil {
		return nil, err
	}
	s := &Service{Cfg: cfg, Store: st}

	s.Policy = policy.New(cfg)
	s.EnvMgr = env.New(&cfg.Environment)
	s.Registry = registry.New(cfg)
	s.Registry.Detect()

	// Managers need each other only through explicit wiring below.
	lm, err := lease.New(cfg, st)
	if err != nil {
		return nil, err
	}
	s.Leases = lm
	s.Leases.SetPersistHook(func() {})

	pm, err := ports.New(st)
	if err != nil {
		return nil, err
	}
	s.Ports = pm
	s.Sessions = session.New(cfg, st)
	wsSeq := 0
	s.Workspace = workspace.New(st, func() string {
		wsSeq++
		return fmt.Sprintf("ws-%06d", wsSeq)
	})
	rm, err := runs.New(cfg.RunsDir())
	if err != nil {
		return nil, err
	}
	s.Runs = rm
	s.Supervisor = supervisor.New()
	s.envSpecs = map[string]*env.Spec{}
	s.registerAdapters()
	return s, nil
}

// registerAdapters is a no-op placeholder for adapter lookup by type.
func (s *Service) registerAdapters() {
	s.mu.Lock()
	s.adapters = map[string]adapter.ToolAdapter{
		"command": command.New(),
		"unity":   unity.New(cfgUnity(s.Cfg)),
		"android": android.New(),
	}
	s.mu.Unlock()
}

func cfgUnity(c *config.Config) *config.UnityConfig {
	if c.Unity != nil {
		return c.Unity
	}
	return &config.UnityConfig{}
}

// AdapterFor returns the adapter serving a tool type.
func (s *Service) AdapterFor(toolType string) (adapter.ToolAdapter, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.adapters[toolType]
	return a, ok
}

// ---------------------------------------------------------------------------
// Lifecycle
// ---------------------------------------------------------------------------

// Recover performs daemon-startup recovery (roadmap §21): reload persisted
// state, reconcile orphaned processes, release leases/ports of dead
// sessions, mark interrupted runs, and clean stale session dirs.
func (s *Service) Recover(ctx context.Context) error {
	if err := s.Leases.RestoreFromStore(); err != nil {
		return err
	}
	if err := s.Ports.RestoreFromStore(); err != nil {
		return err
	}
	if err := s.Sessions.RestoreFromStore(); err != nil {
		return err
	}
	if err := s.Workspace.RestoreFromStore(); err != nil {
		return err
	}
	if err := s.Runs.RestoreFromDisk(); err != nil {
		return err
	}
	// Sessions in non-terminal states at boot were interrupted by the
	// previous daemon exit: finalize them and reap their resources.
	interrupted := []*model.Session{}
	for _, sess := range s.Sessions.List() {
		if sess.State.Terminal() {
			continue
		}
		s.killSessionProcs(sess.ID, 3*time.Second)
		s.Leases.ReleaseAll(sess.ID)
		s.Ports.ReleaseBySession(sess.ID)
		s.Sessions.Finish(sess.ID, model.SessionStopped, "daemon restart: session interrupted")
		s.cleanupSessionDir(sess)
		interrupted = append(interrupted, sess)
	}
	// Only sessions that are alive AND running stay owned: interrupted
	// sessions must have their orphaned processes reaped (roadmap §21).
	live := map[string]bool{}
	for _, sess := range s.Sessions.List() {
		if sess.State.Terminal() {
			continue // interrupted: its processes are orphans now
		}
		live[sess.ID] = true
	}
	prevProcs := s.collectPreviousProcs()
	reaped := s.Supervisor.ReconcileOrphans(prevProcs, live)
	// Leases/ports owned by sessions that no longer exist at all.
	sessionIDs := map[string]bool{}
	for _, sess := range s.Sessions.List() {
		sessionIDs[sess.ID] = true
	}
	_ = s.Leases.ReapOrphans(sessionIDs)
	_ = s.Ports.ReapOrphans(sessionIDs)
	// Runs stuck in non-terminal states: mark killed.
	for _, r := range s.Runs.List("") {
		if !r.Status.Terminal() {
			_ = s.Runs.Finish(r.ID, model.RunKilled, nil, "daemon restart")
		}
	}
	// Persist the reconciled picture.
	_ = s.Leases.Persist()
	_ = s.Ports.Persist()
	_ = s.Sessions.Persist()
	if len(reaped) > 0 {
		s.logf("recovery: reaped %d orphaned process(es)", len(reaped))
	}
	return nil
}

// collectPreviousProcs rebuilds ProcInfo records from run metadata.
// (Runs persist the supervised pid + start time.)
func (s *Service) collectPreviousProcs() []*model.ProcInfo {
	var out []*model.ProcInfo
	for _, r := range s.Runs.List("") {
		if r.PID == 0 {
			continue
		}
		out = append(out, &model.ProcInfo{
			PID: r.PID, StartTime: r.ProcStartTime,
			SessionID: r.SessionID, Exe: r.ToolID, CmdHash: r.ID,
		})
	}
	return out
}

// Shutdown stops every live session (daemon stop, roadmap §9 acceptance).
func (s *Service) Shutdown(ctx context.Context) {
	s.stopOnce.Do(func() {
		for _, sess := range s.Sessions.List() {
			if sess.State.Terminal() {
				continue
			}
			_, _ = s.TerminateSession(ctx, sess.ID, "daemon shutdown")
		}
	})
}

// ---------------------------------------------------------------------------
// Sessions
// ---------------------------------------------------------------------------

// CreateSessionOpts is the RPC payload for session creation.
type CreateSessionOpts struct {
	Owner        string
	Name         string
	Workspace    string // workspace name
	WorkspaceDir string // explicit dir
}

// CreateSession materializes a session with isolated HOME/TMP/XDG.
func (s *Service) CreateSession(o CreateSessionOpts) (*model.Session, error) {
	if err := s.Policy.Evaluate(o.Owner, policy.CapSessionCreate); err != nil {
		return nil, err
	}
	wsDir := o.WorkspaceDir
	if wsDir == "" && o.Workspace != "" {
		if w, ok := s.Workspace.GetByName(o.Workspace); ok {
			wsDir = w.Path
			s.Workspace.MarkSession(w.ID, "pending")
		}
	}
	if wsDir != "" {
		if abs, err := filepath.Abs(wsDir); err == nil {
			wsDir = abs
		}
	}
	sess, err := s.Sessions.Create(session.CreateOptions{
		Owner: o.Owner, Name: o.Name,
		WorkspaceID: o.Workspace, WorkspaceDir: wsDir,
	})
	if err != nil {
		return nil, err
	}
	// Materialize the full isolated env spec.
	spec, err := s.EnvMgr.Prepare(sess.RuntimeDir)
	if err != nil {
		_, _ = s.TerminateSession(context.Background(), sess.ID, "env prepare failed")
		return nil, err
	}
	s.mu.Lock()
	s.envSpecs[sess.ID] = spec
	s.mu.Unlock()
	if o.Workspace != "" {
		if w, ok := s.Workspace.GetByName(o.Workspace); ok {
			s.Workspace.MarkSession(w.ID, sess.ID)
		}
	}
	s.logf("session %s created (owner=%s workspace=%s)", sess.ID, o.Owner, wsDir)
	return sess, nil
}

// GetSession returns a session.
func (s *Service) GetSession(id string) (*model.Session, error) {
	sess, ok := s.Sessions.Get(id)
	if !ok {
		if sess, ok = s.Sessions.GetByName(id); !ok {
			return nil, fmt.Errorf("unknown session %q", id)
		}
	}
	return sess, nil
}

// ListSessions returns all sessions.
func (s *Service) ListSessions() []*model.Session { return s.Sessions.List() }

// SessionProcs lists live processes of a session.
func (s *Service) SessionProcs(sessionID string) ([]*model.ProcInfo, error) {
	if _, err := s.GetSession(sessionID); err != nil {
		return nil, err
	}
	return s.Supervisor.SessionProcs(sessionID), nil
}

// killSessionProcs kills and returns the pids.
func (s *Service) killSessionProcs(sessionID string, grace time.Duration) []int {
	return s.Supervisor.KillSession(sessionID, grace)
}

// TerminateSession kills processes, releases leases/ports, finalizes state.
func (s *Service) TerminateSession(ctx context.Context, sessionID, reason string) (*model.Session, error) {
	sess, err := s.GetSession(sessionID)
	if err != nil {
		return nil, err
	}
	if sess.State.Terminal() {
		return sess, nil // idempotent
	}
	sess.Owner = strings.TrimSpace(sess.Owner) // owner policy enforced at API layer
	s.Sessions.MarkStopping(sess.ID)
	killed := s.killSessionProcs(sess.ID, 5*time.Second)
	rel := s.Leases.ReleaseAll(sess.ID)
	portsReleased := s.Ports.ReleaseBySession(sess.ID)
	for _, r := range s.Runs.List(sess.ID) {
		if !r.Status.Terminal() {
			_ = s.Runs.Finish(r.ID, model.RunKilled, nil, reason)
		}
	}
	s.Sessions.Finish(sess.ID, model.SessionStopped, reason)
	s.cleanupSessionDir(sess)
	s.logf("session %s terminated (%s): %d proc(s) killed, %d lease(s) released, %d port(s) freed",
		sess.ID, reason, len(killed), len(rel), len(portsReleased))
	fin, _ := s.Sessions.Get(sess.ID)
	if fin == nil {
		return sess, nil
	}
	return fin, nil
}

// cleanupSessionDir removes the session runtime dir (home/tmp/xdg) but keeps
// runs (they live under <state>/runs).
func (s *Service) cleanupSessionDir(sess *model.Session) {
	_ = os.RemoveAll(sess.RuntimeDir)
}

// GetEnvSpec returns the env spec for a session.
func (s *Service) GetEnvSpec(sessionID string) (*env.Spec, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	spec, ok := s.envSpecs[sessionID]
	if !ok {
		return nil, fmt.Errorf("no env spec for session %q", sessionID)
	}
	return spec, nil
}

// SessionEnv builds the full child env for a session.
func (s *Service) SessionEnv(sessionID string) ([]string, *env.Spec, error) {
	sess, err := s.GetSession(sessionID)
	if err != nil {
		return nil, nil, err
	}
	spec, err := s.GetEnvSpec(sessionID)
	if err != nil {
		return nil, nil, err
	}
	envv := s.EnvMgr.Build(os.Environ(), spec)
	_ = sess
	return envv, spec, nil
}

// ---------------------------------------------------------------------------
// Tools
// ---------------------------------------------------------------------------

// ListTools returns registered tools.
func (s *Service) ListTools() []*model.Tool { return s.Registry.List() }

// GetTool returns one tool.
func (s *Service) GetTool(id string) (*model.Tool, error) {
	t, ok := s.Registry.Get(id)
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", id)
	}
	return t, nil
}

// DetectTools runs detection and returns new ids.
func (s *Service) DetectTools() []string { return s.Registry.Detect() }

// DoctorTool health-checks a tool.
func (s *Service) DoctorTool(ctx context.Context, id string) (*registry.DoctorResult, error) {
	return s.Registry.Doctor(ctx, id)
}

// ResolveRequirement maps "unity >= 6000.0" to a concrete tool.
func (s *Service) ResolveRequirement(raw string) (*model.Tool, error) {
	req, err := registry.ParseRequirement(raw)
	if err != nil {
		return nil, err
	}
	return s.Registry.Resolve(req)
}

// ---------------------------------------------------------------------------
// Resources & leases
// ---------------------------------------------------------------------------

// ListResources returns configured resources with live usage.
func (s *Service) ListResources() []ResourceView {
	var out []ResourceView
	for _, r := range s.Leases.ListResources() {
		out = append(out, ResourceView{
			Resource: *r,
			Active:   s.Leases.ActiveCount(r.ID),
			Waiting:  s.Leases.QueueDepth(r.ID),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ResourceView is a resource plus live scheduling state.
type ResourceView struct {
	model.Resource
	Active  int `json:"active"`
	Waiting int `json:"waiting"`
}

// AcquireLease acquires a lease for a session, subject to policy.
// adb:<serial> resources are dynamic: they are registered on demand as
// exclusive resources (device leases, roadmap v0.2).
func (s *Service) AcquireLease(ctx context.Context, owner, sessionID, resourceID string, ttl time.Duration) (*model.Lease, error) {
	if err := s.Policy.Evaluate(owner, policy.CapLeaseAcquire); err != nil {
		return nil, err
	}
	if _, err := s.GetSession(sessionID); err != nil {
		return nil, err
	}
	if strings.HasPrefix(resourceID, "adb:") {
		s.Leases.EnsureResource(resourceID, model.ModeExclusive)
	}
	return s.Leases.Acquire(ctx, resourceID, sessionID, ttl)
}

// ReleaseLease releases a lease owned by the session.
func (s *Service) ReleaseLease(owner, sessionID, leaseID string) error {
	if err := s.Policy.Evaluate(owner, policy.CapLeaseRelease); err != nil {
		return err
	}
	l, ok := s.Leases.Get(leaseID)
	if !ok {
		return fmt.Errorf("unknown lease %q", leaseID)
	}
	// Ownership: a session can only release its own lease.
	if l.SessionID != sessionID {
		return fmt.Errorf("lease %s belongs to session %s", leaseID, l.SessionID)
	}
	return s.Leases.Release(leaseID)
}

// ListLeases returns leases (all or by session).
func (s *Service) ListLeases(sessionID string) []*model.Lease {
	if sessionID == "" {
		return s.Leases.List(false)
	}
	return s.Leases.SessionHolds(sessionID)
}

// ---------------------------------------------------------------------------
// Ports
// ---------------------------------------------------------------------------

// AllocatePort reserves a port for a session and returns it.
func (s *Service) AllocatePort(owner, sessionID, protocol, name string) (*ports.Allocation, error) {
	if err := s.Policy.Evaluate(owner, policy.CapPortAllocate); err != nil {
		return nil, err
	}
	if _, err := s.GetSession(sessionID); err != nil {
		return nil, err
	}
	return s.Ports.Acquire(sessionID, protocol, name)
}

// ListPorts returns current allocations.
func (s *Service) ListPorts() []*ports.Allocation { return s.Ports.List() }

// ---------------------------------------------------------------------------
// Workspaces
// ---------------------------------------------------------------------------

// CreateWorkspace creates a git worktree workspace.
func (s *Service) CreateWorkspace(owner string, o workspace.CreateOptions) (*model.Workspace, error) {
	if err := s.Policy.Evaluate(owner, policy.CapWorkspaceCreate); err != nil {
		return nil, err
	}
	return s.Workspace.Create(o)
}

// ListWorkspaces lists workspaces.
func (s *Service) ListWorkspaces() []*model.Workspace { return s.Workspace.List() }

// RemoveWorkspace removes a workspace registration.
func (s *Service) RemoveWorkspace(owner, name string, prune bool) error {
	if err := s.Policy.Evaluate(owner, policy.CapWorkspaceRemove); err != nil {
		return err
	}
	// Refuse to remove while a live session uses it.
	w, ok := s.Workspace.GetByName(name)
	if !ok {
		return fmt.Errorf("unknown workspace %q", name)
	}
	for _, sess := range s.Sessions.List() {
		if sess.WorkspaceID == w.Name && !sess.State.Terminal() {
			return fmt.Errorf("workspace %s is in use by live session %s", name, sess.ID)
		}
	}
	_, err := s.Workspace.Remove(name, prune)
	return err
}

// ---------------------------------------------------------------------------
// Runs / tool execution
// ---------------------------------------------------------------------------

// ExecuteOptions is a tool execution request.
type ExecuteOptions struct {
	Owner     string
	SessionID string
	// Tool requirement: either a registry id or a "type constraint version"
	// requirement resolved through the registry.
	ToolID      string
	Requirement string
	Command     string
	Args        map[string]any
	// Timeout overrides the policy default (minutes).
	TimeoutMinutes int
	// Wait: block until the run finishes. If false the run executes in the
	// background; both CLI and MCP observe it via run records.
	Wait bool
}

// Execute runs a tool through its adapter with leases, supervision,
// logging and artifact collection.
func (s *Service) Execute(ctx context.Context, o ExecuteOptions) (*model.Run, error) {
	if err := s.Policy.Evaluate(o.Owner, policy.CapExecTool); err != nil {
		return nil, err
	}
	sess, err := s.GetSession(o.SessionID)
	if err != nil {
		return nil, err
	}
	if sess.State.Terminal() {
		return nil, fmt.Errorf("session %s is %s", sess.ID, sess.State)
	}
	// Resolve the tool.
	var tool *model.Tool
	if o.ToolID != "" {
		tool, err = s.GetTool(o.ToolID)
	} else if o.Requirement != "" {
		tool, err = s.ResolveRequirement(o.Requirement)
	} else {
		return nil, fmt.Errorf("tool_id or requirement is required")
	}
	if err != nil {
		return nil, err
	}
	if tool.Disabled {
		return nil, fmt.Errorf("tool %s is disabled", tool.ID)
	}
	ad, ok := s.AdapterFor(tool.Type)
	if !ok {
		return nil, fmt.Errorf("no adapter for tool type %q (tool %s)", tool.Type, tool.ID)
	}
	// Session env (isolated) + tool env.
	sessionEnv, spec, err := s.SessionEnv(sess.ID)
	if err != nil {
		return nil, err
	}
	_ = spec
	run, err := s.Runs.NewRun(sess.ID, tool.ID, tool.Type, o.Command)
	if err != nil {
		return nil, err
	}
	run.Env = s.EnvMgr.Redact(envPairs(sessionEnv))
	if o.TimeoutMinutes > 0 {
		run.Attempt = o.TimeoutMinutes // reuse field? no: keep in closure
	}
	_ = s.Runs.Update(run.ID, func(r *model.Run) { r.Env = run.Env })

	req := &adapter.Request{
		Session:    sess,
		Tool:       tool,
		Command:    o.Command,
		Args:       o.Args,
		SessionEnv: sessionEnv,
	}
	if _, err := ad.Prepare(ctx, req); err != nil {
		_ = s.Runs.Finish(run.ID, model.RunError, nil, err.Error())
		return run, err
	}

	rt := s.runtimeBundleFor(sess, run)
	s.Sessions.MarkRunning(sess.ID)

	exec := func() (*adapter.Result, error) {
		res, err := ad.Launch(ctx, req, rt, run.Dir)
		if err != nil {
			_ = s.Runs.Finish(run.ID, model.RunError, nil, err.Error())
			return res, err
		}
		status := res.Status
		if status == "" {
			status = statusFromExit(res.ExitCode)
		}
		_ = s.Runs.Update(run.ID, func(r *model.Run) {
			r.Status = status
			r.ExitCode = res.ExitCode
			if res.Summary != nil {
				r.Args = nil
			}
		})
		_ = s.Runs.Finish(run.ID, status, res.ExitCode, res.Note)
		// Persist adapter-declared extra files.
		for label, p := range res.ExtraFiles {
			if _, err := runs.Collect(run.Dir, label, p); err != nil {
				s.logf("run %s: collect %s: %v", run.ID, label, err)
			}
		}
		return res, nil
	}

	if o.Wait {
		_, err := exec()
		return s.getRunForReturn(run.ID), err
	}
	go func() {
		_, _ = exec()
	}()
	return s.getRunForReturn(run.ID), nil
}

func statusFromExit(code *int) model.RunStatus {
	if code == nil {
		return model.RunError
	}
	if *code == 0 {
		return model.RunSucceeded
	}
	return model.RunFailed
}

func envPairs(envv []string) map[string]string {
	m := map[string]string{}
	for _, kv := range envv {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[kv[:i]] = kv[i+1:]
		}
	}
	return m
}

func (s *Service) getRunForReturn(id string) *model.Run {
	if r, ok := s.Runs.Get(id); ok {
		return r
	}
	return &model.Run{ID: id}
}

// runtimeBundleFor builds the adapter.Runtime view bound to a run.
func (s *Service) runtimeBundleFor(sess *model.Session, run *model.Run) adapter.Runtime {
	return &serviceRuntime{svc: s, sess: sess, run: run}
}

// serviceRuntime implements adapter.Runtime against the daemon.
type serviceRuntime struct {
	svc  *Service
	sess *model.Session
	run  *model.Run
}

// AcquireLease implements adapter.Runtime.
func (r *serviceRuntime) AcquireLease(ctx context.Context, sessionID, resourceID string, ttl time.Duration) (*model.Lease, error) {
	l, err := r.svc.Leases.Acquire(ctx, resourceID, sessionID, ttl)
	if err == nil {
		_ = r.svc.Runs.Update(r.run.ID, func(rr *model.Run) {
			rr.Leases = append(rr.Leases, l.ID)
		})
	}
	return l, err
}

// AllocatePort implements adapter.Runtime.
func (r *serviceRuntime) AllocatePort(sessionID, protocol, name string) (int, error) {
	a, err := r.svc.Ports.Acquire(sessionID, protocol, name)
	if err != nil {
		return 0, err
	}
	return a.Port, nil
}

// Exec implements adapter.Runtime: supervised launch with timeout.
func (r *serviceRuntime) Exec(ctx context.Context, req adapter.ExecRequest) (*adapter.ExecResult, error) {
	s := r.svc
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = time.Duration(s.Policy.MaxRuntimeMinutes()) * time.Minute
	}
	execCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	info, handle, err := s.Supervisor.LaunchWait(supervisor.LaunchOptions{
		SessionID:  req.SessionID,
		RunID:      r.run.ID,
		Label:      req.Label,
		Exe:        req.Exe,
		Args:       req.Args,
		Env:        req.Env,
		Dir:        req.Dir,
		StdoutPath: filepath.Join(req.RunDir, "stdout.log"),
		StderrPath: filepath.Join(req.RunDir, "stderr.log"),
	})
	if err != nil {
		return nil, err
	}
	_ = s.Runs.Update(r.run.ID, func(rr *model.Run) {
		rr.Status = model.RunRunning
		rr.PID = info.PID
		rr.ProcStartTime = info.StartTime
	})

	select {
	case <-handle.Done:
		st := model.RunSucceeded
		if handle.Code() == nil || *handle.Code() != 0 {
			st = model.RunFailed
		}
		if handle.Killed() {
			st = model.RunKilled
		}
		return &adapter.ExecResult{
			ExitCode: handle.Code(),
			Status:   st,
			Duration: handle.Duration(),
			PID:      info.PID,
			Note:     handle.Reason(),
		}, nil
	case <-execCtx.Done():
		// Timeout: kill the tree, report TIMEOUT.
		_ = s.Supervisor.KillRun(r.run.ID)
		go func() { <-handle.Done }() // drain
		return &adapter.ExecResult{
			Status:   model.RunTimeout,
			Duration: time.Since(info.StartedAt),
			PID:      info.PID,
			Note:     fmt.Sprintf("timeout after %s", timeout),
		}, nil
	}
}

// WorkspaceDir implements adapter.Runtime.
func (r *serviceRuntime) WorkspaceDir(sessionID string) string {
	return r.sess.WorkspaceDir
}

// LaunchDetached implements adapter.Runtime: supervised service launch
// without waiting. The process stays under the supervisor until the
// session dies or crash recovery reaps it.
func (r *serviceRuntime) LaunchDetached(ctx context.Context, req adapter.ExecRequest) (adapter.DetachedHandle, error) {
	s := r.svc
	info, err := s.Supervisor.Launch(supervisor.LaunchOptions{
		SessionID:  req.SessionID,
		RunID:      r.run.ID,
		Label:      req.Label,
		Exe:        req.Exe,
		Args:       req.Args,
		Env:        req.Env,
		Dir:        req.Dir,
		StdoutPath: filepath.Join(req.RunDir, "stdout.log"),
		StderrPath: filepath.Join(req.RunDir, "stderr.log"),
	})
	if err != nil {
		return nil, err
	}
	_ = s.Runs.Update(r.run.ID, func(rr *model.Run) { rr.PID = info.PID })
	return &detachedHandle{svc: s, info: info}, nil
}

// detachedHandle adapts a ProcInfo into adapter.DetachedHandle.
type detachedHandle struct {
	svc  *Service
	info *model.ProcInfo
}

func (h *detachedHandle) PID() int { return h.info.PID }

func (h *detachedHandle) Alive() bool {
	return proc.AliveWithIdentity(h.info.PID, h.info.StartTime)
}

// Log implements adapter.Runtime.
func (r *serviceRuntime) Log(runID, level, msg string) {
	runs.AppendLog(r.run.Dir, level, msg)
}

// ---------------------------------------------------------------------------
// Runs & logs
// ---------------------------------------------------------------------------

// ListRuns lists runs (all or by session).
func (s *Service) ListRuns(sessionID string) []*model.Run { return s.Runs.List(sessionID) }

// GetRun returns a run.
func (s *Service) GetRun(id string) (*model.Run, error) {
	r, ok := s.Runs.Get(id)
	if !ok {
		return nil, fmt.Errorf("unknown run %q", id)
	}
	return r, nil
}

// RunArtifacts lists artifact files of a run.
func (s *Service) RunArtifacts(runID string) ([]string, error) {
	r, err := s.GetRun(runID)
	if err != nil {
		return nil, err
	}
	return runs.Artifacts(r.Dir)
}

// RunLogs returns a run log tail.
func (s *Service) RunLogs(runID, name string, tail int) (string, error) {
	r, err := s.GetRun(runID)
	if err != nil {
		return "", err
	}
	if name == "" {
		name = "log.txt"
	}
	switch name {
	case "log.txt", "stdout.log", "stderr.log", "unity.log":
	default:
		return "", fmt.Errorf("log name must be one of log.txt/stdout.log/stderr.log/unity.log")
	}
	out, err := runs.ReadLog(r.Dir, name, tail)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return out, nil
}

// DescribePolicy renders the effective policy.
func (s *Service) DescribePolicy() string { return s.Policy.Describe() }

// DescribeEnv renders the environment policy.
func (s *Service) DescribeEnv() string { return s.EnvMgr.Describe() }

// logf writes to the daemon log (state/logs/daemon.log).
func (s *Service) logf(format string, a ...any) {
	logDir := s.Cfg.LogsDir()
	_ = os.MkdirAll(logDir, 0o755)
	f, err := os.OpenFile(filepath.Join(logDir, "daemon.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s %s\n", time.Now().UTC().Format(time.RFC3339), fmt.Sprintf(format, a...))
}
