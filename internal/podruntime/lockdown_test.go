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

// gcsFuseTestCfg is a lockdown config with the WS-19 GCS-FUSE exemption active:
// the sidecar uid (65534) may reach the restricted Google APIs VIP and the
// metadata server, and nothing else.
func gcsFuseTestCfg() LockdownConfig {
	return LockdownConfig{
		EgressPort:   "8099",
		ProxyUID:     "65533",
		GCSFuseUID:   "65534",
		GCSFuseCIDRs: []string{"199.36.153.4/30", "169.254.169.254/32"},
	}
}

// TestGCSFuseExemptionScopedByUIDAndDest is the load-bearing WS-19 security test
// (brief outcome B): the new exemption must let ONLY the gcs-fuse sidecar uid
// reach ONLY the two fixed destinations, and must NOT give the untrusted runner
// (uid 65532, pinned in the pod spec by hardened()) any way to reach them. If
// this ever regresses, the harness could exfiltrate to Cloud Storage.
func TestGCSFuseExemptionScopedByUIDAndDest(t *testing.T) {
	const runnerUID = "65532" // pinned in internal/controller/pod.go hardened()
	cfg := gcsFuseTestCfg()
	rules := iptablesRules(cfg, rejectIPv4)

	// Every exemption CIDR gets exactly one ACCEPT, uid-scoped to the sidecar AND
	// destination-scoped to that CIDR — never a destination-only or runner-uid
	// ACCEPT.
	for _, cidr := range cfg.GCSFuseCIDRs {
		found := 0
		for _, r := range rules {
			j := strings.Join(r, " ")
			if !strings.Contains(j, "-d "+cidr) {
				continue
			}
			if !contains(r, "ACCEPT") {
				continue
			}
			found++
			if !strings.Contains(j, "--uid-owner "+cfg.GCSFuseUID) {
				t.Errorf("ACCEPT to %s is not uid-scoped to the gcs-fuse uid: %s", cidr, j)
			}
		}
		if found != 1 {
			t.Errorf("want exactly one ACCEPT for %s, got %d", cidr, found)
		}
	}

	// The runner uid must have NO ACCEPT rule whatsoever — its only fate is the
	// final default REJECT, so it reaches none of the exempted destinations.
	for _, r := range rules {
		j := strings.Join(r, " ")
		if strings.Contains(j, "--uid-owner "+runnerUID) {
			t.Errorf("runner uid %s appears in an OUTPUT rule — it must never be exempted: %s", runnerUID, j)
		}
	}
	if last := rules[len(rules)-1]; !contains(last, "REJECT") {
		t.Errorf("final rule must be REJECT (default-deny for the runner), got %v", last)
	}
	// Ordering: every exemption ACCEPT precedes the REJECT.
	rejectIdx := len(rules) - 1
	for i, r := range rules {
		if strings.Contains(strings.Join(r, " "), "--uid-owner "+cfg.GCSFuseUID) && i >= rejectIdx {
			t.Errorf("exemption at index %d is not before the REJECT at %d", i, rejectIdx)
		}
	}
}

// TestGCSFuseExemptionIPv4Only: the exemption destinations are IPv4 literals, so
// they must never be emitted into the ip6tables chain — a v6 rule with a v4 -d
// would be rejected and leave the v6 OUTPUT chain unprogrammed (an escape).
func TestGCSFuseExemptionIPv4Only(t *testing.T) {
	cfg := gcsFuseTestCfg()
	v6 := iptablesRules(cfg, rejectIPv6)
	for _, r := range v6 {
		j := strings.Join(r, " ")
		if strings.Contains(j, "199.36.153.4") || strings.Contains(j, "169.254.169.254") {
			t.Errorf("IPv4 exemption CIDR leaked into the ip6tables chain: %s", j)
		}
	}
	// And the v6 chain still ends in a REJECT.
	if last := v6[len(v6)-1]; !contains(last, "REJECT") {
		t.Errorf("v6 final rule must be REJECT, got %v", last)
	}
}

// TestNoGCSFuseExemptionWhenUnset: with no sidecar uid configured (the common
// case — no GCS mount), the rule set is exactly the original WS-1 lockdown.
func TestNoGCSFuseExemptionWhenUnset(t *testing.T) {
	base := LockdownConfig{EgressPort: "8099", ProxyUID: "65533"}
	rules := iptablesRules(base, rejectIPv4)
	if len(rules) != 5 {
		t.Fatalf("unconfigured lockdown should have 5 rules, got %d", len(rules))
	}
	for _, r := range rules {
		if strings.Contains(strings.Join(r, " "), "-d ") {
			t.Errorf("unexpected destination-scoped rule with no GCS mount: %v", r)
		}
	}
}

func TestDefaultLockdownConfigGCSFuse(t *testing.T) {
	t.Setenv(envGCSFuseUID, "65534")
	t.Setenv(envGCSFuseCIDRs, "199.36.153.4/30, 169.254.169.254/32,")
	cfg := DefaultLockdownConfig()
	if cfg.GCSFuseUID != "65534" {
		t.Errorf("GCSFuseUID = %q, want 65534", cfg.GCSFuseUID)
	}
	// Blanks and surrounding whitespace are dropped.
	want := []string{"199.36.153.4/30", "169.254.169.254/32"}
	if len(cfg.GCSFuseCIDRs) != len(want) {
		t.Fatalf("GCSFuseCIDRs = %v, want %v", cfg.GCSFuseCIDRs, want)
	}
	for i, w := range want {
		if cfg.GCSFuseCIDRs[i] != w {
			t.Errorf("GCSFuseCIDRs[%d] = %q, want %q", i, cfg.GCSFuseCIDRs[i], w)
		}
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
