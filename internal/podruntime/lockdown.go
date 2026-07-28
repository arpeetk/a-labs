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
	envEgressPort   = "WREN_EGRESS_PORT"       // proxy's localhost port (accept lo→port)
	envProxyUID     = "WREN_PROXY_UID"         // uid the egress-proxy runs as (accept uid-owner)
	envGCSFuseUID   = "WREN_GCSFUSE_UID"       // uid the GKE gcs-fuse sidecar runs as ("" = no exemption)
	envGCSFuseCIDRs = "WREN_GCSFUSE_DST_CIDRS" // comma-separated IPv4 CIDRs that uid may reach
)

// LockdownConfig is the resolved input for the iptables program.
type LockdownConfig struct {
	EgressPort string // e.g. "8099"
	ProxyUID   string // e.g. "65533"
	IPv6       bool   // also lock down ip6tables if the stack is present

	// GCS-FUSE checkpoint mount exemption (WS-19). When a run mounts a GCS
	// bucket, the GKE-injected gke-gcsfuse-sidecar runs as GCSFuseUID (its own
	// uid, distinct from the runner and proxy) and must reach Cloud Storage + the
	// GCE metadata server directly — it cannot be routed through the egress-proxy
	// because the injected sidecar's env/args are not user-configurable. The hole
	// is narrowed to BOTH that uid AND the fixed GCSFuseCIDRs (the restricted
	// Google APIs VIP that storage.googleapis.com is pinned to, plus the metadata
	// server). Empty GCSFuseUID means no exemption — the default lockdown.
	GCSFuseUID   string   // e.g. "65534"; "" disables the exemption
	GCSFuseCIDRs []string // IPv4 destination CIDRs the sidecar uid may reach
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
	return LockdownConfig{
		EgressPort:   port,
		ProxyUID:     uid,
		IPv6:         true,
		GCSFuseUID:   os.Getenv(envGCSFuseUID),
		GCSFuseCIDRs: splitCIDRs(os.Getenv(envGCSFuseCIDRs)),
	}
}

// splitCIDRs parses the comma-separated GCS-FUSE destination CIDR list, dropping
// blanks so a trailing comma or an unset value yields no rules.
func splitCIDRs(s string) []string {
	var out []string
	for _, c := range strings.Split(s, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// iptablesRules returns the OUTPUT chain rules, in application order, for the
// given config. Ordering is load-bearing: ACCEPT rules must precede the final
// REJECT. Each entry is the argument list appended after the binary name.
//
//  1. loopback to the proxy port          → ACCEPT (runner reaches the proxy)
//  2. loopback generally                  → ACCEPT (kubelet probes, ipc)
//  3. established/related                 → ACCEPT (return traffic for the above)
//  4. owner uid == proxy uid              → ACCEPT (the proxy reaches the world)
//  5. owner uid == gcs-fuse uid, dst ∈ … → ACCEPT (the mount reaches Storage +
//     metadata, and nothing else — WS-19; only when GCSFuseUID is set)
//  6. everything else (DNS included)      → REJECT (runner resolves/reaches nothing)
//
// Rule 4a is the WS-19 GCS-FUSE exemption: present only when a run mounts a GCS
// bucket (GCSFuseUID set). It is scoped by BOTH the sidecar's uid AND a fixed
// destination set — the runner uid can never match it, so the harness cannot
// reach these destinations even though the rule exists. It is IPv4-only: both
// destinations (the restricted Google APIs VIP and the metadata server) are IPv4
// literals, so it is never emitted into the ip6tables chain (a v6 -d with a v4
// CIDR would be rejected, leaving the v6 chain unprogrammed).
//
// rejectWith differs by family: IPv4 uses icmp-port-unreachable, IPv6 uses
// icmp6-port-unreachable. Passing the right one matters — a rejected REJECT rule
// would leave that family's OUTPUT at its default-ACCEPT policy (a bypass).
func iptablesRules(cfg LockdownConfig, rejectWith string) [][]string {
	rules := [][]string{
		{"-A", "OUTPUT", "-o", "lo", "-p", "tcp", "--dport", cfg.EgressPort, "-j", "ACCEPT"},
		{"-A", "OUTPUT", "-o", "lo", "-j", "ACCEPT"},
		{"-A", "OUTPUT", "-m", "conntrack", "--ctstate", "ESTABLISHED,RELATED", "-j", "ACCEPT"},
		{"-A", "OUTPUT", "-m", "owner", "--uid-owner", cfg.ProxyUID, "-j", "ACCEPT"},
	}
	if rejectWith == rejectIPv4 && cfg.GCSFuseUID != "" {
		for _, cidr := range cfg.GCSFuseCIDRs {
			rules = append(rules, []string{"-A", "OUTPUT",
				"-m", "owner", "--uid-owner", cfg.GCSFuseUID,
				"-d", cidr, "-j", "ACCEPT"})
		}
	}
	return append(rules, []string{"-A", "OUTPUT", "-j", "REJECT", "--reject-with", rejectWith})
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
	// IPv6 default route to escape. A rule-application failure fails closed (a
	// half-applied chain left at default-ACCEPT is an exfil path) — and so does
	// a live stack with no binary to program it: skipping then would leave the
	// runner a wide-open v6 route the IPv4 rules say nothing about, and the
	// (IPv4-only) canary cannot catch it. Only a genuinely disabled IPv6 stack
	// skips the lockdown.
	if cfg.IPv6 {
		if bin := ip6tablesBinary(); bin != "" {
			if err := applyRules(ctx, em, bin, iptablesRules(cfg, rejectIPv6)); err != nil {
				em.Errorf("egress-lockdown: ip6tables: " + err.Error())
				return fmt.Errorf("egress-lockdown ip6tables: %w", err)
			}
			em.Message("egress-lockdown: ip6tables OUTPUT default-reject installed")
		} else if ipv6StackPresent() {
			err := fmt.Errorf("egress-lockdown: IPv6 stack present but no ip6tables binary found; refusing to leave IPv6 egress open")
			em.Errorf(err.Error())
			return err
		} else {
			em.Message("egress-lockdown: IPv6 stack disabled; IPv4-only lockdown")
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

// ip6tablesBinary returns the IPv6 binary if present, else "". A package var
// so tests can simulate a host without one.
var ip6tablesBinary = func() string {
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

// ipv6StackPresent reports whether the pod's network namespace has a live IPv6
// stack. The kernel creates /proc/net/if_inet6 when the stack registers (it
// lists at least ::1) and omits it when IPv6 is disabled — e.g. via
// ipv6.disable=1. A package var so tests can simulate dual-stack hosts.
var ipv6StackPresent = func() bool {
	_, err := os.Stat("/proc/net/if_inet6")
	return err == nil
}
