package podruntime

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

func TestIptablesRulesOrderAndContent(t *testing.T) {
	cfg := LockdownConfig{EgressPort: "8099", ProxyUID: "65533"}
	rules := iptablesRules(cfg, rejectIPv4)

	// The final rule must be the default REJECT — ACCEPTs precede it.
	last := rules[len(rules)-1]
	if !contains(last, "REJECT") {
		t.Fatalf("last rule must be REJECT, got %v", last)
	}
	for _, r := range rules[:len(rules)-1] {
		if !contains(r, "ACCEPT") {
			t.Errorf("non-final rule must ACCEPT, got %v", r)
		}
	}

	joined := joinRules(rules)
	// Runner reaches the proxy port on loopback.
	if !strings.Contains(joined, "-o lo -p tcp --dport 8099 -j ACCEPT") {
		t.Errorf("missing loopback→proxy-port accept:\n%s", joined)
	}
	// Proxy uid can egress.
	if !strings.Contains(joined, "--uid-owner 65533 -j ACCEPT") {
		t.Errorf("missing proxy-uid accept:\n%s", joined)
	}
	// Default reject present (covers DNS for the runner).
	if !strings.Contains(joined, "OUTPUT -j REJECT") {
		t.Errorf("missing default OUTPUT reject:\n%s", joined)
	}
}

func TestRunLockdownAppliesRulesInOrder(t *testing.T) {
	var calls [][]string
	restore := lockdownExec
	lockdownExec = func(_ context.Context, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return nil, nil
	}
	defer func() { lockdownExec = restore }()

	var out bytes.Buffer
	// IPv6 off so the test is deterministic regardless of host ip6tables.
	if err := RunLockdown(context.Background(), &out, LockdownConfig{EgressPort: "8099", ProxyUID: "65533"}); err != nil {
		t.Fatalf("RunLockdown: %v", err)
	}
	want := iptablesRules(LockdownConfig{EgressPort: "8099", ProxyUID: "65533"}, rejectIPv4)
	if len(calls) != len(want) {
		t.Fatalf("applied %d rules, want %d", len(calls), len(want))
	}
	// The last applied rule must be the REJECT (fail-closed default is installed
	// after every ACCEPT).
	if !contains(calls[len(calls)-1], "REJECT") {
		t.Errorf("final applied rule = %v, want REJECT", calls[len(calls)-1])
	}
}

func TestRunLockdownFailsClosedOnExecError(t *testing.T) {
	restore := lockdownExec
	lockdownExec = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("permission denied"), errors.New("exit 1")
	}
	defer func() { lockdownExec = restore }()

	var out bytes.Buffer
	err := RunLockdown(context.Background(), &out, LockdownConfig{EgressPort: "8099", ProxyUID: "65533"})
	if err == nil {
		t.Fatal("expected RunLockdown to fail closed when iptables errors")
	}
}

func TestDefaultLockdownConfigDefaults(t *testing.T) {
	t.Setenv(envEgressPort, "")
	t.Setenv(envProxyUID, "")
	cfg := DefaultLockdownConfig()
	if cfg.EgressPort != "8099" {
		t.Errorf("default port = %q, want 8099", cfg.EgressPort)
	}
	if cfg.ProxyUID != "65533" {
		t.Errorf("default uid = %q, want 65533", cfg.ProxyUID)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func joinRules(rules [][]string) string {
	var b strings.Builder
	for _, r := range rules {
		b.WriteString(strings.Join(r, " "))
		b.WriteString("\n")
	}
	return b.String()
}
