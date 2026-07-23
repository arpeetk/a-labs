package harness

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/summiteight/wren/internal/runspec"
)

// Mock is a deterministic harness that simulates a run without any model or
// network: it writes a marker file into the workspace and reports a PR. It
// exercises the full pod lifecycle and operator completion path (spec §5.4).
type Mock struct {
	Note string
}

// Name implements Harness.
func (Mock) Name() string { return "mock" }

// Run implements Harness.
func (m Mock) Run(ctx context.Context, spec runspec.RunSpec, em *Emitter) (Result, error) {
	if m.Note != "" {
		em.Message(m.Note)
	}
	em.Message("mock harness: task = " + spec.Prompt)

	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	// Produce a workspace change, as a real harness would. (The egress canary
	// runs harness-agnostically in podruntime.RunHarness, not here.)
	em.ToolCall("write_file")
	marker := filepath.Join(spec.WorkspacePath, "WREN_MOCK.md")
	content := fmt.Sprintf("# Wren mock run\n\nRun: %s\nProject: %s\nTask: %s\n",
		spec.RunID, spec.Project, spec.Prompt)
	if err := os.WriteFile(marker, []byte(content), 0o644); err != nil {
		em.Errorf("write workspace: " + err.Error())
		return Result{}, fmt.Errorf("write workspace marker: %w", err)
	}
	em.Message("mock harness: wrote " + marker)

	em.Usage(1234, 567)
	em.CheckpointHint()

	// The authoritative pr_ready event is emitted by the finalize step (which
	// actually pushes the branch and opens the PR); the harness just reports the
	// intended branch via its Result.
	return Result{Branch: branchFor(spec), InputTokens: 1234, OutputTokens: 567}, nil
}
