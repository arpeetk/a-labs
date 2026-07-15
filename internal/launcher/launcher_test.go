package launcher

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
)

func sampleRun(ns, name string) *wrenv1.AgentRun {
	return &wrenv1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       wrenv1.AgentRunSpec{Project: "p", User: "u"},
	}
}

func TestNewScheme(t *testing.T) {
	s, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	if !s.Recognizes(wrenv1.GroupVersion.WithKind("AgentRun")) {
		t.Error("scheme missing AgentRun")
	}
}

func TestFakeLauncher(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	if err := f.EnsureNamespace(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	if !f.Namespaces["ns"] {
		t.Error("namespace not recorded")
	}

	run := sampleRun("ns", "r-1")
	if err := f.CreateRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := f.CreateRun(ctx, run); !apierrors.IsAlreadyExists(err) {
		t.Errorf("dup create = %v, want AlreadyExists", err)
	}

	got, err := f.GetRun(ctx, "ns", "r-1")
	if err != nil || got.Spec.Project != "p" {
		t.Fatalf("GetRun = %+v, %v", got, err)
	}
	if _, err := f.GetRun(ctx, "ns", "missing"); !apierrors.IsNotFound(err) {
		t.Errorf("missing GetRun = %v", err)
	}

	f.SetStatus("ns", "r-1", wrenv1.AgentRunStatus{Phase: wrenv1.PhaseRunning})
	got, _ = f.GetRun(ctx, "ns", "r-1")
	if got.Status.Phase != wrenv1.PhaseRunning {
		t.Errorf("SetStatus not applied: %q", got.Status.Phase)
	}

	if err := f.DeleteRun(ctx, "ns", "r-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.GetRun(ctx, "ns", "r-1"); !apierrors.IsNotFound(err) {
		t.Errorf("after delete = %v", err)
	}
}

func TestK8sLauncher(t *testing.T) {
	ctx := context.Background()
	s, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(s).Build()
	k := &K8s{c: c}

	if err := k.EnsureNamespace(ctx, "ns"); err != nil {
		t.Fatal(err)
	}
	// Idempotent: second EnsureNamespace on an existing ns returns nil.
	if err := k.EnsureNamespace(ctx, "ns"); err != nil {
		t.Errorf("EnsureNamespace not idempotent: %v", err)
	}
	var ns corev1.Namespace
	if err := c.Get(ctx, types.NamespacedName{Name: "ns"}, &ns); err != nil {
		t.Fatalf("namespace not created: %v", err)
	}

	if err := k.CreateRun(ctx, sampleRun("ns", "r-1")); err != nil {
		t.Fatal(err)
	}
	got, err := k.GetRun(ctx, "ns", "r-1")
	if err != nil || got.Name != "r-1" {
		t.Fatalf("GetRun = %+v, %v", got, err)
	}

	if err := k.DeleteRun(ctx, "ns", "r-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.GetRun(ctx, "ns", "r-1"); !apierrors.IsNotFound(err) {
		t.Errorf("after delete = %v", err)
	}
	// Deleting a missing run is a no-op (IgnoreNotFound).
	if err := k.DeleteRun(ctx, "ns", "gone"); err != nil {
		t.Errorf("delete missing = %v", err)
	}
}

func podForRun(ns, name, runID string, created time.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         ns,
			Labels:            map[string]string{LabelRun: runID},
			CreationTimestamp: metav1.NewTime(created),
		},
	}
}

func TestK8sStreamLogs(t *testing.T) {
	ctx := context.Background()
	s, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}

	// The current pod embeds a restart count in its name — the launcher must find
	// it by the wren.dev/run label, not by reconstructing the name. Seed two pods
	// for the same run; the newer one is "current".
	old := podForRun("ns", "run-1-abc", "run-1", time.Unix(100, 0))
	cur := podForRun("ns", "run-1-def-r1", "run-1", time.Unix(200, 0))
	other := podForRun("ns", "run-2-xyz", "run-2", time.Unix(150, 0))

	c := fake.NewClientBuilder().WithScheme(s).WithObjects(old, cur, other).Build()
	cs := k8sfake.NewSimpleClientset()
	k := &K8s{c: c, cs: cs}

	rc, err := k.StreamLogs(ctx, "ns", "run-1", "", false)
	if err != nil {
		t.Fatalf("StreamLogs: %v", err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	// The fake clientset returns "fake logs" for any pod; the value we assert is
	// that a stream opened at all (pod resolved, subresource reached).
	if string(body) != "fake logs" {
		t.Fatalf("log body = %q, want %q", body, "fake logs")
	}

	// Unknown container is rejected before any cluster call.
	if _, err := k.StreamLogs(ctx, "ns", "run-1", "bogus", false); err == nil {
		t.Error("unknown container = nil error, want rejection")
	}

	// A run with no pods → ErrNoPod.
	if _, err := k.StreamLogs(ctx, "ns", "run-missing", "harness", false); !errors.Is(err, ErrNoPod) {
		t.Errorf("no-pod run = %v, want ErrNoPod", err)
	}
}

func TestCurrentPod(t *testing.T) {
	if currentPod(nil) != nil {
		t.Error("currentPod(nil) != nil")
	}
	a := podForRun("ns", "a", "r", time.Unix(100, 0))
	b := podForRun("ns", "b", "r", time.Unix(200, 0))
	got := currentPod([]corev1.Pod{*a, *b})
	if got.Name != "b" {
		t.Errorf("currentPod picked %q, want newest (b)", got.Name)
	}
	// Tie on creation time → deterministic by name (higher wins).
	c1 := podForRun("ns", "c1", "r", time.Unix(100, 0))
	c2 := podForRun("ns", "c2", "r", time.Unix(100, 0))
	if got := currentPod([]corev1.Pod{*c1, *c2}); got.Name != "c2" {
		t.Errorf("tie-break picked %q, want c2", got.Name)
	}
}

func TestFakeStreamLogs(t *testing.T) {
	ctx := context.Background()
	f := NewFake()

	// No logs seeded → ErrNoPod.
	if _, err := f.StreamLogs(ctx, "ns", "r-1", "", false); !errors.Is(err, ErrNoPod) {
		t.Errorf("unseeded = %v, want ErrNoPod", err)
	}

	f.SetLogs("ns", "r-1", "harness", "event: started\nevent: done\n")
	rc, err := f.StreamLogs(ctx, "ns", "r-1", "", false) // "" defaults to harness
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(rc)
	rc.Close()
	if string(body) != "event: started\nevent: done\n" {
		t.Errorf("body = %q", body)
	}

	// Unknown container rejected.
	if _, err := f.StreamLogs(ctx, "ns", "r-1", "nope", false); err == nil {
		t.Error("unknown container = nil, want error")
	}

	// LogsErr is surfaced.
	f.LogsErr = errors.New("boom")
	if _, err := f.StreamLogs(ctx, "ns", "r-1", "harness", false); err == nil {
		t.Error("LogsErr not surfaced")
	}
}
