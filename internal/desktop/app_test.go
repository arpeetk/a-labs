package desktop

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/summiteight/wren/internal/client"
	"github.com/summiteight/wren/internal/config"
)

func TestLoadWithoutContextStartsDisconnected(t *testing.T) {
	t.Setenv("WREN_CONFIG_DIR", t.TempDir())
	got, err := New().Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Contexts) != 0 || len(got.Projects) != 0 || len(got.Runs) != 0 {
		t.Fatalf("disconnected bootstrap = %+v", got)
	}
}

func TestLogStreamBridgesChunksAndCompletion(t *testing.T) {
	t.Setenv("WREN_CONFIG_DIR", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/r-1/logs" || r.URL.Query().Get("follow") != "true" || r.URL.Query().Get("container") != "harness" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, "first\nsecond\n")
	}))
	defer srv.Close()
	if err := (&config.Config{CurrentContext: "test", Contexts: []config.Context{{Name: "test", Server: srv.URL}}}).Save(); err != nil {
		t.Fatal(err)
	}

	events := make(chan LogEvent, 4)
	app := New()
	app.emit = func(_ context.Context, name string, args ...any) {
		if name != logEventName {
			t.Errorf("event name = %q", name)
		}
		events <- args[0].(LogEvent)
	}
	app.Startup(context.Background())
	streamID, err := app.StartLogStream("r-1", "harness")
	if err != nil {
		t.Fatal(err)
	}
	var output string
	for {
		select {
		case event := <-events:
			if event.StreamID != streamID {
				t.Fatalf("stream ID = %q, want %q", event.StreamID, streamID)
			}
			output += event.Chunk
			if event.Done {
				if event.Error != "" || output != "first\nsecond\n" {
					t.Fatalf("completion = %+v, output = %q", event, output)
				}
				return
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for log completion")
		}
	}
}

func TestDesktopManagementUsesRealControlPlaneAPI(t *testing.T) {
	t.Setenv("WREN_CONFIG_DIR", t.TempDir())
	var actions []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Wren-User"); got != "dev@example.com" {
			t.Errorf("X-Wren-User = %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			fmt.Fprint(w, `[{"name":"payments","repo":"acme/payments","defaultHarness":"codex"}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs":
			fmt.Fprint(w, `[{"id":"r-old","project":"payments","phase":"Succeeded","createdAt":"2026-08-08T20:00:00Z"},{"id":"r-1","project":"payments","phase":"Running","createdAt":"2026-08-09T20:00:00Z"}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/r-1":
			fmt.Fprint(w, `{"id":"r-1","project":"payments","phase":"Running"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"task":"fix checkout"`) {
				t.Errorf("create body = %s", body)
			}
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"id":"r-2","project":"payments","phase":"Pending"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/r-1/stop":
			actions = append(actions, "stop")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/r-1/pause":
			actions = append(actions, "pause")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/runs/r-1/resume":
			actions = append(actions, "resume")
			w.WriteHeader(http.StatusAccepted)
		case r.Method == http.MethodDelete && r.URL.Path == "/v1/runs/r-1":
			actions = append(actions, "delete")
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects":
			w.WriteHeader(http.StatusCreated)
			fmt.Fprint(w, `{"name":"checkout","repo":"acme/checkout"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/runs/r-1/logs":
			w.Header().Set("Content-Type", "text/plain")
			fmt.Fprint(w, "agent output\n")
		default:
			http.Error(w, "unexpected request", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{CurrentContext: "test", Contexts: []config.Context{{Name: "test", Server: srv.URL, User: "dev@example.com"}}}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	app := New()
	boot, err := app.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(boot.Projects) != 1 || len(boot.Runs) != 2 || boot.Runs[0].ID != "r-1" || !boot.Contexts[0].Selected {
		t.Fatalf("bootstrap = %+v", boot)
	}
	filtered, err := app.ListRuns("all", "payments", "Running")
	if err != nil || len(filtered) != 1 || filtered[0].ID != "r-1" {
		t.Fatalf("ListRuns = %+v, %v", filtered, err)
	}
	got, err := app.GetRun("r-1")
	if err != nil || got.ID != "r-1" {
		t.Fatalf("GetRun = %+v, %v", got, err)
	}
	run, err := app.CreateRun(client.RunCreateOptions{Project: "payments", Task: "fix checkout"})
	if err != nil || run.ID != "r-2" {
		t.Fatalf("CreateRun = %+v, %v", run, err)
	}
	if err := app.StopRun("r-1"); err != nil {
		t.Fatal(err)
	}
	if err := app.PauseRun("r-1"); err != nil {
		t.Fatal(err)
	}
	if err := app.ResumeRun("r-1"); err != nil {
		t.Fatal(err)
	}
	if err := app.DeleteRun("r-1"); err != nil {
		t.Fatal(err)
	}
	projects, err := app.ListProjects()
	if err != nil || len(projects) != 1 {
		t.Fatalf("ListProjects = %+v, %v", projects, err)
	}
	project, err := app.CreateProject(client.Project{Name: "checkout", Repo: "acme/checkout"})
	if err != nil || project.Name != "checkout" {
		t.Fatalf("CreateProject = %+v, %v", project, err)
	}
	logs, err := app.Logs("r-1", "harness")
	if err != nil || logs != "agent output\n" {
		t.Fatalf("Logs = %q, %v", logs, err)
	}
	if strings.Join(actions, ",") != "stop,pause,resume,delete" {
		t.Fatalf("actions = %v", actions)
	}
	if _, err := app.SelectContext("test"); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SaveContext(config.Context{Name: "test", Server: srv.URL, User: "dev@example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := app.SaveContext(config.Context{Name: "", Server: srv.URL}); err == nil {
		t.Fatal("SaveContext accepted a blank name")
	}
}

func TestShutdownCancelsStreams(t *testing.T) {
	app := New()
	app.Startup(context.Background())
	canceled := make(chan struct{}, 2)
	app.streams["one"] = func() { canceled <- struct{}{} }
	app.streams["two"] = func() { canceled <- struct{}{} }
	app.StopLogStream("one")
	app.StopLogStream("missing")
	app.Shutdown(context.Background())
	for range 2 {
		select {
		case <-canceled:
		case <-time.After(time.Second):
			t.Fatal("stream was not canceled")
		}
	}
	if app.runtimeContext != nil || len(app.streams) != 0 {
		t.Fatalf("shutdown state = context %v streams %d", app.runtimeContext, len(app.streams))
	}
}

func TestContextViewsNeverExposeTokens(t *testing.T) {
	cfg := &config.Config{CurrentContext: "prod", Contexts: []config.Context{{Name: "prod", Server: "wren.example", User: "you", Token: "secret"}}}
	views := contextViews(cfg, "prod")
	if len(views) != 1 || !views[0].Selected || views[0].Server != "wren.example" {
		t.Fatalf("views = %+v", views)
	}
	b, err := json.Marshal(views)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "secret") || strings.Contains(string(b), "token") {
		t.Fatalf("serialized context leaks credential material: %s", b)
	}
}
