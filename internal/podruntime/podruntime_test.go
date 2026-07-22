package podruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/summiteight/wren/internal/runspec"
)

func writeRunSpec(t *testing.T, dir string, spec runspec.RunSpec) string {
	t.Helper()
	b, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "runspec.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRunHarnessHappyPath(t *testing.T) {
	ws := t.TempDir()
	specPath := writeRunSpec(t, t.TempDir(), runspec.RunSpec{
		RunID: "r-1", Project: "p", Harness: "mock", Prompt: "do it", WorkspacePath: ws,
	})
	var buf bytes.Buffer
	if err := RunHarness(context.Background(), &buf, specPath); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"succeeded"`) {
		t.Errorf("expected succeeded status, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(ws, "WREN_MOCK.md")); err != nil {
		t.Errorf("workspace marker missing: %v", err)
	}
}

func TestRunHarnessLoadError(t *testing.T) {
	if err := RunHarness(context.Background(), &bytes.Buffer{}, "/nope/runspec.json"); err == nil {
		t.Fatal("expected load error")
	}
}

func TestLoadRunSpecParseError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runspec.json")
	if err := os.WriteFile(path, []byte("{bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRunSpec(path); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestRunHydrate(t *testing.T) {
	for _, mode := range []runspec.Mode{runspec.ModeStart, runspec.ModeResume} {
		specPath := writeRunSpec(t, t.TempDir(), runspec.RunSpec{RunID: "r-1", Mode: mode})
		var buf bytes.Buffer
		if err := RunHydrate(context.Background(), &buf, specPath); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(buf.String(), "hydrate") {
			t.Errorf("hydrate output = %q", buf.String())
		}
	}
}

func TestRunSidecarStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	var buf bytes.Buffer
	go func() { done <- RunSidecar(ctx, &buf, "egress-proxy") }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("sidecar did not stop on cancel")
	}
	if !strings.Contains(buf.String(), "egress-proxy") {
		t.Errorf("sidecar output = %q", buf.String())
	}
}

func TestDispatch(t *testing.T) {
	ws := t.TempDir()
	specPath := writeRunSpec(t, t.TempDir(), runspec.RunSpec{RunID: "r-1", Harness: "mock", Prompt: "x", WorkspacePath: ws})

	if err := Dispatch(context.Background(), &bytes.Buffer{}, RoleHarness, specPath); err != nil {
		t.Errorf("harness dispatch: %v", err)
	}
	if err := Dispatch(context.Background(), &bytes.Buffer{}, RoleHydrate, specPath); err != nil {
		t.Errorf("hydrate dispatch: %v", err)
	}
	if err := Dispatch(context.Background(), &bytes.Buffer{}, "bogus", specPath); err == nil {
		t.Error("expected error for unknown role")
	}

	// Sidecar role stops when ctx is canceled.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Dispatch(ctx, &bytes.Buffer{}, RoleGateway, specPath); err != nil {
		t.Errorf("gateway dispatch: %v", err)
	}
}
