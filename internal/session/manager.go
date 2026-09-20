// Package session implements the execution session manager (roadmap §9).
//
// A session owns every process, lease, port and run created on its behalf.
// When the session ends — normally, by kill, or by crash recovery — all of
// it is cleaned up deterministically.
package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/config"
	"github.com/xy00099/agent-runtime-harness/internal/model"
	"github.com/xy00099/agent-runtime-harness/internal/store"
)

// Manager owns sessions.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*model.Session
	cfg      *config.Config
	st       *store.Store
	envDirs  func(runtimeDir string) (home, temp, xdg string, err error)
	seq      int
}

// New builds the manager.
func New(cfg *config.Config, st *store.Store) *Manager {
	return &Manager{sessions: map[string]*model.Session{}, cfg: cfg, st: st}
}

// SetEnvDirs overrides the env-dir materializer (tests).
func (m *Manager) SetEnvDirs(fn func(runtimeDir string) (home, temp, xdg string, err error)) {
	m.mu.Lock()
	m.envDirs = fn
	m.mu.Unlock()
}

// State is the persistable snapshot.
type State struct {
	Sessions []*model.Session `json:"sessions"`
}

// Snapshot returns current state.
func (m *Manager) Snapshot() *State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// snapshotLocked builds the state; caller holds mu.
func (m *Manager) snapshotLocked() *State {
	s := &State{Sessions: []*model.Session{}}
	for _, sess := range m.sessions {
		s.Sessions = append(s.Sessions, sess)
	}
	return s
}

// Persist writes state.
func (m *Manager) Persist() error { return m.st.Save("sessions", m.Snapshot()) }

// Restore reloads state after daemon restart. The id sequence resumes
// after the highest restored id so a restarted daemon can never reuse a
// session id (which would overwrite history).
func (m *Manager) Restore(s *State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions = map[string]*model.Session{}
	maxSeq := 0
	for _, sess := range s.Sessions {
		m.sessions[sess.ID] = sess
		if n := parseSeq(sess.ID); n > maxSeq {
			maxSeq = n
		}
	}
	if maxSeq > m.seq {
		m.seq = maxSeq
	}
}

// parseSeq extracts the numeric suffix of sess-000123.
func parseSeq(id string) int {
	var n int
	if _, err := fmt.Sscanf(id, "sess-%d", &n); err == nil {
		return n
	}
	return 0
}

// RestoreFromStore loads persisted sessions if any.
func (m *Manager) RestoreFromStore() error {
	var s State
	if err := m.st.Load("sessions", &s); err != nil {
		return nil
	}
	m.Restore(&s)
	return nil
}

// CreateOptions describe a new session.
type CreateOptions struct {
	Owner        string
	Name         string // optional friendly name; id is generated
	WorkspaceID  string // workspace name
	WorkspaceDir string // explicit dir (worktree path or plain dir)
}

// Create materializes a session: id, isolated dirs, READY state.
func (m *Manager) Create(opts CreateOptions) (*model.Session, error) {
	m.mu.Lock()
	sess, err := m.createLocked(opts)
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	snap := m.snapshotLocked()
	m.mu.Unlock()
	if err := m.st.Save("sessions", snap); err != nil {
		return nil, err
	}
	return sess, nil
}

func (m *Manager) createLocked(opts CreateOptions) (*model.Session, error) {
	live := 0
	for _, s := range m.sessions {
		if !s.State.Terminal() {
			live++
		}
	}
	if live >= m.cfg.Runtime.MaxSessions {
		return nil, fmt.Errorf("max_sessions reached (%d live)", m.cfg.Runtime.MaxSessions)
	}
	m.seq++
	id := fmt.Sprintf("sess-%06d", m.seq)
	runtimeDir := filepath.Join(m.cfg.SessionsDir(), id)
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		return nil, fmt.Errorf("create runtime dir: %w", err)
	}
	sess := &model.Session{
		ID:           id,
		Name:         opts.Name,
		Owner:        opts.Owner,
		WorkspaceID:  opts.WorkspaceID,
		WorkspaceDir: opts.WorkspaceDir,
		State:        model.SessionCreated,
		CreatedAt:    time.Now(),
		RuntimeDir:   runtimeDir,
	}
	// Isolated HOME/TMP dirs live under the runtime dir.
	sess.HomeDir = filepath.Join(runtimeDir, "home")
	sess.TempDir = filepath.Join(runtimeDir, "tmp")
	for _, d := range []string{sess.HomeDir, sess.TempDir, filepath.Join(runtimeDir, "xdg")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("prepare isolated dirs: %w", err)
		}
	}
	sess.State = model.SessionReady
	m.sessions[id] = sess
	return sess, nil
}

// Get returns a session by id.
func (m *Manager) Get(id string) (*model.Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

// GetByName finds a session by name (fallback for CLI ergonomics).
func (m *Manager) GetByName(name string) (*model.Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.Name != "" && s.Name == name {
			return s, true
		}
	}
	return nil, false
}

// List returns all sessions sorted by creation time.
func (m *Manager) List() []*model.Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*model.Session, 0, len(m.sessions))
	for _, s := range m.sessions {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

// Mutate applies fn under the manager lock and persists after unlocking.
func (m *Manager) Mutate(id string, fn func(*model.Session) error) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("unknown session %q", id)
	}
	if err := fn(s); err != nil {
		m.mu.Unlock()
		return err
	}
	snap := m.snapshotLocked()
	m.mu.Unlock()
	return m.st.Save("sessions", snap)
}

// MarkRunning flips a session to RUNNING when its first process launches.
func (m *Manager) MarkRunning(id string) {
	_ = m.Mutate(id, func(s *model.Session) error {
		if s.State == model.SessionReady || s.State == model.SessionCreated {
			s.State = model.SessionRunning
			now := time.Now()
			s.StartedAt = &now
		}
		return nil
	})
}

// MarkStopping records the STOPPING transition.
func (m *Manager) MarkStopping(id string) {
	_ = m.Mutate(id, func(s *model.Session) error {
		s.State = model.SessionStopping
		return nil
	})
}

// Finish records a terminal state.
func (m *Manager) Finish(id string, state model.SessionState, reason string) {
	_ = m.Mutate(id, func(s *model.Session) error {
		if !state.Terminal() {
			return fmt.Errorf("finish: %s is not terminal", state)
		}
		s.State = state
		now := time.Now()
		s.StoppedAt = &now
		if state == model.SessionFailed {
			s.Failure = reason
		}
		return nil
	})
}

// Remove drops a session record after full cleanup.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("unknown session %q", id)
	}
	delete(m.sessions, id)
	m.mu.Unlock()
	_ = os.RemoveAll(s.RuntimeDir)
	return m.Persist()
}

// Prune deletes terminal sessions older than ttl (observability housekeeping).
func (m *Manager) Prune(ttl time.Duration) int {
	m.mu.Lock()
	var doomed []string
	cut := time.Now().Add(-ttl)
	for id, s := range m.sessions {
		if s.State.Terminal() && s.StoppedAt != nil && s.StoppedAt.Before(cut) {
			doomed = append(doomed, id)
		}
	}
	m.mu.Unlock()
	for _, id := range doomed {
		_ = m.Remove(id)
	}
	return len(doomed)
}
