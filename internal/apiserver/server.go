// Package apiserver exposes the control-plane services over HTTP/JSON, following
// the REST mapping in spec §5.2.
//
// M0 transport decision: HTTP/JSON (net/http). The spec's target transport is
// gRPC + Connect; that is a fast-follow (tracked in the spec's living status).
// The handler logic here is transport-agnostic and delegates to coreapi.Service.
package apiserver

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/summiteight/wren/internal/coreapi"
	"github.com/summiteight/wren/internal/store"
)

// Server is the HTTP handler for the control-plane API.
type Server struct {
	svc *coreapi.Service
}

// New builds a Server.
func New(svc *coreapi.Service) *Server { return &Server{svc: svc} }

// Handler returns the configured HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("POST /v1/runs", s.createRun)
	mux.HandleFunc("GET /v1/runs", s.listRuns)
	mux.HandleFunc("GET /v1/runs/{id}", s.getRun)
	mux.HandleFunc("POST /v1/projects", s.createProject)
	mux.HandleFunc("GET /v1/projects", s.listProjects)
	mux.HandleFunc("GET /v1/projects/{name}", s.getProject)
	return mux
}

// --- request/response DTOs ---

type createRunBody struct {
	Project     string `json:"project"`
	Task        string `json:"task"`
	Harness     string `json:"harness,omitempty"`
	Model       string `json:"model,omitempty"`
	BaseRef     string `json:"baseRef,omitempty"`
	Interactive bool   `json:"interactive,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
	CPU         string `json:"cpu,omitempty"`
	Memory      string `json:"memory,omitempty"`
}

type errorBody struct {
	Error string `json:"error"`
}

// --- handlers ---

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	var body createRunBody
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	run, err := s.svc.CreateRun(r.Context(), coreapi.CreateRunRequest{
		Project:     body.Project,
		User:        userOf(r),
		Prompt:      body.Task,
		Harness:     body.Harness,
		Model:       body.Model,
		BaseRef:     body.BaseRef,
		Interactive: body.Interactive,
		Runtime:     body.Runtime,
		CPU:         body.CPU,
		Memory:      body.Memory,
	})
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, run)
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	scope := r.URL.Query().Get("scope")
	runs, err := s.svc.ListRuns(r.Context(), scope, userOf(r))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, runs)
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.svc.GetRun(r.Context(), r.PathValue("id"))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) createProject(w http.ResponseWriter, r *http.Request) {
	var p store.Project
	if err := decode(r, &p); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	out, err := s.svc.CreateProject(r.Context(), &p)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	ps, err := s.svc.ListProjects(r.Context())
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ps)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.GetProject(r.Context(), r.PathValue("name"))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// --- helpers ---

// userOf extracts the caller identity. M0 uses a trusted header set by the CLI;
// OIDC/SSO validation at a gateway lands in M1 (spec §7).
func userOf(r *http.Request) string {
	if u := r.Header.Get("X-Wren-User"); u != "" {
		return u
	}
	return ""
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, errorBody{Error: err.Error()})
}

// writeServiceErr maps service errors to HTTP status codes.
func writeServiceErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, coreapi.ErrValidation):
		writeErr(w, http.StatusBadRequest, err)
	case errors.Is(err, coreapi.ErrNotFound):
		writeErr(w, http.StatusNotFound, err)
	case errors.Is(err, store.ErrExists):
		writeErr(w, http.StatusConflict, err)
	default:
		writeErr(w, http.StatusInternalServerError, err)
	}
}
