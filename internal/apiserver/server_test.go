package apiserver

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/summiteight/wren/internal/coreapi"
	"github.com/summiteight/wren/internal/launcher"
	"github.com/summiteight/wren/internal/store"
)

func newTestServer(t *testing.T) (http.Handler, *store.Memory) {
	t.Helper()
	st := store.NewMemory()
	svc := coreapi.New(st, launcher.NewFake(), coreapi.DefaultDefaults())
	return New(svc).Handler(), st
}

func do(t *testing.T, h http.Handler, method, path, user, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, bytes.NewBufferString(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	if user != "" {
		r.Header.Set("X-Wren-User", user)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestHealth(t *testing.T) {
	h, _ := newTestServer(t)
	w := do(t, h, "GET", "/healthz", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("health code = %d", w.Code)
	}
}

func TestCreateProjectAndGet(t *testing.T) {
	h, _ := newTestServer(t)
	w := do(t, h, "POST", "/v1/projects", "admin@x", `{"name":"payments-api","repo":"corp/payments-api"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create project code = %d, body=%s", w.Code, w.Body.String())
	}
	w = do(t, h, "GET", "/v1/projects/payments-api", "u@x", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "corp/payments-api") {
		t.Fatalf("get project = %d %s", w.Code, w.Body.String())
	}
	// Missing project → 404.
	if w := do(t, h, "GET", "/v1/projects/ghost", "u@x", ""); w.Code != http.StatusNotFound {
		t.Fatalf("ghost project code = %d", w.Code)
	}
}

func TestCreateProjectValidation(t *testing.T) {
	h, _ := newTestServer(t)
	// Missing name → 400.
	if w := do(t, h, "POST", "/v1/projects", "u@x", `{"repo":"x/y"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("missing name code = %d, want 400", w.Code)
	}
	// Repo is OPTIONAL (keyless design): a repo-less project is accepted.
	w := do(t, h, "POST", "/v1/projects", "u@x", `{"name":"keyless","defaultHarness":"mock"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("keyless project code = %d, want 201, body=%s", w.Code, w.Body.String())
	}
}

func TestCreateProjectDuplicateConflict(t *testing.T) {
	h, _ := newTestServer(t)
	body := `{"name":"p","repo":"x/y"}`
	do(t, h, "POST", "/v1/projects", "u@x", body)
	w := do(t, h, "POST", "/v1/projects", "u@x", body)
	if w.Code != http.StatusConflict {
		t.Fatalf("duplicate code = %d, want 409", w.Code)
	}
}

func TestCreateRunFlow(t *testing.T) {
	h, _ := newTestServer(t)
	do(t, h, "POST", "/v1/projects", "u@x", `{"name":"p","repo":"x/y"}`)

	// Create a run.
	w := do(t, h, "POST", "/v1/runs", "arpeet@x", `{"project":"p","task":"do it","interactive":true}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create run code = %d, body=%s", w.Code, w.Body.String())
	}
	var run store.Run
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	if run.User != "arpeet@x" || run.Phase != "Pending" {
		t.Fatalf("run = %+v", run)
	}

	// Get it back.
	w = do(t, h, "GET", "/v1/runs/"+run.ID, "arpeet@x", "")
	if w.Code != http.StatusOK {
		t.Fatalf("get run code = %d", w.Code)
	}

	// List (scope mine).
	w = do(t, h, "GET", "/v1/runs?scope=mine", "arpeet@x", "")
	if w.Code != http.StatusOK {
		t.Fatalf("list code = %d", w.Code)
	}
	var runs []store.Run
	if err := json.Unmarshal(w.Body.Bytes(), &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("list len = %d, want 1", len(runs))
	}
}

func TestCreateRunValidation(t *testing.T) {
	h, _ := newTestServer(t)
	// Missing task → 400 from service validation.
	do(t, h, "POST", "/v1/projects", "u@x", `{"name":"p","repo":"x/y"}`)
	w := do(t, h, "POST", "/v1/runs", "u@x", `{"project":"p"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
}

func TestCreateRunProjectNotFound(t *testing.T) {
	h, _ := newTestServer(t)
	w := do(t, h, "POST", "/v1/runs", "u@x", `{"project":"ghost","task":"hi"}`)
	if w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
}

func TestGetRunNotFound(t *testing.T) {
	h, _ := newTestServer(t)
	if w := do(t, h, "GET", "/v1/runs/nope", "u@x", ""); w.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", w.Code)
	}
}

func TestBadJSON(t *testing.T) {
	h, _ := newTestServer(t)
	if w := do(t, h, "POST", "/v1/runs", "u@x", `{bad json`); w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", w.Code)
	}
	// Unknown field rejected (DisallowUnknownFields).
	if w := do(t, h, "POST", "/v1/runs", "u@x", `{"nope":1}`); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown field code = %d, want 400", w.Code)
	}
}
