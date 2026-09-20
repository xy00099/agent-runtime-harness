// Package supervisor implements process supervision (roadmap §9).
//
// Every child is launched through internal/proc (own process group on unix,
// kill-on-close job object on Windows). The supervisor records OS-native
// process identity to guard against PID reuse, streams stdout/stderr into
// per-run log files, and applies recursive tree cleanup on kill.
package supervisor

import (
	"bufio"
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/model"
	"github.com/xy00099/agent-runtime-harness/internal/proc"
)

// Supervisor owns all supervised processes across sessions.
type Supervisor struct {
	mu   sync.Mutex
	sets map[string]*proc.Set       // sessionID -> live set
	recs map[string]*model.ProcInfo // "session/pid" -> record
}

// New creates the supervisor.
func New() *Supervisor {
	return &Supervisor{sets: map[string]*proc.Set{}, recs: map[string]*model.ProcInfo{}}
}

func pkey(sessionID string, pid int) string { return fmt.Sprintf("%s/%d", sessionID, pid) }

// LaunchOptions describe one supervised launch.
type LaunchOptions struct {
	SessionID  string
	RunID      string
	Label      string
	Exe        string
	Args       []string
	Env        []string
	Dir        string
	StdoutPath string
	StderrPath string
	// OnExit, if set, is invoked once with the final status.
	OnExit func(st ExitStatus)
}

// ExitStatus is the terminal state of a supervised process.
type ExitStatus struct {
	PID      int
	Code     *int
	Err      error
	Killed   bool
	Reason   string
	Duration time.Duration
}

// Launch starts a supervised process and returns its record. Output is
// streamed to StdoutPath/StderrPath; the record is finalized by a
// background waiter that also fires OnExit.
func (s *Supervisor) Launch(opt LaunchOptions) (*model.ProcInfo, error) {
	if opt.SessionID == "" {
		return nil, fmt.Errorf("supervisor: session is required")
	}
	// Wire output capture BEFORE start so pipes never block the child.
	var stdoutW, stderrW io.WriteCloser
	open := func(p string) (io.WriteCloser, error) {
		if p == "" {
			return nil, nil
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return nil, err
		}
		return os.OpenFile(p, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	}
	var err error
	if stdoutW, err = open(opt.StdoutPath); err != nil {
		return nil, fmt.Errorf("stdout file: %w", err)
	}
	if stderrW, err = open(opt.StderrPath); err != nil {
		if stdoutW != nil {
			stdoutW.Close()
		}
		return nil, fmt.Errorf("stderr file: %w", err)
	}

	cmd := exec.Command(opt.Exe, opt.Args...)
	cmd.Env = opt.Env
	cmd.Dir = opt.Dir
	if stdoutW != nil {
		cmd.Stdout = stdoutW
	} else {
		cmd.Stdout = io.Discard
	}
	if stderrW != nil {
		cmd.Stderr = stderrW
	} else {
		cmd.Stderr = io.Discard
	}
	cmd.WaitDelay = 5 * time.Second // unblock Wait if pipes stay open

	if err := proc.Start(cmd); err != nil {
		if stdoutW != nil {
			stdoutW.Close()
		}
		if stderrW != nil {
			stderrW.Close()
		}
		return nil, fmt.Errorf("launch %s: %w", opt.Exe, err)
	}
	pid := cmd.Process.Pid
	identity := proc.ProcessIdentity(pid)

	info := &model.ProcInfo{
		PID:       pid,
		StartTime: identity,
		Exe:       opt.Exe,
		Cmdline:   append([]string{opt.Exe}, opt.Args...),
		SessionID: opt.SessionID,
		RunID:     opt.RunID,
		Label:     opt.Label,
		StartedAt: time.Now(),
		CmdHash:   hashCmd(opt.Exe, opt.Args),
	}
	s.mu.Lock()
	set := s.sets[opt.SessionID]
	if set == nil {
		set = proc.NewSet()
		s.sets[opt.SessionID] = set
	}
	set.Add(&proc.Info{
		PID: pid, StartTime: identity, Exe: opt.Exe, Cmdline: info.Cmdline,
		SessionID: opt.SessionID, Label: opt.Label, StartedAt: info.StartedAt,
	})
	s.recs[pkey(opt.SessionID, pid)] = info
	s.mu.Unlock()

	go s.await(opt, cmd, stdoutW, stderrW, info)
	return info, nil
}

// await closes log writers after Wait, then finalizes the record.
func (s *Supervisor) await(opt LaunchOptions, cmd *exec.Cmd, stdoutW, stderrW io.WriteCloser, info *model.ProcInfo) {
	waitErr := cmd.Wait()
	if stdoutW != nil {
		stdoutW.Close()
	}
	if stderrW != nil {
		stderrW.Close()
	}
	dur := time.Since(info.StartedAt)

	st := ExitStatus{PID: info.PID, Err: waitErr, Duration: dur}
	if ws, ok := cmd.ProcessState.Sys().(interface{ ExitStatus() int }); ok {
		code := ws.ExitStatus()
		st.Code = &code
	}

	s.mu.Lock()
	now := time.Now()
	info.ExitedAt = &now
	info.ExitCode = st.Code
	if info.Killed {
		st.Killed = true
		st.Reason = info.KillReason
	}
	if st.Code == nil && waitErr != nil && info.KillReason == "" {
		info.KillReason = waitErr.Error()
	}
	if set := s.sets[info.SessionID]; set != nil {
		set.Remove(info.PID)
	}
	delete(s.recs, pkey(info.SessionID, info.PID))
	s.mu.Unlock()

	if opt.OnExit != nil {
		opt.OnExit(st)
	}
}

// hashCmd produces a stable identity hash of the command line.
func hashCmd(exe string, args []string) string {
	h := fnv.New64a()
	h.Write([]byte(exe))
	for _, a := range args {
		h.Write([]byte{0})
		h.Write([]byte(a))
	}
	return fmt.Sprintf("%016x", h.Sum64())
}

// ---------------------------------------------------------------------------
// Kill paths
// ---------------------------------------------------------------------------

// KillSession terminates every live process owned by a session. proc.KillTree
// is tree-wide and forced (taskkill /T /F on Windows, SIGKILL to the process
// group on unix); we then wait out the grace period and re-check.
func (s *Supervisor) KillSession(sessionID string, grace time.Duration) []int {
	if grace <= 0 {
		grace = 5 * time.Second
	}
	s.mu.Lock()
	set := s.sets[sessionID]
	s.mu.Unlock()
	if set == nil {
		return nil
	}
	var killed []int
	for _, p := range set.List() {
		if !proc.AliveWithIdentity(p.PID, p.StartTime) {
			continue
		}
		s.markKilled(sessionID, p.PID, "session kill")
		killed = append(killed, p.PID)
		_ = proc.KillTree(p.PID)
	}
	// Wait up to grace for all of them to disappear.
	deadline := time.Now().Add(grace)
	for _, pid := range killed {
		ctx, cancel := context.WithTimeout(context.Background(), time.Until(deadline))
		_ = proc.WaitForExit(ctx, pid, 50*time.Millisecond)
		cancel()
	}
	// Final sweep for stragglers.
	for _, pid := range killed {
		if proc.AliveWithIdentity(pid, 0) {
			_ = proc.KillTree(pid)
		}
	}
	return killed
}

// markKilled flags a record so the finalizer reports KILLED.
func (s *Supervisor) markKilled(sessionID string, pid int, reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.recs[pkey(sessionID, pid)]; ok {
		r.Killed = true
		r.KillReason = reason
	}
}

// LiveCount returns live supervised processes for a session.
func (s *Supervisor) LiveCount(sessionID string) int {
	s.mu.Lock()
	set := s.sets[sessionID]
	s.mu.Unlock()
	if set == nil {
		return 0
	}
	n := 0
	for _, p := range set.List() {
		if proc.AliveWithIdentity(p.PID, p.StartTime) {
			n++
		}
	}
	return n
}

// List returns all live records (all sessions).
func (s *Supervisor) List() []*model.ProcInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*model.ProcInfo, 0, len(s.recs))
	for _, r := range s.recs {
		out = append(out, r)
	}
	return out
}

// SessionProcs returns live records for one session.
func (s *Supervisor) SessionProcs(sessionID string) []*model.ProcInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []*model.ProcInfo{}
	for _, r := range s.recs {
		if r.SessionID == sessionID {
			out = append(out, r)
		}
	}
	return out
}

// Snapshot exports live process records for persistence.
func (s *Supervisor) Snapshot() []*model.ProcInfo { return s.List() }

// ReconcileOrphans reaps persisted processes whose session is gone
// (daemon restart recovery, roadmap §21). PID reuse is guarded by the
// recorded start-time identity: a reused PID will not match and is left
// alone rather than killed (we may miss a reaped orphan, never kill an
// innocent process).
func (s *Supervisor) ReconcileOrphans(prev []*model.ProcInfo, liveSessionIDs map[string]bool) []int {
	var reaped []int
	seen := map[int]bool{}
	for _, r := range prev {
		if seen[r.PID] {
			continue
		}
		seen[r.PID] = true
		if liveSessionIDs[r.SessionID] {
			continue
		}
		if r.StartTime == 0 {
			continue // no identity: do not risk killing an innocent PID
		}
		if proc.AliveWithIdentity(r.PID, r.StartTime) {
			if err := proc.KillTree(r.PID); err == nil {
				reaped = append(reaped, r.PID)
			}
		}
	}
	return reaped
}

var _ = context.Background
var _ = bufio.NewScanner
var _ = io.Discard

// WaitHandle exposes completion of one supervised process.
type WaitHandle struct {
	// Done is closed after the process exits and its record is finalized.
	Done chan struct{}

	code   *int
	err    error
	killed bool
	reason string
	dur    time.Duration
}

// Code returns the exit code (nil when terminated by signal/error).
func (h *WaitHandle) Code() *int { return h.code }

// Err returns the Wait error, if any.
func (h *WaitHandle) Err() error { return h.err }

// Killed reports whether the process was killed by the supervisor.
func (h *WaitHandle) Killed() bool { return h.killed }

// Reason returns the kill reason when Killed.
func (h *WaitHandle) Reason() string { return h.reason }

// Duration returns the measured runtime.
func (h *WaitHandle) Duration() time.Duration { return h.dur }

// LaunchWait launches a process and returns a completion handle. The
// handle's Done channel closes once, after record finalization.
func (s *Supervisor) LaunchWait(opt LaunchOptions) (*model.ProcInfo, *WaitHandle, error) {
	h := &WaitHandle{Done: make(chan struct{})}
	inner := opt.OnExit
	wrapped := opt
	wrapped.OnExit = func(st ExitStatus) {
		h.code, h.err, h.killed, h.reason, h.dur = st.Code, st.Err, st.Killed, st.Reason, st.Duration
		if inner != nil {
			inner(st)
		}
		close(h.Done)
	}
	info, err := s.Launch(wrapped)
	if err != nil {
		return nil, nil, err
	}
	return info, h, nil
}

// KillRun kills every live process attributed to a run.
func (s *Supervisor) KillRun(runID string) []int {
	s.mu.Lock()
	var targets []*model.ProcInfo
	for _, r := range s.recs {
		if r.RunID == runID {
			targets = append(targets, r)
		}
	}
	s.mu.Unlock()
	var out []int
	for _, r := range targets {
		s.markKilled(r.SessionID, r.PID, "run kill")
		if err := proc.KillTree(r.PID); err == nil {
			out = append(out, r.PID)
		} else {
			out = append(out, r.PID) // still record the attempt
		}
	}
	return out
}

// PIDExitCode is retained for API symmetry; exit codes are read through
// WaitHandle or the persisted run record.
func (s *Supervisor) PIDExitCode(sessionID string, pid int) *int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.recs[pkey(sessionID, pid)]; ok {
		return r.ExitCode
	}
	return nil
}
