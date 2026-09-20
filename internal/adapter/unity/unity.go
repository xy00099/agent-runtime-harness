// Package unity implements the Unity adapter (roadmap §15, §17).
//
// Unity runs in batchmode as a supervised child:
//
//	Unity -batchmode -quit -projectPath <workspace> -executeMethod <M>
//	       -logFile <runDir>/unity.log -testResults <runDir>/test-results.xml
//
// Adapter responsibilities per roadmap: resolve version -> validate
// workspace -> acquire leases -> prepare environment -> allocate logs ->
// launch -> monitor -> capture exit -> collect test reports -> release.
package unity

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/adapter"
	"github.com/xy00099/agent-runtime-harness/internal/config"
	"github.com/xy00099/agent-runtime-harness/internal/model"
)

// Adapter runs Unity batch commands.
type Adapter struct {
	Cfg *config.UnityConfig
}

// New builds the adapter.
func New(cfg *config.UnityConfig) *Adapter {
	if cfg == nil {
		cfg = &config.UnityConfig{}
	}
	return &Adapter{Cfg: cfg}
}

// Type implements adapter.ToolAdapter.
func (a *Adapter) Type() string { return "unity" }

// Capabilities implements adapter.ToolAdapter.
func (a *Adapter) Capabilities() []string {
	return []string{"version", "compile", "test.editmode", "test.playmode", "build", "run.batch"}
}

// resources per command (roadmap "Initial Unity Resources").
var cmdResources = map[string][]string{
	"version":       {},
	"compile":       {"unity-process-slot"},
	"test.editmode": {"unity-process-slot", "unity-license"},
	"test.playmode": {"unity-process-slot", "unity-license", "gpu"},
	"build":         {"unity-process-slot", "unity-license"},
	"run.batch":     {"unity-process-slot", "unity-license"},
}

// Prepare implements adapter.ToolAdapter: validate and declare resources.
func (a *Adapter) Prepare(ctx context.Context, req *adapter.Request) ([]string, error) {
	if req.Tool == nil || req.Tool.Executable == "" {
		return nil, fmt.Errorf("unity adapter: no Unity executable resolved")
	}
	if req.Command == "version" {
		return nil, nil
	}
	if req.Session == nil || req.Session.WorkspaceDir == "" {
		return nil, fmt.Errorf("unity adapter: session has no workspace")
	}
	// A Unity project must contain ProjectSettings/ProjectVersion.txt or
	// Assets/. We keep this loose: warn-level validation, hard error only
	// when the dir is missing entirely.
	if st, err := os.Stat(req.Session.WorkspaceDir); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("unity adapter: workspace dir %s missing", req.Session.WorkspaceDir)
	}
	if _, ok := cmdResources[req.Command]; !ok {
		return nil, fmt.Errorf("unity adapter: unknown command %q (have %v)", req.Command, a.Capabilities())
	}
	return cmdResources[req.Command], nil
}

// Launch implements adapter.ToolAdapter.
func (a *Adapter) Launch(ctx context.Context, req *adapter.Request, rt adapter.Runtime, runDir string) (*adapter.Result, error) {
	tool := req.Tool
	sess := req.Session
	unityLog := filepath.Join(runDir, "unity.log")

	// 1. Acquire required leases (Prepare already validated the list).
	leases, err := a.Prepare(ctx, req)
	if err != nil {
		return nil, err
	}
	for _, rid := range leases {
		if _, err := rt.AcquireLease(ctx, sess.ID, rid, 2*time.Hour); err != nil {
			return nil, fmt.Errorf("acquire %s: %w", rid, err)
		}
	}

	// 2. Build the batchmode command line.
	args := []string{"-batchmode", "-quit", "-nographics",
		"-projectPath", sess.WorkspaceDir,
		"-logFile", unityLog}
	testResults := filepath.Join(runDir, "test-results.xml")
	switch req.Command {
	case "version":
		// -version prints and exits; not a project run.
		return a.launchVersion(ctx, req, rt, runDir)
	case "compile":
		// Import + script compilation happens with any executeMethod-less
		// batch run; -quit makes it exit after import.
	case "test.editmode", "test.playmode":
		mode := "EditMode"
		if req.Command == "test.playmode" {
			mode = "PlayMode"
		}
		args = append(args,
			"-runTests", "-testPlatform", mode,
			"-testResults", testResults,
			// UNITY_TEST_COMMANDS / editorArgs pass-through:
			"-forgetProjectPath",
		)
	case "build":
		target := strArg(req.Args, "target", "Android")
		out := strArg(req.Args, "output", filepath.Join(runDir, "Builds", target))
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return nil, err
		}
		method := strArg(req.Args, "method", "")
		if method == "" {
			return nil, fmt.Errorf("build requires --method (your CIScript.BuildPlayer entry point)")
		}
		args = append(args, "-executeMethod", method, "-buildTarget", target)
		_ = out
	case "run.batch":
		method := strArg(req.Args, "method", "")
		if method == "" {
			return nil, fmt.Errorf("run.batch requires --method")
		}
		args = append(args, "-executeMethod", method)
	}

	// 3. Environment: session env + accelerator + per-tool env.
	env := append([]string{}, req.SessionEnv...)
	if a.Cfg.Cache.AcceleratorEnabled {
		env = append(env,
			"UNITY_CACHE_SERVER="+fmt.Sprintf("%s:%s", a.Cfg.Cache.AcceleratorHost, "10080"),
		)
	}
	for k, v := range tool.Env {
		env = append(env, k+"="+v)
	}

	// 4. Launch under supervision.
	rt.Log("", "info", fmt.Sprintf("unity %s: %s %s", req.Command, tool.Executable, strings.Join(args, " ")))
	res, err := rt.Exec(ctx, adapter.ExecRequest{
		SessionID: sess.ID,
		Label:     fmt.Sprintf("unity:%s", req.Command),
		Exe:       tool.Executable,
		Args:      args,
		Env:       env,
		Dir:       runDir,
		RunDir:    runDir,
		Timeout:   time.Duration(a.timeoutMinutes(req)) * time.Minute,
	})
	if err != nil {
		return nil, err
	}

	out := &adapter.Result{ExitCode: res.ExitCode, Status: res.Status, Note: res.Note}
	// 5. Collect test reports.
	if req.Command == "test.editmode" || req.Command == "test.playmode" {
		if sum, perr := ParseTestResults(testResults); perr == nil {
			out.Summary = sum
			out.ExtraFiles = map[string]string{"test-results": testResults}
		} else if res.Status == model.RunSucceeded {
			out.Note = fmt.Sprintf("%s (test report unreadable: %v)", out.Note, perr)
		}
	}
	if req.Command == "build" {
		out.ExtraFiles = map[string]string{"builds": filepath.Join(runDir, "Builds")}
	}
	return out, nil
}

// launchVersion runs Unity -version (no project, no leases).
func (a *Adapter) launchVersion(ctx context.Context, req *adapter.Request, rt adapter.Runtime, runDir string) (*adapter.Result, error) {
	args := []string{"-version"}
	env := append([]string{}, req.SessionEnv...)
	for k, v := range req.Tool.Env {
		env = append(env, k+"="+v)
	}
	res, err := rt.Exec(ctx, adapter.ExecRequest{
		SessionID: req.Session.ID,
		Label:     "unity:version",
		Exe:       req.Tool.Executable,
		Args:      args,
		Env:       env,
		Dir:       runDir,
		RunDir:    runDir,
		Timeout:   2 * time.Minute,
	})
	if err != nil {
		return nil, err
	}
	return &adapter.Result{ExitCode: res.ExitCode, Status: res.Status, Note: res.Note}, nil
}

func (a *Adapter) timeoutMinutes(req *adapter.Request) int {
	if v, ok := req.Args["timeout_minutes"]; ok {
		switch n := v.(type) {
		case float64:
			if n > 0 {
				return int(n)
			}
		case int:
			if n > 0 {
				return n
			}
		}
	}
	if req.Command == "test.playmode" {
		return 60
	}
	return 30
}

func strArg(args map[string]any, key, def string) string {
	if v, ok := args[key]; ok {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return def
}

// ---------------------------------------------------------------------------
// Test report parsing (NUnit3 XML emitted by Unity Test Framework)
// ---------------------------------------------------------------------------

// ParseTestResults reads a Unity test-results.xml into a summary.
func ParseTestResults(path string) (*model.TestSummary, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc struct {
		XMLName      xml.Name `xml:"test-run"`
		Result       string   `xml:"result,attr"`
		Total        int      `xml:"total,attr"`
		Passed       int      `xml:"passed,attr"`
		Failed       int      `xml:"failed,attr"`
		Skipped      int      `xml:"skipped,attr"`
		Inconclusive int      `xml:"inconclusive,attr"`
		Duration     float64  `xml:"duration,attr"`
	}
	if err := xml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse test-results.xml: %w", err)
	}
	return &model.TestSummary{
		Total:        doc.Total,
		Passed:       doc.Passed,
		Failed:       doc.Failed,
		Skipped:      doc.Skipped,
		Inconclusive: doc.Inconclusive,
		DurationMS:   int64(doc.Duration * 1000),
	}, nil
}

var _ adapter.ToolAdapter = (*Adapter)(nil)
