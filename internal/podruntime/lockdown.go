package podruntime

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/summiteight/wren/internal/harness"
)

// egress-lockdown role (WS-1, spec §5.6).
//
// This init container runs FIRST in the pod, as root with only NET_ADMIN +
// NET_RAW, and installs iptables OUTPUT rules that make the runner physically
// unable to reach the network except through the in-pod egress-proxy. The
// containers share a network namespace, so a uid-owner match is what
// distinguishes the trusted proxy (uid 65533) from the untrusted runner
// (uid 65532). This is the Istio-proven pattern; NetworkPolicy alone cannot
// tell two containers of the same pod apart. After applying the rules the
// container exits 0 — the rules persist in the shared netns for the pod's life.

// Env the operator sets for the lockdown role.
const (
	envEgressPort = "WREN_EGRESS_PORT" // proxy's localhost port (accept lo→port)
	envProxyUID   = "WREN_PROXY_UID"   // uid the egress-proxy runs as (accept uid-owner)
)

// LockdownConfig is the resolved input for the iptables program.
type LockdownConfig struct {
	EgressPort string // e.g. "8099"
	ProxyUID   string // e.g. "65533"
	IPv6       bool   // also lock down ip6tables if the stack is present
}

// DefaultLockdownConfig reads the config from the environment the operator sets.
func DefaultLockdownConfig() LockdownConfig {
	port := os.Getenv(envEgressPort)
	if port == "" {
		port = "8099" // egress.DefaultPort; kept literal to avoid import churn
	}
	uid := os.Getenv(envProxyUID)
	if uid == "" {
		uid = "65533"
	}
	return LockdownConfig{EgressPort: port, ProxyUID: uid, IPv6: true}
}

// iptablesRules returns the OUTPUT chain rules, in application order, for the
// given config. Ordering is load-bearing: ACCEPT rules must precede the final
// REJECT. Each entry is the argument list appended after the binary name.
//
//  1. loopback to the proxy port      → ACCEPT (runner reaches the proxy)
//  2. loopback generally              → ACCEPT (kubelet probes, ipc)
//  3. established/related             → ACCEPT (return traffic for the above)
//  4. owner uid == proxy uid          → ACCEPT (the proxy reaches the world)
//  5. everything else (DNS included)  → REJECT (runner resolves/reaches nothing)
//
// rejectWith differs by family: IPv4 uses icmp-port-unreachable, IPv6 uses
// icmp6-port-unreachable. Passing the right one matters — a rejected REJECT rule
// would leave that family's OUTPUT at its default-ACCEPT policy (a bypass).
func iptablesRules(cfg LockdownConfig, rejectWith string) [][]string {
	return [][]string{
		{"-A", "OUTPUT", "-o", "lo", "-p", "tcp", "--dport", cfg.EgressPort, "-j", "ACCEPT"},
		{"-A", "OUTPUT", "-o", "lo", "-j", "ACCEPT"},
		{"-A", "OUTPUT", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"-A", "OUTPUT", "-m", "owner", "--uid-owner", cfg.ProxyUID, "-j", "ACCEPT"},
		{"-A", "OUTPUT", "-j", "REJECT", "--reject-with", rejectWith},
	}
}

const (
	rejectIPv4 = "icmp-port-unreachable"
	rejectIPv6 = "icmp6-port-unreachable"
)

// runner runs one command; a package var so tests can capture the invocations
// without touching the host's iptables.
var lockdownExec = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// RunLockdown applies the egress iptables rules and exits. It is idempotent in
// spirit (a fresh pod netns each run) and fails closed: any error applying a
// rule aborts the pod so a run never proceeds with the network wide open.
func RunLockdown(ctx context.Context, out io.Writer, cfg LockdownConfig) error {
	em := harness.NewEmitter(out)
	em.Message(fmt.Sprintf("egress-lockdown: applying iptables (proxy port=%s uid=%s)", cfg.EgressPort, cfg.ProxyUID))

	if err := applyRules(ctx, em, iptablesBinary(), iptablesRules(cfg, rejectIPv4)); err != nil {
		em.Errorf("egress-lockdown: " + err.Error())
		return err
	}
	// IPv6: if the stack is present, lock it down too so the runner cannot use an
	// IPv6 default route to escape. A missing ip6tables binary is not fatal
	// (IPv4-only nodes, e.g. many kind setups) — but if the binary IS present and
	// applying the rules fails, we fail closed: a half-applied IPv6 chain that
	// left the default at ACCEPT would be an exfil path.
	if cfg.IPv6 {
		if bin := ip6tablesBinary(); bin != "" {
			if err := applyRules(ctx, em, bin, iptablesRules(cfg, rejectIPv6)); err != nil {
				em.Errorf("egress-lockdown: ip6tables: " + err.Error())
				return fmt.Errorf("egress-lockdown ip6tables: %w", err)
			}
			em.Message("egress-lockdown: ip6tables OUTPUT default-reject installed")
		} else {
			em.Message("egress-lockdown: no ip6tables binary found; IPv4-only lockdown")
		}
	}
	em.Message("egress-lockdown: OUTPUT default-reject installed; runner egress restricted to the proxy")
	return nil
}

func applyRules(ctx context.Context, em *harness.Emitter, bin string, rules [][]string) error {
	for _, args := range rules {
		if out, err := lockdownExec(ctx, bin, args...); err != nil {
			return fmt.Errorf("%s %s: %w (%s)", bin, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
	}
	return nil
}

// iptablesBinary is the IPv4 iptables binary. We prefer the nft backend (the
// runtime image ships xtables-nft-multi); an override lets an image with a
// different path work without a rebuild.
func iptablesBinary() string {
	if b := os.Getenv("WREN_IPTABLES_BIN"); b != "" {
		return b
	}
	for _, cand := range []string{"iptables-nft", "iptables"} {
		if p, err := exec.LookPath(cand); err == nil {
			return p
		}
	}
	return "iptables-nft" // let exec surface a clear not-found error
}

// ip6tablesBinary returns the IPv6 binary if present, else "".
func ip6tablesBinary() string {
	if b := os.Getenv("WREN_IP6TABLES_BIN"); b != "" {
		return b
	}
	for _, cand := range []string{"ip6tables-nft", "ip6tables"} {
		if p, err := exec.LookPath(cand); err == nil {
			return p
		}
	}
	return ""
}
