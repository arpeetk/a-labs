// Package runspec defines the RunSpec contract the operator hands to a harness
// runner. The operator marshals a RunSpec to JSON, stores it in a per-run
// ConfigMap, and mounts it into the pod at MountPath/FileName. Every harness
// image reads this file on startup.
package runspec

import "path/filepath"

// The operator uses harness exit codes to decide whether a failure may be
// retried. A clean error is deterministic and
// must not be retried (it would just repeat and re-spend the agent's tokens),
// while ExitRetryable signals a transient condition worth a fresh pod.
const (
	ExitSuccess   = 0
	ExitError     = 1  // deterministic failure — do NOT retry
	ExitRetryable = 75 // transient failure — the operator may retry (EX_TEMPFAIL)
)

// CodexHomePath returns the per-run Codex state directory on the durable
// workspace. Repository-backed runs keep it under .git so finalization cannot
// stage transcripts; repo-less runs avoid creating a misleading .git tree.
func CodexHomePath(workspacePath, repo string) string {
	if repo != "" {
		return filepath.Join(workspacePath, ".git", "wren", "codex")
	}
	return filepath.Join(workspacePath, ".wren", "codex")
}

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
	// RestoreRequired is true only when Mode is ModeResume AND the workspace
	// PVC was just recreated after a confirmed loss (the controller's
	// WorkspaceRestorePending condition) — i.e. the disk is genuinely empty
	// and hydrate MUST restore the latest checkpoint before the harness starts,
	// as opposed to an ordinary crash-resume where the PVC survived and
	// restoring would overwrite live, still-present work. False (the default)
	// is every ModeResume case today.
	RestoreRequired bool `json:"restoreRequired,omitempty"`
	// RestoreCheckpoint identifies a checkpoint manifest that hydrate must
	// restore exactly. It is set for pause/resume after workspace loss; unlike
	// ordinary recovery, hydrate must not silently fall back to another object.
	RestoreCheckpoint string `json:"restoreCheckpoint,omitempty"`
	// BranchPrefix is the git branch namespace for the PR the harness opens.
	BranchPrefix string `json:"branchPrefix,omitempty"`
}
