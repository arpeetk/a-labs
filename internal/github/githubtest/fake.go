// Package githubtest provides test doubles for the GitHub client contract.
package githubtest

import (
	"context"
	"fmt"
	"sync"

	"github.com/summiteight/wren/internal/github"
)

// Fake records opened pull requests and returns deterministic URLs.
type Fake struct {
	mu   sync.Mutex
	next int
	PRs  []github.PRRequest
	Err  error
}

var _ github.Client = (*Fake)(nil)

func (f *Fake) OpenPR(_ context.Context, req github.PRRequest) (*github.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	f.next++
	f.PRs = append(f.PRs, req)
	return &github.PullRequest{
		Number: f.next,
		URL:    fmt.Sprintf("https://github.com/%s/%s/pull/%d", req.Owner, req.Repo, f.next),
		Branch: req.HeadBranch,
	}, nil
}
