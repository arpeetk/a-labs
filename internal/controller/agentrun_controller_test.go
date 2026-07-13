package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := wrenv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newReconciler(t *testing.T, objs ...client.Object) (*AgentRunReconciler, client.Client) {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&wrenv1.AgentRun{}, &corev1.Pod{}).
		Build()
	return &AgentRunReconciler{Client: c, Scheme: s, PodConfig: PodConfig{Images: testImages}}, c
}

func reconcile(t *testing.T, r *AgentRunReconciler, run *wrenv1.AgentRun) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: run.Namespace, Name: run.Name},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func getRun(t *testing.T, c client.Client, run *wrenv1.AgentRun) *wrenv1.AgentRun {
	t.Helper()
	var got wrenv1.AgentRun
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatalf("get run: %v", err)
	}
	return &got
}

func setPodPhase(t *testing.T, c client.Client, ns, name string, phase corev1.PodPhase, mutate func(*corev1.Pod)) {
	t.Helper()
	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &pod); err != nil {
		t.Fatalf("get pod %s: %v", name, err)
	}
	pod.Status.Phase = phase
	if mutate != nil {
		mutate(&pod)
	}
	if err := c.Status().Update(context.Background(), &pod); err != nil {
		t.Fatalf("update pod status: %v", err)
	}
}

func TestReconcileAdmitsAndProvisions(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run)

	// 1st pass: admit → Pending.
	reconcile(t, r, run)
	if got := getRun(t, c, run); got.Status.Phase != wrenv1.PhasePending {
		t.Fatalf("phase = %q, want Pending", got.Status.Phase)
	}

	// 2nd pass: create PVC, RunSpec ConfigMap, and the pod.
	reconcile(t, r, run)

	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-workspace"}, &pvc); err != nil {
		t.Fatalf("expected workspace PVC: %v", err)
	}
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-runspec"}, &cm); err != nil {
		t.Fatalf("expected runspec ConfigMap: %v", err)
	}
	if _, ok := cm.Data["runspec.json"]; !ok {
		t.Error("runspec ConfigMap missing runspec.json")
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-0"}, &pod); err != nil {
		t.Fatalf("expected agent pod: %v", err)
	}
}

func TestReconcileRunningPhase(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run)
	reconcile(t, r, run) // Pending
	reconcile(t, r, run) // create pod

	setPodPhase(t, c, run.Namespace, "r-abc-0", corev1.PodRunning, nil)
	reconcile(t, r, run)

	if got := getRun(t, c, run); got.Status.Phase != wrenv1.PhaseRunning {
		t.Fatalf("phase = %q, want Running", got.Status.Phase)
	}
}

func TestReconcileResumesOnFailure(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run)
	reconcile(t, r, run) // Pending
	reconcile(t, r, run) // create pod r-abc-0

	// Harness OOMKilled.
	setPodPhase(t, c, run.Namespace, "r-abc-0", corev1.PodFailed, func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: ContainerHarness,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 137, Reason: "OOMKilled",
			}},
		}}
	})
	reconcile(t, r, run)

	got := getRun(t, c, run)
	if got.Status.Phase != wrenv1.PhaseInterrupted {
		t.Fatalf("phase = %q, want Interrupted", got.Status.Phase)
	}
	if got.Status.RestartCount != 1 {
		t.Fatalf("restartCount = %d, want 1", got.Status.RestartCount)
	}

	// Old pod deleted.
	var old corev1.Pod
	err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-0"}, &old)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected old pod deleted, got err=%v", err)
	}

	// Next reconcile creates the resume pod r-abc-1.
	reconcile(t, r, got)
	var resumePod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-1"}, &resumePod); err != nil {
		t.Fatalf("expected resume pod r-abc-1: %v", err)
	}
}

func TestReconcileDeterministicFailureDoesNotRetry(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run)
	reconcile(t, r, run) // Pending
	reconcile(t, r, run) // create pod r-abc-0

	// Harness exits 1 on its own (a deterministic app/finalize error) — NOT OOM.
	setPodPhase(t, c, run.Namespace, "r-abc-0", corev1.PodFailed, func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  ContainerHarness,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
		}}
	})
	reconcile(t, r, run)

	got := getRun(t, c, run)
	if got.Status.Phase != wrenv1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed (fail fast, no retry)", got.Status.Phase)
	}
	if got.Status.RestartCount != 0 {
		t.Fatalf("restartCount = %d, want 0 (must not re-run a deterministic failure)", got.Status.RestartCount)
	}
	// The failed pod is NOT deleted/recreated (no resume happened).
	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-0"}, &pod); err != nil {
		t.Errorf("expected pod to remain for diagnosis, got %v", err)
	}
}

func TestReconcileFailsAfterRetryBudget(t *testing.T) {
	run := testRun()
	run.Spec.Retry.MaxRestarts = 1
	run.Status.Phase = wrenv1.PhaseRunning
	run.Status.RestartCount = 1 // already at budget
	r, c := newReconciler(t, run)

	// Manually create the current pod, then fail it.
	pod := buildAgentPod(run, PodConfig{Images: testImages})
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	setPodPhase(t, c, run.Namespace, pod.Name, corev1.PodFailed, nil)
	reconcile(t, r, run)

	if got := getRun(t, c, run); got.Status.Phase != wrenv1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
}

func TestReconcileIgnoresTerminatingPod(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run)
	reconcile(t, r, run) // Pending
	reconcile(t, r, run) // create pod r-abc-0

	// Hold the pod in a terminating state: a finalizer makes Delete set a
	// DeletionTimestamp instead of removing the object.
	var pod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-0"}, &pod); err != nil {
		t.Fatal(err)
	}
	pod.Finalizers = []string{"wren.dev/test-hold"}
	if err := c.Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	// A terminating pod can briefly report Failed; the operator must ignore it.
	setPodPhase(t, c, run.Namespace, "r-abc-0", corev1.PodFailed, nil)

	reconcile(t, r, run)

	got := getRun(t, c, run)
	if got.Status.RestartCount != 0 {
		t.Fatalf("terminating pod must not bump restartCount; got %d", got.Status.RestartCount)
	}
	if got.Status.Phase == wrenv1.PhaseInterrupted {
		t.Fatal("terminating pod must not trigger resume")
	}
}

func TestReconcileTerminalIsNoop(t *testing.T) {
	run := testRun()
	run.Status.Phase = wrenv1.PhaseSucceeded
	r, c := newReconciler(t, run)
	reconcile(t, r, run)
	if got := getRun(t, c, run); got.Status.Phase != wrenv1.PhaseSucceeded {
		t.Fatalf("terminal run mutated to %q", got.Status.Phase)
	}
	// No pod should be created for a terminal run.
	var pod corev1.Pod
	err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-0"}, &pod)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected no pod for terminal run, got err=%v", err)
	}
}
