package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/summiteight/wren/internal/config"
)

func TestNewNormalizesServer(t *testing.T) {
	c := New(&config.Context{Server: "wren.corp.internal:443", User: "me@x"})
	if c.base != "http://wren.corp.internal:443" {
		t.Errorf("base = %q", c.base)
	}
	c2 := New(&config.Context{Server: "https://host/", User: "me@x"})
	if c2.base != "https://host" {
		t.Errorf("base = %q", c2.base)
	}
}

func TestCreateRunSendsRequest(t *testing.T) {
	var gotBody map[string]any
	var gotUser, gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser = r.Header.Get("X-Wren-User")
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Run{ID: "r-1", Project: "p", Phase: "Pending"})
	}))
	defer srv.Close()

	c := New(&config.Context{Server: srv.URL, User: "arpeet@x"})
	run, err := c.CreateRun(context.Background(), RunCreateOptions{Project: "p", Task: "do it", Interactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if run.ID != "r-1" || run.Phase != "Pending" {
		t.Fatalf("run = %+v", run)
	}
	if gotMethod != http.MethodPost || gotPath != "/v1/runs" {
		t.Errorf("request = %s %s", gotMethod, gotPath)
	}
	if gotUser != "arpeet@x" {
		t.Errorf("X-Wren-User = %q", gotUser)
	}
	if gotBody["project"] != "p" || gotBody["task"] != "do it" || gotBody["interactive"] != true {
		t.Errorf("body = %+v", gotBody)
	}
}

func TestListAndGetRun(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/runs":
			gotQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode([]Run{{ID: "r-1"}, {ID: "r-2"}})
		case strings.HasPrefix(r.URL.Path, "/v1/runs/"):
			_ = json.NewEncoder(w).Encode(Run{ID: "r-9", Phase: "Running"})
		}
	}))
	defer srv.Close()

	c := New(&config.Context{Server: srv.URL})
	runs, err := c.ListRuns(context.Background(), "mine", "payments-api")
	if err != nil || len(runs) != 2 {
		t.Fatalf("ListRuns = %v, %v", runs, err)
	}
	if !strings.Contains(gotQuery, "scope=mine") || !strings.Contains(gotQuery, "project=payments-api") {
		t.Errorf("query = %q, want scope=mine and project=payments-api", gotQuery)
	}

	// project="" must omit the query param entirely, not send an empty value.
	if _, err := c.ListRuns(context.Background(), "all", ""); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(gotQuery, "project") {
		t.Errorf("empty project leaked into the query string: %q", gotQuery)
	}

	run, err := c.GetRun(context.Background(), "r-9")
	if err != nil || run.Phase != "Running" {
		t.Fatalf("GetRun = %+v, %v", run, err)
	}
}

func TestPauseRunEscapesIDAndPosts(t *testing.T) {
	var method, path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.EscapedPath()
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	c := New(&config.Context{Server: srv.URL})
	if err := c.PauseRun(context.Background(), "r/with space"); err != nil {
		t.Fatal(err)
	}
	if method != http.MethodPost || path != "/v1/runs/r%2Fwith%20space/pause" {
		t.Fatalf("request = %s %s", method, path)
	}
}

func TestRunLifecycleAndEventEndpoints(t *testing.T) {
	var requests []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		if strings.HasSuffix(r.URL.Path, "/events") {
			_ = json.NewEncoder(w).Encode([]RunEvent{{ID: 8, RunID: "r-1", Type: "status"}})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(&config.Context{Server: srv.URL})
	events, err := c.ListRunEvents(context.Background(), "r-1", 7, 3)
	if err != nil || len(events) != 1 || events[0].ID != 8 {
		t.Fatalf("ListRunEvents = %+v, %v", events, err)
	}
	for name, call := range map[string]func(context.Context, string) error{
		"delete": c.DeleteRun,
		"stop":   c.StopRun,
		"resume": c.ResumeRun,
	} {
		if err := call(context.Background(), "r-1"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	want := []string{
		"GET /v1/runs/r-1/events?after=7&limit=3",
		"DELETE /v1/runs/r-1",
		"POST /v1/runs/r-1/stop",
		"POST /v1/runs/r-1/resume",
	}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
}

func TestCreateAndListProject(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/projects":
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(Project{Name: "demo", Repo: "acme/api"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects":
			_ = json.NewEncoder(w).Encode([]Project{{Name: "demo"}, {Name: "keyless"}})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/projects/demo":
			_ = json.NewEncoder(w).Encode(Project{Name: "demo", Repo: "acme/api"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(&config.Context{Server: srv.URL, User: "me@x"})
	p, err := c.CreateProject(context.Background(), Project{Name: "demo", Repo: "acme/api", DefaultHarness: "mock"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "demo" || p.Repo != "acme/api" {
		t.Fatalf("created project = %+v", p)
	}
	if gotBody["name"] != "demo" || gotBody["defaultHarness"] != "mock" {
		t.Errorf("create body = %+v", gotBody)
	}
	projects, err := c.ListProjects(context.Background())
	if err != nil || len(projects) != 2 || projects[1].Name != "keyless" {
		t.Fatalf("ListProjects = %+v, %v", projects, err)
	}
	got, err := c.GetProject(context.Background(), "demo")
	if err != nil || got.Repo != "acme/api" {
		t.Fatalf("GetProject = %+v, %v", got, err)
	}
}

func TestStreamLogs(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/runs/r-fail/logs" {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "no pod yet"})
			return
		}
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte("line1\nline2\n"))
	}))
	defer srv.Close()

	c := New(&config.Context{Server: srv.URL, User: "me@x"})
	var buf bytes.Buffer
	if err := c.StreamLogs(context.Background(), "r-1", LogsOptions{Container: "harness", Follow: true}, &buf); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "line1\nline2\n" {
		t.Errorf("streamed = %q", buf.String())
	}
	if !strings.Contains(gotQuery, "follow=true") || !strings.Contains(gotQuery, "container=harness") {
		t.Errorf("query = %q", gotQuery)
	}

	// A control-plane error surfaces and the body is not streamed to w.
	buf.Reset()
	err := c.StreamLogs(context.Background(), "r-fail", LogsOptions{}, &buf)
	if err == nil || !strings.Contains(err.Error(), "no pod yet") {
		t.Fatalf("err = %v, want parsed conflict", err)
	}
	if buf.Len() != 0 {
		t.Errorf("error body must not be written to the log sink, got %q", buf.String())
	}
}

func TestErrorResponseParsed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "task is required"})
	}))
	defer srv.Close()

	c := New(&config.Context{Server: srv.URL})
	_, err := c.CreateRun(context.Background(), RunCreateOptions{Project: "p"})
	if err == nil || !strings.Contains(err.Error(), "task is required") {
		t.Fatalf("err = %v, want parsed control-plane error", err)
	}
}

func TestConnectionErrorSurfaced(t *testing.T) {
	// Nothing listening on this port.
	c := New(&config.Context{Server: "127.0.0.1:1"})
	if _, err := c.ListRuns(context.Background(), "mine", ""); err == nil {
		t.Fatal("expected connection error")
	}
}
