package podruntime

import "testing"

// findRoute returns the upstream for a given route prefix, or "" if absent.
func findUpstream(cfg egressRoutesView, prefix string) string {
	for _, r := range cfg {
		if r.prefix == prefix {
			return r.upstream
		}
	}
	return ""
}

// egressRoutesView flattens egressConfigFromEnv routes to the fields we assert
// on, decoupling the test from the egress.Route auth internals.
type egressRoutesView []struct {
	prefix   string
	upstream string
}

func routesView() egressRoutesView {
	cfg := egressConfigFromEnv()
	out := make(egressRoutesView, 0, len(cfg.Routes))
	for _, r := range cfg.Routes {
		out = append(out, struct {
			prefix   string
			upstream string
		}{r.Prefix, r.Upstream})
	}
	return out
}

func TestEnvUpstreamDefault(t *testing.T) {
	if got := envUpstream("WREN_TEST_UNSET_UPSTREAM", "https://default.example"); got != "https://default.example" {
		t.Fatalf("unset override = %q, want default", got)
	}
}

func TestEnvUpstreamOverrideAndTrim(t *testing.T) {
	t.Setenv("WREN_TEST_SET_UPSTREAM", "  http://gitea.local:3000  ")
	if got := envUpstream("WREN_TEST_SET_UPSTREAM", "https://default.example"); got != "http://gitea.local:3000" {
		t.Fatalf("override = %q, want trimmed value", got)
	}
	// Whitespace-only is treated as unset.
	t.Setenv("WREN_TEST_BLANK_UPSTREAM", "   ")
	if got := envUpstream("WREN_TEST_BLANK_UPSTREAM", "https://default.example"); got != "https://default.example" {
		t.Fatalf("blank override = %q, want default", got)
	}
}

func TestEgressConfigDefaultUpstreams(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "gh_tok")
	t.Setenv("ANTHROPIC_API_KEY", "an_key")
	t.Setenv("WREN_GITHUB_UPSTREAM", "")
	t.Setenv("WREN_GITHUB_API_UPSTREAM", "")
	t.Setenv("WREN_ANTHROPIC_UPSTREAM", "")

	v := routesView()
	if got := findUpstream(v, "/github/"); got != defaultGitHubUpstream {
		t.Errorf("github upstream = %q, want %q", got, defaultGitHubUpstream)
	}
	if got := findUpstream(v, "/github-api/"); got != defaultGitHubAPIUpstream {
		t.Errorf("github-api upstream = %q, want %q", got, defaultGitHubAPIUpstream)
	}
	if got := findUpstream(v, "/anthropic/"); got != defaultAnthropicUpstream {
		t.Errorf("anthropic upstream = %q, want %q", got, defaultAnthropicUpstream)
	}
}

func TestEgressConfigOverriddenUpstreams(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "gh_tok")
	t.Setenv("ANTHROPIC_API_KEY", "an_key")
	t.Setenv("WREN_GITHUB_UPSTREAM", "http://gitea.local:3000")
	t.Setenv("WREN_GITHUB_API_UPSTREAM", "http://gitea.local:3000/api")
	t.Setenv("WREN_ANTHROPIC_UPSTREAM", "http://anthropic-stub.local")

	v := routesView()
	if got := findUpstream(v, "/github/"); got != "http://gitea.local:3000" {
		t.Errorf("github upstream = %q, want override", got)
	}
	if got := findUpstream(v, "/github-api/"); got != "http://gitea.local:3000/api" {
		t.Errorf("github-api upstream = %q, want override", got)
	}
	if got := findUpstream(v, "/anthropic/"); got != "http://anthropic-stub.local" {
		t.Errorf("anthropic upstream = %q, want override", got)
	}
}
