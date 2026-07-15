package harness

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// blockedProbe simulates enforcement working: both direct probes fail, the proxy
// probe succeeds.
func blockedProbe() canaryProbe {
	return canaryProbe{
		directDial:  func(string) error { return errors.New("connection refused") },
		directHTTPS: func(string) error { return errors.New("connection refused") },
		viaProxy:    func(string) error { return nil },
	}
}

func TestCanarySkippedWhenNotExpected(t *testing.T) {
	var out bytes.Buffer
	em := NewEmitter(&out)
	if err := runCanary(context.Background(), em, false, "http://127.0.0.1:8099", blockedProbe()); err != nil {
		t.Fatalf("canary must be a no-op when enforcement not expected: %v", err)
	}
	if !strings.Contains(out.String(), "skipped") {
		t.Errorf("expected skip message, got: %s", out.String())
	}
}

func TestCanaryPassesWhenEnforced(t *testing.T) {
	var out bytes.Buffer
	em := NewEmitter(&out)
	if err := runCanary(context.Background(), em, true, "http://127.0.0.1:8099", blockedProbe()); err != nil {
		t.Fatalf("canary should pass when direct blocked + proxy works: %v", err)
	}
	if !strings.Contains(out.String(), "PASSED") {
		t.Errorf("expected PASSED, got: %s", out.String())
	}
}

func TestCanaryFailsWhenDirectDialSucceeds(t *testing.T) {
	p := blockedProbe()
	p.directDial = func(string) error { return nil } // BYPASS: raw egress works
	var out bytes.Buffer
	err := runCanary(context.Background(), NewEmitter(&out), true, "http://127.0.0.1:8099", p)
	if err == nil {
		t.Fatal("canary must FAIL when a direct dial succeeds under enforcement")
	}
	if !strings.Contains(out.String(), "bypass") {
		t.Errorf("expected bypass error event, got: %s", out.String())
	}
}

func TestCanaryFailsWhenDirectHTTPSSucceeds(t *testing.T) {
	p := blockedProbe()
	p.directHTTPS = func(string) error { return nil } // BYPASS to an allowed host
	var out bytes.Buffer
	err := runCanary(context.Background(), NewEmitter(&out), true, "http://127.0.0.1:8099", p)
	if err == nil {
		t.Fatal("canary must FAIL when direct HTTPS succeeds under enforcement")
	}
}

func TestCanaryFailsWhenProxyUnusable(t *testing.T) {
	p := blockedProbe()
	p.viaProxy = func(string) error { return errors.New("no route") }
	var out bytes.Buffer
	err := runCanary(context.Background(), NewEmitter(&out), true, "http://127.0.0.1:8099", p)
	if err == nil {
		t.Fatal("canary must FAIL when the sanctioned proxy path is unusable")
	}
}

func TestCanaryFailsWhenProxyUnset(t *testing.T) {
	var out bytes.Buffer
	err := runCanary(context.Background(), NewEmitter(&out), true, "", blockedProbe())
	if err == nil {
		t.Fatal("canary must FAIL when enforcement expected but no proxy configured")
	}
}
