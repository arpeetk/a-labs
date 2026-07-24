package install

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// FakeKube is the in-memory Kube for tests. It records every mutation in call
// order (Calls) so tests can assert the install's sequencing, and lets a test
// inject a server version or a failure on any operation.
type FakeKube struct {
	mu sync.Mutex

	Version string // returned by ServerVersion; default "1.31"

	// FailOn, when set to an operation name ("ServerVersion", "ApplyManifests",
	// ...), makes that operation return Err.
	FailOn string
	Err    error

	Calls   []string
	Secrets map[string]map[string]string // "ns/name" → data
}

func NewFakeKube() *FakeKube {
	return &FakeKube{Version: "1.31", Secrets: map[string]map[string]string{}}
}

func (f *FakeKube) fail(op string) error {
	f.Calls = append(f.Calls, op)
	if f.FailOn == op {
		if f.Err != nil {
			return f.Err
		}
		return fmt.Errorf("injected %s failure", op)
	}
	return nil
}

func (f *FakeKube) ServerVersion(ctx context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("ServerVersion"); err != nil {
		return "", err
	}
	return f.Version, nil
}

func (f *FakeKube) ApplyManifests(ctx context.Context, manifests []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail("ApplyManifests")
}

func (f *FakeKube) EnsureNamespace(ctx context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail("EnsureNamespace:" + name)
}

func (f *FakeKube) UpsertSecret(ctx context.Context, ns, name string, data map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail("UpsertSecret:" + name); err != nil {
		return err
	}
	k := ns + "/" + name
	if f.Secrets[k] == nil {
		f.Secrets[k] = map[string]string{}
	}
	for key, v := range data {
		f.Secrets[k][key] = v
	}
	return nil
}

func (f *FakeKube) OverrideImages(ctx context.Context, registry, tag string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail("OverrideImages:" + registry + ":" + tag)
}

func (f *FakeKube) SetServiceType(ctx context.Context, ns, name, svcType string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail("SetServiceType:" + name + "=" + svcType)
}

func (f *FakeKube) WaitDeployments(ctx context.Context, ns string, names []string, timeout time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail("WaitDeployments:" + strings.Join(names, ","))
}

func (f *FakeKube) DeleteNamespace(ctx context.Context, name string, timeout time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail("DeleteNamespace:" + name)
}

func (f *FakeKube) DeleteClusterScoped(ctx context.Context, manifests []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.fail("DeleteClusterScoped")
}

// HasCall reports whether a recorded call starts with the given prefix.
func (f *FakeKube) HasCall(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.Calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

// FakeRunner is the in-memory Runner for tests. It records invocations and
// serves canned Output responses keyed by the joined command line.
type FakeRunner struct {
	mu sync.Mutex

	// Tools is the set of binaries LookPath reports present (default: all asked
	// for — tests that want "missing tool" set Tools explicitly).
	Tools map[string]bool
	// Outputs maps "name arg1 arg2…" → stdout. A missing key is an error,
	// matching how a failed command surfaces.
	Outputs map[string]string
	// FailRun, when set to a command prefix ("docker build"), makes matching
	// Run calls return Err.
	FailRun string
	Err     error

	Runs []string
}

func NewFakeRunner() *FakeRunner {
	return &FakeRunner{Outputs: map[string]string{}}
}

func (f *FakeRunner) LookPath(name string) bool {
	if f.Tools == nil {
		return true
	}
	return f.Tools[name]
}

func (f *FakeRunner) Run(ctx context.Context, name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.Join(append([]string{name}, args...), " ")
	f.Runs = append(f.Runs, line)
	if f.FailRun != "" && strings.HasPrefix(line, f.FailRun) {
		if f.Err != nil {
			return f.Err
		}
		return fmt.Errorf("injected failure: %s", line)
	}
	return nil
}

func (f *FakeRunner) Output(ctx context.Context, name string, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.Join(append([]string{name}, args...), " ")
	if out, ok := f.Outputs[line]; ok {
		return out, nil
	}
	return "", fmt.Errorf("no canned output for %q", line)
}

// Ran reports whether any recorded Run starts with the prefix.
func (f *FakeRunner) Ran(prefix string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.Runs {
		if strings.HasPrefix(r, prefix) {
			return true
		}
	}
	return false
}

// SortedRuns returns the recorded Runs sorted (order-insensitive assertions).
func (f *FakeRunner) SortedRuns() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := append([]string(nil), f.Runs...)
	sort.Strings(out)
	return out
}
