package v1alpha1

import (
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HarnessKind identifies which agent harness runs the task.
type HarnessKind string

const (
	HarnessClaudeCode HarnessKind = "claude-code"
	HarnessCodex      HarnessKind = "codex"
	HarnessBYO        HarnessKind = "byo"
)

// RuntimeClass selects the pod sandbox runtime. v1 defaults to runc; gVisor and
// Kata are pluggable and land in a later milestone (see the technical spec §5.6).
type RuntimeClass string

const (
	RuntimeRunc   RuntimeClass = "runc"
	RuntimeGVisor RuntimeClass = "gvisor"
	RuntimeKata   RuntimeClass = "kata"
)

// RunPhase is the lifecycle phase of an AgentRun.
type RunPhase string

const (
	PhasePending      RunPhase = "Pending"
	PhaseProvisioning RunPhase = "Provisioning"
	PhaseCloning      RunPhase = "Cloning"
	PhaseRunning      RunPhase = "Running"
	PhaseInterrupted  RunPhase = "Interrupted" // transient: operator will resume
	PhaseFinalizing   RunPhase = "Finalizing"
	PhaseSucceeded    RunPhase = "Succeeded"
	PhaseFailed       RunPhase = "Failed"
	PhaseCanceled     RunPhase = "Canceled"
)

// HarnessSpec describes the agent harness to run.
type HarnessSpec struct {
	Kind  HarnessKind `json:"kind"`
	Image string      `json:"image"`
	Model string      `json:"model,omitempty"`
}

// TaskSpec is the work the agent is asked to do.
type TaskSpec struct {
	Prompt  string `json:"prompt"`
	BaseRef string `json:"baseRef,omitempty"`
}

// ResourceSpec is the compute request for the agent pod.
type ResourceSpec struct {
	CPU           resource.Quantity `json:"cpu"`
	Memory        resource.Quantity `json:"memory"`
	EphemeralDisk resource.Quantity `json:"ephemeralDisk,omitempty"`
}

// SandboxSpec configures pod isolation and resources.
type SandboxSpec struct {
	// RuntimeClass selects the sandbox runtime; defaults to runc.
	// +kubebuilder:default=runc
	RuntimeClass RuntimeClass `json:"runtimeClass,omitempty"`
	Resources    ResourceSpec `json:"resources"`
}

// PVCSpec describes the live workspace volume.
type PVCSpec struct {
	StorageClass string            `json:"storageClass,omitempty"`
	Size         resource.Quantity `json:"size"`
}

// CheckpointSpec controls durable snapshotting of the workspace to object store.
type CheckpointSpec struct {
	IntervalSeconds int32  `json:"intervalSeconds,omitempty"`
	Bucket          string `json:"bucket"`
}

// WorkspaceSpec is the durable workspace configuration.
type WorkspaceSpec struct {
	PVC        PVCSpec        `json:"pvc"`
	Checkpoint CheckpointSpec `json:"checkpoint"`
}

// MCPSpec references a rendered MCP configuration for the run.
type MCPSpec struct {
	ConfigRef string `json:"configRef,omitempty"`
}

// EgressSpec is the allowlist enforced by the in-pod egress proxy.
type EgressSpec struct {
	Allowlist []string `json:"allowlist,omitempty"`
}

// RetrySpec bounds automatic crash-resume attempts.
type RetrySpec struct {
	// +kubebuilder:default=5
	MaxRestarts int32  `json:"maxRestarts,omitempty"`
	Backoff     string `json:"backoff,omitempty"` // e.g. "exponential"
}

// AgentRunSpec is the desired state of a single agent run.
type AgentRunSpec struct {
	Project     string        `json:"project"`
	Repo        string        `json:"repo,omitempty"` // GitHub "owner/repo" for the PR
	User        string        `json:"user"`
	Harness     HarnessSpec   `json:"harness"`
	Task        TaskSpec      `json:"task"`
	Interactive bool          `json:"interactive,omitempty"`
	Sandbox     SandboxSpec   `json:"sandbox"`
	Workspace   WorkspaceSpec `json:"workspace"`
	MCP         MCPSpec       `json:"mcp,omitempty"`
	Egress      EgressSpec    `json:"egress,omitempty"`
	Retry       RetrySpec     `json:"retry,omitempty"`
}

// CheckpointRef points at the most recent durable checkpoint.
type CheckpointRef struct {
	URI    string      `json:"uri,omitempty"`
	At     metav1.Time `json:"at,omitempty"`
	Commit string      `json:"commit,omitempty"`
}

// PRStatus records the pull request the run produces.
type PRStatus struct {
	URL    string `json:"url,omitempty"`
	Branch string `json:"branch,omitempty"`
}

// UsageStatus is per-run resource and token accounting.
type UsageStatus struct {
	InputTokens  int64  `json:"inputTokens,omitempty"`
	OutputTokens int64  `json:"outputTokens,omitempty"`
	CostUSD      string `json:"costUsd,omitempty"`
}

// AgentRunStatus is the observed state of an agent run.
type AgentRunStatus struct {
	Phase          RunPhase           `json:"phase,omitempty"`
	PodName        string             `json:"podName,omitempty"`
	RestartCount   int32              `json:"restartCount,omitempty"`
	LastCheckpoint *CheckpointRef     `json:"lastCheckpoint,omitempty"`
	SessionID      string             `json:"sessionId,omitempty"`
	PR             PRStatus           `json:"pr,omitempty"`
	Usage          UsageStatus        `json:"usage,omitempty"`
	Conditions     []metav1.Condition `json:"conditions,omitempty"`
}

// AgentRun is one task executed by one harness in one sandbox against a project.
//
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Project",type=string,JSONPath=`.spec.project`
type AgentRun struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentRunSpec   `json:"spec,omitempty"`
	Status AgentRunStatus `json:"status,omitempty"`
}

// AgentRunList is a list of AgentRun resources.
//
// +kubebuilder:object:root=true
type AgentRunList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentRun `json:"items"`
}
