// Package github is Wren's GitHub integration (spec §5.7): minting short-lived
// GitHub App installation tokens and opening pull requests. The Client interface
// keeps callers testable; RESTClient is the go-github implementation and Fake is
// an in-memory double.
package github

import (
	"context"
	"strings"
)

// PRRequest describes a pull request to open. The head branch must already be
// pushed to the remote (see internal/gitwork).
type PRRequest struct {
	Owner      string
	Repo       string
	BaseBranch string
	HeadBranch string
	Title      string
	Body       string
}

// PullRequest is the opened (or pre-existing) pull request.
type PullRequest struct {
	Number int
	URL    string
	Branch string
}

// Client opens pull requests.
type Client interface {
	// OpenPR opens a PR, or returns the existing one if head already has an open
	// PR against base (idempotent for retries/resume).
	OpenPR(ctx context.Context, req PRRequest) (*PullRequest, error)
}

// SplitRepo splits an "owner/repo" string. It returns ok=false (and empty
// strings) for anything that is not exactly one owner and one repo.
func SplitRepo(full string) (owner, repo string, ok bool) {
	o, r, found := strings.Cut(full, "/")
	if !found || o == "" || r == "" || strings.Contains(r, "/") {
		return "", "", false
	}
	return o, r, true
}
