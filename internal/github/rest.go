package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	gh "github.com/google/go-github/v66/github"
)

// RESTClient opens PRs via the GitHub REST API (go-github).
type RESTClient struct {
	gh *gh.Client
}

var _ Client = (*RESTClient)(nil)

// NewREST builds a RESTClient authenticated with the given token (a GitHub App
// installation token or a PAT). httpClient may be nil.
func NewREST(token string, httpClient *http.Client) *RESTClient {
	c := gh.NewClient(httpClient).WithAuthToken(token)
	return &RESTClient{gh: c}
}

// NewRESTWithBaseURL is NewREST pointed at a custom API base URL (for testing
// against a mock GitHub server). The path is used verbatim (no /api/v3 munging).
func NewRESTWithBaseURL(token, baseURL string, httpClient *http.Client) (*RESTClient, error) {
	c := gh.NewClient(httpClient).WithAuthToken(token)
	if !strings.HasSuffix(baseURL, "/") {
		baseURL += "/"
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	c.BaseURL = u
	return &RESTClient{gh: c}, nil
}

// OpenPR opens a pull request, returning an existing open PR for the same head
// branch if one is already present.
func (c *RESTClient) OpenPR(ctx context.Context, req PRRequest) (*PullRequest, error) {
	pr, _, err := c.gh.PullRequests.Create(ctx, req.Owner, req.Repo, &gh.NewPullRequest{
		Title: gh.String(req.Title),
		Head:  gh.String(req.HeadBranch),
		Base:  gh.String(req.BaseBranch),
		Body:  gh.String(req.Body),
	})
	if err != nil {
		if existing, found := c.findExisting(ctx, req); found {
			return existing, nil
		}
		return nil, fmt.Errorf("create pull request: %w", err)
	}
	return &PullRequest{Number: pr.GetNumber(), URL: pr.GetHTMLURL(), Branch: req.HeadBranch}, nil
}

// findExisting looks up an open PR for the head branch (used when Create fails
// because a PR already exists — common on resume/retry).
func (c *RESTClient) findExisting(ctx context.Context, req PRRequest) (*PullRequest, bool) {
	prs, _, err := c.gh.PullRequests.List(ctx, req.Owner, req.Repo, &gh.PullRequestListOptions{
		State: "open",
		Head:  req.Owner + ":" + req.HeadBranch,
	})
	if err != nil || len(prs) == 0 {
		return nil, false
	}
	pr := prs[0]
	return &PullRequest{Number: pr.GetNumber(), URL: pr.GetHTMLURL(), Branch: req.HeadBranch}, true
}
