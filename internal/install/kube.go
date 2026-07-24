package install

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/retry"
)

// realKube is the client-go implementation of Kube. It is deliberately thin —
// all decisions live in the Installer — and is exercised against a real cluster
// by `wren install --kind` (the WS-13 DoD); the unit tests cover it through the
// client-go fakes where those exist (kubernetes/fake).
type realKube struct {
	cs     kubernetes.Interface
	disco  discovery.DiscoveryInterface
	dyn    dynamic.Interface
	mapper meta.RESTMapper
}

// kubeConfig builds a *rest.Config for the chosen context ("" = the kubeconfig
// current-context), honoring the standard loading rules (~/.kube/config,
// $KUBECONFIG).
func kubeConfig(kubeContext string) (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: kubeContext}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kube config: %w", err)
	}
	return cfg, nil
}

// newRealKube wires the typed, discovery, and dynamic clients off one config.
func newRealKube(cfg *rest.Config) (*realKube, error) {
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	// DeferredDiscoveryRESTMapper resolves each object's mapping on first use
	// and re-discovers on miss, so freshly-applied CRDs need no restart.
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(cs.Discovery()))
	return &realKube{cs: cs, disco: cs.Discovery(), dyn: dyn, mapper: mapper}, nil
}

// New builds an Installer against the real cluster and the real local tools,
// streaming progress to out. kubeContext "" = the kubeconfig current-context.
// The cluster connection is established lazily on first use: a kind install
// creates its cluster (and thus its context) mid-flow, after preflight.
// The CLI wires PromptSecret separately (terminal-dependent).
func New(kubeContext string, out io.Writer) (*Installer, error) {
	return &Installer{
		Kube:   &lazyKube{context: kubeContext},
		Runner: &execRunner{out: out},
		Out:    out,
	}, nil
}

// lazyKube defers loading the kube config until the first cluster call (see New).
type lazyKube struct {
	context string
	once    sync.Once
	k       *realKube
	err     error
}

func (l *lazyKube) get() (*realKube, error) {
	l.once.Do(func() {
		cfg, err := kubeConfig(l.context)
		if err != nil {
			l.err = err
			return
		}
		l.k, l.err = newRealKube(cfg)
	})
	return l.k, l.err
}

func (l *lazyKube) ServerVersion(ctx context.Context) (string, error) {
	k, err := l.get()
	if err != nil {
		return "", err
	}
	return k.ServerVersion(ctx)
}

func (l *lazyKube) ApplyManifests(ctx context.Context, manifests []byte) error {
	k, err := l.get()
	if err != nil {
		return err
	}
	return k.ApplyManifests(ctx, manifests)
}

func (l *lazyKube) EnsureNamespace(ctx context.Context, name string) error {
	k, err := l.get()
	if err != nil {
		return err
	}
	return k.EnsureNamespace(ctx, name)
}

func (l *lazyKube) UpsertSecret(ctx context.Context, ns, name string, data map[string]string) error {
	k, err := l.get()
	if err != nil {
		return err
	}
	return k.UpsertSecret(ctx, ns, name, data)
}

func (l *lazyKube) OverrideImages(ctx context.Context, registry, tag string) error {
	k, err := l.get()
	if err != nil {
		return err
	}
	return k.OverrideImages(ctx, registry, tag)
}

func (l *lazyKube) SetApiserverRunNamespace(ctx context.Context, namespace string) error {
	k, err := l.get()
	if err != nil {
		return err
	}
	return k.SetApiserverRunNamespace(ctx, namespace)
}

func (l *lazyKube) SetServiceType(ctx context.Context, ns, name, svcType string) error {
	k, err := l.get()
	if err != nil {
		return err
	}
	return k.SetServiceType(ctx, ns, name, svcType)
}

func (l *lazyKube) WaitDeployments(ctx context.Context, ns string, names []string, timeout time.Duration) error {
	k, err := l.get()
	if err != nil {
		return err
	}
	return k.WaitDeployments(ctx, ns, names, timeout)
}

func (l *lazyKube) DeleteNamespace(ctx context.Context, name string, timeout time.Duration) error {
	k, err := l.get()
	if err != nil {
		return err
	}
	return k.DeleteNamespace(ctx, name, timeout)
}

func (l *lazyKube) DeleteClusterScoped(ctx context.Context, manifests []byte) error {
	k, err := l.get()
	if err != nil {
		return err
	}
	return k.DeleteClusterScoped(ctx, manifests)
}

func (k *realKube) ServerVersion(ctx context.Context) (string, error) {
	v, err := k.disco.ServerVersion()
	if err != nil {
		return "", err
	}
	return v.Major + "." + v.Minor, nil
}

// ApplyManifests server-side-applies every document in a multi-doc YAML stream
// with field-manager "wren". SSA is what makes re-installs idempotent: the same
// rendered asset applied twice converges instead of conflicting. NOTE: the
// client-go fakes do not model SSA, so this is covered against a real cluster
// by `wren install --kind` (run twice, in the WS-13 DoD) — the decoding half is
// unit-tested via splitManifests.
func (k *realKube) ApplyManifests(ctx context.Context, data []byte) error {
	objs, err := splitManifests(data)
	if err != nil {
		return err
	}
	for i := range objs {
		u := &objs[i]
		gvk := u.GroupVersionKind()
		mapping, err := k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return fmt.Errorf("map %s: %w", gvk, err)
		}
		var ri dynamic.ResourceInterface = k.dyn.Resource(mapping.Resource)
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			ns := u.GetNamespace()
			if ns == "" {
				return fmt.Errorf("%s %q has no namespace", gvk.Kind, u.GetName())
			}
			ri = k.dyn.Resource(mapping.Resource).Namespace(ns)
		}
		payload, err := json.Marshal(u.Object)
		if err != nil {
			return fmt.Errorf("encode %s/%s: %w", gvk.Kind, u.GetName(), err)
		}
		force := true
		if _, err := ri.Patch(ctx, u.GetName(), types.ApplyPatchType, payload, metav1.PatchOptions{
			FieldManager: "wren",
			Force:        &force,
		}); err != nil {
			return fmt.Errorf("apply %s/%s: %w", gvk.Kind, u.GetName(), err)
		}
	}
	return nil
}

// splitManifests decodes a multi-doc YAML stream into unstructured objects,
// skipping empty documents (trailing "---" separators).
func splitManifests(data []byte) ([]unstructured.Unstructured, error) {
	dec := kubeyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var out []unstructured.Unstructured
	for {
		var raw map[string]any
		err := dec.Decode(&raw)
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("decode manifest: %w", err)
		}
		if len(raw) == 0 {
			continue
		}
		out = append(out, unstructured.Unstructured{Object: raw})
	}
}

func (k *realKube) EnsureNamespace(ctx context.Context, name string) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	_, err := k.cs.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	switch {
	case err == nil, apierrors.IsAlreadyExists(err):
		return nil
	default:
		return err
	}
}

// UpsertSecret creates or replaces a Secret's data. Re-running install rotates
// the credential in place rather than failing on AlreadyExists.
func (k *realKube) UpsertSecret(ctx context.Context, ns, name string, data map[string]string) error {
	secrets := k.cs.CoreV1().Secrets(ns)
	cur, err := secrets.Get(ctx, name, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		_, err = secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
			StringData: data,
		}, metav1.CreateOptions{})
		return err
	case err != nil:
		return err
	}
	if cur.StringData == nil {
		cur.StringData = map[string]string{}
	}
	for key, v := range data {
		cur.StringData[key] = v
	}
	_, err = secrets.Update(ctx, cur, metav1.UpdateOptions{})
	return err
}

// OverrideImages points the control plane at pushed images: the operator +
// apiserver Deployment images, the operator's --runtime-image arg (replaced in
// place if present, appended otherwise — Go flags: last occurrence wins), and
// imagePullPolicy=Always so a re-pushed tag is never served stale. This is the
// hack/e2e-gke.sh pattern (kubectl set image + arg patch) with a typed client.
// Each Deployment's get-modify-update runs under RetryOnConflict: the
// Deployment controller writes to the same object (status, revision
// annotation) the moment a prior step's edit lands, so a bare Update racing
// it is a routine 409, not an edge case — observed live against a real GKE
// cluster, not hypothetical.
func (k *realKube) OverrideImages(ctx context.Context, registry, tag string) error {
	deploys := k.cs.AppsV1().Deployments(SystemNamespace)

	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		op, err := deploys.Get(ctx, OperatorDeployment, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get %s: %w", OperatorDeployment, err)
		}
		oc := containerByName(op, "operator")
		if oc == nil {
			return fmt.Errorf("%s has no container %q", OperatorDeployment, "operator")
		}
		oc.Image = registry + "/operator:" + tag
		oc.ImagePullPolicy = corev1.PullAlways
		oc.Args = setArg(oc.Args, "--runtime-image=", registry+"/runtime:"+tag)
		_, err = deploys.Update(ctx, op, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("update %s: %w", OperatorDeployment, err)
	}

	err = retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		api, err := deploys.Get(ctx, ApiserverDeployment, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get %s: %w", ApiserverDeployment, err)
		}
		ac := containerByName(api, "apiserver")
		if ac == nil {
			return fmt.Errorf("%s has no container %q", ApiserverDeployment, "apiserver")
		}
		ac.Image = registry + "/apiserver:" + tag
		ac.ImagePullPolicy = corev1.PullAlways
		_, err = deploys.Update(ctx, api, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("update %s: %w", ApiserverDeployment, err)
	}
	return nil
}

// SetApiserverRunNamespace sets WREN_DEFAULT_RUN_NAMESPACE on the apiserver
// container so `wren project create` with no --namespace lands runs in the
// install's --run-namespace — the namespace where the proxy credential Secrets
// live (WS-15 Part A). Replaces the env in place if present (the manifest ships
// a default), appends it otherwise. Idempotent across re-installs. Runs under
// RetryOnConflict for the same reason as OverrideImages: it lands right after
// that call touches the same Deployment, while the Deployment controller is
// still actively reconciling it.
func (k *realKube) SetApiserverRunNamespace(ctx context.Context, namespace string) error {
	deploys := k.cs.AppsV1().Deployments(SystemNamespace)
	err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		api, err := deploys.Get(ctx, ApiserverDeployment, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get %s: %w", ApiserverDeployment, err)
		}
		ac := containerByName(api, "apiserver")
		if ac == nil {
			return fmt.Errorf("%s has no container %q", ApiserverDeployment, "apiserver")
		}
		ac.Env = setEnv(ac.Env, "WREN_DEFAULT_RUN_NAMESPACE", namespace)
		_, err = deploys.Update(ctx, api, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("update %s: %w", ApiserverDeployment, err)
	}
	return nil
}

// setEnv replaces the value of a named env var if present, else appends it.
func setEnv(env []corev1.EnvVar, name, value string) []corev1.EnvVar {
	for i := range env {
		if env[i].Name == name {
			env[i].Value = value
			env[i].ValueFrom = nil
			return env
		}
	}
	return append(env, corev1.EnvVar{Name: name, Value: value})
}

// containerByName finds a container in a Deployment's pod template.
func containerByName(d *appsv1.Deployment, name string) *corev1.Container {
	for i := range d.Spec.Template.Spec.Containers {
		if d.Spec.Template.Spec.Containers[i].Name == name {
			return &d.Spec.Template.Spec.Containers[i]
		}
	}
	return nil
}

// setArg replaces the value of a --flag= arg if present, else appends it.
func setArg(args []string, prefix, value string) []string {
	for i, a := range args {
		if strings.HasPrefix(a, prefix) {
			args[i] = prefix + value
			return args
		}
	}
	return append(args, prefix+value)
}

func (k *realKube) SetServiceType(ctx context.Context, ns, name, svcType string) error {
	svcs := k.cs.CoreV1().Services(ns)
	svc, err := svcs.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	svc.Spec.Type = corev1.ServiceType(svcType)
	// Switching away from LoadBalancer must drop stale allocations.
	if svc.Spec.Type != corev1.ServiceTypeLoadBalancer {
		svc.Spec.LoadBalancerIP = ""
	}
	_, err = svcs.Update(ctx, svc, metav1.UpdateOptions{})
	return err
}

// WaitDeployments polls until each Deployment reports Available (mirrors
// `kubectl rollout status`, without exec'ing kubectl).
func (k *realKube) WaitDeployments(ctx context.Context, ns string, names []string, timeout time.Duration) error {
	deploys := k.cs.AppsV1().Deployments(ns)
	for _, name := range names {
		err := wait.PollUntilContextTimeout(ctx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
			d, err := deploys.Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				return false, nil // apply may still be settling
			}
			if err != nil {
				return false, err
			}
			// Available + the controller has observed the latest spec (so an
			// image override applied mid-wait still counts as "our" rollout).
			if d.Status.ObservedGeneration < d.Generation {
				return false, nil
			}
			for _, c := range d.Status.Conditions {
				if c.Type == appsv1.DeploymentAvailable && c.Status == corev1.ConditionTrue {
					return true, nil
				}
			}
			return false, nil
		})
		if err != nil {
			return fmt.Errorf("deployment/%s: %w", name, err)
		}
	}
	return nil
}

// DeleteNamespace deletes a namespace (absent is fine) and waits until the API
// stops returning it, so `wren uninstall` leaves verifiably no residue.
func (k *realKube) DeleteNamespace(ctx context.Context, name string, timeout time.Duration) error {
	nss := k.cs.CoreV1().Namespaces()
	err := nss.Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if apierrors.IsNotFound(err) {
		return nil
	}
	return wait.PollUntilContextTimeout(ctx, 3*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := nss.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	})
}

// DeleteClusterScoped deletes every cluster-scoped object in a rendered
// manifest stream — CRDs (all instances go with them) and cluster RBAC — in
// reverse apply order. Namespaces are skipped (their deletion is awaited
// separately in DeleteNamespace) and namespaced objects die with their
// namespace. NotFound and unknown types are tolerated so a partial install
// still uninstalls clean.
func (k *realKube) DeleteClusterScoped(ctx context.Context, data []byte) error {
	objs, err := splitManifests(data)
	if err != nil {
		return err
	}
	for i := len(objs) - 1; i >= 0; i-- {
		u := &objs[i]
		if u.GetKind() == "Namespace" {
			continue
		}
		gvk := u.GroupVersionKind()
		mapping, err := k.mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if meta.IsNoMatchError(err) {
			continue // the cluster doesn't know the type → nothing to delete
		}
		if err != nil {
			return fmt.Errorf("map %s: %w", gvk, err)
		}
		if mapping.Scope.Name() != meta.RESTScopeNameRoot {
			continue
		}
		err = k.dyn.Resource(mapping.Resource).Delete(ctx, u.GetName(), metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s/%s: %w", gvk.Kind, u.GetName(), err)
		}
	}
	return nil
}
