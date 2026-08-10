package controller

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
	"github.com/summiteight/wren/internal/runspec"
)

// runSpecFor reads and parses the run's RunSpec ConfigMap.
func runSpecFor(t *testing.T, c client.Client, run *wrenv1.AgentRun) runspec.RunSpec {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: run.Name + "-runspec"}, &cm); err != nil {
		t.Fatalf("get runspec configmap: %v", err)
	}
	var rs runspec.RunSpec
	if err := json.Unmarshal([]byte(cm.Data["runspec.json"]), &rs); err != nil {
		t.Fatalf("parse runspec.json: %v", err)
	}
	return rs
}

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

// TestReconcileWorkspacePVCLostFailsDeterministically is WS-16 A.4: once a run
// has progressed past Pending (meaning its workspace PVC was already created
// once), a PVC that later comes back NotFound is a disk-destroying loss — not
// this run's first-ever provisioning — and must fail the run with a clear
// signal rather than silently resuming into a fresh, empty workspace.
func TestReconcileWorkspacePVCLostFailsDeterministically(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run)
	reconcile(t, r, run) // Pending
	reconcile(t, r, run) // create PVC + pod

	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-workspace"}, &pvc); err != nil {
		t.Fatalf("expected workspace PVC to exist after provisioning: %v", err)
	}

	setPodPhase(t, c, run.Namespace, "r-abc-0", corev1.PodRunning, nil)
	reconcile(t, r, run)
	if got := getRun(t, c, run); got.Status.Phase != wrenv1.PhaseRunning {
		t.Fatalf("phase = %q, want Running", got.Status.Phase)
	}

	// Simulate the disk-destroying loss itself: the PVC disappears out from
	// under the run (node/zone loss, manual deletion) — not the controller's
	// own doing.
	if err := c.Delete(context.Background(), &pvc); err != nil {
		t.Fatalf("delete pvc: %v", err)
	}

	reconcile(t, r, run)

	got := getRun(t, c, run)
	if got.Status.Phase != wrenv1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed (a lost PVC after provisioning is not retryable)", got.Status.Phase)
	}
	if got.Status.RestartCount != 0 {
		t.Errorf("restartCount = %d, want 0 — this must not be misclassified as an ordinary pod-crash resume", got.Status.RestartCount)
	}
	cond := findCondition(got, "Ready")
	if cond == nil || cond.Reason != "WorkspaceLost" {
		t.Fatalf("Ready condition = %+v, want reason WorkspaceLost", cond)
	}
	if !strings.Contains(cond.Message, "gone") && !strings.Contains(cond.Message, "destroyed") {
		t.Errorf("message should explain the data loss, got: %s", cond.Message)
	}

	// Terminal: a further reconcile does not flap or try to recreate the PVC.
	reconcile(t, r, run)
	if got := getRun(t, c, run); got.Status.Phase != wrenv1.PhaseFailed {
		t.Errorf("phase flapped after terminal failure: %q", got.Status.Phase)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-workspace"}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected no PVC recreated after WorkspaceLost, got err=%v", err)
	}
}

// TestReconcileWorkspaceRestore_FullFlow is the WS-21 recovery path: a run
// that HAS opted into checkpointing (CheckpointGCSMount + a bucket) survives a
// lost workspace PVC by recreating it and telling hydrate to restore, instead
// of failing outright like TestReconcileWorkspacePVCLostFailsDeterministically.
// Exercises DoD items (b)-(e) as one continuous flow.
func TestReconcileWorkspaceRestore_FullFlow(t *testing.T) {
	run := testRun() // bucket already set (gs://wren-ckpt)
	r, c := newReconciler(t, run)
	r.PodConfig.CheckpointGCSMount = true

	reconcile(t, r, run) // Pending
	reconcile(t, r, run) // create PVC + pod r-abc-0

	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-workspace"}, &pvc); err != nil {
		t.Fatalf("expected workspace PVC: %v", err)
	}
	setPodPhase(t, c, run.Namespace, "r-abc-0", corev1.PodRunning, nil)
	reconcile(t, r, run)
	if got := getRun(t, c, run); got.Status.Phase != wrenv1.PhaseRunning {
		t.Fatalf("phase = %q, want Running", got.Status.Phase)
	}

	// The disk-destroying loss: the PVC disappears out from under the run.
	if err := c.Delete(context.Background(), &pvc); err != nil {
		t.Fatalf("delete pvc: %v", err)
	}

	// (b) First reconcile after the loss: old pod deleted, RestartCount
	// bumped, WorkspaceRestorePending set True, requeue (not a failure).
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: run.Namespace, Name: run.Name},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res != (ctrl.Result{Requeue: true}) {
		t.Errorf("Result = %+v, want {Requeue: true}", res)
	}
	got := getRun(t, c, run)
	if got.Status.Phase != wrenv1.PhaseInterrupted {
		t.Fatalf("phase = %q, want Interrupted", got.Status.Phase)
	}
	if got.Status.RestartCount != 1 {
		t.Fatalf("restartCount = %d, want 1", got.Status.RestartCount)
	}
	cond := findCondition(got, workspaceRestoreConditionType)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("WorkspaceRestorePending = %+v, want True", cond)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-0"}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected old pod r-abc-0 deleted, got err=%v", err)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-workspace"}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Errorf("PVC must NOT be recreated in the same call that observed the loss, got err=%v", err)
	}

	// (c) The requeued follow-up reconcile creates the fresh PVC and proceeds
	// to build the resume pod r-abc-1.
	reconcile(t, r, got)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-workspace"}, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("expected fresh workspace PVC after follow-up reconcile: %v", err)
	}
	var resumePod corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-1"}, &resumePod); err != nil {
		t.Fatalf("expected resume pod r-abc-1: %v", err)
	}

	// (d) buildRunSpec sets RestoreRequired=true for this pod.
	rs := runSpecFor(t, c, run)
	if !rs.RestoreRequired {
		t.Errorf("RunSpec.RestoreRequired = false, want true for the just-recreated PVC")
	}
	if rs.Mode != runspec.ModeResume {
		t.Errorf("RunSpec.Mode = %q, want resume", rs.Mode)
	}

	// (e) Once the resume pod reaches Running, the condition clears.
	setPodPhase(t, c, run.Namespace, "r-abc-1", corev1.PodRunning, nil)
	reconcile(t, r, getRun(t, c, run))
	got = getRun(t, c, run)
	if got.Status.Phase != wrenv1.PhaseRunning {
		t.Fatalf("phase = %q, want Running", got.Status.Phase)
	}
	cond = findCondition(got, workspaceRestoreConditionType)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("WorkspaceRestorePending = %+v, want False once the pod is Running", cond)
	}
}

// TestReconcileOrdinaryCrashResume_DoesNotReTriggerRestore is DoD item (f): a
// later, ordinary pod crash (the PVC stays intact — a harness OOM, say) on a
// checkpointing-enabled run must resume exactly like any other crash, WITHOUT
// re-triggering a checkpoint restore into a workspace that's still there.
func TestReconcileOrdinaryCrashResume_DoesNotReTriggerRestore(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run)
	r.PodConfig.CheckpointGCSMount = true

	reconcile(t, r, run) // Pending
	reconcile(t, r, run) // create PVC + pod r-abc-0
	setPodPhase(t, c, run.Namespace, "r-abc-0", corev1.PodRunning, nil)
	reconcile(t, r, run) // Running

	// An ordinary crash: OOMKilled, PVC untouched.
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
	// The PVC survived this whole time — ensurePVC's Get succeeds, so the
	// restore machinery (and its condition) is never touched.
	if cond := findCondition(got, workspaceRestoreConditionType); cond != nil {
		t.Errorf("WorkspaceRestorePending = %+v, want absent for an ordinary crash-resume", cond)
	}

	reconcile(t, r, got) // create resume pod r-abc-1
	rs := runSpecFor(t, c, run)
	if rs.RestoreRequired {
		t.Errorf("RunSpec.RestoreRequired = true, want false — the PVC was never lost")
	}
	if rs.Mode != runspec.ModeResume {
		t.Errorf("RunSpec.Mode = %q, want resume", rs.Mode)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-1"}, &corev1.Pod{}); err != nil {
		t.Fatalf("expected resume pod r-abc-1: %v", err)
	}
}

// TestReconcileCancelStopsRun is WS-15 Part C: the cancel annotation deletes the
// current pod and drives the run to Canceled (terminal — not auto-resumed).
func TestReconcileCancelStopsRun(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run)
	reconcile(t, r, run) // Pending
	reconcile(t, r, run) // create pod r-abc-0
	setPodPhase(t, c, run.Namespace, "r-abc-0", corev1.PodRunning, nil)
	reconcile(t, r, run) // Running

	// User runs `wren run stop` → cancel annotation set on the CR.
	cur := getRun(t, c, run)
	cur.Annotations = map[string]string{wrenv1.CancelAnnotation: "true"}
	if err := c.Update(context.Background(), cur); err != nil {
		t.Fatalf("annotate: %v", err)
	}
	reconcile(t, r, run)

	if got := getRun(t, c, run); got.Status.Phase != wrenv1.PhaseCanceled {
		t.Fatalf("phase = %q, want Canceled", got.Status.Phase)
	}
	// The pod is deleted so the agent actually halts.
	var pod corev1.Pod
	err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-0"}, &pod)
	if !apierrors.IsNotFound(err) {
		t.Errorf("pod after cancel = %v, want NotFound", err)
	}
	// A further reconcile is a no-op (terminal): no new pod is recreated.
	reconcile(t, r, run)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-0"}, &pod); !apierrors.IsNotFound(err) {
		t.Errorf("canceled run must not recreate a pod, got %v", err)
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

// TestReconcileManualResume covers `wren run resume`: a terminally-Failed run
// (retry budget exhausted, pod left behind for diagnosis) gets a fresh
// attempt once the resume annotation is set — leftover pod deleted, retry
// budget reset, trigger annotation cleared so a later unrelated failure can't
// be silently auto-resumed by a stale annotation.
func TestReconcileManualResume(t *testing.T) {
	run := testRun()
	run.Spec.Retry.MaxRestarts = 1
	run.Status.Phase = wrenv1.PhaseRunning
	run.Status.RestartCount = 1 // already at budget
	r, c := newReconciler(t, run)

	pod := buildAgentPod(run, PodConfig{Images: testImages})
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	setPodPhase(t, c, run.Namespace, pod.Name, corev1.PodFailed, nil)
	reconcile(t, r, run)

	got := getRun(t, c, run)
	if got.Status.Phase != wrenv1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	// The exhausted-budget pod is left for diagnosis.
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: pod.Name}, &corev1.Pod{}); err != nil {
		t.Fatalf("expected leftover pod for diagnosis: %v", err)
	}

	// `wren run resume` → the resume annotation is set on the CR.
	got.Annotations = map[string]string{wrenv1.ResumeAnnotation: "true"}
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatalf("annotate: %v", err)
	}
	reconcile(t, r, got)

	resumed := getRun(t, c, run)
	if resumed.Status.Phase != wrenv1.PhaseInterrupted {
		t.Fatalf("phase = %q, want Interrupted", resumed.Status.Phase)
	}
	if resumed.Status.RestartCount != 0 {
		t.Fatalf("restartCount = %d, want 0 (fresh retry budget)", resumed.Status.RestartCount)
	}
	if resumed.Annotations[wrenv1.ResumeAnnotation] != "" {
		t.Error("resume annotation should be cleared so a later failure can't stale-auto-resume")
	}
	cond := findCondition(resumed, "Resumed")
	if cond == nil || cond.Reason != "ManualResume" {
		t.Fatalf("Resumed condition = %+v, want reason ManualResume", cond)
	}
	// The leftover pod from the exhausted attempt is deleted.
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: pod.Name}, &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Errorf("leftover pod after resume = %v, want NotFound", err)
	}

	// The next reconcile gives the run a real fresh attempt: a new pod at
	// generation 0 (no collision — the old r-abc-0 was already deleted above).
	reconcile(t, r, resumed)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-0"}, &corev1.Pod{}); err != nil {
		t.Fatalf("expected fresh pod r-abc-0 after resume: %v", err)
	}
}

// TestReconcileManualResumeWorkspaceLostFailsAgain is the invariant regression
// this feature must never weaken: resuming a run that is Failed/WorkspaceLost
// (its PVC was genuinely destroyed, no checkpoint restore configured) must hit
// ensurePVC's same NotFound check on the very next reconcile and fail
// WorkspaceLost again — never silently succeed by creating a fresh, empty PVC
// and hiding that the prior work is gone.
func TestReconcileManualResumeWorkspaceLostFailsAgain(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run)
	reconcile(t, r, run) // Pending
	reconcile(t, r, run) // create PVC + pod r-abc-0

	setPodPhase(t, c, run.Namespace, "r-abc-0", corev1.PodRunning, nil)
	reconcile(t, r, run) // Running

	// The workspace PVC is genuinely destroyed (node/zone loss, manual delete).
	if err := c.Delete(context.Background(), &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: run.Namespace, Name: "r-abc-workspace"},
	}); err != nil {
		t.Fatalf("delete pvc: %v", err)
	}
	reconcile(t, r, run)

	got := getRun(t, c, run)
	if got.Status.Phase != wrenv1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if cond := findCondition(got, "Ready"); cond == nil || cond.Reason != "WorkspaceLost" {
		t.Fatalf("Ready condition = %+v, want reason WorkspaceLost", cond)
	}

	// `wren run resume` — must not paper over the destroyed workspace.
	got.Annotations = map[string]string{wrenv1.ResumeAnnotation: "true"}
	if err := c.Update(context.Background(), got); err != nil {
		t.Fatalf("annotate: %v", err)
	}
	reconcile(t, r, got) // clears the trigger, resets budget, drops to Interrupted

	interrupted := getRun(t, c, run)
	if interrupted.Status.Phase != wrenv1.PhaseInterrupted {
		t.Fatalf("phase after resume = %q, want Interrupted", interrupted.Status.Phase)
	}

	// The follow-up reconcile hits ensurePVC's NotFound check again: still no
	// checkpoint bucket configured, so it must fail WorkspaceLost again.
	reconcile(t, r, interrupted)

	final := getRun(t, c, run)
	if final.Status.Phase != wrenv1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed again — resume must not silently succeed on a destroyed workspace", final.Status.Phase)
	}
	if cond := findCondition(final, "Ready"); cond == nil || cond.Reason != "WorkspaceLost" {
		t.Fatalf("Ready condition = %+v, want reason WorkspaceLost again", cond)
	}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-workspace"}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Errorf("expected no PVC recreated after re-failing WorkspaceLost, got err=%v", err)
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

func TestReconcile_EgressEnforcementOff_WritesDisabledCondition(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run)
	r.PodConfig.EgressEnforcement = EgressEnforcementOff

	reconcile(t, r, run) // admit
	reconcile(t, r, run) // provision (sets condition + creates children)

	cond := findCondition(getRun(t, c, run), egressEnforcementConditionType)
	if cond == nil {
		t.Fatal("expected EgressEnforcement condition")
	}
	if cond.Status != metav1.ConditionFalse || cond.Reason != "Disabled" {
		t.Errorf("condition = %s/%s, want False/Disabled", cond.Status, cond.Reason)
	}
}

func TestReconcile_EgressEnforcementIptables_WritesEnforcedCondition(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run) // default PodConfig → iptables

	reconcile(t, r, run) // admit
	reconcile(t, r, run) // provision

	cond := findCondition(getRun(t, c, run), egressEnforcementConditionType)
	if cond == nil {
		t.Fatal("expected EgressEnforcement condition")
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != "Iptables" {
		t.Errorf("condition = %s/%s, want True/Iptables", cond.Status, cond.Reason)
	}
}

// A pod the apiserver refuses to admit (e.g. the privileged egress-lockdown
// init container on GKE Autopilot or a PSA-restricted namespace) must fail the
// run deterministically — requeuing would hang it in Provisioning forever.
func TestReconcile_PodCreateForbidden_FailsDeterministically(t *testing.T) {
	run := testRun()
	s := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(run).
		WithStatusSubresource(&wrenv1.AgentRun{}, &corev1.Pod{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cli client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, isPod := obj.(*corev1.Pod); isPod {
					return apierrors.NewForbidden(corev1.Resource("pods"), obj.GetName(),
						errors.New("admission webhook denied: privileged init containers are not allowed"))
				}
				return cli.Create(ctx, obj, opts...)
			},
		}).
		Build()
	r := &AgentRunReconciler{Client: c, Scheme: s, PodConfig: PodConfig{Images: testImages}}

	reconcile(t, r, run) // admit
	reconcile(t, r, run) // provision: pod create hits Forbidden

	got := getRun(t, c, run)
	if got.Status.Phase != wrenv1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed (Forbidden is permanent; requeueing cannot fix it)", got.Status.Phase)
	}
	cond := findCondition(got, "Ready")
	if cond == nil || cond.Reason != "PodAdmissionForbidden" {
		t.Fatalf("Ready condition = %+v, want reason PodAdmissionForbidden", cond)
	}
	if !strings.Contains(cond.Message, "--egress-enforcement=off") {
		t.Errorf("message should point at the escape hatch, got: %s", cond.Message)
	}

	// Terminal: a further reconcile is a no-op (no error, no flap).
	reconcile(t, r, run)
	if got := getRun(t, c, run); got.Status.Phase != wrenv1.PhaseFailed {
		t.Errorf("phase flapped after terminal failure: %q", got.Status.Phase)
	}
}
