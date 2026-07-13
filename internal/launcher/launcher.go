// Package launcher is the control plane's bridge to Kubernetes: it creates and
// reads AgentRun custom resources. The Runs service depends on the Launcher
// interface, never on a Kubernetes client directly, so the control-plane logic
// stays unit-testable (spec §5.2: "a stable API that hides Kubernetes").
package launcher

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
)

// Launcher creates and reads AgentRun resources in a cluster.
type Launcher interface {
	EnsureNamespace(ctx context.Context, ns string) error
	CreateRun(ctx context.Context, run *wrenv1.AgentRun) error
	GetRun(ctx context.Context, ns, name string) (*wrenv1.AgentRun, error)
	DeleteRun(ctx context.Context, ns, name string) error
}

// K8s is a Launcher backed by a controller-runtime client.
type K8s struct {
	c client.Client
}

var _ Launcher = (*K8s)(nil)

// NewScheme returns a runtime.Scheme with core + Wren types registered.
func NewScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := wrenv1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// NewK8s builds a K8s launcher from a REST config.
func NewK8s(cfg *rest.Config) (*K8s, error) {
	s, err := NewScheme()
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		return nil, err
	}
	return &K8s{c: c}, nil
}

func (k *K8s) EnsureNamespace(ctx context.Context, ns string) error {
	obj := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}
	err := k.c.Create(ctx, obj)
	if apierrors.IsAlreadyExists(err) {
		return nil
	}
	return err
}

func (k *K8s) CreateRun(ctx context.Context, run *wrenv1.AgentRun) error {
	return k.c.Create(ctx, run)
}

func (k *K8s) GetRun(ctx context.Context, ns, name string) (*wrenv1.AgentRun, error) {
	var run wrenv1.AgentRun
	if err := k.c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

func (k *K8s) DeleteRun(ctx context.Context, ns, name string) error {
	run := &wrenv1.AgentRun{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	err := k.c.Delete(ctx, run)
	return client.IgnoreNotFound(err)
}
