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
	t.Setenv("ANTHROPIC_API_KEY", "")

	if h := Select(runspec.RunSpec{Harness: "mock"}); h.Name() != "mock" {
		t.Errorf("mock → %s", h.Name())
	}
	if h := Select(runspec.RunSpec{Harness: "byo"}); h.Name() != "mock" {
		t.Errorf("byo → %s", h.Name())
	}
	// claude-code with no key falls back to mock (M0).
	if h := Select(runspec.RunSpec{Harness: "claude-code"}); h.Name() != "mock" {
		t.Errorf("claude-code(no key) → %s", h.Name())
	}
	// With a key, selects the claude-code adapter.
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	if h := Select(runspec.RunSpec{Harness: "claude-code"}); h.Name() != "claude-code" {
		t.Errorf("claude-code(key) → %s", h.Name())
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
