// Package controller holds the Wren operator's reconcilers: AgentRun (one agent
// run → a hardened pod with a durable workspace and crash-resume) and AgentPool
// (pre-warmed pods).
package controller

import (
	"context"
	"encoding/json"
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
}

// +kubebuilder:rbac:groups=wren.dev,resources=agentruns,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=wren.dev,resources=agentruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=wren.dev,resources=agentruns/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
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
		return ctrl.Result{}, nil
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
		return ctrl.Result{}, fmt.Errorf("ensure workspace pvc: %w", err)
	}
	if err := r.ensureRunSpec(ctx, &run); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure runspec: %w", err)
	}

	pod, err := r.ensurePod(ctx, &run)
	if err != nil {
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

// ensurePVC creates the workspace PVC if it does not already exist. The PVC name
// is stable across restarts so a surviving disk is reattached on resume.
func (r *AgentRunReconciler) ensurePVC(ctx context.Context, run *wrenv1.AgentRun) error {
	var existing corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: pvcName(run)}, &existing)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	pvc := buildWorkspacePVC(run)
	if err := controllerutil.SetControllerReference(run, pvc, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, pvc)
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
		return r.setPhaseIfChanged(ctx, run, wrenv1.PhaseRunning, "PodRunning", "harness running")
	case corev1.PodSucceeded:
		// Harness exited 0: the PR is opened by the harness/control plane; the
		// operator records terminal success.
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
