package harness

import (
	"context"
	"os"

	"github.com/summiteight/wren/internal/runspec"
)

// Result is the outcome of a completed harness run.
type Result struct {
	Branch       string
	PRURL        string
	InputTokens  int64
	OutputTokens int64
}

// Harness executes a task against a workspace and emits events. A nil error with
// a Result means the task completed and a PR is ready (contract exit 0).
type Harness interface {
	// Name identifies the adapter (for logging/telemetry).
	Name() string
	// Run executes the task. It should honor ctx cancellation.
	Run(ctx context.Context, spec runspec.RunSpec, em *Emitter) (Result, error)
}

// Select chooses a harness adapter for a run. The WREN_HARNESS env var overrides
// the RunSpec. In M0, claude-code falls back to the mock adapter unless an
// ANTHROPIC_API_KEY is present, so the pipeline is demonstrable without secrets.
func Select(spec runspec.RunSpec) Harness {
	kind := os.Getenv("WREN_HARNESS")
	if kind == "" {
		kind = spec.Harness
	}
	switch kind {
	case "claude-code":
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			return &ClaudeCode{}
		}
		return &Mock{Note: "claude-code requested but no ANTHROPIC_API_KEY; using mock (M0)"}
	case "mock", "byo", "":
		return &Mock{}
	default:
		return &Mock{Note: "unknown harness " + kind + "; using mock (M0)"}
	}
}
