package harness

import (
	"context"
	"errors"
	"os/exec"

	"github.com/summiteight/wren/internal/runspec"
)

// ClaudeCode runs a task with Claude Code. In M0 this is a thin adapter over the
// bundled `claude` CLI; the full Agent SDK integration (SessionStore→GCS
// transcript mirroring, OTEL usage, resume-by-sessionId, tool-permission
// routing to the gateway — spec §5.4) lands with the harness image work.
//
// It is selected only when ANTHROPIC_API_KEY is present; otherwise Select falls
// back to Mock so the pipeline is demonstrable without secrets.
type ClaudeCode struct{}

// Name implements Harness.
func (ClaudeCode) Name() string { return "claude-code" }

// Run implements Harness.
func (ClaudeCode) Run(ctx context.Context, spec runspec.RunSpec, em *Emitter) (Result, error) {
	bin, err := exec.LookPath("claude")
	if err != nil {
		em.Errorf("claude CLI not found on PATH")
		return Result{}, errors.New("claude-code harness: `claude` CLI not installed in the image")
	}

	em.Status("running")
	em.Message("claude-code: invoking " + bin)

	// Headless, one-shot invocation. Streaming/session/resume wiring is a
	// follow-up; here we run to completion and surface output as a message.
	cmd := exec.CommandContext(ctx, bin, "-p", spec.Prompt)
	cmd.Dir = spec.WorkspacePath
	out, err := cmd.CombinedOutput()
	if err != nil {
		em.Errorf("claude invocation failed: " + err.Error())
		return Result{}, err
	}
	em.Message(string(out))

	branch := spec.BranchPrefix + "/" + spec.RunID
	em.PRReady(PRInfo{Branch: branch, Title: "Wren: " + truncate(spec.Prompt, 60)})
	return Result{Branch: branch}, nil
}
