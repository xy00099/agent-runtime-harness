//go:build integration

package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/store"
)

func newTestRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("-c", "user.email=t@t", "-c", "user.name=t", "commit", "-m", "init")
	return dir
}

func newManager(t *testing.T) *Manager {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	seq := 0
	return New(st, func() string { seq++; return "ws" })
}

// TestCreatePersistsWithoutDeadlock guards the manager's own history: an
// early version persisted while holding m.mu and deadlocked every RPC.
func TestCreatePersistsWithoutDeadlock(t *testing.T) {
	m := newManager(t)
	repo := newTestRepo(t)
	done := make(chan *struct{ err error }, 1)
	go func() {
		w, err := m.Create(CreateOptions{Repo: repo, Branch: "agent/fix-a"})
		if err == nil && w == nil {
			err = errNil()
		}
		done <- &struct{ err error }{err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("create: %v", r.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Create deadlocked (persist under lock regression)")
	}
	if len(m.List()) != 1 {
		t.Fatalf("workspace not listed: %d", len(m.List()))
	}
}

func errNil() error { return &staticErr{"nil workspace"} }

type staticErr struct{ s string }

func (e *staticErr) Error() string { return e.s }

// TestExistingBranchChecksOut guards: a branch left over from a previous
// (failed) create must be checked out, not fatal.
func TestExistingBranchChecksOut(t *testing.T) {
	m := newManager(t)
	repo := newTestRepo(t)
	w1, err := m.Create(CreateOptions{Repo: repo, Branch: "agent/keep"})
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a stale registration+directory: delete the worktree dir
	// without telling git, then create another workspace on the SAME branch.
	if err := os.RemoveAll(w1.Path); err != nil {
		t.Fatal(err)
	}
	w2, err := m.Create(CreateOptions{Repo: repo, Branch: "agent/keep", Name: "second"})
	if err != nil {
		t.Fatalf("second create on existing branch: %v", err)
	}
	if w2.Path == "" || w2.Path == w1.Path {
		t.Fatalf("second workspace path: %q (was %q)", w2.Path, w1.Path)
	}
}

// TestNormalizePathMSYS guards the git-bash path forms.
func TestNormalizePathMSYS(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows path forms")
	}
	cases := map[string]string{
		"/c/Users/me":   "C:/Users/me",
		"/cygdrive/d/x": "D:/x",
	}
	eq := func(a, b string) bool {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	for in, want := range cases {
		if got := normalizePath(in); !eq(got, want) {
			t.Errorf("normalizePath(%q) = %q, want %q", in, got, want)
		}
	}
}
