package harness

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Egress canary (WS-1, spec §5.6).
//
// When the operator runs with egress enforcement on it sets
// WREN_EXPECT_ENFORCEMENT=1 in the harness env. podruntime.RunHarness runs the
// canary before invoking ANY harness adapter (mock, claude-code, byo), proving
// from inside the untrusted runner — before tokens are spent — that the
// iptables lockdown actually holds:
//
//  1. a direct TCP dial to 1.1.1.1:443 MUST fail (no raw egress),
//  2. a direct HTTPS GET to github.com MUST fail (no bypass to an allowed host),
//  3. a request via WREN_EGRESS_PROXY MUST succeed (the sanctioned path works).
//
// If enforcement is expected but any direct probe SUCCEEDS, the runner can
// bypass the proxy — a security failure — and the canary returns an error so the
// harness exits non-zero (failing `make e2e`, the acceptance test). When
// WREN_EXPECT_ENFORCEMENT is unset the canary is skipped entirely.

const (
	canaryDialTarget  = "1.1.1.1:443"
	canaryHTTPSTarget = "https://github.com/"
	canaryTimeout     = 4 * time.Second
)

// canaryProbe abstracts the three probes so tests can inject deterministic
// results without real network access.
type canaryProbe struct {
	// directDial reports the error from a direct TCP dial to addr (nil = it
	// connected, which under enforcement is a bypass).
	directDial func(addr string) error
	// directHTTPS reports the error from a direct HTTPS GET to url.
	directHTTPS func(url string) error
	// viaProxy reports the error from a request through the egress-proxy base URL.
	viaProxy func(base string) error
}

// defaultCanaryProbe wires the real network probes.
func defaultCanaryProbe() canaryProbe {
	return canaryProbe{
		directDial: func(addr string) error {
			c, err := net.DialTimeout("tcp", addr, canaryTimeout)
			if err == nil {
				_ = c.Close()
			}
			return err
		},
		directHTTPS: func(target string) error {
			// A dedicated transport with NO proxy: this must hit the network
			// directly and be rejected by iptables.
			tr := &http.Transport{
				Proxy:               nil,
				DialContext:         (&net.Dialer{Timeout: canaryTimeout}).DialContext,
				TLSClientConfig:     &tls.Config{},
				TLSHandshakeTimeout: canaryTimeout,
			}
			cl := &http.Client{Transport: tr, Timeout: canaryTimeout}
			resp, err := cl.Get(target)
			if err == nil {
				_ = resp.Body.Close()
			}
			return err
		},
		viaProxy: func(base string) error {
			base = strings.TrimRight(base, "/")
			// The proxy exposes a github-api route; a GET to it exercises the
			// sanctioned egress path (creds injected proxy-side). We only need the
			// request to reach the proxy and get a response, not a 2xx.
			cl := &http.Client{Timeout: canaryTimeout}
			resp, err := cl.Get(base + "/github-api/")
			if err == nil {
				_ = resp.Body.Close()
			}
			return err
		},
	}
}

// RunCanary runs the egress canary if WREN_EXPECT_ENFORCEMENT is set, emitting
// each probe's result as an event. It returns an error (failing the run) if a
// direct probe succeeds under enforcement, or if the proxy path is unusable.
func RunCanary(ctx context.Context, em *Emitter) error {
	return runCanary(ctx, em, os.Getenv("WREN_EXPECT_ENFORCEMENT") == "1",
		strings.TrimRight(os.Getenv("WREN_EGRESS_PROXY"), "/"), defaultCanaryProbe())
}

func runCanary(ctx context.Context, em *Emitter, expectEnforcement bool, proxyBase string, p canaryProbe) error {
	if !expectEnforcement {
		em.Message("egress canary: skipped (enforcement not expected)")
		return nil
	}
	em.Message("egress canary: enforcement expected — verifying the runner cannot bypass the proxy")

	// Probe 1: raw dial must be blocked.
	if err := p.directDial(canaryDialTarget); err == nil {
		em.Errorf("egress canary: direct dial to " + canaryDialTarget + " SUCCEEDED — proxy bypass is possible")
		return fmt.Errorf("egress canary: direct dial to %s succeeded under enforcement (bypass)", canaryDialTarget)
	}
	em.Message("egress canary: direct dial to " + canaryDialTarget + " blocked (good)")

	// Probe 2: direct HTTPS to an allowed host must be blocked (it must go
	// through the proxy, not around it).
	if err := p.directHTTPS(canaryHTTPSTarget); err == nil {
		em.Errorf("egress canary: direct HTTPS to " + canaryHTTPSTarget + " SUCCEEDED — proxy bypass is possible")
		return fmt.Errorf("egress canary: direct HTTPS to %s succeeded under enforcement (bypass)", canaryHTTPSTarget)
	}
	em.Message("egress canary: direct HTTPS to " + canaryHTTPSTarget + " blocked (good)")

	// Probe 3: the sanctioned path through the proxy must work.
	if proxyBase == "" {
		em.Errorf("egress canary: WREN_EGRESS_PROXY unset but enforcement expected")
		return fmt.Errorf("egress canary: enforcement expected but no egress-proxy configured")
	}
	if _, err := url.Parse(proxyBase); err != nil {
		return fmt.Errorf("egress canary: bad proxy base %q: %w", proxyBase, err)
	}
	if err := p.viaProxy(proxyBase); err != nil {
		em.Errorf("egress canary: request via proxy failed: " + err.Error())
		return fmt.Errorf("egress canary: proxy path unusable: %w", err)
	}
	em.Message("egress canary: request via egress-proxy succeeded (good)")
	em.Message("egress canary: PASSED — direct egress blocked, proxy path works")
	return nil
}
