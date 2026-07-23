package harness

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/summiteight/wren/internal/runspec"
)

// Command-construction matrix: the headless invocation must stay stable
// (verified against developers.openai.com/codex/noninteractive).
func TestCodexArgs(t *testing.T) {
	cases := []struct {
		name string
		spec runspec.RunSpec
		want []string
	}{
		{
			name: "prompt only",
			spec: runspec.RunSpec{Prompt: "fix the bug"},
			want: []string{"exec", "--json", "--sandbox", "danger-full-access",
				"--skip-git-repo-check", "fix the bug"},
		},
		{
			name: "model maps to --model",
			spec: runspec.RunSpec{Prompt: "p", Model: "gpt-5.2-codex"},
			want: []string{"exec", "--json", "--sandbox", "danger-full-access",
				"--skip-git-repo-check", "--model", "gpt-5.2-codex", "p"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := codexArgs(tc.spec)
			if strings.Join(got, " ") != strings.Join(tc.want, " ") {
				t.Errorf("codexArgs = %v, want %v", got, tc.want)
			}
		})
	}
}

// Event-parse matrix for the `codex exec --json` JSONL schema.
func TestParseCodexLine(t *testing.T) {
	cases := []struct {
		name string
		line string
		want []cliEvent
	}{
		{
			name: "agent message → text",
			line: `{"type":"item.completed","item":{"id":"i3","type":"agent_message","text":"done"}}`,
			want: []cliEvent{{text: "done"}},
		},
		{
			name: "command execution → tool",
			line: `{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"bash -lc ls","status":"completed"}}`,
			want: []cliEvent{{tool: "bash -lc ls"}},
		},
		{
			name: "mcp tool call → tool name",
			line: `{"type":"item.completed","item":{"id":"i2","type":"mcp_tool_call","tool":"search"}}`,
			want: []cliEvent{{tool: "search"}},
		},
		{
			name: "file change → tool label",
			line: `{"type":"item.completed","item":{"id":"i4","type":"file_change"}}`,
			want: []cliEvent{{tool: "file_change"}},
		},
		{
			name: "item.started is ignored (no double tool events)",
			line: `{"type":"item.started","item":{"id":"i1","type":"command_execution","command":"bash -lc ls"}}`,
			want: nil,
		},
		{
			name: "turn.completed → usage",
			line: `{"type":"turn.completed","usage":{"input_tokens":24763,"cached_input_tokens":24448,"output_tokens":122}}`,
			want: []cliEvent{{hasUsage: true, inTokens: 24763, outTokens: 122}},
		},
		{
			name: "turn.failed → error with message",
			line: `{"type":"turn.failed","error":{"message":"rate limited"}}`,
			want: []cliEvent{{text: "rate limited", isErr: true}},
		},
		{
			name: "top-level error → error",
			line: `{"type":"error","message":"stream disconnected"}`,
			want: []cliEvent{{text: "stream disconnected", isErr: true}},
		},
		{
			name: "turn.started ignored",
			line: `{"type":"turn.started"}`,
			want: nil,
		},
		{
			name: "non-JSON tolerated",
			line: `codex: some warning`,
			want: nil,
		},
		{
			name: "unknown item type ignored",
			line: `{"type":"item.completed","item":{"id":"i9","type":"reasoning"}}`,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseCodexLine([]byte(tc.line))
			if len(got) != len(tc.want) {
				t.Fatalf("parseCodexLine = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("event %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestCodexMissingBinary(t *testing.T) {
	t.Setenv("PATH", "")
	spec := runspec.RunSpec{RunID: "r-1", Prompt: "x", WorkspacePath: t.TempDir()}
	if _, err := (Codex{}).Run(context.Background(), spec, NewEmitter(&bytes.Buffer{})); err == nil {
		t.Fatal("expected error when codex CLI absent")
	}
}

// The placeholder keys let the CLI start in API-key mode; the egress-proxy
// overwrites the header on the /openai/ route (spec §5.6). Both CODEX_API_KEY
// (the codex exec automation key per the non-interactive docs) and
// OPENAI_API_KEY (fallback) both get one.
func TestCodexEnvPlaceholderKey(t *testing.T) {
	t.Setenv("CODEX_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	env := codexEnv()
	if !hasEnv(env, "CODEX_API_KEY=injected-by-egress-proxy") {
		t.Error("expected placeholder CODEX_API_KEY when unset")
	}
	if !hasEnv(env, "OPENAI_API_KEY=injected-by-egress-proxy") {
		t.Error("expected placeholder OPENAI_API_KEY when unset")
	}
	t.Setenv("CODEX_API_KEY", "real-key-from-somewhere")
	t.Setenv("OPENAI_API_KEY", "real-key-from-somewhere")
	env = codexEnv()
	if hasEnv(env, "CODEX_API_KEY=injected-by-egress-proxy") ||
		hasEnv(env, "OPENAI_API_KEY=injected-by-egress-proxy") {
		t.Error("placeholder must not override an existing key")
	}
}

// TestCodexRunsAndStreams uses a fake `codex` on PATH to verify the adapter
// runs it in the workspace, parses its JSONL events, and reports usage.
func TestCodexRunsAndStreams(t *testing.T) {
	fakeBin := t.TempDir()
	script := "#!/bin/sh\n" +
		`echo '{"type":"thread.started","thread_id":"t1"}'` + "\n" +
		`echo '{"type":"item.completed","item":{"id":"i1","type":"command_execution","command":"bash -lc ls"}}'` + "\n" +
		"echo changed > CHANGED.txt\n" +
		`echo '{"type":"item.completed","item":{"id":"i2","type":"agent_message","text":"all done"}}'` + "\n" +
		`echo '{"type":"turn.completed","usage":{"input_tokens":900,"output_tokens":40}}'` + "\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))

	ws := t.TempDir()
	var buf bytes.Buffer
	res, err := Codex{}.Run(context.Background(),
		runspec.RunSpec{RunID: "r-1", Prompt: "do it", WorkspacePath: ws, BranchPrefix: "wren/me"},
		NewEmitter(&buf))
	if err != nil {
		t.Fatal(err)
	}
	if res.InputTokens != 900 || res.OutputTokens != 40 {
		t.Errorf("usage = %+v", res)
	}
	if res.Branch != "wren/me/r-1" {
		t.Errorf("branch = %q", res.Branch)
	}
	if _, err := os.Stat(filepath.Join(ws, "CHANGED.txt")); err != nil {
		t.Errorf("codex did not run in the workspace: %v", err)
	}
	evs := decodeEvents(t, &buf)
	if len(eventsOfType(evs, EventToolCall)) != 1 {
		t.Errorf("expected 1 tool_call, got events %+v", evs)
	}
	if len(eventsOfType(evs, EventTokenUsage)) != 1 {
		t.Errorf("expected 1 token_usage, got events %+v", evs)
	}
	if len(eventsOfType(evs, EventMessage)) < 1 {
		t.Error("expected the agent message to be re-emitted")
	}
}

// A failing CLI exits non-zero → the adapter returns the error (deterministic;
// the operator must not retry — spec §5.4).
func TestCodexRunFailurePropagates(t *testing.T) {
	fakeBin := t.TempDir()
	script := "#!/bin/sh\n" +
		`echo '{"type":"turn.failed","error":{"message":"boom"}}'` + "\n" +
		"exit 1\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "codex"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))
	spec := runspec.RunSpec{RunID: "r-1", Prompt: "x", WorkspacePath: t.TempDir()}
	if _, err := (Codex{}).Run(context.Background(), spec, NewEmitter(&bytes.Buffer{})); err == nil {
		t.Fatal("expected the CLI failure to propagate")
	}
}

func hasEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}
