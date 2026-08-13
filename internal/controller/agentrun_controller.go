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
	"time"

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
	// Executor backs the pause critical section (quiesce/checkpoint/unquiesce).
	// A nil executor makes pause fail safely without deleting the live pod.
	Executor PodExecutor
}

// +kubebuilder:rbac:groups=wren.dev,resources=agentruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=wren.dev,resources=agentruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=wren.dev,resources=agentruns/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims;configmaps,verbs=get;list;watch;create;update;patch;delete

// Reconcile drives an AgentRun toward its terminal state.
func (r *AgentRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	lg := log.FromContext(ctx)

	var run wrenv1.AgentRun
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if result, handled, err := r.handleRequestedTransition(ctx, &run); handled {
		return result, err
	}

	if result, handled, err := r.ensurePrerequisites(ctx, &run); handled {
		return result, err
	}

	pod, err := r.ensurePod(ctx, &run)
	if err != nil {
		if errors.Is(err, errPodDisappeared) {
			return r.handleMissingPod(ctx, &run)
		}
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

// handleRequestedTransition handles lifecycle requests before normal pod
// reconciliation. The boolean distinguishes a handled no-op from "continue".
func (r *AgentRunReconciler) handleRequestedTransition(ctx context.Context, run *wrenv1.AgentRun) (ctrl.Result, bool, error) {
	if !run.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, true, nil // owned children are garbage-collected
	}
	if run.Status.Phase == wrenv1.PhasePaused {
		switch {
		case run.Annotations[wrenv1.CancelAnnotation] == "true":
			result, err := r.cancel(ctx, run)
			return result, true, err
		case run.Annotations[wrenv1.ResumeAnnotation] == "true":
			result, err := r.resume(ctx, run)
			return result, true, err
		default:
			return ctrl.Result{}, true, nil
		}
	}
	if isTerminal(run.Status.Phase) {
		// Only an explicit request may resurrect a Failed run. Succeeded and
		// Canceled runs remain terminal even if they carry a stray annotation.
		if run.Status.Phase == wrenv1.PhaseFailed && run.Annotations[wrenv1.ResumeAnnotation] == "true" {
			result, err := r.resume(ctx, run)
			return result, true, err
		}
		return ctrl.Result{}, true, nil
	}
	if run.Annotations[wrenv1.CancelAnnotation] == "true" {
		result, err := r.cancel(ctx, run)
		return result, true, err
	}
	if run.Status.Phase == wrenv1.PhasePausing || run.Annotations[wrenv1.PauseAnnotation] == "true" {
		result, err := r.pause(ctx, run)
		return result, true, err
	}
	if run.Status.Phase == "" {
		result, err := r.setPhase(ctx, run, wrenv1.PhasePending, "Admitted", "run accepted")
		return result, true, err
	}
	return ctrl.Result{}, false, nil
}

// ensurePrerequisites records the run's security/durability posture and makes
// its durable workspace and RunSpec available before pod reconciliation.
func (r *AgentRunReconciler) ensurePrerequisites(ctx context.Context, run *wrenv1.AgentRun) (ctrl.Result, bool, error) {
	if err := r.ensureEgressCondition(ctx, run); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("record egress condition: %w", err)
	}
	if err := r.ensureCheckpointStorageCondition(ctx, run); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("record checkpoint storage condition: %w", err)
	}
	if err := r.ensurePVC(ctx, run); err != nil {
		switch {
		case errors.Is(err, errWorkspaceLost):
			// Never replace a lost, uncheckpointed workspace with an empty disk.
			// Also remove a replacement pod created by an earlier disappearance
			// event before making the run terminal.
			if err := r.deleteCurrentPod(ctx, run); err != nil {
				return ctrl.Result{}, true, fmt.Errorf("delete pod after workspace loss: %w", err)
			}
			result, err := r.setPhase(ctx, run, wrenv1.PhaseFailed, "WorkspaceLost",
				"workspace PVC is gone after the run had already progressed past Pending — the disk (and any in-progress work) was destroyed; this run cannot resume and will not be retried")
			return result, true, err
		case errors.Is(err, errWorkspaceRestoring):
			return ctrl.Result{Requeue: true}, true, nil
		default:
			return ctrl.Result{}, true, fmt.Errorf("ensure workspace pvc: %w", err)
		}
	}
	if err := r.ensureRunSpec(ctx, run); err != nil {
		return ctrl.Result{}, true, fmt.Errorf("ensure runspec: %w", err)
	}
	return ctrl.Result{}, false, nil
}

// egressEnforcementConditionType is the condition type recording the egress
// bypass-prevention posture (spec §5.6, WS-1).
const egressEnforcementConditionType = "EgressEnforcement"
const checkpointStorageConditionType = "CheckpointStorage"

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

func (r *AgentRunReconciler) ensureCheckpointStorageCondition(ctx context.Context, run *wrenv1.AgentRun) error {
	want := metav1.Condition{Type: checkpointStorageConditionType, Status: metav1.ConditionFalse, Reason: "Unavailable", Message: "operator has no checkpoint mount configured; durable pause is unavailable"}
	if r.PodConfig.checkpointMountEnabled(run) {
		want.Status = metav1.ConditionTrue
		want.Reason = "Mounted"
		want.Message = "trusted checkpointer and hydrate containers have checkpoint storage; harness does not"
	}
	if existing := findCondition(run, checkpointStorageConditionType); existing != nil && existing.Status == want.Status && existing.Reason == want.Reason {
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

// errPodDisappeared means the pod recorded in status vanished without a
// terminal pod object for classifyTermination to inspect (for example an
// external deletion or node loss). It is still an infrastructure interruption
// and must consume retry budget and advance to a resume generation.
var errPodDisappeared = errors.New("recorded run pod disappeared")

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
const pauseResumeConditionType = "PauseResumePending"

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
// operator has no checkpoint mount enabled), silently creating a fresh,
// empty PVC would resume the harness into a workspace with no signal that
// everything on disk was lost (WS-8 truthing pass; WS-16 A.4) — so that case
// still returns errWorkspaceLost, unchanged from before WS-21.
//
// For a run that HAS opted in (restoreEligible), the loss is recoverable: the
// dead pod (if any) is deleted, the attempt/retry counters are advanced once
// for the recovery incident, and workspaceRestoreConditionType is set True —
// but the PVC is NOT created in this
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

	restoreEligible := r.PodConfig.checkpointMountEnabled(run)
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

	// A pod disappearance and its PVC deletion commonly arrive as two separate
	// watch events for the same node-loss incident. If recovery is already in
	// progress, do not charge the retry budget or advance the pod generation a
	// second time. The recovery pod (if Kubernetes managed to create it before
	// the PVC finalizer completed) has not reached Running and performed work.
	resuming := findCondition(run, "Resuming")
	recoveryAlreadyCharged := resuming != nil && resuming.Status == metav1.ConditionTrue
	if !recoveryAlreadyCharged {
		advanceAttempt(run)
		run.Status.RestartCount++
	} else {
		// We deliberately deleted this not-yet-running recovery pod above. Clear
		// its recorded identity so ensurePod may recreate the same generation;
		// otherwise the missing-pod detector would count our own deletion as a
		// second interruption on the following reconcile.
		run.Status.PodName = ""
	}
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

// clearRecoveryConditions closes the active recovery window once a replacement
// pod is confirmed Running. While Resuming=True, a delayed PVC-deletion event
// is part of the same infrastructure incident and must not consume a second
// retry. This is a no-op when both conditions are absent/already False.
func (r *AgentRunReconciler) clearRecoveryConditions(ctx context.Context, run *wrenv1.AgentRun) error {
	changed := false
	if existing := findCondition(run, workspaceRestoreConditionType); existing != nil && existing.Status == metav1.ConditionTrue {
		setCondition(run, metav1.Condition{
			Type:    workspaceRestoreConditionType,
			Status:  metav1.ConditionFalse,
			Reason:  "PodRunning",
			Message: "workspace restore (if any) completed; hydrate succeeded and the pod is running",
		})
		changed = true
	}
	if existing := findCondition(run, "Resuming"); existing != nil && existing.Status == metav1.ConditionTrue {
		setCondition(run, metav1.Condition{
			Type:    "Resuming",
			Status:  metav1.ConditionFalse,
			Reason:  "PodRunning",
			Message: "replacement pod is running; recovery incident completed",
		})
		changed = true
	}
	if existing := findCondition(run, pauseResumeConditionType); existing != nil && existing.Status == metav1.ConditionTrue {
		setCondition(run, metav1.Condition{Type: pauseResumeConditionType, Status: metav1.ConditionFalse, Reason: "PodRunning", Message: "exact paused checkpoint is no longer pinned; resumed workspace is live"})
		changed = true
	}
	if !changed {
		return nil
	}
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
		Mode:             mode(attemptGeneration(run) > 0),
		Interactive:      run.Spec.Interactive,
		CheckpointBucket: run.Spec.Workspace.Checkpoint.Bucket,
		RestoreRequired:  restoreCond != nil && restoreCond.Status == metav1.ConditionTrue,
		BranchPrefix:     fmt.Sprintf("%s/%s", branchPrefix, sanitizeRef(run.Spec.User)),
	}
	pauseResume := findCondition(run, pauseResumeConditionType)
	if rs.RestoreRequired && run.Status.LastCheckpoint != nil && pauseResume != nil && pauseResume.Status == metav1.ConditionTrue {
		rs.RestoreCheckpoint = run.Status.LastCheckpoint.URI
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
	if run.Status.PodName == podName(run) {
		return nil, errPodDisappeared
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
	// Persist the exact pod identity before interpreting its phase. A later
	// NotFound for this name is then distinguishable from first provisioning
	// and is recovered as an interrupted attempt rather than silently recreated
	// in start mode.
	if run.Status.PodName != pod.Name {
		run.Status.PodName = pod.Name
		if err := r.Status().Update(ctx, run); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
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
		if err := r.clearRecoveryConditions(ctx, run); err != nil {
			return ctrl.Result{}, fmt.Errorf("clear recovery conditions: %w", err)
		}
		changed := r.scrapeCheckpointStatus(ctx, run, pod)
		if run.Status.Phase != wrenv1.PhaseRunning {
			run.Status.Phase = wrenv1.PhaseRunning
			setCondition(run, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "PodRunning", Message: "harness running"})
			changed = true
		}
		if changed {
			if err := r.Status().Update(ctx, run); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: checkpointStatusPoll(run)}, nil
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

// handleMissingPod recovers an infrastructure disappearance for which no
// terminal pod remains to classify (external deletion, node loss). The
// recorded PodName is the proof this was a real attempt, not first creation.
func (r *AgentRunReconciler) handleMissingPod(ctx context.Context, run *wrenv1.AgentRun) (ctrl.Result, error) {
	max := run.Spec.Retry.MaxRestarts
	if max == 0 {
		max = defaultMaxRestarts
	}
	if run.Status.RestartCount >= max {
		return r.setPhase(ctx, run, wrenv1.PhaseFailed, "RetryBudgetExhausted",
			fmt.Sprintf("failed after %d restarts (pod disappeared)", run.Status.RestartCount))
	}
	advanceAttempt(run)
	run.Status.RestartCount++
	run.Status.Phase = wrenv1.PhaseInterrupted
	setCondition(run, metav1.Condition{
		Type:    "Resuming",
		Status:  metav1.ConditionTrue,
		Reason:  "PodDisappeared",
		Message: fmt.Sprintf("restart %d/%d after recorded pod disappeared", run.Status.RestartCount, max),
	})
	if err := r.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
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
	advanceAttempt(run)
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
	if err := r.deleteCurrentPod(ctx, run); err != nil {
		return ctrl.Result{}, fmt.Errorf("delete pod on cancel: %w", err)
	}
	return r.setPhase(ctx, run, wrenv1.PhaseCanceled, "Canceled", "run canceled by user (wren run stop)")
}

func (r *AgentRunReconciler) deleteCurrentPod(ctx context.Context, run *wrenv1.AgentRun) error {
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: podName(run)}, &pod)
	switch {
	case err == nil:
		if delErr := r.Delete(ctx, &pod, client.PropagationPolicy(metav1.DeletePropagationBackground)); delErr != nil && !apierrors.IsNotFound(delErr) {
			return delErr
		}
	case !apierrors.IsNotFound(err):
		return err
	}
	return nil
}

const pauseCheckpointConditionType = "PauseCheckpointReady"

// pause performs the durable pause as a restart-safe state machine. Status is
// the journal: once the checkpoint reference is committed, reconciliation
// never executes another snapshot and proceeds only to pod removal.
func (r *AgentRunReconciler) pause(ctx context.Context, run *wrenv1.AgentRun) (ctrl.Result, error) {
	if !r.PodConfig.checkpointMountEnabled(run) {
		return r.abortPause(ctx, run, nil, errors.New("durable pause is unavailable because checkpoint storage is not mounted by the operator"))
	}
	if run.Status.Phase != wrenv1.PhasePausing {
		run.Status.Phase = wrenv1.PhasePausing
		setCondition(run, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "Pausing", Message: "quiescing harness and publishing a verified checkpoint"})
		if err := r.Status().Update(ctx, run); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}

	ready := findCondition(run, pauseCheckpointConditionType)
	if ready == nil || ready.Status != metav1.ConditionTrue || run.Status.LastCheckpoint == nil {
		return r.publishPauseCheckpoint(ctx, run)
	}
	return r.removeCheckpointedPod(ctx, run)
}

type pauseCheckpointProof struct {
	ID            string    `json:"id"`
	ManifestKey   string    `json:"manifestKey"`
	SHA256        string    `json:"sha256"`
	SizeBytes     int64     `json:"sizeBytes"`
	FormatVersion int32     `json:"formatVersion"`
	CreatedAt     time.Time `json:"createdAt"`
	Trigger       string    `json:"trigger"`
	Warning       string    `json:"warning"`
}

func (r *AgentRunReconciler) publishPauseCheckpoint(ctx context.Context, run *wrenv1.AgentRun) (ctrl.Result, error) {
	if r.Executor == nil {
		return r.abortPause(ctx, run, nil, errors.New("operator pod executor is not configured"))
	}
	var pod corev1.Pod
	if err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: podName(run)}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return r.setPhase(ctx, run, wrenv1.PhaseFailed, "PausePodLost", "agent pod disappeared before a pause checkpoint was verified")
		}
		return ctrl.Result{}, err
	}
	if _, err := r.Executor.Execute(ctx, run.Namespace, pod.Name, ContainerHarness, []string{"/usr/local/bin/wren-runtime", "quiesce"}); err != nil {
		return r.abortPause(ctx, run, &pod, fmt.Errorf("quiesce harness: %w", err))
	}
	out, err := r.Executor.Execute(ctx, run.Namespace, pod.Name, ContainerCheckpointer, []string{"/usr/local/bin/wren-runtime", "checkpoint-once"})
	if err != nil {
		return r.abortPause(ctx, run, &pod, fmt.Errorf("forced checkpoint: %w", err))
	}
	var proof pauseCheckpointProof
	if err := json.Unmarshal(out, &proof); err != nil || proof.ID == "" || proof.ManifestKey == "" || proof.SHA256 == "" {
		if err == nil {
			err = errors.New("checkpoint proof is incomplete")
		}
		return r.abortPause(ctx, run, &pod, fmt.Errorf("decode checkpoint proof: %w", err))
	}
	run.Status.LastCheckpoint = &wrenv1.CheckpointRef{ID: proof.ID, URI: proof.ManifestKey, At: metav1.NewTime(proof.CreatedAt), SHA256: proof.SHA256, SizeBytes: proof.SizeBytes, FormatVersion: proof.FormatVersion, Trigger: proof.Trigger}
	setCondition(run, metav1.Condition{Type: pauseCheckpointConditionType, Status: metav1.ConditionTrue, Reason: "Verified", Message: fmt.Sprintf("checkpoint %s verified (sha256=%s)", proof.ID, proof.SHA256)})
	if proof.Warning != "" {
		setCondition(run, metav1.Condition{Type: "CheckpointRetention", Status: metav1.ConditionFalse, Reason: "CleanupFailed", Message: proof.Warning})
	}
	if err := r.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{Requeue: true}, nil
}

func (r *AgentRunReconciler) removeCheckpointedPod(ctx context.Context, run *wrenv1.AgentRun) (ctrl.Result, error) {
	var pod corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: podName(run)}, &pod)
	if err == nil {
		zero := int64(0)
		if err := r.Delete(ctx, &pod, &client.DeleteOptions{GracePeriodSeconds: &zero, PropagationPolicy: ptr(metav1.DeletePropagationBackground)}); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("delete checkpointed pod on pause: %w", err)
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	delete(run.Annotations, wrenv1.PauseAnnotation)
	if err := r.Update(ctx, run); err != nil {
		return ctrl.Result{}, fmt.Errorf("clear pause annotation: %w", err)
	}
	return r.setPhase(ctx, run, wrenv1.PhasePaused, "Paused", fmt.Sprintf("paused at verified checkpoint %s; no agent pod is running", run.Status.LastCheckpoint.ID))
}

func (r *AgentRunReconciler) abortPause(ctx context.Context, run *wrenv1.AgentRun, pod *corev1.Pod, cause error) (ctrl.Result, error) {
	if pod != nil && r.Executor != nil {
		if _, err := r.Executor.Execute(ctx, run.Namespace, pod.Name, ContainerHarness, []string{"/usr/local/bin/wren-runtime", "unquiesce"}); err != nil {
			cause = errors.Join(cause, fmt.Errorf("unquiesce harness: %w", err))
		}
	}
	delete(run.Annotations, wrenv1.PauseAnnotation)
	if err := r.Update(ctx, run); err != nil {
		return ctrl.Result{}, fmt.Errorf("clear failed pause request: %w", err)
	}
	run.Status.Phase = wrenv1.PhaseRunning
	setCondition(run, metav1.Condition{Type: pauseCheckpointConditionType, Status: metav1.ConditionFalse, Reason: "CheckpointFailed", Message: cause.Error()})
	setCondition(run, metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "PauseAborted", Message: "pause failed safely; harness was left running"})
	if err := r.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// resume manually restarts a terminally-Failed run at the user's request
// (wren run resume): it deletes any leftover pod from the failed attempt,
// resets the retry budget, and drops the run back to Interrupted — the exact
// non-terminal phase handlePodFailure uses for an ordinary crash-resume, so
// the very next reconcile takes that same, already-tested path (ensurePVC's
// checks, ensurePod recreating the pod at a fresh generation). This is deliberate:
// resume gives the reconciler another real attempt, it does not paper over
// whatever made the run fail — a run that failed WorkspaceLost because its
// PVC is genuinely gone will hit ensurePVC's same NotFound check on the very
// next reconcile and fail WorkspaceLost again, truthfully.
func (r *AgentRunReconciler) resume(ctx context.Context, run *wrenv1.AgentRun) (ctrl.Result, error) {
	wasPaused := run.Status.Phase == wrenv1.PhasePaused
	// Resolve and delete the CURRENT attempt's pod before advancing its monotonic
	// generation and resetting the independent automatic-retry counter.
	// This must happen before the status mutation below so the exact pod that
	// failed remains addressable.
	// Delete the leftover pod (if handlePodFailure left one for diagnosis, e.g.
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

	advanceAttempt(run) // resume mode + a never-reused pod generation
	if !wasPaused {
		run.Status.RestartCount = 0 // failed manual resume gets a fresh retry budget
	}
	if wasPaused {
		setCondition(run, metav1.Condition{Type: pauseResumeConditionType, Status: metav1.ConditionTrue, Reason: "ManualResume", Message: "resume is pinned to the verified pause checkpoint if the PVC must be recreated"})
	}
	run.Status.Phase = wrenv1.PhaseInterrupted
	setCondition(run, metav1.Condition{
		Type:    "Resumed",
		Status:  metav1.ConditionTrue,
		Reason:  "ManualResume",
		Message: "manually resumed via `wren run resume` from durable workspace state",
	})
	setCondition(run, metav1.Condition{
		Type:    "Resuming",
		Status:  metav1.ConditionTrue,
		Reason:  "ManualResume",
		Message: "manual recovery attempt is pending",
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
