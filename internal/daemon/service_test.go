//go:build integration

package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/config"
	"github.com/xy00099/agent-runtime-harness/internal/model"
)

// newTestService builds a daemon Service with an isolated state dir.
func newTestService(t *testing.T) *Service {
	t.Helper()
	cfg := config.Default()
	cfg.Runtime.StateDir = filepath.Join(t.TempDir(), "arh-state")
	cfg.Normalize()
	// A command tool that prints env, so isolation is observable.
	cfg.Tools["echo-env"] = config.ToolConfig{
		Type:       "command",
		Executable: echoExe(),
		Args:       []string{},
	}
	// A command tool that spawns a long child, so tree-kill is observable.
	cfg.Tools["sleeper"] = config.ToolConfig{
		Type:       "command",
		Executable: sleepScript(t),
	}
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { svc.Shutdown(context.Background()) })
	return svc
}

func echoExe() string {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	}
	return "/bin/sh"
}

// sleepScript returns an executable that stays alive ~60s (or spawns a
// child that does) so kill behavior is observable.
func sleepScript(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	var path, content string
	if runtime.GOOS == "windows" {
		path = filepath.Join(dir, "sleeper.bat")
		content = "@echo off\r\nping -n 60 127.0.0.1 > nul\r\n"
	} else {
		path = filepath.Join(dir, "sleeper.sh")
		content = "#!/bin/sh\nsleep 60\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// echoArgs builds Args for echo-env that dump HOME/TMP into stdout.
func envDumpArgs() map[string]any {
	if runtime.GOOS == "windows" {
		return map[string]any{"args": []any{"/c", "echo HOME=%HOME% TMP=%TMP%"}}
	}
	return map[string]any{"args": []any{"-c", "echo HOME=$HOME TMP=$TMPDIR"}}
}

func TestSessionsIsolatedHomeTemp(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	a, err := svc.CreateSession(CreateSessionOpts{Owner: "cli:test", Name: "a"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := svc.CreateSession(CreateSessionOpts{Owner: "cli:test", Name: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if a.HomeDir == b.HomeDir || a.TempDir == b.TempDir {
		t.Fatalf("sessions share dirs: %s vs %s", a.HomeDir, b.HomeDir)
	}
	// Run echo-env in both, wait, compare stdout.
	runA, err := svc.Execute(ctx, ExecuteOptions{
		Owner: "cli:test", SessionID: a.ID, ToolID: "echo-env",
		Command: "run", Args: envDumpArgs(), Wait: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	runB, err := svc.Execute(ctx, ExecuteOptions{
		Owner: "cli:test", SessionID: b.ID, ToolID: "echo-env",
		Command: "run", Args: envDumpArgs(), Wait: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	outA, _ := svc.RunLogs(runA.ID, "stdout.log", 10)
	outB, _ := svc.RunLogs(runB.ID, "stdout.log", 10)
	if !strings.Contains(outA, a.HomeDir) {
		t.Fatalf("session A stdout does not show its HOME:\n%s", outA)
	}
	if !strings.Contains(outB, b.HomeDir) {
		t.Fatalf("session B stdout does not show its HOME:\n%s", outB)
	}
	if strings.Contains(outA, b.HomeDir) {
		t.Fatal("session A sees session B HOME")
	}
}

func TestSessionKillDoesNotAffectOther(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	a, _ := svc.CreateSession(CreateSessionOpts{Owner: "cli:test"})
	b, _ := svc.CreateSession(CreateSessionOpts{Owner: "cli:test"})

	// Start a long-run in A (no wait) and a quick one in B.
	runA, err := svc.Execute(ctx, ExecuteOptions{
		Owner: "cli:test", SessionID: a.ID, ToolID: "sleeper",
		Command: "run", Wait: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond) // let A's process start
	procs := svc.Supervisor.SessionProcs(a.ID)
	if len(procs) == 0 {
		t.Fatal("session A should have a live process")
	}
	pidA := procs[0].PID

	// Kill A. B was never touched.
	if _, err := svc.TerminateSession(ctx, a.ID, "test kill"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for procExists(pidA) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if procExists(pidA) {
		t.Fatal("session A process survived termination")
	}
	if got, err := svc.GetSession(a.ID); err != nil || got.State != model.SessionStopped {
		t.Fatalf("session A state: %v %v", got.State, err)
	}
	// B unaffected.
	gotB, _ := svc.GetSession(b.ID)
	if gotB.State.Terminal() {
		t.Fatalf("session B was collateral damage: %s", gotB.State)
	}
	// A's run record should be terminal too.
	ra, _ := svc.GetRun(runA.ID)
	if !ra.Status.Terminal() {
		t.Fatalf("run A still %s after kill", ra.Status)
	}
}

func TestLeaseHandoffOnSessionDeath(t *testing.T) {
	// Roadmap acceptance: A acquires exclusive, B waits, A dies, B gets it.
	svc := newTestService(t)
	ctx := context.Background()
	a, _ := svc.CreateSession(CreateSessionOpts{Owner: "cli:test"})
	b, _ := svc.CreateSession(CreateSessionOpts{Owner: "cli:test"})
	if _, err := svc.AcquireLease(ctx, "cli:test", a.ID, "unity-license", 0); err != nil {
		t.Fatal(err)
	}
	acquired := make(chan string, 1)
	go func() {
		l, err := svc.AcquireLease(context.Background(), "cli:test", b.ID, "unity-license", 0)
		if err == nil {
			acquired <- l.SessionID
		} else {
			acquired <- "err:" + err.Error()
		}
	}()
	time.Sleep(200 * time.Millisecond)
	if _, err := svc.TerminateSession(ctx, a.ID, "death"); err != nil {
		t.Fatal(err)
	}
	select {
	case sid := <-acquired:
		if sid != b.ID {
			t.Fatalf("handoff went to %s", sid)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("B did not acquire after A died")
	}
}

func TestPortsReleasedOnSessionEnd(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	s, _ := svc.CreateSession(CreateSessionOpts{Owner: "cli:test"})
	p1, err := svc.AllocatePort("cli:test", s.ID, "tcp", "debug")
	if err != nil {
		t.Fatal(err)
	}
	p2, _ := svc.AllocatePort("cli:test", s.ID, "tcp", "api")
	if p1.Port == p2.Port {
		t.Fatal("two allocations for the same session collided")
	}
	if _, err := svc.TerminateSession(ctx, s.ID, "done"); err != nil {
		t.Fatal(err)
	}
	for _, p := range svc.ListPorts() {
		if p.Port == p1.Port || p.Port == p2.Port {
			t.Fatal("port leaked after session end")
		}
	}
}

func TestCrashRecoveryReapsOrphans(t *testing.T) {
	// Simulate: daemon dies without cleanup; a new daemon must recover.
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Runtime.StateDir = filepath.Join(dir, "state")
	cfg.Normalize()
	cfg.Tools["sleeper"] = config.ToolConfig{Type: "command", Executable: sleepScript(t)}

	svc1, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc1.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	sess, err := svc1.CreateSession(CreateSessionOpts{Owner: "cli:test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc1.Execute(context.Background(), ExecuteOptions{
		Owner: "cli:test", SessionID: sess.ID, ToolID: "sleeper", Command: "run", Wait: false,
	}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	procs := svc1.Supervisor.SessionProcs(sess.ID)
	if len(procs) == 0 {
		t.Fatal("expected a live process before crash")
	}
	pid := procs[0].PID

	// "Crash": NO graceful shutdown. Build a fresh service on the same state.
	// (Supervisor state is in-memory; the run record persists the pid.)
	svc2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc2.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for procExists(pid) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if procExists(pid) {
		t.Fatal("orphaned process survived daemon restart")
	}
	got, _ := svc2.GetSession(sess.ID)
	if got.State != model.SessionStopped {
		t.Fatalf("interrupted session should be STOPPED after recovery, got %s", got.State)
	}
}

func TestExecuteFailurePath(t *testing.T) {
	svc := newTestService(t)
	ctx := context.Background()
	s, _ := svc.CreateSession(CreateSessionOpts{Owner: "cli:test"})
	// Failing command tool (portable: sh -c "exit 7" / cmd exit 7).
	exe, fargs := failExe(t)
	svc.Registry.Register(toolModel("failing", exe, fargs))
	run, err := svc.Execute(ctx, ExecuteOptions{
		Owner: "cli:test", SessionID: s.ID, ToolID: "failing",
		Command: "run", Wait: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != model.RunFailed {
		t.Fatalf("want FAILED, got %s", run.Status)
	}
	if run.ExitCode == nil || *run.ExitCode == 0 {
		t.Fatal("failing run should carry a nonzero exit code")
	}
	// Unknown tool errors cleanly.
	if _, err := svc.Execute(ctx, ExecuteOptions{
		Owner: "cli:test", SessionID: s.ID, ToolID: "nope", Command: "run",
	}); err == nil {
		t.Fatal("unknown tool must error")
	}
	// Unknown session errors cleanly.
	if _, err := svc.Execute(ctx, ExecuteOptions{
		Owner: "cli:test", SessionID: "sess-999", ToolID: "echo-env", Command: "run",
	}); err == nil {
		t.Fatal("unknown session must error")
	}
}

// toolModel builds a registry Tool for direct registration.
func toolModel(id, exe string, args []string) *model.Tool {
	return &model.Tool{ID: id, Type: "command", Executable: exe, Args: args}
}

// failExe returns a command that exits nonzero, portable across platforms.
func failExe(t *testing.T) (string, []string) {
	if runtime.GOOS == "windows" {
		return filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), []string{"/c", "exit 7"}
	}
	return "/bin/sh", []string{"-c", "exit 7"}
}

func execCommand(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }

// procExists checks the OS process table via tasklist/ps.
func procExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		out, err := execCombined("tasklist", "/FI", fmt.Sprintf("PID eq %d", pid))
		if err != nil {
			return false
		}
		return strings.Contains(out, fmt.Sprint(pid))
	}
	out, err := execCombined("ps", "-p", fmt.Sprint(pid))
	if err != nil {
		return false
	}
	return strings.Contains(out, fmt.Sprint(pid))
}

func execCombined(name string, args ...string) (string, error) {
	cmd := execCommand(name, args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
