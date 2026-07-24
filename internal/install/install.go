// Package install implements `wren install` / `wren uninstall`: standing up the
// Wren control plane on an existing Kubernetes cluster (GKE or kind) without a
// repo checkout or a kustomize binary. The rendered deployment (config/default)
// is embedded as an asset; config/ stays the source of truth and `make
// check-assets` fails CI if the two drift.
//
// Onboarding is product surface (code standards rule 8): the flow lives here as
// a first-class CLI command, not in hack/. The orchestration depends on two
// seams — Kube (cluster operations) and Runner (local tools: docker/kind/gh) —
// with real implementations in kube.go/run.go and fakes in fake.go.
package install

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/summiteight/wren/internal/install/assets"
)

// Fixed coordinates of the rendered deployment. These must match the embedded
// asset (config/default); TestAssetMatchesConstants fails if config/ drifts.
const (
	// SystemNamespace is where the operator + apiserver Deployments live.
	SystemNamespace = "wren-system"
	// OperatorDeployment / ApiserverDeployment are waited on after apply.
	OperatorDeployment  = "wren-operator"
	ApiserverDeployment = "wren-apiserver"
	// ApiserverService is the control-plane front door (port-forward target).
	ApiserverService = "wren-apiserver"
	// GitHubTokenSecret / AnthropicKeySecret mirror the operator's
	// --github-token-secret / --anthropic-key-secret defaults; the egress-proxy
	// reads them in the run namespace (keys "token"/"key").
	GitHubTokenSecret  = "wren-github-token"
	AnthropicKeySecret = "wren-anthropic-key"
)

// minServerVersion is the floor the preflight enforces (kind e2e and GKE both
// run well past it; older servers lack the SSA/admission behavior config/
// relies on).
var minServerVersion = [2]int{1, 27}

// Options carries every resolved install decision. The CLI layer fills it from
// flags + env; nothing here is read from the environment directly.
type Options struct {
	// KubeContext selects the kubeconfig context. Empty with KindCluster set
	// targets kind-<cluster>; empty without it targets the current context.
	KubeContext string
	// Registry, when set, is an image-prefix (e.g. an Artifact Registry path):
	// the three images are built for linux/amd64, pushed, and the Deployments
	// overridden to point at them (the hack/e2e-gke.sh pattern, in Go).
	Registry string
	// KindCluster, when set, selects the local-eval path: the kind cluster is
	// created if absent and the images are built and `kind load`ed into it.
	KindCluster string
	// ImageTag tags pushed registry images. Empty resolves to the source
	// tree's short git SHA, falling back to "dev".
	ImageTag string
	// SrcDir is the repo checkout the Docker builds run against.
	SrcDir string
	// Expose LoadBalancer switches the apiserver Service type for team setups;
	// the default ClusterIP stays port-forward-only (the apiserver's only auth
	// is the X-Wren-User header stand-in — do not expose it publicly).
	Expose string
	// RunNamespace is where the proxy credential Secrets are created and where
	// credentialed projects should point their `namespace` field.
	RunNamespace string
	// GitHubToken / AnthropicKey are pre-resolved credentials (env). When
	// empty and a Prompter is available the installer asks interactively.
	GitHubToken  string
	AnthropicKey string
	// SkipCredentials turns the credential step off entirely (keyless eval).
	SkipCredentials bool
	// WaitTimeout bounds the wait for the control-plane Deployments.
	WaitTimeout time.Duration
}

// UninstallOptions carries the resolved `wren uninstall` decisions.
type UninstallOptions struct {
	KubeContext  string
	RunNamespace string
	// WaitTimeout bounds the wait for namespace deletion.
	WaitTimeout time.Duration
}

func (o *Options) defaults() {
	if o.SrcDir == "" {
		o.SrcDir = "."
	}
	if o.RunNamespace == "" {
		o.RunNamespace = "wren-runs"
	}
	if o.WaitTimeout <= 0 {
		o.WaitTimeout = 3 * time.Minute
	}
}

func (o *UninstallOptions) defaults() {
	if o.RunNamespace == "" {
		o.RunNamespace = "wren-runs"
	}
	if o.WaitTimeout <= 0 {
		o.WaitTimeout = 2 * time.Minute
	}
}

// contextName resolves which kube context the install targets.
func (o *Options) contextName() string {
	if o.KubeContext != "" {
		return o.KubeContext
	}
	if o.KindCluster != "" {
		return "kind-" + o.KindCluster
	}
	return "" // current context
}

// Kube abstracts the cluster operations install needs. Real impl: client-go
// (kube.go). The interface is deliberately operation-shaped — the Installer
// never sees a typed client — so the Fake can model an entire cluster.
type Kube interface {
	// ServerVersion returns the cluster's Kubernetes version, e.g. "1.31".
	ServerVersion(ctx context.Context) (string, error)
	// ApplyManifests server-side-applies a multi-doc YAML stream (idempotent).
	ApplyManifests(ctx context.Context, manifests []byte) error
	// EnsureNamespace creates the namespace if absent.
	EnsureNamespace(ctx context.Context, name string) error
	// UpsertSecret creates or replaces a Secret's data.
	UpsertSecret(ctx context.Context, ns, name string, data map[string]string) error
	// OverrideImages points the control-plane Deployments at pushed images and
	// appends/replaces the operator's --runtime-image arg (last flag wins).
	OverrideImages(ctx context.Context, registry, tag string) error
	// SetServiceType patches the apiserver Service's type (e.g. LoadBalancer).
	SetServiceType(ctx context.Context, ns, name, svcType string) error
	// WaitDeployments blocks until each named Deployment is Available.
	WaitDeployments(ctx context.Context, ns string, names []string, timeout time.Duration) error
	// DeleteNamespace deletes a namespace and waits for it to be gone.
	DeleteNamespace(ctx context.Context, name string, timeout time.Duration) error
	// DeleteClusterScoped deletes every cluster-scoped object in a rendered
	// manifest stream (CRDs, cluster RBAC), tolerating absence.
	DeleteClusterScoped(ctx context.Context, manifests []byte) error
}

// Runner executes local tools. Real impl: os/exec (run.go).
type Runner interface {
	// LookPath reports whether the tool is on PATH.
	LookPath(name string) bool
	// Run streams the command's output to the Installer's writer.
	Run(ctx context.Context, name string, args ...string) error
	// Output captures stdout (used for `gh auth token` — never streamed).
	Output(ctx context.Context, name string, args ...string) (string, error)
}

// Installer runs the install/uninstall flow against a Kube and a Runner.
type Installer struct {
	Kube   Kube
	Runner Runner
	// Out receives progress + the engineer hand-off. Secrets are never written.
	Out io.Writer
	// PromptSecret, when non-nil, asks for a credential interactively; the CLI
	// wires it to a no-echo terminal read. Nil means non-interactive: missing
	// credentials are skipped with a note instead of blocking.
	PromptSecret func(label string) (string, error)
}

// Install executes the full flow: preflight → cluster → manifests → images →
// credentials → wait → hand-off. Safe to re-run (idempotent throughout).
func (in *Installer) Install(ctx context.Context, opts Options) error {
	opts.defaults()
	if opts.Registry == "" && opts.KindCluster == "" {
		return errors.New("one of --registry (build + push images for a real cluster) or --kind (build + load into kind) is required")
	}
	if opts.Registry != "" && opts.KindCluster != "" {
		return errors.New("--registry and --kind are mutually exclusive")
	}
	if opts.Expose != "" && opts.Expose != "LoadBalancer" {
		return fmt.Errorf("--expose must be LoadBalancer or empty, got %q", opts.Expose)
	}

	st := &steps{in: in, opts: opts}
	if err := st.preflight(ctx); err != nil {
		return err
	}
	if err := st.ensureKind(ctx); err != nil {
		return err
	}
	if err := st.checkServer(ctx); err != nil {
		return err
	}
	if err := st.applyManifests(ctx); err != nil {
		return err
	}
	if err := st.images(ctx); err != nil {
		return err
	}
	if err := st.credentials(ctx); err != nil {
		return err
	}
	if opts.Expose != "" {
		if err := in.Kube.SetServiceType(ctx, SystemNamespace, ApiserverService, opts.Expose); err != nil {
			return fmt.Errorf("expose apiserver: %w", err)
		}
	}
	st.logf("waiting for the control plane to become Ready")
	if err := in.Kube.WaitDeployments(ctx, SystemNamespace,
		[]string{OperatorDeployment, ApiserverDeployment}, opts.WaitTimeout); err != nil {
		return fmt.Errorf("control plane did not become Ready: %w\nremedy: kubectl -n %s logs deploy/%s deploy/%s --tail=100",
			err, SystemNamespace, OperatorDeployment, ApiserverDeployment)
	}
	st.handOff()
	return nil
}

// Uninstall removes the install: the system + run namespaces and every
// cluster-scoped object the install created (CRDs — deleting them deletes
// every AgentRun/AgentPool cluster-wide — and cluster RBAC). The CLI gates
// this behind --confirm.
func (in *Installer) Uninstall(ctx context.Context, opts UninstallOptions) error {
	opts.defaults()
	if err := in.Kube.DeleteNamespace(ctx, SystemNamespace, opts.WaitTimeout); err != nil {
		return fmt.Errorf("delete namespace %s: %w", SystemNamespace, err)
	}
	if err := in.Kube.DeleteNamespace(ctx, opts.RunNamespace, opts.WaitTimeout); err != nil {
		return fmt.Errorf("delete namespace %s: %w", opts.RunNamespace, err)
	}
	// Asset-driven (not a name list): uninstall removes exactly what install
	// applied, so the two can never drift as config/default grows.
	if err := in.Kube.DeleteClusterScoped(ctx, assets.Manifests); err != nil {
		return fmt.Errorf("delete cluster-scoped resources: %w", err)
	}
	fmt.Fprintf(in.Out, "wren uninstalled: namespaces %s, %s and cluster-scoped resources (CRDs, cluster RBAC) removed\n",
		SystemNamespace, opts.RunNamespace)
	return nil
}

// steps holds per-install state so the phases share the resolved options.
type steps struct {
	in   *Installer
	opts Options
	tag  string // resolved image tag (registry path), for the hand-off hint
}

func (s *steps) logf(format string, args ...any) {
	fmt.Fprintf(s.in.Out, "==> "+format+"\n", args...)
}

// preflight checks the local tools with actionable remediation (code standards
// rule 2: validation errors fail loud, to stderr, with the fix).
func (s *steps) preflight(ctx context.Context) error {
	r := s.in.Runner
	if !r.LookPath("kubectl") {
		return errors.New("kubectl not found on PATH\nremedy: brew install kubectl (or https://kubernetes.io/docs/tasks/tools/)")
	}
	if !r.LookPath("docker") {
		return errors.New("docker not found on PATH\nremedy: install Docker Desktop (https://docs.docker.com/desktop/) — install builds the runtime/operator/apiserver images")
	}
	// Output, not Run: `docker info` is a health probe — its ~40-line report is
	// noise in the install transcript, so it is captured and discarded.
	if _, err := r.Output(ctx, "docker", "info"); err != nil {
		return errors.New("docker daemon is not reachable\nremedy: start Docker Desktop (or your container runtime) and re-run `wren install`")
	}
	if s.opts.KindCluster != "" && !r.LookPath("kind") {
		return errors.New("kind not found on PATH\nremedy: brew install kind (or https://kind.sigs.k8s.io/docs/user/quick-start/#installation)")
	}
	return nil
}

// ensureKind creates the kind cluster on the local-eval path. An existing
// cluster is reused, keeping re-installs idempotent.
func (s *steps) ensureKind(ctx context.Context) error {
	name := s.opts.KindCluster
	if name == "" {
		return nil
	}
	out, err := s.in.Runner.Output(ctx, "kind", "get", "clusters")
	if err != nil {
		return fmt.Errorf("list kind clusters: %w", err)
	}
	for _, c := range strings.Fields(out) {
		if c == name {
			s.logf("reusing existing kind cluster %q", name)
			return nil
		}
	}
	s.logf("creating kind cluster %q", name)
	if err := s.in.Runner.Run(ctx, "kind", "create", "cluster", "--name", name, "--wait", "120s"); err != nil {
		return fmt.Errorf("create kind cluster %q: %w", name, err)
	}
	return nil
}

// checkServer proves the context is reachable and enforces the version floor.
func (s *steps) checkServer(ctx context.Context) error {
	v, err := s.in.Kube.ServerVersion(ctx)
	if err != nil {
		return fmt.Errorf("cannot reach the cluster via context %q: %w\nremedy: check `kubectl config get-contexts`, your VPN, or re-run `gcloud container clusters get-credentials …`", s.contextLabel(), err)
	}
	maj, min, err := parseServerVersion(v)
	if err != nil {
		return fmt.Errorf("unparsable server version %q: %w", v, err)
	}
	if maj < minServerVersion[0] || (maj == minServerVersion[0] && min < minServerVersion[1]) {
		return fmt.Errorf("cluster runs Kubernetes %d.%d; wren needs ≥ %d.%d\nremedy: upgrade the cluster (GKE: --cluster-version) or use a newer kind node image",
			maj, min, minServerVersion[0], minServerVersion[1])
	}
	s.logf("cluster %q reachable (Kubernetes %d.%d)", s.contextLabel(), maj, min)
	return nil
}

func (s *steps) contextLabel() string {
	if c := s.opts.contextName(); c != "" {
		return c
	}
	return "<current>"
}

// parseServerVersion extracts major/minor from a discovery version string,
// tolerating vendor suffixes ("1.31+", "1.29.5-gke.100").
func parseServerVersion(v string) (int, int, error) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	m := regexp.MustCompile(`^(\d+)\.(\d+)`).FindStringSubmatch(v)
	if m == nil {
		return 0, 0, fmt.Errorf("want <major>.<minor>, got %q", v)
	}
	maj, _ := strconv.Atoi(m[1])
	min, _ := strconv.Atoi(m[2])
	return maj, min, nil
}

// applyManifests server-side-applies the embedded render of config/default.
func (s *steps) applyManifests(ctx context.Context) error {
	s.logf("applying CRDs, RBAC, and the control-plane Deployments (embedded config/default render)")
	if err := s.in.Kube.ApplyManifests(ctx, assets.Manifests); err != nil {
		return fmt.Errorf("apply manifests: %w\nremedy: the embedded asset renders config/default — check RBAC on the target cluster (needs CRD + namespace write)", err)
	}
	return nil
}
