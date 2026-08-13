package install

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
)

func (s *steps) gatewayCredential(ctx context.Context) error {
	if err := s.in.Kube.EnsureNamespace(ctx, s.opts.RunNamespace); err != nil {
		return fmt.Errorf("ensure run namespace for gateway: %w", err)
	}
	token, err := s.in.Kube.SecretValue(ctx, SystemNamespace, GatewayTokenSecret, "token")
	if err != nil {
		return fmt.Errorf("read existing gateway credential: %w", err)
	}
	if token == "" {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return fmt.Errorf("generate gateway credential: %w", err)
		}
		token = base64.RawURLEncoding.EncodeToString(raw)
	}
	for _, namespace := range []string{SystemNamespace, s.opts.RunNamespace} {
		if err := s.in.Kube.UpsertSecret(ctx, namespace, GatewayTokenSecret, map[string]string{"token": token}); err != nil {
			return fmt.Errorf("write gateway credential in %s: %w", namespace, err)
		}
	}
	s.logf("configured authenticated, replay-safe run event delivery")
	return nil
}

// imageNames are the three control-plane images every install builds, in
// push/load order. The operator's --runtime-image points at "runtime";
// operator + apiserver run the control plane itself.
var imageNames = []string{"runtime", "operator", "apiserver"}

// harnessImageNames are the harness images `wren install` can build, keyed by
// the same names internal/harness.New switches on (claude-code/codex/opencode)
// and the build/Dockerfile.<name> each image comes from. This is the default
// build set — a team shouldn't have to discover a separate manual step to
// unlock codex/opencode later.
var harnessImageNames = []string{"claude-code", "codex", "opencode"}

// resolveHarnessImages parses --harness-images into the concrete list of
// harness image names to build: empty selects the default (all of
// harnessImageNames), "none" skips harness images entirely (a keyless/
// mock-only eval install), and a comma list restricts to the named subset.
func resolveHarnessImages(spec string) ([]string, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return harnessImageNames, nil
	}
	if spec == "none" {
		return nil, nil
	}
	valid := make(map[string]bool, len(harnessImageNames))
	for _, n := range harnessImageNames {
		valid[n] = true
	}
	var out []string
	seen := make(map[string]bool, len(harnessImageNames))
	for _, n := range strings.Split(spec, ",") {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if !valid[n] {
			return nil, fmt.Errorf("--harness-images: unknown harness %q (want a comma list of %s, or \"none\")",
				n, strings.Join(harnessImageNames, ", "))
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out, nil
}

// images builds and delivers the control-plane images plus the selected
// harness images: pushed to --registry for a real cluster (GKE), or built +
// `kind load`ed for local eval.
func (s *steps) images(ctx context.Context) error {
	if s.opts.KindCluster != "" {
		return s.kindImages(ctx)
	}
	return s.registryImages(ctx)
}

// kindImages builds wren/*:dev — the refs the embedded manifests already pin
// for the control-plane images — plus wren/<harness>:dev for the selected
// harness images, and loads all of them into the kind node.
func (s *steps) kindImages(ctx context.Context) error {
	all := append(append([]string{}, imageNames...), s.harnesses...)
	s.logf("building images (wren/{%s}:dev)", strings.Join(all, ","))
	var refs []string
	for _, name := range all {
		ref := "wren/" + name + ":dev"
		if err := s.build(ctx, name, ref, ""); err != nil {
			return err
		}
		refs = append(refs, ref)
	}
	s.logf("loading images into kind cluster %q", s.opts.KindCluster)
	// `kind load` has no typed client — it talks to the container runtime on the
	// node — so it stays an exec'd command (as does docker).
	args := append([]string{"load", "docker-image"}, refs...)
	args = append(args, "--name", s.opts.KindCluster)
	if err := s.in.Runner.Run(ctx, "kind", args...); err != nil {
		return fmt.Errorf("kind load: %w", err)
	}
	return nil
}

// registryImages builds linux/amd64 control-plane + selected harness images
// (GKE Standard nodes are x86), pushes them, and overrides the Deployment
// refs imperatively — the hack/e2e-gke.sh pattern moved into Go, so no
// registry or tag is baked into committed manifests (code standards rule 3).
// Harness images have no Deployment to override — a project points at one
// explicitly via `wren project create --harness-image`.
func (s *steps) registryImages(ctx context.Context) error {
	tag, err := s.resolveTag(ctx)
	if err != nil {
		return err
	}
	s.tag = tag
	all := append(append([]string{}, imageNames...), s.harnesses...)
	reg := strings.TrimSuffix(s.opts.Registry, "/")
	s.logf("building + pushing linux/amd64 images to %s (tag %s)", reg, tag)
	for _, name := range all {
		if err := s.build(ctx, name, reg+"/"+name+":"+tag, "linux/amd64"); err != nil {
			return err
		}
		if err := s.in.Runner.Run(ctx, "docker", "push", reg+"/"+name+":"+tag); err != nil {
			return fmt.Errorf("docker push %s/%s:%s: %w\nremedy: run `gcloud auth configure-docker` (Artifact Registry) or `docker login` for your registry", reg, name, tag, err)
		}
	}
	if err := s.in.Kube.OverrideImages(ctx, reg, tag); err != nil {
		return fmt.Errorf("override control-plane image refs: %w", err)
	}
	return nil
}

// build runs one docker build against the repo checkout. name selects the
// Dockerfile/binary: runtime has its own, operator/apiserver share gobin
// (BIN=wren-<name>), and each harness has its own build/Dockerfile.<name>.
func (s *steps) build(ctx context.Context, name, ref, platform string) error {
	// Resolve the Dockerfile against the checkout: docker resolves a relative
	// -f against the process cwd, which need not equal SrcDir.
	src, err := filepath.Abs(s.opts.SrcDir)
	if err != nil {
		return fmt.Errorf("resolve --src: %w", err)
	}
	args := []string{"build"}
	if platform != "" {
		args = append(args, "--platform", platform)
	}
	var dockerfile string
	switch name {
	case "runtime":
		dockerfile = "build/Dockerfile.runtime"
	case "operator", "apiserver":
		dockerfile = "build/Dockerfile.gobin"
		args = append(args, "--build-arg", "BIN=wren-"+name)
	default:
		// Harness images (claude-code/codex/opencode): each has its own
		// Dockerfile that builds wren-runtime itself, no BIN arg needed.
		dockerfile = "build/Dockerfile." + name
	}
	args = append(args, "-f", filepath.Join(src, dockerfile), "-t", ref, src)
	if err := s.in.Runner.Run(ctx, "docker", args...); err != nil {
		return fmt.Errorf("docker build %s: %w", ref, err)
	}
	return nil
}

// resolveTag pins the pushed tag once: explicit --tag wins, then the source
// tree's short SHA, then "dev" (a tarball checkout with no .git).
func (s *steps) resolveTag(ctx context.Context) (string, error) {
	if s.opts.ImageTag != "" {
		return s.opts.ImageTag, nil
	}
	if s.in.Runner.LookPath("git") {
		if sha, err := s.in.Runner.Output(ctx, "git", "-C", s.opts.SrcDir, "rev-parse", "--short", "HEAD"); err == nil {
			if t := strings.TrimSpace(sha); t != "" {
				return t, nil
			}
		}
	}
	return "dev", nil
}

// credentials collects provider credentials and stores them as the Secrets the
// egress-proxy injects. Values are only passed to Secret writes—never logged.
func (s *steps) credentials(ctx context.Context) error {
	if s.opts.SkipCredentials {
		s.logf("skipping credentials (--skip-credentials); runs will be keyless (mock harness / no PRs)")
		return nil
	}
	gh, err := s.resolveGitHubToken(ctx)
	if err != nil {
		return err
	}
	ak, err := s.resolveProviderKey(s.opts.AnthropicKey, "Anthropic API key (input hidden; Enter to skip)", "Anthropic API key")
	if err != nil {
		return err
	}
	openAI, err := s.resolveProviderKey(s.opts.OpenAIKey, "OpenAI API key (input hidden; Enter to skip)", "OpenAI API key")
	if err != nil {
		return err
	}
	if gh == "" && ak == "" && openAI == "" {
		s.logf("no credentials provided — continuing keyless (mock harness works; model-backed runs and PRs need secrets)")
		s.logf("  add them later with kubectl in namespace %s; see docs/harnesses.md", s.opts.RunNamespace)
		return nil
	}
	if err := s.in.Kube.EnsureNamespace(ctx, s.opts.RunNamespace); err != nil {
		return fmt.Errorf("ensure run namespace %s: %w", s.opts.RunNamespace, err)
	}
	credentials := []struct {
		name  string
		key   string
		value string
		label string
	}{
		{name: GitHubTokenSecret, key: "token", value: gh, label: "GitHub token"},
		{name: AnthropicKeySecret, key: "key", value: ak, label: "Anthropic key"},
		{name: OpenAIKeySecret, key: "key", value: openAI, label: "OpenAI key"},
	}
	for _, credential := range credentials {
		if credential.value == "" {
			continue
		}
		if err := s.in.Kube.UpsertSecret(ctx, s.opts.RunNamespace, credential.name, map[string]string{credential.key: credential.value}); err != nil {
			return fmt.Errorf("write %s secret: %w", credential.name, err)
		}
		s.logf("stored %s in secret %s/%s (value never displayed)", credential.label, s.opts.RunNamespace, credential.name)
	}
	return nil
}

func (s *steps) resolveGitHubToken(ctx context.Context) (string, error) {
	gh := strings.TrimSpace(s.opts.GitHubToken)
	if gh == "" && s.in.Runner.LookPath("gh") {
		if tok, err := s.in.Runner.Output(ctx, "gh", "auth", "token"); err == nil {
			gh = strings.TrimSpace(tok)
		}
	}
	if gh == "" && s.in.PromptSecret != nil {
		tok, err := s.in.PromptSecret("GitHub token (PAT, repo scope — input hidden; Enter to skip)")
		if err != nil {
			return "", fmt.Errorf("read GitHub token: %w", err)
		}
		gh = strings.TrimSpace(tok)
	}
	return gh, nil
}

func (s *steps) resolveProviderKey(value, prompt, label string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" && s.in.PromptSecret != nil {
		key, err := s.in.PromptSecret(prompt)
		if err != nil {
			return "", fmt.Errorf("read %s: %w", label, err)
		}
		value = strings.TrimSpace(key)
	}
	return value, nil
}

// handOff prints the engineer-facing next steps: reach the control plane, log
// in, register a project, submit a run, and understand the current auth boundary.
func (s *steps) handOff() {
	kctl := "kubectl"
	if c := s.opts.contextName(); c != "" {
		kctl += " --context " + c
	}
	fmt.Fprintf(s.in.Out, `
Wren control plane is Ready.

Reach it (local):
  %s -n %s port-forward svc/%s 8090:8090
`, kctl, SystemNamespace, ApiserverService)
	if s.opts.Expose == "LoadBalancer" {
		fmt.Fprintf(s.in.Out, `
Or via the LoadBalancer (team setups):
  %s -n %s get svc %s   # EXTERNAL-IP, then use <ip>:8090 below
`, kctl, SystemNamespace, ApiserverService)
	}
	// The credential Secrets live in --run-namespace, which install just made the
	// apiserver's default, and harness/model/cpu/memory/disk all
	// have control-plane defaults — so the minimum project is just a name + repo.
	// On a registry install the default harness image (wren/claude-code:dev) is
	// not on the cluster, so the project must still point at the pushed image.
	if ref, ok := s.harnessImageHint(); ok {
		if s.opts.KindCluster != "" {
			fmt.Fprintf(s.in.Out, `
Then, as an engineer:
  wren login --control-plane localhost:8090 --user you@corp.com
  wren project create demo --repo owner/repo
  wren run create --project demo --task "Add a health endpoint"
%s`, authNote)
		} else {
			fmt.Fprintf(s.in.Out, `
Then, as an engineer:
  wren login --control-plane localhost:8090 --user you@corp.com
  wren project create demo --repo owner/repo --harness-image %s
  wren run create --project demo --task "Add a health endpoint"
%s`, ref, authNote)
		}
		return
	}
	fmt.Fprintf(s.in.Out, `
Then, as an engineer:
  wren login --control-plane localhost:8090 --user you@corp.com
  wren project create demo --harness mock
  wren run create --project demo --task "Add a health endpoint"

NOTE: this install built no claude-code harness image (--harness-images=%s).
mock is the only harness available until you re-run install with it included,
e.g. `+"`wren install ... --harness-images=claude-code`"+` — or point a project at a
harness image you built/pushed yourself.
%s`, s.opts.HarnessImages, authNote)
}

// authNote is the shared authentication caveat printed at the end of hand-off.
const authNote = `
NOTE: the control plane authenticates callers with a trusted X-Wren-User header
only; SSO/OIDC is not implemented. Keep it on port-forward or a
trusted network — do NOT expose it publicly.
`

// harnessImageHint resolves the --harness-image example for the hand-off: the
// image this install actually built for the project's default harness
// (claude-code, per coreapi.DefaultDefaults). ok is false when this install
// did not build a claude-code image (e.g. --harness-images=codex or
// --harness-images=none) — the caller falls back to a mock-only example.
func (s *steps) harnessImageHint() (ref string, ok bool) {
	for _, h := range s.harnesses {
		if h == "claude-code" {
			ok = true
			break
		}
	}
	if !ok {
		return "", false
	}
	if s.opts.KindCluster != "" {
		return "wren/claude-code:dev", true
	}
	return strings.TrimSuffix(s.opts.Registry, "/") + "/claude-code:" + s.tag, true
}
