package egress

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func connectVia(t *testing.T, frontURL, target string) *http.Response {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(frontURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body = io.NopCloser(br) // expose buffered tunnel bytes for reads
	t.Cleanup(func() { conn.Close() })
	return resp
}

func TestConnectForbidden(t *testing.T) {
	p, _ := New(Config{Allowlist: []string{"github.com"}})
	front := httptest.NewServer(p)
	defer front.Close()
	resp := connectVia(t, front.URL, "evil.example.com:443")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("CONNECT to disallowed host = %d, want 403", resp.StatusCode)
	}
}

func TestConnectTunnelsAllowed(t *testing.T) {
	// A raw TCP echo server as the tunnel target.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		_, _ = io.Copy(c, c)
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	// CONNECT is restricted to :443 in production; point the check at this test's
	// ephemeral listener port for the happy-path tunnel assertion.
	prevPort := connectPort
	connectPort = port
	defer func() { connectPort = prevPort }()

	p, _ := New(Config{Allowlist: []string{host}})
	front := httptest.NewServer(p)
	defer front.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(front.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", ln.Addr(), ln.Addr())
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: "CONNECT"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT to allowed host = %d, want 200", resp.StatusCode)
	}
	// Tunnel is live: write through it, expect the echo back.
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Errorf("tunnel echo = %q, want ping", buf)
	}
}

func TestReverseRouteInjectsCredsAndRewritesPath(t *testing.T) {
	var gotAuth, gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()

	p, err := New(Config{Routes: []Route{{
		Prefix:   "/github/",
		Upstream: upstream.URL,
		Auth:     BasicAuth{Username: "x-access-token", Password: "ghs_tok"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(p)
	defer front.Close()

	resp, err := http.Get(front.URL + "/github/owner/repo.git/info/refs")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	// Path prefix stripped, forwarded to upstream root.
	if gotPath != "/owner/repo.git/info/refs" {
		t.Errorf("upstream path = %q", gotPath)
	}
	// Credential injected by the proxy (runner sent none).
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Errorf("expected injected Basic auth, got %q", gotAuth)
	}
}

func TestReverseRouteHeaderAuth(t *testing.T) {
	var gotKey string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
	}))
	defer upstream.Close()

	p, _ := New(Config{Routes: []Route{{
		Prefix: "/anthropic/", Upstream: upstream.URL, Auth: HeaderAuth{Key: "x-api-key", Value: "sk-secret"},
	}}})
	front := httptest.NewServer(p)
	defer front.Close()

	resp, err := http.Get(front.URL + "/anthropic/v1/messages")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotKey != "sk-secret" {
		t.Errorf("x-api-key = %q, want injected", gotKey)
	}
}

func TestForwardProxyAllowlist(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "upstream-ok")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	p, _ := New(Config{Allowlist: []string{upURL.Hostname()}})
	front := httptest.NewServer(p)
	defer front.Close()

	// Allowed host → forwarded.
	frontURL, _ := url.Parse(front.URL)
	proxied := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(frontURL)}}
	req, _ := http.NewRequest("GET", upstream.URL+"/x", nil)
	resp, err := proxied.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "upstream-ok" {
		t.Errorf("allowed forward body = %q", body)
	}

	// Disallowed host → 403.
	req2, _ := http.NewRequest("GET", "http://evil.example.com/x", nil)
	resp2, err := proxied.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Errorf("disallowed host status = %d, want 403", resp2.StatusCode)
	}
}

func TestAllowedMatcher(t *testing.T) {
	p, _ := New(Config{Allowlist: []string{"github.com", "*.pkg.corp.com"}})
	cases := map[string]bool{
		"github.com":         true,
		"github.com:443":     true,
		"api.github.com":     false,
		"a.pkg.corp.com":     true,
		"a.pkg.corp.com:443": true,
		"pkg.corp.com":       false,
		"evil.com":           false,
	}
	for host, want := range cases {
		if got := p.Allowed(host); got != want {
			t.Errorf("Allowed(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestNoMatchingRoute(t *testing.T) {
	p, _ := New(Config{})
	front := httptest.NewServer(p)
	defer front.Close()
	resp, err := http.Get(front.URL + "/unmapped")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

// --- WS-1: proxy tightening ---

func TestConnectPortRestrictedTo443(t *testing.T) {
	// Host is allowed, but the port is not 443 → forbidden (no arbitrary-port
	// tunnels through the proxy).
	p, _ := New(Config{Allowlist: []string{"github.com"}})
	front := httptest.NewServer(p)
	defer front.Close()
	resp := connectVia(t, front.URL, "github.com:22")
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("CONNECT to :22 = %d, want 403 (only :443 allowed)", resp.StatusCode)
	}
}

func TestForwardStripsInboundCredsAndHopByHop(t *testing.T) {
	var gotAuth, gotProxyAuth, gotConnClose string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotProxyAuth = r.Header.Get("Proxy-Authorization")
		// A hop-by-hop header named in Connection must have been removed.
		gotConnClose = r.Header.Get("X-Hop")
		_, _ = io.WriteString(w, "ok")
	}))
	defer upstream.Close()
	upURL, _ := url.Parse(upstream.URL)

	p, _ := New(Config{Allowlist: []string{upURL.Hostname()}})
	front := httptest.NewServer(p)
	defer front.Close()
	frontURL, _ := url.Parse(front.URL)
	proxied := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(frontURL)}}

	req, _ := http.NewRequest("GET", upstream.URL+"/x", nil)
	req.Header.Set("Authorization", "Bearer runner-smuggled")
	req.Header.Set("Proxy-Authorization", "Basic abc")
	req.Header.Set("X-Hop", "should-be-dropped")
	req.Header.Set("Connection", "X-Hop")
	resp, err := proxied.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if gotAuth != "" {
		t.Errorf("inbound Authorization leaked upstream: %q", gotAuth)
	}
	if gotProxyAuth != "" {
		t.Errorf("inbound Proxy-Authorization leaked upstream: %q", gotProxyAuth)
	}
	if gotConnClose != "" {
		t.Errorf("Connection-listed hop-by-hop header leaked upstream: %q", gotConnClose)
	}
}
