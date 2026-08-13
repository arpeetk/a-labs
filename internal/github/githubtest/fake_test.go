package githubtest

import (
	"context"
	"strings"
	"testing"

	"github.com/summiteight/wren/internal/github"
)

func TestFakeOpenPR(t *testing.T) {
	f := &Fake{}
	pr, err := f.OpenPR(context.Background(), github.PRRequest{Owner: "o", Repo: "r", HeadBranch: "b"})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 1 || !strings.Contains(pr.URL, "o/r/pull/1") {
		t.Errorf("pr = %+v", pr)
	}
	if len(f.PRs) != 1 {
		t.Error("PR not recorded")
	}
}
