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

func TestCloneBadURL(t *testing.T) {
	if _, err := Clone("file:///nonexistent/repo", "", t.TempDir(), ""); err == nil {
		t.Fatal("expected clone error")
	}
}
