// Package harness implements the in-pod agent runner contract (spec §5.4): read
// a RunSpec, execute the task, and emit a newline-delimited JSON event stream.
//
// Adapters (claude-code, mock, ...) implement the Harness interface; the runtime
// binary (cmd/wren-runtime) selects one and streams its events to stdout, where
// the agent-gateway sidecar bridges them to the control plane.
package harness

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// EventType enumerates the harness event stream (spec §5.4).
type EventType string

const (
	EventStatus         EventType = "status"
	EventMessage        EventType = "message"
	EventTokenUsage     EventType = "token_usage"
	EventToolCall       EventType = "tool_call"
	EventCheckpointHint EventType = "checkpoint_hint"
	EventPRReady        EventType = "pr_ready"
	EventError          EventType = "error"
)

// PRInfo describes the pull request the harness opened (or intends to).
type PRInfo struct {
	Branch string `json:"branch,omitempty"`
	URL    string `json:"url,omitempty"`
	Title  string `json:"title,omitempty"`
}

// Event is one item in the harness output stream.
type Event struct {
	Type         EventType `json:"type"`
	Time         time.Time `json:"time"`
	Phase        string    `json:"phase,omitempty"`
	Message      string    `json:"message,omitempty"`
	InputTokens  int64     `json:"inputTokens,omitempty"`
	OutputTokens int64     `json:"outputTokens,omitempty"`
	Tool         string    `json:"tool,omitempty"`
	PR           *PRInfo   `json:"pr,omitempty"`
	Error        string    `json:"error,omitempty"`
}

// Emitter serializes Events as newline-delimited JSON to a writer. Safe for
// concurrent use by the harness and its background goroutines.
type Emitter struct {
	mu  sync.Mutex
	enc *json.Encoder
	now func() time.Time
}

// NewEmitter returns an Emitter writing to w.
func NewEmitter(w io.Writer) *Emitter {
	return &Emitter{enc: json.NewEncoder(w), now: time.Now}
}

// Emit writes a single event, stamping the time if unset.
func (e *Emitter) Emit(ev Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ev.Time.IsZero() {
		ev.Time = e.now()
	}
	_ = e.enc.Encode(ev)
}

// Convenience emitters.

func (e *Emitter) Status(phase string)  { e.Emit(Event{Type: EventStatus, Phase: phase}) }
func (e *Emitter) Message(msg string)   { e.Emit(Event{Type: EventMessage, Message: msg}) }
func (e *Emitter) ToolCall(tool string) { e.Emit(Event{Type: EventToolCall, Tool: tool}) }
func (e *Emitter) CheckpointHint()      { e.Emit(Event{Type: EventCheckpointHint}) }

func (e *Emitter) Usage(in, out int64) {
	e.Emit(Event{Type: EventTokenUsage, InputTokens: in, OutputTokens: out})
}

func (e *Emitter) PRReady(pr PRInfo) { e.Emit(Event{Type: EventPRReady, PR: &pr}) }

func (e *Emitter) Errorf(msg string) { e.Emit(Event{Type: EventError, Error: msg}) }
