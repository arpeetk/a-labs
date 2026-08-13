// Package coreapi is the control plane's business logic: the Projects and Runs
// services. It validates requests, resolves effective run configuration
// (project defaults ⊕ request overrides), maps a submission onto an AgentRun
// custom resource, and mirrors CR status back into the store.
package coreapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"

	wrenv1 "github.com/summiteight/wren/api/v1alpha1"
	"github.com/summiteight/wren/internal/launcher"
	"github.com/summiteight/wren/internal/store"
)

// ErrValidation wraps client (400-class) errors. ErrNotFound is re-exported from
// the store so transport layers can map it to 404.
var (
	ErrValidation = errors.New("validation error")
	ErrNotFound   = store.ErrNotFound
)

// Defaults fill in fields left unset by both the project and the request.
type Defaults struct {
	Harness          string
	HarnessImage     string
	Model            string
	RuntimeClass     string
	CPU              string
	Memory           string
	Disk             string
	CheckpointBucket string
	// DefaultNamespace, when set, is the run namespace for any project
	// registered without an explicit --namespace. `wren install` sets it (via
	// the apiserver's WREN_DEFAULT_RUN_NAMESPACE env) to its --run-namespace, so
	// the common single-shared-namespace case lands runs where install wrote the
	// credential Secrets. Empty falls back to NamespacePrefix.
	DefaultNamespace string
	NamespacePrefix  string // e.g. "user-" → namespace "user-<sanitized-user>"
	// GitHubTokenSecret / AnthropicKeySecret / OpenAIKeySecret name the proxy
	// credential Secrets the run's namespace must hold before a pod is worth
	// scheduling. They mirror the operator's --github-token-secret /
	// --anthropic-key-secret / --openai-key-secret defaults; the pre-flight
	// credential check reads them.
	GitHubTokenSecret  string
	AnthropicKeySecret string
	OpenAIKeySecret    string
}

// DefaultDefaults returns the built-in fallback configuration.
func DefaultDefaults() Defaults {
	return Defaults{
		Harness: "claude-code",
		// Matches the kind zero-config path's naming scheme (wren install
		// --kind builds/loads wren/claude-code:dev) — a project registered
		// with no --harness-image still resolves to an image that exists on a
		// freshly-installed kind cluster, instead of a dead placeholder that
		// never matched anything this repo builds.
		HarnessImage:     "wren/claude-code:dev",
		Model:            "claude-opus-4-8",
		RuntimeClass:     "runc",
		CPU:              "2",
		Memory:           "4Gi",
		Disk:             "10Gi",
		CheckpointBucket: "gs://wren-ckpt",
		NamespacePrefix:  "user-",
		// Mirror the operator's Secret-name defaults (cmd/wren-operator) and the
		// install constants (internal/install). The credential pre-flight only
		// checks a Secret it can name, so keep these in lockstep with those.
		GitHubTokenSecret:  "wren-github-token",
		AnthropicKeySecret: "wren-anthropic-key",
		OpenAIKeySecret:    "wren-openai-key",
	}
}

// Service implements the Projects and Runs logic over a Store and a Launcher.
type Service struct {
	store          store.Store
	launcher       launcher.Launcher
	defaults       Defaults
	now            func() time.Time
	idgen          func() string
	inlineDispatch bool
}

// Option customizes service execution policy.
type Option func(*Service)

// WithInlineDispatch controls the latency optimization that attempts a newly
// committed outbox item on the request goroutine. The background worker remains
// authoritative; disabling this is useful for deterministic recovery drills.
func WithInlineDispatch(enabled bool) Option {
	return func(s *Service) { s.inlineDispatch = enabled }
}

// New builds a Service.
func New(s store.Store, l launcher.Launcher, d Defaults, opts ...Option) *Service {
	svc := &Service{store: s, launcher: l, defaults: d, now: time.Now, idgen: genRunID, inlineDispatch: true}
	for _, opt := range opts {
		opt(svc)
	}
	return svc
}

// Ready verifies dependencies required to accept a durable submission.
func (s *Service) Ready(ctx context.Context) error {
	if health, ok := s.store.(store.Health); ok {
		if err := health.Ping(ctx); err != nil {
			return fmt.Errorf("store: %w", err)
		}
	}
	return nil
}

// --- Projects ---

// CreateProject validates and persists a project.
func (s *Service) CreateProject(ctx context.Context, p *store.Project) (*store.Project, error) {
	if strings.TrimSpace(p.Name) == "" {
		return nil, fmt.Errorf("%w: project name is required", ErrValidation)
	}
	if problems := utilvalidation.IsDNS1123Label(p.Name); len(problems) > 0 {
		return nil, fmt.Errorf("%w: invalid project name %q: %s", ErrValidation, p.Name, strings.Join(problems, "; "))
	}
	if p.Namespace != "" {
		if problems := utilvalidation.IsDNS1123Label(p.Namespace); len(problems) > 0 {
			return nil, fmt.Errorf("%w: invalid namespace %q: %s", ErrValidation, p.Namespace, strings.Join(problems, "; "))
		}
	}
	if p.Repo != "" && !validRepo(p.Repo) {
		return nil, fmt.Errorf("%w: repo must be GitHub owner/name, got %q", ErrValidation, p.Repo)
	}
	if p.DefaultHarness != "" && !validHarness(p.DefaultHarness) {
		return nil, fmt.Errorf("%w: unsupported default harness %q", ErrValidation, p.DefaultHarness)
	}
	if p.RuntimeClass != "" && !validRuntime(p.RuntimeClass) {
		return nil, fmt.Errorf("%w: unsupported runtime class %q", ErrValidation, p.RuntimeClass)
	}
	// repo is OPTIONAL: a repo-less project is the keyless design — its runs have
	// an empty RunSpec.Repo, so hydrate's clone and finalize's PR are both skipped
	// (see internal/podruntime). This is what `make e2e` exercises with no creds.
	if p.CreatedAt.IsZero() {
		p.CreatedAt = s.now()
	}
	if err := s.store.CreateProject(ctx, p); err != nil {
		return nil, err
	}
	return p, nil
}

// GetProject returns a project by name.
func (s *Service) GetProject(ctx context.Context, name string) (*store.Project, error) {
	return s.store.GetProject(ctx, name)
}

// ListProjects returns all projects.
func (s *Service) ListProjects(ctx context.Context) ([]*store.Project, error) {
	return s.store.ListProjects(ctx)
}

// --- Runs ---

// CreateRunRequest is a validated run submission.
type CreateRunRequest struct {
	Project     string
	User        string
	Prompt      string
	Harness     string // override
	Model       string // override
	BaseRef     string
	Interactive bool
	Runtime     string // override
	CPU         string // override
	Memory      string // override
}

// CreateRun resolves config and commits the run plus its Kubernetes publication
// intent atomically. An inline dispatch keeps the API responsive in the common
// case; the leased outbox worker replays the same idempotent operation if this
// process dies anywhere between the database commit and AgentRun publication.
func (s *Service) CreateRun(ctx context.Context, req CreateRunRequest) (*store.Run, error) {
	if strings.TrimSpace(req.Project) == "" {
		return nil, fmt.Errorf("%w: project is required", ErrValidation)
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("%w: task prompt is required", ErrValidation)
	}
	if strings.TrimSpace(req.User) == "" {
		return nil, fmt.Errorf("%w: user is required", ErrValidation)
	}
	if req.BaseRef == "" {
		req.BaseRef = "main" // PR base defaults to main when the project omits it.
	}

	proj, err := s.store.GetProject(ctx, req.Project)
	if err != nil {
		return nil, err // ErrNotFound → 404 at transport
	}

	eff := s.resolve(proj, req)

	// Fail loud, not silent: a run resolved to a namespace missing the harness's
	// credential Secret would otherwise start a pod that gets no credential
	// injected (the egress-proxy mounts them Optional) and fail minutes later,
	// far from the real cause.
	if err := s.checkCredentials(ctx, req, eff); err != nil {
		return nil, err
	}

	id := s.idgen()
	ns := eff.namespace

	run, err := buildAgentRun(id, ns, req, eff)
	if err != nil {
		return nil, err
	}

	rec := &store.Run{
		ID:          id,
		Project:     req.Project,
		User:        req.User,
		Prompt:      req.Prompt,
		Harness:     eff.harness,
		Model:       eff.model,
		BaseRef:     req.BaseRef,
		Interactive: req.Interactive,
		Runtime:     eff.runtime,
		Namespace:   ns,
		Phase:       string(wrenv1.PhasePending),
		CreatedAt:   s.now(),
	}
	payload, err := json.Marshal(run)
	if err != nil {
		return nil, fmt.Errorf("encode launch operation: %w", err)
	}
	now := s.now().UTC()
	op := &store.Operation{
		ID: "launch/" + id, RunID: id, Kind: store.OperationLaunchRun,
		Payload: payload, State: store.OperationPending, AvailableAt: now, CreatedAt: now,
	}
	event := &store.RunEvent{
		RunID: id, Source: "control-plane", SourceID: "submitted", Type: "run.submitted",
		Payload: mustJSON(map[string]any{"phase": rec.Phase, "project": rec.Project, "user": rec.User}), CreatedAt: now,
	}
	if err := store.CreateRunWithOperation(ctx, s.store, rec, op, event); err != nil {
		return nil, err
	}
	// Best effort only: a transient cluster error is now durable pending work,
	// not a reason to delete the sole record of the user's accepted request.
	if s.inlineDispatch {
		_, _ = s.DispatchPending(ctx, "inline-"+id, 1)
	}
	return rec, nil
}

// GetRun returns a run, refreshing phase/PR/restartCount from its CR.
func (s *Service) GetRun(ctx context.Context, id string) (*store.Run, error) {
	rec, err := s.store.GetRun(ctx, id)
	if err != nil {
		return nil, err
	}
	cr, err := s.launcher.GetRun(ctx, rec.Namespace, id)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return rec, nil // CR gone; return last-known store record
		}
		return nil, err
	}
	changed := false
	if p := string(cr.Status.Phase); p != "" && p != rec.Phase {
		rec.Phase, changed = p, true
	}
	if cr.Status.RestartCount != rec.RestartCount {
		rec.RestartCount, changed = cr.Status.RestartCount, true
	}
	if url := cr.Status.PR.URL; url != "" && url != rec.PRURL {
		rec.PRURL, changed = url, true
	}
	checkpoint := checkpointFromStatus(cr.Status.LastCheckpoint)
	if checkpoint != nil && !reflect.DeepEqual(checkpoint, rec.LastCheckpoint) {
		rec.LastCheckpoint, changed = checkpoint, true
	}
	conditions := conditionsFromStatus(cr.Status.Conditions)
	if len(conditions) > 0 && !reflect.DeepEqual(conditions, rec.Conditions) {
		rec.Conditions, changed = conditions, true
	}
	if changed {
		_ = store.UpsertRunWithEvent(ctx, s.store, rec, eventFromCR(cr, rec, s.now()))
	}
	return rec, nil
}

// DeleteRun removes a run entirely: its AgentRun CR (whose owner references
// cascade the pod/PVC/ConfigMap cleanup) and its store record. The store record
// must exist (ErrNotFound otherwise); a CR already gone is tolerated by the
// launcher (`wren run rm`).
func (s *Service) DeleteRun(ctx context.Context, id string) error {
	rec, err := s.store.GetRun(ctx, id)
	if err != nil {
		return err
	}
	if err := s.launcher.DeleteRun(ctx, rec.Namespace, id); err != nil {
		return fmt.Errorf("delete AgentRun: %w", err)
	}
	return s.store.DeleteRun(ctx, id)
}

// StopRun cancels a run without deleting it: it asks the operator (via the
// cancel annotation) to delete the pod and drive the run to Canceled — a
// terminal state the reconciler does NOT auto-resume, unlike a crash. The store
// record is kept, so the run stays visible in `wren run list/get`.
func (s *Service) StopRun(ctx context.Context, id string) error {
	rec, err := s.store.GetRun(ctx, id)
	if err != nil {
		return err
	}
	return s.launcher.RequestCancel(ctx, rec.Namespace, id)
}

// PauseRun requests a durable pause. The live CR is authoritative: accepting a
// pause from a stale store phase could checkpoint an already-completed pod.
// Checkpoint storage is mandatory because Paused promises recoverable state.
func (s *Service) PauseRun(ctx context.Context, id string) error {
	rec, err := s.store.GetRun(ctx, id)
	if err != nil {
		return err
	}
	cr, err := s.launcher.GetRun(ctx, rec.Namespace, id)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: run %q has no AgentRun to pause", ErrNotFound, id)
		}
		return fmt.Errorf("get AgentRun: %w", err)
	}
	if cr.Status.Phase == wrenv1.PhasePausing || cr.Status.Phase == wrenv1.PhasePaused {
		return nil
	}
	if cr.Status.Phase != wrenv1.PhaseRunning {
		return fmt.Errorf("%w: run %q is %s, not Running; nothing to pause", ErrValidation, id, cr.Status.Phase)
	}
	if strings.TrimSpace(cr.Spec.Workspace.Checkpoint.Bucket) == "" {
		return fmt.Errorf("%w: run %q has no checkpoint storage configured", ErrValidation, id)
	}
	storageReady := false
	for _, c := range cr.Status.Conditions {
		if c.Type == "CheckpointStorage" && c.Status == metav1.ConditionTrue {
			storageReady = true
			break
		}
	}
	if !storageReady {
		return fmt.Errorf("%w: run %q has no mounted checkpoint storage; durable pause is unavailable", ErrValidation, id)
	}
	return s.launcher.RequestPause(ctx, rec.Namespace, id)
}

// ResumeRun restarts a Paused or terminally-Failed run: it asks the operator
// (via the resume annotation) to reset the retry budget, clear any leftover
// pod, and give the reconciler a fresh attempt at the run — a deliberate
// human/automated decision distinct from the operator's own crash-resume.
// Only a run whose CR currently reports PhaseFailed is eligible; the CR (not
// the possibly-stale store record) is authoritative here so a request racing
// a phase transition sees the true current state. Anything else — Running,
// Pending, Succeeded, Canceled, or an already-resumed run that has moved past
// Failed — is a validation error: resuming a run that isn't stuck is not a
// silent no-op, it's a clear rejection (spec deferred "run resume" note).
func (s *Service) ResumeRun(ctx context.Context, id string) error {
	rec, err := s.store.GetRun(ctx, id)
	if err != nil {
		return err
	}
	cr, err := s.launcher.GetRun(ctx, rec.Namespace, id)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: run %q has no AgentRun to resume (already deleted?)", ErrNotFound, id)
		}
		return fmt.Errorf("get AgentRun: %w", err)
	}
	if cr.Status.Phase != wrenv1.PhaseFailed && cr.Status.Phase != wrenv1.PhasePaused {
		return fmt.Errorf("%w: run %q is %s, not Failed or Paused; nothing to resume", ErrValidation, id, cr.Status.Phase)
	}
	return s.launcher.RequestResume(ctx, rec.Namespace, id)
}

// ListRuns returns runs for a scope, optionally narrowed to one project.
// scope "mine" filters to user; "all"/"team" return everything (team RBAC
// narrowing is not implemented).
//
// Before reading, it syncs the store from live AgentRun CRs through the same
// bulk reconciliation used at apiserver boot. This avoids an N+1 read pattern
// while keeping fleet results current. Best-effort: a sync error
// doesn't fail the list — callers get the store's last-known state rather
// than an error for what is fundamentally a read endpoint.
func (s *Service) ListRuns(ctx context.Context, scope, user, project string) ([]*store.Run, error) {
	// Best-effort sync: a transient cluster-read error here shouldn't fail a
	// list request, it should just leave the store's last-known state as the
	// answer (same tolerance ReconcileFromCluster's boot-time caller applies).
	_, _ = s.ReconcileFromCluster(ctx)
	f := store.RunFilter{Project: project}
	if scope == "" || scope == "mine" {
		f.User = user
	}
	return s.store.ListRuns(ctx, f)
}

// ReconcileFromCluster re-learns in-flight runs from the AgentRun CRs into the
// store at apiserver boot. The CR is the source of truth for run status, so a
// restarted apiserver (especially one backed by a store it just migrated to)
// re-derives its worklist here instead of forgetting runs. It upserts every
// CR's store row and returns the number reconciled;
// individual failures are logged by the caller via the returned error slice.
func (s *Service) ReconcileFromCluster(ctx context.Context) (int, error) {
	crs, err := s.launcher.ListRuns(ctx)
	if err != nil {
		return 0, fmt.Errorf("list AgentRuns: %w", err)
	}
	n := 0
	for i := range crs {
		cr := &crs[i]
		// Merge onto any existing store row: the CR is authoritative for status,
		// but an empty CR field must not clobber known store data (same rule as
		// GetRun's mirroring — status is only written when the CR carries it).
		existing, _ := s.store.GetRun(ctx, cr.Name)
		rec := runFromCR(cr, existing)
		if err := store.UpsertRunWithEvent(ctx, s.store, rec, eventFromCR(cr, rec, s.now())); err != nil {
			return n, fmt.Errorf("upsert run %s: %w", rec.ID, err)
		}
		n++
	}
	return n, nil
}

// ListRunEvents returns the immutable audit/event stream after an optional
// cursor. The run lookup keeps unknown IDs a deliberate 404 even for stores
// whose journal implementation is absent.
func (s *Service) ListRunEvents(ctx context.Context, id string, afterID int64, limit int) ([]*store.RunEvent, error) {
	if _, err := s.store.GetRun(ctx, id); err != nil {
		return nil, err
	}
	return store.ListRunEvents(ctx, s.store, id, afterID, limit)
}

// AppendGatewayEvent ingests an event that the trusted gateway forwarded from
// the harness stream. sourceID is attempt/sequence, making a full replay after
// gateway restart harmless.
func (s *Service) AppendGatewayEvent(ctx context.Context, id, sourceID, eventType string, payload json.RawMessage, at time.Time) (bool, error) {
	if strings.TrimSpace(sourceID) == "" || strings.TrimSpace(eventType) == "" {
		return false, fmt.Errorf("%w: sourceId and type are required", ErrValidation)
	}
	if _, err := s.store.GetRun(ctx, id); err != nil {
		return false, err
	}
	if at.IsZero() {
		at = s.now()
	}
	return store.AppendRunEvent(ctx, s.store, &store.RunEvent{
		RunID: id, Source: "gateway", SourceID: sourceID, Type: eventType,
		Payload: append([]byte(nil), payload...), CreatedAt: at.UTC(),
	})
}

func eventFromCR(cr *wrenv1.AgentRun, rec *store.Run, at time.Time) *store.RunEvent {
	payload := mustJSON(map[string]any{
		"phase": rec.Phase, "restartCount": rec.RestartCount, "prUrl": rec.PRURL,
		"lastCheckpoint": rec.LastCheckpoint, "conditions": rec.Conditions,
	})
	sourceID := "resource-version/" + cr.ResourceVersion
	if cr.ResourceVersion == "" {
		sum := sha256.Sum256(payload)
		sourceID = "snapshot/" + hex.EncodeToString(sum[:])
	}
	return &store.RunEvent{RunID: rec.ID, Source: "kubernetes", SourceID: sourceID, Type: "run.snapshot", Payload: payload, CreatedAt: at.UTC()}
}

func mustJSON(value any) json.RawMessage {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err) // all current callers pass fixed, JSON-safe DTOs
	}
	return b
}

// runFromCR maps an AgentRun CR onto a store.Run for reconcile-on-boot. The CR
// spec carries the immutable submission fields; the CR status is authoritative
// for phase/PR/restartCount, but only overwrites the prior store row when it
// actually carries a value (an unstarted CR has empty status — we keep the
// store's last-known phase). Fields the CR does not carry at all (the original
// submission timestamp) fall back to the store row, then to the CR creation
// time. prior may be nil (run unknown to the store).
func runFromCR(cr *wrenv1.AgentRun, prior *store.Run) *store.Run {
	rec := &store.Run{
		ID:             cr.Name,
		Project:        cr.Spec.Project,
		User:           cr.Spec.User,
		Prompt:         cr.Spec.Task.Prompt,
		Harness:        string(cr.Spec.Harness.Kind),
		Model:          cr.Spec.Harness.Model,
		BaseRef:        cr.Spec.Task.BaseRef,
		Interactive:    cr.Spec.Interactive,
		Runtime:        string(cr.Spec.Sandbox.RuntimeClass),
		Namespace:      cr.Namespace,
		Phase:          string(cr.Status.Phase),
		PRURL:          cr.Status.PR.URL,
		RestartCount:   cr.Status.RestartCount,
		LastCheckpoint: checkpointFromStatus(cr.Status.LastCheckpoint),
		Conditions:     conditionsFromStatus(cr.Status.Conditions),
		CreatedAt:      cr.CreationTimestamp.Time,
	}
	if prior != nil {
		if rec.Phase == "" {
			rec.Phase = prior.Phase
		}
		if rec.PRURL == "" {
			rec.PRURL = prior.PRURL
		}
		if rec.LastCheckpoint == nil {
			rec.LastCheckpoint = prior.LastCheckpoint
		}
		if len(rec.Conditions) == 0 {
			rec.Conditions = append([]store.RunCondition(nil), prior.Conditions...)
		}
		if !prior.CreatedAt.IsZero() {
			rec.CreatedAt = prior.CreatedAt // keep the true submission time
		}
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now()
	}
	return rec
}

func checkpointFromStatus(in *wrenv1.CheckpointRef) *store.RunCheckpoint {
	if in == nil {
		return nil
	}
	return &store.RunCheckpoint{ID: in.ID, URI: in.URI, At: in.At.Time, SHA256: in.SHA256, SizeBytes: in.SizeBytes, FormatVersion: in.FormatVersion, Trigger: in.Trigger}
}

func conditionsFromStatus(in []metav1.Condition) []store.RunCondition {
	out := make([]store.RunCondition, 0, len(in))
	for _, c := range in {
		out = append(out, store.RunCondition{Type: c.Type, Status: string(c.Status), Reason: c.Reason, Message: c.Message, LastTransitionTime: c.LastTransitionTime.Time})
	}
	return out
}

// --- internals ---

type effectiveConfig struct {
	repo      string
	harness   string
	image     string
	model     string
	runtime   string
	cpu       string
	memory    string
	disk      string
	bucket    string
	allowlist []string
	namespace string
}

func (s *Service) resolve(p *store.Project, req CreateRunRequest) effectiveConfig {
	// Namespace resolution: an explicit per-project --namespace wins (multi-tenant
	// isolation), then the install-configured shared default, then
	// the per-user prefix fallback for installs that set neither.
	ns := firstNonEmpty(p.Namespace, s.defaults.DefaultNamespace, s.defaults.NamespacePrefix+sanitizeLabel(req.User))
	return effectiveConfig{
		repo:      p.Repo,
		harness:   firstNonEmpty(req.Harness, p.DefaultHarness, s.defaults.Harness),
		image:     firstNonEmpty(p.HarnessImage, s.defaults.HarnessImage),
		model:     firstNonEmpty(req.Model, p.DefaultModel, s.defaults.Model),
		runtime:   firstNonEmpty(req.Runtime, p.RuntimeClass, s.defaults.RuntimeClass),
		cpu:       firstNonEmpty(req.CPU, p.CPU, s.defaults.CPU),
		memory:    firstNonEmpty(req.Memory, p.Memory, s.defaults.Memory),
		disk:      firstNonEmpty(p.Disk, s.defaults.Disk),
		bucket:    firstNonEmpty(p.CheckpointBucket, s.defaults.CheckpointBucket),
		allowlist: p.EgressAllowlist,
		namespace: ns,
	}
}

// secretNeed is a credential Secret the resolved run requires in its namespace.
type secretNeed struct {
	secret string // Secret name (mirrors the operator's --*-secret flags)
	key    string // key within the Secret
	human  string // human-readable label for the error message
}

// requiredSecrets returns the credential Secrets the resolved run needs present
// in its namespace before a pod is worth scheduling. The mock harness and a
// keyless (no-repo) project legitimately need nothing. A repo needs the GitHub
// token (private clone + PR); a model harness needs its provider key on the
// route the egress-proxy injects (claude-code/opencode → Anthropic, codex →
// OpenAI). byo brings its own credentials, so only the repo token is required.
func (s *Service) requiredSecrets(eff effectiveConfig) []secretNeed {
	if eff.harness == "mock" {
		return nil
	}
	var needs []secretNeed
	if eff.repo != "" && s.defaults.GitHubTokenSecret != "" {
		needs = append(needs, secretNeed{s.defaults.GitHubTokenSecret, "token", "GitHub token"})
	}
	switch eff.harness {
	case "claude-code", "opencode":
		if s.defaults.AnthropicKeySecret != "" {
			needs = append(needs, secretNeed{s.defaults.AnthropicKeySecret, "key", "Anthropic API key"})
		}
	case "codex":
		if s.defaults.OpenAIKeySecret != "" {
			needs = append(needs, secretNeed{s.defaults.OpenAIKeySecret, "key", "OpenAI API key"})
		}
	}
	return needs
}

// checkCredentials rejects a submission whose resolved namespace is missing a
// Secret the run needs, turning a silent multi-minute downstream failure into an
// immediate, actionable validation response. It is best-effort: a transient API
// error checking a Secret does not block the run (the pod path still has the
// egress-proxy's Optional-secret behavior as a backstop).
func (s *Service) checkCredentials(ctx context.Context, req CreateRunRequest, eff effectiveConfig) error {
	for _, need := range s.requiredSecrets(eff) {
		ok, err := s.launcher.SecretHasKey(ctx, eff.namespace, need.secret, need.key)
		if err != nil {
			continue // don't turn an API blip into a hard submit failure
		}
		if !ok {
			return fmt.Errorf("%w: project %q needs a %s in namespace %q (Secret %q key %q), but it is missing%s",
				ErrValidation, req.Project, need.human, eff.namespace, need.secret, need.key, s.credentialHint(eff.namespace))
		}
	}
	return nil
}

// credentialHint points the caller at the likely fix: the install's
// --run-namespace (where `wren install` writes the proxy Secrets) when the run
// resolved elsewhere, else re-running install with credentials.
func (s *Service) credentialHint(ns string) string {
	if def := s.defaults.DefaultNamespace; def != "" && def != ns {
		return fmt.Sprintf(" — did you mean --namespace %q (the install's --run-namespace, where `wren install` stores the proxy credentials)?", def)
	}
	return fmt.Sprintf(" — re-run `wren install` with credentials, or add them: kubectl -n %s create secret generic …", ns)
}

// buildAgentRun maps the effective config onto an AgentRun custom resource.
func buildAgentRun(id, ns string, req CreateRunRequest, eff effectiveConfig) (*wrenv1.AgentRun, error) {
	if !validHarness(eff.harness) {
		return nil, fmt.Errorf("%w: unsupported harness %q", ErrValidation, eff.harness)
	}
	if !validRuntime(eff.runtime) {
		return nil, fmt.Errorf("%w: unsupported runtime class %q", ErrValidation, eff.runtime)
	}
	if eff.harness != "mock" && strings.TrimSpace(eff.image) == "" {
		return nil, fmt.Errorf("%w: harness image is required for %q", ErrValidation, eff.harness)
	}
	cpu, err := resource.ParseQuantity(eff.cpu)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid cpu %q", ErrValidation, eff.cpu)
	}
	if cpu.Sign() <= 0 {
		return nil, fmt.Errorf("%w: cpu must be positive, got %q", ErrValidation, eff.cpu)
	}
	mem, err := resource.ParseQuantity(eff.memory)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid memory %q", ErrValidation, eff.memory)
	}
	if mem.Sign() <= 0 {
		return nil, fmt.Errorf("%w: memory must be positive, got %q", ErrValidation, eff.memory)
	}
	disk, err := resource.ParseQuantity(eff.disk)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid disk %q", ErrValidation, eff.disk)
	}
	if disk.Sign() <= 0 {
		return nil, fmt.Errorf("%w: disk must be positive, got %q", ErrValidation, eff.disk)
	}
	return &wrenv1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      id,
			Namespace: ns,
			Labels: map[string]string{
				"wren.dev/run":     id,
				"wren.dev/project": req.Project,
			},
		},
		Spec: wrenv1.AgentRunSpec{
			Project: req.Project,
			Repo:    eff.repo,
			User:    req.User,
			Harness: wrenv1.HarnessSpec{
				Kind:  wrenv1.HarnessKind(eff.harness),
				Image: eff.image,
				Model: eff.model,
			},
			Task:        wrenv1.TaskSpec{Prompt: req.Prompt, BaseRef: req.BaseRef},
			Interactive: req.Interactive,
			Sandbox: wrenv1.SandboxSpec{
				RuntimeClass: wrenv1.RuntimeClass(eff.runtime),
				Resources:    wrenv1.ResourceSpec{CPU: cpu, Memory: mem, EphemeralDisk: disk},
			},
			Workspace: wrenv1.WorkspaceSpec{
				PVC:        wrenv1.PVCSpec{Size: disk},
				Checkpoint: wrenv1.CheckpointSpec{Bucket: eff.bucket},
			},
			Egress: wrenv1.EgressSpec{Allowlist: eff.allowlist},
		},
	}, nil
}

var repoPart = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)

func validRepo(repo string) bool {
	parts := strings.Split(repo, "/")
	return len(parts) == 2 && repoPart.MatchString(parts[0]) && repoPart.MatchString(parts[1])
}

func validHarness(h string) bool {
	switch h {
	case "mock", "claude-code", "codex", "opencode", "byo":
		return true
	default:
		return false
	}
}

func validRuntime(runtime string) bool {
	switch runtime {
	case "runc", "gvisor", "kata":
		return true
	default:
		return false
	}
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

var nonLabel = regexp.MustCompile(`[^a-z0-9]+`)

// sanitizeLabel turns an arbitrary identity (e.g. an email) into a DNS-1123
// label suitable for a namespace suffix.
func sanitizeLabel(s string) string {
	s = strings.ToLower(s)
	s = nonLabel.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "anon"
	}
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

func genRunID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "r-" + hex.EncodeToString(b[:])
}
