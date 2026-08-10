// Package controller holds the Wren operator's reconciler: AgentRun (one agent
// run → a hardened pod with a durable workspace and crash-resume).
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
	"github.com/summiteight/wren/internal/runspec"
)

const (
	defaultMaxRestarts        int32  = 5
	defaultCheckpointInterval int32  = 120
	branchPrefix              string = "wren"
)

// AgentRunReconciler reconciles AgentRun objects into agent pods.
type AgentRunReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// PodConfig is the operator-level pod configuration (images, credential
	// Secrets injected into the egress-proxy, egress port).
	PodConfig PodConfig
	// Logs reads pod container logs (pods/log). It backs the v0.1 run-results
	// channel: terminal harness events are scraped into Status.PR/Usage/
	// SessionID (WS-11). Nil disables the scrape (tests, bring-up).
	Logs LogReader
}

// +kubebuilder:rbac:groups=wren.dev,resources=agentruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=wren.dev,resources=agentruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=wren.dev,resources=agentruns/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims;configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives an AgentRun toward its terminal state.
func (r *AgentRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)

	var run wrenv1.AgentRun
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !run.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil // owned children are garbage-collected
	}
	if isTerminal(run.Status.Phase) {
		// Manual resume (wren run resume) is the one exception to "terminal
		// means done": an explicit human/automated decision to give a Failed
		// run another attempt. Gated on Phase == Failed specifically (not
		// just isTerminal) so a stray annotation can never resurrect a
		// Succeeded or Canceled run.
		if run.Status.Phase == wrenv1.PhaseFailed && run.Annotations[wrenv1.ResumeAnnotation] == "true" {
			return r.resume(ctx, &run)
		}
		return ctrl.Result{}, nil
	}

	// User asked to stop the run (wren run stop): halt the pod and mark it
	// Canceled (terminal — no auto-resume). Checked before the retry path so a
	// deliberate stop is never mistaken for a crash to retry.
	if run.Annotations[wrenv1.CancelAnnotation] == "true" {
		return r.cancel(ctx, &run)
	}

	// First sight of the run: admit it.
	if run.Status.Phase == "" {
		return r.setPhase(ctx, &run, wrenv1.PhasePending, "Admitted", "run accepted")
	}

	// Record the egress-enforcement posture on the run so an operator can see,
	// per run, whether the runner is physically confined to the proxy
	// (EgressEnforcement=True/Iptables) or free to bypass it (False/Disabled).
	if err := r.ensureEgressCondition(ctx, &run); err != nil {
		return ctrl.Result{}, fmt.Errorf("record egress condition: %w", err)
	}

	// Ensure the durable prerequisites exist before the pod.
	if err := r.ensurePVC(ctx, &run); err != nil {
		if errors.Is(err, errWorkspaceLost) {
			// A permanent, non-retryable condition (like PodAdmissionForbidden
			// below): recreating an empty PVC and resuming into it wouldn't
			// recover anything, it would just hide that the work is gone. Fail
			// deterministically rather than requeue-and-hang (code standards
			// rule #2) or silently resume into an empty workspace (WS-8
			// truthing pass; WS-16 A.4).
			return r.setPhase(ctx, &run, wrenv1.PhaseFailed, "WorkspaceLost",
				"workspace PVC is gone after the run had already progressed past Pending — the disk (and any in-progress work) was destroyed; this run cannot resume and will not be retried")
		}
		if errors.Is(err, errWorkspaceRestoring) {
			// Recoverable: this run opted into checkpointing, so the PVC is
			// being recreated for hydrate to restore into (WS-21). Not a
			// failure, not a no-op — requeue so the follow-up reconcile
			// actually creates the fresh PVC.
			return ctrl.Result{Requeue: true}, nil
		}
		return ctrl.Result{}, fmt.Errorf("ensure workspace pvc: %w", err)
	}
	if err := r.ensureRunSpec(ctx, &run); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure runspec: %w", err)
	}

	pod, err := r.ensurePod(ctx, &run)
	if err != nil {
		// An admission rejection (Forbidden) is permanent: no pod with this
		// spec will ever be admitted — e.g. the privileged egress-lockdown init
		// container on GKE Autopilot or in a PSA-restricted namespace. Fail the
		// run deterministically with the escape hatch, rather than requeuing
		// forever while the run hangs in Provisioning.
		if apierrors.IsForbidden(err) {
			return r.setPhase(ctx, &run, wrenv1.PhaseFailed, "PodAdmissionForbidden",
				"agent pod rejected by apiserver admission (Forbidden): this cluster may forbid the privileged egress-lockdown init container — run the operator with --egress-enforcement=off (weaker: the runner can bypass the proxy) or use a cluster/namespace that admits it")
		}
		return ctrl.Result{}, fmt.Errorf("ensure pod: %w", err)
	}

	lg.V(1).Info("reconciled", "phase", run.Status.Phase, "pod", pod.Name, "podPhase", pod.Status.Phase)
	return r.reconcilePodState(ctx, &run, pod)
}

// egressEnforcementConditionType is the condition type recording the egress
// bypass-prevention posture (spec §5.6, WS-1).
const egressEnforcementConditionType = "EgressEnforcement"

// ensureEgressCondition records an EgressEnforcement=Disabled condition when the
// operator runs with --egress-enforcement=off, so the weaker posture is visible
// on `wren run get`. With enforcement on (the default) it sets Enforced=True.
// Idempotent: it persists only when the condition would actually change.
func (r *AgentRunReconciler) ensureEgressCondition(ctx context.Context, run *wrenv1.AgentRun) error {
	var want metav1.Condition
	if r.PodConfig.enforcementMode() == EgressEnforcementOff {
		want = metav1.Condition{
			Type:    egressEnforcementConditionType,
			Status:  metav1.ConditionFalse,
			Reason:  "Disabled",
			Message: "egress bypass enforcement disabled (--egress-enforcement=off); the runner can bypass the proxy",
		}
	} else {
		want = metav1.Condition{
			Type:    egressEnforcementConditionType,
			Status:  metav1.ConditionTrue,
			Reason:  "Iptables",
			Message: "egress locked down via iptables uid-match; the runner cannot bypass the proxy",
		}
	}
	if existing := findCondition(run, egressEnforcementConditionType); existing != nil &&
		existing.Status == want.Status && existing.Reason == want.Reason {
		return nil
	}
	setCondition(run, want)
	return r.Status().Update(ctx, run)
}

// findCondition returns the condition of the given type, or nil.
func findCondition(run *wrenv1.AgentRun, condType string) *metav1.Condition {
	for i := range run.Status.Conditions {
		if run.Status.Conditions[i].Type == condType {
			return &run.Status.Conditions[i]
		}
	}
	return nil
}

// errWorkspaceLost is ensurePVC's sentinel for a disk-destroying loss on a run
// that has NOT opted into checkpointing: the workspace PVC is NotFound on a
// run that has already progressed past Pending, i.e. this is not the PVC's
// first-ever creation. Mapped to PhaseFailed at the Reconcile call site (code
// standards rule #4: sentinels, mapped deliberately at the boundary).
var errWorkspaceLost = errors.New("workspace PVC lost after provisioning")

// errWorkspaceRestoring is ensurePVC's sentinel for the same disk-destroying
// loss, but on a run that HAS opted into checkpointing (WS-21): unlike
// errWorkspaceLost this is recoverable — the PVC is being recreated so hydrate
// can restore the latest checkpoint into it before the harness starts. Mapped
// to a requeue (not a failure) at the Reconcile call site.
var errWorkspaceRestoring = errors.New("workspace PVC lost; recreating for checkpoint-restore")

// workspaceRestoreConditionType marks a run whose workspace PVC was lost and
// is being recreated for checkpoint-restore (WS-21). It is set True the
// reconcile the loss is first observed, stays True across the requeued
// reconcile that actually creates the fresh PVC (buildRunSpec reads it to set
// RestoreRequired=true for the pod about to be built), and is cleared only
// once that pod reaches Running (reconcilePodState) — proof hydrate
// completed, whether or not this particular run ever needed a restore. That
// stops it from lingering True into this run's later, ordinary crash-resumes,
// where the PVC will have survived and a restore must NOT be re-attempted
// into a non-empty workspace.
const workspaceRestoreConditionType = "WorkspaceRestorePending"

// ensurePVC creates the workspace PVC if it does not already exist. The PVC
// name is stable across restarts so a surviving disk is reattached on resume
// (see mode() / RestartCount).
//
// A NotFound PVC is ambiguous by itself — it's expected exactly once, on this
// run's first-ever reconcile past admission, and a data-loss signal every
// time after. Phase disambiguates them for free, no new field needed: the
// very first ensurePVC call for a run always happens while Status.Phase is
// still Pending (Reconcile only reaches ensurePVC once Phase != "", and
// ensurePVC runs — and the PVC gets created — before ensurePod/
// reconcilePodState ever advance the phase past Pending within that same
// call). So a NotFound PVC on any LATER reconcile (Phase already
// Provisioning/Running/Interrupted/...) means a PVC that definitely existed
// is now gone — a real disk-destroying loss (node/zone loss, manual
// deletion), not first-time provisioning.
//
// For a run that has NOT opted into checkpointing (no bucket, or the
// operator's --checkpoint-gcs-mount flag off), silently creating a fresh,
// empty PVC would resume the harness into a workspace with no signal that
// everything on disk was lost (WS-8 truthing pass; WS-16 A.4) — so that case
// still returns errWorkspaceLost, unchanged from before WS-21.
//
// For a run that HAS opted in (restoreEligible), the loss is recoverable: the
// dead pod (if any) is deleted, RestartCount bumped, and
// workspaceRestoreConditionType set True — but the PVC is NOT created in this
// same call (errWorkspaceRestoring instead), so the just-deleted pod can't
// race a same-generation PVC. The requeued follow-up reconcile calls ensurePVC
// again, finds the condition already True, and creates the PVC then; hydrate
// (told via RestoreRequired, §7) restores the latest checkpoint into it.
func (r *AgentRunReconciler) ensurePVC(ctx context.Context, run *wrenv1.AgentRun) error {
	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: pvcName(run)}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	if run.Status.Phase == wrenv1.PhasePending {
		return r.createWorkspacePVC(ctx, run)
	}

	restoreEligible := r.PodConfig.CheckpointGCSMount && run.Spec.Workspace.Checkpoint.Bucket != ""
	if !restoreEligible {
		return errWorkspaceLost
	}

	if pending := findCondition(run, workspaceRestoreConditionType); pending != nil && pending.Status == metav1.ConditionTrue {
		// The requeued follow-up reconcile: the loss was already recorded, so
		// this NotFound is the fresh PVC not existing yet — create it now.
		return r.createWorkspacePVC(ctx, run)
	}

	// First time this loss is observed for this run: delete the dead pod
	// (mirrors cancel()'s Get-then-Delete-ignoring-NotFound pattern) BEFORE
	// bumping RestartCount, using today's stable podName(run).
	var pod corev1.Pod
	getErr := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: podName(run)}, &pod)
	switch {
	case getErr == nil:
		if delErr := r.Delete(ctx, &pod, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
			return fmt.Errorf("delete pod before workspace restore: %w", delErr)
		}
	case !apierrors.IsNotFound(getErr):
		return getErr
	}

	run.Status.RestartCount++
	run.Status.Phase = wrenv1.PhaseInterrupted
	setCondition(run, metav1.Condition{
		Type:    workspaceRestoreConditionType,
		Status:  metav1.ConditionTrue,
		Reason:  "WorkspaceLost",
		Message: "workspace PVC lost after provisioning; recreating and restoring from the latest checkpoint",
	})
	if err := r.Status().Update(ctx, run); err != nil {
		return err
	}
	return errWorkspaceRestoring
}

// createWorkspacePVC builds and creates the workspace PVC, owned by run.
func (r *AgentRunReconciler) createWorkspacePVC(ctx context.Context, run *wrenv1.AgentRun) error {
	pvc := buildWorkspacePVC(run)
	if err := controllerutil.SetControllerReference(run, pvc, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, pvc)
}

// clearWorkspaceRestoreCondition clears WorkspaceRestorePending=True once a
// new pod is confirmed Running. A no-op (no Status().Update) when the
// condition is absent or already False, so ordinary reconciles of an
// already-running pod don't churn status every loop.
func (r *AgentRunReconciler) clearWorkspaceRestoreCondition(ctx context.Context, run *wrenv1.AgentRun) error {
	existing := findCondition(run, workspaceRestoreConditionType)
	if existing == nil || existing.Status != metav1.ConditionTrue {
		return nil
	}
	setCondition(run, metav1.Condition{
		Type:    workspaceRestoreConditionType,
		Status:  metav1.ConditionFalse,
		Reason:  "PodRunning",
		Message: "workspace restore (if any) completed; hydrate succeeded and the pod is running",
	})
	return r.Status().Update(ctx, run)
}

// ensureRunSpec writes/updates the per-run RunSpec ConfigMap the harness reads.
func (r *AgentRunReconciler) ensureRunSpec(ctx context.Context, run *wrenv1.AgentRun) error {
	spec := r.buildRunSpec(run)
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runSpecConfigMapName(run),
			Namespace: run.Namespace,
			Labels:    runLabels(run),
		},
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, cm, func() error {
		cm.Data = map[string]string{runspec.FileName: string(data)}
		return controllerutil.SetControllerReference(run, cm, r.Scheme)
	})
	return err
}

func (r *AgentRunReconciler) buildRunSpec(run *wrenv1.AgentRun) runspec.RunSpec {
	restoreCond := findCondition(run, workspaceRestoreConditionType)
	rs := runspec.RunSpec{
		RunID:            run.Name,
		Project:          run.Spec.Project,
		Repo:             run.Spec.Repo,
		User:             run.Spec.User,
		Harness:          string(run.Spec.Harness.Kind),
		Model:            run.Spec.Harness.Model,
		Prompt:           run.Spec.Task.Prompt,
		BaseRef:          run.Spec.Task.BaseRef,
		WorkspacePath:    runspec.WorkspacePath,
		SessionID:        run.Status.SessionID,
		Mode:             mode(run.Status.RestartCount > 0),
		Interactive:      run.Spec.Interactive,
		CheckpointBucket: run.Spec.Workspace.Checkpoint.Bucket,
		RestoreRequired:  restoreCond != nil && restoreCond.Status == metav1.ConditionTrue,
		BranchPrefix:     fmt.Sprintf("%s/%s", branchPrefix, sanitizeRef(run.Spec.User)),
	}
	if run.Spec.MCP.ConfigRef != "" {
		rs.MCPConfigPath = runspec.MCPConfigPath
	}
	return rs
}

// ensurePod fetches the current-generation pod, creating it if absent.
func (r *AgentRunReconciler) ensurePod(ctx context.Context, run *wrenv1.AgentRun) (*corev1.Pod, error) {
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: podName(run)}, &pod)
	if err == nil {
		return &pod, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	desired := buildAgentPod(run, r.PodConfig)
	if err := controllerutil.SetControllerReference(run, desired, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, desired); err != nil {
		return nil, err
	}
	return desired, nil
}

// reconcilePodState maps the pod's phase onto the run's phase, driving resume or
// failure on pod termination.
func (r *AgentRunReconciler) reconcilePodState(ctx context.Context, run *wrenv1.AgentRun, pod *corev1.Pod) (ctrl.Result, error) {
	// A pod that is being deleted — externally, or by us during resume — is not
	// a harness crash. A terminating pod can briefly report phase=Failed; acting
	// on it would spuriously consume the retry budget. Wait for it to disappear;
	// the next reconcile recreates it via ensurePod.
	if !pod.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	switch pod.Status.Phase {
	case corev1.PodPending:
		return r.setPhaseIfChanged(ctx, run, wrenv1.PhaseProvisioning, "PodPending", "pod scheduling")
	case corev1.PodRunning:
		// Proof hydrate completed successfully (it blocks the harness from
		// starting): clear any pending workspace-restore condition so a later,
		// ordinary crash-resume of this same run does not re-trigger a restore.
		if err := r.clearWorkspaceRestoreCondition(ctx, run); err != nil {
			return ctrl.Result{}, fmt.Errorf("clear workspace-restore condition: %w", err)
		}
		return r.setPhaseIfChanged(ctx, run, wrenv1.PhaseRunning, "PodRunning", "harness running")
	case corev1.PodSucceeded:
		// Harness exited 0: the PR is opened by the harness/control plane; the
		// operator records terminal success. Scrape the harness event stream
		// first so Status.PR/Usage/SessionID land with the terminal phase.
		r.scrapeRunResults(ctx, run, pod)
		return r.setPhaseIfChanged(ctx, run, wrenv1.PhaseSucceeded, "HarnessCompleted", "task complete")
	case corev1.PodFailed:
		return r.handlePodFailure(ctx, run, pod)
	default:
		return ctrl.Result{}, nil
	}
}

// handlePodFailure resumes the run (up to the retry budget) or fails it,
// recording the classified reason so `wren run get` shows continuity.
func (r *AgentRunReconciler) handlePodFailure(ctx context.Context, run *wrenv1.AgentRun, pod *corev1.Pod) (ctrl.Result, error) {
	// Scrape the harness event stream BEFORE the pod is deleted for resume:
	// the events survive container termination only while the pod object
	// lives, and the results (tokens spent, any PR) must not be lost.
	r.scrapeRunResults(ctx, run, pod)
	info := classifyTermination(pod)
	max := run.Spec.Retry.MaxRestarts
	if max == 0 {
		max = defaultMaxRestarts
	}

	// Deterministic failures are terminal: retrying just repeats them and, for
	// an agent harness, re-spends its tokens.
	if !info.retryable {
		return r.setPhase(ctx, run, wrenv1.PhaseFailed, "HarnessError",
			fmt.Sprintf("run failed (%s); not retryable", info.reason))
	}

	if run.Status.RestartCount >= max {
		return r.setPhase(ctx, run, wrenv1.PhaseFailed, "RetryBudgetExhausted",
			fmt.Sprintf("failed after %d restarts (%s)", run.Status.RestartCount, info.reason))
	}

	// Delete the failed pod, bump the restart count, and drop back to
	// Provisioning so the next reconcile recreates a resume pod.
	if err := r.Delete(ctx, pod, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("delete failed pod: %w", err)
	}
	run.Status.RestartCount++
	run.Status.Phase = wrenv1.PhaseInterrupted
	meta := metav1.Condition{
		Type:    "Resuming",
		Status:  metav1.ConditionTrue,
		Reason:  "PodTerminated",
		Message: fmt.Sprintf("restart %d/%d after %s", run.Status.RestartCount, max, info.reason),
	}
	setCondition(run, meta)
	if err := r.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// cancel stops a run at the user's request (wren run stop): it deletes the
// current pod so the agent halts, then marks the run Canceled. Canceled is
// terminal, so — unlike a crash — the operator does not recreate the pod. Pod
// deletion is best-effort: a run canceled before any pod exists just
// transitions to Canceled.
func (r *AgentRunReconciler) cancel(ctx context.Context, run *wrenv1.AgentRun) (ctrl.Result, error) {
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: podName(run)}, &pod)
	switch {
	case err == nil:
		if delErr := r.Delete(ctx, &pod, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
			return ctrl.Result{}, fmt.Errorf("delete pod on cancel: %w", delErr)
		}
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}
	return r.setPhase(ctx, run, wrenv1.PhaseCanceled, "Canceled", "run canceled by user (wren run stop)")
}

// resume manually restarts a terminally-Failed run at the user's request
// (wren run resume): it deletes any leftover pod from the failed attempt,
// resets the retry budget, and drops the run back to Interrupted — the exact
// non-terminal phase handlePodFailure uses for an ordinary crash-resume, so
// the very next reconcile takes that same, already-tested path (ensurePVC's
// checks, ensurePod recreating the pod at generation 0). This is deliberate:
// resume gives the reconciler another real attempt, it does not paper over
// whatever made the run fail — a run that failed WorkspaceLost because its
// PVC is genuinely gone will hit ensurePVC's same NotFound check on the very
// next reconcile and fail WorkspaceLost again, truthfully.
func (r *AgentRunReconciler) resume(ctx context.Context, run *wrenv1.AgentRun) (ctrl.Result, error) {
	// podName is keyed off the CURRENT RestartCount — resolve and delete the
	// leftover pod (if handlePodFailure left one for diagnosis, e.g.
	// RetryBudgetExhausted/HarnessError) BEFORE that count is reset below.
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: podName(run)}, &pod)
	switch {
	case err == nil:
		if delErr := r.Delete(ctx, &pod, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
			return ctrl.Result{}, fmt.Errorf("delete leftover pod on resume: %w", delErr)
		}
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, err
	}

	// Clear the trigger annotation via a metadata-only Update BEFORE the
	// Status().Update below (the status subresource means the two are
	// separate writes to the API server) — see ResumeAnnotation's doc comment
	// for why this must happen unconditionally, not just on the happy path.
	delete(run.Annotations, wrenv1.ResumeAnnotation)
	if err := r.Update(ctx, run); err != nil {
		return ctrl.Result{}, fmt.Errorf("clear resume annotation: %w", err)
	}

	run.Status.RestartCount = 0 // a fresh Spec.Retry.MaxRestarts worth of attempts
	run.Status.Phase = wrenv1.PhaseInterrupted
	setCondition(run, metav1.Condition{
		Type:    "Resumed",
		Status:  metav1.ConditionTrue,
		Reason:  "ManualResume",
		Message: "manually resumed via `wren run resume`; retry budget reset for a fresh attempt",
	})
	if err := r.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

// setPhase unconditionally sets the phase + a condition and persists status.
func (r *AgentRunReconciler) setPhase(ctx context.Context, run *wrenv1.AgentRun, phase wrenv1.RunPhase, reason, msg string) (ctrl.Result, error) {
	run.Status.Phase = phase
	setCondition(run, metav1.Condition{Type: "Ready", Status: readyStatus(phase), Reason: reason, Message: msg})
	if err := r.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// setPhaseIfChanged persists only when the phase actually changes, to avoid
// reconcile churn.
func (r *AgentRunReconciler) setPhaseIfChanged(ctx context.Context, run *wrenv1.AgentRun, phase wrenv1.RunPhase, reason, msg string) (ctrl.Result, error) {
	if run.Status.Phase == phase {
		return ctrl.Result{}, nil
	}
	return r.setPhase(ctx, run, phase, reason, msg)
}

// SetupWithManager wires the reconciler to watch AgentRuns and owned pods.
func (r *AgentRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&wrenv1.AgentRun{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.ConfigMap{}).
		Complete(r)
}

var refUnsafe = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// sanitizeRef turns an identity (e.g. an email) into a valid git branch-name
// component: git refs disallow "@{", spaces, and several characters.
func sanitizeRef(s string) string {
	s = refUnsafe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-._")
	if s == "" {
		return "user"
	}
	return s
}

func isTerminal(p wrenv1.RunPhase) bool {
	switch p {
	case wrenv1.PhaseSucceeded, wrenv1.PhaseFailed, wrenv1.PhaseCanceled:
		return true
	default:
		return false
	}
}

func readyStatus(p wrenv1.RunPhase) metav1.ConditionStatus {
	switch p {
	case wrenv1.PhaseRunning, wrenv1.PhaseSucceeded:
		return metav1.ConditionTrue
	default:
		return metav1.ConditionFalse
	}
}

// terminationInfo describes why a pod failed and whether a retry could help.
type terminationInfo struct {
	reason    string
	retryable bool
}

// classifyTermination inspects a failed pod and decides whether a retry is
// warranted. Infrastructure-caused terminations (OOM, eviction, node loss) are
// retryable — a fresh pod may succeed (Journey C). A container that exits
// non-zero on its own is a deterministic failure and is NOT retried, unless it
// used the ExitRetryable code to explicitly request one.
func classifyTermination(pod *corev1.Pod) terminationInfo {
	statuses := append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	for _, cs := range statuses {
		t := cs.State.Terminated
		if t == nil || t.ExitCode == 0 {
			continue
		}
		switch {
		case t.Reason == "OOMKilled":
			return terminationInfo{reason: "OOMKilled", retryable: true}
		case int(t.ExitCode) == runspec.ExitRetryable:
			return terminationInfo{reason: fmt.Sprintf("%s requested retry", cs.Name), retryable: true}
		default:
			return terminationInfo{reason: fmt.Sprintf("%s exit %d", cs.Name, t.ExitCode), retryable: false}
		}
	}
	if pod.Status.Reason != "" {
		return terminationInfo{reason: pod.Status.Reason, retryable: true} // Evicted, NodeLost, ...
	}
	return terminationInfo{reason: "unknown failure", retryable: true}
}

// setCondition upserts a condition by type.
func setCondition(run *wrenv1.AgentRun, c metav1.Condition) {
	c.LastTransitionTime = metav1.Now()
	c.ObservedGeneration = run.Generation
	for i := range run.Status.Conditions {
		if run.Status.Conditions[i].Type == c.Type {
			run.Status.Conditions[i] = c
			return
		}
	}
	run.Status.Conditions = append(run.Status.Conditions, c)
}
