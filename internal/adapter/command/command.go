// Package command is the generic adapter: run any host executable under
// supervision with the session's isolated environment. It is the fallback
// adapter and the reference implementation of the adapter contract.
package command

import (
	"context"
	"fmt"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/adapter"
	"github.com/xy00099/agent-runtime-harness/internal/model"
)

// Adapter runs configured commands.
type Adapter struct{}

// New builds the adapter.
func New() *Adapter { return &Adapter{} }

// Type implements adapter.ToolAdapter.
func (a *Adapter) Type() string { return "command" }

// Capabilities implements adapter.ToolAdapter.
func (a *Adapter) Capabilities() []string {
	return []string{"run", "version", "custom"}
}

// Prepare implements adapter.ToolAdapter: generic runs need no leases.
func (a *Adapter) Prepare(ctx context.Context, req *adapter.Request) ([]string, error) {
	if req.Tool == nil || req.Tool.Executable == "" {
		return nil, fmt.Errorf("command adapter: tool has no executable")
	}
	return nil, nil
}

// Launch implements adapter.ToolAdapter.
func (a *Adapter) Launch(ctx context.Context, req *adapter.Request, rt adapter.Runtime, runDir string) (*adapter.Result, error) {
	exe := req.Tool.Executable
	args := append([]string{}, req.Tool.Args...)
	switch req.Command {
	case "version":
		if len(req.Tool.VersionCommand) > 0 {
			args = append([]string{}, req.Tool.VersionCommand...)
		}
	default:
		// Caller-supplied positional args under Args["args"].
		switch extra := req.Args["args"].(type) {
		case []any:
			for _, v := range extra {
				if s, ok := v.(string); ok {
					args = append(args, s)
				}
			}
		case string:
			if extra != "" {
				args = append(args, extra)
			}
		}
	}

	env := append([]string{}, req.SessionEnv...)
	for k, v := range req.Tool.Env { // per-tool overrides win
		env = append(env, k+"="+v)
	}

	rt.Log("", "info", fmt.Sprintf("command: %s %v", exe, args))
	timeout := 10 * time.Minute
	if req.Timeout > 0 {
		timeout = req.Timeout // caller override (CLI --timeout-min, MCP)
	}
	res, err := rt.Exec(ctx, adapter.ExecRequest{
		SessionID: req.Session.ID,
		Label:     fmt.Sprintf("command:%s", req.Tool.ID),
		Exe:       exe,
		Args:      args,
		Env:       env,
		Dir:       rt.WorkspaceDir(req.Session.ID),
		RunDir:    runDir,
		Timeout:   timeout,
	})
	if err != nil {
		return nil, err
	}
	return &adapter.Result{ExitCode: res.ExitCode, Status: res.Status, Note: res.Note}, nil
}

var _ adapter.ToolAdapter = (*Adapter)(nil)
var _ = model.RunSucceeded
