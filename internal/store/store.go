// Package store provides crash-safe JSON persistence for daemon state.
//
// Every write is atomic: data is written to a temp file in the same
// directory, fsynced, then renamed over the target. Readers either see the
// old complete file or the new complete file, never a torn write.
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
)

// Store is a directory-backed JSON store. Saves are serialized per key and
// use unique temp names so concurrent writers never collide (Windows cannot
// rename over an open file).
type Store struct {
	dir string
	mu  sync.Mutex
	seq atomic.Int64
}

// New creates the store, making the directory if needed.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}
	return &Store{dir: dir}, nil
}

// Path returns the on-disk path for a key.
func (s *Store) Path(key string) string {
	return filepath.Join(s.dir, key+".json")
}

// Save atomically writes v under key. Writers to the same key are
// serialized; the temp file name is unique per attempt.
func (s *Store) Save(key string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", key, err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.seq.Add(1)
	tmp := fmt.Sprintf("%s.%d.%d.tmp", s.Path(key), os.Getpid(), n)
	defer os.Remove(tmp) // no-op after successful rename
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open tmp %s: %w", tmp, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("write tmp %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	// Windows: Rename to an existing target is fine unless the target is
	// open; the per-key lock plus closed handles make that a non-issue.
	if err := os.Rename(tmp, s.Path(key)); err != nil {
		return fmt.Errorf("rename %s: %w", key, err)
	}
	return nil
}

// Load reads key into out. Missing file returns os.ErrNotExist unwrapped.
func (s *Store) Load(key string, out any) error {
	data, err := os.ReadFile(s.Path(key))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse %s: %w", key, err)
	}
	return nil
}

// Exists reports whether key has a file.
func (s *Store) Exists(key string) bool {
	_, err := os.Stat(s.Path(key))
	return err == nil
}

// Remove deletes the file for key if present.
func (s *Store) Remove(key string) error {
	if err := os.Remove(s.Path(key)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
