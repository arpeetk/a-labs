package launcher

import (
	"context"
	"io"
	"strings"
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
	// Logs maps "ns/name/container" → the log body a StreamLogs call returns.
	// A run absent from Logs has no pod backing it (ErrNoPod). LogsErr, when
	// set, is returned by every StreamLogs call (to exercise error paths).
	Logs    map[string]string
	LogsErr error
	// SecretKeys records seeded secret keys as "ns/name/key" → present. When
	// AssumeSecretsPresent is true (the default), SecretHasKey reports any secret
	// not explicitly tracked as present — so the many tests that create
	// credentialed runs need no secret wiring. Tests exercising the
	// missing-credential guard (WS-15 Part A) set AssumeSecretsPresent=false and
	// seed the secrets that DO exist via SetSecret.
	SecretKeys           map[string]bool
	AssumeSecretsPresent bool
	// SecretErr, when set, is returned by every SecretHasKey call (error path).
	SecretErr error
}

var _ Launcher = (*Fake)(nil)

// NewFake returns an empty fake launcher.
func NewFake() *Fake {
	return &Fake{
		Namespaces:           map[string]bool{},
		Runs:                 map[string]*wrenv1.AgentRun{},
		Logs:                 map[string]string{},
		SecretKeys:           map[string]bool{},
		AssumeSecretsPresent: true,
	}
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

func (f *Fake) ListRuns(_ context.Context) ([]wrenv1.AgentRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]wrenv1.AgentRun, 0, len(f.Runs))
	for _, run := range f.Runs {
		out = append(out, *run.DeepCopy())
	}
	return out, nil
}

func (f *Fake) DeleteRun(_ context.Context, ns, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.Runs, key(ns, name))
	return nil
}

func (f *Fake) SecretHasKey(_ context.Context, ns, name, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.SecretErr != nil {
		return false, f.SecretErr
	}
	if present, ok := f.SecretKeys[ns+"/"+name+"/"+key]; ok {
		return present, nil
	}
	return f.AssumeSecretsPresent, nil
}

// SetSecret marks a Secret key present (or absent) in a namespace for the
// missing-credential guard tests.
func (f *Fake) SetSecret(ns, name, key string, present bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.SecretKeys[ns+"/"+name+"/"+key] = present
}

func (f *Fake) StreamLogs(_ context.Context, ns, runID, container string, _ bool) (io.ReadCloser, error) {
	container, err := resolveContainer(container)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.LogsErr != nil {
		return nil, f.LogsErr
	}
	body, ok := f.Logs[key(ns, runID)+"/"+container]
	if !ok {
		return nil, ErrNoPod
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

// SetLogs seeds the log body a StreamLogs call returns for a run's container.
func (f *Fake) SetLogs(ns, name, container, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Logs[key(ns, name)+"/"+container] = body
}

// SetStatus updates a stored run's status, simulating the operator writing back.
func (f *Fake) SetStatus(ns, name string, status wrenv1.AgentRunStatus) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if run, ok := f.Runs[key(ns, name)]; ok {
		run.Status = status
	}
}
