package controller

import (
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// PodExecutor executes a bounded command in an existing pod container. Pause
// uses it only for the harness signal roles and trusted checkpointer role.
type PodExecutor interface {
	Execute(ctx context.Context, namespace, pod, container string, command []string) ([]byte, error)
}

type k8sPodExecutor struct {
	config *rest.Config
	client kubernetes.Interface
}

func NewPodExecutor(config *rest.Config, client kubernetes.Interface) PodExecutor {
	return &k8sPodExecutor{config: rest.CopyConfig(config), client: client}
}

func (e *k8sPodExecutor) Execute(ctx context.Context, namespace, pod, container string, command []string) ([]byte, error) {
	req := e.client.CoreV1().RESTClient().Post().
		Resource("pods").Namespace(namespace).Name(pod).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{Container: container, Command: command, Stdout: true, Stderr: true}, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(e.config, "POST", req.URL())
	if err != nil {
		return nil, fmt.Errorf("build pod exec: %w", err)
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		return nil, fmt.Errorf("exec %s/%s container %s: %w: %s", namespace, pod, container, err, stderr.String())
	}
	return stdout.Bytes(), nil
}
