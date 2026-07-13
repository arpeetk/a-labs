package store

import (
	"context"
	"sort"
	"sync"
)

// Memory is a thread-safe in-memory Store for tests and local development.
type Memory struct {
	mu       sync.RWMutex
	projects map[string]*Project
	runs     map[string]*Run
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		projects: map[string]*Project{},
		runs:     map[string]*Run{},
	}
}

var _ Store = (*Memory)(nil)

func (m *Memory) CreateProject(_ context.Context, p *Project) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.projects[p.Name]; ok {
		return ErrExists
	}
	cp := *p
	m.projects[p.Name] = &cp
	return nil
}

func (m *Memory) GetProject(_ context.Context, name string) (*Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.projects[name]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *Memory) ListProjects(_ context.Context) ([]*Project, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Project, 0, len(m.projects))
	for _, p := range m.projects {
		cp := *p
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (m *Memory) CreateRun(_ context.Context, r *Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[r.ID]; ok {
		return ErrExists
	}
	cp := *r
	m.runs[r.ID] = &cp
	return nil
}

func (m *Memory) GetRun(_ context.Context, id string) (*Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.runs[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (m *Memory) ListRuns(_ context.Context, f RunFilter) ([]*Run, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Run, 0, len(m.runs))
	for _, r := range m.runs {
		if f.User != "" && r.User != f.User {
			continue
		}
		if f.Project != "" && r.Project != f.Project {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	// Newest first, tie-break by ID for determinism.
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out, nil
}

func (m *Memory) UpdateRun(_ context.Context, r *Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.runs[r.ID]; !ok {
		return ErrNotFound
	}
	cp := *r
	m.runs[r.ID] = &cp
	return nil
}
