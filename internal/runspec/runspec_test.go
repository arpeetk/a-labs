package runspec

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestRunSpecJSONRoundTrip(t *testing.T) {
	in := RunSpec{
		RunID:            "r-abc",
		Project:          "payments-api",
		User:             "arpeet@corp.com",
		Harness:          "claude-code",
		Model:            "claude-opus-4-8",
		Prompt:           "do the thing",
		BaseRef:          "main",
		WorkspacePath:    WorkspacePath,
		MCPConfigPath:    MCPConfigPath,
		SessionID:        "sess-1",
		Mode:             ModeResume,
		Interactive:      true,
		CheckpointBucket: "gs://wren-ckpt",
		RestoreRequired:  true,
		BranchPrefix:     "wren/arpeet",
	}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	var out RunSpec
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

func TestCodexHomePath(t *testing.T) {
	if got := CodexHomePath("/work", "owner/repo"); got != filepath.Join("/work", ".git", "wren", "codex") {
		t.Fatalf("repo Codex home = %q", got)
	}
	if got := CodexHomePath("/work", ""); got != filepath.Join("/work", ".wren", "codex") {
		t.Fatalf("repo-less Codex home = %q", got)
	}
}

func TestModeConstants(t *testing.T) {
	if ModeStart != "start" || ModeResume != "resume" {
		t.Fatalf("unexpected mode values: %q %q", ModeStart, ModeResume)
	}
}
