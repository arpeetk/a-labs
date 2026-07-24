package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/summiteight/wren/internal/runspec"
)

// Command-construction matrix (opencode.ai/docs/cli: `run --format json`).
func TestOpenCodeArgs(t *testing.T) {
	cases := []struct {
		name string
		spec runspec.RunSpec
		want []string
	}{
		{
			name: "prompt only",
			spec: runspec.RunSpec{Prompt: "fix the bug"},
			want: []string{"run", "--format", "json", "--auto", "fix the bug"},
		},
		{
			name: "bare model defaults to the anthropic provider",
			spec: runspec.RunSpec{Prompt: "p", Model: "claude-sonnet-4-20250514"},
			want: []string{"run", "--format", "json", "--auto", "--model",
				"anthropic/claude-sonnet-4-20250514", "p"},
		},
		{
			name: "provider-qualified model passes through",
			spec: runspec.RunSpec{Prompt: "p", Model: "anthropic/claude-opus-4-1"},
			want: []string{"run", "--format", "json", "--auto", "--model",
				"anthropic/claude-opus-4-1", "p"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := opencodeArgs(tc.spec)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("opencodeArgs = %v, want %v", got, tc.want)
			}
		})
	}
}

// The rendered config points opencode's anthropic provider at the
// egress-proxy's route (the proxy injects the real key — spec §5.6).
func TestOpenCodeConfigJSON(t *testing.T) {
	var parsed struct {
		Provider map[string]struct {
			Options struct {
				BaseURL string `json:"baseURL"`
			} `json:"options"`
		} `json:"provider"`
	}
	if err := json.Unmarshal(opencodeConfigJSON("http://127.0.0.1:8099/anthropic"), &parsed); err != nil {
		t.Fatal(err)
	}
	// opencode expects the full API base (its provider appends no version
	// segment), so the adapter appends /v1 to the proxy route.
	if got := parsed.Provider["anthropic"].Options.BaseURL; got != "http://127.0.0.1:8099/anthropic/v1" {
		t.Errorf("baseURL = %q", got)
	}

	// Trailing slash is normalized, not doubled.
	if err := json.Unmarshal(opencodeConfigJSON("http://127.0.0.1:8099/anthropic/"), &parsed); err != nil {
		t.Fatal(err)
	}
	if got := parsed.Provider["anthropic"].Options.BaseURL; got != "http://127.0.0.1:8099/anthropic/v1" {
		t.Errorf("trailing-slash baseURL = %q", got)
	}

	// No proxy wired → no baseURL override (opencode uses its default
	// endpoint; the egress lockdown then fails the run closed).
	if err := json.Unmarshal(opencodeConfigJSON(""), &parsed); err != nil {
		t.Fatal(err)
	}
	if got := parsed.Provider["anthropic"].Options.BaseURL; got != "" {
		t.Errorf("empty env should omit baseURL, got %q", got)
	}
}

// Event-parse matrix for `opencode run --format json` events.
func TestParseOpenCodeLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []cliEvent
	}{
		{
			name: "text part → message",
			line: `{"type":"text","part":{"text":"here is the plan"}}`,
			want: []cliEvent{{text: "here is the plan"}},
		},
		{
			name: "tool_use part → tool",
			line: `{"type":"tool_use","part":{"tool":"bash","state":{"status":"running"}}}`,
			want: []cliEvent{{tool: "bash"}},
		},
		{
			name: "step_finish → usage",
			line: `{"type":"step_finish","part":{"tokens":{"input":1200,"output":80,"reasoning":10},"cost":0.01}}`,
			want: []cliEvent{{hasUsage: true, inTokens: 1200, outTokens: 80}},
		},
		{
			name: "error → terminal error",
			line: `{"type":"error","error":"provider exploded"}`,
			want: []cliEvent{{text: "provider exploded", isErr: true}},
		},
		{
			name: "step_start ignored",
			line: `{"type":"step_start","part":{}}`,
			want: nil,
		},
		{
			name: "empty text part ignored",
			line: `{"type":"text","part":{"text":""}}`,
			want: nil,
		},
		{
			name: "non-JSON tolerated",
			line: `opencode: some warning`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseOpenCodeLine([]byte(tc.line))
			if len(got) != len(tc.want) {
				t.Fatalf("parseOpenCodeLine = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("event %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestOpenCodeMissingBinary(t *testing.T) {
	t.Setenv("PATH", "")
	spec := runspec.RunSpec{RunID: "r-1", Prompt: "x", WorkspacePath: t.TempDir()}
	if _, err := (OpenCode{}).Run(context.Background(), spec, NewEmitter(&bytes.Buffer{})); err == nil {
		t.Fatal("expected error when opencode CLI absent")
	}
}

func TestOpenCodeEnv(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	env := opencodeEnv("/tmp/wren-opencode-x/opencode.json")
	if !hasEnv(env, "ANTHROPIC_API_KEY=injected-by-egress-proxy") {
		t.Error("expected placeholder ANTHROPIC_API_KEY when unset")
	}
	if !hasEnv(env, "OPENCODE_CONFIG=/tmp/wren-opencode-x/opencode.json") {
		t.Error("expected OPENCODE_CONFIG to point at the rendered config")
	}
	if !hasEnv(env, "OPENCODE_DISABLE_AUTOUPDATE=1") || !hasEnv(env, "OPENCODE_DISABLE_MODELS_FETCH=1") {
		t.Error("expected opencode's off-route update/catalog fetches disabled")
	}
}

// TestOpenCodeRunsAndStreams uses a fake `opencode` on PATH to verify the
// adapter renders the provider config, runs the CLI in the workspace, and
// parses its JSON events.
func TestOpenCodeRunsAndStreams(t *testing.T) {
	fakeBin := t.TempDir()
	// The fake records the config path it was handed (proving OPENCODE_CONFIG
	// wiring), then emits a JSON event stream.
	script := "#!/bin/sh\n" +
		"cp \"$OPENCODE_CONFIG\" ./SEEN_CONFIG.json\n" +
		`echo '{"type":"text","part":{"text":"planning"}}'` + "\n" +
		`echo '{"type":"tool_use","part":{"tool":"edit"}}'` + "\n" +
		"echo changed > CHANGED.txt\n" +
		`echo '{"type":"step_finish","part":{"tokens":{"input":700,"output":60}}}'` + "\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "opencode"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
	t.Setenv("ANTHROPIC_BASE_URL", "http://127.0.0.1:8099/anthropic")

	ws := t.TempDir()
	var buf bytes.Buffer
	res, err := OpenCode{}.Run(context.Background(),
		runspec.RunSpec{RunID: "r-1", Prompt: "do it", WorkspacePath: ws, BranchPrefix: "wren/me"},
		NewEmitter(&buf))
	if err != nil {
		t.Fatal(err)
	}
	if res.InputTokens != 700 || res.OutputTokens != 60 {
		t.Errorf("usage = %+v", res)
	}
	if res.Branch != "wren/me/r-1" {
		t.Errorf("branch = %q", res.Branch)
	}
	if _, err := os.Stat(filepath.Join(ws, "CHANGED.txt")); err != nil {
		t.Errorf("opencode did not run in the workspace: %v", err)
	}
	// The rendered config pointed the anthropic provider at the proxy route.
	seen, err := os.ReadFile(filepath.Join(ws, "SEEN_CONFIG.json"))
	if err != nil {
		t.Fatalf("opencode was not handed the rendered config: %v", err)
	}
	if !strings.Contains(string(seen), `"baseURL":"http://127.0.0.1:8099/anthropic/v1"`) {
		t.Errorf("rendered config = %s", seen)
	}
	evs := decodeEvents(t, &buf)
	if len(eventsOfType(evs, EventToolCall)) != 1 {
		t.Errorf("expected 1 tool_call, got events %+v", evs)
	}
	if len(eventsOfType(evs, EventTokenUsage)) != 1 {
		t.Errorf("expected 1 token_usage, got events %+v", evs)
	}
	if len(eventsOfType(evs, EventMessage)) < 1 {
		t.Error("expected the text part to be re-emitted")
	}
}
