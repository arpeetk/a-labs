package install

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// The exec runner is local-process glue; test it with real but hermetic
// commands (echo/false), no network involved.
func TestExecRunner(t *testing.T) {
	var out bytes.Buffer
	r := &execRunner{out: &out}
	if !r.LookPath("echo") {
		t.Fatal("echo should be on PATH")
	}
	if r.LookPath("wren-no-such-tool-anywhere") {
		t.Error("bogus tool reported present")
	}
	if err := r.Run(context.Background(), "echo", "hello"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Errorf("Run should stream output, got %q", out.String())
	}
	if err := r.Run(context.Background(), "false"); err == nil {
		t.Error("expected failure from `false`")
	}
	got, err := r.Output(context.Background(), "echo", "captured")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != "captured" {
		t.Errorf("Output = %q", got)
	}
	if strings.Contains(out.String(), "captured") {
		t.Error("Output must not stream (it is used for secrets)")
	}
	if _, err := r.Output(context.Background(), "false"); err == nil {
		t.Error("expected failure from `false`")
	}
}
