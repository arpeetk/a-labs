// Package podruntime implements the in-pod roles the wren-runtime binary runs:
// the harness runner, the hydrate init container, and the long-lived sidecars
// (egress-proxy, checkpointer, agent-gateway). Roles are functions here so they
// are unit-testable; cmd/wren-runtime is a thin dispatcher.
//
// The egress-proxy is real (spec §5.6). The agent-gateway sidecar is still an
// M0 stand-in (it keeps the pod's native-sidecar shape valid and logs
// liveness only). The checkpointer is real as of WS-21 when a checkpoint
// mount is configured: it takes periodic workspace snapshots (checkpointLoop)
// against internal/blob.Store, and hydrate restores the latest one on a
// confirmed workspace loss (RunHydrate, RestoreRequired). Without the mount
// configured, checkpointer behaves exactly like the plain liveness stub. GCS
// is the production-shaped mount; local hostPath exists only for kind/dev E2E.
// Stream bridging lands with interactive steering (M2).
package podruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/summiteight/wren/internal/blob"
	"github.com/summiteight/wren/internal/egress"
	"github.com/summiteight/wren/internal/finalize"
	"github.com/summiteight/wren/internal/github"
	"github.com/summiteight/wren/internal/gitwork"
	"github.com/summiteight/wren/internal/harness"
	"github.com/summiteight/wren/internal/runspec"
)

// egressProxyBase is the base URL of the in-pod egress-proxy, when the operator
// wired one in. In that mode the runner holds NO GitHub token — the proxy
// injects it (spec §5.6/§5.7).
func egressProxyBase() string { return strings.TrimRight(os.Getenv("WREN_EGRESS_PROXY"), "/") }

// githubToken is the direct-mode (no-proxy) token, read from the runner env.
func githubToken() string { return os.Getenv("GITHUB_TOKEN") }

// prConfigured reports whether a run can open a real PR: it needs a repo and a
// way to authenticate to GitHub — either the egress-proxy (preferred) or a
// direct token (M0 fallback).
func prConfigured(spec runspec.RunSpec) bool {
	return spec.Repo != "" && (egressProxyBase() != "" || githubToken() != "")
}

// gitCloneURL returns the clone URL and push token for a run. Via the proxy the
// token is empty (the proxy injects it); direct mode embeds github.com + token.
func gitCloneURL(spec runspec.RunSpec) (url, token string, ok bool) {
	if spec.Repo == "" {
		return "", "", false
	}
	if base := egressProxyBase(); base != "" {
		return base + egress.RouteGitHub + spec.Repo + ".git", "", true
	}
	if tok := githubToken(); tok != "" {
		return "https://github.com/" + spec.Repo + ".git", tok, true
	}
	return "", "", false
}

// prClientAndToken builds the GitHub PR API client and the push token. Via the
// proxy: client points at the proxy's github-api route (no token). Direct: a
// real client with the runner's token (through the injectable seam for tests).
func prClientAndToken() (github.Client, string, error) {
	if base := egressProxyBase(); base != "" {
		c, err := github.NewRESTWithBaseURL("", base+egress.RouteGitHubAPI, nil)
		return c, "", err
	}
	tok := githubToken()
	return newGitHubClient(tok), tok, nil
}

// newGitHubClient builds the direct-mode PR client; a seam so tests can inject a fake.
var newGitHubClient = func(token string) github.Client { return github.NewREST(token, nil) }

// ErrRetryable marks a transient failure (e.g. the egress-proxy never came up)
// that the operator may retry on a fresh pod. cmd/wren-runtime maps it to the
// ExitRetryable exit code.
var ErrRetryable = errors.New("retryable")

// proxyWaitTimeout is how long the runner waits for the egress-proxy socket.
// A package var so tests can shorten it.
var proxyWaitTimeout = 30 * time.Second

// waitForProxy reports whether the in-pod egress-proxy is accepting connections.
// The proxy is a native sidecar that starts before the runner, but "started"
// does not guarantee its socket is listening yet; the runner (same network
// namespace) waits on 127.0.0.1. Returns true immediately when no proxy is
// configured (nothing to wait for).
func waitForProxy(ctx context.Context, em *harness.Emitter) bool {
	base := egressProxyBase()
	if base == "" {
		return true
	}
	u, err := url.Parse(base)
	if err != nil {
		return true
	}
	deadline := time.Now().Add(proxyWaitTimeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return false
		}
		if c, err := net.DialTimeout("tcp", u.Host, 500*time.Millisecond); err == nil {
			_ = c.Close()
			return true
		}
		time.Sleep(200 * time.Millisecond)
	}
	em.Message("egress-proxy not reachable after " + proxyWaitTimeout.String())
	return false
}

// skipReason explains why finalize was skipped.
func skipReason(spec runspec.RunSpec) string {
	if spec.Repo == "" {
		return "no repo configured for this project"
	}
	return "no egress-proxy or GITHUB_TOKEN available to the runner"
}

// DefaultRunSpecPath is where the operator mounts the RunSpec ConfigMap.
var DefaultRunSpecPath = runspec.MountPath + "/" + runspec.FileName

// LoadRunSpec reads and parses a RunSpec JSON file.
func LoadRunSpec(path string) (runspec.RunSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return runspec.RunSpec{}, err
	}
	var spec runspec.RunSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return runspec.RunSpec{}, fmt.Errorf("parse runspec %s: %w", path, err)
	}
	return spec, nil
}

// RunHarness runs the harness role: load the RunSpec, select an adapter, execute
// the task, and stream events to out. It returns an error if the task fails.
func RunHarness(ctx context.Context, out io.Writer, specPath string) error {
	if specPath == "" {
		specPath = DefaultRunSpecPath
	}
	eventOut := out
	var eventFile *os.File
	if path := strings.TrimSpace(os.Getenv("WREN_EVENT_FILE")); path != "" {
		var err error
		eventFile, err = os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return fmt.Errorf("open durable event stream: %w", err)
		}
		defer eventFile.Close()
		eventOut = io.MultiWriter(out, eventFile)
	}
	em := harness.NewEmitter(eventOut)

	spec, err := LoadRunSpec(specPath)
	if err != nil {
		em.Errorf("load runspec: " + err.Error())
		return err
	}
	if spec.WorkspacePath == "" {
		spec.WorkspacePath = runspec.WorkspacePath
	}

	h := harness.Select(spec)
	em.Status("running")
	em.Message("harness: " + h.Name() + " (mode=" + string(spec.Mode) + ")")

	// The harness (and finalize) reach GitHub / the model API via the proxy. If
	// it never comes up, abort before running the (expensive) harness and let
	// the operator retry on a fresh pod.
	if !waitForProxy(ctx, em) {
		em.Status("failed")
		return fmt.Errorf("%w: egress-proxy unreachable", ErrRetryable)
	}

	// Egress canary (WS-1): when the operator signals enforcement, prove from
	// inside the untrusted runner that the iptables lockdown holds — for EVERY
	// harness, before the (expensive) agent runs. A bypass is a deterministic
	// security failure: fail the run, spend no tokens.
	if err := harness.RunCanary(ctx, em); err != nil {
		em.Status("failed")
		return err
	}

	res, err := h.Run(ctx, spec, em)
	if err != nil {
		em.Status("failed")
		return err
	}

	em.Status("finalizing")
	em.Message(fmt.Sprintf("harness complete: branch=%s tokens=%d/%d",
		res.Branch, res.InputTokens, res.OutputTokens))

	pr := harness.PRInfo{Branch: res.Branch}
	if prConfigured(spec) {
		client, token, cerr := prClientAndToken()
		if cerr != nil {
			em.Errorf("finalize: " + cerr.Error())
			em.Status("failed")
			return cerr
		}
		p, ferr := finalize.Run(ctx, spec, token, client)
		switch {
		case errors.Is(ferr, finalize.ErrNoChanges):
			em.Message("finalize: harness made no changes; no PR opened")
		case errors.Is(ferr, finalize.ErrRetryable):
			// Transient (GitHub 5xx/429, dropped push connection, timeout):
			// exit ExitRetryable so the operator resumes on a fresh pod — the
			// commit survives on the workspace PVC, so the retry only has to
			// redo the push/PR (WS-11).
			em.Errorf("finalize (transient, will retry on a fresh pod): " + ferr.Error())
			em.Status("failed")
			return fmt.Errorf("%w: %w", ErrRetryable, ferr)
		case ferr != nil:
			em.Errorf("finalize: " + ferr.Error())
			em.Status("failed")
			return ferr
		default:
			pr = harness.PRInfo{Branch: p.Branch, URL: p.URL}
			em.Message("finalize: opened PR " + p.URL)
		}
	} else {
		em.Message("finalize: PR creation skipped — " + skipReason(spec) + " (M0)")
	}

	em.PRReady(pr)
	em.Status("succeeded")
	return nil
}

// RunHydrate runs the hydrate init container: it prepares the workspace before
// the harness starts.
//   - Mode == ModeResume, RestoreRequired: a freshly-recreated, empty PVC after
//     a confirmed workspace loss — restore is mandatory (restoreFromCheckpoint).
//   - Mode == ModeResume, !RestoreRequired: an ordinary crash-resume where the
//     PVC survived (today's only ModeResume case pre-WS-21) — a no-op, the
//     workspace files are already there.
//   - Otherwise (a fresh start): clone the repo when configured, else no-op.
func RunHydrate(ctx context.Context, out io.Writer, specPath string) error {
	if specPath == "" {
		specPath = DefaultRunSpecPath
	}
	em := harness.NewEmitter(out)
	spec, err := LoadRunSpec(specPath)
	if err != nil {
		em.Errorf("load runspec: " + err.Error())
		return err
	}

	if spec.Mode == runspec.ModeResume {
		if !spec.RestoreRequired {
			if err := prepareHarnessState(spec); err != nil {
				em.Errorf("hydrate harness state: " + err.Error())
				return err
			}
			em.Message("hydrate: workspace ready (resume; PVC survived, no restore needed)")
			return nil
		}
		if err := restoreFromCheckpoint(ctx, em, spec); err != nil {
			return err
		}
		if err := prepareHarnessState(spec); err != nil {
			em.Errorf("hydrate harness state: " + err.Error())
			return err
		}
		return nil
	}

	// Fresh start: when a repo + GitHub auth are configured, do a real clone so
	// the harness works in a git checkout and finalize can push a branch. The
	// clone routes through the egress-proxy when present (no token in the runner).
	if url, token, ok := gitCloneURL(spec); ok {
		if !waitForProxy(ctx, em) {
			return fmt.Errorf("%w: egress-proxy unreachable", ErrRetryable)
		}
		if _, err := gitwork.Clone(url, spec.BaseRef, spec.WorkspacePath, token); err != nil {
			em.Errorf("hydrate clone: " + err.Error())
			return err
		}
		if err := prepareHarnessState(spec); err != nil {
			em.Errorf("hydrate harness state: " + err.Error())
			return err
		}
		via := "direct"
		if egressProxyBase() != "" {
			via = "egress-proxy"
		}
		em.Message("hydrate: cloned " + spec.Repo + " @ " + orDefault(spec.BaseRef, "default") + " (" + via + ")")
		return nil
	}

	if err := prepareHarnessState(spec); err != nil {
		em.Errorf("hydrate harness state: " + err.Error())
		return err
	}
	em.Message("hydrate: workspace ready (fresh clone skipped; no repo/token — M0)")
	return nil
}

func prepareHarnessState(spec runspec.RunSpec) error {
	if spec.Harness != "codex" {
		return nil
	}
	path := runspec.CodexHomePath(spec.WorkspacePath, spec.Repo)
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create Codex state directory %s: %w", path, err)
	}
	return nil
}

// restoreFromCheckpoint runs only when the controller has confirmed the
// workspace PVC was recreated after a genuine loss (spec.RestoreRequired):
// restore is mandatory here, not best-effort. No checkpoint to restore is NOT
// a silent empty-workspace resume — it's a deterministic, non-retryable
// failure: this plain (non-ErrRetryable) error flows through
// cmd/wren-runtime -> runspec.ExitError -> classifyTermination -> PhaseFailed,
// with zero new controller code (mirrors WS-16's existing invariant).
func restoreFromCheckpoint(ctx context.Context, em *harness.Emitter, spec runspec.RunSpec) error {
	mountPath := os.Getenv("WREN_CHECKPOINT_MOUNT_PATH")
	if mountPath == "" {
		err := fmt.Errorf("hydrate: workspace restore required but no checkpoint mount is configured")
		em.Errorf(err.Error())
		return err
	}
	store := blob.NewMountStore(mountPath, blob.RunPrefix(spec.CheckpointBucket, spec.RunID))
	manifest, rc, err := blob.LoadCheckpoint(ctx, store, spec.RestoreCheckpoint)
	if err != nil {
		which := "latest valid checkpoint"
		if spec.RestoreCheckpoint != "" {
			which = "exact checkpoint " + spec.RestoreCheckpoint
		}
		werr := fmt.Errorf("hydrate: load %s for run %s: %w", which, spec.RunID, err)
		em.Errorf(werr.Error())
		return werr
	}
	defer rc.Close()
	if err := blob.Unarchive(rc, spec.WorkspacePath); err != nil {
		werr := fmt.Errorf("hydrate: restore checkpoint %s: %w", manifest.ManifestKey, err)
		em.Errorf(werr.Error())
		return werr
	}
	em.Message(fmt.Sprintf("hydrate: restore-from-checkpoint PASSED — restored %s (%d bytes, sha256=%s) into %s",
		manifest.ManifestKey, manifest.SizeBytes, manifest.SHA256, spec.WorkspacePath))
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// RunEgressProxy runs the egress-proxy sidecar: it holds the run's credentials
// and forwards the runner's traffic, injecting auth for github.com /
// api.github.com / api.anthropic.com / api.openai.com and enforcing the domain
// allowlist for everything else (spec §5.6). Secrets (GITHUB_TOKEN,
// ANTHROPIC_API_KEY, OPENAI_API_KEY) are mounted into THIS container, never
// the harness.
func RunEgressProxy(ctx context.Context, out io.Writer) error {
	em := harness.NewEmitter(out)

	cfg := egressConfigFromEnv()
	proxy, err := egress.New(cfg)
	if err != nil {
		em.Errorf("egress-proxy: " + err.Error())
		return err
	}
	port := os.Getenv("WREN_EGRESS_PORT")
	if port == "" {
		port = egress.DefaultPort
	}
	srv := &http.Server{Addr: "127.0.0.1:" + port, Handler: proxy}
	em.Message(fmt.Sprintf("egress-proxy: listening on 127.0.0.1:%s (%d cred routes, %d allowlisted hosts)",
		port, len(cfg.Routes), len(cfg.Allowlist)))

	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	em.Message("egress-proxy: stopping")
	return nil
}

// Default upstreams for the egress-proxy's credentialed routes. Each is
// overridable via an env var so an e2e tier can point the proxy at a local
// stand-in (e.g. a gitea server for the gitea-backed e2e-pr tier) without
// touching production defaults. See envUpstream.
const (
	defaultGitHubUpstream       = "https://github.com"
	defaultGitHubAPIUpstream    = "https://api.github.com"
	defaultAnthropicUpstream    = "https://api.anthropic.com"
	defaultOpenAIUpstream       = "https://api.openai.com"
	defaultControlPlaneUpstream = "http://wren-apiserver.wren-system.svc:8090"
)

// envUpstream returns the override from env if set (trimmed, non-empty),
// otherwise def. The default behavior is unchanged when the var is unset.
func envUpstream(envVar, def string) string {
	if v := strings.TrimSpace(os.Getenv(envVar)); v != "" {
		return v
	}
	return def
}

// egressConfigFromEnv builds the proxy config from the egress-proxy container's
// environment (its mounted credentials + the run's allowlist). The upstream
// URLs default to the real endpoints but are env-overridable (WREN_*_UPSTREAM)
// so a local e2e tier can retarget them.
func egressConfigFromEnv() egress.Config {
	cfg := egress.Config{Allowlist: splitAllowlist(os.Getenv("WREN_EGRESS_ALLOWLIST"))}
	if tok := os.Getenv("GITHUB_TOKEN"); tok != "" {
		cfg.Routes = append(cfg.Routes,
			egress.Route{Prefix: egress.RouteGitHub,
				Upstream: envUpstream("WREN_GITHUB_UPSTREAM", defaultGitHubUpstream),
				Auth:     egress.BasicAuth{Username: "x-access-token", Password: tok}},
			egress.Route{Prefix: egress.RouteGitHubAPI,
				Upstream: envUpstream("WREN_GITHUB_API_UPSTREAM", defaultGitHubAPIUpstream),
				Auth:     egress.HeaderAuth{Key: "Authorization", Value: "Bearer " + tok}},
		)
	}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		cfg.Routes = append(cfg.Routes, egress.Route{Prefix: egress.RouteAnthropic,
			Upstream: envUpstream("WREN_ANTHROPIC_UPSTREAM", defaultAnthropicUpstream),
			Auth:     egress.HeaderAuth{Key: "x-api-key", Value: key}})
	}
	if key := os.Getenv("OPENAI_API_KEY"); key != "" {
		cfg.Routes = append(cfg.Routes, egress.Route{Prefix: egress.RouteOpenAI,
			Upstream: envUpstream("WREN_OPENAI_UPSTREAM", defaultOpenAIUpstream),
			Auth:     egress.HeaderAuth{Key: "Authorization", Value: "Bearer " + key}})
	}
	if token := os.Getenv("WREN_GATEWAY_TOKEN"); token != "" {
		runID := os.Getenv("WREN_RUN_ID")
		cfg.Routes = append(cfg.Routes, egress.Route{Prefix: egress.RouteControlPlane,
			Upstream: envUpstream("WREN_CONTROL_PLANE_UPSTREAM", defaultControlPlaneUpstream),
			Auth: egress.HeaderAuths{
				{Key: "Authorization", Value: "Bearer " + token},
				{Key: "X-Wren-Run-ID", Value: runID},
			}})
	}
	return cfg
}

func splitAllowlist(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// RunSidecar runs a long-lived sidecar role: it logs liveness and blocks until
// the context is canceled (SIGTERM), then exits cleanly so the pod can complete.
// It backs sidecars without a configured active implementation (currently the
// agent-gateway, plus checkpointer when no checkpoint mount is enabled).
func RunSidecar(ctx context.Context, out io.Writer, name string) error {
	em := harness.NewEmitter(out)
	em.Message(name + ": started (M0 stand-in)")
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			em.Message(name + ": stopping")
			return nil
		case <-ticker.C:
			em.Message(name + ": alive")
		}
	}
}

// RunCheckpointer runs the checkpointer sidecar. When a GCS checkpoint mount is
// configured (both WREN_CHECKPOINT_BUCKET and WREN_CHECKPOINT_MOUNT_PATH set —
// the operator sets the mount-path env only when --checkpoint-gcs-mount added
// the CSI volume), it first runs a one-shot mount self-check: a Put+Get+List
// round-trip against a small object, proving the mount is live and writable.
// It then runs the real periodic-snapshot loop (checkpointLoop) instead of
// falling through to the generic sidecar stub. With no mount configured,
// behavior is unchanged: the plain sidecar liveness loop, so runs without the
// feature are completely unaffected.
func RunCheckpointer(ctx context.Context, out io.Writer, role string) error {
	bucket := os.Getenv("WREN_CHECKPOINT_BUCKET")
	mountPath := os.Getenv("WREN_CHECKPOINT_MOUNT_PATH")
	if bucket == "" || mountPath == "" {
		return RunSidecar(ctx, out, role)
	}
	em := harness.NewEmitter(out)
	runID := os.Getenv("WREN_RUN_ID")
	if err := mountSelfCheck(ctx, em, mountPath, bucket, runID); err != nil {
		em.Message(fmt.Sprintf("%s: mount self-check FAILED at %s: %v", role, mountPath, err))
	}
	return checkpointLoop(ctx, em, role, blob.NewMountStore(mountPath, blob.RunPrefix(bucket, runID)))
}

// defaultCheckpointIntervalSeconds mirrors the controller's own default
// (checkpointInterval / defaultCheckpointInterval = 120) for the case the env
// var is absent or unparseable — belt-and-suspenders only, since the operator
// always sets WREN_CHECKPOINT_INTERVAL whenever the mount is configured.
const defaultCheckpointIntervalSeconds = 120

// checkpointTickUnit is the unit WREN_CHECKPOINT_INTERVAL is measured in.
// Production always uses whole seconds (matching the operator-set env var,
// itself seconds — spec §5.5); a package var, same seam pattern as
// proxyWaitTimeout, so tests can shrink it to milliseconds and observe several
// real ticks quickly instead of waiting on production-scale intervals.
var checkpointTickUnit = time.Second

// checkpointInterval parses WREN_CHECKPOINT_INTERVAL (seconds) into a ticker
// duration, falling back to the default on anything unparseable or <= 0.
func checkpointInterval() time.Duration {
	n, err := strconv.Atoi(os.Getenv("WREN_CHECKPOINT_INTERVAL"))
	if err != nil || n <= 0 {
		n = defaultCheckpointIntervalSeconds
	}
	return time.Duration(n) * checkpointTickUnit
}

// checkpointLoop ticks every WREN_CHECKPOINT_INTERVAL, archiving the workspace
// and Putting it to store as a new checkpoint object, until ctx is canceled. A
// failed snapshot attempt logs loudly and the loop continues to the next tick
// — crash-looping this sidecar over a transient GCS blip would kill the whole
// pod for no benefit (same non-fatal posture as the mount self-check).
func checkpointLoop(ctx context.Context, em *harness.Emitter, role string, store blob.Store) error {
	ticker := time.NewTicker(checkpointInterval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			em.Message(role + ": stopping")
			return nil
		case <-ticker.C:
			takeCheckpoint(ctx, em, role, store)
		}
	}
}

// checkpointWorkspacePath is where the checkpointer container archives from.
// It's always runspec.WorkspacePath in production (the container's read-only
// workspace mount, pod.go); a package var only so tests can point it at a
// temp dir instead of the real "/workspace", same seam pattern as
// proxyWaitTimeout / checkpointTickUnit.
var checkpointWorkspacePath = runspec.WorkspacePath

// takeCheckpoint archives the workspace directory and Puts it as one new
// checkpoint object, logging a PASSED line (key + byte size) on success — the
// hook the live-verification proof asserts on, same pattern as the self-check's
// PASSED line — or a loud FAILED line on any error.
func takeCheckpoint(ctx context.Context, em *harness.Emitter, role string, store blob.Store) {
	m, err := blob.PublishCheckpoint(ctx, store, checkpointWorkspacePath, os.Getenv("WREN_RUN_ID"), "periodic", checkpointRetention(), time.Now())
	if err != nil {
		em.Message(fmt.Sprintf("%s: checkpoint snapshot FAILED: %v", role, err))
		return
	}
	em.CheckpointReady(harness.CheckpointInfo{ID: m.ID, URI: m.ManifestKey, SHA256: m.SHA256, SizeBytes: m.SizeBytes, FormatVersion: int32(m.FormatVersion), Trigger: m.Trigger, At: m.CreatedAt})
	em.Message(fmt.Sprintf("%s: checkpoint snapshot PASSED — published %s (%d bytes, sha256=%s)", role, m.ManifestKey, m.SizeBytes, m.SHA256))
}

func checkpointRetention() int {
	n, err := strconv.Atoi(os.Getenv("WREN_CHECKPOINT_RETAIN"))
	if err != nil || n <= 0 {
		return 5
	}
	return n
}

// RunCheckpointOnce is invoked through pods/exec in the trusted checkpointer
// container after the harness has been stopped. It emits exactly one manifest
// JSON object so the operator can commit that proof into AgentRun status.
func RunCheckpointOnce(ctx context.Context, out io.Writer) error {
	bucket, mountPath, runID := os.Getenv("WREN_CHECKPOINT_BUCKET"), os.Getenv("WREN_CHECKPOINT_MOUNT_PATH"), os.Getenv("WREN_RUN_ID")
	if bucket == "" || mountPath == "" {
		return errors.New("forced checkpoint requires a configured checkpoint mount")
	}
	m, err := blob.PublishCheckpoint(ctx, blob.NewMountStore(mountPath, blob.RunPrefix(bucket, runID)), checkpointWorkspacePath, runID, "pause", checkpointRetention(), time.Now())
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(m)
}

var signalProcess = syscall.Kill

func quiesceHarness(sig syscall.Signal) error {
	if err := signalProcess(1, sig); err != nil {
		return fmt.Errorf("signal harness pid 1 with %s: %w", sig, err)
	}
	return nil
}

// mountSelfCheck writes, reads back, and lists a small self-check object through
// the mounted store, returning an error on any step or a read-back mismatch. On
// success it logs a clear PASSED line naming the object and mount path — the
// hook the WS-18 live proof asserts on (and cross-checks with gcloud). The
// store is scoped to this run's own prefix (blob.RunPrefix) so the self-check
// never lists another run's objects sharing the same bucket.
func mountSelfCheck(ctx context.Context, em *harness.Emitter, mountPath, bucket, runID string) error {
	if runID == "" {
		runID = "unknown"
	}
	store := blob.NewMountStore(mountPath, blob.RunPrefix(bucket, runID))
	key := "_wren-mount-check/" + runID + ".txt"
	payload := fmt.Sprintf("wren mount self-check run=%s at=%s\n", runID, time.Now().UTC().Format(time.RFC3339Nano))

	if err := store.Put(ctx, key, strings.NewReader(payload)); err != nil {
		return fmt.Errorf("put: %w", err)
	}
	rc, err := store.Get(ctx, key)
	if err != nil {
		return fmt.Errorf("get: %w", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return fmt.Errorf("read: %w", err)
	}
	if string(got) != payload {
		return fmt.Errorf("read-back mismatch: wrote %d bytes, read %d", len(payload), len(got))
	}
	objs, err := store.List(ctx, "_wren-mount-check/")
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}
	em.Message(fmt.Sprintf("checkpointer: mount self-check PASSED — wrote+read+listed %s (%d bytes, %d object(s) under prefix) at %s",
		key, len(got), len(objs), mountPath))
	return nil
}

// Roles that the dispatcher understands.
const (
	RoleHarness        = "harness"
	RoleHydrate        = "hydrate"
	RoleEgressProxy    = "egress-proxy"
	RoleEgressLockdown = "egress-lockdown"
	RoleCheckpointer   = "checkpointer"
	RoleGateway        = "agent-gateway"
	RoleCheckpointOnce = "checkpoint-once"
	RoleQuiesce        = "quiesce"
	RoleUnquiesce      = "unquiesce"
)

// Dispatch runs the named role to completion.
func Dispatch(ctx context.Context, out io.Writer, role, specPath string) error {
	switch role {
	case RoleHarness, "":
		return RunHarness(ctx, out, specPath)
	case RoleHydrate:
		return RunHydrate(ctx, out, specPath)
	case RoleEgressProxy:
		return RunEgressProxy(ctx, out)
	case RoleEgressLockdown:
		return RunLockdown(ctx, out, DefaultLockdownConfig())
	case RoleCheckpointer:
		return RunCheckpointer(ctx, out, role)
	case RoleCheckpointOnce:
		return RunCheckpointOnce(ctx, out)
	case RoleQuiesce:
		return quiesceHarness(syscall.SIGSTOP)
	case RoleUnquiesce:
		return quiesceHarness(syscall.SIGCONT)
	case RoleGateway:
		return RunGateway(ctx, out)
	default:
		return fmt.Errorf("unknown role %q", role)
	}
}
