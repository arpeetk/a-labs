// Package finalize turns a completed run's workspace changes into a pull request:
// commit the changes on a run branch, push, and open a PR
// with the rubric body. It composes internal/gitwork and internal/github, so it
// is testable against a local bare repo and a fake GitHub client.
package finalize

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"

	"github.com/summiteight/wren/internal/github"
	"github.com/summiteight/wren/internal/gitwork"
	"github.com/summiteight/wren/internal/runspec"
)

// ErrNoChanges is re-exported so callers can treat an empty run distinctly.
var ErrNoChanges = gitwork.ErrNoChanges

// prAuthor is the git identity for Wren-authored commits.
var prAuthor = gitwork.Author{Name: "Wren Agent", Email: "wren@users.noreply.github.com"}

// Run commits the workspace, pushes the run branch, and opens a PR. The
// workspace must already be a git clone of the repo (done by hydrate).
func Run(ctx context.Context, spec runspec.RunSpec, token string, client github.Client) (*github.PullRequest, error) {
	owner, name, ok := github.SplitRepo(spec.Repo)
	if !ok {
		return nil, fmt.Errorf("finalize: invalid repo %q", spec.Repo)
	}
	repo, err := git.PlainOpen(spec.WorkspacePath)
	if err != nil {
		return nil, fmt.Errorf("finalize: open workspace repo: %w", err)
	}

	branch := BranchName(spec)
	base := spec.BaseRef
	if base == "" {
		base = "main"
	}
	title := "Wren: " + truncate(spec.Prompt, 72)

	// Validate the history before staging anything. In particular, an existing
	// run branch with unrelated ancestry must not become publishable merely
	// because CommitAll adds a new commit on top of it.
	if err := validateStartingPoint(repo, branch, base); err != nil {
		return nil, fmt.Errorf("finalize: verify starting point: %w", err)
	}
	if _, err := gitwork.CommitAll(repo, branch, title, prAuthor); err != nil {
		if !errors.Is(err, gitwork.ErrNoChanges) {
			return nil, err
		}
		// A clean worktree can mean either that the harness did nothing or that
		// it committed its own changes. Compare the resulting run branch with
		// origin/base, which hydrate recorded before the untrusted harness ran.
		// Requiring that base to be an ancestor also prevents publishing a
		// replacement or otherwise unrelated history.
		ahead, err := branchAheadOfBase(repo, branch, base)
		if err != nil {
			return nil, fmt.Errorf("finalize: verify run branch: %w", err)
		}
		if !ahead {
			return nil, gitwork.ErrNoChanges
		}
	}
	if err := gitwork.Push(repo, branch, token); err != nil {
		return nil, classify(err)
	}
	pr, err := client.OpenPR(ctx, github.PRRequest{
		Owner:      owner,
		Repo:       name,
		BaseBranch: base,
		HeadBranch: branch,
		Title:      title,
		Body:       Rubric(spec),
	})
	if err != nil {
		return nil, classify(err)
	}
	return pr, nil
}

// branchAheadOfBase reports whether branch contains commits on top of the
// requested base. The remote-tracking ref is the immutable-in-run anchor:
// the harness may commit on or move the local base branch itself.
func branchAheadOfBase(repo *git.Repository, branch, base string) (bool, error) {
	branchRef, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		return false, fmt.Errorf("resolve run branch %s: %w", branch, err)
	}
	return commitAheadOfBase(repo, branchRef.Hash(), base)
}

func validateStartingPoint(repo *git.Repository, branch, base string) error {
	ref, err := repo.Reference(plumbing.NewBranchReferenceName(branch), true)
	if err != nil {
		ref, err = repo.Head()
		if err != nil {
			return fmt.Errorf("resolve HEAD: %w", err)
		}
	}
	_, err = commitAheadOfBase(repo, ref.Hash(), base)
	return err
}

func commitAheadOfBase(repo *git.Repository, head plumbing.Hash, base string) (bool, error) {
	baseRef, err := repo.Reference(plumbing.NewRemoteReferenceName("origin", base), true)
	if err != nil {
		return false, fmt.Errorf("resolve requested base origin/%s: %w", base, err)
	}
	if head == baseRef.Hash() {
		return false, nil
	}

	commits, err := repo.Log(&git.LogOptions{From: head})
	if err != nil {
		return false, fmt.Errorf("walk candidate history: %w", err)
	}
	foundBase := false
	err = commits.ForEach(func(commit *object.Commit) error {
		if commit.Hash == baseRef.Hash() {
			foundBase = true
			return storer.ErrStop
		}
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("walk candidate history: %w", err)
	}
	if !foundBase {
		return false, fmt.Errorf("candidate history is not descended from requested base origin/%s", base)
	}
	return true, nil
}

// BranchName is the run's PR branch: "<prefix>/<run-id>".
func BranchName(spec runspec.RunSpec) string {
	prefix := spec.BranchPrefix
	if prefix == "" {
		prefix = "wren"
	}
	return prefix + "/" + spec.RunID
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
