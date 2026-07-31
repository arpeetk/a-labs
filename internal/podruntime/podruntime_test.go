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

// runCheckpointerUntil starts RunCheckpointer, waits for the self-check to run,
// then cancels and waits for a clean stop before reading the log. Reading buf
// only after RunCheckpointer has actually returned (not just after the sleep)
// matters: RunCheckpointer's sidecar-liveness loop keeps running — and keeps
// writing to buf — until it observes ctx cancellation, so reading buf.String()
// on the test goroutine before that return is a data race with whatever the
// background goroutine is still writing (caught live by `go test -race` in CI
// on the original version of this test — race detected, not hypothetical).
// Once <-done has fired, RunCheckpointer has returned and nothing else can
// write to buf, so the read below is race-free by construction.
func runCheckpointerUntil(t *testing.T) func() string {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- RunCheckpointer(ctx, &buf, "checkpointer") }()
	time.Sleep(50 * time.Millisecond) // let the one-shot self-check complete
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunCheckpointer returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("checkpointer did not stop on cancel")
	}
	logSoFar := buf.String()
	return func() string { return logSoFar }
}

// TestRunCheckpointer_SelfCheckPasses: with a bucket + a writable mount path,
// the startup self-check writes/reads/lists the object and logs PASSED, and the
// object really lands under the mount on disk.
func TestRunCheckpointer_SelfCheckPasses(t *testing.T) {
	mount := t.TempDir()
	t.Setenv("WREN_CHECKPOINT_BUCKET", "gs://some-bucket")
	t.Setenv("WREN_CHECKPOINT_MOUNT_PATH", mount)
	t.Setenv("WREN_RUN_ID", "r-selfcheck")

	logf := runCheckpointerUntil(t)
	if !strings.Contains(logf(), "mount self-check PASSED") {
		t.Errorf("expected PASSED self-check, got log:\n%s", logf())
	}
	// Scoped under the run's own prefix (blob.RunPrefix) — not directly under
	// the mount root — so two runs sharing a bucket can't see each other's keys.
	if _, err := os.Stat(filepath.Join(mount, "runs", "r-selfcheck", "_wren-mount-check", "r-selfcheck.txt")); err != nil {
		t.Errorf("self-check object not written to the run-scoped mount path: %v", err)
	}
}

// TestRunCheckpointer_NoMountConfigured: without the mount env, the checkpointer
// is the plain liveness stub — no self-check runs, behavior is unchanged.
func TestRunCheckpointer_NoMountConfigured(t *testing.T) {
	// Ensure a stale env from the process does not leak in.
	t.Setenv("WREN_CHECKPOINT_BUCKET", "")
	t.Setenv("WREN_CHECKPOINT_MOUNT_PATH", "")

	logf := runCheckpointerUntil(t)
	if strings.Contains(logf(), "self-check") {
		t.Errorf("self-check ran with no mount configured: %s", logf())
	}
	if !strings.Contains(logf(), "checkpointer") {
		t.Errorf("expected plain sidecar liveness output, got: %s", logf())
	}
}

// TestRunCheckpointer_SelfCheckFailsNonFatal: a broken mount path makes the
// self-check fail, but the sidecar logs FAILED and keeps running (it must not
// crash-loop the pod for an experimental feature).
func TestRunCheckpointer_SelfCheckFailsNonFatal(t *testing.T) {
	// Point the mount path at a regular file so MkdirAll under it fails.
	f := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(f, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("WREN_CHECKPOINT_BUCKET", "gs://some-bucket")
	t.Setenv("WREN_CHECKPOINT_MOUNT_PATH", f)
	t.Setenv("WREN_RUN_ID", "r-fail")

	logf := runCheckpointerUntil(t)
	if !strings.Contains(logf(), "mount self-check FAILED") {
		t.Errorf("expected FAILED self-check, got: %s", logf())
	}
}

// TestRunCheckpointer_PeriodicSnapshots: with a short tick interval, the
// checkpointer takes multiple real snapshots of the workspace and Puts each
// one to the mount as a distinct object — not just the WS-18 self-check.
func TestRunCheckpointer_PeriodicSnapshots(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "marker.txt"), []byte("hello workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	origWorkspace := checkpointWorkspacePath
	checkpointWorkspacePath = ws
	t.Cleanup(func() { checkpointWorkspacePath = origWorkspace })

	origUnit := checkpointTickUnit
	checkpointTickUnit = time.Millisecond
	t.Cleanup(func() { checkpointTickUnit = origUnit })

	mount := t.TempDir()
	t.Setenv("WREN_CHECKPOINT_BUCKET", "gs://some-bucket")
	t.Setenv("WREN_CHECKPOINT_MOUNT_PATH", mount)
	t.Setenv("WREN_CHECKPOINT_INTERVAL", "5") // 5ms with the millisecond unit above
	t.Setenv("WREN_RUN_ID", "r-periodic")

	ctx, cancel := context.WithCancel(context.Background())
	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- RunCheckpointer(ctx, &buf, "checkpointer") }()

	ckptDir := filepath.Join(mount, "runs", "r-periodic", "checkpoints")
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, _ := os.ReadDir(ckptDir)
		if len(entries) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("expected at least 2 checkpoint objects, found %d under %s (log:\n%s)", len(entries), ckptDir, buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunCheckpointer returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("checkpointer did not stop on cancel")
	}

	entries, err := os.ReadDir(ckptDir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 checkpoint objects, found %d", len(entries))
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".tar.gz") {
			t.Errorf("checkpoint object %q does not have .tar.gz suffix", e.Name())
		}
	}
	logSoFar := buf.String()
	if !strings.Contains(logSoFar, "checkpoint snapshot PASSED") {
		t.Errorf("expected PASSED snapshot log line, got:\n%s", logSoFar)
	}
}

// TestRunCheckpointer_SnapshotFailureNonFatal: when Archive fails (workspace
// path missing), the loop logs FAILED and keeps ticking rather than crashing
// the sidecar — mirroring the self-check's non-fatal posture.
func TestRunCheckpointer_SnapshotFailureNonFatal(t *testing.T) {
	origWorkspace := checkpointWorkspacePath
	checkpointWorkspacePath = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { checkpointWorkspacePath = origWorkspace })

	origUnit := checkpointTickUnit
	checkpointTickUnit = time.Millisecond
	t.Cleanup(func() { checkpointTickUnit = origUnit })

	mount := t.TempDir()
	t.Setenv("WREN_CHECKPOINT_BUCKET", "gs://some-bucket")
	t.Setenv("WREN_CHECKPOINT_MOUNT_PATH", mount)
	t.Setenv("WREN_CHECKPOINT_INTERVAL", "5")
	t.Setenv("WREN_RUN_ID", "r-snapfail")

	ctx, cancel := context.WithCancel(context.Background())
	var buf bytes.Buffer
	done := make(chan error, 1)
	go func() { done <- RunCheckpointer(ctx, &buf, "checkpointer") }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunCheckpointer returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("checkpointer did not stop on cancel")
	}
	if !strings.Contains(buf.String(), "checkpoint snapshot FAILED") {
		t.Errorf("expected FAILED snapshot log line, got:\n%s", buf.String())
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
