package install

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

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

// credentials collects the GitHub token + Anthropic key and stores them as the
// Secrets the egress-proxy injects. Resolution order per credential: env (the
// CLI fills Options) → `gh auth token` (GitHub only) → interactive prompt →
// skip with a note. Values are only ever passed to the Secret write — never
// logged, never echoed.
func (s *steps) credentials(ctx context.Context) error {
	if s.opts.SkipCredentials {
		s.logf("skipping credentials (--skip-credentials); runs will be keyless (mock harness / no PRs)")
		return nil
	}
	gh := s.opts.GitHubToken
	if gh == "" && s.in.Runner.LookPath("gh") {
		if tok, err := s.in.Runner.Output(ctx, "gh", "auth", "token"); err == nil {
			gh = strings.TrimSpace(tok)
		}
	}
	if gh == "" && s.in.PromptSecret != nil {
		tok, err := s.in.PromptSecret("GitHub token (PAT, repo scope — input hidden; Enter to skip)")
		if err != nil {
			return fmt.Errorf("read GitHub token: %w", err)
		}
		gh = strings.TrimSpace(tok)
	}
	ak := s.opts.AnthropicKey
	if ak == "" && s.in.PromptSecret != nil {
		key, err := s.in.PromptSecret("Anthropic API key (input hidden; Enter to skip)")
		if err != nil {
			return fmt.Errorf("read Anthropic API key: %w", err)
		}
		ak = strings.TrimSpace(key)
	}
	if gh == "" && ak == "" {
		s.logf("no credentials provided — continuing keyless (mock harness works; claude-code runs and PRs need secrets)")
		s.logf("  add them later: kubectl -n %s create secret generic %s --from-literal=token=…", s.opts.RunNamespace, GitHubTokenSecret)
		return nil
	}
	// The proxy reads these Secrets in the run's namespace, so they live in the
	// install's RunNamespace; credentialed projects point their namespace there.
	if err := s.in.Kube.EnsureNamespace(ctx, s.opts.RunNamespace); err != nil {
		return fmt.Errorf("ensure run namespace %s: %w", s.opts.RunNamespace, err)
	}
	if gh != "" {
		if err := s.in.Kube.UpsertSecret(ctx, s.opts.RunNamespace, GitHubTokenSecret, map[string]string{"token": gh}); err != nil {
			return fmt.Errorf("write %s secret: %w", GitHubTokenSecret, err)
		}
		s.logf("stored GitHub token in secret %s/%s (value never displayed)", s.opts.RunNamespace, GitHubTokenSecret)
	}
	if ak != "" {
		if err := s.in.Kube.UpsertSecret(ctx, s.opts.RunNamespace, AnthropicKeySecret, map[string]string{"key": ak}); err != nil {
			return fmt.Errorf("write %s secret: %w", AnthropicKeySecret, err)
		}
		s.logf("stored Anthropic key in secret %s/%s (value never displayed)", s.opts.RunNamespace, AnthropicKeySecret)
	}
	return nil
}

// handOff prints the engineer-facing next steps: reach the control plane, log
// in, register a project, submit a run — plus the M0 auth caveat (spec §7).
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
	if ref, ok := s.harnessImageHint(); ok {
		fmt.Fprintf(s.in.Out, `
Then, as an engineer:
  wren login --control-plane localhost:8090 --user you@corp.com
  wren project create demo --repo owner/repo --harness claude-code \
      --harness-image %s --cpu 1 --memory 2Gi --disk 5Gi --namespace %s
  wren run create --project demo --task "Add a health endpoint"

NOTE: the control plane authenticates callers with a trusted X-Wren-User header
only (M0 stand-in; SSO/OIDC is a later milestone). Keep it on port-forward or a
trusted network — do NOT expose it publicly.
`, ref, s.opts.RunNamespace)
		return
	}
	fmt.Fprintf(s.in.Out, `
Then, as an engineer:
  wren login --control-plane localhost:8090 --user you@corp.com
  wren project create demo --harness mock \
      --cpu 1 --memory 2Gi --disk 5Gi --namespace %s
  wren run create --project demo --task "Add a health endpoint"

NOTE: this install built no claude-code harness image (--harness-images=%s).
mock is the only harness available until you re-run install with it included,
e.g. `+"`wren install ... --harness-images=claude-code`"+` — or point a project at a
harness image you built/pushed yourself.

NOTE: the control plane authenticates callers with a trusted X-Wren-User header
only (M0 stand-in; SSO/OIDC is a later milestone). Keep it on port-forward or a
trusted network — do NOT expose it publicly.
`, s.opts.RunNamespace, s.opts.HarnessImages)
}

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
