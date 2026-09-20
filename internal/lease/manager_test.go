package lease

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/config"
	"github.com/xy00099/agent-runtime-harness/internal/model"
	"github.com/xy00099/agent-runtime-harness/internal/store"
)

func testCfg() *config.Config {
	cfg := config.Default()
	cfg.Normalize()
	return cfg
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestExclusiveLeaseBlocksSecondAcquirer(t *testing.T) {
	m, err := New(testCfg(), testStore(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	l1, err := m.Acquire(ctx, "unity-license", "sess-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if l1.State != "LEASED" {
		t.Fatalf("want LEASED, got %s", l1.State)
	}
	// Second acquire must time out, not double-grant.
	ctx2, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_, err = m.Acquire(ctx2, "unity-license", "sess-2", 0)
	if err == nil {
		t.Fatal("second acquire should have blocked and timed out")
	}
}

func TestReleaseSatisfiesWaiter(t *testing.T) {
	m, _ := New(testCfg(), testStore(t))
	ctx := context.Background()
	if _, err := m.Acquire(ctx, "unity-license", "sess-1", 0); err != nil {
		t.Fatal(err)
	}
	// sess-2 queues.
	granted := make(chan *model.Lease, 1)
	go func() {
		l, err := m.Acquire(context.Background(), "unity-license", "sess-2", 0)
		if err == nil {
			granted <- l
		}
	}()
	time.Sleep(100 * time.Millisecond) // let it enqueue
	leases := m.List(true)
	if len(leases) != 1 {
		t.Fatalf("want 1 active lease, got %d", len(leases))
	}
	if err := m.Release(leases[0].ID); err != nil {
		t.Fatal(err)
	}
	select {
	case l := <-granted:
		if l.SessionID != "sess-2" {
			t.Fatalf("waiter grant went to %s", l.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter was not granted after release")
	}
}

func TestSessionTerminateAutoAcquire(t *testing.T) {
	// Roadmap acceptance: A acquires, B waits, A dies, B auto-acquires.
	m, _ := New(testCfg(), testStore(t))
	ctx := context.Background()
	_, _ = m.Acquire(ctx, "unity-license", "sess-A", 0)
	granted := make(chan *model.Lease, 1)
	go func() {
		l, err := m.Acquire(context.Background(), "unity-license", "sess-B", 0)
		if err == nil {
			granted <- l
		}
	}()
	time.Sleep(100 * time.Millisecond)
	m.ReleaseAll("sess-A")
	select {
	case l := <-granted:
		if l.SessionID != "sess-B" {
			t.Fatalf("auto-acquire went to %s", l.SessionID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sess-B did not auto-acquire after A died")
	}
}

func TestCapacityMode(t *testing.T) {
	m, _ := New(testCfg(), testStore(t))
	ctx := context.Background()
	// Default build-slot capacity is 2.
	if _, err := m.Acquire(ctx, "build-slot", "s1", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Acquire(ctx, "build-slot", "s2", 0); err != nil {
		t.Fatal(err)
	}
	ctx3, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	if _, err := m.Acquire(ctx3, "build-slot", "s3", 0); err == nil {
		t.Fatal("capacity 2 should block the third acquirer")
	}
	if m.ActiveCount("build-slot") != 2 {
		t.Fatalf("active count: want 2, got %d", m.ActiveCount("build-slot"))
	}
}

func TestTTLSweep(t *testing.T) {
	m, _ := New(testCfg(), testStore(t))
	base := time.Now()
	m.SetNow(func() time.Time { return base })
	if _, err := m.Acquire(context.Background(), "gpu", "s1", time.Second); err != nil {
		t.Fatal(err)
	}
	m.SetNow(func() time.Time { return base.Add(2 * time.Second) })
	expired := m.Sweep(base.Add(2 * time.Second))
	if len(expired) != 1 {
		t.Fatalf("want 1 expired, got %v", expired)
	}
	if m.ActiveCount("gpu") != 0 {
		t.Fatal("expired lease should free the resource")
	}
}

func TestReapOrphans(t *testing.T) {
	m, _ := New(testCfg(), testStore(t))
	ctx := context.Background()
	_, _ = m.Acquire(ctx, "gpu", "sess-gone", 0)
	reaped := m.ReapOrphans(map[string]bool{"sess-alive": true})
	if len(reaped) != 1 {
		t.Fatalf("want 1 reaped lease, got %v", reaped)
	}
	if m.ActiveCount("gpu") != 0 {
		t.Fatal("orphan lease should be released")
	}
}

func TestUnknownResource(t *testing.T) {
	m, _ := New(testCfg(), testStore(t))
	if _, err := m.Acquire(context.Background(), "nope", "s1", 0); err == nil {
		t.Fatal("unknown resource must error")
	}
}
