package client

import (
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
	if c.Server() != "http://wren.corp.internal:443" {
		t.Errorf("Server() = %q", c.Server())
	}
	c2 := New(&config.Context{Server: "https://host/", User: "me@x"})
	if c2.Server() != "https://host" {
		t.Errorf("Server() = %q", c2.Server())
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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/runs":
			if r.URL.Query().Get("scope") != "mine" {
				t.Errorf("scope = %q", r.URL.Query().Get("scope"))
			}
			_ = json.NewEncoder(w).Encode([]Run{{ID: "r-1"}, {ID: "r-2"}})
		case strings.HasPrefix(r.URL.Path, "/v1/runs/"):
			_ = json.NewEncoder(w).Encode(Run{ID: "r-9", Phase: "Running"})
		}
	}))
	defer srv.Close()

	c := New(&config.Context{Server: srv.URL})
	runs, err := c.ListRuns(context.Background(), "mine")
	if err != nil || len(runs) != 2 {
		t.Fatalf("ListRuns = %v, %v", runs, err)
	}
	run, err := c.GetRun(context.Background(), "r-9")
	if err != nil || run.Phase != "Running" {
		t.Fatalf("GetRun = %+v, %v", run, err)
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
	if _, err := c.ListRuns(context.Background(), "mine"); err == nil {
		t.Fatal("expected connection error")
	}
}
