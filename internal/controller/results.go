package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"io"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/log"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
	"github.com/summiteight/wren/internal/harness"
)

// logScrapeTailLines bounds the harness-log read at pod terminal state. The
// run's pr_ready/token_usage events are the LAST lines the harness emits, but
// a chatty agent can produce thousands of lines — a generous tail keeps the
// read cheap while never cutting off the terminal events.
const logScrapeTailLines = 10000

// LogReader reads a pod container's logs (the pods/log subresource). The
// controller-runtime client does not serve subresources, so the real
// implementation keeps a typed clientset — the same split as
// internal/launcher's K8s.
//
// This is the v0.1 run-results channel (WS-11): the pod holds no SA token by
// design, so the runner cannot write its own status; the operator scrapes the
// harness container's event stream instead. It adds no credentials, endpoints,
// or attack surface. The gateway event bridge (spec §5.4) remains the v0.2
// target — the event schema does not change, so the swap is internal.
type LogReader interface {
	ReadLogs(ctx context.Context, namespace, pod, container string, tailLines int64) (io.ReadCloser, error)
}

// k8sLogReader is the cluster-backed LogReader.
type k8sLogReader struct{ cs kubernetes.Interface }

// NewLogReader returns a LogReader backed by a typed clientset.
func NewLogReader(cs kubernetes.Interface) LogReader { return k8sLogReader{cs: cs} }

// ReadLogs implements LogReader.
func (r k8sLogReader) ReadLogs(ctx context.Context, namespace, pod, container string, tailLines int64) (io.ReadCloser, error) {
	req := r.cs.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{
		Container: container,
		TailLines: &tailLines,
	})
	return req.Stream(ctx)
}

// resultEvents are the run results extracted from the harness event stream.
type resultEvents struct {
	prURL, prBranch string
	hasPR           bool
	inTok, outTok   int64
	hasUsage        bool
	sessionID       string
}

// parseResultEvents scans newline-delimited harness events (the schema in
// internal/harness/event.go) out of a container log stream and extracts the
// run's results. The LAST pr_ready and token_usage win — v0.1 records terminal
// values only; mid-run usage increments are superseded by the final one.
// Non-JSON lines (cmd/wren-runtime's own log.Printf shares the stream) are
// tolerated, as are blank lines.
func parseResultEvents(r io.Reader) resultEvents {
	var res resultEvents
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // agent messages can be large
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev harness.Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch ev.Type {
		case harness.EventPRReady:
			if ev.PR != nil {
				res.prURL, res.prBranch, res.hasPR = ev.PR.URL, ev.PR.Branch, true
			}
		case harness.EventTokenUsage:
			res.inTok, res.outTok, res.hasUsage = ev.InputTokens, ev.OutputTokens, true
		}
		// No event type carries the session id yet (the schema is frozen this
		// round); lift it tolerantly from any line that has one so a future
		// adapter needs no operator change.
		var sid struct {
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(line, &sid); err == nil && sid.SessionID != "" {
			res.sessionID = sid.SessionID
		}
	}
	return res
}

// scrapeRunResults reads the harness container's events from the pod's logs
// and merges pr_ready / token_usage / sessionId into run.Status. It runs when
// the pod reaches a terminal state — and, on failure, BEFORE the pod is
// deleted for resume (the event lines survive container termination only
// while the pod object lives).
//
// Best-effort: a log-read failure never blocks the reconcile — the results
// are a status nicety, not the run's fate. Keyless runs emit no events and
// are a no-op. Events only ever fill status in, never blank it (the CR stays
// authoritative — the same rule as coreapi's runFromCR).
func (r *AgentRunReconciler) scrapeRunResults(ctx context.Context, run *wrenv1.AgentRun, pod *corev1.Pod) {
	if r.Logs == nil {
		return
	}
	rc, err := r.Logs.ReadLogs(ctx, pod.Namespace, pod.Name, ContainerHarness, logScrapeTailLines)
	if err != nil {
		log.FromContext(ctx).V(1).Info("harness log scrape skipped", "pod", pod.Name, "error", err)
		return
	}
	defer func() { _ = rc.Close() }()
	res := parseResultEvents(rc)
	if res.hasPR {
		run.Status.PR = wrenv1.PRStatus{URL: res.prURL, Branch: res.prBranch}
	}
	if res.hasUsage {
		run.Status.Usage = wrenv1.UsageStatus{InputTokens: res.inTok, OutputTokens: res.outTok}
	}
	if res.sessionID != "" {
		run.Status.SessionID = res.sessionID
	}
}
