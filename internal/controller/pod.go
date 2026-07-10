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
	InitHydrate           = "hydrate"

	VolumeWorkspace = "workspace"
	VolumeIPC       = "ipc"
	VolumeRunSpec   = "runspec"
	VolumeMCP       = "mcp"
	VolumeTmp       = "tmp"
	VolumeHome      = "home"

	MountIPC  = "/var/run/wren"
	MountMCP  = "/etc/wren/mcp"
	MountTmp  = "/tmp"
	MountHome = "/home/agent"

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

// PodConfig is the operator-level configuration applied to every agent pod.
type PodConfig struct {
	Images Images
	// GitHubTokenSecret / AnthropicKeySecret are Secrets (keys "token"/"key")
	// injected into the egress-proxy container (not the runner). Empty disables.
	GitHubTokenSecret  string
	AnthropicKeySecret string
	// EgressPort is the localhost port the egress-proxy listens on.
	EgressPort string
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
	return fmt.Sprintf("%s-%d", run.Name, run.Status.RestartCount)
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
// container in the agent pod (spec §5.6, pod hardening).
func hardened(readOnlyRoot bool) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr(false),
		ReadOnlyRootFilesystem:   ptr(readOnlyRoot),
		RunAsNonRoot:             ptr(true),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
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
	resume := run.Status.RestartCount > 0
	images := cfg.Images
	proxyBase := cfg.proxyBaseURL()
	// The runner routes GitHub/model traffic through the egress-proxy; it holds
	// no credentials of its own (spec §5.6).
	proxyEnv := []corev1.EnvVar{
		{Name: "WREN_EGRESS_PROXY", Value: proxyBase},
		{Name: "ANTHROPIC_BASE_URL", Value: proxyBase + strings.TrimSuffix(egress.RouteAnthropic, "/")},
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

	egressProxy := corev1.Container{
		Name:            ContainerEgressProxy,
		Image:           images.Runtime,
		Args:            []string{ContainerEgressProxy},
		RestartPolicy:   &sidecar,
		SecurityContext: hardened(true),
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

	harness := corev1.Container{
		Name:            ContainerHarness,
		Image:           run.Spec.Harness.Image,
		SecurityContext: hardened(true),
		Resources:       resources(run.Spec.Sandbox.Resources),
		Env: append([]corev1.EnvVar{
			{Name: "WREN_RUN_ID", Value: run.Name},
			{Name: "WREN_MODE", Value: string(mode(resume))},
			runSpecEnv,
			{Name: "HOME", Value: MountHome},
		}, proxyEnv...),
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

	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName(run),
			Namespace: run.Namespace,
			Labels:    runLabels(run),
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever, // operator owns re-creation
			RuntimeClassName:             runtimeClassName(run.Spec.Sandbox.RuntimeClass),
			AutomountServiceAccountToken: ptr(false),
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot:   ptr(true),
				FSGroup:        ptr(int64(10001)),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
			},
			// Order matters: egress-proxy up first, then hydrate (which needs
			// egress to clone), then the remaining sidecars, then the harness.
			InitContainers: []corev1.Container{egressProxy, hydrate, checkpointer, gateway},
			Containers:     []corev1.Container{harness},
			Volumes:        volumes,
		},
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
