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

	// egress-lockdown (enforcement default=iptables) runs first, then native
	// sidecars + hydrate init, in order.
	wantInit := []string{InitEgressLockdown, ContainerEgressProxy, InitHydrate, ContainerCheckpointer, ContainerGateway}
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

// --- WS-1: egress enforcement flag matrix, UIDs, caps ---

func TestBuildAgentPod_EnforcementIptables_LockdownPresent(t *testing.T) {
	for _, mode := range []EgressEnforcement{"", EgressEnforcementIptables} {
		run := testRun()
		pod := buildAgentPod(run, PodConfig{Images: testImages, EgressEnforcement: mode})

		lock := containerByName(pod.Spec.InitContainers, InitEgressLockdown)
		if lock == nil {
			t.Fatalf("mode=%q: expected egress-lockdown init container", mode)
		}
		// It must be FIRST — iptables in place before anything touches the net.
		if pod.Spec.InitContainers[0].Name != InitEgressLockdown {
			t.Errorf("mode=%q: egress-lockdown must be the first init container, got %q", mode, pod.Spec.InitContainers[0].Name)
		}
		// Runs the lockdown role off the runtime image.
		if lock.Image != testImages.Runtime {
			t.Errorf("mode=%q: lockdown image = %q, want runtime image", mode, lock.Image)
		}
		if len(lock.Args) != 1 || lock.Args[0] != InitEgressLockdown {
			t.Errorf("mode=%q: lockdown args = %v, want [%s]", mode, lock.Args, InitEgressLockdown)
		}
		// Not a sidecar — it runs to completion.
		if lock.RestartPolicy != nil {
			t.Errorf("mode=%q: lockdown must run-to-completion, not be a sidecar", mode)
		}

		// Security context: root + NET_ADMIN/NET_RAW only, no priv-esc, seccomp.
		sc := lock.SecurityContext
		if sc == nil {
			t.Fatalf("mode=%q: lockdown missing securityContext", mode)
		}
		if sc.RunAsNonRoot == nil || *sc.RunAsNonRoot {
			t.Errorf("mode=%q: lockdown must run as root (RunAsNonRoot=false)", mode)
		}
		if sc.RunAsUser == nil || *sc.RunAsUser != 0 {
			t.Errorf("mode=%q: lockdown RunAsUser = %v, want 0", mode, sc.RunAsUser)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("mode=%q: lockdown must not allow privilege escalation", mode)
		}
		if sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("mode=%q: lockdown must keep the runtime-default seccomp profile", mode)
		}
		if sc.Capabilities == nil {
			t.Fatalf("mode=%q: lockdown missing capabilities", mode)
		}
		if len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
			t.Errorf("mode=%q: lockdown must Drop ALL, got %v", mode, sc.Capabilities.Drop)
		}
		wantAdd := map[corev1.Capability]bool{"NET_ADMIN": false, "NET_RAW": false}
		for _, c := range sc.Capabilities.Add {
			if _, ok := wantAdd[c]; !ok {
				t.Errorf("mode=%q: unexpected added capability %q", mode, c)
			}
			wantAdd[c] = true
		}
		for c, seen := range wantAdd {
			if !seen {
				t.Errorf("mode=%q: lockdown missing capability %q", mode, c)
			}
		}

		// Carries the proxy port + uid the iptables rules match on.
		if v, _ := envValue(lock, "WREN_PROXY_UID"); v.Value != "65533" {
			t.Errorf("mode=%q: lockdown WREN_PROXY_UID = %q, want 65533", mode, v.Value)
		}
		if v, _ := envValue(lock, "WREN_EGRESS_PORT"); v.Value == "" {
			t.Errorf("mode=%q: lockdown missing WREN_EGRESS_PORT", mode)
		}

		// Enforcement-on signals the harness canary via WREN_EXPECT_ENFORCEMENT.
		h := containerByName(pod.Spec.Containers, ContainerHarness)
		if v, ok := envValue(h, "WREN_EXPECT_ENFORCEMENT"); !ok || v.Value != "1" {
			t.Errorf("mode=%q: harness WREN_EXPECT_ENFORCEMENT = %q,%v, want 1,true", mode, v.Value, ok)
		}
	}
}

func TestBuildAgentPod_EnforcementOff_LockdownAbsent(t *testing.T) {
	run := testRun()
	pod := buildAgentPod(run, PodConfig{Images: testImages, EgressEnforcement: EgressEnforcementOff})

	if lock := containerByName(pod.Spec.InitContainers, InitEgressLockdown); lock != nil {
		t.Error("enforcement=off must omit the egress-lockdown init container")
	}
	// Original init order preserved (no lockdown prepended).
	wantInit := []string{ContainerEgressProxy, InitHydrate, ContainerCheckpointer, ContainerGateway}
	if len(pod.Spec.InitContainers) != len(wantInit) {
		t.Fatalf("enforcement=off initContainers = %d, want %d", len(pod.Spec.InitContainers), len(wantInit))
	}
	for i, name := range wantInit {
		if pod.Spec.InitContainers[i].Name != name {
			t.Errorf("enforcement=off initContainer[%d] = %q, want %q", i, pod.Spec.InitContainers[i].Name, name)
		}
	}
	// Canary must be skipped when there is no lockdown to prove.
	h := containerByName(pod.Spec.Containers, ContainerHarness)
	if _, ok := envValue(h, "WREN_EXPECT_ENFORCEMENT"); ok {
		t.Error("enforcement=off must not set WREN_EXPECT_ENFORCEMENT on the harness")
	}
}

func TestBuildAgentPod_ProxyUIDSeparation(t *testing.T) {
	run := testRun()
	pod := buildAgentPod(run, PodConfig{Images: testImages})

	proxy := containerByName(pod.Spec.InitContainers, ContainerEgressProxy)
	if proxy == nil || proxy.SecurityContext == nil {
		t.Fatal("missing egress-proxy or its securityContext")
	}
	if proxy.SecurityContext.RunAsUser == nil || *proxy.SecurityContext.RunAsUser != proxyUID {
		t.Errorf("egress-proxy RunAsUser = %v, want %d", proxy.SecurityContext.RunAsUser, proxyUID)
	}
	// The runner/harness must NOT be pinned to the proxy uid (it keeps the image
	// default). If they collided, the uid-match lockdown would not distinguish
	// them — the core of the boundary.
	h := containerByName(pod.Spec.Containers, ContainerHarness)
	if h.SecurityContext != nil && h.SecurityContext.RunAsUser != nil && *h.SecurityContext.RunAsUser == proxyUID {
		t.Error("harness must not share the egress-proxy uid (would break the lockdown boundary)")
	}
}
