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

func decodeEvents(t *testing.T, r *bytes.Buffer) []Event {
	t.Helper()
	var out []Event
	dec := json.NewDecoder(r)
	for dec.More() {
		var e Event
		if err := dec.Decode(&e); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		out = append(out, e)
	}
	return out
}

func eventsOfType(evs []Event, tp EventType) []Event {
	var out []Event
	for _, e := range evs {
		if e.Type == tp {
			out = append(out, e)
		}
	}
	return out
}

func TestEmitterStampsTimeAndSerializes(t *testing.T) {
	var buf bytes.Buffer
	em := NewEmitter(&buf)
	em.Status("running")
	em.Usage(10, 20)
	em.PRReady(PRInfo{Branch: "b"})
	evs := decodeEvents(t, &buf)
	if len(evs) != 3 {
		t.Fatalf("got %d events", len(evs))
	}
	if evs[0].Time.IsZero() {
		t.Error("time not stamped")
	}
	if evs[1].InputTokens != 10 || evs[1].OutputTokens != 20 {
		t.Errorf("usage = %+v", evs[1])
	}
	if evs[2].PR == nil || evs[2].PR.Branch != "b" {
		t.Errorf("pr = %+v", evs[2].PR)
	}
}

func TestMockHarnessWritesWorkspaceAndReportsPR(t *testing.T) {
	ws := t.TempDir()
	spec := runspec.RunSpec{
		RunID: "r-1", Project: "p", Prompt: "add tests",
		WorkspacePath: ws, BranchPrefix: "wren/arpeet",
	}
	var buf bytes.Buffer
	em := NewEmitter(&buf)

	res, err := Mock{}.Run(context.Background(), spec, em)
	if err != nil {
		t.Fatal(err)
	}
	if res.Branch != "wren/arpeet/r-1" || res.InputTokens != 1234 {
		t.Fatalf("result = %+v", res)
	}
	// Wrote the marker file.
	b, err := os.ReadFile(filepath.Join(ws, "WREN_MOCK.md"))
	if err != nil {
		t.Fatalf("marker not written: %v", err)
	}
	if !strings.Contains(string(b), "add tests") {
		t.Errorf("marker content = %q", b)
	}
	// The harness reports usage; the authoritative pr_ready is emitted later by
	// the finalize step, not the adapter.
	evs := decodeEvents(t, &buf)
	if len(eventsOfType(evs, EventTokenUsage)) != 1 {
		t.Error("expected a token_usage event")
	}
	if len(eventsOfType(evs, EventToolCall)) != 1 {
		t.Error("expected a tool_call event")
	}
}

func TestMockHarnessDefaultBranch(t *testing.T) {
	spec := runspec.RunSpec{RunID: "r-9", Prompt: "x", WorkspacePath: t.TempDir()}
	res, err := Mock{}.Run(context.Background(), spec, NewEmitter(&bytes.Buffer{}))
	if err != nil || res.Branch != "wren/r-9" {
		t.Fatalf("default branch = %+v, %v", res, err)
	}
}

func TestMockHarnessWorkspaceError(t *testing.T) {
	// Non-existent workspace dir → write fails.
	spec := runspec.RunSpec{RunID: "r-1", Prompt: "x", WorkspacePath: "/no/such/dir/here"}
	_, err := Mock{}.Run(context.Background(), spec, NewEmitter(&bytes.Buffer{}))
	if err == nil {
		t.Fatal("expected workspace write error")
	}
}

func TestMockHarnessContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	spec := runspec.RunSpec{RunID: "r-1", Prompt: "x", WorkspacePath: t.TempDir()}
	if _, err := (Mock{}).Run(ctx, spec, NewEmitter(&bytes.Buffer{})); err == nil {
		t.Fatal("expected context cancellation error")
	}
}

func TestSelect(t *testing.T) {
	t.Setenv("WREN_HARNESS", "")

	if h := Select(runspec.RunSpec{Harness: "mock"}); h.Name() != "mock" {
		t.Errorf("mock → %s", h.Name())
	}
	if h := Select(runspec.RunSpec{Harness: "byo"}); h.Name() != "mock" {
		t.Errorf("byo → %s", h.Name())
	}
	// claude-code always selects the real adapter (key comes via the proxy).
	if h := Select(runspec.RunSpec{Harness: "claude-code"}); h.Name() != "claude-code" {
		t.Errorf("claude-code → %s", h.Name())
	}
	// codex / opencode select their real adapters (keys come via the proxy).
	if h := Select(runspec.RunSpec{Harness: "codex"}); h.Name() != "codex" {
		t.Errorf("codex → %s", h.Name())
	}
	if h := Select(runspec.RunSpec{Harness: "opencode"}); h.Name() != "opencode" {
		t.Errorf("opencode → %s", h.Name())
	}
	if h := Select(runspec.RunSpec{Harness: "weird"}); h.Name() != "mock" {
		t.Errorf("unknown → %s", h.Name())
	}
	// Env override wins over the spec.
	t.Setenv("WREN_HARNESS", "mock")
	if h := Select(runspec.RunSpec{Harness: "claude-code"}); h.Name() != "mock" {
		t.Errorf("WREN_HARNESS override → %s", h.Name())
	}
}

func TestClaudeCodeMissingBinary(t *testing.T) {
	// Empty PATH → claude not found → graceful error.
	t.Setenv("PATH", "")
	spec := runspec.RunSpec{RunID: "r-1", Prompt: "x", WorkspacePath: t.TempDir()}
	if _, err := (ClaudeCode{}).Run(context.Background(), spec, NewEmitter(&bytes.Buffer{})); err == nil {
		t.Fatal("expected error when claude CLI absent")
	}
}

// TestClaudeCodeRunsAndStreams uses a fake `claude` on PATH to verify the
// adapter runs it in the workspace, parses its stream-json events, and reports
// token usage — without a real model call.
func TestClaudeCodeRunsAndStreams(t *testing.T) {
	fakeBin := t.TempDir()
	script := "#!/bin/sh\n" +
		`echo '{"type":"assistant","message":{"content":[{"type":"text","text":"planning"},{"type":"tool_use","name":"Edit"}]}}'` + "\n" +
		"echo changed > CHANGED.txt\n" +
		`echo '{"type":"result","subtype":"success","is_error":false,"result":"done","usage":{"input_tokens":120,"output_tokens":45}}'` + "\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "claude"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+":"+os.Getenv("PATH"))

	ws := t.TempDir()
	var buf bytes.Buffer
	res, err := ClaudeCode{}.Run(context.Background(),
		runspec.RunSpec{RunID: "r-1", Prompt: "do it", WorkspacePath: ws, BranchPrefix: "wren/me"},
		NewEmitter(&buf))
	if err != nil {
		t.Fatal(err)
	}
	if res.InputTokens != 120 || res.OutputTokens != 45 {
		t.Errorf("usage = %+v", res)
	}
	if res.Branch != "wren/me/r-1" {
		t.Errorf("branch = %q", res.Branch)
	}
	// The agent ran with cwd = workspace (the fake wrote a file there).
	if _, err := os.Stat(filepath.Join(ws, "CHANGED.txt")); err != nil {
		t.Errorf("claude did not run in the workspace: %v", err)
	}
	evs := decodeEvents(t, &buf)
	if len(eventsOfType(evs, EventToolCall)) != 1 {
		t.Errorf("expected 1 tool_call, got events %+v", evs)
	}
	if len(eventsOfType(evs, EventMessage)) < 2 {
		t.Error("expected assistant + result messages")
	}
}
