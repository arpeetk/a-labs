// Package egress implements the agent pod's egress-proxy (spec §5.6): the pod's
// controlled route to the internet. It (1) reverse-proxies specific upstreams
// (github.com, api.github.com, api.anthropic.com) injecting per-run credentials
// so the untrusted runner never holds a secret, and (2) enforces a domain
// allowlist for any other egress (HTTP forward + CONNECT tunneling).
//
// Enforcement (WS-1): the runner is *physically* prevented from bypassing this
// proxy by in-pod iptables uid-isolation — the `egress-lockdown` init container
// rejects all OUTPUT except (a) traffic to the proxy's localhost port and (b)
// traffic owned by the proxy's own uid (65533). The runner (uid 65532) can reach
// nothing but the proxy; the proxy resolves DNS and reaches the world. Because
// pod containers share a network namespace, NetworkPolicy alone cannot make that
// distinction — hence the uid-match. This package additionally hardens the
// forward path: CONNECT is restricted to :443, hop-by-hop and inbound
// credential headers are stripped, and the forward transport carries timeouts.
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

// forwardTransport is the transport used for the allowlist HTTP forward path.
// It carries dial/response/idle timeouts so a slow or hung upstream cannot pin
// a runner request open indefinitely (a small DoS / resource-exhaustion guard).
func forwardTransport() *http.Transport {
	return &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: 5 * time.Second,
	}
}

// New compiles a Proxy from config.
func New(cfg Config) (*Proxy, error) {
	p := &Proxy{allow: cfg.Allowlist, fwd: forwardTransport()}
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
				// The proxy is the sole credential authority on the reverse
				// path too. Apply uses Set — it replaces only its OWN header,
				// so an inbound Authorization would otherwise ride the
				// x-api-key (Anthropic) route upstream untouched. Scrub first,
				// then inject.
				scrubForwardHeaders(req.Header)
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

// connectPort is the only port CONNECT tunnels may target. Restricting to 443
// keeps the tunnel to TLS: a runner cannot open an arbitrary-port raw tunnel
// (e.g. to a plaintext service or an exfil listener) through the proxy. A
// package var (not const) only so the tunnel test can point at an ephemeral
// listener; production never changes it.
var connectPort = "443"

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	if port := portOf(r.Host); port != connectPort {
		http.Error(w, "egress: CONNECT allowed only to port "+connectPort, http.StatusForbidden)
		return
	}
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
	// Scrub hop-by-hop headers and any credential the runner tried to smuggle
	// out: the proxy is the sole credential authority, so an inbound
	// Authorization/Proxy-Authorization is never forwarded upstream.
	scrubForwardHeaders(r.Header)
	resp, err := p.fwd.RoundTrip(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	removeHopByHop(resp.Header)
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

// portOf returns the port component of a host:port, or "" if none is present.
func portOf(hostport string) string {
	if _, p, err := net.SplitHostPort(hostport); err == nil {
		return p
	}
	return ""
}

// hopByHopHeaders are per-connection headers that must not be forwarded end to
// end (RFC 7230 §6.1). We also strip inbound credential headers so the runner
// cannot inject its own auth on the forward path — the proxy owns credentials.
var hopByHopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}

// removeHopByHop deletes hop-by-hop headers, including any listed in the
// request's Connection header (RFC 7230 §6.1).
func removeHopByHop(h http.Header) {
	for _, c := range h.Values("Connection") {
		for _, f := range strings.Split(c, ",") {
			if f = strings.TrimSpace(f); f != "" {
				h.Del(f)
			}
		}
	}
	for _, name := range hopByHopHeaders {
		h.Del(name)
	}
}

// scrubForwardHeaders strips hop-by-hop headers and inbound credentials on the
// forward (runner → upstream) path.
func scrubForwardHeaders(h http.Header) {
	removeHopByHop(h)
	h.Del("Authorization")
	h.Del("Proxy-Authorization")
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
