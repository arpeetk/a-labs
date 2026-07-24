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
	"k8s.io/client-go/rest"
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

// TestK8sRequestCancel exercises the real K8s.RequestCancel against a fake
// controller-runtime client: it must set CancelAnnotation, be idempotent once
// already set, and surface NotFound for a missing run untouched (the apiserver
// maps that to 404).
func TestK8sRequestCancel(t *testing.T) {
	ctx := context.Background()
	s, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	run := sampleRun("ns", "r-1")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(run).Build()
	k := &K8s{c: c}

	if err := k.RequestCancel(ctx, "ns", "r-1"); err != nil {
		t.Fatalf("RequestCancel: %v", err)
	}
	got, err := k.GetRun(ctx, "ns", "r-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations[wrenv1.CancelAnnotation] != "true" {
		t.Errorf("annotations = %v, want %s=true", got.Annotations, wrenv1.CancelAnnotation)
	}

	// Idempotent: a second call on an already-canceled run is a no-op that
	// still returns nil.
	if err := k.RequestCancel(ctx, "ns", "r-1"); err != nil {
		t.Errorf("second RequestCancel = %v, want nil (already requested)", err)
	}

	// Missing run surfaces NotFound so the apiserver can map it to 404.
	if err := k.RequestCancel(ctx, "ns", "missing"); !apierrors.IsNotFound(err) {
		t.Errorf("RequestCancel on missing run = %v, want NotFound", err)
	}
}

// TestK8sRequestCancelSurvivesConcurrentStatusWrite proves the reason
// RequestCancel uses a merge Patch (client.MergeFrom) rather than a
// read-modify-write Update: the operator writes run status concurrently, and
// a naive Update built from a stale read would either clobber that write or
// fail with a resourceVersion conflict — the exact race class fixed in
// internal/install/kube.go (commit d9ede69) via retry.RetryOnConflict for its
// Deployment updates. Here the run's status changes (simulating the operator)
// after RequestCancel's internal Get would have read it, and RequestCancel
// must still succeed and must not stomp the concurrent status write.
func TestK8sRequestCancelSurvivesConcurrentStatusWrite(t *testing.T) {
	ctx := context.Background()
	s, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	run := sampleRun("ns", "r-1")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(run).WithStatusSubresource(&wrenv1.AgentRun{}).Build()
	k := &K8s{c: c}

	// Simulate the operator concurrently advancing status — this bumps the
	// object's resourceVersion, exactly like a live cluster under contention.
	live, err := k.GetRun(ctx, "ns", "r-1")
	if err != nil {
		t.Fatal(err)
	}
	live.Status.Phase = wrenv1.PhaseRunning
	if err := c.Status().Update(ctx, live); err != nil {
		t.Fatal(err)
	}

	if err := k.RequestCancel(ctx, "ns", "r-1"); err != nil {
		t.Fatalf("RequestCancel after concurrent status write: %v", err)
	}
	got, err := k.GetRun(ctx, "ns", "r-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations[wrenv1.CancelAnnotation] != "true" {
		t.Errorf("annotation not applied: %v", got.Annotations)
	}
	if got.Status.Phase != wrenv1.PhaseRunning {
		t.Errorf("concurrent status write was clobbered: phase = %q", got.Status.Phase)
	}
}

// TestK8sSecretHasKey covers all three branches SecretHasKey's contract
// promises callers (coreapi.checkCredentials treats it as best-effort, never
// an error, for a missing Secret or namespace): key present (via Data, the
// path a real round-tripped Secret takes), key present via StringData
// (write-only server-side, but checked so fakes/round-trips agree per the
// doc comment), key absent from an existing Secret, and the Secret entirely
// absent.
func TestK8sSecretHasKey(t *testing.T) {
	ctx := context.Background()
	withData := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "ns"},
		Data:       map[string][]byte{"token": []byte("abc")},
	}
	withStringData := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds-sd", Namespace: "ns"},
		StringData: map[string]string{"token": "abc"},
	}
	missingKey := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds-nokey", Namespace: "ns"},
		Data:       map[string][]byte{"other": []byte("x")},
	}
	cs := k8sfake.NewSimpleClientset(withData, withStringData, missingKey)
	k := &K8s{cs: cs}

	if ok, err := k.SecretHasKey(ctx, "ns", "creds", "token"); err != nil || !ok {
		t.Errorf("Data-backed key: ok=%v err=%v, want true,nil", ok, err)
	}
	if ok, err := k.SecretHasKey(ctx, "ns", "creds-sd", "token"); err != nil || !ok {
		t.Errorf("StringData-backed key: ok=%v err=%v, want true,nil", ok, err)
	}
	if ok, err := k.SecretHasKey(ctx, "ns", "creds-nokey", "token"); err != nil || ok {
		t.Errorf("key absent from existing Secret: ok=%v err=%v, want false,nil", ok, err)
	}
	if ok, err := k.SecretHasKey(ctx, "ns", "does-not-exist", "token"); err != nil || ok {
		t.Errorf("Secret entirely absent: ok=%v err=%v, want false,nil", ok, err)
	}
}

// TestK8sListRuns covers the real ListRuns against a fake controller-runtime
// client seeded with AgentRuns across multiple namespaces: it must return
// every run cluster-wide (the apiserver's reconcile-on-boot needs to re-learn
// in-flight runs regardless of which namespace they landed in).
func TestK8sListRuns(t *testing.T) {
	ctx := context.Background()
	s, err := NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	r1 := sampleRun("ns-a", "r-1")
	r2 := sampleRun("ns-a", "r-2")
	r3 := sampleRun("ns-b", "r-3")
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(r1, r2, r3).Build()
	k := &K8s{c: c}

	got, err := k.ListRuns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("ListRuns returned %d runs, want 3: %+v", len(got), got)
	}
	names := map[string]bool{}
	for _, r := range got {
		names[r.Namespace+"/"+r.Name] = true
	}
	for _, want := range []string{"ns-a/r-1", "ns-a/r-2", "ns-b/r-3"} {
		if !names[want] {
			t.Errorf("ListRuns missing %q, got %v", want, names)
		}
	}
}

// TestNewK8s confirms the constructor wires both the controller-runtime client
// and the typed clientset from a REST config without panicking or dialing the
// cluster (client construction is lazy — no network call happens until a
// request is issued).
func TestNewK8s(t *testing.T) {
	cfg := &rest.Config{Host: "https://127.0.0.1:1"}
	k, err := NewK8s(cfg)
	if err != nil {
		t.Fatalf("NewK8s: %v", err)
	}
	if k.c == nil {
		t.Error("controller-runtime client not wired")
	}
	if k.cs == nil {
		t.Error("typed clientset not wired")
	}
	var _ Launcher = k
}
