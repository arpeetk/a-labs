package launcher

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
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
