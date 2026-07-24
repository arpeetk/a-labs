// Package coreapi is the control plane's business logic: the Projects and Runs
// services (spec §5.2). It validates requests, resolves effective run config
// (project defaults ⊕ request overrides), maps a submission onto an AgentRun
// custom resource, and mirrors CR status back into the store.
package coreapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

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
	// credential Secrets. Empty falls back to NamespacePrefix (WS-15 Part A).
	DefaultNamespace string
	NamespacePrefix  string // e.g. "user-" → namespace "user-<sanitized-user>"
	// GitHubTokenSecret / AnthropicKeySecret / OpenAIKeySecret name the proxy
	// credential Secrets the run's namespace must hold before a pod is worth
	// scheduling. They mirror the operator's --github-token-secret /
	// --anthropic-key-secret / --openai-key-secret defaults; the pre-flight
	// credential check (WS-15 Part A) reads them.
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
	store    store.Store
	launcher launcher.Launcher
	defaults Defaults
	now      func() time.Time
	idgen    func() string
}

// New builds a Service.
func New(s store.Store, l launcher.Launcher, d Defaults) *Service {
	return &Service{store: s, launcher: l, defaults: d, now: time.Now, idgen: genRunID}
}

// --- Projects ---

// CreateProject validates and persists a project.
func (s *Service) CreateProject(ctx context.Context, p *store.Project) (*store.Project, error) {
	if strings.TrimSpace(p.Name) == "" {
		return nil, fmt.Errorf("%w: project name is required", ErrValidation)
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

// CreateRun resolves config, creates the AgentRun CR, and records the run.
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
		req.BaseRef = "main" // M0: PR base defaults to main
	}

	proj, err := s.store.GetProject(ctx, req.Project)
	if err != nil {
		return nil, err // ErrNotFound → 404 at transport
	}

	eff := s.resolve(proj, req)

	// Fail loud, not silent: a run resolved to a namespace missing the harness's
	// credential Secret would otherwise start a pod that gets no credential
	// injected (the egress-proxy mounts them Optional) and fail minutes later,
	// far from the real cause (WS-15 Part A).
	if err := s.checkCredentials(ctx, req, eff); err != nil {
		return nil, err
	}

	id := s.idgen()
	ns := eff.namespace

	run, err := buildAgentRun(id, ns, req, eff)
	if err != nil {
		return nil, err
	}

	if err := s.launcher.EnsureNamespace(ctx, ns); err != nil {
		return nil, fmt.Errorf("ensure namespace: %w", err)
	}
	if err := s.launcher.CreateRun(ctx, run); err != nil {
		return nil, fmt.Errorf("create AgentRun: %w", err)
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
	if err := s.store.CreateRun(ctx, rec); err != nil {
		return nil, err
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
	if changed {
		_ = s.store.UpdateRun(ctx, rec)
	}
	return rec, nil
}

// DeleteRun removes a run entirely: its AgentRun CR (whose owner references
// cascade the pod/PVC/ConfigMap cleanup) and its store record. The store record
// must exist (ErrNotFound otherwise); a CR already gone is tolerated by the
// launcher (`wren run rm`, WS-15 Part C).
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
// record is kept (the run stays visible in `wren run list/get`). WS-15 Part C.
func (s *Service) StopRun(ctx context.Context, id string) error {
	rec, err := s.store.GetRun(ctx, id)
	if err != nil {
		return err
	}
	return s.launcher.RequestCancel(ctx, rec.Namespace, id)
}

// ListRuns returns runs for a scope. scope "mine" filters to user; "all"/"team"
// return everything (team RBAC narrowing lands in M1).
func (s *Service) ListRuns(ctx context.Context, scope, user string) ([]*store.Run, error) {
	f := store.RunFilter{}
	if scope == "" || scope == "mine" {
		f.User = user
	}
	return s.store.ListRuns(ctx, f)
}

// ReconcileFromCluster re-learns in-flight runs from the AgentRun CRs into the
// store at apiserver boot. The CR is the source of truth for run status, so a
// restarted apiserver (especially one backed by a store it just migrated to)
// re-derives its worklist here instead of forgetting runs (implementation-plan
// §WS-3). It upserts every CR's store row and returns the number reconciled;
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
		if err := store.UpsertRun(ctx, s.store, rec); err != nil {
			return n, fmt.Errorf("upsert run %s: %w", rec.ID, err)
		}
		n++
	}
	return n, nil
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
		ID:           cr.Name,
		Project:      cr.Spec.Project,
		User:         cr.Spec.User,
		Prompt:       cr.Spec.Task.Prompt,
		Harness:      string(cr.Spec.Harness.Kind),
		Model:        cr.Spec.Harness.Model,
		BaseRef:      cr.Spec.Task.BaseRef,
		Interactive:  cr.Spec.Interactive,
		Runtime:      string(cr.Spec.Sandbox.RuntimeClass),
		Namespace:    cr.Namespace,
		Phase:        string(cr.Status.Phase),
		PRURL:        cr.Status.PR.URL,
		RestartCount: cr.Status.RestartCount,
		CreatedAt:    cr.CreationTimestamp.Time,
	}
	if prior != nil {
		if rec.Phase == "" {
			rec.Phase = prior.Phase
		}
		if rec.PRURL == "" {
			rec.PRURL = prior.PRURL
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
	// isolation), then the install-configured shared default (WS-15 Part A), then
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
// immediate, actionable 400 (WS-15 Part A). It is best-effort: a transient API
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
	cpu, err := resource.ParseQuantity(eff.cpu)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid cpu %q", ErrValidation, eff.cpu)
	}
	mem, err := resource.ParseQuantity(eff.memory)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid memory %q", ErrValidation, eff.memory)
	}
	disk, err := resource.ParseQuantity(eff.disk)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid disk %q", ErrValidation, eff.disk)
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
