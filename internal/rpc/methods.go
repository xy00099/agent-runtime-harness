// Package rpc: method table binding the daemon service to RPC methods.
package rpc

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/daemon"
	"github.com/xy00099/agent-runtime-harness/internal/model"
	"github.com/xy00099/agent-runtime-harness/internal/workspace"
)

// Wire types (all JSON-friendly). Kept explicit so the wire contract is
// versionable independently of internal model structs.

type SessionView struct {
	ID           string            `json:"id"`
	Name         string            `json:"name,omitempty"`
	Owner        string            `json:"owner"`
	Workspace    string            `json:"workspace,omitempty"`
	WorkspaceDir string            `json:"workspace_dir,omitempty"`
	State        string            `json:"state"`
	HomeDir      string            `json:"home_dir"`
	TempDir      string            `json:"temp_dir"`
	RuntimeDir   string            `json:"runtime_dir"`
	CreatedAt    string            `json:"created_at"`
	StartedAt    string            `json:"started_at,omitempty"`
	StoppedAt    string            `json:"stopped_at,omitempty"`
	Failure      string            `json:"failure,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
}

func sessionView(s *model.Session) SessionView {
	v := SessionView{
		ID: s.ID, Name: s.Name, Owner: s.Owner,
		Workspace: s.WorkspaceID, WorkspaceDir: s.WorkspaceDir,
		State:   string(s.State),
		HomeDir: s.HomeDir, TempDir: s.TempDir, RuntimeDir: s.RuntimeDir,
		CreatedAt: s.CreatedAt.UTC().Format(time.RFC3339),
		Failure:   s.Failure,
		Env:       s.Env,
	}
	if s.StartedAt != nil {
		v.StartedAt = s.StartedAt.UTC().Format(time.RFC3339)
	}
	if s.StoppedAt != nil {
		v.StoppedAt = s.StoppedAt.UTC().Format(time.RFC3339)
	}
	return v
}

type CreateSessionParams struct {
	Owner        string `json:"owner"`
	Name         string `json:"name,omitempty"`
	Workspace    string `json:"workspace,omitempty"` // workspace name
	WorkspaceDir string `json:"workspace_dir,omitempty"`
}

type CreateSessionResult struct {
	Session SessionView `json:"session"`
}

type ListResult[T any] struct {
	Items []T `json:"items"`
}

type Empty struct{}

type IDParams struct {
	ID     string `json:"id"`
	Owner  string `json:"owner,omitempty"`
	Reason string `json:"reason,omitempty"`
}

type SessionProcsResult struct {
	Items []ProcView `json:"items"`
}

type ProcView struct {
	PID       int      `json:"pid"`
	SessionID string   `json:"session_id"`
	RunID     string   `json:"run_id,omitempty"`
	Label     string   `json:"label"`
	Exe       string   `json:"exe"`
	Cmdline   []string `json:"cmdline"`
	StartedAt string   `json:"started_at"`
	ExitCode  *int     `json:"exit_code,omitempty"`
	Killed    bool     `json:"killed,omitempty"`
}

func procView(p *model.ProcInfo) ProcView {
	return ProcView{
		PID: p.PID, SessionID: p.SessionID, RunID: p.RunID, Label: p.Label,
		Exe: p.Exe, Cmdline: p.Cmdline,
		StartedAt: p.StartedAt.UTC().Format(time.RFC3339),
		ExitCode:  p.ExitCode, Killed: p.Killed,
	}
}

// ToolView is a wire tool.
type ToolView struct {
	ID           string   `json:"id"`
	Type         string   `json:"type"`
	Version      string   `json:"version,omitempty"`
	Executable   string   `json:"executable"`
	Capabilities []string `json:"capabilities"`
	Source       string   `json:"source"`
	Disabled     bool     `json:"disabled"`
}

func toolView(t *model.Tool) ToolView {
	return ToolView{
		ID: t.ID, Type: t.Type, Version: t.Version, Executable: t.Executable,
		Capabilities: t.Capabilities, Source: t.Source, Disabled: t.Disabled,
	}
}

type DoctorView struct {
	ToolID     string   `json:"tool_id"`
	OK         bool     `json:"ok"`
	Executable string   `json:"executable"`
	Problems   []string `json:"problems,omitempty"`
	VersionOut string   `json:"version_output,omitempty"`
}

type ResourceView struct {
	ID       string `json:"id"`
	Mode     string `json:"mode"`
	Capacity int    `json:"capacity,omitempty"`
	Provider string `json:"provider,omitempty"`
	Active   int    `json:"active"`
	Waiting  int    `json:"waiting"`
}

type AcquireLeaseParams struct {
	Owner      string `json:"owner"`
	SessionID  string `json:"session_id"`
	ResourceID string `json:"resource_id"`
	TTLSeconds int    `json:"ttl_seconds,omitempty"`
	Wait       bool   `json:"wait"`
}

type LeaseView struct {
	ID         string `json:"id"`
	ResourceID string `json:"resource_id"`
	SessionID  string `json:"session_id"`
	CreatedAt  string `json:"created_at"`
	ExpiresAt  string `json:"expires_at,omitempty"`
	State      string `json:"state"`
}

func leaseView(l *model.Lease) LeaseView {
	return LeaseView{
		ID: l.ID, ResourceID: l.ResourceID, SessionID: l.SessionID,
		CreatedAt: l.CreatedAt.UTC().Format(time.RFC3339),
		ExpiresAt: optTime(l.ExpiresAt), State: string(l.State),
	}
}

func optTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

type AllocatePortParams struct {
	Owner     string `json:"owner"`
	SessionID string `json:"session_id"`
	Protocol  string `json:"protocol,omitempty"`
	Name      string `json:"name,omitempty"`
}

type PortView struct {
	Port      int    `json:"port"`
	SessionID string `json:"session_id"`
	Protocol  string `json:"protocol"`
	Name      string `json:"name"`
}

type WorkspaceView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Repo      string `json:"repo"`
	Branch    string `json:"branch,omitempty"`
	Path      string `json:"path"`
	CreatedAt string `json:"created_at"`
	Plain     bool   `json:"plain"`
}

func workspaceView(w *model.Workspace) WorkspaceView {
	return WorkspaceView{
		ID: w.ID, Name: w.Name, Repo: w.Repo, Branch: w.Branch, Path: w.Path,
		CreatedAt: w.CreatedAt.UTC().Format(time.RFC3339), Plain: w.Plain,
	}
}

type CreateWorkspaceParams struct {
	Owner  string `json:"owner"`
	Repo   string `json:"repo"`
	Branch string `json:"branch,omitempty"`
	Name   string `json:"name,omitempty"`
	Detach bool   `json:"detach,omitempty"`
	Path   string `json:"path,omitempty"`
}

type RunView struct {
	ID         string   `json:"id"`
	SessionID  string   `json:"session_id"`
	ToolID     string   `json:"tool_id"`
	ToolType   string   `json:"tool_type"`
	Command    string   `json:"command"`
	Status     string   `json:"status"`
	StartedAt  string   `json:"started_at"`
	DurationMS int64    `json:"duration_ms"`
	ExitCode   *int     `json:"exit_code,omitempty"`
	Dir        string   `json:"dir"`
	Leases     []string `json:"leases,omitempty"`
	Error      string   `json:"error,omitempty"`
	PID        int      `json:"pid,omitempty"`
}

func runView(r *model.Run) RunView {
	return RunView{
		ID: r.ID, SessionID: r.SessionID, ToolID: r.ToolID, ToolType: r.ToolType,
		Command: r.Command, Status: string(r.Status),
		StartedAt:  r.StartedAt.UTC().Format(time.RFC3339),
		DurationMS: r.DurationMS, ExitCode: r.ExitCode, Dir: r.Dir,
		Leases: r.Leases, Error: r.Error, PID: r.PID,
	}
}

type ExecuteParams struct {
	Owner          string         `json:"owner"`
	SessionID      string         `json:"session_id"`
	ToolID         string         `json:"tool_id,omitempty"`
	Requirement    string         `json:"requirement,omitempty"` // "unity >= 6000.0"
	Command        string         `json:"command"`
	Args           map[string]any `json:"args,omitempty"`
	TimeoutMinutes int            `json:"timeout_minutes,omitempty"`
	Wait           bool           `json:"wait"`
}

type LogParams struct {
	RunID string `json:"run_id"`
	Name  string `json:"name,omitempty"`
	Tail  int    `json:"tail,omitempty"`
}

type LogResult struct {
	RunID   string `json:"run_id"`
	Name    string `json:"name"`
	Content string `json:"content"`
}

type ArtifactsResult struct {
	RunID string   `json:"run_id"`
	Files []string `json:"files"`
}

type DescribeResult struct {
	Policy string `json:"policy"`
	Env    string `json:"env"`
}

// Register mounts every daemon capability on the mux.
func Register(mux *Mux, svc *daemon.Service) {
	must := func(method string, h Handler) { mux.Handle(method, h) }

	must("session.create", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p CreateSessionParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "session.create: %v", err)
		}
		sess, err := svc.CreateSession(daemon.CreateSessionOpts{
			Owner: p.Owner, Name: p.Name,
			Workspace: p.Workspace, WorkspaceDir: p.WorkspaceDir,
		})
		if err != nil {
			return nil, Errorf("%v", err)
		}
		return CreateSessionResult{Session: sessionView(sess)}, nil
	})

	must("session.get", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p IDParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "%v", err)
		}
		sess, err := svc.GetSession(p.ID)
		if err != nil {
			return nil, Errorf("%v", err)
		}
		return sessionView(sess), nil
	})

	must("session.list", func(ctx context.Context, raw json.RawMessage) (any, error) {
		out := ListResult[SessionView]{Items: []SessionView{}}
		for _, s := range svc.ListSessions() {
			out.Items = append(out.Items, sessionView(s))
		}
		return out, nil
	})

	must("session.terminate", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p IDParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "%v", err)
		}
		sess, err := svc.TerminateSession(ctx, p.ID, orDefault(p.Reason, "api request"))
		if err != nil {
			return nil, Errorf("%v", err)
		}
		return sessionView(sess), nil
	})

	must("session.procs", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p IDParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "%v", err)
		}
		procs, err := svc.SessionProcs(p.ID)
		if err != nil {
			return nil, Errorf("%v", err)
		}
		out := SessionProcsResult{Items: []ProcView{}}
		for _, pr := range procs {
			out.Items = append(out.Items, procView(pr))
		}
		return out, nil
	})

	must("tool.list", func(ctx context.Context, raw json.RawMessage) (any, error) {
		out := ListResult[ToolView]{Items: []ToolView{}}
		for _, t := range svc.ListTools() {
			out.Items = append(out.Items, toolView(t))
		}
		svc.DetectTools()
		return out, nil
	})

	must("tool.inspect", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p IDParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "%v", err)
		}
		t, err := svc.GetTool(p.ID)
		if err != nil {
			return nil, Errorf("%v", err)
		}
		return toolView(t), nil
	})

	must("tool.doctor", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p IDParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "%v", err)
		}
		res, err := svc.DoctorTool(ctx, p.ID)
		if err != nil {
			return nil, Errorf("%v", err)
		}
		v := DoctorView{ToolID: res.ToolID, OK: res.OK, Executable: res.Executable,
			Problems: res.Problems, VersionOut: res.VersionOut}
		if res.VersionOut == "" && res.VersionErr != "" {
			v.VersionOut = "error: " + res.VersionErr
		}
		return v, nil
	})

	must("tool.resolve", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Requirement string `json:"requirement"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "%v", err)
		}
		t, err := svc.ResolveRequirement(p.Requirement)
		if err != nil {
			return nil, Errorf("%v", err)
		}
		return toolView(t), nil
	})

	must("resource.list", func(ctx context.Context, raw json.RawMessage) (any, error) {
		out := ListResult[ResourceView]{Items: []ResourceView{}}
		for _, r := range svc.ListResources() {
			out.Items = append(out.Items, ResourceView{
				ID: r.ID, Mode: string(r.Mode), Capacity: r.Capacity,
				Provider: r.Provider, Active: r.Active, Waiting: r.Waiting,
			})
		}
		return out, nil
	})

	must("lease.acquire", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p AcquireLeaseParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "%v", err)
		}
		var ttl time.Duration
		if p.TTLSeconds > 0 {
			ttl = time.Duration(p.TTLSeconds) * time.Second
		}
		callCtx := ctx
		if !p.Wait {
			var cancel context.CancelFunc
			callCtx, cancel = context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
		}
		l, err := svc.AcquireLease(callCtx, p.Owner, p.SessionID, p.ResourceID, ttl)
		if err != nil {
			return nil, Errorf("%v", err)
		}
		return leaseView(l), nil
	})

	must("lease.release", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Owner     string `json:"owner"`
			SessionID string `json:"session_id"`
			LeaseID   string `json:"lease_id"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "%v", err)
		}
		if err := svc.ReleaseLease(p.Owner, p.SessionID, p.LeaseID); err != nil {
			return nil, Errorf("%v", err)
		}
		return Empty{}, nil
	})

	must("lease.list", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			SessionID string `json:"session_id,omitempty"`
		}
		_ = json.Unmarshal(raw, &p)
		out := ListResult[LeaseView]{Items: []LeaseView{}}
		for _, l := range svc.ListLeases(p.SessionID) {
			out.Items = append(out.Items, leaseView(l))
		}
		return out, nil
	})

	must("port.allocate", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p AllocatePortParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "%v", err)
		}
		a, err := svc.AllocatePort(p.Owner, p.SessionID, orDefault(p.Protocol, "tcp"), p.Name)
		if err != nil {
			return nil, Errorf("%v", err)
		}
		return PortView{Port: a.Port, SessionID: a.SessionID, Protocol: a.Protocol, Name: a.Name}, nil
	})

	must("port.list", func(ctx context.Context, raw json.RawMessage) (any, error) {
		out := ListResult[PortView]{Items: []PortView{}}
		for _, a := range svc.ListPorts() {
			out.Items = append(out.Items, PortView{
				Port: a.Port, SessionID: a.SessionID, Protocol: a.Protocol, Name: a.Name,
			})
		}
		return out, nil
	})

	must("workspace.create", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p CreateWorkspaceParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "%v", err)
		}
		w, err := svc.CreateWorkspace(p.Owner, workspace.CreateOptions{
			Repo: p.Repo, Branch: p.Branch, Name: p.Name, Detach: p.Detach, Path: p.Path,
		})
		if err != nil {
			return nil, Errorf("%v", err)
		}
		return workspaceView(w), nil
	})

	must("workspace.list", func(ctx context.Context, raw json.RawMessage) (any, error) {
		out := ListResult[WorkspaceView]{Items: []WorkspaceView{}}
		for _, w := range svc.ListWorkspaces() {
			out.Items = append(out.Items, workspaceView(w))
		}
		return out, nil
	})

	must("workspace.remove", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
			Prune bool   `json:"prune"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "%v", err)
		}
		if err := svc.RemoveWorkspace(p.Owner, p.Name, p.Prune); err != nil {
			return nil, Errorf("%v", err)
		}
		return Empty{}, nil
	})

	must("run.execute", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ExecuteParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "%v", err)
		}
		run, err := svc.Execute(ctx, daemon.ExecuteOptions{
			Owner: p.Owner, SessionID: p.SessionID,
			ToolID: p.ToolID, Requirement: p.Requirement,
			Command: p.Command, Args: p.Args,
			TimeoutMinutes: p.TimeoutMinutes, Wait: p.Wait,
		})
		if err != nil && run == nil {
			return nil, Errorf("%v", err)
		}
		if err != nil {
			// Run record exists (Prepare failed): return it with the error
			// embedded rather than losing the record.
			v := runView(run)
			v.Error = err.Error()
			return v, nil
		}
		return runView(run), nil
	})

	must("run.list", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p struct {
			SessionID string `json:"session_id,omitempty"`
		}
		_ = json.Unmarshal(raw, &p)
		out := ListResult[RunView]{Items: []RunView{}}
		for _, r := range svc.ListRuns(p.SessionID) {
			out.Items = append(out.Items, runView(r))
		}
		return out, nil
	})

	must("run.get", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p IDParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "%v", err)
		}
		r, err := svc.GetRun(p.ID)
		if err != nil {
			return nil, Errorf("%v", err)
		}
		return runView(r), nil
	})

	must("run.logs", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p LogParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "%v", err)
		}
		content, err := svc.RunLogs(p.RunID, p.Name, p.Tail)
		if err != nil {
			return nil, Errorf("%v", err)
		}
		return LogResult{RunID: p.RunID, Name: p.Name, Content: content}, nil
	})

	must("run.artifacts", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p IDParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, ErrorfCode(CodeParams, "%v", err)
		}
		files, err := svc.RunArtifacts(p.ID)
		if err != nil {
			return nil, Errorf("%v", err)
		}
		if files == nil {
			files = []string{}
		}
		return ArtifactsResult{RunID: p.ID, Files: files}, nil
	})

	must("describe", func(ctx context.Context, raw json.RawMessage) (any, error) {
		return DescribeResult{Policy: svc.DescribePolicy(), Env: svc.DescribeEnv()}, nil
	})
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
