// Package desktop exposes Wren's control-plane client as a native desktop
// application backend. It deliberately reuses internal/client so the CLI and
// desktop surface share transport semantics and lifecycle behavior.
package desktop

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/summiteight/wren/internal/client"
	"github.com/summiteight/wren/internal/config"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const logEventName = "wren:log"

// App is the stateful backend bound into the Wails frontend.
type App struct {
	mu              sync.RWMutex
	selectedContext string
	runtimeContext  context.Context
	streams         map[string]context.CancelFunc
	nextStream      uint64
	emit            func(context.Context, string, ...any)
}

// New returns an uninitialized desktop backend.
func New() *App {
	return &App{streams: make(map[string]context.CancelFunc), emit: runtime.EventsEmit}
}

// Startup captures the Wails application context used by asynchronous event
// streams. It is wired to options.App.OnStartup by the desktop entrypoint.
func (a *App) Startup(ctx context.Context) {
	a.mu.Lock()
	a.runtimeContext = ctx
	a.mu.Unlock()
}

// Shutdown releases any followed HTTP log streams before the native window
// exits. Stream cancellation propagates to the control-plane request.
func (a *App) Shutdown(context.Context) {
	a.stopAllStreams()
	a.mu.Lock()
	a.runtimeContext = nil
	a.mu.Unlock()
}

// ContextView is a credential-free control-plane context suitable for UI use.
type ContextView struct {
	Name     string `json:"name"`
	Server   string `json:"server"`
	Org      string `json:"org,omitempty"`
	User     string `json:"user,omitempty"`
	Selected bool   `json:"selected"`
}

// Bootstrap is the first application query: contexts plus the selected
// control plane's projects and fleet. A missing context is not fatal to app
// startup; it returns an empty management surface so the user can connect one.
type Bootstrap struct {
	Contexts []ContextView    `json:"contexts"`
	Projects []client.Project `json:"projects"`
	Runs     []client.Run     `json:"runs"`
}

// Load returns everything required for the initial desktop frame.
func (a *App) Load() (Bootstrap, error) {
	cfg, err := config.Load()
	if err != nil {
		return Bootstrap{}, err
	}
	selected := a.contextName(cfg)
	out := Bootstrap{Contexts: contextViews(cfg, selected)}
	if selected == "" {
		return out, nil
	}
	c, err := a.client(cfg, selected)
	if err != nil {
		return Bootstrap{}, err
	}
	out.Projects, err = c.ListProjects(context.Background())
	if err != nil {
		return Bootstrap{}, err
	}
	out.Runs, err = c.ListRuns(context.Background(), "all", "")
	if err != nil {
		return Bootstrap{}, err
	}
	sortRuns(out.Runs)
	return out, nil
}

// SelectContext switches the desktop without changing the CLI's on-disk
// current context. The choice lasts for the app process.
func (a *App) SelectContext(name string) (Bootstrap, error) {
	cfg, err := config.Load()
	if err != nil {
		return Bootstrap{}, err
	}
	if _, err := cfg.Resolve(name); err != nil {
		return Bootstrap{}, err
	}
	a.stopAllStreams()
	a.mu.Lock()
	a.selectedContext = name
	a.mu.Unlock()
	return a.Load()
}

// SaveContext creates or updates a control-plane connection and selects it in
// both desktop and CLI configuration. Tokens stay in the owner-only config and
// are never returned to the frontend.
func (a *App) SaveContext(ctx config.Context) (Bootstrap, error) {
	ctx.Name = strings.TrimSpace(ctx.Name)
	ctx.Server = strings.TrimSpace(ctx.Server)
	ctx.User = strings.TrimSpace(ctx.User)
	if ctx.Name == "" || ctx.Server == "" {
		return Bootstrap{}, fmt.Errorf("context name and control-plane server are required")
	}
	cfg, err := config.Load()
	if err != nil {
		return Bootstrap{}, err
	}
	// Preserve an existing token when an edit leaves it blank.
	if old, err := cfg.Resolve(ctx.Name); err == nil && ctx.Token == "" {
		ctx.Token = old.Token
	}
	cfg.Upsert(ctx)
	cfg.CurrentContext = ctx.Name
	if err := cfg.Save(); err != nil {
		return Bootstrap{}, err
	}
	a.stopAllStreams()
	a.mu.Lock()
	a.selectedContext = ctx.Name
	a.mu.Unlock()
	return a.Load()
}

// ListRuns refreshes fleet data with server-side scope/project filtering and
// client-side phase filtering, matching the CLI fleet semantics.
func (a *App) ListRuns(scope, project, phase string) ([]client.Run, error) {
	c, err := a.currentClient()
	if err != nil {
		return nil, err
	}
	runs, err := c.ListRuns(context.Background(), scope, project)
	if err != nil {
		return nil, err
	}
	if phase != "" {
		filtered := runs[:0:0]
		for _, run := range runs {
			if run.Phase == phase {
				filtered = append(filtered, run)
			}
		}
		runs = filtered
	}
	sortRuns(runs)
	return runs, nil
}

func (a *App) GetRun(id string) (*client.Run, error) {
	c, err := a.currentClient()
	if err != nil {
		return nil, err
	}
	return c.GetRun(context.Background(), id)
}

func (a *App) CreateRun(opts client.RunCreateOptions) (*client.Run, error) {
	c, err := a.currentClient()
	if err != nil {
		return nil, err
	}
	return c.CreateRun(context.Background(), opts)
}

func (a *App) StopRun(id string) error {
	c, err := a.currentClient()
	if err != nil {
		return err
	}
	return c.StopRun(context.Background(), id)
}

func (a *App) PauseRun(id string) error {
	c, err := a.currentClient()
	if err != nil {
		return err
	}
	return c.PauseRun(context.Background(), id)
}

func (a *App) ResumeRun(id string) error {
	c, err := a.currentClient()
	if err != nil {
		return err
	}
	return c.ResumeRun(context.Background(), id)
}

func (a *App) DeleteRun(id string) error {
	c, err := a.currentClient()
	if err != nil {
		return err
	}
	return c.DeleteRun(context.Background(), id)
}

func (a *App) ListProjects() ([]client.Project, error) {
	c, err := a.currentClient()
	if err != nil {
		return nil, err
	}
	return c.ListProjects(context.Background())
}

func (a *App) CreateProject(project client.Project) (*client.Project, error) {
	c, err := a.currentClient()
	if err != nil {
		return nil, err
	}
	return c.CreateProject(context.Background(), project)
}

// Logs returns a bounded current snapshot. Streaming is layered on this backend
// next; keeping the first method finite makes detail navigation reliable even
// for completed runs and sidecars.
func (a *App) Logs(id, container string) (string, error) {
	c, err := a.currentClient()
	if err != nil {
		return "", err
	}
	var out bytes.Buffer
	if err := c.StreamLogs(context.Background(), id, client.LogsOptions{Container: container}, &out); err != nil {
		return "", err
	}
	return out.String(), nil
}

// LogEvent is emitted to the frontend while a followed container log stream is
// active. Chunks are intentionally unparsed so terminal output stays lossless.
type LogEvent struct {
	StreamID string `json:"streamId"`
	Chunk    string `json:"chunk,omitempty"`
	Error    string `json:"error,omitempty"`
	Done     bool   `json:"done,omitempty"`
}

// StartLogStream follows one container and forwards chunks over the Wails
// event bridge. The returned ID lets the frontend ignore stale streams after
// navigating or changing containers.
func (a *App) StartLogStream(id, container string) (string, error) {
	if strings.TrimSpace(id) == "" {
		return "", fmt.Errorf("run ID is required")
	}
	c, err := a.currentClient()
	if err != nil {
		return "", err
	}
	a.mu.Lock()
	if a.runtimeContext == nil {
		a.mu.Unlock()
		return "", fmt.Errorf("desktop runtime is not ready")
	}
	a.nextStream++
	streamID := fmt.Sprintf("log-%d", a.nextStream)
	streamCtx, cancel := context.WithCancel(a.runtimeContext)
	a.streams[streamID] = cancel
	runtimeCtx := a.runtimeContext
	emit := a.emit
	a.mu.Unlock()

	go func() {
		writer := eventWriter(func(chunk string) {
			emit(runtimeCtx, logEventName, LogEvent{StreamID: streamID, Chunk: chunk})
		})
		err := c.StreamLogs(streamCtx, id, client.LogsOptions{Container: container, Follow: true}, writer)
		event := LogEvent{StreamID: streamID, Done: true}
		if err != nil && streamCtx.Err() == nil {
			event.Error = err.Error()
		}
		emit(runtimeCtx, logEventName, event)
		a.mu.Lock()
		delete(a.streams, streamID)
		a.mu.Unlock()
	}()
	return streamID, nil
}

// StopLogStream stops a single followed stream. It is idempotent so frontend
// cleanup remains safe when a stream has already ended.
func (a *App) StopLogStream(streamID string) {
	a.mu.Lock()
	cancel := a.streams[streamID]
	delete(a.streams, streamID)
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (a *App) stopAllStreams() {
	a.mu.Lock()
	streams := a.streams
	a.streams = make(map[string]context.CancelFunc)
	a.mu.Unlock()
	for _, cancel := range streams {
		cancel()
	}
}

type eventWriter func(string)

func (w eventWriter) Write(p []byte) (int, error) {
	w(string(p))
	return len(p), nil
}

func (a *App) currentClient() (*client.Client, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	return a.client(cfg, a.contextName(cfg))
}

func (a *App) client(cfg *config.Config, name string) (*client.Client, error) {
	ctx, err := cfg.Resolve(name)
	if err != nil {
		return nil, err
	}
	return client.New(ctx), nil
}

func (a *App) contextName(cfg *config.Config) string {
	a.mu.RLock()
	selected := a.selectedContext
	a.mu.RUnlock()
	if selected != "" {
		return selected
	}
	return cfg.CurrentContext
}

func contextViews(cfg *config.Config, selected string) []ContextView {
	out := make([]ContextView, 0, len(cfg.Contexts))
	for _, ctx := range cfg.Contexts {
		out = append(out, ContextView{Name: ctx.Name, Server: ctx.Server, Org: ctx.Org, User: ctx.User, Selected: ctx.Name == selected})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func sortRuns(runs []client.Run) {
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
}
