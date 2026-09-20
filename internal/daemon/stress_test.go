//go:build integration

package daemon

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"
)

// stressService builds a service with a high session cap for load tests.
func stressService(t *testing.T) *Service {
	t.Helper()
	svc := newTestService(t)
	svc.Cfg.Runtime.MaxSessions = 128
	return svc
}

// TestStressManySessionsRapidCreateDestroy hammers session lifecycle.
func TestStressManySessionsRapidCreateDestroy(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	svc := stressService(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s, err := svc.CreateSession(CreateSessionOpts{Owner: "cli:stress", Name: fmt.Sprintf("s-%d", n)})
			if err != nil {
				t.Errorf("create %d: %v", n, err)
				return
			}
			// Acquire + release a lease on a capacity resource.
			if _, err := svc.AcquireLease(ctx, "cli:stress", s.ID, "build-slot", 0); err != nil {
				t.Errorf("lease %d: %v", n, err)
			}
			if _, err := svc.TerminateSession(ctx, s.ID, "stress"); err != nil {
				t.Errorf("terminate %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()
	sessions := svc.ListSessions()
	for _, s := range sessions {
		if !s.State.Terminal() {
			t.Fatalf("session %s left non-terminal: %s", s.ID, s.State)
		}
	}
	for _, l := range svc.ListLeases("") {
		if l.State == "LEASED" {
			t.Fatalf("leaked active lease: %+v", l)
		}
	}
}

// TestStressLeaseContention: many sessions fight over one exclusive lease.
func TestStressLeaseContention(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	svc := stressService(t)
	ctx := context.Background()
	const n = 8
	grants := make(chan string, n)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			s, err := svc.CreateSession(CreateSessionOpts{Owner: "cli:stress"})
			if err != nil {
				t.Error(err)
				return
			}
			l, err := svc.AcquireLease(ctx, "cli:stress", s.ID, "unity-license", 0)
			if err != nil {
				t.Error(err)
				return
			}
			grants <- l.SessionID
			time.Sleep(50 * time.Millisecond) // hold it briefly
			if err := svc.ReleaseLease("cli:stress", s.ID, l.ID); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	close(grants)
	seen := map[string]bool{}
	for sid := range grants {
		if seen[sid] {
			t.Fatalf("two simultaneous grants to %s", sid)
		}
		seen[sid] = true
	}
	if elapsed := time.Since(start); elapsed < 8*50*time.Millisecond {
		t.Fatalf("contention resolved suspiciously fast (%v); lease was not serialized", elapsed)
	}
}

// TestStressParallelRuns: many concurrent tool executions with logs.
func TestStressParallelRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	svc := stressService(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			s, err := svc.CreateSession(CreateSessionOpts{Owner: "cli:stress"})
			if err != nil {
				errs <- err
				return
			}
			run, err := svc.Execute(ctx, ExecuteOptions{
				Owner: "cli:stress", SessionID: s.ID, ToolID: "echo-env",
				Command: "run", Args: envDumpArgs(), Wait: true,
			})
			if err != nil {
				errs <- err
				return
			}
			if run.Status != "SUCCEEDED" {
				errs <- fmt.Errorf("run %s: %s", run.ID, run.Status)
			}
			if _, err := svc.TerminateSession(ctx, s.ID, "stress"); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if runtime.GOOS == "windows" {
		// Windows file cleanup races; give the sweeper a beat.
		time.Sleep(100 * time.Millisecond)
	}
}
