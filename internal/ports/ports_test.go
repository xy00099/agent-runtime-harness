package ports

import (
	"path/filepath"
	"testing"

	"github.com/xy00099/agent-runtime-harness/internal/store"
)

func newManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(st)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParallelAllocationNeverCollides(t *testing.T) {
	m := newManager(t)
	seen := map[int]bool{}
	for i := 0; i < 50; i++ {
		a, err := m.Acquire("sess-x", "tcp", "")
		if err != nil {
			t.Fatal(err)
		}
		if seen[a.Port] {
			t.Fatalf("port %d allocated twice", a.Port)
		}
		seen[a.Port] = true
	}
}

func TestReleaseBySession(t *testing.T) {
	m := newManager(t)
	a, _ := m.Acquire("sess-1", "tcp", "debug")
	if !m.Taken(a.Port) {
		t.Fatal("port should be taken")
	}
	released := m.ReleaseBySession("sess-1")
	if len(released) != 1 || released[0].Port != a.Port {
		t.Fatalf("release: %+v", released)
	}
	if m.Taken(a.Port) {
		t.Fatal("port should be free again")
	}
	// The freed port can be re-acquired (listener closed).
	b, err := m.Acquire("sess-2", "tcp", "")
	if err != nil {
		t.Fatal(err)
	}
	if b.Port == 0 {
		t.Fatal("expected a real port")
	}
}

func TestReapOrphans(t *testing.T) {
	m := newManager(t)
	a, _ := m.Acquire("sess-dead", "tcp", "")
	reaped := m.ReapOrphans(map[string]bool{})
	if len(reaped) != 1 || reaped[0] != a.Port {
		t.Fatalf("reap: %v", reaped)
	}
	if m.Taken(a.Port) {
		t.Fatal("orphan port should be freed")
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, _ := store.New(filepath.Join(dir, "state"))
	m, _ := New(st)
	a, _ := m.Acquire("sess-1", "tcp", "api")
	if err := m.Persist(); err != nil {
		t.Fatal(err)
	}
	m2, _ := New(st)
	if err := m2.RestoreFromStore(); err != nil {
		t.Fatal(err)
	}
	if !m2.Taken(a.Port) {
		t.Fatal("restore lost the allocation")
	}
}
