package github

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSplitRepo(t *testing.T) {
	cases := []struct {
		in          string
		owner, repo string
		ok          bool
	}{
		{"corp/payments-api", "corp", "payments-api", true},
		{"owner/repo", "owner", "repo", true},
		{"noslash", "", "", false},
		{"a/b/c", "", "", false},
		{"/repo", "", "", false},
		{"owner/", "", "", false},
	}
	for _, c := range cases {
		o, r, ok := SplitRepo(c.in)
		if ok != c.ok || o != c.owner || r != c.repo {
			t.Errorf("SplitRepo(%q) = %q,%q,%v want %q,%q,%v", c.in, o, r, ok, c.owner, c.repo, c.ok)
		}
	}
}

func TestRESTOpenPR(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/repos/corp/payments/pulls" {
			buf := make([]byte, r.ContentLength)
			_, _ = r.Body.Read(buf)
			gotBody = string(buf)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"number":42,"html_url":"https://github.com/corp/payments/pull/42"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c, err := NewRESTWithBaseURL("tok", srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	pr, err := c.OpenPR(context.Background(), PRRequest{
		Owner: "corp", Repo: "payments", BaseBranch: "main",
		HeadBranch: "wren/x", Title: "T", Body: "B",
	})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 42 || pr.URL != "https://github.com/corp/payments/pull/42" {
		t.Fatalf("pr = %+v", pr)
	}
	if !strings.Contains(gotBody, `"wren/x"`) || !strings.Contains(gotBody, `"main"`) {
		t.Errorf("request body = %s", gotBody)
	}
}

func TestRESTOpenPRReturnsExisting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			// Create fails: a PR already exists.
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"A pull request already exists"}`))
		case http.MethodGet:
			_, _ = w.Write([]byte(`[{"number":7,"html_url":"https://github.com/corp/payments/pull/7"}]`))
		}
	}))
	defer srv.Close()

	c, _ := NewRESTWithBaseURL("tok", srv.URL, nil)
	pr, err := c.OpenPR(context.Background(), PRRequest{Owner: "corp", Repo: "payments", HeadBranch: "wren/x"})
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != 7 {
		t.Fatalf("expected existing PR #7, got %+v", pr)
	}
}
