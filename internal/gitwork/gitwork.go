// Package gitwork performs the git operations of a run's finalize step using
// go-git (pure Go, so it runs in the distroless runtime image without a git
// binary): clone the repo, commit the workspace changes on a new branch, and
// push it so a PR can be opened (spec §5.7).
package gitwork

import (
	"errors"
	"fmt"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
)

// ErrNoChanges means the harness produced no workspace changes, so there is
// nothing to commit or open a PR for.
var ErrNoChanges = errors.New("no workspace changes to commit")

// Author identifies the committer.
type Author struct {
	Name  string
	Email string
}

func auth(token string) *http.BasicAuth {
	if token == "" {
		return nil
	}
	// GitHub accepts the token as the password with any non-empty username
	// (x-access-token is conventional for installation tokens).
	return &http.BasicAuth{Username: "x-access-token", Password: token}
}

// Clone clones repoURL at baseRef into dir. If baseRef is empty the remote's
// default branch is used. The clone is single-branch but full-depth: a shallow
// clone can make pushing a new branch to some remotes fail ("shallow update
// not allowed").
func Clone(repoURL, baseRef, dir, token string) (*git.Repository, error) {
	opts := &git.CloneOptions{URL: repoURL, Auth: auth(token), SingleBranch: true}
	if baseRef != "" {
		opts.ReferenceName = plumbing.NewBranchReferenceName(baseRef)
	}
	repo, err := git.PlainClone(dir, false, opts)
	if err != nil {
		return nil, fmt.Errorf("clone %s: %w", repoURL, err)
	}
	return repo, nil
}

// CommitAll switches to the run branch, stages every change in the worktree,
// and commits. It returns the commit hash. A no-op (no changes) is an error so
// the caller can avoid opening an empty PR.
//
// Resume-safe: a pod restarted after the commit (but before/during the push)
// re-runs finalize on the durable workspace, where the run branch already
// exists. Re-creating it would fail "branch already exists" and turn a good
// run terminal, so ensureBranch reuses the existing branch; if its HEAD
// already captures the worktree there is nothing new to commit (ErrNoChanges)
// and the caller proceeds to the idempotent push/PR.
func CommitAll(repo *git.Repository, branch, message string, a Author) (plumbing.Hash, error) {
	wt, err := repo.Worktree()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if err := ensureBranch(repo, branch); err != nil {
		return plumbing.ZeroHash, err
	}
	if err := wt.AddGlob("."); err != nil {
		return plumbing.ZeroHash, err
	}
	status, err := wt.Status()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if status.IsClean() {
		return plumbing.ZeroHash, ErrNoChanges
	}
	hash, err := wt.Commit(message, &git.CommitOptions{
		Author: &object.Signature{Name: a.Name, Email: a.Email, When: time.Now()},
	})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("commit: %w", err)
	}
	return hash, nil
}

// ensureBranch points HEAD at the run branch, creating the branch at the
// current HEAD when it does not exist yet. It moves refs only — never the
// worktree or index: go-git's Checkout (MergeReset) rejects a worktree with
// unstaged modifications, which is exactly what a harness leaves behind (an
// agent edits tracked files), and a hard reset would destroy that work. When
// HEAD is already on the run branch (a resume — .git survives on the durable
// workspace PVC) this is a no-op.
func ensureBranch(repo *git.Repository, branch string) error {
	ref := plumbing.NewBranchReferenceName(branch)
	head, err := repo.Head()
	if err != nil {
		return fmt.Errorf("read HEAD: %w", err)
	}
	if head.Name() == ref {
		return nil
	}
	if _, err := repo.Reference(ref, true); err != nil {
		if err := repo.Storer.SetReference(plumbing.NewHashReference(ref, head.Hash())); err != nil {
			return fmt.Errorf("create branch %s: %w", branch, err)
		}
	}
	if err := repo.Storer.SetReference(plumbing.NewSymbolicReference(plumbing.HEAD, ref)); err != nil {
		return fmt.Errorf("switch to branch %s: %w", branch, err)
	}
	return nil
}

// Push pushes branch to origin.
func Push(repo *git.Repository, branch, token string) error {
	ref := plumbing.NewBranchReferenceName(branch)
	err := repo.Push(&git.PushOptions{
		Auth: auth(token),
		RefSpecs: []config.RefSpec{
			config.RefSpec(fmt.Sprintf("%s:%s", ref, ref)),
		},
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return fmt.Errorf("push %s: %w", branch, err)
	}
	return nil
}
