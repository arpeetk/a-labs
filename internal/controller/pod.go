package controller

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
	"github.com/summiteight/wren/internal/egress"
	"github.com/summiteight/wren/internal/runspec"
)

// Component and volume identifiers used across pod, PVC, and ConfigMap.
const (
	ContainerHarness      = "harness"
	ContainerGateway      = "agent-gateway"
	ContainerCheckpointer = "checkpointer"
	ContainerEgressProxy  = "egress-proxy"
	InitEgressLockdown    = "egress-lockdown"
	InitHydrate           = "hydrate"

	// UIDs (spec §5.6). Every runner-side container is pinned to runnerUID; the
	// egress-proxy is pinned to proxyUID so the lockdown iptables rules can
	// uid-match it (accept the proxy's egress, reject the runner's). Never
	// collapse these two — the uid gap is the security boundary. The pin (not
	// the image's USER) is what makes it hold: Kubernetes overrides the image
	// USER with RunAsUser, so a project-supplied harness image cannot bake in
	// USER 65533 and inherit the proxy's iptables exemption.
	runnerUID int64 = 65532
	proxyUID  int64 = 65533

	VolumeWorkspace   = "workspace"
	VolumeIPC         = "ipc"
	VolumeRunSpec     = "runspec"
	VolumeMCP         = "mcp"
	VolumeTmp         = "tmp"
	VolumeHome        = "home"
	VolumeCheckpoints = "checkpoints"

	MountIPC         = "/var/run/wren"
	MountMCP         = "/etc/wren/mcp"
	MountTmp         = "/tmp"
	MountHome        = "/home/agent"
	MountCheckpoints = "/mnt/checkpoints"

	// gcsFuseCSIDriver is GKE's Cloud Storage FUSE CSI driver; it surfaces a GCS
	// bucket as a POSIX filesystem inside the container. gcsFuseVolumeAnnotation
	// is the pod annotation its sidecar-injection webhook requires — set only
	// when the mount is actually added (WS-18, spec §5.5).
	gcsFuseCSIDriver        = "gcsfuse.csi.storage.gke.io"
	gcsFuseVolumeAnnotation = "gke-gcsfuse/volumes"

	// GCS-FUSE checkpoint-mount egress under the default lockdown (WS-19). The
	// GKE-injected gke-gcsfuse-sidecar runs as gcsFuseSidecarUID — a uid GKE sets,
	// verified live on GKE 1.35 / CSI driver v1.22.16 to be 65534 (non-root),
	// distinct from runnerUID/proxyUID. Its traffic cannot be routed through the
	// egress-proxy: the injection webhook discards any env/args on a
	// user-declared sidecar (verified — HTTPS_PROXY is stripped), and gcsfuse
	// exposes no proxy mount option. So under the default iptables lockdown it
	// must reach Cloud Storage + the metadata server directly. To keep that hole
	// tight, the pod pins storage.googleapis.com to the restricted Google APIs
	// VIP (a fixed /30) via hostAliases — instead of its broad, rotating public
	// GFE ranges — and the lockdown allows this uid to reach ONLY that /30 and the
	// metadata /32 (see internal/podruntime/lockdown.go). If GKE ever changes the
	// sidecar uid, the exemption simply stops matching and the mount fails closed
	// (never opens the hole to the runner). Requires Private Google Access on the
	// node subnet so the VIP routes — a documented prerequisite (SETUP.md).
	gcsFuseSidecarUID     int64 = 65534
	gcsStorageHost              = "storage.googleapis.com"
	gcsRestrictedAPIsVIP        = "199.36.153.4"    // restricted.googleapis.com VIP
	gcsRestrictedAPIsCIDR       = "199.36.153.4/30" // the VIP's /30
	gcpMetadataHost             = "metadata.google.internal"
	gcpMetadataIP               = "169.254.169.254"
	gcpMetadataCIDR             = "169.254.169.254/32"

	// DefaultCheckpointKSA is the Kubernetes ServiceAccount used ONLY by pods
	// that have the GCS checkpoint mount enabled. It is annotated
	// iam.gke.io/gcp-service-account so the CSI sidecar authenticates to GCS via
	// Workload Identity. Pods without the mount keep the namespace "default" KSA
	// unchanged — no new identity, no behavior change (WS-18 item 4).
	DefaultCheckpointKSA = "wren-checkpointer"

	LabelRun       = "wren.dev/run"
	LabelComponent = "wren.dev/component"
	LabelPool      = "wren.dev/pool"

	componentAgent = "agent"
)

// Images holds the operator-injected image references. Runtime is the
// Wren-provided wren-runtime image used for every in-pod role except the
// harness (whose image comes from the AgentRun spec). The role is passed as the
// container's first argument; wren-runtime's entrypoint dispatches on it.
type Images struct {
	Runtime string
}

// EgressEnforcement selects how the runner is prevented from bypassing the
// egress-proxy (spec §5.6, WS-1).
type EgressEnforcement string

const (
	// EgressEnforcementIptables (default) injects a privileged egress-lockdown
	// init container that installs iptables OUTPUT rules restricting the runner
	// to the proxy. Requires a cluster that admits privileged init containers.
	EgressEnforcementIptables EgressEnforcement = "iptables"
	// EgressEnforcementOff omits the lockdown container — an escape hatch for
	// clusters that forbid privileged init containers (e.g. GKE Autopilot). The
	// operator records an EgressEnforcement=Disabled condition so the weaker
	// posture is visible on every run.
	EgressEnforcementOff EgressEnforcement = "off"
)

// PodConfig is the operator-level configuration applied to every agent pod.
type PodConfig struct {
	Images Images
	// GitHubTokenSecret / AnthropicKeySecret / OpenAIKeySecret are Secrets
	// (keys "token"/"key"/"key") injected into the egress-proxy container (not
	// the runner). Empty disables.
	GitHubTokenSecret  string
	AnthropicKeySecret string
	OpenAIKeySecret    string
	// EgressPort is the localhost port the egress-proxy listens on.
	EgressPort string
	// EgressEnforcement selects the bypass-prevention mechanism (default
	// iptables). Empty is treated as iptables.
	EgressEnforcement EgressEnforcement
	// CheckpointGCSMount enables mounting the run's checkpoint bucket into the
	// trusted checkpointer/hydrate containers via the GCS FUSE CSI driver
	// (WS-18–WS-21). Off by default; requires the CSI addon + a Workload Identity
	// binding. The mount is added only when this is true AND the run sets a
	// checkpoint bucket.
	CheckpointGCSMount bool
	// CheckpointLocalPath mounts an operator-administered node directory into
	// the trusted checkpointer/hydrate containers. It exists for kind and other
	// single-node development E2E only; unlike GCS it is not node-durable and
	// must never be used as a production recovery guarantee.
	CheckpointLocalPath string
	// CheckpointKSA is the Kubernetes ServiceAccount bound (via Workload
	// Identity) to a GCP SA with objectAdmin on the checkpoint bucket. Applied to
	// the pod ONLY when the GCS mount is added; empty falls back to
	// DefaultCheckpointKSA.
	CheckpointKSA string
}

func (c PodConfig) checkpointMountEnabled(run *wrenv1.AgentRun) bool {
	return run.Spec.Workspace.Checkpoint.Bucket != "" && (c.CheckpointGCSMount || c.CheckpointLocalPath != "")
}

// checkpointKSA is the ServiceAccount for GCS-mount pods, defaulting when unset.
func (c PodConfig) checkpointKSA() string {
	if c.CheckpointKSA != "" {
		return c.CheckpointKSA
	}
	return DefaultCheckpointKSA
}

// enforcementMode normalizes the configured mode, defaulting to iptables.
func (c PodConfig) enforcementMode() EgressEnforcement {
	if c.EgressEnforcement == EgressEnforcementOff {
		return EgressEnforcementOff
	}
	return EgressEnforcementIptables
}

func (c PodConfig) egressPort() string {
	if c.EgressPort != "" {
		return c.EgressPort
	}
	return egress.DefaultPort
}

func (c PodConfig) proxyBaseURL() string { return "http://127.0.0.1:" + c.egressPort() }

// secretEnv builds an optional Secret-sourced env var (optional so a missing
// Secret does not block the pod).
func secretEnv(envName, secretName, key string) []corev1.EnvVar {
	if secretName == "" {
		return nil
	}
	return []corev1.EnvVar{{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
				Optional:             ptr(true),
			},
		},
	}}
}

// pvcName is the stable workspace PVC name for a run. It is intentionally stable
// across restarts so a surviving PVC is reattached on resume.
func pvcName(run *wrenv1.AgentRun) string { return run.Name + "-workspace" }

// runSpecConfigMapName is the per-run RunSpec ConfigMap name.
func runSpecConfigMapName(run *wrenv1.AgentRun) string { return run.Name + "-runspec" }

// podName is the pod name for the current restart generation. It embeds the
// restart count so a recreated pod never collides with a terminating one.
func podName(run *wrenv1.AgentRun) string {
	return fmt.Sprintf("%s-%d", run.Name, attemptGeneration(run))
}

// attemptGeneration tolerates CRs created before status.attemptGeneration was
// introduced: their restart count was also their pod generation. New writes
// always advance AttemptGeneration explicitly and never reset it.
func attemptGeneration(run *wrenv1.AgentRun) int32 {
	if run.Status.AttemptGeneration > run.Status.RestartCount {
		return run.Status.AttemptGeneration
	}
	return run.Status.RestartCount
}

func advanceAttempt(run *wrenv1.AgentRun) {
	run.Status.AttemptGeneration = attemptGeneration(run) + 1
}

func runLabels(run *wrenv1.AgentRun) map[string]string {
	return map[string]string{
		LabelRun:       run.Name,
		LabelComponent: componentAgent,
	}
}

func ptr[T any](v T) *T { return &v }

// resources maps a ResourceSpec to a Kubernetes ResourceRequirements. Requests
// equal limits for CPU/memory (predictable scheduling); ephemeral disk, when
// set, is a limit only.
func resources(rs wrenv1.ResourceSpec) corev1.ResourceRequirements {
	req := corev1.ResourceList{
		corev1.ResourceCPU:    rs.CPU,
		corev1.ResourceMemory: rs.Memory,
	}
	lim := corev1.ResourceList{
		corev1.ResourceCPU:    rs.CPU,
		corev1.ResourceMemory: rs.Memory,
	}
	if !rs.EphemeralDisk.IsZero() {
		lim[corev1.ResourceEphemeralStorage] = rs.EphemeralDisk
	}
	return corev1.ResourceRequirements{Requests: req, Limits: lim}
}

// hardened returns the per-container security context applied to every
// container in the agent pod (spec §5.6, pod hardening). It pins the runner
// uid so the uid-match lockdown boundary holds by construction: no container
// image can choose the egress-proxy's uid via its own USER. The egress-proxy
// overrides the pin with proxyUID (see buildAgentPod).
func hardened(readOnlyRoot bool) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		ReadOnlyRootFilesystem:   ptr(readOnlyRoot),
		RunAsNonRoot:             ptr(true),
		RunAsUser:                ptr(runnerUID),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// lockdownSecurityContext is the security context for the egress-lockdown init
// container. It is the ONE privileged exception in the pod: it must run as root
// with NET_ADMIN+NET_RAW to program iptables in the pod's network namespace. It
// drops every other capability, keeps privilege-escalation off, and still uses
// the runtime-default seccomp profile — so the blast radius is exactly "can edit
// this pod's netfilter rules", nothing more. It runs to completion before any
// runner-side container starts (spec §5.6, WS-1).
func lockdownSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsNonRoot:             ptr(false),
		RunAsUser:                ptr(int64(0)),
		AllowPrivilegeEscalation: ptr(false),
		ReadOnlyRootFilesystem:   ptr(true),
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
			Add:  []corev1.Capability{"NET_ADMIN", "NET_RAW"},
		},
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// buildWorkspacePVC returns the desired workspace PersistentVolumeClaim.
func buildWorkspacePVC(run *wrenv1.AgentRun) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName(run),
			Namespace: run.Namespace,
			Labels:    runLabels(run),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: run.Spec.Workspace.PVC.Size},
			},
		},
	}
	if sc := run.Spec.Workspace.PVC.StorageClass; sc != "" {
		pvc.Spec.StorageClassName = ptr(sc)
	}
	return pvc
}

// harnessImage picks the harness container's image. The mock harness runs
// entirely inside wren-runtime (no CLI to install) — it never needed a
// distinct image, so it always uses the operator's own runtime image, the one
// already proven pullable in this pod (every sidecar just used it). Any other
// kind uses the image the control plane resolved onto the spec.
//
// This matters beyond mock: coreapi's HarnessImage default (a hardcoded
// "wren/claude-code:dev") only resolves to something real on a kind install,
// where that literal tag happens to be what got built and loaded locally. On
// a --registry (GKE) install nothing pushes that tag, so a project created
// with an explicit --harness mock but no --harness-image previously inherited
// that broken default and hit ImagePullBackOff — caught live on a real GKE
// cluster, not by unit tests (mock never has a reason to need an image at
// all, so route it around the default entirely rather than trying to make
// the default itself registry-aware).
func harnessImage(run *wrenv1.AgentRun, cfg PodConfig) string {
	if run.Spec.Harness.Kind == "mock" {
		return cfg.Images.Runtime
	}
	return run.Spec.Harness.Image
}

// runtimeClassName maps the spec runtime to a pod RuntimeClassName. The default
// runtime (runc / empty) leaves it nil so the node's default RuntimeClass runs;
// gvisor/kata set an explicit class (deferred, but wired through — spec §5.6).
func runtimeClassName(rc wrenv1.RuntimeClass) *string {
	switch rc {
	case wrenv1.RuntimeGVisor:
		return ptr("gvisor")
	case wrenv1.RuntimeKata:
		return ptr("kata")
	default:
		return nil
	}
}

// buildAgentPod assembles the agent pod for a run: a single untrusted harness
// container plus native-sidecar egress-proxy, checkpointer, and gateway, with a
// hydrate init container that clones the repo or restores a checkpoint.
func buildAgentPod(run *wrenv1.AgentRun, cfg PodConfig) *corev1.Pod {
	resume := attemptGeneration(run) > 0
	images := cfg.Images
	proxyBase := cfg.proxyBaseURL()
	// The runner routes GitHub/model traffic through the egress-proxy; it holds
	// no credentials of its own (spec §5.6). Every harness gets both model base
	// URLs; the adapter uses the one its provider speaks (claude-code/opencode →
	// Anthropic route, codex → OpenAI route).
	proxyEnv := []corev1.EnvVar{
		{Name: "WREN_EGRESS_PROXY", Value: proxyBase},
		{Name: "ANTHROPIC_BASE_URL", Value: proxyBase + strings.TrimSuffix(egress.RouteAnthropic, "/")},
		{Name: "OPENAI_BASE_URL", Value: proxyBase + strings.TrimSuffix(egress.RouteOpenAI, "/")},
	}

	workspaceMount := corev1.VolumeMount{Name: VolumeWorkspace, MountPath: runspec.WorkspacePath}
	ipcMount := corev1.VolumeMount{Name: VolumeIPC, MountPath: MountIPC}
	runSpecMount := corev1.VolumeMount{Name: VolumeRunSpec, MountPath: runspec.MountPath, ReadOnly: true}

	// Native sidecars are init containers with an Always restart policy: they
	// start (in order) before the main container and run alongside it without
	// blocking pod completion when the harness exits.
	sidecar := corev1.ContainerRestartPolicyAlways

	runSpecEnv := corev1.EnvVar{Name: "WREN_RUNSPEC", Value: runspec.MountPath + "/" + runspec.FileName}

	egressProxyEnv := []corev1.EnvVar{
		{Name: "WREN_RUN_ID", Value: run.Name},
		{Name: "WREN_EGRESS_PORT", Value: cfg.egressPort()},
		{Name: "WREN_EGRESS_ALLOWLIST", Value: joinAllowlist(run.Spec.Egress.Allowlist)},
	}
	// Credentials live here — on the trusted proxy, never the runner.
	egressProxyEnv = append(egressProxyEnv, secretEnv("GITHUB_TOKEN", cfg.GitHubTokenSecret, "token")...)
	egressProxyEnv = append(egressProxyEnv, secretEnv("ANTHROPIC_API_KEY", cfg.AnthropicKeySecret, "key")...)
	egressProxyEnv = append(egressProxyEnv, secretEnv("OPENAI_API_KEY", cfg.OpenAIKeySecret, "key")...)

	// The egress-proxy runs as a distinct uid (proxyUID) so the lockdown iptables
	// rules can uid-match it: override the runner-uid pin from hardened().
	proxySecCtx := hardened(true)
	proxySecCtx.RunAsUser = ptr(proxyUID)
	egressProxy := corev1.Container{
		Name:            ContainerEgressProxy,
		Image:           images.Runtime,
		Args:            []string{ContainerEgressProxy},
		RestartPolicy:   &sidecar,
		SecurityContext: proxySecCtx,
		Env:             egressProxyEnv,
	}

	hydrate := corev1.Container{
		Name:            InitHydrate,
		Image:           images.Runtime,
		Args:            []string{InitHydrate},
		SecurityContext: hardened(true),
		Env: append([]corev1.EnvVar{
			runSpecEnv,
			{Name: "WREN_MODE", Value: string(mode(resume))},
			{Name: "WREN_BASE_REF", Value: run.Spec.Task.BaseRef},
			{Name: "WREN_CHECKPOINT_BUCKET", Value: run.Spec.Workspace.Checkpoint.Bucket},
		}, proxyEnv...),
		VolumeMounts: []corev1.VolumeMount{workspaceMount, runSpecMount},
	}

	checkpointer := corev1.Container{
		Name:            ContainerCheckpointer,
		Image:           images.Runtime,
		Args:            []string{ContainerCheckpointer},
		RestartPolicy:   &sidecar,
		SecurityContext: hardened(true),
		Env: []corev1.EnvVar{
			{Name: "WREN_RUN_ID", Value: run.Name},
			{Name: "WREN_CHECKPOINT_BUCKET", Value: run.Spec.Workspace.Checkpoint.Bucket},
			{Name: "WREN_CHECKPOINT_INTERVAL", Value: fmt.Sprintf("%d", checkpointInterval(run))},
		},
		VolumeMounts: []corev1.VolumeMount{{Name: VolumeWorkspace, MountPath: runspec.WorkspacePath, ReadOnly: true}},
	}

	gateway := corev1.Container{
		Name:            ContainerGateway,
		Image:           images.Runtime,
		Args:            []string{ContainerGateway},
		RestartPolicy:   &sidecar,
		SecurityContext: hardened(true),
		Env: []corev1.EnvVar{
			{Name: "WREN_RUN_ID", Value: run.Name},
			{Name: "WREN_INTERACTIVE", Value: fmt.Sprintf("%t", run.Spec.Interactive)},
		},
		VolumeMounts: []corev1.VolumeMount{ipcMount},
	}

	harnessEnv := append([]corev1.EnvVar{
		{Name: "WREN_RUN_ID", Value: run.Name},
		{Name: "WREN_MODE", Value: string(mode(resume))},
		runSpecEnv,
		{Name: "HOME", Value: MountHome},
		// Hydrate and the untrusted harness intentionally use different UIDs.
		// Git otherwise rejects the cloned repository as "dubious ownership",
		// breaking ordinary agent inspection. Trust exactly the pod's workspace
		// (not "*") through Git's environment-backed config, which is ephemeral
		// and cannot be committed into the target repository.
		{Name: "GIT_CONFIG_COUNT", Value: "1"},
		{Name: "GIT_CONFIG_KEY_0", Value: "safe.directory"},
		{Name: "GIT_CONFIG_VALUE_0", Value: runspec.WorkspacePath},
	}, proxyEnv...)
	if run.Spec.Harness.Kind == wrenv1.HarnessCodex {
		// Codex stores resumable sessions under CODEX_HOME. Keep them inside
		// .git on the durable workspace PVC: checkpoints include .git, while
		// finalization can never stage this private execution state into the PR.
		harnessEnv = append(harnessEnv, corev1.EnvVar{
			Name: "CODEX_HOME", Value: runspec.CodexHomePath(runspec.WorkspacePath, run.Spec.Repo),
		})
	}
	// When enforcement is on, tell the harness to run its egress canary (a direct
	// dial/HTTPS attempt that MUST fail). Off mode omits the flag so the canary
	// is skipped — there is no lockdown to prove.
	if cfg.enforcementMode() == EgressEnforcementIptables {
		harnessEnv = append(harnessEnv, corev1.EnvVar{Name: "WREN_EXPECT_ENFORCEMENT", Value: "1"})
	}

	harness := corev1.Container{
		Name:            ContainerHarness,
		Image:           harnessImage(run, cfg),
		SecurityContext: hardened(true),
		Resources:       resources(run.Spec.Sandbox.Resources),
		Env:             harnessEnv,
		VolumeMounts: []corev1.VolumeMount{
			workspaceMount,
			ipcMount,
			runSpecMount,
			{Name: VolumeTmp, MountPath: MountTmp},
			{Name: VolumeHome, MountPath: MountHome},
		},
	}

	volumes := []corev1.Volume{
		{Name: VolumeWorkspace, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: pvcName(run)},
		}},
		{Name: VolumeIPC, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: VolumeTmp, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: VolumeHome, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: VolumeRunSpec, VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: runSpecConfigMapName(run)},
			},
		}},
	}

	// Attach the rendered MCP config secret if the run references one.
	if ref := run.Spec.MCP.ConfigRef; ref != "" {
		volumes = append(volumes, corev1.Volume{Name: VolumeMCP, VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{SecretName: ref},
		}})
		mcpMount := corev1.VolumeMount{Name: VolumeMCP, MountPath: MountMCP, ReadOnly: true}
		harness.VolumeMounts = append(harness.VolumeMounts, mcpMount)
	}

	var podAnnotations map[string]string
	var saName string
	var hostAliases []corev1.HostAlias
	gcsMount := cfg.CheckpointGCSMount && run.Spec.Workspace.Checkpoint.Bucket != ""
	localCheckpointMount := !gcsMount && cfg.CheckpointLocalPath != "" && run.Spec.Workspace.Checkpoint.Bucket != ""
	// GCS checkpoint mount (WS-18): a CSI volume backed by the run's checkpoint
	// bucket, mounted into the checkpointer container ONLY. The checkpointer is a
	// trusted native sidecar; the harness runs untrusted model-generated code and
	// must never hold a credential or a writable path to durable storage outside
	// its own workspace PVC. This is the same trust-tier reasoning as the
	// egress-proxy/runner uid split (code standards rule #1) — the invariant is
	// pinned here in code and asserted by TestBuildAgentPod_GCSMount_HarnessNever*.
	if gcsMount {
		volumes = append(volumes, corev1.Volume{
			Name: VolumeCheckpoints,
			VolumeSource: corev1.VolumeSource{
				CSI: &corev1.CSIVolumeSource{
					Driver:           gcsFuseCSIDriver,
					VolumeAttributes: map[string]string{"bucketName": gcsBucketName(run.Spec.Workspace.Checkpoint.Bucket)},
				},
			},
		})
		// Mount into the checkpointer sidecar (read-write, so it can Put new
		// snapshots) and hydrate (read-only — it only ever restores FROM the
		// store, never writes to it, least privilege even though it's already a
		// trusted init container). NEVER the harness. (checkpointer/hydrate are
		// passed by value into initContainers below, so mutate them here first.)
		checkpointer.VolumeMounts = append(checkpointer.VolumeMounts,
			corev1.VolumeMount{Name: VolumeCheckpoints, MountPath: MountCheckpoints})
		checkpointer.Env = append(checkpointer.Env,
			corev1.EnvVar{Name: "WREN_CHECKPOINT_MOUNT_PATH", Value: MountCheckpoints})
		hydrate.VolumeMounts = append(hydrate.VolumeMounts,
			corev1.VolumeMount{Name: VolumeCheckpoints, MountPath: MountCheckpoints, ReadOnly: true})
		hydrate.Env = append(hydrate.Env,
			corev1.EnvVar{Name: "WREN_CHECKPOINT_MOUNT_PATH", Value: MountCheckpoints})
		// The CSI sidecar-injection webhook only runs when this annotation is present.
		podAnnotations = map[string]string{gcsFuseVolumeAnnotation: "true"}
		// The mount's GCS credentials come from Workload Identity bound to this
		// dedicated KSA — applied only here, so pods without the mount keep the
		// namespace default KSA untouched (WS-18 item 4).
		saName = cfg.checkpointKSA()
		// Pin storage.googleapis.com to the restricted Google APIs VIP (a fixed
		// /30) and metadata.google.internal to the GCE metadata IP (WS-19). This
		// lets the lockdown's gcs-fuse exemption be destination-scoped to two small
		// stable CIDRs instead of Storage's broad, rotating public ranges, and lets
		// the credential fetch resolve with no DNS under lockdown. hostAliases
		// applies to every container's /etc/hosts, but only the gcs-fuse sidecar
		// (and only its uid) is granted egress to these destinations, so the
		// untrusted harness gains nothing. See the gcsFuseSidecarUID comment.
		hostAliases = []corev1.HostAlias{
			{IP: gcsRestrictedAPIsVIP, Hostnames: []string{gcsStorageHost}},
			{IP: gcpMetadataIP, Hostnames: []string{gcpMetadataHost}},
		}
	}
	if localCheckpointMount {
		hostPathType := corev1.HostPathDirectory
		volumes = append(volumes, corev1.Volume{
			Name: VolumeCheckpoints,
			VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{
				Path: cfg.CheckpointLocalPath, Type: &hostPathType,
			}},
		})
		checkpointer.VolumeMounts = append(checkpointer.VolumeMounts,
			corev1.VolumeMount{Name: VolumeCheckpoints, MountPath: MountCheckpoints})
		checkpointer.Env = append(checkpointer.Env,
			corev1.EnvVar{Name: "WREN_CHECKPOINT_MOUNT_PATH", Value: MountCheckpoints})
		hydrate.VolumeMounts = append(hydrate.VolumeMounts,
			corev1.VolumeMount{Name: VolumeCheckpoints, MountPath: MountCheckpoints, ReadOnly: true})
		hydrate.Env = append(hydrate.Env,
			corev1.EnvVar{Name: "WREN_CHECKPOINT_MOUNT_PATH", Value: MountCheckpoints})
	}

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:        podName(run),
			Namespace:   run.Namespace,
			Labels:      runLabels(run),
			Annotations: podAnnotations,
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever, // operator owns re-creation
			RuntimeClassName:             runtimeClassName(run.Spec.Sandbox.RuntimeClass),
			ServiceAccountName:           saName,
			AutomountServiceAccountToken: ptr(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr(true),
				FSGroup:        ptr(int64(10001)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			// Order matters: egress-lockdown runs FIRST (to completion) so iptables
			// is in place before anything else touches the network; then the
			// egress-proxy sidecar comes up; then hydrate (which needs egress to
			// clone); then the remaining sidecars; then the harness.
			InitContainers: initContainers(cfg, gcsMount, egressProxy, hydrate, checkpointer, gateway),
			Containers:     []corev1.Container{harness},
			Volumes:        volumes,
			HostAliases:    hostAliases,
		},
	}
}

// initContainers assembles the pod's init-container list, prepending the
// privileged egress-lockdown container when enforcement is on. With enforcement
// off the lockdown container is omitted entirely (the escape hatch); the run's
// EgressEnforcement=Disabled condition then records the weaker posture.
func initContainers(cfg PodConfig, gcsMount bool, egressProxy, hydrate, checkpointer, gateway corev1.Container) []corev1.Container {
	rest := []corev1.Container{egressProxy, hydrate, checkpointer, gateway}
	if cfg.enforcementMode() == EgressEnforcementOff {
		return rest
	}
	return append([]corev1.Container{buildLockdownContainer(cfg, gcsMount)}, rest...)
}

// buildLockdownContainer builds the egress-lockdown init container: the
// wren-runtime image invoked with the egress-lockdown role, running privileged
// enough to program iptables and nothing more. It carries the proxy port and
// uid the iptables rules match on.
func buildLockdownContainer(cfg PodConfig, gcsMount bool) corev1.Container {
	env := []corev1.EnvVar{
		{Name: "WREN_EGRESS_PORT", Value: cfg.egressPort()},
		{Name: "WREN_PROXY_UID", Value: fmt.Sprintf("%d", proxyUID)},
	}
	if gcsMount {
		// Narrow gcs-fuse exemption (WS-19): only the sidecar's uid, only the
		// restricted Google APIs VIP (where storage.googleapis.com is pinned) and
		// the metadata server. The runner uid can never match it. See the
		// gcsFuseSidecarUID comment and internal/podruntime/lockdown.go.
		env = append(env,
			corev1.EnvVar{Name: "WREN_GCSFUSE_UID", Value: fmt.Sprintf("%d", gcsFuseSidecarUID)},
			corev1.EnvVar{Name: "WREN_GCSFUSE_DST_CIDRS", Value: gcsRestrictedAPIsCIDR + "," + gcpMetadataCIDR},
		)
	}
	return corev1.Container{
		Name:            InitEgressLockdown,
		Image:           cfg.Images.Runtime,
		Args:            []string{InitEgressLockdown},
		SecurityContext: lockdownSecurityContext(),
		Env:             env,
	}
}

func mode(resume bool) runspec.Mode {
	if resume {
		return runspec.ModeResume
	}
	return runspec.ModeStart
}

func checkpointInterval(run *wrenv1.AgentRun) int32 {
	if iv := run.Spec.Workspace.Checkpoint.IntervalSeconds; iv > 0 {
		return iv
	}
	return defaultCheckpointInterval
}

// gcsBucketName extracts the bare bucket name the CSI driver's bucketName
// attribute wants from a checkpoint-bucket value. It strips the "gs://" scheme
// and keeps only the first path segment (the bucket); any prefix path within
// the bucket is handled by the Store's per-run prefix, not the mount.
func gcsBucketName(bucket string) string {
	b := strings.TrimPrefix(bucket, "gs://")
	if i := strings.IndexByte(b, '/'); i >= 0 {
		b = b[:i]
	}
	return b
}

func joinAllowlist(list []string) string {
	out := ""
	for i, d := range list {
		if i > 0 {
			out += ","
		}
		out += d
	}
	return out
}
