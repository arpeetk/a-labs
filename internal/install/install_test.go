package install

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/summiteight/wren/internal/install/assets"
)

// fixture builds an Installer on the fakes with kind-default canned outputs.
func fixture(t *testing.T) (*Installer, *FakeKube, *FakeRunner, *bytes.Buffer) {
	t.Helper()
	k := NewFakeKube()
	r := NewFakeRunner()
	// kind cluster "eval" exists by default; tests override Outputs as needed.
	r.Outputs["kind get clusters"] = "eval\n"
	r.Outputs["git -C . rev-parse --short HEAD"] = "abc1234\n"
	r.Outputs["docker info"] = "ok\n"
	var out bytes.Buffer
	in := &Installer{Kube: k, Runner: r, Out: &out}
	return in, k, r, &out
}

func kindOpts() Options {
	return Options{KindCluster: "eval", SkipCredentials: true}
}

// ranContains reports whether any recorded Run contains substr (FakeRunner's
// own Ran only matches a prefix, which the docker build args' absolute
// -f path defeats).
func ranContains(r *FakeRunner, substr string) bool {
	for _, run := range r.Runs {
		if strings.Contains(run, substr) {
			return true
		}
	}
	return false
}

func TestInstallKindHappyPath(t *testing.T) {
	in, k, r, out := fixture(t)
	if err := in.Install(context.Background(), kindOpts()); err != nil {
		t.Fatal(err)
	}
	// Cluster reused (no create), images built and loaded, manifests applied,
	// control plane waited on.
	if r.Ran("kind create cluster") {
		t.Errorf("existing kind cluster should be reused, runs: %v", r.Runs)
	}
	for _, want := range []string{
		"docker build -f ",
		"kind load docker-image wren/runtime:dev wren/operator:dev wren/apiserver:dev " +
			"wren/claude-code:dev wren/codex:dev wren/opencode:dev --name eval",
	} {
		if !r.Ran(want) {
			t.Errorf("expected run %q, runs: %v", want, r.Runs)
		}
	}
	// Default --harness-images builds all three harness Dockerfiles too — a
	// team shouldn't need a separate manual step to unlock codex/opencode.
	for _, df := range []string{"Dockerfile.claude-code", "Dockerfile.codex", "Dockerfile.opencode"} {
		if !ranContains(r, df) {
			t.Errorf("expected harness build for %s, runs: %v", df, r.Runs)
		}
	}
	if !k.HasCall("ApplyManifests") {
		t.Errorf("expected ApplyManifests, calls: %v", k.Calls)
	}
	if !k.HasCall("WaitDeployments:wren-operator,wren-apiserver") {
		t.Errorf("expected WaitDeployments for both deployments, calls: %v", k.Calls)
	}
	if k.HasCall("OverrideImages") {
		t.Errorf("kind path must not override images (manifests pin wren/*:dev), calls: %v", k.Calls)
	}
	// WS-15 Part A: install makes its --run-namespace the apiserver's default so
	// `wren project create` with no --namespace lands where the credentials went.
	if !k.HasCall("SetApiserverRunNamespace:wren-runs") {
		t.Errorf("expected SetApiserverRunNamespace:wren-runs, calls: %v", k.Calls)
	}
	if !strings.Contains(out.String(), "port-forward svc/"+ApiserverService) {
		t.Errorf("hand-off missing port-forward, out:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "X-Wren-User") {
		t.Errorf("hand-off must carry the M0 header-auth warning, out:\n%s", out.String())
	}
}

func TestInstallKindCreatesMissingCluster(t *testing.T) {
	in, _, r, _ := fixture(t)
	r.Outputs["kind get clusters"] = "other\n"
	if err := in.Install(context.Background(), kindOpts()); err != nil {
		t.Fatal(err)
	}
	if !r.Ran("kind create cluster --name eval --wait 120s") {
		t.Errorf("expected kind create, runs: %v", r.Runs)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	// testing.md rule 5: a re-run must converge, not fail on existing state.
	in, _, _, _ := fixture(t)
	for i := 0; i < 2; i++ {
		if err := in.Install(context.Background(), kindOpts()); err != nil {
			t.Fatalf("install %d: %v", i+1, err)
		}
	}
}

func TestInstallRegistryPath(t *testing.T) {
	in, k, r, out := fixture(t)
	opts := Options{Registry: "us-central1-docker.pkg.dev/proj/wren", SkipCredentials: true}
	if err := in.Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	// linux/amd64 cross-builds + pushes, tag resolved from git — the 3
	// control-plane images plus all 3 harness images (default --harness-images).
	for _, name := range []string{"runtime", "operator", "apiserver", "claude-code", "codex", "opencode"} {
		ref := "us-central1-docker.pkg.dev/proj/wren/" + name + ":abc1234"
		if !r.Ran("docker build --platform linux/amd64") {
			t.Errorf("expected linux/amd64 build for %s, runs: %v", name, r.SortedRuns())
		}
		if !r.Ran("docker push " + ref) {
			t.Errorf("expected push of %s, runs: %v", ref, r.SortedRuns())
		}
	}
	if !k.HasCall("OverrideImages:us-central1-docker.pkg.dev/proj/wren:abc1234") {
		t.Errorf("expected OverrideImages with resolved tag, calls: %v", k.Calls)
	}
	// OverrideImages only repoints the control-plane Deployments (runtime/
	// operator/apiserver) — harness images aren't referenced by any
	// Deployment, so pushing them must not add more OverrideImages calls.
	overrides := 0
	for _, c := range k.Calls {
		if strings.HasPrefix(c, "OverrideImages:") {
			overrides++
		}
	}
	if overrides != 1 {
		t.Errorf("expected exactly 1 OverrideImages call, got %d: %v", overrides, k.Calls)
	}
	if !strings.Contains(out.String(), "--harness-image us-central1-docker.pkg.dev/proj/wren/claude-code:abc1234") {
		t.Errorf("hand-off should hint the pushed claude-code harness image, out:\n%s", out.String())
	}
}

func TestInstallHarnessImagesRestrictsSet(t *testing.T) {
	in, _, r, out := fixture(t)
	opts := kindOpts()
	opts.HarnessImages = "codex"
	if err := in.Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if !ranContains(r, "Dockerfile.codex") {
		t.Errorf("expected codex harness build, runs: %v", r.Runs)
	}
	for _, df := range []string{"Dockerfile.claude-code", "Dockerfile.opencode"} {
		if ranContains(r, df) {
			t.Errorf("restricted --harness-images=codex must not build %s, runs: %v", df, r.Runs)
		}
	}
	if !r.Ran("kind load docker-image wren/runtime:dev wren/operator:dev wren/apiserver:dev wren/codex:dev --name eval") {
		t.Errorf("expected kind load with just the codex harness image, runs: %v", r.Runs)
	}
	// claude-code wasn't built, so the hand-off must not recommend an image
	// that doesn't exist — it should fall back to the mock-only example.
	if strings.Contains(out.String(), "--harness-image wren/claude-code:dev") {
		t.Errorf("hand-off must not hint an unbuilt claude-code image, out:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "no claude-code harness image") {
		t.Errorf("hand-off should note claude-code wasn't built, out:\n%s", out.String())
	}
}

func TestInstallHarnessImagesNoneSkipsAll(t *testing.T) {
	in, _, r, out := fixture(t)
	opts := kindOpts()
	opts.HarnessImages = "none"
	if err := in.Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	for _, df := range []string{"Dockerfile.claude-code", "Dockerfile.codex", "Dockerfile.opencode"} {
		if ranContains(r, df) {
			t.Errorf("--harness-images=none must build no harness image, runs: %v", r.Runs)
		}
	}
	if !r.Ran("kind load docker-image wren/runtime:dev wren/operator:dev wren/apiserver:dev --name eval") {
		t.Errorf("expected kind load with only the 3 control-plane images, runs: %v", r.Runs)
	}
	if !strings.Contains(out.String(), "mock") {
		t.Errorf("hand-off should fall back to the mock-only example, out:\n%s", out.String())
	}
}

func TestInstallHarnessImagesUnknownName(t *testing.T) {
	in, _, _, _ := fixture(t)
	opts := kindOpts()
	opts.HarnessImages = "claude-code,bogus"
	err := in.Install(context.Background(), opts)
	if err == nil || !strings.Contains(err.Error(), `unknown harness "bogus"`) {
		t.Fatalf("expected unknown-harness validation error, got %v", err)
	}
}

func TestInstallRegistryExplicitTagWins(t *testing.T) {
	in, k, r, _ := fixture(t)
	delete(r.Outputs, "git -C . rev-parse --short HEAD") // git failure must not matter
	opts := Options{Registry: "ghcr.io/x", ImageTag: "v0.1.0", SkipCredentials: true}
	if err := in.Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if !k.HasCall("OverrideImages:ghcr.io/x:v0.1.0") {
		t.Errorf("expected explicit tag, calls: %v", k.Calls)
	}
}

func TestInstallRequiresAnImagePath(t *testing.T) {
	in, _, _, _ := fixture(t)
	err := in.Install(context.Background(), Options{})
	if err == nil || !strings.Contains(err.Error(), "--registry") {
		t.Fatalf("expected image-path guidance, got %v", err)
	}
}

func TestInstallRejectsBothImagePaths(t *testing.T) {
	in, _, _, _ := fixture(t)
	err := in.Install(context.Background(), Options{Registry: "r", KindCluster: "k"})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual-exclusion error, got %v", err)
	}
}

func TestInstallPreflightMissingKubectl(t *testing.T) {
	in, _, r, _ := fixture(t)
	r.Tools = map[string]bool{"docker": true, "kind": true}
	err := in.Install(context.Background(), kindOpts())
	if err == nil || !strings.Contains(err.Error(), "kubectl not found") || !strings.Contains(err.Error(), "remedy:") {
		t.Fatalf("expected kubectl remediation, got %v", err)
	}
}

func TestInstallPreflightDockerDaemonDown(t *testing.T) {
	in, _, r, _ := fixture(t)
	delete(r.Outputs, "docker info") // probe fails when the daemon is down
	err := in.Install(context.Background(), kindOpts())
	if err == nil || !strings.Contains(err.Error(), "docker daemon is not reachable") {
		t.Fatalf("expected docker-daemon remediation, got %v", err)
	}
}

func TestInstallPreflightOldServer(t *testing.T) {
	in, k, _, _ := fixture(t)
	k.Version = "1.26"
	err := in.Install(context.Background(), kindOpts())
	if err == nil || !strings.Contains(err.Error(), "≥ 1.27") {
		t.Fatalf("expected version-floor error, got %v", err)
	}
}

func TestInstallServerUnreachable(t *testing.T) {
	in, k, _, _ := fixture(t)
	k.FailOn = "ServerVersion"
	k.Err = errors.New("dial tcp 10.0.0.1:443: i/o timeout")
	err := in.Install(context.Background(), kindOpts())
	if err == nil || !strings.Contains(err.Error(), "cannot reach the cluster") || !strings.Contains(err.Error(), "remedy:") {
		t.Fatalf("expected connectivity remediation, got %v", err)
	}
}

func TestInstallCredentialsFromEnv(t *testing.T) {
	in, k, _, out := fixture(t)
	opts := kindOpts()
	opts.SkipCredentials = false
	opts.GitHubToken = "ghp_supersecret"
	opts.AnthropicKey = "sk-ant-supersecret"
	// PromptSecret nil: env values must suffice with no prompt.
	if err := in.Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if got := k.Secrets["wren-runs/"+GitHubTokenSecret]["token"]; got != "ghp_supersecret" {
		t.Errorf("github token secret = %q", got)
	}
	if got := k.Secrets["wren-runs/"+AnthropicKeySecret]["key"]; got != "sk-ant-supersecret" {
		t.Errorf("anthropic secret = %q", got)
	}
	// The whole point of the step: values never appear in the output.
	if strings.Contains(out.String(), "ghp_supersecret") || strings.Contains(out.String(), "sk-ant-supersecret") {
		t.Errorf("credential value leaked into output:\n%s", out.String())
	}
}

func TestInstallCredentialsFromGhCLIFallback(t *testing.T) {
	in, k, r, _ := fixture(t)
	r.Outputs["gh auth token"] = "ghp_fromcli\n"
	opts := kindOpts()
	opts.SkipCredentials = false
	if err := in.Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if got := k.Secrets["wren-runs/"+GitHubTokenSecret]["token"]; got != "ghp_fromcli" {
		t.Errorf("github token from gh = %q", got)
	}
}

func TestInstallCredentialsPrompt(t *testing.T) {
	in, k, r, _ := fixture(t)
	delete(r.Outputs, "gh auth token")
	opts := kindOpts()
	opts.SkipCredentials = false
	var prompts int
	in.PromptSecret = func(label string) (string, error) {
		prompts++
		return "from-prompt", nil
	}
	if err := in.Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if prompts == 0 {
		t.Error("expected the interactive prompt to be used")
	}
	if got := k.Secrets["wren-runs/"+GitHubTokenSecret]["token"]; got != "from-prompt" {
		t.Errorf("github token from prompt = %q", got)
	}
}

func TestInstallCredentialsSkippedWhenUnavailable(t *testing.T) {
	in, k, r, out := fixture(t)
	delete(r.Outputs, "gh auth token")                                       // gh fails
	r.Tools = map[string]bool{"kubectl": true, "docker": true, "kind": true} // no gh at all
	opts := kindOpts()
	opts.SkipCredentials = false
	if err := in.Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if len(k.Secrets) != 0 {
		t.Errorf("no credentials → no secrets, got %v", k.Secrets)
	}
	if !strings.Contains(out.String(), "keyless") {
		t.Errorf("should note the keyless continuation, out:\n%s", out.String())
	}
}

func TestInstallExposeLoadBalancer(t *testing.T) {
	in, k, _, out := fixture(t)
	opts := kindOpts()
	opts.Expose = "LoadBalancer"
	if err := in.Install(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if !k.HasCall("SetServiceType:wren-apiserver=LoadBalancer") {
		t.Errorf("expected service type patch, calls: %v", k.Calls)
	}
	if !strings.Contains(out.String(), "EXTERNAL-IP") {
		t.Errorf("hand-off should explain the LoadBalancer path, out:\n%s", out.String())
	}
}

func TestInstallWaitFailureHasRemedy(t *testing.T) {
	in, k, _, _ := fixture(t)
	k.FailOn = "WaitDeployments:wren-operator,wren-apiserver"
	k.Err = errors.New("context deadline exceeded")
	err := in.Install(context.Background(), kindOpts())
	if err == nil || !strings.Contains(err.Error(), "logs deploy/") {
		t.Fatalf("expected log-dump remedy, got %v", err)
	}
}

func TestUninstall(t *testing.T) {
	in, k, _, out := fixture(t)
	if err := in.Uninstall(context.Background(), UninstallOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"DeleteNamespace:" + SystemNamespace,
		"DeleteNamespace:wren-runs",
		"DeleteClusterScoped",
	} {
		if !k.HasCall(want) {
			t.Errorf("expected call %q, calls: %v", want, k.Calls)
		}
	}
	if !strings.Contains(out.String(), "uninstalled") {
		t.Errorf("expected confirmation output, got:\n%s", out.String())
	}
}

func TestParseServerVersion(t *testing.T) {
	cases := map[string][2]int{
		"1.26":       {1, 26},
		"v1.31":      {1, 31},
		"1.31+":      {1, 31}, // GKE appends "+"
		"1.29.5-gke": {1, 29},
		" 1.27\n":    {1, 27},
		"1.27.3":     {1, 27},
	}
	for in, want := range cases {
		maj, min, err := parseServerVersion(in)
		if err != nil {
			t.Errorf("parseServerVersion(%q): %v", in, err)
			continue
		}
		if [2]int{maj, min} != want {
			t.Errorf("parseServerVersion(%q) = %d.%d, want %d.%d", in, maj, min, want[0], want[1])
		}
	}
	if _, _, err := parseServerVersion("garbage"); err == nil {
		t.Error("expected error for unparsable version")
	}
}

// TestAssetMatchesConstants is the drift tripwire between the install package's
// fixed coordinates and the rendered asset (config/default via `make assets`).
// If config/ changes names/namespaces, this fails — not the live install.
func TestAssetMatchesConstants(t *testing.T) {
	objs, err := splitManifests(assets.Manifests)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) == 0 {
		t.Fatal("embedded asset has no objects — run `make assets`")
	}
	type key struct{ kind, ns, name string }
	have := map[key]bool{}
	crds := map[string]bool{}
	for _, o := range objs {
		have[key{o.GetKind(), o.GetNamespace(), o.GetName()}] = true
		if o.GetKind() == "CustomResourceDefinition" {
			crds[o.GetName()] = true
		}
	}
	for _, want := range []key{
		{"Namespace", "", SystemNamespace},
		{"Deployment", SystemNamespace, OperatorDeployment},
		{"Deployment", SystemNamespace, ApiserverDeployment},
		{"Service", SystemNamespace, ApiserverService},
	} {
		if !have[want] {
			t.Errorf("embedded asset missing %s %s/%s — constants in install.go are stale", want.kind, want.ns, want.name)
		}
	}
	// Uninstall derives the CRDs (and cluster RBAC) from the asset, so the
	// render must carry the AgentRun CRD — else AgentRuns would survive an
	// uninstall.
	for _, name := range []string{"agentruns.wren.dev"} {
		if !crds[name] {
			t.Errorf("embedded asset missing CRD %s", name)
		}
	}
}
