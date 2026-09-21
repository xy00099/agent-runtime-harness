// Package lease implements the generic resource lease manager
// (roadmap §11 Phase 3).
//
// Semantics:
//
//   - exclusive: one holder at a time
//   - shared:    unlimited concurrent holders (best-effort)
//   - capacity:  up to N concurrent holders
//
// All operations are serialized through a single mutex, which is enough for
// a local single-daemon deployment. Leases are persisted on every mutation;
// waiting acquires are queued FIFO (fair scheduling) and auto-satisfied when
// a holder releases, lets a lease expire, or its session dies.
package lease

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xy00099/agent-runtime-harness/internal/config"
	"github.com/xy00099/agent-runtime-harness/internal/model"
	"github.com/xy00099/agent-runtime-harness/internal/store"
)

// Waiter is a queued acquire request.
type Waiter struct {
	SessionID  string
	EnqueuedAt time.Time
	// done is closed exactly once when the lease is granted or ctx ends it.
	done    chan struct{}
	lease   *model.Lease
	err     error
	granted bool
}

// Result returns the granted lease (valid only after Done).
func (w *Waiter) Result() (*model.Lease, error) { return w.lease, w.err }

// Done returns a channel closed when the wait finishes.
func (w *Waiter) Done() <-chan struct{} { return w.done }

// Granted reports whether the wait ended in a grant.
func (w *Waiter) Granted() bool { return w.granted }

// Manager owns all resources and leases.
type Manager struct {
	mu     sync.Mutex
	res    map[string]*model.Resource
	leases map[string]*model.Lease // by lease id
	// holders counts live leases per resource (state LEASED).
	holders map[string]map[string]bool // resourceID -> set(leaseID)
	// waiters per resource, FIFO.
	waiters map[string][]*Waiter
	st      *store.Store
	persist func() // full-state persist hook set by daemon
	nowFn   func() time.Time
	seq     int
}

// New builds a manager seeded from config.
func New(cfg *config.Config, st *store.Store) (*Manager, error) {
	m := &Manager{
		res:     map[string]*model.Resource{},
		leases:  map[string]*model.Lease{},
		holders: map[string]map[string]bool{},
		waiters: map[string][]*Waiter{},
		st:      st,
		nowFn:   time.Now,
	}
	for id, rc := range cfg.Resources {
		mode := model.LeaseMode(rc.Mode)
		if mode == "" {
			mode = model.ModeExclusive
		}
		cap := rc.Capacity
		if mode != model.ModeCapacity {
			cap = 0
		}
		m.res[id] = &model.Resource{
			ID: id, Mode: mode, Capacity: cap, Provider: rc.Provider, Meta: rc.Meta,
		}
	}
	return m, nil
}

// SetPersistHook installs the daemon-wide persistence callback.
func (m *Manager) SetPersistHook(fn func()) { m.persist = fn }

// SetNow overrides the clock (tests).
func (m *Manager) SetNow(fn func() time.Time) { m.mu.Lock(); m.nowFn = fn; m.mu.Unlock() }

// State is a persistable snapshot.
type State struct {
	Resources []*model.Resource `json:"resources"`
	Leases    []*model.Lease    `json:"leases"`
}

// Snapshot returns the current state for persistence.
func (m *Manager) Snapshot() *State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.snapshotLocked()
}

// snapshotLocked builds the state; caller holds mu (or already released it).
func (m *Manager) snapshotLocked() *State {
	s := &State{Resources: []*model.Resource{}, Leases: []*model.Lease{}}
	for _, r := range m.res {
		s.Resources = append(s.Resources, r)
	}
	for _, l := range m.leases {
		s.Leases = append(s.Leases, l)
	}
	return s
}

// Restore replaces in-memory state from a snapshot (daemon restart).
// Non-terminal leases whose session no longer exists are cleaned by the
// daemon via CleanupSession calls; here we just reload them.
func (m *Manager) Restore(s *State) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.res = map[string]*model.Resource{}
	m.leases = map[string]*model.Lease{}
	m.holders = map[string]map[string]bool{}
	m.waiters = map[string][]*Waiter{}
	for _, r := range s.Resources {
		m.res[r.ID] = r
	}
	maxSeq := 0
	for _, l := range s.Leases {
		m.leases[l.ID] = l
		if n := parseLeaseSeq(l.ID); n > maxSeq {
			maxSeq = n
		}
		if l.State == model.LeaseActive {
			if m.holders[l.ResourceID] == nil {
				m.holders[l.ResourceID] = map[string]bool{}
			}
			m.holders[l.ResourceID][l.ID] = true
		}
	}
	if maxSeq > m.seq {
		m.seq = maxSeq
	}
}

// parseLeaseSeq extracts the numeric suffix of lease-00000001.
func parseLeaseSeq(id string) int {
	var n int
	if _, err := fmt.Sscanf(id, "lease-%x", &n); err == nil {
		return n
	}
	return 0
}

// Persist writes the lease state through the store.
func (m *Manager) Persist() error {
	return m.st.Save("leases", m.Snapshot())
}

// RestoreFromStore loads persisted leases if any.
func (m *Manager) RestoreFromStore() error {
	var s State
	if err := m.st.Load("leases", &s); err != nil {
		if err.Error() == "EOF" || strings.Contains(err.Error(), "not exist") {
			return nil
		}
		// Missing file is normal on first boot.
		return nil
	}
	m.Restore(&s)
	return nil
}

func (m *Manager) now() time.Time { return m.nowFn() }

// nextLeaseID allocates a lease id. Caller holds mu.
func (m *Manager) nextLeaseID() string {
	m.seq++
	return fmt.Sprintf("lease-%08x", m.seq)
}

// EnsureResource registers a resource on demand (dynamic discovery, e.g.
// adb device serials). Existing definitions are never overwritten.
func (m *Manager) EnsureResource(id string, mode model.LeaseMode) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.res[id]; ok {
		return
	}
	cap := 0
	if mode == model.ModeCapacity {
		cap = 1
	}
	m.res[id] = &model.Resource{ID: id, Mode: mode, Capacity: cap}
	_ = m.st.Save("leases", m.snapshotLocked())
}

// Resource returns the resource descriptor.
func (m *Manager) Resource(id string) (*model.Resource, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.res[id]
	return r, ok
}

// ListResources returns all known resources.
func (m *Manager) ListResources() []*model.Resource {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*model.Resource, 0, len(m.res))
	for _, r := range m.res {
		out = append(out, r)
	}
	return out
}

// List returns all leases (optionally only active).
func (m *Manager) List(activeOnly bool) []*model.Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*model.Lease{}
	for _, l := range m.leases {
		if activeOnly && l.State != model.LeaseActive {
			continue
		}
		out = append(out, l)
	}
	// stable order by creation
	sortByCreated(out)
	return out
}

func sortByCreated(ls []*model.Lease) {
	for i := 1; i < len(ls); i++ {
		for j := i; j > 0 && ls[j].CreatedAt.Before(ls[j-1].CreatedAt); j-- {
			ls[j], ls[j-1] = ls[j-1], ls[j]
		}
	}
}

// ActiveCount returns the number of active leases on a resource.
func (m *Manager) ActiveCount(resourceID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.holders[resourceID])
}

// QueueDepth returns the number of waiters on a resource.
func (m *Manager) QueueDepth(resourceID string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.waiters[resourceID])
}

// capacityOf returns effective capacity for a resource. Caller holds mu.
func (m *Manager) capacityOf(r *model.Resource) int {
	switch r.Mode {
	case model.ModeExclusive:
		return 1
	case model.ModeShared:
		return 1 << 30 // effectively unlimited
	case model.ModeCapacity:
		if r.Capacity <= 0 {
			return 1
		}
		return r.Capacity
	}
	return 1
}

// grant creates a lease and records it. Caller holds mu.
func (m *Manager) grant(resourceID, sessionID string, ttl time.Duration) *model.Lease {
	now := m.now()
	l := &model.Lease{
		ID:         m.nextLeaseID(),
		ResourceID: resourceID,
		SessionID:  sessionID,
		CreatedAt:  now,
		State:      model.LeaseActive,
	}
	if ttl > 0 {
		l.ExpiresAt = now.Add(ttl)
	}
	m.leases[l.ID] = l
	if m.holders[resourceID] == nil {
		m.holders[resourceID] = map[string]bool{}
	}
	m.holders[resourceID][l.ID] = true
	return l
}

// Acquire blocks until the resource is available for the session or ctx
// ends. ttl <= 0 means no expiry.
func (m *Manager) Acquire(ctx context.Context, resourceID, sessionID string, ttl time.Duration) (*model.Lease, error) {
	m.mu.Lock()
	if _, ok := m.res[resourceID]; !ok {
		m.mu.Unlock()
		return nil, fmt.Errorf("unknown resource %q", resourceID)
	}
	if cur := len(m.holders[resourceID]); cur < m.capacityOf(m.res[resourceID]) {
		l := m.grant(resourceID, sessionID, ttl)
		snap := m.snapshotLocked()
		m.mu.Unlock()
		if err := m.st.Save("leases", snap); err != nil {
			return nil, err
		}
		if m.persist != nil {
			m.persist()
		}
		return l, nil
	}
	// Queue up.
	w := &Waiter{SessionID: sessionID, EnqueuedAt: m.now(), done: make(chan struct{})}
	m.waiters[resourceID] = append(m.waiters[resourceID], w)
	m.mu.Unlock()

	granted := false
	select {
	case <-w.done:
		granted = w.granted
	case <-ctx.Done():
		// Try to dequeue ourselves.
		m.mu.Lock()
		m.removeWaiter(resourceID, w)
		m.mu.Unlock()
		return nil, ctx.Err()
	}
	if !granted {
		return nil, w.err
	}
	return w.lease, nil
}

// removeWaiter drops a waiter and returns whether it was still queued.
// Caller holds mu.
func (m *Manager) removeWaiter(resourceID string, w *Waiter) bool {
	q := m.waiters[resourceID]
	for i, x := range q {
		if x == w {
			m.waiters[resourceID] = append(q[:i], q[i+1:]...)
			return true
		}
	}
	return false
}

// trySatisfy grants the head waiter if capacity allows. Caller holds mu.
// Returns true if a waiter was granted.
func (m *Manager) trySatisfy(resourceID string) bool {
	for len(m.waiters[resourceID]) > 0 {
		r := m.res[resourceID]
		if len(m.holders[resourceID]) >= m.capacityOf(r) {
			return false
		}
		w := m.waiters[resourceID][0]
		m.waiters[resourceID] = m.waiters[resourceID][1:]
		l := m.grant(resourceID, w.SessionID, 0) // waiters get no expiry by default
		w.lease = l
		w.granted = true
		close(w.done)
		return true
	}
	return false
}

// Release ends a lease. The next waiter is granted automatically (roadmap
// acceptance: "Session A terminate -> Session B automatically acquire").
func (m *Manager) Release(leaseID string) error {
	m.mu.Lock()
	l, ok := m.leases[leaseID]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("unknown lease %q", leaseID)
	}
	if l.State != model.LeaseActive {
		m.mu.Unlock()
		return fmt.Errorf("lease %q already %s", leaseID, l.State)
	}
	now := m.now()
	l.State = model.LeaseReleased
	l.ReleasedAt = &now
	delete(m.holders[l.ResourceID], l.ID)
	m.trySatisfy(l.ResourceID)
	snap := m.snapshotLocked()
	m.mu.Unlock()
	if err := m.st.Save("leases", snap); err != nil {
		return err
	}
	return nil
}

// ReleaseAll releases every active lease held by a session (session end,
// crash cleanup). Satisfies queued waiters as a side effect.
func (m *Manager) ReleaseAll(sessionID string) (released []*model.Lease) {
	m.mu.Lock()
	for _, l := range m.leases {
		if l.SessionID == sessionID && l.State == model.LeaseActive {
			now := m.now()
			l.State = model.LeaseReleased
			l.ReleasedAt = &now
			delete(m.holders[l.ResourceID], l.ID)
			released = append(released, l)
			m.trySatisfy(l.ResourceID)
		}
	}
	// Also drop queued waiters owned by the dying session: they will never
	// be satisfied usefully and would otherwise linger.
	for rid, q := range m.waiters {
		kept := q[:0]
		for _, w := range q {
			if w.SessionID == sessionID {
				w.err = fmt.Errorf("session ended while waiting")
				w.granted = false
				close(w.done)
			} else {
				kept = append(kept, w)
			}
		}
		m.waiters[rid] = kept
	}
	snap := m.snapshotLocked()
	m.mu.Unlock()
	if len(released) > 0 {
		_ = m.st.Save("leases", snap)
	}
	return released
}

// Renew extends a lease's expiry.
func (m *Manager) Renew(leaseID string, ttl time.Duration) (*model.Lease, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("renew ttl must be > 0")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leases[leaseID]
	if !ok {
		return nil, fmt.Errorf("unknown lease %q", leaseID)
	}
	if l.State != model.LeaseActive {
		return nil, fmt.Errorf("lease %q is %s, cannot renew", leaseID, l.State)
	}
	l.ExpiresAt = m.now().Add(ttl)
	out := *l
	return &out, nil
}

// ExpireNow forces an active lease into EXPIRED (scheduler tick).
func (m *Manager) ExpireNow(leaseID string) error {
	m.mu.Lock()
	l, ok := m.leases[leaseID]
	if !ok || l.State != model.LeaseActive {
		m.mu.Unlock()
		return fmt.Errorf("lease %q not active", leaseID)
	}
	now := m.now()
	l.State = model.LeaseExpired
	l.ReleasedAt = &now
	delete(m.holders[l.ResourceID], l.ID)
	m.trySatisfy(l.ResourceID)
	snap := m.snapshotLocked()
	m.mu.Unlock()
	return m.st.Save("leases", snap)
}

// Sweep expires leases past their expiry. Returns the expired lease ids.
func (m *Manager) Sweep(now time.Time) []string {
	m.mu.Lock()
	var expired []string
	for _, l := range m.leases {
		if l.State == model.LeaseActive && !l.ExpiresAt.IsZero() && l.ExpiresAt.Before(now) {
			nowv := now
			l.State = model.LeaseExpired
			l.ReleasedAt = &nowv
			delete(m.holders[l.ResourceID], l.ID)
			expired = append(expired, l.ID)
			m.trySatisfy(l.ResourceID)
		}
	}
	snap := m.snapshotLocked()
	m.mu.Unlock()
	if len(expired) > 0 {
		_ = m.st.Save("leases", snap)
	}
	return expired
}

// SessionHolds reports active leases for a session.
func (m *Manager) SessionHolds(sessionID string) []*model.Lease {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []*model.Lease{}
	for _, l := range m.leases {
		if l.SessionID == sessionID && l.State == model.LeaseActive {
			out = append(out, l)
		}
	}
	sortByCreated(out)
	return out
}

// Get returns a lease by id.
func (m *Manager) Get(leaseID string) (*model.Lease, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	l, ok := m.leases[leaseID]
	return l, ok
}

// ForEachActive iterates active leases without copy semantics.
func (m *Manager) ForEachActive(fn func(l *model.Lease)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, l := range m.leases {
		if l.State == model.LeaseActive {
			fn(l)
		}
	}
}

// ReapOrphans releases active leases whose session no longer exists
// (daemon restart recovery).
func (m *Manager) ReapOrphans(liveSessionIDs map[string]bool) []string {
	m.mu.Lock()
	var reaped []string
	for _, l := range m.leases {
		if l.State == model.LeaseActive && !liveSessionIDs[l.SessionID] {
			now := m.now()
			l.State = model.LeaseExpired
			l.ReleasedAt = &now
			delete(m.holders[l.ResourceID], l.ID)
			reaped = append(reaped, l.ID)
			m.trySatisfy(l.ResourceID)
		}
	}
	snap := m.snapshotLocked()
	m.mu.Unlock()
	if len(reaped) > 0 {
		_ = m.st.Save("leases", snap)
	}
	return reaped
}
