// Package client is the CLI's transport to the Wren control plane. It speaks the
// control-plane HTTP/JSON API (spec §5.2) over the context's server address.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/summiteight/wren/internal/config"
)

// Client talks to a single control-plane context.
type Client struct {
	base string
	user string
	http *http.Client
}

// New builds a client bound to the given context. A bare host:port server is
// assumed to be plain HTTP (control planes on localhost/kind); an explicit
// scheme is honored.
func New(ctx *config.Context) *Client {
	base := ctx.Server
	if !strings.HasPrefix(base, "http://") && !strings.HasPrefix(base, "https://") {
		base = "http://" + base
	}
	user := ctx.User
	if user == "" {
		user = os.Getenv("USER")
	}
	return &Client{
		base: strings.TrimRight(base, "/"),
		user: user,
		http: &http.Client{Timeout: 30 * time.Second},
	}
}

// Server returns the control-plane base URL this client targets.
func (c *Client) Server() string { return c.base }

// RunCreateOptions is a request to start a new agent run.
type RunCreateOptions struct {
	Project     string `json:"project"`
	Task        string `json:"task"`
	Harness     string `json:"harness,omitempty"`
	Interactive bool   `json:"interactive,omitempty"`
	BaseRef     string `json:"baseRef,omitempty"`
	CPU         string `json:"cpu,omitempty"`
	Memory      string `json:"memory,omitempty"`
	Runtime     string `json:"runtime,omitempty"`
}

// Run is a summary view of an agent run.
type Run struct {
	ID           string `json:"id"`
	Project      string `json:"project"`
	User         string `json:"user,omitempty"`
	Phase        string `json:"phase"`
	Harness      string `json:"harness,omitempty"`
	Namespace    string `json:"namespace,omitempty"`
	PRURL        string `json:"prUrl,omitempty"`
	RestartCount int32  `json:"restartCount,omitempty"`
}

// CreateRun submits a task to a new agent run.
func (c *Client) CreateRun(ctx context.Context, opts RunCreateOptions) (*Run, error) {
	var run Run
	if err := c.do(ctx, http.MethodPost, "/v1/runs", opts, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// ListRuns returns runs visible to the caller. Scope is one of mine|team|all.
func (c *Client) ListRuns(ctx context.Context, scope string) ([]Run, error) {
	var runs []Run
	path := "/v1/runs"
	if scope != "" {
		path += "?scope=" + scope
	}
	if err := c.do(ctx, http.MethodGet, path, nil, &runs); err != nil {
		return nil, err
	}
	return runs, nil
}

// GetRun returns a single run by ID.
func (c *Client) GetRun(ctx context.Context, id string) (*Run, error) {
	var run Run
	if err := c.do(ctx, http.MethodGet, "/v1/runs/"+id, nil, &run); err != nil {
		return nil, err
	}
	return &run, nil
}

// LogsOptions selects which container to tail and whether to follow the stream.
type LogsOptions struct {
	Container string
	Follow    bool
}

// StreamLogs copies a run's pod logs to w. With Follow set it blocks until the
// stream ends (or ctx is canceled). A 4xx/5xx from the control plane is
// returned as an error (the CLI exits non-zero); the body is not written.
func (c *Client) StreamLogs(ctx context.Context, id string, opts LogsOptions, w io.Writer) error {
	q := url.Values{}
	if opts.Follow {
		q.Set("follow", "true")
	}
	if opts.Container != "" {
		q.Set("container", opts.Container)
	}
	path := "/v1/runs/" + id + "/logs"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	// A followed stream has no fixed length; use a client without the default
	// 30s timeout so long-running tails are not cut off. Cancellation flows
	// through ctx instead.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	if c.user != "" {
		req.Header.Set("X-Wren-User", c.user)
	}
	resp, err := c.streamClient().Do(req)
	if err != nil {
		return fmt.Errorf("control plane request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("control plane error (%s): %s", resp.Status, errorMessage(resp.Body))
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("read log stream: %w", err)
	}
	return nil
}

// streamClient returns an HTTP client with no overall timeout, suitable for a
// followed (open-ended) log stream. It reuses the base client's transport.
func (c *Client) streamClient() *http.Client {
	return &http.Client{Transport: c.http.Transport}
}

// do performs a JSON request/response against the control plane.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.user != "" {
		req.Header.Set("X-Wren-User", c.user)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("control plane request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("control plane error (%s): %s", resp.Status, errorMessage(resp.Body))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// errorMessage extracts the {"error": "..."} field, falling back to raw body.
func errorMessage(r io.Reader) string {
	b, _ := io.ReadAll(r)
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		return e.Error
	}
	return strings.TrimSpace(string(b))
}
