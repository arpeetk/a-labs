package launcher

import (
	"context"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
)

// Fake is an in-memory Launcher for tests. It records created runs and lets
// tests inject status (as the operator would) via SetStatus.
type Fake struct {
	mu         sync.Mutex
	Namespaces map[string]bool
	Runs       map[string]*wrenv1.AgentRun // key "ns/name"
}

var _ Launcher = (*Fake)(nil)

// NewFake returns an empty fake launcher.
func NewFake() *Fake {
	return &Fake{Namespaces: map[string]bool{}, Runs: map[string]*wrenv1.AgentRun{}}
}

func key(ns, name string) string { return ns + "/" + name }

func (f *Fake) EnsureNamespace(_ context.Context, ns string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Namespaces[ns] = true
	return nil
}

func (f *Fake) CreateRun(_ context.Context, run *wrenv1.AgentRun) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := key(run.Namespace, run.Name)
	if _, ok := f.Runs[k]; ok {
		return apierrors.NewAlreadyExists(schema.GroupResource{Group: "wren.dev", Resource: "agentruns"}, run.Name)
	}
	cp := run.DeepCopy()
	f.Runs[k] = cp
	return nil
}

func (f *Fake) GetRun(_ context.Context, ns, name string) (*wrenv1.AgentRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	run, ok := f.Runs[key(ns, name)]
	if !ok {
		return nil, apierrors.NewNotFound(schema.GroupResource{Group: "wren.dev", Resource: "agentruns"}, name)
	}
	return run.DeepCopy(), nil
}

func (f *Fake) DeleteRun(_ context.Context, ns, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Runs, key(ns, name))
	return nil
}

// SetStatus updates a stored run's status, simulating the operator writing back.
func (f *Fake) SetStatus(ns, name string, status wrenv1.AgentRunStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if run, ok := f.Runs[key(ns, name)]; ok {
		run.Status = status
	}
}
