// Package launcher is the control plane's bridge to Kubernetes: it creates and
// reads AgentRun custom resources. The Runs service depends on the Launcher
// interface, never on a Kubernetes client directly, so the control-plane logic
// stays unit-testable (spec §5.2: "a stable API that hides Kubernetes").
package launcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
)

// LabelRun is the pod label whose value is the AgentRun name; the current pod
// for a run is resolved via this selector. It mirrors controller.LabelRun —
// duplicated here (rather than imported) to keep the launcher free of a
// dependency on the controller package.
const LabelRun = "wren.dev/run"

// DefaultLogContainer is the container whose logs `run logs` tails by default:
// the harness (the agent itself). The remaining names are the known sidecar /
// init containers a caller may target with --container.
const DefaultLogContainer = "harness"

var knownLogContainers = map[string]bool{
	"harness":         true,
	"agent-gateway":   true,
	"checkpointer":    true,
	"egress-proxy":    true,
	"egress-lockdown": true,
	"hydrate":         true,
}

// ErrNoPod signals that a run has no pod backing it right now: either it has not
// been scheduled yet (Pending) or the pod is already gone (a finished run whose
// pod was reaped). The apiserver maps this to a 409 with a phase hint.
var ErrNoPod = errors.New("no pod for run")

// Launcher creates and reads AgentRun resources in a cluster.
type Launcher interface {
	EnsureNamespace(ctx context.Context, ns string) error
	CreateRun(ctx context.Context, run *wrenv1.AgentRun) error
	GetRun(ctx context.Context, ns, name string) (*wrenv1.AgentRun, error)
	// ListRuns returns every AgentRun across all namespaces. The apiserver uses
	// it at boot to re-learn in-flight runs into the store (reconcile-on-boot),
	// so a restarted apiserver does not forget runs.
	ListRuns(ctx context.Context) ([]wrenv1.AgentRun, error)
	DeleteRun(ctx context.Context, ns, name string) error
	// StreamLogs opens the log stream of the run's current pod. container
	// defaults to DefaultLogContainer when empty and is validated against the
	// known container names. follow keeps the stream open (tail -f semantics).
	// Returns ErrNoPod when no pod backs the run yet (or anymore).
	StreamLogs(ctx context.Context, namespace, runID, container string, follow bool) (io.ReadCloser, error)
}

// K8s is a Launcher backed by a controller-runtime client. The pods/log
// subresource is not served by the controller-runtime client, so a typed
// clientset is kept alongside it purely for log streaming.
type K8s struct {
	c  client.Client
	cs kubernetes.Interface
}

var _ Launcher = (*K8s)(nil)

// NewScheme returns a runtime.Scheme with core + Wren types registered.
func NewScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := wrenv1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// NewK8s builds a K8s launcher from a REST config.
func NewK8s(cfg *rest.Config) (*K8s, error) {
	s, err := NewScheme()
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		return nil, err
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	return &K8s{c: c, cs: cs}, nil
}

func (k *K8s) EnsureNamespace(ctx context.Context, ns string) error {
	obj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	err := k.c.Create(ctx, obj)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (k *K8s) CreateRun(ctx context.Context, run *wrenv1.AgentRun) error {
	return k.c.Create(ctx, run)
}

func (k *K8s) GetRun(ctx context.Context, ns, name string) (*wrenv1.AgentRun, error) {
	var run wrenv1.AgentRun
	if err := k.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (k *K8s) ListRuns(ctx context.Context) ([]wrenv1.AgentRun, error) {
	var list wrenv1.AgentRunList
	if err := k.c.List(ctx, &list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (k *K8s) DeleteRun(ctx context.Context, ns, name string) error {
	run := &wrenv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	err := k.c.Delete(ctx, run)
	return client.IgnoreNotFound(err)
}

func (k *K8s) StreamLogs(ctx context.Context, ns, runID, container string, follow bool) (io.ReadCloser, error) {
	container, err := resolveContainer(container)
	if err != nil {
		return nil, err
	}
	// Resolve the CURRENT pod by label — pod names embed the restart count, so
	// reconstructing them is wrong. There is one agent pod per run.
	var pods corev1.PodList
	if err := k.c.List(ctx, &pods, client.InNamespace(ns), client.MatchingLabels{LabelRun: runID}); err != nil {
		return nil, fmt.Errorf("list pods for run %q: %w", runID, err)
	}
	pod := currentPod(pods.Items)
	if pod == nil {
		return nil, ErrNoPod
	}
	req := k.cs.CoreV1().Pods(ns).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: container,
		Follow:    follow,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("stream logs for pod %q: %w", pod.Name, err)
	}
	return stream, nil
}

// resolveContainer defaults an empty container to DefaultLogContainer and
// validates it against the known names.
func resolveContainer(container string) (string, error) {
	if container == "" {
		return DefaultLogContainer, nil
	}
	if !knownLogContainers[container] {
		return "", fmt.Errorf("unknown container %q", container)
	}
	return container, nil
}

// currentPod picks the run's live pod. In practice there is exactly one agent
// pod per run; if a stale pod lingers during a restart, prefer the most
// recently created one (deterministic by creation time then name).
func currentPod(pods []corev1.Pod) *corev1.Pod {
	if len(pods) == 0 {
		return nil
	}
	sort.Slice(pods, func(i, j int) bool {
		ti, tj := pods[i].CreationTimestamp, pods[j].CreationTimestamp
		if ti.Equal(&tj) {
			return pods[i].Name > pods[j].Name
		}
		return ti.After(tj.Time)
	})
	return &pods[0]
}
