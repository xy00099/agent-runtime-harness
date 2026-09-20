// Package ports implements exclusive port and IPC-endpoint allocation
// (roadmap §12). Parallel sessions never receive the same exclusive port;
// allocations are released automatically when the owning session ends.
package ports

import (
	"fmt"
	"net"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/xy00099/agent-runtime-harness/internal/store"
)

// Allocation is one port allocation.
type Allocation struct {
	Port       int    `json:"port"`
	SessionID  string `json:"session_id"`
	Protocol   string `json:"protocol"` // tcp | udp
	Name       string `json:"name"`     // optional label, e.g. "debug"
	UnixSocket string `json:"unix_socket,omitempty"`
}

// Manager owns the allocation table.
type Manager struct {
	mu sync.Mutex
	// taken tracks ports currently handed out, by port number.
	taken map[int]*Allocation
	// liveness: keep listeners on tcp allocations so the port cannot be
	// grabbed by anyone else while leased.
	held map[int]*heldListener
	st   *store.Store
	seq  int
}

type heldListener struct {
	ln net.Listener
	// conn keeps UDP placeholders alive.
	conn net.PacketConn
}

// Close releases the held listener/conn.
func (h *heldListener) Close() {
	if h == nil {
		return
	}
	if h.ln != nil {
		h.ln.Close()
	}
	if h.conn != nil {
		h.conn.Close()
	}
}

// New builds the manager.
func New(st *store.Store) (*Manager, error) {
	return &Manager{
		taken: map[int]*Allocation{},
		held:  map[int]*heldListener{},
		st:    st,
	}, nil
}

// State is the persistable snapshot.
type State struct {
	Allocations []*Allocation `json:"allocations"`
}

// Snapshot returns current allocations.
func (m *Manager) Snapshot() *State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// snapshotLocked builds the state; caller holds mu.
func (m *Manager) snapshotLocked() *State {
	s := &State{Allocations: []*Allocation{}}
	for _, a := range m.taken {
		s.Allocations = append(s.Allocations, a)
	}
	return s
}

// Persist writes state.
func (m *Manager) Persist() error { return m.st.Save("ports", m.Snapshot()) }

// Restore reloads state after daemon restart. Ports held by sessions that
// no longer exist are released by ReapOrphans.
func (m *Manager) Restore(s *State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.taken = map[int]*Allocation{}
	m.held = map[int]*heldListener{}
	for _, a := range s.Allocations {
		m.taken[a.Port] = a
	}
}

// RestoreFromStore loads persisted allocations if any.
func (m *Manager) RestoreFromStore() error {
	var s State
	if err := m.st.Load("ports", &s); err != nil {
		return nil // missing file on first boot is normal
	}
	m.Restore(&s)
	return nil
}

// findFreePort asks the OS for a free loopback port by binding :0.
// The caller must keep the returned holder open until the port is released.
func findFreePort(protocol string) (int, *heldListener, error) {
	if protocol == "udp" {
		addr, err := net.ResolveUDPAddr("udp", "127.0.0.1:0")
		if err != nil {
			return 0, nil, err
		}
		c, err := net.ListenUDP("udp", addr)
		if err != nil {
			return 0, nil, err
		}
		return c.LocalAddr().(*net.UDPAddr).Port, &heldListener{conn: c}, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, nil, err
	}
	return ln.Addr().(*net.TCPAddr).Port, &heldListener{ln: ln}, nil
}

// Acquire allocates an exclusive port for a session. name becomes part of
// the suggested env var (AGENT_RUNTIME_PORT_<NAME>).
func (m *Manager) Acquire(sessionID, protocol, name string) (*Allocation, error) {
	if protocol == "" {
		protocol = "tcp"
	}
	if protocol != "tcp" && protocol != "udp" {
		return nil, fmt.Errorf("protocol must be tcp or udp")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// Try a few times: the OS picks the port, we re-check our table.
	for attempt := 0; attempt < 64; attempt++ {
		port, held, err := findFreePort(protocol)
		if err != nil {
			return nil, fmt.Errorf("find free port: %w", err)
		}
		if _, busy := m.taken[port]; busy {
			held.Close()
			continue
		}
		a := &Allocation{Port: port, SessionID: sessionID, Protocol: protocol, Name: name}
		m.taken[port] = a
		m.held[port] = held
		snap := m.snapshotLocked()
		_ = m.st.Save("ports", snap) // store is a different lock; safe under mu
		return a, nil
	}
	return nil, fmt.Errorf("no free port found after 64 attempts")
}

// ReleaseBySession releases every allocation owned by a session.
func (m *Manager) ReleaseBySession(sessionID string) []*Allocation {
	m.mu.Lock()
	var released []*Allocation
	for port, a := range m.taken {
		if a.SessionID == sessionID {
			released = append(released, a)
			delete(m.taken, port)
			if h, ok := m.held[port]; ok {
				h.Close()
				delete(m.held, port)
			}
		}
	}
	m.mu.Unlock()
	if len(released) > 0 {
		_ = m.Persist()
	}
	return released
}

// ReleasePort releases one specific port.
func (m *Manager) ReleasePort(port int) bool {
	m.mu.Lock()
	a, ok := m.taken[port]
	if !ok {
		m.mu.Unlock()
		return false
	}
	delete(m.taken, port)
	if h, ok2 := m.held[port]; ok2 {
		h.Close()
		delete(m.held, port)
	}
	m.mu.Unlock()
	_ = m.Persist()
	return a != nil
}

// List returns all allocations.
func (m *Manager) List() []*Allocation {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Allocation, 0, len(m.taken))
	for _, a := range m.taken {
		out = append(out, a)
	}
	return out
}

// Taken reports whether a port is currently allocated.
func (m *Manager) Taken(port int) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.taken[port]
	return ok
}

// ReapOrphans releases ports owned by dead sessions.
func (m *Manager) ReapOrphans(liveSessionIDs map[string]bool) []int {
	m.mu.Lock()
	var reaped []int
	for port, a := range m.taken {
		if !liveSessionIDs[a.SessionID] {
			reaped = append(reaped, port)
			delete(m.taken, port)
			if h, ok := m.held[port]; ok {
				h.Close()
				delete(m.held, port)
			}
		}
	}
	m.mu.Unlock()
	if len(reaped) > 0 {
		_ = m.Persist()
	}
	return reaped
}

// AllocateSocketPath reserves a session-scoped unix socket path under
// runtimeDir and returns it. The path is NOT created here; callers create
// their listener at it. Cleanup removes it with the session.
func AllocateSocketPath(runtimeDir, name string) (string, error) {
	if name == "" {
		name = "sock"
	}
	if runtime.GOOS == "windows" {
		// Named pipes live in a different namespace; return a pipe path.
		return fmt.Sprintf(`\\.\pipe\arh-%s`, sanitize(name)), nil
	}
	return filepath.Join(runtimeDir, fmt.Sprintf("%s.sock", sanitize(name))), nil
}

func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if r == '.' || r == '-' || r == '_' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			out = append(out, r)
		} else {
			out = append(out, '-')
		}
	}
	return string(out)
}
