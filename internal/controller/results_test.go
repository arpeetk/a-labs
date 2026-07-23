package controller

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
)

// fakeLogReader is the LogReader double: pod/container-keyed log fixtures plus
// an injectable error, so the scrape's best-effort path is testable.
type fakeLogReader struct {
	logs map[string]string
	err  error
}

func (f *fakeLogReader) ReadLogs(_ context.Context, ns, pod, container string, _ int64) (io.ReadCloser, error) {
	if f.err != nil {
		return nil, f.err
	}
	body, ok := f.logs[ns+"/"+pod+"/"+container]
	if !ok {
		return nil, errors.New("no fixture")
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

// eventLogFixture mimics a real harness-container stream: wren-runtime's own
// log.Printf lines interleaved with the emitter's NDJSON.
const eventLogFixture = `2026/07/22 10:00:00 wren-runtime harness: starting
{"type":"status","time":"2026-07-22T10:00:01Z","phase":"running"}
{"type":"message","time":"2026-07-22T10:00:02Z","message":"mock harness: task = do it"}
{"type":"token_usage","time":"2026-07-22T10:00:03Z","inputTokens":100,"outputTokens":10}
not json at all
{"type":"token_usage","time":"2026-07-22T10:00:04Z","inputTokens":1234,"outputTokens":567}
{"type":"status","time":"2026-07-22T10:00:05Z","phase":"finalizing","sessionId":"sess-42"}
{"type":"pr_ready","time":"2026-07-22T10:00:06Z","pr":{"branch":"wren/me/r-abc","url":"https://github.com/corp/payments/pull/7"}}
{"type":"status","time":"2026-07-22T10:00:07Z","phase":"succeeded"}
`

func TestParseResultEvents(t *testing.T) {
	res := parseResultEvents(strings.NewReader(eventLogFixture))
	if !res.hasPR || res.prURL != "https://github.com/corp/payments/pull/7" || res.prBranch != "wren/me/r-abc" {
		t.Errorf("pr = %+v", res)
	}
	// Last token_usage wins (v0.1 records terminal values only).
	if !res.hasUsage || res.inTok != 1234 || res.outTok != 567 {
		t.Errorf("usage = %+v", res)
	}
	if res.sessionID != "sess-42" {
		t.Errorf("sessionID = %q", res.sessionID)
	}
}

func TestParseResultEventsEmpty(t *testing.T) {
	res := parseResultEvents(strings.NewReader("just logs\nno events\n"))
	if res.hasPR || res.hasUsage || res.sessionID != "" {
		t.Errorf("expected no results, got %+v", res)
	}
}

// TestScrapeOnSucceeded: when the pod succeeds, the harness events land in
// Status.PR/Usage/SessionID alongside the terminal phase.
func TestScrapeOnSucceeded(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run)
	r.Logs = &fakeLogReader{logs: map[string]string{
		run.Namespace + "/r-abc-0/harness": eventLogFixture,
	}}
	reconcile(t, r, run) // Pending
	reconcile(t, r, run) // create pod r-abc-0

	setPodPhase(t, c, run.Namespace, "r-abc-0", corev1.PodSucceeded, nil)
	reconcile(t, r, run)

	got := getRun(t, c, run)
	if got.Status.Phase != wrenv1.PhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded", got.Status.Phase)
	}
	if got.Status.PR.URL != "https://github.com/corp/payments/pull/7" || got.Status.PR.Branch != "wren/me/r-abc" {
		t.Errorf("status.pr = %+v", got.Status.PR)
	}
	if got.Status.Usage.InputTokens != 1234 || got.Status.Usage.OutputTokens != 567 {
		t.Errorf("status.usage = %+v", got.Status.Usage)
	}
	if got.Status.SessionID != "sess-42" {
		t.Errorf("status.sessionId = %q", got.Status.SessionID)
	}
}

// TestScrapeBeforeResumeDelete: on a retryable failure the scrape must run
// BEFORE the failed pod is deleted, so the partial results survive the resume.
func TestScrapeBeforeResumeDelete(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run)
	r.Logs = &fakeLogReader{logs: map[string]string{
		run.Namespace + "/r-abc-0/harness": eventLogFixture,
	}}
	reconcile(t, r, run) // Pending
	reconcile(t, r, run) // create pod r-abc-0

	// OOMKill the harness (retryable): the pod is deleted for resume.
	setPodPhase(t, c, run.Namespace, "r-abc-0", corev1.PodFailed, func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: ContainerHarness,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 137, Reason: "OOMKilled",
			}},
		}}
	})
	reconcile(t, r, run)

	got := getRun(t, c, run)
	if got.Status.Phase != wrenv1.PhaseInterrupted {
		t.Fatalf("phase = %q, want Interrupted (resume)", got.Status.Phase)
	}
	if got.Status.Usage.InputTokens != 1234 {
		t.Errorf("usage lost across resume: %+v", got.Status.Usage)
	}
	if got.Status.PR.URL != "https://github.com/corp/payments/pull/7" {
		t.Errorf("pr lost across resume: %+v", got.Status.PR)
	}
	// The failed pod is gone, but its events made it into status.
	var old corev1.Pod
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: run.Namespace, Name: "r-abc-0"}, &old); err == nil {
		t.Error("failed pod should be deleted for resume")
	}
}

// TestScrapeOnDeterministicFailure: a terminal failure still records whatever
// the harness reported before dying.
func TestScrapeOnDeterministicFailure(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run)
	r.Logs = &fakeLogReader{logs: map[string]string{
		run.Namespace + "/r-abc-0/harness": eventLogFixture,
	}}
	reconcile(t, r, run) // Pending
	reconcile(t, r, run) // create pod r-abc-0

	setPodPhase(t, c, run.Namespace, "r-abc-0", corev1.PodFailed, func(p *corev1.Pod) {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:  ContainerHarness,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
		}}
	})
	reconcile(t, r, run)

	got := getRun(t, c, run)
	if got.Status.Phase != wrenv1.PhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Status.Phase)
	}
	if got.Status.PR.URL != "https://github.com/corp/payments/pull/7" {
		t.Errorf("status.pr = %+v", got.Status.PR)
	}
}

// TestScrapeIsBestEffort: a log-read failure must not block the reconcile.
func TestScrapeIsBestEffort(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run)
	r.Logs = &fakeLogReader{err: errors.New("log read boom")}
	reconcile(t, r, run)
	reconcile(t, r, run)

	setPodPhase(t, c, run.Namespace, "r-abc-0", corev1.PodSucceeded, nil)
	reconcile(t, r, run)

	got := getRun(t, c, run)
	if got.Status.Phase != wrenv1.PhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded (scrape failure must not block)", got.Status.Phase)
	}
	if got.Status.PR.URL != "" {
		t.Errorf("no events scraped, pr should be empty: %+v", got.Status.PR)
	}
}

// TestScrapeKeylessRunIsNoop: a keyless run emits a pr_ready with no URL; the
// branch is recorded but the URL stays empty (the apiserver then omits prUrl).
func TestScrapeKeylessRunIsNoop(t *testing.T) {
	run := testRun()
	r, c := newReconciler(t, run)
	r.Logs = &fakeLogReader{logs: map[string]string{
		run.Namespace + "/r-abc-0/harness": `{"type":"token_usage","inputTokens":1234,"outputTokens":567}` + "\n" +
			`{"type":"pr_ready","pr":{"branch":"wren/r-abc"}}` + "\n",
	}}
	reconcile(t, r, run)
	reconcile(t, r, run)

	setPodPhase(t, c, run.Namespace, "r-abc-0", corev1.PodSucceeded, nil)
	reconcile(t, r, run)

	got := getRun(t, c, run)
	if got.Status.PR.URL != "" {
		t.Errorf("keyless run must not gain a PR URL, got %+v", got.Status.PR)
	}
	if got.Status.PR.Branch != "wren/r-abc" {
		t.Errorf("branch should still be recorded, got %+v", got.Status.PR)
	}
	if got.Status.Usage.InputTokens != 1234 {
		t.Errorf("usage = %+v", got.Status.Usage)
	}
}
