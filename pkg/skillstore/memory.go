package skillstore

import (
	"context"
	"sync"

	"github.com/matty-v/kyber/pkg/skillscan"
)

// MemoryStore is the in-memory Store used when no Postgres URL is configured
// (dev, tests, single-replica installs). Reports do not survive a control-plane
// restart; agents re-report on their next boot or identity sync.
type MemoryStore struct {
	mu      sync.RWMutex
	reports map[string]*skillscan.Report
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{reports: make(map[string]*skillscan.Report)}
}

// Put stores a deep copy so a caller mutating its report after Put cannot
// change what a concurrent reader sees.
func (m *MemoryStore) Put(_ context.Context, agentName string, report *skillscan.Report) error {
	if report == nil {
		return errNilReport
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reports[agentName] = cloneReport(report)
	return nil
}

// Get returns a copy of the stored report, or ErrNotFound.
func (m *MemoryStore) Get(_ context.Context, agentName string) (*skillscan.Report, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rep, ok := m.reports[agentName]
	if !ok {
		return nil, ErrNotFound
	}
	return cloneReport(rep), nil
}

// Delete removes the agent's report, if any.
func (m *MemoryStore) Delete(_ context.Context, agentName string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.reports, agentName)
	return nil
}

// cloneReport deep-copies every slice so stored and returned reports never
// share backing arrays with the caller.
func cloneReport(in *skillscan.Report) *skillscan.Report {
	out := *in
	out.Issues = append([]skillscan.Issue(nil), in.Issues...)
	out.Skills = make([]skillscan.Skill, len(in.Skills))
	for i, s := range in.Skills {
		s.Linked = append([]string(nil), s.Linked...)
		s.Issues = append([]skillscan.Issue(nil), s.Issues...)
		out.Skills[i] = s
	}
	return &out
}
