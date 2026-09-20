// Package runs implements run records and artifact collection (roadmap §17).
//
// Layout under <state>/runs/<run-id>/:
//
//	metadata.json   structured record (session, tool, command, timing, exit)
//	stdout.log      child stdout
//	stderr.log      child stderr
//	log.txt         adapter/runtime structured lines
//	artifacts/      tool outputs (test reports, builds, screenshots)
//	unity.log       tool-specific logs (Unity editor log)
package runs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/model"
)

// Manager owns run records.
type Manager struct {
	mu   sync.Mutex
	byID map[string]*model.Run
	root string
	seq  int
}

// New builds the manager. root is the runs directory.
func New(root string) (*Manager, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Manager{byID: map[string]*model.Run{}, root: root}, nil
}

// Root returns the runs root.
func (m *Manager) Root() string { return m.root }

// NewRun allocates a run record and its directory.
func (m *Manager) NewRun(sessionID, toolID, toolType, command string) (*model.Run, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	id := fmt.Sprintf("run-%06d", m.seq)
	dir := filepath.Join(m.root, id)
	if err := os.MkdirAll(filepath.Join(dir, "artifacts"), 0o755); err != nil {
		return nil, err
	}
	r := &model.Run{
		ID:        id,
		SessionID: sessionID,
		ToolID:    toolID,
		ToolType:  toolType,
		Command:   command,
		Status:    model.RunQueued,
		StartedAt: time.Now(),
		Dir:       dir,
	}
	m.byID[id] = r
	return r, nil
}

// Get returns a run by id.
func (m *Manager) Get(id string) (*model.Run, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.byID[id]
	return r, ok
}

// Update applies fn under the lock and rewrites metadata.json.
func (m *Manager) Update(id string, fn func(*model.Run)) error {
	m.mu.Lock()
	r, ok := m.byID[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("unknown run %q", id)
	}
	fn(r)
	m.mu.Unlock()
	return m.WriteMetadata(r)
}

// WriteMetadata flushes metadata.json for a run.
func (m *Manager) WriteMetadata(r *model.Run) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.Dir, "metadata.json"), data, 0o644)
}

// Finish marks a run terminal with timing + exit code.
func (m *Manager) Finish(id string, status model.RunStatus, exit *int, errMsg string) error {
	return m.Update(id, func(r *model.Run) {
		now := time.Now()
		r.FinishedAt = &now
		r.Status = status
		r.ExitCode = exit
		r.DurationMS = now.Sub(r.StartedAt).Milliseconds()
		if errMsg != "" {
			r.Error = errMsg
		}
	})
}

// List returns runs sorted newest-first.
func (m *Manager) List(sessionID string) []*model.Run {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []*model.Run
	for _, r := range m.byID {
		if sessionID == "" || r.SessionID == sessionID {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.After(out[j].StartedAt) })
	return out
}

// RestoreFromDisk rebuilds in-memory records by scanning metadata.json files
// after a daemon restart, so `runtime logs <run>` keeps working.
func (m *Manager) RestoreFromDisk() error {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.root, e.Name(), "metadata.json"))
		if err != nil {
			continue
		}
		var r model.Run
		if err := json.Unmarshal(data, &r); err != nil {
			continue
		}
		if r.Dir == "" {
			r.Dir = filepath.Join(m.root, e.Name())
		}
		// Continue numbering after the highest restored id.
		var n int
		if _, err := fmt.Sscanf(e.Name(), "run-%06d", &n); err == nil && n > m.seq {
			m.seq = n
		}
		m.byID[r.ID] = &r
	}
	return nil
}

// AppendLog writes one structured line to run log.txt.
func AppendLog(dir, level, msg string) {
	if dir == "" {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "log.txt"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%s [%s] %s\n", time.Now().UTC().Format(time.RFC3339), level, msg)
}

// Collect copies a produced file into the run artifacts dir.
func Collect(dir, label, srcPath string) (string, error) {
	dst := filepath.Join(dir, "artifacts", label)
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

// Artifacts lists collected artifact files for a run.
func Artifacts(dir string) ([]string, error) {
	root := filepath.Join(dir, "artifacts")
	var out []string
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // tolerate partially-collected runs
		}
		if !info.IsDir() {
			rel, _ := filepath.Rel(root, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return out, nil
}

// ReadLog returns the tail of a run log file ("" if missing).
func ReadLog(dir, name string, tail int) (string, error) {
	if tail <= 0 {
		tail = 200
	}
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	lines := splitLines(string(data))
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return joinLines(lines), nil
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func joinLines(ls []string) string {
	out := ""
	for i, l := range ls {
		if i > 0 {
			out += "\n"
		}
		out += l
	}
	return out
}
