package gitwork

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// makeOrigin creates a bare repo with one commit on main and returns its path.
// It uses a non-bare seed repo to author the initial commit, then a bare clone
// serves as the push target ("origin").
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
	if _, err := wt.Commit("init", &git.CommitOptions{
		Author: &object.Signature{Name: "seed", Email: "seed@x"},
	}); err != nil {
		t.Fatal(err)
	}
	// Name the branch "main".
	head, _ := repo.Head()
	ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName("main"), head.Hash())
	if err := repo.Storer.SetReference(ref); err != nil {
		t.Fatal(err)
	}

	bare := t.TempDir()
	if _, err := git.PlainClone(bare, true, &git.CloneOptions{URL: seed}); err != nil {
		t.Fatal(err)
	}
	return bare
}

func TestCloneCommitPush(t *testing.T) {
	origin := makeOrigin(t)
	ws := t.TempDir()

	repo, err := Clone("file://"+origin, "", ws, "")
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	// Harness makes a change.
	if err := os.WriteFile(filepath.Join(ws, "WREN.md"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	hash, err := CommitAll(repo, "wren/r-1", "Wren: do it", Author{Name: "wren", Email: "wren@x"})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}
	if hash.IsZero() {
		t.Fatal("zero commit hash")
	}

	if err := Push(repo, "wren/r-1", ""); err != nil {
		t.Fatalf("push: %v", err)
	}

	// Verify the branch landed on origin.
	ob, err := git.PlainOpen(origin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ob.Reference(plumbing.NewBranchReferenceName("wren/r-1"), true); err != nil {
		t.Fatalf("branch not pushed to origin: %v", err)
	}
}

func TestCommitAllNoChanges(t *testing.T) {
	origin := makeOrigin(t)
	ws := t.TempDir()
	repo, err := Clone("file://"+origin, "", ws, "")
	if err != nil {
		t.Fatal(err)
	}
	// No workspace change → ErrNoChanges.
	if _, err := CommitAll(repo, "wren/empty", "noop", Author{Name: "w", Email: "w@x"}); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("err = %v, want ErrNoChanges", err)
	}
}

// TestCommitAllTwiceIsIdempotent is the WS-11 resume regression: a pod that
// crashes after `git commit` but before the push re-runs finalize on the same
// durable workspace. The second CommitAll must check out the existing run
// branch (never fail "branch already exists") and report ErrNoChanges — the
// caller then continues to the idempotent push/PR.
func TestCommitAllTwiceIsIdempotent(t *testing.T) {
	origin := makeOrigin(t)
	ws := t.TempDir()
	repo, err := Clone("file://"+origin, "", ws, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "WREN.md"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := CommitAll(repo, "wren/r-1", "Wren: do it", Author{Name: "w", Email: "w@x"})
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}

	// Second run (resume): same workspace, no new changes.
	if _, err := CommitAll(repo, "wren/r-1", "Wren: do it", Author{Name: "w", Email: "w@x"}); !errors.Is(err, ErrNoChanges) {
		t.Fatalf("second CommitAll err = %v, want ErrNoChanges (never 'branch already exists')", err)
	}

	// The branch still points at the first commit (no empty commit on top).
	ref, err := repo.Reference(plumbing.NewBranchReferenceName("wren/r-1"), true)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Hash() != hash {
		t.Errorf("branch moved on idempotent re-run: %s → %s", hash, ref.Hash())
	}
}

// TestCommitAllExistingBranchNewChanges covers the other resume shape: the
// branch exists (created before the crash) but the resumed harness produced
// more work — the new commit must land on top of the existing branch.
func TestCommitAllExistingBranchNewChanges(t *testing.T) {
	origin := makeOrigin(t)
	ws := t.TempDir()
	repo, err := Clone("file://"+origin, "", ws, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ws, "WREN.md"), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	first, err := CommitAll(repo, "wren/r-1", "Wren: part 1", Author{Name: "w", Email: "w@x"})
	if err != nil {
		t.Fatalf("first commit: %v", err)
	}

	// Resume: the harness does more work on the same workspace.
	if err := os.WriteFile(filepath.Join(ws, "WREN.md"), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := CommitAll(repo, "wren/r-1", "Wren: part 2", Author{Name: "w", Email: "w@x"})
	if err != nil {
		t.Fatalf("second CommitAll on existing branch: %v", err)
	}
	if second == first {
		t.Fatal("expected a new commit on top of the existing branch")
	}
	commit, err := repo.CommitObject(second)
	if err != nil {
		t.Fatal(err)
	}
	if commit.NumParents() != 1 || commit.ParentHashes[0] != first {
		t.Errorf("second commit should have the first as parent; parents = %v", commit.ParentHashes)
	}
}

// TestCommitAllModifiedTrackedFile pins the checkout-guard fix: an agent that
// MODIFIES an existing tracked file (not just adds new ones) leaves unstaged
// changes that go-git's MergeReset checkout rejects — CommitAll must still
// commit them onto the fresh run branch.
func TestCommitAllModifiedTrackedFile(t *testing.T) {
	origin := makeOrigin(t)
	ws := t.TempDir()
	repo, err := Clone("file://"+origin, "", ws, "")
	if err != nil {
		t.Fatal(err)
	}
	// README.md is tracked on main; the harness edits it in place.
	if err := os.WriteFile(filepath.Join(ws, "README.md"), []byte("# seed\nedited by agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	hash, err := CommitAll(repo, "wren/r-2", "Wren: edit", Author{Name: "w", Email: "w@x"})
	if err != nil {
		t.Fatalf("commit with modified tracked file: %v", err)
	}
	if hash.IsZero() {
		t.Fatal("zero commit hash")
	}
}

func TestCloneBadURL(t *testing.T) {
	if _, err := Clone("file:///nonexistent/repo", "", t.TempDir(), ""); err == nil {
		t.Fatal("expected clone error")
	}
}
