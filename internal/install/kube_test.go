package install

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

// These cover the typed-client half of realKube through the client-go fake.
// The SSA apply path has no usable fake (see the seam note on ApplyManifests)
// and is covered against a real cluster by the kind install run.

func fakeKube(objs ...runtime.Object) *realKube {
	return &realKube{cs: fake.NewSimpleClientset(objs...)}
}

func deployment(ns, name, container, image string, args ...string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: container, Image: image, Args: args}},
				},
			},
		},
	}
}

func TestUpsertSecretCreateThenRotate(t *testing.T) {
	k := fakeKube()
	ctx := context.Background()
	if err := k.UpsertSecret(ctx, "ns", GitHubTokenSecret, map[string]string{"token": "v1"}); err != nil {
		t.Fatal(err)
	}
	// Second call updates in place (idempotent re-install / rotation).
	if err := k.UpsertSecret(ctx, "ns", GitHubTokenSecret, map[string]string{"token": "v2"}); err != nil {
		t.Fatal(err)
	}
	s, err := k.cs.CoreV1().Secrets("ns").Get(ctx, GitHubTokenSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// A real apiserver folds StringData into Data on write; the fake stores it
	// verbatim, so assert against StringData here.
	if s.StringData["token"] != "v2" {
		t.Errorf("token = %q, want rotated v2", s.StringData["token"])
	}
}

func TestEnsureNamespaceIdempotent(t *testing.T) {
	k := fakeKube()
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := k.EnsureNamespace(ctx, "wren-runs"); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
}

func TestOverrideImages(t *testing.T) {
	op := deployment(SystemNamespace, OperatorDeployment, "operator", "wren/operator:dev",
		"--leader-elect", "--runtime-image=wren/runtime:dev")
	api := deployment(SystemNamespace, ApiserverDeployment, "apiserver", "wren/apiserver:dev")
	k := fakeKube(op, api)
	ctx := context.Background()
	reg := "us-central1-docker.pkg.dev/p/wren"
	if err := k.OverrideImages(ctx, reg, "abc1234"); err != nil {
		t.Fatal(err)
	}

	gotOp, _ := k.cs.AppsV1().Deployments(SystemNamespace).Get(ctx, OperatorDeployment, metav1.GetOptions{})
	oc := containerByName(gotOp, "operator")
	if oc.Image != reg+"/operator:abc1234" {
		t.Errorf("operator image = %q", oc.Image)
	}
	if oc.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("operator pullPolicy = %q, want Always", oc.ImagePullPolicy)
	}
	// The pre-existing --runtime-image arg must be replaced in place, not
	// duplicated (Go flags: last wins — a duplicate would silently shadow).
	count := 0
	for _, a := range oc.Args {
		if a == "--runtime-image="+reg+"/runtime:abc1234" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("--runtime-image occurrences = %d, args: %v", count, oc.Args)
	}

	gotAPI, _ := k.cs.AppsV1().Deployments(SystemNamespace).Get(ctx, ApiserverDeployment, metav1.GetOptions{})
	ac := containerByName(gotAPI, "apiserver")
	if ac.Image != reg+"/apiserver:abc1234" || ac.ImagePullPolicy != corev1.PullAlways {
		t.Errorf("apiserver container = %+v", ac)
	}

	// Idempotent: a second override converges on the same single arg.
	if err := k.OverrideImages(ctx, reg, "def5678"); err != nil {
		t.Fatal(err)
	}
	gotOp, _ = k.cs.AppsV1().Deployments(SystemNamespace).Get(ctx, OperatorDeployment, metav1.GetOptions{})
	count = 0
	for _, a := range containerByName(gotOp, "operator").Args {
		if a == "--runtime-image="+reg+"/runtime:def5678" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("after re-override, --runtime-image occurrences = %d", count)
	}
}

func TestSetServiceType(t *testing.T) {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Namespace: SystemNamespace, Name: ApiserverService}}
	k := fakeKube(svc)
	ctx := context.Background()
	if err := k.SetServiceType(ctx, SystemNamespace, ApiserverService, "LoadBalancer"); err != nil {
		t.Fatal(err)
	}
	got, _ := k.cs.CoreV1().Services(SystemNamespace).Get(ctx, ApiserverService, metav1.GetOptions{})
	if got.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Errorf("service type = %q", got.Spec.Type)
	}
}

func TestWaitDeploymentsReady(t *testing.T) {
	d := deployment(SystemNamespace, OperatorDeployment, "operator", "img")
	d.Generation = 2
	d.Status.ObservedGeneration = 2
	d.Status.Conditions = []appsv1.DeploymentCondition{{
		Type:   appsv1.DeploymentAvailable,
		Status: corev1.ConditionTrue,
	}}
	k := fakeKube(d)
	if err := k.WaitDeployments(context.Background(), SystemNamespace, []string{OperatorDeployment}, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestWaitDeploymentsTimesOut(t *testing.T) {
	d := deployment(SystemNamespace, OperatorDeployment, "operator", "img")
	k := fakeKube(d)
	err := k.WaitDeployments(context.Background(), SystemNamespace, []string{OperatorDeployment}, 50*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout for a never-Available deployment")
	}
}

func TestDeleteNamespaceGone(t *testing.T) {
	k := fakeKube(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "wren-system"}})
	ctx := context.Background()
	if err := k.DeleteNamespace(ctx, "wren-system", time.Minute); err != nil {
		t.Fatal(err)
	}
	// Absent namespace is a no-op (uninstall on a partial install).
	if err := k.DeleteNamespace(ctx, "wren-system", time.Minute); err != nil {
		t.Fatal(err)
	}
}

func TestSetArg(t *testing.T) {
	args := setArg([]string{"--leader-elect", "--runtime-image=old"}, "--runtime-image=", "new")
	if len(args) != 2 || args[1] != "--runtime-image=new" {
		t.Errorf("replace: %v", args)
	}
	args = setArg([]string{"--leader-elect"}, "--runtime-image=", "new")
	if len(args) != 2 || args[1] != "--runtime-image=new" {
		t.Errorf("append: %v", args)
	}
}

// DeleteClusterScoped must remove the CRDs + cluster RBAC from the rendered
// stream, tolerate objects that are already gone, and leave namespaced objects
// (and Namespaces, which are awaited separately) alone.
func TestDeleteClusterScoped(t *testing.T) {
	crdGVR := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
	crGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	crdGVK := schema.GroupVersionKind{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"}
	crGVK := schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"}
	crd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]any{"name": "agentruns.wren.dev"},
	}}
	clusterRole := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "ClusterRole",
		"metadata":   map[string]any{"name": "wren-operator-role"},
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(), map[schema.GroupVersionResource]string{
			crdGVR: "CustomResourceDefinitionList",
			crGVR:  "ClusterRoleList",
		}, crd, clusterRole)
	mapper := meta.NewDefaultRESTMapper(nil)
	mapper.Add(crdGVK, meta.RESTScopeRoot)
	mapper.Add(crGVK, meta.RESTScopeRoot)
	mapper.Add(schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "Role"}, meta.RESTScopeNamespace)

	// The stream mixes cluster-scoped and namespaced objects: CRDs (one absent
	// from the fake client → tolerated), a ClusterRole, a namespaced Role
	// (skipped — dies with the namespace), a Namespace (skipped — awaited
	// separately).
	stream := `
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: agentruns.wren.dev
---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: agentpools.wren.dev
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: wren-operator-role
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: wren-apiserver-role
  namespace: wren-system
---
apiVersion: v1
kind: Namespace
metadata:
  name: wren-system
`
	k := &realKube{dyn: dyn, mapper: mapper}
	ctx := context.Background()
	if err := k.DeleteClusterScoped(ctx, []byte(stream)); err != nil {
		t.Fatal(err)
	}
	if _, err := dyn.Resource(crdGVR).Get(ctx, "agentruns.wren.dev", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("agentruns CRD should be deleted, got err=%v", err)
	}
	if _, err := dyn.Resource(crGVR).Get(ctx, "wren-operator-role", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Errorf("ClusterRole should be deleted, got err=%v", err)
	}
}

func TestServerVersionViaDiscovery(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{Major: "1", Minor: "31"}
	k := &realKube{disco: cs.Discovery()}
	v, err := k.ServerVersion(context.Background())
	if err != nil || v != "1.31" {
		t.Errorf("ServerVersion = %q, %v", v, err)
	}
}

// kubeConfig honors KUBECONFIG, so an empty one keeps this hermetic: loading a
// config (or a named context) must fail, and the lazy Kube must surface that
// error on first use rather than at New time (kind creates its context
// mid-flow).
func TestLazyKubeSurfacesConfigError(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "no-such-file"))
	if _, err := kubeConfig("any-context"); err == nil {
		t.Fatal("expected config load error with an empty KUBECONFIG")
	}
	l := &lazyKube{context: ""}
	if _, err := l.ServerVersion(context.Background()); err == nil {
		t.Fatal("expected lazy config error on first cluster call")
	}
	// The error is sticky (sync.Once) — a second call fails the same way.
	if _, err := l.ServerVersion(context.Background()); err == nil {
		t.Fatal("expected sticky lazy config error")
	}
}

func TestNewWiresRealSeams(t *testing.T) {
	var out bytes.Buffer
	in, err := New("some-context", &out)
	if err != nil {
		t.Fatal(err)
	}
	if in.Kube == nil || in.Runner == nil || in.Out != &out {
		t.Errorf("New = %+v, want wired Kube/Runner/Out", in)
	}
}

func TestSplitManifestsSkipsEmptyDocs(t *testing.T) {
	objs, err := splitManifests([]byte("---\nkind: Namespace\nmetadata:\n  name: a\n---\n---\nkind: Secret\nmetadata:\n  name: b\n  namespace: a\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 2 || objs[0].GetKind() != "Namespace" || objs[1].GetKind() != "Secret" {
		t.Errorf("objs = %v", objs)
	}
}
