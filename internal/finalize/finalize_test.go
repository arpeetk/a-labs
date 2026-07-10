package finalize

import (
	"context"
	"errors"
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

// makeOrigin builds a bare repo with an initial commit on main.
func makeOrigin(t *testing.T) string {
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

func cloneInto(t *testing.T, origin string) string {
	t.Helper()
	ws := t.TempDir()
	if _, err := git.PlainClone(ws, false, &git.CloneOptions{URL: "file://" + origin}); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestFinalizeOpensPR(t *testing.T) {
	origin := makeOrigin(t)
	ws := cloneInto(t, origin)
	// Harness produced a change.
	if err := os.WriteFile(filepath.Join(ws, "WREN.md"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	spec := runspec.RunSpec{
		RunID: "r-1", Project: "payments", Repo: "corp/payments",
		Prompt: "Add idempotency keys", BaseRef: "main",
		WorkspacePath: ws, BranchPrefix: "wren/arpeet", Harness: "mock",
	}
	fake := &github.Fake{}
	pr, err := Run(context.Background(), spec, "", fake)
	if err != nil {
		t.Fatalf("finalize: %v", err)
	}
	if pr.Number != 1 || !strings.Contains(pr.URL, "corp/payments/pull/1") {
		t.Fatalf("pr = %+v", pr)
	}

	// The PR request carried the right branch, base, and rubric body.
	if len(fake.PRs) != 1 {
		t.Fatal("no PR recorded")
	}
	req := fake.PRs[0]
	if req.Owner != "corp" || req.Repo != "payments" || req.BaseBranch != "main" {
		t.Errorf("pr target = %+v", req)
	}
	if req.HeadBranch != "wren/arpeet/r-1" {
		t.Errorf("head branch = %q", req.HeadBranch)
	}
	if !strings.Contains(req.Body, "Add idempotency keys") || !strings.Contains(req.Body, "Test plan") {
		t.Errorf("rubric body = %q", req.Body)
	}

	// The branch was actually pushed to origin.
	ob, _ := git.PlainOpen(origin)
	if _, err := ob.Reference(plumbing.NewBranchReferenceName("wren/arpeet/r-1"), true); err != nil {
		t.Errorf("branch not pushed: %v", err)
	}
}

func TestFinalizeNoChanges(t *testing.T) {
	origin := makeOrigin(t)
	ws := cloneInto(t, origin)
	spec := runspec.RunSpec{RunID: "r-1", Repo: "corp/payments", WorkspacePath: ws, BaseRef: "main"}
	if _, err := Run(context.Background(), spec, "", &github.Fake{}); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("err = %v, want ErrNoChanges", err)
	}
}

func TestFinalizeInvalidRepo(t *testing.T) {
	spec := runspec.RunSpec{RunID: "r-1", Repo: "not-a-repo", WorkspacePath: t.TempDir()}
	if _, err := Run(context.Background(), spec, "", &github.Fake{}); err == nil {
		t.Fatal("expected invalid repo error")
	}
}

func TestBranchName(t *testing.T) {
	if got := BranchName(runspec.RunSpec{RunID: "r-1", BranchPrefix: "wren/me"}); got != "wren/me/r-1" {
		t.Errorf("branch = %q", got)
	}
	if got := BranchName(runspec.RunSpec{RunID: "r-2"}); got != "wren/r-2" {
		t.Errorf("default branch = %q", got)
	}
}

func TestRubric(t *testing.T) {
	body := Rubric(runspec.RunSpec{RunID: "r-9", Prompt: "fix bug", Harness: "claude-code"})
	for _, want := range []string{"## Summary", "fix bug", "## Test plan", "r-9", "claude-code"} {
		if !strings.Contains(body, want) {
			t.Errorf("rubric missing %q", want)
		}
	}
}
