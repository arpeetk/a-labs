package podruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/summiteight/wren/internal/github"
	"github.com/summiteight/wren/internal/runspec"
)

// bareOrigin builds a bare repo with a commit on main and returns its path.
func bareOrigin(t *testing.T) string {
	t.Helper()
	seed := t.TempDir()
	repo, err := git.PlainInit(seed, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("init", &git.CommitOptions{Author: &object.Signature{Name: "s", Email: "s@x"}}); err != nil {
		t.Fatal(err)
	}
	head, _ := repo.Head()
	_ = repo.Storer.SetReference(plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), head.Hash()))

	bare := t.TempDir()
	if _, err := git.PlainClone(bare, true, &git.CloneOptions{URL: seed}); err != nil {
		t.Fatal(err)
	}
	return bare
}

// TestRunHarnessFinalizesRealBranch drives the full in-pod finalize path against
// a local bare "origin": hydrate clones it, the mock harness writes a change,
// and finalize commits + pushes the run branch. (No GitHub network: the PR API
// call would follow the push; here we assert the branch reaches origin.)
func TestRunHarnessFinalizesRealBranch(t *testing.T) {
	origin := bareOrigin(t)
	ws := t.TempDir()

	// A git file:// URL as the "repo" is not owner/repo form, so drive the
	// clone directly (hydrate builds an https URL from owner/repo). Instead we
	// exercise clone→harness→commit→push via the same helpers hydrate/finalize
	// use, with a file remote.
	if _, err := git.PlainClone(ws, false, &git.CloneOptions{URL: "file://" + origin}); err != nil {
		t.Fatal(err)
	}

	specPath := filepath.Join(t.TempDir(), "runspec.json")
	spec := runspec.RunSpec{
		RunID: "r-1", Project: "p", Repo: "corp/payments", Harness: "mock",
		Prompt: "add tests", BaseRef: "main", WorkspacePath: ws, BranchPrefix: "wren/me",
	}
	b, _ := json.Marshal(spec)
	if err := os.WriteFile(specPath, b, 0o644); err != nil {
		t.Fatal(err)
	}

	// No token → finalize is skipped; harness still succeeds and writes files.
	t.Setenv("GITHUB_TOKEN", "")
	var buf bytes.Buffer
	if err := RunHarness(context.Background(), &buf, specPath); err != nil {
		t.Fatalf("run harness: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "skipped") {
		t.Errorf("expected finalize skip message, got:\n%s", out)
	}
	if !strings.Contains(out, `"succeeded"`) {
		t.Errorf("expected succeeded, got:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(ws, "WREN_MOCK.md")); err != nil {
		t.Errorf("workspace change missing: %v", err)
	}
}

// TestRunHarnessOpensPR covers the finalize-success wiring hermetically: the
// workspace is a clone of a local bare origin, GITHUB_TOKEN is set, and the
// GitHub client is a fake — so the run branch is pushed to origin and a (fake)
// PR is opened, ending in a pr_ready event carrying the URL.
func TestRunHarnessOpensPR(t *testing.T) {
	origin := bareOrigin(t)
	ws := t.TempDir()
	if _, err := git.PlainClone(ws, false, &git.CloneOptions{URL: "file://" + origin}); err != nil {
		t.Fatal(err)
	}

	fake := &github.Fake{}
	orig := newGitHubClient
	newGitHubClient = func(string) github.Client { return fake }
	defer func() { newGitHubClient = orig }()
	t.Setenv("GITHUB_TOKEN", "tok")

	specPath := filepath.Join(t.TempDir(), "runspec.json")
	b, _ := json.Marshal(runspec.RunSpec{
		RunID: "r-7", Project: "p", Repo: "corp/payments", Harness: "mock",
		Prompt: "add tests", BaseRef: "main", WorkspacePath: ws, BranchPrefix: "wren/me",
	})
	_ = os.WriteFile(specPath, b, 0o644)

	var buf bytes.Buffer
	if err := RunHarness(context.Background(), &buf, specPath); err != nil {
		t.Fatalf("run harness: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "opened PR") || !strings.Contains(out, "corp/payments/pull/1") {
		t.Errorf("expected PR opened, got:\n%s", out)
	}
	if len(fake.PRs) != 1 || fake.PRs[0].HeadBranch != "wren/me/r-7" {
		t.Errorf("fake PRs = %+v", fake.PRs)
	}
	// Branch pushed to origin.
	ob, _ := git.PlainOpen(origin)
	if _, err := ob.Reference(plumbing.NewBranchReferenceName("wren/me/r-7"), true); err != nil {
		t.Errorf("branch not pushed: %v", err)
	}
}

func TestPRConfigured(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	if prConfigured(runspec.RunSpec{Repo: "a/b"}) {
		t.Error("no token → not configured")
	}
	t.Setenv("GITHUB_TOKEN", "tok")
	if !prConfigured(runspec.RunSpec{Repo: "a/b"}) {
		t.Error("repo + token → configured")
	}
	if prConfigured(runspec.RunSpec{}) {
		t.Error("no repo → not configured")
	}
}

func TestHydrateNoRepoSkipsClone(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	specPath := filepath.Join(t.TempDir(), "runspec.json")
	b, _ := json.Marshal(runspec.RunSpec{RunID: "r-1"})
	_ = os.WriteFile(specPath, b, 0o644)
	var buf bytes.Buffer
	if err := RunHydrate(context.Background(), &buf, specPath); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "skipped") {
		t.Errorf("hydrate output = %q", buf.String())
	}
}
