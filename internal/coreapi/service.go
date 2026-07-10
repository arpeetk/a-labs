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
	NamespacePrefix  string // e.g. "user-" → namespace "user-<sanitized-user>"
}

// DefaultDefaults returns the built-in fallback configuration.
func DefaultDefaults() Defaults {
	return Defaults{
		Harness:          "claude-code",
		HarnessImage:     "wren/claude-code-runner:latest",
		Model:            "claude-opus-4-8",
		RuntimeClass:     "runc",
		CPU:              "2",
		Memory:           "4Gi",
		Disk:             "10Gi",
		CheckpointBucket: "gs://wren-ckpt",
		NamespacePrefix:  "user-",
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
	if strings.TrimSpace(p.Repo) == "" {
		return nil, fmt.Errorf("%w: project repo is required", ErrValidation)
	}
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

// ListRuns returns runs for a scope. scope "mine" filters to user; "all"/"team"
// return everything (team RBAC narrowing lands in M1).
func (s *Service) ListRuns(ctx context.Context, scope, user string) ([]*store.Run, error) {
	f := store.RunFilter{}
	if scope == "" || scope == "mine" {
		f.User = user
	}
	return s.store.ListRuns(ctx, f)
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
	ns := firstNonEmpty(p.Namespace, s.defaults.NamespacePrefix+sanitizeLabel(req.User))
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
