package github

import (
	"context"
	"fmt"
	"sync"
)

// Fake is an in-memory Client for tests. It records opened PRs and returns
// deterministic URLs.
type Fake struct {
	mu   sync.Mutex
	next int
	PRs  []PRRequest
	Err  error // if set, OpenPR returns it
}

var _ Client = (*Fake)(nil)

// OpenPR implements Client.
func (f *Fake) OpenPR(_ context.Context, req PRRequest) (*PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return nil, f.Err
	}
	f.next++
	f.PRs = append(f.PRs, req)
	return &PullRequest{
		Number: f.next,
		URL:    fmt.Sprintf("https://github.com/%s/%s/pull/%d", req.Owner, req.Repo, f.next),
		Branch: req.HeadBranch,
	}, nil
}
