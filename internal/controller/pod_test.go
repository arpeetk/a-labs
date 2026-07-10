package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
)

func testRun() *wrenv1.AgentRun {
	return &wrenv1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "r-abc", Namespace: "user-a"},
		Spec: wrenv1.AgentRunSpec{
			Project: "payments-api",
			User:    "arpeet@corp.com",
			Harness: wrenv1.HarnessSpec{Kind: wrenv1.HarnessClaudeCode, Image: "reg/claude-code:1.0", Model: "claude-opus-4-8"},
			Task:    wrenv1.TaskSpec{Prompt: "do the thing", BaseRef: "main"},
			Sandbox: wrenv1.SandboxSpec{
				RuntimeClass: wrenv1.RuntimeRunc,
				Resources: wrenv1.ResourceSpec{
					CPU:           resource.MustParse("2"),
					Memory:        resource.MustParse("4Gi"),
					EphemeralDisk: resource.MustParse("10Gi"),
				},
			},
			Workspace: wrenv1.WorkspaceSpec{
				PVC:        wrenv1.PVCSpec{StorageClass: "regional-pd", Size: resource.MustParse("20Gi")},
				Checkpoint: wrenv1.CheckpointSpec{IntervalSeconds: 120, Bucket: "gs://wren-ckpt"},
			},
		},
	}
}

var testImages = Images{Runtime: "wren/runtime:test"}

func containerByName(cs []corev1.Container, name string) *corev1.Container {
	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}
	return nil
}

func TestBuildAgentPod(t *testing.T) {
	run := testRun()
	pod := buildAgentPod(run, PodConfig{Images: testImages})

	if pod.Name != "r-abc-0" {
		t.Errorf("pod name = %q, want r-abc-0", pod.Name)
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("restartPolicy = %q, want Never (operator owns recreation)", pod.Spec.RestartPolicy)
	}
	if pod.Spec.RuntimeClassName != nil {
		t.Errorf("runtimeClassName = %v, want nil for runc", *pod.Spec.RuntimeClassName)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("expected AutomountServiceAccountToken=false")
	}

	// Exactly one untrusted main container: the harness.
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Name != ContainerHarness {
		t.Fatalf("expected single harness main container, got %+v", pod.Spec.Containers)
	}
	harness := pod.Spec.Containers[0]
	if harness.Image != "reg/claude-code:1.0" {
		t.Errorf("harness image = %q", harness.Image)
	}
	if got := harness.Resources.Limits.Cpu().String(); got != "2" {
		t.Errorf("harness cpu limit = %q, want 2", got)
	}
	if harness.SecurityContext == nil || harness.SecurityContext.RunAsNonRoot == nil || !*harness.SecurityContext.RunAsNonRoot {
		t.Error("harness must run as non-root")
	}

	// Native sidecars + hydrate init, in order.
	wantInit := []string{ContainerEgressProxy, InitHydrate, ContainerCheckpointer, ContainerGateway}
	if len(pod.Spec.InitContainers) != len(wantInit) {
		t.Fatalf("initContainers = %d, want %d", len(pod.Spec.InitContainers), len(wantInit))
	}
	for i, name := range wantInit {
		if pod.Spec.InitContainers[i].Name != name {
			t.Errorf("initContainer[%d] = %q, want %q", i, pod.Spec.InitContainers[i].Name, name)
		}
	}

	// Sidecars must have Always restart policy; hydrate must not.
	for _, name := range []string{ContainerEgressProxy, ContainerCheckpointer, ContainerGateway} {
		c := containerByName(pod.Spec.InitContainers, name)
		if c.RestartPolicy == nil || *c.RestartPolicy != corev1.ContainerRestartPolicyAlways {
			t.Errorf("%s should be a native sidecar (RestartPolicy=Always)", name)
		}
	}
	if h := containerByName(pod.Spec.InitContainers, InitHydrate); h.RestartPolicy != nil {
		t.Error("hydrate should be a run-to-completion init container, not a sidecar")
	}

	// Workspace PVC is mounted into the harness.
	if vm := volumeMount(harness, VolumeWorkspace); vm == nil {
		t.Error("harness missing workspace mount")
	}

	// Sidecars/init run the wren-runtime image with their role as the argument.
	for _, name := range []string{ContainerEgressProxy, InitHydrate, ContainerCheckpointer, ContainerGateway} {
		c := containerByName(pod.Spec.InitContainers, name)
		if c.Image != "wren/runtime:test" {
			t.Errorf("%s image = %q, want runtime image", name, c.Image)
		}
		if len(c.Args) != 1 || c.Args[0] != name {
			t.Errorf("%s args = %v, want [%s]", name, c.Args, name)
		}
	}
	// The harness uses the per-project image (not the runtime image) and no arg
	// override (its entrypoint runs the harness role by default).
	if harness.Image != "reg/claude-code:1.0" || len(harness.Args) != 0 {
		t.Errorf("harness image/args = %q/%v", harness.Image, harness.Args)
	}
}

func TestBuildAgentPodRuntimeClass(t *testing.T) {
	run := testRun()
	run.Spec.Sandbox.RuntimeClass = wrenv1.RuntimeGVisor
	pod := buildAgentPod(run, PodConfig{Images: testImages})
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "gvisor" {
		t.Errorf("expected runtimeClassName=gvisor, got %v", pod.Spec.RuntimeClassName)
	}
}

func TestBuildAgentPodResumeName(t *testing.T) {
	run := testRun()
	run.Status.RestartCount = 2
	pod := buildAgentPod(run, PodConfig{Images: testImages})
	if pod.Name != "r-abc-2" {
		t.Errorf("resume pod name = %q, want r-abc-2", pod.Name)
	}
}

func TestBuildWorkspacePVC(t *testing.T) {
	pvc := buildWorkspacePVC(testRun())
	if pvc.Name != "r-abc-workspace" {
		t.Errorf("pvc name = %q", pvc.Name)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "regional-pd" {
		t.Errorf("pvc storageClass = %v", pvc.Spec.StorageClassName)
	}
	if got := pvc.Spec.Resources.Requests.Storage().String(); got != "20Gi" {
		t.Errorf("pvc size = %q, want 20Gi", got)
	}
}

func volumeMount(c corev1.Container, name string) *corev1.VolumeMount {
	for i := range c.VolumeMounts {
		if c.VolumeMounts[i].Name == name {
			return &c.VolumeMounts[i]
		}
	}
	return nil
}
