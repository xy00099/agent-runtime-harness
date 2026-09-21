// Package workspace integrates git worktrees with runtime sessions
// (roadmap §14). Workspace lifetime is independent of session lifetime:
// workspaces survive, sessions are disposable.
package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/model"
	"github.com/xy00099/agent-runtime-harness/internal/store"
)

// Manager owns workspaces.
type Manager struct {
	mu   sync.Mutex
	ws   map[string]*model.Workspace
	st   *store.Store
	next func() string
}

// New builds the manager. next allocates workspace ids.
func New(st *store.Store, next func() string) *Manager {
	return &Manager{ws: map[string]*model.Workspace{}, st: st, next: next}
}

// State is the persistable snapshot.
type State struct {
	Workspaces []*model.Workspace `json:"workspaces"`
}

// Snapshot returns current state.
func (m *Manager) Snapshot() *State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// snapshotLocked builds the state; caller holds mu.
func (m *Manager) snapshotLocked() *State {
	s := &State{Workspaces: []*model.Workspace{}}
	for _, w := range m.ws {
		s.Workspaces = append(s.Workspaces, w)
	}
	return s
}

// Persist writes state.
func (m *Manager) Persist() error { return m.st.Save("workspaces", m.Snapshot()) }

// Restore reloads state after daemon restart.
func (m *Manager) Restore(s *State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ws = map[string]*model.Workspace{}
	for _, w := range s.Workspaces {
		m.ws[w.ID] = w
	}
}

// RestoreFromStore loads persisted workspaces if any.
func (m *Manager) RestoreFromStore() error {
	var s State
	if err := m.st.Load("workspaces", &s); err != nil {
		return nil
	}
	m.Restore(&s)
	return nil
}

// CreateOptions describe a new workspace.
type CreateOptions struct {
	Repo   string // path or URL of an existing git repo
	Branch string // branch to check out in the worktree; created if missing
	Name   string // friendly name; defaults to branch basename
	// Detach creates a detached worktree (no new branch).
	Detach bool
	// Path places the worktree at an explicit directory.
	Path string
}

// normalizePath makes sense of the paths users actually type on Windows.
// Git-bash/MSYS expands ~ to a POSIX-style path (/c/Users/...); a naive
// filepath.Abs treats that as RELATIVE and produces C:\c\Users\...
// Detected forms: /c/..., /cygdrive/c/..., plus plain ~/... and ~.
func normalizePath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	if runtime.GOOS == "windows" {
		if strings.HasPrefix(p, "/cygdrive/") && len(p) > len("/cygdrive/x") {
			rest := strings.TrimPrefix(p, "/cygdrive/")
			return string(rest[0]) + ":" + rest[1:]
		}
		if len(p) > 2 && p[0] == '/' && isDriveLetter(p[1]) && p[2] == '/' {
			// /c/Users/... -> C:\Users\...
			return string(p[1]) + ":" + p[2:]
		}
	}
	return p
}

func isDriveLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// Create makes a git worktree for the repo.
func (m *Manager) Create(opts CreateOptions) (*model.Workspace, error) {
	m.mu.Lock()
	w, err := m.createLocked(opts)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	snap := m.snapshotLocked()
	m.mu.Unlock()
	if err := m.st.Save("workspaces", snap); err != nil {
		return nil, err
	}
	return w, nil
}

func (m *Manager) createLocked(opts CreateOptions) (*model.Workspace, error) {
	if opts.Repo == "" {
		return nil, fmt.Errorf("repo is required")
	}
	repo, err := filepath.Abs(normalizePath(opts.Repo))
	if err != nil {
		return nil, fmt.Errorf("resolve repo path: %w", err)
	}
	if err := gitAt(repo, "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("%s is not a git repository: %w", repo, err)
	}
	// Self-heal worktrees whose target directories vanished (previous failed
	// runs, manual deletes): prune stale registrations before adding ours.
	_ = gitAt(repo, "worktree", "prune")
	name := opts.Name
	if name == "" && opts.Branch != "" {
		name = filepath.Base(strings.TrimSuffix(opts.Branch, "/")) // e.g. agent/foo -> foo
	}
	if name == "" {
		name = filepath.Base(repo)
	}
	if _, exists := m.ws[name]; exists {
		return nil, fmt.Errorf("workspace %q already exists", name)
	}
	id := m.next()
	// Worktree location: <state>/worktrees/<id> unless Path given.
	wtRoot := filepath.Join(filepath.Dir(m.st.Path("workspaces")), "..", "..", "worktrees")
	wtRoot = normalizeRoot(wtRoot)
	if opts.Path != "" {
		wtRoot = opts.Path
	}
	// The target directory may already exist (a previous failed create left
	// it, or two managers raced). Pick a fresh suffix instead of failing.
	target := filepath.Join(wtRoot, fmt.Sprintf("%s-%s", sanitize(name), id))
	for i := 0; ; i++ {
		if _, err := os.Stat(target); os.IsNotExist(err) {
			break
		}
		target = filepath.Join(wtRoot, fmt.Sprintf("%s-%s-%d", sanitize(name), id, i+1))
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, fmt.Errorf("create worktree parent: %w", err)
	}

	// Branch strategy: create a ref so two worktrees never share a branch.
	// An existing branch is not an error: check it out into the worktree.
	args := []string{"worktree", "add"}
	switch {
	case opts.Detach:
		args = append(args, "--detach", target)
		if opts.Branch != "" {
			args = append(args, opts.Branch)
		}
	default:
		if branchExists(repo, opts.Branch) {
			args = append(args, target, opts.Branch) // checkout existing branch
		} else {
			args = append(args, "-b", opts.Branch, target)
		}
	}
	if err := gitAt(repo, args...); err != nil {
		return nil, fmt.Errorf("git worktree add: %w\n(output: run manually: git -C %s %v)", err, repo, strings.Join(args, " "))
	}
	w := &model.Workspace{
		ID:        id,
		Name:      name,
		Repo:      repo,
		Branch:    opts.Branch,
		Path:      target,
		CreatedAt: time.Now(),
	}
	m.ws[name] = w
	return w, nil
}

// normalizeRoot flattens a ".."-joined path.
func normalizeRoot(p string) string {
	return filepath.Clean(p)
}

// CreatePlain registers a plain directory as a workspace (no git). Useful
// for non-git projects and tests.
func (m *Manager) CreatePlain(dir, name string) (*model.Workspace, error) {
	m.mu.Lock()
	w, err := m.createPlainLocked(dir, name)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	snap := m.snapshotLocked()
	m.mu.Unlock()
	if err := m.st.Save("workspaces", snap); err != nil {
		return nil, err
	}
	return w, nil
}

func (m *Manager) createPlainLocked(dir, name string) (*model.Workspace, error) {
	if dir == "" {
		return nil, fmt.Errorf("dir is required")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("plain workspace dir %s does not exist", abs)
	}
	if name == "" {
		name = filepath.Base(abs)
	}
	if _, exists := m.ws[name]; exists {
		return nil, fmt.Errorf("workspace %q already exists", name)
	}
	id := m.next()
	w := &model.Workspace{
		ID:        id,
		Name:      name,
		Repo:      abs,
		Path:      abs,
		CreatedAt: time.Now(),
		Plain:     true,
	}
	m.ws[name] = w
	snap := m.snapshotLocked()
	m.mu.Unlock()
	if err := m.st.Save("workspaces", snap); err != nil {
		return nil, err
	}
	return w, nil
}

// GetByName returns a workspace by name.
func (m *Manager) GetByName(name string) (*model.Workspace, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.ws[name]
	return w, ok
}

// GetByID returns a workspace by id.
func (m *Manager) GetByID(id string) (*model.Workspace, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.ws {
		if w.ID == id {
			return w, true
		}
	}
	return nil, false
}

// List returns all workspaces sorted by name.
func (m *Manager) List() []*model.Workspace {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*model.Workspace, 0, len(m.ws))
	for _, w := range m.ws {
		out = append(out, w)
	}
	// sort by name
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Name < out[j-1].Name; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// MarkSession records the last session that used a workspace.
func (m *Manager) MarkSession(id, sessionID string) {
	m.mu.Lock()
	if w, ok := m.ws[id]; ok {
		w.LastSessionID = sessionID
	}
	snap := m.snapshotLocked()
	m.mu.Unlock()
	_ = m.st.Save("workspaces", snap)
}

// Remove deletes a workspace registration and prunes its worktree.
// refuses when a live session is using it (the daemon checks liveness).
func (m *Manager) Remove(name string, prune bool) (*model.Workspace, error) {
	m.mu.Lock()
	w, ok := m.ws[name]
	if !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("unknown workspace %q", name)
	}
	delete(m.ws, name)
	m.mu.Unlock()
	if prune && !w.Plain {
		// git worktree remove is safe: refuses dirty worktrees without --force.
		_ = gitAt(w.Repo, "worktree", "remove", w.Path)
		_ = gitAt(w.Repo, "worktree", "prune")
	}
	if err := m.Persist(); err != nil {
		return w, err
	}
	return w, nil
}

// branchExists reports whether a local branch exists in the repo.
func branchExists(repo, branch string) bool {
	if branch == "" {
		return false
	}
	return gitAt(repo, "show-ref", "--verify", "--quiet", "refs/heads/"+branch) == nil
}

func gitAt(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %w: %s", args, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
