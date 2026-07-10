package cli

import (
	"bytes"
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
	want := []string{"run", "project", "mcp", "fleet", "usage", "login", "version"}
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

func TestPlaceholderReportsMilestone(t *testing.T) {
	_, err := run(t, "fleet")
	if err == nil || !strings.Contains(err.Error(), "M1") {
		t.Fatalf("expected fleet placeholder M1 error, got %v", err)
	}
	_, err = run(t, "mcp", "add")
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("expected mcp add placeholder, got %v", err)
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

func TestRunLogsPlaceholder(t *testing.T) {
	_, err := run(t, "run", "logs", "r-1")
	if err == nil || !strings.Contains(err.Error(), "not implemented") {
		t.Fatalf("expected run logs placeholder, got %v", err)
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
