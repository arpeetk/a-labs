// Package finalize turns a completed run's workspace changes into a pull request
// (spec §5.7 PR flow): commit the changes on a run branch, push, and open a PR
// with the rubric body. It composes internal/gitwork and internal/github, so it
// is testable against a local bare repo and a fake GitHub client.
package finalize

import (
	"context"
	"fmt"

	"github.com/go-git/go-git/v5"

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

	if _, err := gitwork.CommitAll(repo, branch, title, prAuthor); err != nil {
		return nil, err // includes ErrNoChanges
	}
	if err := gitwork.Push(repo, branch, token); err != nil {
		return nil, err
	}
	return client.OpenPR(ctx, github.PRRequest{
		Owner:      owner,
		Repo:       name,
		BaseBranch: base,
		HeadBranch: branch,
		Title:      title,
		Body:       Rubric(spec),
	})
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
