// Package proc provides platform-neutral process supervision primitives:
// launch into an isolated environment, recursive tree kill, and OS-native
// process identity (start time) to guard against PID reuse (roadmap §21).
//
// Platform strategy:
//
//   - unix:    children get their own process group (setpgid); killing the
//     group reaches the whole tree.
//   - windows: children are assigned to a Job Object with
//     JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE, so even if the daemon
//     itself crashes, the OS reaps the whole tree.
package proc

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"
)

// Set is a supervised process set for one session.
type Set struct {
	mu    sync.Mutex
	procs map[int]*Info
}

// Info tracks one supervised process.
type Info struct {
	PID       int
	StartTime uint64 // kernel start-time identity; guards against PID reuse
	Exe       string
	Cmdline   []string
	SessionID string
	Label     string
	StartedAt time.Time
}

// NewSet creates an empty set.
func NewSet() *Set { return &Set{procs: map[int]*Info{}} }

// Add records a process.
func (s *Set) Add(i *Info) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.procs[i.PID] = i
}

// Remove drops a process record.
func (s *Set) Remove(pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.procs, pid)
}

// List returns a snapshot of records.
func (s *Set) List() []*Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*Info, 0, len(s.procs))
	for _, p := range s.procs {
		out = append(out, p)
	}
	return out
}

// Get returns the record for a pid.
func (s *Set) Get(pid int) (*Info, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.procs[pid]
	return p, ok
}

// Count returns the number of tracked processes.
func (s *Set) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.procs)
}

// AliveWithIdentity reports whether pid is alive and (when startTime > 0)
// still the same process instance we recorded (PID-reuse guard).
func AliveWithIdentity(pid int, startTime uint64) bool {
	if pid <= 0 {
		return false
	}
	if !processExists(pid) {
		return false
	}
	if startTime == 0 {
		return true
	}
	return ProcessIdentity(pid) == startTime
}

// WaitForExit polls until the pid no longer exists or ctx ends.
func WaitForExit(ctx context.Context, pid int, poll time.Duration) error {
	if poll <= 0 {
		poll = 100 * time.Millisecond
	}
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		if !processExists(pid) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Env renders a map as an exec environment slice.
func Env(pairs map[string]string) []string {
	out := make([]string, 0, len(pairs))
	for k, v := range pairs {
		out = append(out, k+"="+v)
	}
	return out
}

// SyncStatus describes the platform isolation mechanisms in use.
type SyncStatus struct {
	JobObject bool // windows: assigned to a kill-on-close job object
	ProcGroup bool // unix: own process group
}

var _ = fmt.Sprintf
var _ = os.Getpid
