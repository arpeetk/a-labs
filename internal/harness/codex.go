package harness

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/summiteight/wren/internal/runspec"
)

// Codex runs a task with OpenAI's Codex CLI: it drives `codex exec` (the CLI's
// non-interactive mode) inside the workspace and streams its --json JSONL
// events onto the Wren event bus (spec §5.4). Model calls go to
// OPENAI_BASE_URL — the operator points it at the egress-proxy's /openai/
// route. The adapter selects a Responses-over-HTTP/SSE provider for that
// route; the proxy injects the real Bearer token, so the runner holds no
// credential (spec §5.6).
type Codex struct{}

// Name implements Harness.
func (Codex) Name() string { return "codex" }

// Run implements Harness.
func (Codex) Run(ctx context.Context, spec runspec.RunSpec, em *Emitter) (Result, error) {
	baseURL := os.Getenv("OPENAI_BASE_URL")
	if spec.Mode == runspec.ModeResume && !codexSessionExists(os.Getenv("CODEX_HOME")) {
		// Older runs and repo-less custom images may not have persisted Codex's
		// transcript. Workspace-only recovery is still useful and safer than a
		// deterministic "no session found" loop: start a fresh conversation in
		// the surviving workspace and tell the operator exactly what degraded.
		em.Message("codex: no persisted session found; continuing in workspace-only recovery mode")
		spec.Mode = runspec.ModeStart
	}
	return runAgentCLI(ctx, spec, em, agentCLI{
		adapter:   "codex",
		bin:       "codex",
		args:      codexArgs(spec, baseURL),
		env:       codexEnv(baseURL),
		parseLine: parseCodexLine,
	})
}

// codexArgs builds the headless invocation. When the operator supplies its
// proxy route, an ephemeral provider config keeps all traffic on the supported
// Responses HTTP/SSE path. A local invocation without the route retains the
// user's normal Codex provider configuration.
func codexArgs(spec runspec.RunSpec, baseURL string) []string {
	args := []string{"exec"}
	if spec.Mode == runspec.ModeResume {
		args = append(args,
			"resume", "--last", "--json",
			// `exec resume` does not accept `--sandbox`; this is its supported
			// equivalent for an externally sandboxed Wren pod.
			"--dangerously-bypass-approvals-and-sandbox",
			"--skip-git-repo-check",
		)
	} else {
		args = append(args,
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
		)
	}
	if baseURL != "" {
		baseURL = strings.TrimRight(baseURL, "/")
		if !strings.HasSuffix(baseURL, "/v1") {
			baseURL += "/v1"
		}
		args = append(args,
			"--config", `model_provider="wren"`,
			"--config", `model_providers.wren.name="Wren egress proxy"`,
			"--config", "model_providers.wren.base_url="+strconv.Quote(baseURL),
			"--config", `model_providers.wren.env_key="OPENAI_API_KEY"`,
			"--config", `model_providers.wren.wire_api="responses"`,
			"--config", "model_providers.wren.supports_websockets=false",
		)
	}
	if spec.Model != "" {
		args = append(args, "--model", spec.Model)
	}
	if spec.Mode == runspec.ModeResume {
		return append(args, "Continue the interrupted task from the durable workspace. Inspect existing work before changing anything, complete the original task, and do not duplicate finished work.")
	}
	return append(args, spec.Prompt)
}

// codexSessionExists checks only Codex's documented session directory and is
// deliberately tolerant of versioned subdirectories. Each run has a private
// CODEX_HOME, so --last cannot select another run's conversation.
func codexSessionExists(home string) bool {
	if home == "" {
		return false
	}
	found := false
	_ = filepath.WalkDir(filepath.Join(home, "sessions"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".jsonl") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

// codexEnv adds placeholder keys only in proxy mode. The CLI needs an API key
// to start, but the egress proxy replaces it at the trust boundary. Direct
// invocations retain their ambient environment unchanged.
func codexEnv(baseURL string) []string {
	env := os.Environ()
	if baseURL == "" {
		return env
	}
	env = ensureEnv(env, "CODEX_API_KEY", "injected-by-egress-proxy")
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
