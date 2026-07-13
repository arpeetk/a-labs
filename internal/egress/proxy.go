// Package egress implements the agent pod's egress-proxy (spec §5.6): the pod's
// controlled route to the internet. It (1) reverse-proxies specific upstreams
// (github.com, api.github.com, api.anthropic.com) injecting per-run credentials
// so the untrusted runner never holds a secret, and (2) enforces a domain
// allowlist for any other egress (HTTP forward + CONNECT tunneling).
//
// Note on enforcement: because containers in a pod share a network namespace,
// this sidecar cannot by itself stop a runner from bypassing it. Bypass
// prevention requires uid-based iptables redirection (Istio-style) or a separate
// egress pod + NetworkPolicy — a documented follow-up. What this delivers today
// is that credentials live only in the proxy, and the runner is configured to
// route through it.
package egress

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Shared defaults so the operator (runner config) and the proxy agree.
const (
	DefaultPort    = "8099"
	RouteGitHub    = "/github/"     // git smart-HTTP → github.com
	RouteGitHubAPI = "/github-api/" // REST API → api.github.com
	RouteAnthropic = "/anthropic/"  // model API → api.anthropic.com
)

// Route reverse-proxies requests under Prefix to Upstream, injecting Auth.
type Route struct {
	Prefix   string // e.g. "/github/"
	Upstream string // e.g. "https://github.com"
	Auth     Authorizer
}

// Config configures the proxy.
type Config struct {
	Allowlist []string // hostnames allowed for forward-proxy egress (supports "*.example.com")
	Routes    []Route
}

// Proxy is an http.Handler implementing the egress rules.
type Proxy struct {
	routes []compiledRoute
	allow  []string
	fwd    *http.Transport
}

type compiledRoute struct {
	prefix string
	rp     *httputil.ReverseProxy
}

// New compiles a Proxy from config.
func New(cfg Config) (*Proxy, error) {
	p := &Proxy{allow: cfg.Allowlist, fwd: &http.Transport{}}
	for _, rt := range cfg.Routes {
		u, err := url.Parse(rt.Upstream)
		if err != nil {
			return nil, fmt.Errorf("egress: bad upstream %q: %w", rt.Upstream, err)
		}
		prefix, auth, target := rt.Prefix, rt.Auth, u
		rp := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = target.Scheme
				req.URL.Host = target.Host
				req.Host = target.Host
				req.URL.Path = singleJoin(target.Path, strings.TrimPrefix(req.URL.Path, prefix))
				if auth != nil {
					auth.Apply(req)
				}
			},
		}
		p.routes = append(p.routes, compiledRoute{prefix: prefix, rp: rp})
	}
	return p, nil
}

// ServeHTTP routes a request: CONNECT tunnel (allowlist), reverse-proxy route
// (credential injection), or HTTP forward (allowlist).
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	for _, rt := range p.routes {
		if strings.HasPrefix(r.URL.Path, rt.prefix) {
			rt.rp.ServeHTTP(w, r)
			return
		}
	}
	if r.URL.IsAbs() { // forward-proxy style (absolute request URI)
		p.handleHTTPForward(w, r)
		return
	}
	http.Error(w, "egress: no matching route", http.StatusNotFound)
}

// Allowed reports whether host is permitted for forward-proxy egress.
func (p *Proxy) Allowed(host string) bool {
	host = stripPort(host)
	for _, a := range p.allow {
		if a == host {
			return true
		}
		if strings.HasPrefix(a, "*.") && strings.HasSuffix(host, a[1:]) {
			return true
		}
	}
	return false
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	if !p.Allowed(r.Host) {
		http.Error(w, "egress blocked: "+stripPort(r.Host), http.StatusForbidden)
		return
	}
	dst, err := net.DialTimeout("tcp", r.Host, 15*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		dst.Close()
		http.Error(w, "egress: hijacking unsupported", http.StatusInternalServerError)
		return
	}
	src, _, err := hj.Hijack()
	if err != nil {
		dst.Close()
		return
	}
	_, _ = src.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	go tunnel(dst, src)
	go tunnel(src, dst)
}

func (p *Proxy) handleHTTPForward(w http.ResponseWriter, r *http.Request) {
	if !p.Allowed(r.URL.Host) {
		http.Error(w, "egress blocked: "+stripPort(r.URL.Host), http.StatusForbidden)
		return
	}
	r.RequestURI = ""
	resp, err := p.fwd.RoundTrip(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func tunnel(dst, src net.Conn) {
	defer dst.Close()
	defer src.Close()
	_, _ = io.Copy(dst, src)
}

func stripPort(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return hostport
}

func singleJoin(a, b string) string {
	b = strings.TrimPrefix(b, "/")
	if a == "" || a == "/" {
		return "/" + b
	}
	if strings.HasSuffix(a, "/") {
		return a + b
	}
	return a + "/" + b
}
