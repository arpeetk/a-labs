package harness

import (
	"context"
	"encoding/json"
	"os"

	"github.com/summiteight/wren/internal/runspec"
)

// Codex runs a task with OpenAI's Codex CLI: it drives `codex exec` (the CLI's
// non-interactive mode) inside the workspace and streams its --json JSONL
// events onto the Wren event bus (spec §5.4). Model calls go to
// OPENAI_BASE_URL — the operator points it at the egress-proxy's /openai/
// route, which injects the real key as a Bearer token; the runner holds no
// credential (spec §5.6).
type Codex struct{}

// Name implements Harness.
func (Codex) Name() string { return "codex" }

// Run implements Harness.
func (Codex) Run(ctx context.Context, spec runspec.RunSpec, em *Emitter) (Result, error) {
	return runAgentCLI(ctx, spec, em, agentCLI{
		adapter:   "codex",
		bin:       "codex",
		args:      codexArgs(spec),
		env:       codexEnv(),
		parseLine: parseCodexLine,
	})
}

// codexArgs builds the headless invocation. Verified against the Codex
// non-interactive docs (developers.openai.com/codex/noninteractive):
// `codex exec` prints JSONL events on stdout with --json.
func codexArgs(spec runspec.RunSpec) []string {
	args := []string{
		"exec",
		"--json",
		// danger-full-access disables Codex's own sandbox/approvals — safe
		// here for the same reason as claude's --dangerously-skip-permissions:
		// the pod IS the sandbox, and codex's landlock sandbox would otherwise
		// also deny the agent's spawned commands their (proxied) network path
		// (spec §5.6).
		"--sandbox", "danger-full-access",
		// A repo-less run (no clone) has no .git; the pod boundary, not git,
		// is what makes the workspace safe.
		"--skip-git-repo-check",
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	return append(args, spec.Prompt)
}

// codexEnv provides the subprocess environment. It ensures the API-key env
// vars hold placeholders so the CLI starts in API-key mode even though the
// real key is injected by the egress-proxy (which overwrites the
// Authorization header on the /openai/ route). Both vars are set: the Codex
// non-interactive docs make CODEX_API_KEY the supported key for `codex exec`
// automation, with OPENAI_API_KEY the fallback — which one wins is the CLI's
// choice, and irrelevant here because the proxy owns the credential either
// way. OPENAI_BASE_URL itself is wired by the operator and simply passed
// through.
func codexEnv() []string {
	env := ensureEnv(os.Environ(), "CODEX_API_KEY", "injected-by-egress-proxy")
	return ensureEnv(env, "OPENAI_API_KEY", "injected-by-egress-proxy")
}

// codexStreamEvent is the subset of the `codex exec --json` JSONL schema we
// consume (event types thread/turn/item.*; item types agent_message,
// command_execution, mcp_tool_call, file_change, web_search, ...).
type codexStreamEvent struct {
	Type string `json:"type"`
	Item *struct {
		Type    string `json:"type"`
		Text    string `json:"text"`    // agent_message
		Command string `json:"command"` // command_execution
		Tool    string `json:"tool"`    // mcp_tool_call
	} `json:"item"`
	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"` // turn.failed
	Message string `json:"message"` // top-level "error" events
}

// parseCodexLine maps one codex JSONL line to its normalized events. Only
// completed items are surfaced (item.started would double-report tool calls).
func parseCodexLine(line []byte) []cliEvent {
	var ev codexStreamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil // tolerate non-JSON / partial lines
	}
	switch ev.Type {
	case "item.completed":
		if ev.Item == nil {
			return nil
		}
		switch ev.Item.Type {
		case "agent_message":
			return []cliEvent{{text: ev.Item.Text}}
		case "command_execution":
			return []cliEvent{{tool: ev.Item.Command}}
		case "mcp_tool_call":
			return []cliEvent{{tool: ev.Item.Tool}}
		case "file_change", "web_search":
			return []cliEvent{{tool: ev.Item.Type}}
		}
	case "turn.completed":
		if ev.Usage != nil {
			return []cliEvent{usageEvent(ev.Usage.InputTokens, ev.Usage.OutputTokens)}
		}
	case "turn.failed":
		e := cliEvent{isErr: true}
		if ev.Error != nil {
			e.text = ev.Error.Message
		}
		return []cliEvent{e}
	case "error":
		return []cliEvent{{text: ev.Message, isErr: true}}
	}
	return nil
}
