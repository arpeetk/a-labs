// Package apiserver exposes the control-plane services over HTTP/JSON, following
// the REST mapping in spec §5.2.
//
// M0 transport decision: HTTP/JSON (net/http). The spec's target transport is
// gRPC + Connect; that is a fast-follow (tracked in the spec's living status).
// The handler logic here is transport-agnostic and delegates to coreapi.Service.
package apiserver

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
	"github.com/summiteight/wren/internal/coreapi"
	"github.com/summiteight/wren/internal/launcher"
	"github.com/summiteight/wren/internal/store"
)

// Server is the HTTP handler for the control-plane API.
type Server struct {
	svc          *coreapi.Service
	lc           launcher.Launcher
	gatewayToken string
}

// Option customizes the API server without expanding its stable constructor.
type Option func(*Server)

// WithGatewayToken enables the internal event-ingest endpoint. The token is
// held by the trusted egress proxy, never by the untrusted harness container.
func WithGatewayToken(token string) Option {
	return func(s *Server) { s.gatewayToken = token }
}

// New builds a Server. The launcher is used directly for the pods/log stream
// (run logs), which is not part of the coreapi request/response surface.
func New(svc *coreapi.Service, lc launcher.Launcher, opts ...Option) *Server {
	s := &Server{svc: svc, lc: lc}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Handler returns the configured HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("POST /v1/runs", s.createRun)
	mux.HandleFunc("GET /v1/runs", s.listRuns)
	mux.HandleFunc("GET /v1/runs/{id}", s.getRun)
	mux.HandleFunc("DELETE /v1/runs/{id}", s.deleteRun)
	mux.HandleFunc("POST /v1/runs/{id}/stop", s.stopRun)
	mux.HandleFunc("POST /v1/runs/{id}/pause", s.pauseRun)
	mux.HandleFunc("POST /v1/runs/{id}/resume", s.resumeRun)
	mux.HandleFunc("GET /v1/runs/{id}/logs", s.runLogs)
	mux.HandleFunc("GET /v1/runs/{id}/events", s.runEvents)
	mux.HandleFunc("POST /v1/internal/runs/{id}/events", s.ingestRunEvent)
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

type ingestEventBody struct {
	SourceID string          `json:"sourceId"`
	Type     string          `json:"type"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	At       time.Time       `json:"at,omitempty"`
}

// --- handlers ---

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.svc.Ready(ctx); err != nil {
		writeErr(w, http.StatusServiceUnavailable, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
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
	project := r.URL.Query().Get("project")
	runs, err := s.svc.ListRuns(r.Context(), scope, userOf(r), project)
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

func (s *Server) runEvents(w http.ResponseWriter, r *http.Request) {
	after, err := queryInt64(r, "after", 0)
	if err != nil || after < 0 {
		writeErr(w, http.StatusBadRequest, errors.New("after must be a non-negative integer"))
		return
	}
	limit64, err := queryInt64(r, "limit", 200)
	if err != nil || limit64 < 1 || limit64 > 1000 {
		writeErr(w, http.StatusBadRequest, errors.New("limit must be between 1 and 1000"))
		return
	}
	events, err := s.svc.ListRunEvents(r.Context(), r.PathValue("id"), after, int(limit64))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

func (s *Server) ingestRunEvent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if s.gatewayToken == "" {
		writeErr(w, http.StatusServiceUnavailable, errors.New("gateway event ingestion is not configured"))
		return
	}
	want := "Bearer " + s.gatewayToken
	got := r.Header.Get("Authorization")
	if len(got) != len(want) || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 || r.Header.Get("X-Wren-Run-ID") != id {
		writeErr(w, http.StatusUnauthorized, errors.New("invalid gateway identity"))
		return
	}
	var body ingestEventBody
	if err := decode(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	inserted, err := s.svc.AppendGatewayEvent(r.Context(), id, body.SourceID, body.Type, body.Payload, body.At)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	if !inserted {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"inserted": true})
}

// deleteRun removes a run and its cluster resources (`wren run rm`).
func (s *Server) deleteRun(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.DeleteRun(r.Context(), r.PathValue("id")); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// stopRun cancels a run without deleting it (`wren run stop`): the operator
// halts the pod and drives the run to Canceled (terminal, no auto-resume).
func (s *Server) stopRun(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.StopRun(r.Context(), r.PathValue("id")); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) pauseRun(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.PauseRun(r.Context(), r.PathValue("id")); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// resumeRun manually restarts a terminally-Failed run (`wren run resume`):
// the operator resets the retry budget, clears any leftover pod, and gives
// the run a fresh attempt. Rejects a non-Failed run with 400, not a no-op.
func (s *Server) resumeRun(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.ResumeRun(r.Context(), r.PathValue("id")); err != nil {
		writeServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// runLogs streams a run's pod logs as plaintext (chunked, flush-per-line). It
// resolves the run's namespace from the store, rejects runs that have no pod
// yet/anymore with a 409 + phase hint, and honors ?follow= and ?container=.
func (s *Server) runLogs(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	run, err := s.svc.GetRun(r.Context(), id)
	if err != nil {
		writeServiceErr(w, err) // 404 for unknown run
		return
	}
	// A Pending run has not been scheduled onto a pod yet — short-circuit before
	// touching the cluster so the caller gets a crisp hint.
	if run.Phase == string(wrenv1.PhasePending) {
		writeErr(w, http.StatusConflict, errors.New("run is Pending: no pod yet"))
		return
	}

	container := r.URL.Query().Get("container")
	follow := r.URL.Query().Get("follow") == "true"

	stream, err := s.lc.StreamLogs(r.Context(), run.Namespace, id, container, follow)
	if err != nil {
		switch {
		case errors.Is(err, launcher.ErrNoPod):
			writeErr(w, http.StatusConflict, fmt.Errorf("no pod for run (phase %s)", run.Phase))
		default:
			writeErr(w, http.StatusBadGateway, err)
		}
		return
	}
	defer stream.Close()

	// The server sets a blanket WriteTimeout (cmd/wren-apiserver/main.go) to
	// harden the rest of the API against slow-client resource exhaustion, but
	// that same timeout would truncate a `?follow=true` tail mid-stream — an
	// agent run can legitimately run far longer than any fixed write budget.
	// Disable the write deadline for this handler specifically (zero value =
	// no deadline) rather than weakening the server-wide setting.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)

	// Copy line-by-line so `-f` tails feel live: flush after each newline.
	br := bufio.NewReader(stream)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return // client hung up
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if err != io.EOF {
				// Best-effort: surface a trailing note; headers already sent.
				_, _ = w.Write([]byte("\n[log stream error: " + err.Error() + "]\n"))
			}
			return
		}
	}
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

func queryInt64(r *http.Request, name string, fallback int64) (int64, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return fallback, nil
	}
	return strconv.ParseInt(raw, 10, 64)
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
