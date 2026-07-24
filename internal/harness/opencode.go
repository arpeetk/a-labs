package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/summiteight/wren/internal/runspec"
)

// OpenCode runs a task with the OpenCode CLI: it drives `opencode run` (the
// CLI's non-interactive mode) inside the workspace and streams its
// --format=json events onto the Wren event bus (spec §5.4).
//
// OpenCode rides the existing Anthropic egress route — no new proxy surface:
// the adapter renders a per-run opencode config pointing the anthropic
// provider's baseURL at the injected ANTHROPIC_BASE_URL, and the proxy injects
// the real x-api-key on the way out (spec §5.6).
type OpenCode struct{}

// Name implements Harness.
func (OpenCode) Name() string { return "opencode" }

// Run implements Harness.
func (OpenCode) Run(ctx context.Context, spec runspec.RunSpec, em *Emitter) (Result, error) {
	// The config must live outside the workspace: a file in the repo would be
	// committed into the PR by finalize. The per-run tmp dir is destroyed on
	// return.
	cfgDir, err := os.MkdirTemp("", "wren-opencode-")
	if err != nil {
		return Result{}, fmt.Errorf("opencode harness: make config dir: %w", err)
	}
	defer os.RemoveAll(cfgDir)
	cfgPath := filepath.Join(cfgDir, "opencode.json")
	if err := os.WriteFile(cfgPath, opencodeConfigJSON(os.Getenv("ANTHROPIC_BASE_URL")), 0o600); err != nil {
		return Result{}, fmt.Errorf("opencode harness: write config: %w", err)
	}

	return runAgentCLI(ctx, spec, em, agentCLI{
		adapter:   "opencode",
		bin:       "opencode",
		args:      opencodeArgs(spec),
		env:       opencodeEnv(cfgPath),
		parseLine: parseOpenCodeLine,
	})
}

// opencodeArgs builds the headless invocation (opencode.ai/docs/cli):
// `opencode run --format json` emits raw JSON events; --auto auto-approves
// permission prompts — safe for the same reason as claude's
// --dangerously-skip-permissions: the pod IS the sandbox (spec §5.6).
func opencodeArgs(spec runspec.RunSpec) []string {
	args := []string{"run", "--format", "json", "--auto"}
	if spec.Model != "" {
		args = append(args, "--model", opencodeModel(spec.Model))
	}
	return append(args, spec.Prompt)
}

// opencodeModel maps RunSpec.Model to opencode's provider/model form. The
// adapter wires only the Anthropic provider (it is the one the egress-proxy
// can credential), so a bare model name defaults to it.
func opencodeModel(model string) string {
	if strings.Contains(model, "/") {
		return model
	}
	return "anthropic/" + model
}

// opencodeConfigJSON renders the per-run opencode config. baseURL is the
// egress-proxy's Anthropic route; opencode's anthropic provider expects the
// full API base (it appends no version segment itself — the provider docs show
// "https://api.anthropic.com/v1"), so /v1 is appended here. An empty baseURL
// omits the override (no proxy wired; opencode then uses its default endpoint
// and the egress lockdown, when on, fails the run closed).
func opencodeConfigJSON(baseURL string) []byte {
	type options struct {
		BaseURL string `json:"baseURL,omitempty"`
	}
	type provider struct {
		Options options `json:"options"`
	}
	cfg := struct {
		Schema   string              `json:"$schema"`
		Provider map[string]provider `json:"provider"`
	}{
		Schema:   "https://opencode.ai/config.json",
		Provider: map[string]provider{"anthropic": {}},
	}
	if baseURL != "" {
		cfg.Provider["anthropic"] = provider{Options: options{
			BaseURL: strings.TrimRight(baseURL, "/") + "/v1",
		}}
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		panic(err) // static shape — cannot fail
	}
	return b
}

// opencodeEnv provides the subprocess environment: the rendered config path,
// the ANTHROPIC_API_KEY placeholder (the proxy injects the real x-api-key),
// and switches off opencode's own update/model-catalog fetches so the run does
// not stall on off-route egress the lockdown would block anyway (mirrors
// claudeEnv's CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC rationale).
func opencodeEnv(configPath string) []string {
	env := ensureEnv(os.Environ(), "ANTHROPIC_API_KEY", "injected-by-egress-proxy")
	return append(env,
		"OPENCODE_CONFIG="+configPath,
		"OPENCODE_DISABLE_AUTOUPDATE=1",
		"OPENCODE_DISABLE_MODELS_FETCH=1",
	)
}

// openCodeStreamEvent is the subset of the `opencode run --format json` event
// schema we consume: text / tool_use / step_finish parts, plus errors.
type openCodeStreamEvent struct {
	Type string `json:"type"`
	Part *struct {
		Text   string `json:"text"` // text
		Tool   string `json:"tool"` // tool_use
		Tokens *struct {
			Input  int64 `json:"input"`
			Output int64 `json:"output"`
		} `json:"tokens"` // step_finish
	} `json:"part"`
	Error string `json:"error"` // error events (shape tolerated loosely)
}

// parseOpenCodeLine maps one opencode JSON event line to its normalized
// events. Unknown event types (step_start, message bookkeeping) are skipped.
func parseOpenCodeLine(line []byte) []cliEvent {
	var ev openCodeStreamEvent
	if err := json.Unmarshal(line, &ev); err != nil {
		return nil // tolerate non-JSON / partial lines
	}
	switch ev.Type {
	case "text":
		if ev.Part != nil && ev.Part.Text != "" {
			return []cliEvent{{text: ev.Part.Text}}
		}
	case "tool_use":
		if ev.Part != nil && ev.Part.Tool != "" {
			return []cliEvent{{tool: ev.Part.Tool}}
		}
	case "step_finish":
		if ev.Part != nil && ev.Part.Tokens != nil {
			return []cliEvent{usageEvent(ev.Part.Tokens.Input, ev.Part.Tokens.Output)}
		}
	case "error":
		return []cliEvent{{text: ev.Error, isErr: true}}
	}
	return nil
}
