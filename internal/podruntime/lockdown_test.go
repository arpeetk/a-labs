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

// stubIPv6 fixes the IPv6 inputs (binary presence, stack presence) for a test
// and restores them on cleanup.
func stubIPv6(t *testing.T, bin string, stack bool) {
	t.Helper()
	restoreBin, restoreStack := ip6tablesBinary, ipv6StackPresent
	ip6tablesBinary = func() string { return bin }
	ipv6StackPresent = func() bool { return stack }
	t.Cleanup(func() { ip6tablesBinary, ipv6StackPresent = restoreBin, restoreStack })
}

func okExec(t *testing.T, calls *[][]string) {
	t.Helper()
	restore := lockdownExec
	lockdownExec = func(_ context.Context, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string{name}, args...))
		return nil, nil
	}
	t.Cleanup(func() { lockdownExec = restore })
}

func TestRunLockdownIPv6StackWithoutBinaryFailsClosed(t *testing.T) {
	// A dual-stack pod whose image lacks ip6tables must NOT proceed: the runner
	// could egress over IPv6, and the IPv4-only canary would never notice.
	var calls [][]string
	okExec(t, &calls)
	stubIPv6(t, "", true)

	var out bytes.Buffer
	err := RunLockdown(context.Background(), &out, LockdownConfig{EgressPort: "8099", ProxyUID: "65533", IPv6: true})
	if err == nil {
		t.Fatal("expected fail-closed when IPv6 stack is live but ip6tables is missing")
	}
	if !strings.Contains(out.String(), "IPv6") {
		t.Errorf("expected the failure to mention IPv6, got:\n%s", out.String())
	}
	// The IPv4 lockdown must already be in place (fail-closed ordering), but no
	// further container may start — RunLockdown's error aborts the pod.
	if len(calls) == 0 {
		t.Error("expected IPv4 rules applied before the IPv6 failure")
	}
}

func TestRunLockdownIPv6DisabledWithoutBinarySkips(t *testing.T) {
	// A genuinely v6-disabled netns (no /proc/net/if_inet6) has no v6 route to
	// escape through — the IPv4-only lockdown is complete.
	var calls [][]string
	okExec(t, &calls)
	stubIPv6(t, "", false)

	var out bytes.Buffer
	if err := RunLockdown(context.Background(), &out, LockdownConfig{EgressPort: "8099", ProxyUID: "65533", IPv6: true}); err != nil {
		t.Fatalf("RunLockdown with disabled IPv6 stack: %v", err)
	}
	if !strings.Contains(out.String(), "IPv4-only lockdown") {
		t.Errorf("expected IPv4-only message, got:\n%s", out.String())
	}
}

func TestRunLockdownAppliesIPv6RulesWhenBinaryPresent(t *testing.T) {
	var calls [][]string
	okExec(t, &calls)
	stubIPv6(t, "ip6tables-nft", true)

	var out bytes.Buffer
	if err := RunLockdown(context.Background(), &out, LockdownConfig{EgressPort: "8099", ProxyUID: "65533", IPv6: true}); err != nil {
		t.Fatalf("RunLockdown: %v", err)
	}
	wantV4 := iptablesRules(LockdownConfig{EgressPort: "8099", ProxyUID: "65533"}, rejectIPv4)
	wantV6 := iptablesRules(LockdownConfig{EgressPort: "8099", ProxyUID: "65533"}, rejectIPv6)
	if len(calls) != len(wantV4)+len(wantV6) {
		t.Fatalf("applied %d rules, want %d (v4 %d + v6 %d)", len(calls), len(wantV4)+len(wantV6), len(wantV4), len(wantV6))
	}
	// The v6 REJECT must use the v6 reject type — a rejected rule would leave
	// the v6 chain at default-ACCEPT.
	last := calls[len(calls)-1]
	if !contains(last, "icmp6-port-unreachable") {
		t.Errorf("final v6 rule = %v, want reject-with icmp6-port-unreachable", last)
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
