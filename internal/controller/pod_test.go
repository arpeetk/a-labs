package controller

import (
	"fmt"
	"strings"
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

// TestBuildAgentPodMockHarnessUsesRuntimeImage pins the fix for a bug found
// live on real GKE: coreapi's HarnessImage default only resolves to something
// pullable on a kind install (where that literal tag happens to be loaded
// locally); on a --registry install a mock-harness run with no explicit
// --harness-image inherited that default and hit ImagePullBackOff. mock needs
// no CLI at all — it must always run the operator's own runtime image (the one
// every sidecar in this same pod already pulled successfully), regardless of
// whatever (possibly bogus, for this run) image landed in spec.Harness.Image.
func TestBuildAgentPodMockHarnessUsesRuntimeImage(t *testing.T) {
	run := testRun()
	run.Spec.Harness = wrenv1.HarnessSpec{Kind: "mock", Image: "wren/claude-code:dev"}
	pod := buildAgentPod(run, PodConfig{Images: testImages})
	harness := containerByName(pod.Spec.Containers, ContainerHarness)
	if harness.Image != testImages.Runtime {
		t.Errorf("mock harness image = %q, want the runtime image %q (ignoring spec.Harness.Image=%q)",
			harness.Image, testImages.Runtime, run.Spec.Harness.Image)
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
	// Every non-proxy container must be pinned to the runner uid. The pin — not
	// the image's USER — is the boundary: a harness image that baked in
	// USER 65533 would otherwise inherit the proxy's iptables exemption, and
	// Kubernetes overrides the image USER with RunAsUser. (The lockdown init
	// container is the one root exception; it programs the rules and exits.)
	uidOf := func(c *corev1.Container) int64 {
		if c.SecurityContext == nil || c.SecurityContext.RunAsUser == nil {
			return -1
		}
		return *c.SecurityContext.RunAsUser
	}
	for i := range pod.Spec.InitContainers {
		c := &pod.Spec.InitContainers[i]
		if c.Name == ContainerEgressProxy || c.Name == InitEgressLockdown {
			continue
		}
		if got := uidOf(c); got != runnerUID {
			t.Errorf("init container %q RunAsUser = %d, want pinned runner uid %d", c.Name, got, runnerUID)
		}
	}
	h := containerByName(pod.Spec.Containers, ContainerHarness)
	if got := uidOf(h); got != runnerUID {
		t.Errorf("harness RunAsUser = %d, want pinned runner uid %d (an unpinned harness image could set USER %d and bypass the lockdown)", got, runnerUID, proxyUID)
	}
}

// --- WS-18: GCS checkpoint mount ---

func podVolume(pod *corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

// TestBuildAgentPod_GCSMount_DisabledByDefault: with the flag off (the default),
// nothing about the pod changes — no CSI volume, no annotation, no ServiceAccount
// override, and the checkpointer has no checkpoints mount. Runs on clusters
// without the feature are completely unaffected.
func TestBuildAgentPod_GCSMount_DisabledByDefault(t *testing.T) {
	run := testRun() // testRun already sets a checkpoint bucket
	pod := buildAgentPod(run, PodConfig{Images: testImages})

	if v := podVolume(pod, VolumeCheckpoints); v != nil {
		t.Error("checkpoints CSI volume present with the flag off; want absent")
	}
	if pod.Annotations[gcsFuseVolumeAnnotation] != "" {
		t.Errorf("gcsfuse annotation set with the flag off: %q", pod.Annotations[gcsFuseVolumeAnnotation])
	}
	if pod.Spec.ServiceAccountName != "" {
		t.Errorf("ServiceAccountName = %q with the flag off; want empty (namespace default)", pod.Spec.ServiceAccountName)
	}
	ck := containerByName(pod.Spec.InitContainers, ContainerCheckpointer)
	if volumeMount(*ck, VolumeCheckpoints) != nil {
		t.Error("checkpointer has the checkpoints mount with the flag off")
	}
}

// TestBuildAgentPod_GCSMount_Enabled: flag on + a bucket set adds the CSI volume
// (correct driver + bare bucket name), mounts it into the checkpointer only,
// sets the sidecar-injection annotation, the mount-path env, and the dedicated
// ServiceAccount.
func TestBuildAgentPod_GCSMount_Enabled(t *testing.T) {
	run := testRun()
	run.Spec.Workspace.Checkpoint.Bucket = "gs://wren-ckpt-bucket/some/prefix"
	pod := buildAgentPod(run, PodConfig{Images: testImages, CheckpointGCSMount: true})

	v := podVolume(pod, VolumeCheckpoints)
	if v == nil || v.CSI == nil {
		t.Fatal("expected a CSI checkpoints volume")
	}
	if v.CSI.Driver != gcsFuseCSIDriver {
		t.Errorf("CSI driver = %q, want %q", v.CSI.Driver, gcsFuseCSIDriver)
	}
	// bucketName must be the bare bucket, gs:// stripped and any path dropped.
	if got := v.CSI.VolumeAttributes["bucketName"]; got != "wren-ckpt-bucket" {
		t.Errorf("bucketName = %q, want %q", got, "wren-ckpt-bucket")
	}
	if pod.Annotations[gcsFuseVolumeAnnotation] != "true" {
		t.Errorf("gcsfuse annotation = %q, want \"true\"", pod.Annotations[gcsFuseVolumeAnnotation])
	}
	if pod.Spec.ServiceAccountName != DefaultCheckpointKSA {
		t.Errorf("ServiceAccountName = %q, want %q", pod.Spec.ServiceAccountName, DefaultCheckpointKSA)
	}
	// Automounting the SA token stays off even with the mount + a KSA set; the
	// CSI sidecar's Workload-Identity auth does not depend on it (WS-18 finding).
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Error("AutomountServiceAccountToken must remain false with the GCS mount enabled")
	}

	ck := containerByName(pod.Spec.InitContainers, ContainerCheckpointer)
	vm := volumeMount(*ck, VolumeCheckpoints)
	if vm == nil {
		t.Fatal("checkpointer missing the checkpoints mount")
	}
	if vm.MountPath != MountCheckpoints {
		t.Errorf("checkpointer mount path = %q, want %q", vm.MountPath, MountCheckpoints)
	}
	if !hasEnv(ck.Env, "WREN_CHECKPOINT_MOUNT_PATH", MountCheckpoints) {
		t.Errorf("checkpointer missing WREN_CHECKPOINT_MOUNT_PATH=%s env", MountCheckpoints)
	}
}

// TestBuildAgentPod_GCSMount_CustomKSA confirms the operator flag overrides the
// default KSA name.
func TestBuildAgentPod_GCSMount_CustomKSA(t *testing.T) {
	run := testRun()
	pod := buildAgentPod(run, PodConfig{Images: testImages, CheckpointGCSMount: true, CheckpointKSA: "custom-ksa"})
	if pod.Spec.ServiceAccountName != "custom-ksa" {
		t.Errorf("ServiceAccountName = %q, want custom-ksa", pod.Spec.ServiceAccountName)
	}
}

// TestBuildAgentPod_GCSMount_NoBucket: the flag on but no bucket set adds
// nothing — the mount requires both.
func TestBuildAgentPod_GCSMount_NoBucket(t *testing.T) {
	run := testRun()
	run.Spec.Workspace.Checkpoint.Bucket = ""
	pod := buildAgentPod(run, PodConfig{Images: testImages, CheckpointGCSMount: true})
	if v := podVolume(pod, VolumeCheckpoints); v != nil {
		t.Error("checkpoints volume added despite an empty bucket")
	}
	if pod.Spec.ServiceAccountName != "" {
		t.Errorf("ServiceAccountName = %q with no bucket; want empty", pod.Spec.ServiceAccountName)
	}
}

// TestBuildAgentPod_GCSMount_HarnessNeverMounts is the load-bearing invariant
// test (code standards rule #1, WS-18 item 3): the untrusted harness container
// must NEVER see the GCS checkpoint volume — neither its VolumeMounts nor any
// mount at the checkpoints path — whether the feature is enabled or disabled.
// It mirrors TestBuildAgentPod_ProxyUIDSeparation: pin the boundary in code, and
// have a test assert the pin. The checkpointer holds the bucket credential and a
// writable path to durable storage; the harness runs model-generated code and
// must not.
func TestBuildAgentPod_GCSMount_HarnessNeverMounts(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		run := testRun()
		pod := buildAgentPod(run, PodConfig{Images: testImages, CheckpointGCSMount: enabled})
		h := containerByName(pod.Spec.Containers, ContainerHarness)
		if h == nil {
			t.Fatalf("enabled=%v: no harness container", enabled)
		}
		for _, vm := range h.VolumeMounts {
			if vm.Name == VolumeCheckpoints {
				t.Errorf("enabled=%v: harness has the checkpoints volume mount %q — SECURITY BOUNDARY VIOLATION", enabled, vm.Name)
			}
			if vm.MountPath == MountCheckpoints {
				t.Errorf("enabled=%v: harness mounts something at the checkpoints path %q", enabled, MountCheckpoints)
			}
		}
	}
}

func hasEnv(env []corev1.EnvVar, name, val string) bool {
	for _, e := range env {
		if e.Name == name && e.Value == val {
			return true
		}
	}
	return false
}

func envVal(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}

func hostAliasIP(pod *corev1.Pod, host string) string {
	for _, ha := range pod.Spec.HostAliases {
		for _, h := range ha.Hostnames {
			if h == host {
				return ha.IP
			}
		}
	}
	return ""
}

// TestBuildAgentPod_GCSMount_EgressExemption is the WS-19 wiring test: with the
// mount enabled under the default iptables lockdown, the egress-lockdown init
// container is told to carve a narrow hole for the gcs-fuse sidecar — scoped to
// its OWN uid (never the runner's) AND a fixed destination set — and the pod
// pins Storage/metadata hostnames onto those destinations via hostAliases so the
// hole stays tight to the restricted-APIs VIP rather than Storage's broad
// public ranges.
func TestBuildAgentPod_GCSMount_EgressExemption(t *testing.T) {
	run := testRun()
	run.Spec.Workspace.Checkpoint.Bucket = "gs://wren-ckpt-bucket"
	pod := buildAgentPod(run, PodConfig{Images: testImages, CheckpointGCSMount: true})

	lk := containerByName(pod.Spec.InitContainers, InitEgressLockdown)
	if lk == nil {
		t.Fatal("expected an egress-lockdown init container under default enforcement")
	}
	uid, ok := envVal(lk.Env, "WREN_GCSFUSE_UID")
	if !ok || uid != fmt.Sprintf("%d", gcsFuseSidecarUID) {
		t.Errorf("WREN_GCSFUSE_UID = %q (present=%v), want %d", uid, ok, gcsFuseSidecarUID)
	}
	// The exemption uid must NOT be the runner's — that is the whole boundary.
	if uid == fmt.Sprintf("%d", runnerUID) {
		t.Fatal("gcs-fuse exemption uid equals the runner uid — SECURITY BOUNDARY COLLAPSE")
	}
	cidrs, ok := envVal(lk.Env, "WREN_GCSFUSE_DST_CIDRS")
	if !ok {
		t.Fatal("WREN_GCSFUSE_DST_CIDRS not set")
	}
	for _, want := range []string{gcsRestrictedAPIsCIDR, gcpMetadataCIDR} {
		if !strings.Contains(cidrs, want) {
			t.Errorf("WREN_GCSFUSE_DST_CIDRS = %q, missing %q", cidrs, want)
		}
	}

	// hostAliases pin Storage to the restricted VIP and metadata to its fixed IP.
	if got := hostAliasIP(pod, gcsStorageHost); got != gcsRestrictedAPIsVIP {
		t.Errorf("hostAlias %s = %q, want %q", gcsStorageHost, got, gcsRestrictedAPIsVIP)
	}
	if got := hostAliasIP(pod, gcpMetadataHost); got != gcpMetadataIP {
		t.Errorf("hostAlias %s = %q, want %q", gcpMetadataHost, got, gcpMetadataIP)
	}
}

// TestBuildAgentPod_GCSMount_NoExemptionWhenOff: without the mount, the lockdown
// carries no gcs-fuse exemption and the pod sets no hostAliases — the WS-19
// change is inert for every run that does not use the feature.
func TestBuildAgentPod_GCSMount_NoExemptionWhenOff(t *testing.T) {
	// Flag on but no bucket, and flag off entirely: both must stay inert.
	cases := []PodConfig{
		{Images: testImages, CheckpointGCSMount: false},
		{Images: testImages, CheckpointGCSMount: true}, // no bucket set on the run
	}
	for i, cfg := range cases {
		run := testRun()
		if i == 0 {
			run.Spec.Workspace.Checkpoint.Bucket = "gs://wren-ckpt" // present, but feature off
		} else {
			run.Spec.Workspace.Checkpoint.Bucket = ""
		}
		pod := buildAgentPod(run, cfg)
		if lk := containerByName(pod.Spec.InitContainers, InitEgressLockdown); lk != nil {
			if _, ok := envVal(lk.Env, "WREN_GCSFUSE_UID"); ok {
				t.Errorf("case %d: lockdown carries a gcs-fuse exemption with the mount inactive", i)
			}
		}
		if len(pod.Spec.HostAliases) != 0 {
			t.Errorf("case %d: pod sets hostAliases with the mount inactive: %+v", i, pod.Spec.HostAliases)
		}
	}
}
