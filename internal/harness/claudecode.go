package harness

import (
	"context"
	"encoding/json"
	"os"

	"github.com/summiteight/wren/internal/runspec"
)

// ClaudeCode runs a task with Claude Code: it drives the bundled `claude` CLI in
// headless mode inside the cloned workspace, letting the agent read/edit files
// and run tools autonomously, and streams its stream-json events onto the Wren
// event bus. Model calls go through the egress-proxy (ANTHROPIC_BASE_URL); the
// real API key is injected there, never held by the runner (spec §5.4/§5.6).
type ClaudeCode struct{}

// Name implements Harness.
func (ClaudeCode) Name() string { return "claude-code" }

// Run implements Harness.
func (ClaudeCode) Run(ctx context.Context, spec runspec.RunSpec, em *Emitter) (Result, error) {
	args := []string{
		"--print", spec.Prompt,
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions", // safe: the pod IS the sandbox
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	return runAgentCLI(ctx, spec, em, agentCLI{
		adapter:   "claude-code",
		bin:       "claude",
		args:      args,
		env:       claudeEnv(),
		parseLine: parseClaudeLine,
	})
}

// claudeEnv provides the subprocess environment. It ensures ANTHROPIC_API_KEY is
// set to a placeholder so the CLI starts even though the real key is injected by
// the egress-proxy (which overwrites the x-api-key header on the way out).
func claudeEnv() []string {
	env := ensureEnv(os.Environ(), "ANTHROPIC_API_KEY", "injected-by-egress-proxy")
	// Keep the agent's own config/telemetry from leaking across runs.
	return append(env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
}

// claudeStreamEvent is the subset of the claude stream-json schema we consume.
type claudeStreamEvent struct {
	Type    string `json:"type"`
	IsError bool   `json:"is_error"`
	Result  string `json:"result"`
	Message *struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	} `json:"message"`
	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

// parseClaudeLine maps one claude stream-json line to its normalized events.
// Non-JSON / partial lines are tolerated (skipped).
func parseClaudeLine(line []byte) []cliEvent {
	var ev claudeStreamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil
	}
	var out []cliEvent
	switch ev.Type {
	case "assistant":
		if ev.Message != nil {
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "text":
					if c.Text != "" {
						out = append(out, cliEvent{text: c.Text})
					}
				case "tool_use":
					out = append(out, cliEvent{tool: c.Name})
				}
			}
		}
		if ev.Usage != nil {
			out = append(out, usageEvent(ev.Usage.InputTokens, ev.Usage.OutputTokens))
		}
	case "result":
		e := cliEvent{text: ev.Result, isErr: ev.IsError}
		if ev.Usage != nil {
			e.hasUsage, e.inTokens, e.outTokens = true, ev.Usage.InputTokens, ev.Usage.OutputTokens
		}
		out = append(out, e)
	}
	return out
}

func usageEvent(in, out int64) cliEvent {
	return cliEvent{hasUsage: true, inTokens: in, outTokens: out}
}
