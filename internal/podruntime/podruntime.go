// Package podruntime implements the in-pod roles the wren-runtime binary runs:
// the harness runner, the hydrate init container, and the long-lived sidecars
// (egress-proxy, checkpointer, agent-gateway). Roles are functions here so they
// are unit-testable; cmd/wren-runtime is a thin dispatcher.
//
// The sidecars are M0 stand-ins (they keep the pod's native-sidecar shape valid
// and log liveness) — real egress allowlisting, GCS checkpointing, and stream
// bridging land in their respective milestones.
package podruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/summiteight/wren/internal/finalize"
	"github.com/summiteight/wren/internal/github"
	"github.com/summiteight/wren/internal/gitwork"
	"github.com/summiteight/wren/internal/harness"
	"github.com/summiteight/wren/internal/runspec"
)

// githubToken is the M0 stand-in for the egress-proxy-injected installation
// token: a token handed to the runner via env (a mounted Secret). The secure
// design injects it at the egress proxy so it never lives in the runner env
// (spec §5.6/§5.7); that lands with the egress-proxy work.
func githubToken() string { return os.Getenv("GITHUB_TOKEN") }

// prConfigured reports whether a run can open a real PR (repo + token present).
func prConfigured(spec runspec.RunSpec) bool {
	return spec.Repo != "" && githubToken() != ""
}

// newGitHubClient builds the PR client; a seam so tests can inject a fake.
var newGitHubClient = func(token string) github.Client { return github.NewREST(token, nil) }

// skipReason explains why finalize was skipped.
func skipReason(spec runspec.RunSpec) string {
	if spec.Repo == "" {
		return "no repo configured for this project"
	}
	return "no GITHUB_TOKEN available in the runner"
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
	em := harness.NewEmitter(out)

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
		p, ferr := finalize.Run(ctx, spec, githubToken(), newGitHubClient(githubToken()))
		switch {
		case errors.Is(ferr, finalize.ErrNoChanges):
			em.Message("finalize: harness made no changes; no PR opened")
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

// RunHydrate runs the hydrate init container. M0: it confirms the workspace is
// present. Real clone / checkpoint-restore lands with the checkpointer work.
func RunHydrate(_ context.Context, out io.Writer, specPath string) error {
	if specPath == "" {
		specPath = DefaultRunSpecPath
	}
	em := harness.NewEmitter(out)
	spec, err := LoadRunSpec(specPath)
	if err != nil {
		em.Errorf("load runspec: " + err.Error())
		return err
	}
	// When a repo + token are configured, do a real clone so the harness works
	// in a git checkout and finalize can push a branch. Checkpoint-restore on
	// resume lands with the checkpointer work.
	if prConfigured(spec) && spec.Mode != runspec.ModeResume {
		repoURL := "https://github.com/" + spec.Repo + ".git"
		if _, err := gitwork.Clone(repoURL, spec.BaseRef, spec.WorkspacePath, githubToken()); err != nil {
			em.Errorf("hydrate clone: " + err.Error())
			return err
		}
		em.Message("hydrate: cloned " + spec.Repo + " @ " + orDefault(spec.BaseRef, "default"))
		return nil
	}

	mode := "fresh clone"
	if spec.Mode == runspec.ModeResume {
		mode = "restore-from-checkpoint"
	}
	em.Message("hydrate: workspace ready (" + mode + " skipped; no repo/token — M0)")
	return nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// RunSidecar runs a long-lived sidecar role: it logs liveness and blocks until
// the context is cancelled (SIGTERM), then exits cleanly so the pod can complete.
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

// Roles that the dispatcher understands.
const (
	RoleHarness      = "harness"
	RoleHydrate      = "hydrate"
	RoleEgressProxy  = "egress-proxy"
	RoleCheckpointer = "checkpointer"
	RoleGateway      = "agent-gateway"
)

// Dispatch runs the named role to completion.
func Dispatch(ctx context.Context, out io.Writer, role, specPath string) error {
	switch role {
	case RoleHarness, "":
		return RunHarness(ctx, out, specPath)
	case RoleHydrate:
		return RunHydrate(ctx, out, specPath)
	case RoleEgressProxy, RoleCheckpointer, RoleGateway:
		return RunSidecar(ctx, out, role)
	default:
		return fmt.Errorf("unknown role %q", role)
	}
}
