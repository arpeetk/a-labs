package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
)

func newPoolReconciler(t *testing.T, objs ...client.Object) (*AgentPoolReconciler, client.Client) {
	t.Helper()
	s := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(s).
		WithObjects(objs...).
		WithStatusSubresource(&wrenv1.AgentPool{}, &corev1.Pod{}).
		Build()
	return &AgentPoolReconciler{Client: c, Scheme: s}, c
}

func poolReconcile(t *testing.T, r *AgentPoolReconciler, ns, name string) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
	}); err != nil {
		t.Fatalf("pool reconcile: %v", err)
	}
}

func testPool() *wrenv1.AgentPool {
	return &wrenv1.AgentPool{
		ObjectMeta: metav1.ObjectMeta{Name: "warm", Namespace: "wren-system"},
		Spec: wrenv1.AgentPoolSpec{
			HarnessImage: "reg/claude-code:1.0",
			RuntimeClass: wrenv1.RuntimeRunc,
			Replicas:     2,
			Resources:    wrenv1.ResourceSpec{CPU: resource.MustParse("1"), Memory: resource.MustParse("1Gi")},
		},
	}
}

func poolPods(t *testing.T, c client.Client) *corev1.PodList {
	t.Helper()
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods, client.MatchingLabels{LabelPool: "warm"}); err != nil {
		t.Fatal(err)
	}
	return &pods
}

func TestAgentPoolScalesUpToReplicas(t *testing.T) {
	pool := testPool()
	r, c := newPoolReconciler(t, pool)

	poolReconcile(t, r, pool.Namespace, pool.Name)

	pods := poolPods(t, c)
	if len(pods.Items) != 2 {
		t.Fatalf("expected 2 warm pods, got %d", len(pods.Items))
	}
	for _, p := range pods.Items {
		if p.Spec.Containers[0].Image != "reg/claude-code:1.0" {
			t.Errorf("warm pod image = %q", p.Spec.Containers[0].Image)
		}
		if p.OwnerReferences[0].Kind != "AgentPool" {
			t.Errorf("warm pod not owned by pool: %+v", p.OwnerReferences)
		}
	}
}

func TestAgentPoolIdempotent(t *testing.T) {
	pool := testPool()
	r, c := newPoolReconciler(t, pool)
	poolReconcile(t, r, pool.Namespace, pool.Name)
	poolReconcile(t, r, pool.Namespace, pool.Name) // second pass must not add more
	if got := len(poolPods(t, c).Items); got != 2 {
		t.Fatalf("expected 2 pods after 2 reconciles, got %d", got)
	}
}

func TestAgentPoolReportsAvailable(t *testing.T) {
	pool := testPool()
	r, c := newPoolReconciler(t, pool)
	poolReconcile(t, r, pool.Namespace, pool.Name)

	// Mark both warm pods Running.
	for _, p := range poolPods(t, c).Items {
		p.Status.Phase = corev1.PodRunning
		if err := c.Status().Update(context.Background(), &p); err != nil {
			t.Fatal(err)
		}
	}
	poolReconcile(t, r, pool.Namespace, pool.Name)

	var got wrenv1.AgentPool
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: pool.Namespace, Name: pool.Name}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Available != 2 {
		t.Fatalf("status.available = %d, want 2", got.Status.Available)
	}
}

func TestAgentPoolMissingIsNoop(t *testing.T) {
	r, _ := newPoolReconciler(t)
	poolReconcile(t, r, "wren-system", "does-not-exist") // must not error
}
