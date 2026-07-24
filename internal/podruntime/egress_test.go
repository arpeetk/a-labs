package podruntime

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/summiteight/wren/internal/runspec"
)

func TestRunEgressProxyServes(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "upstream-ok")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	const port = "37411"
	t.Setenv("WREN_EGRESS_PORT", port)
	t.Setenv("WREN_EGRESS_ALLOWLIST", upURL.Hostname())
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunEgressProxy(ctx, io.Discard) }()

	addr := "127.0.0.1:" + port
	waitListening(t, addr)

	proxyURL, _ := url.Parse("http://" + addr)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	// Allowlisted host is forwarded.
	resp, err := client.Get(upstream.URL + "/x")
	if err != nil {
		t.Fatalf("forward via proxy: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "upstream-ok" {
		t.Errorf("forwarded body = %q", body)
	}

	// Non-allowlisted host is blocked before any dial (no real network).
	resp2, err := client.Get("http://blocked.example.com/")
	if err != nil {
		t.Fatalf("blocked request: %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("blocked status = %d, want 403", resp2.StatusCode)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("RunEgressProxy returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("RunEgressProxy did not shut down on cancel")
	}
}

func waitListening(t *testing.T, addr string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond); err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("proxy never listened on %s", addr)
}

func TestGitCloneURLProxyMode(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("WREN_EGRESS_PROXY", "http://127.0.0.1:8099")

	url, token, ok := gitCloneURL(runspec.RunSpec{Repo: "corp/payments"})
	if !ok || url != "http://127.0.0.1:8099/github/corp/payments.git" || token != "" {
		t.Fatalf("proxy clone = %q token=%q ok=%v", url, token, ok)
	}
}

func TestGitCloneURLDirectMode(t *testing.T) {
	t.Setenv("WREN_EGRESS_PROXY", "")
	t.Setenv("GITHUB_TOKEN", "ghs_tok")

	url, token, ok := gitCloneURL(runspec.RunSpec{Repo: "corp/payments"})
	if !ok || url != "https://github.com/corp/payments.git" || token != "ghs_tok" {
		t.Fatalf("direct clone = %q token=%q ok=%v", url, token, ok)
	}
}

func TestGitCloneURLNoneConfigured(t *testing.T) {
	t.Setenv("WREN_EGRESS_PROXY", "")
	t.Setenv("GITHUB_TOKEN", "")
	if _, _, ok := gitCloneURL(runspec.RunSpec{Repo: "corp/payments"}); ok {
		t.Fatal("expected not-ok with no proxy and no token")
	}
	if _, _, ok := gitCloneURL(runspec.RunSpec{}); ok {
		t.Fatal("expected not-ok with no repo")
	}
}

func TestPrClientAndTokenProxyMode(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("WREN_EGRESS_PROXY", "http://127.0.0.1:8099")
	client, token, err := prClientAndToken()
	if err != nil || client == nil || token != "" {
		t.Fatalf("proxy client=%v token=%q err=%v", client, token, err)
	}
}

func TestPrClientAndTokenDirectMode(t *testing.T) {
	t.Setenv("WREN_EGRESS_PROXY", "")
	t.Setenv("GITHUB_TOKEN", "ghs_tok")
	client, token, err := prClientAndToken()
	if err != nil || client == nil || token != "ghs_tok" {
		t.Fatalf("direct client=%v token=%q err=%v", client, token, err)
	}
}

func TestPrConfiguredProxyMode(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("WREN_EGRESS_PROXY", "http://127.0.0.1:8099")
	if !prConfigured(runspec.RunSpec{Repo: "a/b"}) {
		t.Error("repo + proxy → configured")
	}
	if prConfigured(runspec.RunSpec{}) {
		t.Error("no repo → not configured")
	}
}

func TestEgressConfigFromEnv(t *testing.T) {
	t.Setenv("WREN_EGRESS_ALLOWLIST", "github.com, *.pkg.corp.com ,")
	t.Setenv("GITHUB_TOKEN", "ghs_tok")
	t.Setenv("ANTHROPIC_API_KEY", "sk-key")
	t.Setenv("OPENAI_API_KEY", "")

	cfg := egressConfigFromEnv()
	if len(cfg.Allowlist) != 2 { // empty entry trimmed out
		t.Errorf("allowlist = %v", cfg.Allowlist)
	}
	// 2 github routes (git + api) + 1 anthropic = 3.
	if len(cfg.Routes) != 3 {
		t.Fatalf("routes = %d, want 3", len(cfg.Routes))
	}
	prefixes := map[string]bool{}
	for _, r := range cfg.Routes {
		prefixes[r.Prefix] = true
	}
	for _, want := range []string{"/github/", "/github-api/", "/anthropic/"} {
		if !prefixes[want] {
			t.Errorf("missing route %s", want)
		}
	}
}

// An OPENAI_API_KEY on the proxy adds the /openai/ credentialed route (WS-12).
func TestEgressConfigOpenAIRoute(t *testing.T) {
	t.Setenv("WREN_EGRESS_ALLOWLIST", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "sk-openai")

	cfg := egressConfigFromEnv()
	if len(cfg.Routes) != 1 || cfg.Routes[0].Prefix != "/openai/" {
		t.Fatalf("routes = %+v, want only /openai/", cfg.Routes)
	}
	// The route must inject the key as a Bearer Authorization header.
	req, err := http.NewRequest(http.MethodPost, "http://proxy/openai/v1/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Routes[0].Auth.Apply(req)
	if got := req.Header.Get("Authorization"); got != "Bearer sk-openai" {
		t.Errorf("Authorization = %q, want Bearer injection", got)
	}
}

func TestEgressConfigNoSecrets(t *testing.T) {
	t.Setenv("WREN_EGRESS_ALLOWLIST", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("OPENAI_API_KEY", "")
	cfg := egressConfigFromEnv()
	if len(cfg.Routes) != 0 || len(cfg.Allowlist) != 0 {
		t.Errorf("expected empty config, got routes=%d allow=%d", len(cfg.Routes), len(cfg.Allowlist))
	}
}
