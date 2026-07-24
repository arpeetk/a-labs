package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// projectServer stubs POST/GET /v1/projects and records the create body.
func projectServer(t *testing.T, gotBody *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects":
			*gotBody = nil
			_ = json.NewDecoder(r.Body).Decode(gotBody)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"name":"demo","repo":"acme/api","defaultHarness":"mock","harnessImage":"wren/runtime:dev","cpu":"100m"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			_, _ = w.Write([]byte(`[
			  {"name":"demo","repo":"acme/api","defaultHarness":"claude-code","harnessImage":"reg/wren/runtime:abc","cpu":"1","memory":"2Gi","disk":"5Gi"},
			  {"name":"keyless","defaultHarness":"mock"}
			]`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestProjectCreateSendsRequest(t *testing.T) {
	var gotBody map[string]any
	srv := projectServer(t, &gotBody)
	defer srv.Close()

	dir := t.TempDir()
	if _, err := execIn(t, dir, "login", "--control-plane", srv.URL, "--user", "me"); err != nil {
		t.Fatal(err)
	}
	out, err := execIn(t, dir, "project", "create", "demo",
		"--repo", "acme/api", "--harness", "mock", "--harness-image", "wren/runtime:dev", "--cpu", "100m")
	if err != nil {
		t.Fatal(err)
	}
	if gotBody["name"] != "demo" || gotBody["repo"] != "acme/api" ||
		gotBody["defaultHarness"] != "mock" || gotBody["harnessImage"] != "wren/runtime:dev" ||
		gotBody["cpu"] != "100m" {
		t.Errorf("create body = %v", gotBody)
	}
	// Unset flags are omitted so the control plane's defaults apply.
	for _, k := range []string{"memory", "disk", "model", "namespace"} {
		if _, ok := gotBody[k]; ok {
			t.Errorf("%s should be omitted when unset, body: %v", k, gotBody)
		}
	}
	if !strings.Contains(out, "\"name\": \"demo\"") {
		t.Errorf("create output = %q", out)
	}
}

func TestProjectCreateRequiresName(t *testing.T) {
	_, err := run(t, "project", "create")
	if err == nil {
		t.Fatal("expected usage error without a name")
	}
}

func TestProjectListTable(t *testing.T) {
	var unused map[string]any
	srv := projectServer(t, &unused)
	defer srv.Close()

	dir := t.TempDir()
	if _, err := execIn(t, dir, "login", "--control-plane", srv.URL, "--user", "me"); err != nil {
		t.Fatal(err)
	}
	out, err := execIn(t, dir, "project", "list")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NAME", "REPO", "HARNESS", "demo", "acme/api", "claude-code", "keyless", "mock"} {
		if !strings.Contains(out, want) {
			t.Errorf("list output missing %q:\n%s", want, out)
		}
	}
	// Empty optional cells render as "-" (the keyless row has no repo/image).
	if !strings.Contains(out, "-") {
		t.Errorf("expected dashed empty cells:\n%s", out)
	}
}

func TestProjectListNeedsLogin(t *testing.T) {
	_, err := run(t, "project", "list")
	if err == nil || !strings.Contains(err.Error(), "control plane") {
		t.Fatalf("expected no-context error, got %v", err)
	}
}

func TestRootHasInstallCommands(t *testing.T) {
	root := NewRootCommand()
	have := map[string]bool{}
	for _, c := range root.Commands() {
		have[c.Name()] = true
	}
	for _, w := range []string{"install", "uninstall"} {
		if !have[w] {
			t.Errorf("missing subcommand %q", w)
		}
	}
}

// TestInstallHasHarnessImagesFlag guards the CLI wiring for WS-14: `wren
// install` must expose a way to restrict/skip the harness images it builds
// (install.Options.HarnessImages), not just the 3 control-plane images.
func TestInstallHasHarnessImagesFlag(t *testing.T) {
	root := NewRootCommand()
	for _, c := range root.Commands() {
		if c.Name() != "install" {
			continue
		}
		if c.Flags().Lookup("harness-images") == nil {
			t.Error("install command missing --harness-images flag")
		}
		return
	}
	t.Fatal("install subcommand not found")
}

func TestUninstallRequiresConfirm(t *testing.T) {
	_, err := run(t, "uninstall")
	if err == nil || !strings.Contains(err.Error(), "--confirm") {
		t.Fatalf("expected --confirm gate, got %v", err)
	}
}
