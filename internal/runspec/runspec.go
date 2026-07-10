// Package runspec defines the RunSpec contract the operator hands to a harness
// runner. The operator marshals a RunSpec to JSON, stores it in a per-run
// ConfigMap, and mounts it into the pod at MountPath/FileName. Every harness
// image (claude-code, codex, byo) reads this file on startup (spec §5.4).
package runspec

const (
	// MountPath is where the RunSpec ConfigMap is mounted in the pod.
	MountPath = "/etc/wren"
	// FileName is the RunSpec file name within MountPath.
	FileName = "runspec.json"
	// WorkspacePath is where the durable workspace volume is mounted.
	WorkspacePath = "/workspace"
	// MCPConfigPath is where the rendered MCP config is mounted (if any).
	MCPConfigPath = "/etc/wren/mcp/config.json"
)

// Mode is how the harness should start.
type Mode string

const (
	// ModeStart begins a fresh task.
	ModeStart Mode = "start"
	// ModeResume continues an interrupted run from its restored workspace and
	// mirrored session transcript.
	ModeResume Mode = "resume"
)

// RunSpec is the input contract for a harness runner.
type RunSpec struct {
	RunID         string `json:"runId"`
	Project       string `json:"project"`
	Repo          string `json:"repo,omitempty"` // GitHub "owner/repo" for the PR
	User          string `json:"user"`
	Harness       string `json:"harness"`
	Model         string `json:"model,omitempty"`
	Prompt        string `json:"prompt"`
	BaseRef       string `json:"baseRef,omitempty"`
	WorkspacePath string `json:"workspacePath"`
	MCPConfigPath string `json:"mcpConfigPath,omitempty"`
	SessionID     string `json:"sessionId,omitempty"`
	Mode          Mode   `json:"mode"`
	Interactive   bool   `json:"interactive"`

	// CheckpointBucket is the object-store prefix the checkpointer sidecar
	// snapshots the workspace and mirrors the session transcript to.
	CheckpointBucket string `json:"checkpointBucket,omitempty"`
	// BranchPrefix is the git branch namespace for the PR the harness opens.
	BranchPrefix string `json:"branchPrefix,omitempty"`
}
