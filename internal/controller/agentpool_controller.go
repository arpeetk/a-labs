package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
)

// AgentPoolReconciler maintains a set of pre-warmed, repo-agnostic agent pods.
//
// This is an M0 skeleton: it maintains the desired replica count of idle warm
// pods and reports occupancy. Claim/hand-off to AgentRuns lands in M3.
type AgentPoolReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=wren.dev,resources=agentpools,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=wren.dev,resources=agentpools/status,verbs=get;update;patch

// Reconcile keeps the pool at its desired warm-pod count.
func (r *AgentPoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pool wrenv1.AgentPool
	if err := r.Get(ctx, req.NamespacedName, &pool); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !pool.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}

	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(pool.Namespace),
		client.MatchingLabels{LabelPool: pool.Name},
	); err != nil {
		return ctrl.Result{}, err
	}

	// Scale up toward the desired replica count.
	for i := int32(len(pods.Items)); i < pool.Spec.Replicas; i++ {
		if err := r.createWarmPod(ctx, &pool, i); err != nil {
			return ctrl.Result{}, fmt.Errorf("create warm pod: %w", err)
		}
	}

	available := int32(0)
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			available++
		}
	}
	if pool.Status.Available != available || pool.Status.Claimed != 0 {
		pool.Status.Available = available
		pool.Status.Claimed = 0
		if err := r.Status().Update(ctx, &pool); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *AgentPoolReconciler) createWarmPod(ctx context.Context, pool *wrenv1.AgentPool, idx int32) error {
	pause := corev1.ContainerRestartPolicyAlways
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-warm-%d", pool.Name, idx),
			Namespace: pool.Namespace,
			Labels:    map[string]string{LabelPool: pool.Name, LabelComponent: "warm"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			RuntimeClassName:             runtimeClassName(pool.Spec.RuntimeClass),
			AutomountServiceAccountToken: ptr(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr(true),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			// A warm pod idles on the harness image until claimed by a run.
			Containers: []corev1.Container{{
				Name:            ContainerHarness,
				Image:           pool.Spec.HarnessImage,
				RestartPolicy:   &pause,
				SecurityContext: hardened(true),
				Resources:       resources(pool.Spec.Resources),
			}},
		},
	}
	if err := controllerutil.SetControllerReference(pool, pod, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, pod)
}

// SetupWithManager wires the pool reconciler.
func (r *AgentPoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&wrenv1.AgentPool{}).
		Owns(&corev1.Pod{}).
		Complete(r)
}
