package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/summiteight/wren/internal/config"
)

// run executes the root command with args against an isolated config dir and
// returns combined stdout/stderr and the error.
func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("WREN_CONFIG_DIR", t.TempDir())
	root := NewRootCommand()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

func TestVersionCommand(t *testing.T) {
	out, err := run(t, "version")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "wren ") {
		t.Errorf("version output = %q", out)
	}
}

func TestRootHasExpectedSubcommands(t *testing.T) {
	root := NewRootCommand()
	want := []string{"run", "project", "login", "version", "install", "uninstall"}
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("missing subcommand %q", w)
		}
	}
}

func TestRunCreateRequiresProject(t *testing.T) {
	_, err := run(t, "login", "--control-plane", "x:443")
	if err != nil {
		t.Fatal(err)
	}
	_, err = run(t, "run", "create", "--task", "do it")
	if err == nil || !strings.Contains(err.Error(), "--project") {
		t.Fatalf("expected --project required error, got %v", err)
	}
}

func TestRunCreateRequiresTask(t *testing.T) {
	out, err := run(t, "run", "create", "--project", "p")
	_ = out
	if err == nil || !strings.Contains(err.Error(), "--task") {
		t.Fatalf("expected --task required error, got %v", err)
	}
}

func TestRunCreateNeedsLogin(t *testing.T) {
	// Valid flags but no context configured → clientFromFlags fails.
	_, err := run(t, "run", "create", "--project", "p", "--task", "t")
	if err == nil || !strings.Contains(err.Error(), "control plane") {
		t.Fatalf("expected no-context error, got %v", err)
	}
}

func TestLoginWritesContext(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("WREN_CONFIG_DIR", dir)

	root := NewRootCommand()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs([]string{"login", "--control-plane", "host:443", "--org", "acme"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "Saved context") {
		t.Errorf("login output = %q", buf.String())
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	cctx, err := cfg.Resolve("")
	if err != nil {
		t.Fatalf("resolve after login: %v", err)
	}
	if cctx.Server != "host:443" || cctx.Name != "acme" {
		t.Errorf("saved context = %+v", cctx)
	}
}

func TestLoginRequiresServer(t *testing.T) {
	_, err := run(t, "login")
	if err == nil || !strings.Contains(err.Error(), "control-plane") {
		t.Fatalf("expected --control-plane required, got %v", err)
	}
}

// TestNoNotImplementedCommands guards WS-15 Part C: the CLI ships no command
// that exists only to error with "not implemented yet". Removed roadmap
// commands (fleet, mcp, usage) are simply absent, so cobra reports them as
// unknown rather than running a placeholder.
func TestNoNotImplementedCommands(t *testing.T) {
	for _, name := range []string{"fleet", "usage"} {
		_, err := run(t, name)
		if err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("`wren %s` should be an unknown command, got %v", name, err)
		}
	}
}

// TestRunCreateHarnessDefaultsToProject guards a regression where the CLI's
// --harness flag defaulted to "claude-code" and thus always overrode a project's
// configured default. Unset, it must be omitted so the project default applies.
func TestRunCreateHarnessDefaultsToProject(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/runs" {
			gotBody = nil
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"r-1","phase":"Pending"}`))
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	if _, err := execIn(t, dir, "login", "--control-plane", srv.URL, "--user", "me"); err != nil {
		t.Fatal(err)
	}
	// No --harness → omitted from the request (project default applies).
	if _, err := execIn(t, dir, "run", "create", "--project", "p", "--task", "t"); err != nil {
		t.Fatal(err)
	}
	if h, ok := gotBody["harness"]; ok && h != "" {
		t.Errorf("harness should be omitted when --harness unset, got %v", h)
	}
	// Explicit override is sent through.
	if _, err := execIn(t, dir, "run", "create", "--project", "p", "--task", "t", "--harness", "codex"); err != nil {
		t.Fatal(err)
	}
	if gotBody["harness"] != "codex" {
		t.Errorf("explicit --harness override not sent, got %v", gotBody["harness"])
	}
}

// execIn runs args against a shared config dir (so login persists for later commands).
func execIn(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	t.Setenv("WREN_CONFIG_DIR", dir)
	root := NewRootCommand()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute() // execute before reading buf
	return buf.String(), err
}

func TestRunCommandsHitControlPlane(t *testing.T) {
	// A real (test) control plane the CLI talks to over HTTP.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"r-xyz","project":"p","phase":"Pending"}`))
		case r.URL.Path == "/v1/runs/r-xyz":
			_, _ = w.Write([]byte(`{"id":"r-xyz","phase":"Running"}`))
		case r.URL.Path == "/v1/runs":
			_, _ = w.Write([]byte(`[{"id":"r-xyz","phase":"Running"}]`))
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	if _, err := execIn(t, dir, "login", "--control-plane", srv.URL, "--user", "me@x"); err != nil {
		t.Fatal(err)
	}

	out, err := execIn(t, dir, "run", "create", "--project", "p", "--task", "do it")
	if err != nil {
		t.Fatalf("run create: %v", err)
	}
	if !strings.Contains(out, "r-xyz") {
		t.Errorf("create output = %q", out)
	}
	if out, err := execIn(t, dir, "run", "list"); err != nil || !strings.Contains(out, "r-xyz") {
		t.Fatalf("run list = %q, %v", out, err)
	}
	if out, err := execIn(t, dir, "run", "get", "r-xyz"); err != nil || !strings.Contains(out, "Running") {
		t.Fatalf("run get = %q, %v", out, err)
	}
}

// TestRunStopAndRm covers the WS-15 Part C commands hitting the control plane.
func TestRunStopAndRm(t *testing.T) {
	var stopHit, delHit bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/r-1/stop":
			stopHit = true
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/runs/r-1":
			delHit = true
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	if _, err := execIn(t, dir, "login", "--control-plane", srv.URL, "--user", "me"); err != nil {
		t.Fatal(err)
	}
	if out, err := execIn(t, dir, "run", "stop", "r-1"); err != nil || !strings.Contains(out, "stopping") {
		t.Fatalf("run stop = %q, %v", out, err)
	}
	if !stopHit {
		t.Error("run stop did not POST /stop")
	}
	if out, err := execIn(t, dir, "run", "rm", "r-1"); err != nil || !strings.Contains(out, "deleted") {
		t.Fatalf("run rm = %q, %v", out, err)
	}
	if !delHit {
		t.Error("run rm did not DELETE the run")
	}
}

// TestRunCreateRejectsNonRuncRuntime is WS-15 Part C: --runtime gvisor|kata is
// rejected client-side with an M4 pointer instead of a confusing pod-admission
// failure downstream.
func TestRunCreateRejectsNonRuncRuntime(t *testing.T) {
	dir := t.TempDir()
	// No server needed: validation fails before any request.
	_, err := execIn(t, dir, "run", "create", "--project", "p", "--task", "t", "--runtime", "gvisor")
	if err == nil || !strings.Contains(err.Error(), "M4") {
		t.Fatalf("expected M4 rejection for --runtime gvisor, got %v", err)
	}
}

// TestProjectGetHitsControlPlane covers the real `wren project get`.
func TestProjectGetHitsControlPlane(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/projects/payments" {
			_, _ = w.Write([]byte(`{"name":"payments","repo":"acme/payments"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	dir := t.TempDir()
	if _, err := execIn(t, dir, "login", "--control-plane", srv.URL, "--user", "me"); err != nil {
		t.Fatal(err)
	}
	out, err := execIn(t, dir, "project", "get", "payments")
	if err != nil || !strings.Contains(out, "acme/payments") {
		t.Fatalf("project get = %q, %v", out, err)
	}
}

func TestRunLogsStreamsToStdout(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("event: started\nevent: done\n"))
	}))
	defer srv.Close()

	dir := t.TempDir()
	if _, err := execIn(t, dir, "login", "--control-plane", srv.URL, "--user", "me"); err != nil {
		t.Fatal(err)
	}
	out, err := execIn(t, dir, "run", "logs", "-f", "r-1", "--container", "harness")
	if err != nil {
		t.Fatalf("run logs: %v", err)
	}
	if !strings.Contains(out, "event: done") {
		t.Errorf("logs output = %q", out)
	}
	if !strings.Contains(gotPath, "/v1/runs/r-1/logs") || !strings.Contains(gotPath, "follow=true") || !strings.Contains(gotPath, "container=harness") {
		t.Errorf("request path = %q", gotPath)
	}
}

// TestRunLogsNonZeroOn4xx: a 409 from the control plane surfaces as a non-nil
// error (the CLI exits non-zero) and the error message carries the hint.
func TestRunLogsNonZeroOn4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"run is Pending: no pod yet"}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	if _, err := execIn(t, dir, "login", "--control-plane", srv.URL, "--user", "me"); err != nil {
		t.Fatal(err)
	}
	_, err := execIn(t, dir, "run", "logs", "r-1")
	if err == nil || !strings.Contains(err.Error(), "Pending") {
		t.Fatalf("expected Pending conflict error, got %v", err)
	}
}

func TestEmitJSON(t *testing.T) {
	cmd := &cobra.Command{}
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	if err := emit(cmd, map[string]string{"a": "b"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "\"a\": \"b\"") {
		t.Errorf("emit output = %q", buf.String())
	}
}
