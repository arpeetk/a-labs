package podruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/summiteight/wren/internal/blob"
	"github.com/summiteight/wren/internal/egress"
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

func TestRunHarnessMirrorsEventsToGatewayFile(t *testing.T) {
	ws := t.TempDir()
	specPath := writeRunSpec(t, t.TempDir(), runspec.RunSpec{RunID: "r-events", Harness: "mock", Prompt: "x", WorkspacePath: ws})
	eventPath := t.TempDir() + "/events.jsonl"
	t.Setenv("WREN_EVENT_FILE", eventPath)
	var stdout bytes.Buffer
	if err := RunHarness(context.Background(), &stdout, specPath); err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile(eventPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(events, stdout.Bytes()) {
		t.Fatalf("gateway event file differs from stdout\nfile=%s\nout=%s", events, stdout.String())
	}
}

func TestRunGatewayForwardsStableAttemptSequence(t *testing.T) {
	eventPath := t.TempDir() + "/events.jsonl"
	contents := `{"type":"status","time":"2026-08-11T10:00:00Z","phase":"running"}
{"type":"tool_call","time":"2026-08-11T10:00:01Z","tool":"go test"}
`
	if err := os.WriteFile(eventPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	received := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/internal/runs/r-bridge/events" {
			t.Errorf("path = %q", r.URL.Path)
		}
		var body struct {
			SourceID string          `json:"sourceId"`
			Type     string          `json:"type"`
			Payload  json.RawMessage `json:"payload"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		if len(body.Payload) == 0 {
			t.Error("missing original event payload")
		}
		received <- body.SourceID + ":" + body.Type
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	t.Setenv("WREN_EVENT_FILE", eventPath)
	t.Setenv("WREN_GATEWAY_URL", server.URL)
	t.Setenv("WREN_RUN_ID", "r-bridge")
	t.Setenv("WREN_ATTEMPT", "3")
	oldPoll := gatewayPollInterval
	gatewayPollInterval = time.Millisecond
	defer func() { gatewayPollInterval = oldPoll }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunGateway(ctx, io.Discard) }()
	for _, want := range []string{"attempt-3/1:status", "attempt-3/2:tool_call"} {
		select {
		case got := <-received:
			if got != want {
				t.Errorf("gateway event = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s", want)
		}
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("RunGateway: %v", err)
	}
}

func TestRunGatewayTailsEventsAppendedAfterOpen(t *testing.T) {
	eventPath := t.TempDir() + "/events.jsonl"
	if err := os.WriteFile(eventPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	received := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		received <- struct{}{}
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	t.Setenv("WREN_EVENT_FILE", eventPath)
	t.Setenv("WREN_GATEWAY_URL", server.URL)
	t.Setenv("WREN_RUN_ID", "r-tail")
	oldPoll := gatewayPollInterval
	gatewayPollInterval = time.Millisecond
	defer func() { gatewayPollInterval = oldPoll }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunGateway(ctx, io.Discard) }()
	time.Sleep(10 * time.Millisecond)
	f, err := os.OpenFile(eventPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"type":"status","time":"2026-08-11T10:00:00Z","phase":"running"}` + "\n"); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	select {
	case <-received:
		cancel()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("gateway did not observe an event appended after initial EOF")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRunGatewayThroughTrustedProxyInjectsIdentity(t *testing.T) {
	eventPath := t.TempDir() + "/events.jsonl"
	if err := os.WriteFile(eventPath, []byte(`{"type":"status","time":"2026-08-11T10:00:00Z","phase":"running"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	received := make(chan struct{}, 1)
	controlPlane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/internal/runs/r-proxied/events" {
			t.Errorf("upstream path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer bridge-secret" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("X-Wren-Run-ID"); got != "r-proxied" {
			t.Errorf("run identity = %q", got)
		}
		received <- struct{}{}
		w.WriteHeader(http.StatusCreated)
	}))
	defer controlPlane.Close()
	t.Setenv("WREN_CONTROL_PLANE_UPSTREAM", controlPlane.URL)
	t.Setenv("WREN_GATEWAY_TOKEN", "bridge-secret")
	t.Setenv("WREN_RUN_ID", "r-proxied")
	proxy, err := egress.New(egressConfigFromEnv())
	if err != nil {
		t.Fatal(err)
	}
	proxyServer := httptest.NewServer(proxy)
	defer proxyServer.Close()
	t.Setenv("WREN_EVENT_FILE", eventPath)
	t.Setenv("WREN_GATEWAY_URL", proxyServer.URL+"/control-plane")
	t.Setenv("WREN_ATTEMPT", "1")
	oldPoll := gatewayPollInterval
	gatewayPollInterval = time.Millisecond
	defer func() { gatewayPollInterval = oldPoll }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunGateway(ctx, io.Discard) }()
	select {
	case <-received:
		cancel()
	case <-time.After(time.Second):
		cancel()
		t.Fatal("gateway event did not cross the trusted proxy")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
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

// TestRunHydrate_RestoreRequired_NoCheckpoints: a freshly-recreated, empty PVC
// with no checkpoint ever taken must fail deterministically — not retryable,
// not a silent empty-workspace resume.
func TestRunHydrate_RestoreRequired_NoCheckpoints(t *testing.T) {
	mount := t.TempDir()
	t.Setenv("WREN_CHECKPOINT_MOUNT_PATH", mount)
	ws := t.TempDir()

	specPath := writeRunSpec(t, t.TempDir(), runspec.RunSpec{
		RunID: "r-norestore", Mode: runspec.ModeResume, RestoreRequired: true,
		CheckpointBucket: "gs://some-bucket", WorkspacePath: ws,
	})
	var buf bytes.Buffer
	err := RunHydrate(context.Background(), &buf, specPath)
	if err == nil {
		t.Fatal("expected an error when restore is required but no checkpoint exists")
	}
	if errors.Is(err, ErrRetryable) {
		t.Fatalf("expected a plain (non-retryable) error, got ErrRetryable-wrapped: %v", err)
	}
}

// TestRunHydrate_RestoreRequired_PicksLatest: with multiple checkpoints
// present, hydrate restores the one with the latest Modified time, not
// necessarily the lexicographically-last key.
func TestRunHydrate_RestoreRequired_PicksLatest(t *testing.T) {
	mount := t.TempDir()
	bucket := "gs://some-bucket"
	runID := "r-restore"
	store := blob.NewMountStore(mount, blob.RunPrefix(bucket, runID))

	// Archive two tiny source trees and Put them under checkpoints/, forcing
	// distinct mtimes so "latest" is unambiguous.
	putCheckpoint := func(t *testing.T, key, marker string, mtime time.Time) {
		t.Helper()
		src := t.TempDir()
		if err := os.WriteFile(filepath.Join(src, "marker.txt"), []byte(marker), 0o644); err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := blob.Archive(&buf, src); err != nil {
			t.Fatal(err)
		}
		if err := store.Put(context.Background(), key, &buf); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Join(mount, "runs", runID, key), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)
	putCheckpoint(t, "checkpoints/ck-000001.tar.gz", "old\n", base)
	putCheckpoint(t, "checkpoints/ck-000002.tar.gz", "newest\n", base.Add(30*time.Minute))

	t.Setenv("WREN_CHECKPOINT_MOUNT_PATH", mount)
	ws := t.TempDir()
	specPath := writeRunSpec(t, t.TempDir(), runspec.RunSpec{
		RunID: runID, Mode: runspec.ModeResume, RestoreRequired: true,
		CheckpointBucket: bucket, WorkspacePath: ws,
	})

	var buf bytes.Buffer
	if err := RunHydrate(context.Background(), &buf, specPath); err != nil {
		t.Fatalf("RunHydrate: %v (log: %s)", err, buf.String())
	}
	if !strings.Contains(buf.String(), "restore-from-checkpoint PASSED") {
		t.Errorf("expected PASSED restore log line, got: %s", buf.String())
	}
	got, err := os.ReadFile(filepath.Join(ws, "marker.txt"))
	if err != nil {
		t.Fatalf("read restored marker: %v", err)
	}
	if string(got) != "newest\n" {
		t.Errorf("restored wrong checkpoint: got marker %q, want %q", got, "newest\n")
	}
}

// TestRunHydrate_ResumeNoRestore_Unchanged: the ordinary (pre-WS-21)
// crash-resume path where the PVC survived stays a no-op, even with a
// checkpoint mount configured — restoring into a non-empty workspace would be
// wrong (it's not what RestoreRequired=false means).
func TestRunHydrate_ResumeNoRestore_Unchanged(t *testing.T) {
	t.Setenv("WREN_CHECKPOINT_MOUNT_PATH", t.TempDir())
	specPath := writeRunSpec(t, t.TempDir(), runspec.RunSpec{
		RunID: "r-plain-resume", Mode: runspec.ModeResume, RestoreRequired: false,
		CheckpointBucket: "gs://some-bucket",
	})
	var buf bytes.Buffer
	if err := RunHydrate(context.Background(), &buf, specPath); err != nil {
		t.Fatalf("RunHydrate: %v", err)
	}
	if !strings.Contains(buf.String(), "no restore needed") {
		t.Errorf("expected no-op resume message, got: %s", buf.String())
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

func TestPrepareHarnessState(t *testing.T) {
	for _, tc := range []struct {
		name, repo, want string
	}{
		{"repository", "owner/repo", filepath.Join(".git", "wren", "codex")},
		{"repo-less", "", filepath.Join(".wren", "codex")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			spec := runspec.RunSpec{Harness: "codex", Repo: tc.repo, WorkspacePath: ws}
			if err := prepareHarnessState(spec); err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(filepath.Join(ws, tc.want))
			if err != nil || !info.IsDir() {
				t.Fatalf("Codex state dir missing: info=%v err=%v", info, err)
			}
		})
	}
	ws := t.TempDir()
	if err := prepareHarnessState(runspec.RunSpec{Harness: "mock", WorkspacePath: ws}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(ws)
	if err != nil || len(entries) != 0 {
		t.Fatalf("non-Codex hydrate mutated workspace: entries=%v err=%v", entries, err)
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
		manifests := 0
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".json") {
				manifests++
			}
		}
		if manifests >= 2 {
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
	manifests := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			manifests++
		}
	}
	if manifests < 2 {
		t.Fatalf("expected at least 2 published checkpoint manifests, found %d", manifests)
	}
	objects, err := os.ReadDir(filepath.Join(ckptDir, "objects"))
	if err != nil || len(objects) < manifests {
		t.Fatalf("immutable checkpoint archives = %d, manifests = %d, err=%v", len(objects), manifests, err)
	}
	for _, e := range objects {
		if !strings.HasSuffix(e.Name(), ".tar.gz") {
			t.Errorf("archive %q does not have .tar.gz suffix", e.Name())
		}
	}
	logSoFar := buf.String()
	if !strings.Contains(logSoFar, "checkpoint snapshot PASSED") {
		t.Errorf("expected PASSED snapshot log line, got:\n%s", logSoFar)
	}
	if !strings.Contains(logSoFar, `"type":"checkpoint_ready"`) || !strings.Contains(logSoFar, `"sha256"`) {
		t.Errorf("periodic checkpoint proof event missing:\n%s", logSoFar)
	}
}

func TestRunCheckpointOncePublishesPauseManifest(t *testing.T) {
	ws := t.TempDir()
	if err := os.WriteFile(filepath.Join(ws, "accepted.txt"), []byte("yes"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := checkpointWorkspacePath
	checkpointWorkspacePath = ws
	t.Cleanup(func() { checkpointWorkspacePath = orig })
	mount := t.TempDir()
	t.Setenv("WREN_CHECKPOINT_BUCKET", "gs://bucket")
	t.Setenv("WREN_CHECKPOINT_MOUNT_PATH", mount)
	t.Setenv("WREN_RUN_ID", "r-pause")
	t.Setenv("WREN_CHECKPOINT_RETAIN", "3")
	var out bytes.Buffer
	if err := RunCheckpointOnce(context.Background(), &out); err != nil {
		t.Fatalf("RunCheckpointOnce: %v", err)
	}
	var m blob.CheckpointManifest
	if err := json.Unmarshal(out.Bytes(), &m); err != nil {
		t.Fatalf("manifest output: %v (%q)", err, out.String())
	}
	if m.Trigger != "pause" || m.RunID != "r-pause" || m.SHA256 == "" {
		t.Fatalf("manifest = %+v", m)
	}
}

func TestDispatchQuiesceAndUnquiesceSignalHarnessPID1(t *testing.T) {
	orig := signalProcess
	t.Cleanup(func() { signalProcess = orig })
	var got []syscall.Signal
	signalProcess = func(pid int, sig syscall.Signal) error {
		if pid != 1 {
			t.Fatalf("pid=%d, want 1", pid)
		}
		got = append(got, sig)
		return nil
	}
	if err := Dispatch(context.Background(), io.Discard, RoleQuiesce, ""); err != nil {
		t.Fatal(err)
	}
	if err := Dispatch(context.Background(), io.Discard, RoleUnquiesce, ""); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != syscall.SIGSTOP || got[1] != syscall.SIGCONT {
		t.Fatalf("signals = %v", got)
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
