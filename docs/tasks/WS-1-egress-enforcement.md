# WS-1: Egress bypass enforcement

**Branch:** `ws1-egress-enforcement` · **Worktree:** `../wren-ws1` · **Size:** L · **State:** READY (after WS-0 merges)
**Security-critical: a strong-model or human review is mandatory before merge.**

## Context (read first)

- `AGENTS.md` in full.
- `docs/implementation-plan.md` §WS-1 — the authoritative design; implement it,
  don't redesign it.
- `docs/technical-spec.md` §5.6 (security model) and the bypass note at the top
  of `internal/egress/proxy.go`.
- Current pod shape: `internal/controller/pod.go` (`buildAgentPod`).

## Scope

**IN** (all from implementation-plan §WS-1):

1. **UID separation:** explicit `RunAsUser: 65533` on the egress-proxy
   container; runner and other containers stay at the image default (65532).
2. **`egress-lockdown` init container**, first in init order, as a new
   `wren-runtime` role: runs as root with capabilities add `NET_ADMIN`,`NET_RAW`
   (drop ALL others), applies the iptables OUTPUT rules from the plan
   (accept lo→proxy-port; accept uid 65533; reject everything else, DNS
   included), then exits. Add a static iptables binary to
   `build/Dockerfile.runtime`; if distroless friction proves too high, a
   separate minimal `Dockerfile.lockdown` is acceptable — document the choice
   in the hand-off.
3. **Operator flag** `--egress-enforcement=iptables|off` (default `iptables`);
   `off` omits the lockdown container and writes an
   `EgressEnforcement=Disabled` condition on each run.
4. **Proxy tightening** in `internal/egress`: CONNECT restricted to port 443;
   strip hop-by-hop headers + incoming `Authorization`/`Proxy-Authorization`
   on the forward path; dial/read/idle timeouts on the forward transport.
5. **Canary in the mock harness** (`internal/harness/mock.go`): attempt a
   direct dial to `1.1.1.1:443` and a direct HTTPS request to `github.com` —
   both MUST fail; then a request via `WREN_EGRESS_PROXY` MUST succeed. Emit
   the results as events; the harness exits non-zero if enforcement is on and
   the direct path worked. Gate on an env flag so the canary only runs when
   enforcement is expected (`WREN_EXPECT_ENFORCEMENT=1`, set by the operator
   when enforcement is on).
6. **Manifests:** optional default-deny egress NetworkPolicy example under
   `config/netpol/` (not applied by default) + doc comments.
7. Unit tests: pod-builder assertions (lockdown container present/absent per
   flag, UIDs, caps); proxy tests for CONNECT-port restriction and header
   scrubbing.
8. Update spec living-status + README status table: egress row goes from
   "cooperative" to "enforced (iptables uid-match; `off` escape hatch)".

**OUT:** GitHub App work (WS-2 — do not add spec fields); NetworkPolicy
FQDN/Cilium variants beyond the example; gVisor/Kata anything; kind-level
node firewalling.

## Hot files

You own: `internal/controller/pod.go` (+tests), `internal/egress/*`,
`internal/podruntime/*` (new role), `internal/harness/mock.go`,
`build/Dockerfile.runtime`, `cmd/wren-operator/main.go` (flag),
`config/netpol/` (new).
Do NOT touch: `api/v1alpha1/*` (no CRD changes in this WS),
`cmd/wren-apiserver/*`, `internal/store/*`, `internal/{coreapi,launcher}/*`,
`Makefile` (append-only if you must add a target).

## Definition of done

- [ ] `make test vet` green; new tests cover the flag matrix.
- [ ] `make e2e` green with enforcement ON and the canary proving direct
      egress is blocked (this is the acceptance test).
- [ ] `make e2e` green with `--egress-enforcement=off` (canary skipped).
- [ ] Kind caveat checked: confirm the lockdown container is admitted on kind
      (privileged init containers are allowed there); note any GKE
      Autopilot/PSA caveats in the hand-off, do not attempt GKE.
- [ ] Spec + README rows updated in this branch.
- [ ] Hand-off note, explicitly listing what was NOT verified (e.g. GKE).
