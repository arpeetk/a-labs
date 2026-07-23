# Security Policy

Wren runs **untrusted, model-generated code** in the cloud. Security is the
core design constraint, not an add-on. This document summarizes the threat
model, states the *honest* residual risks as they stand today, and explains how
to report a vulnerability privately.

The authoritative design lives in the technical spec (§9, Threat model). This
file is the user-facing summary — where it and the spec disagree, the spec is
canonical.

---

## Reporting a vulnerability

**Please do not open public GitHub issues for security vulnerabilities.**

Report privately by one of:

- **GitHub Security Advisories** — use the repository's *Security → Report a
  vulnerability* button (preferred; keeps the report and fix coordination in one
  place), or
- **Email** — **arpeetkale@gmail.com** with a subject beginning `[SECURITY]`.

Please include: a description, affected component/version or commit, reproduction
steps or a proof of concept, and impact. We aim to acknowledge within **72
hours** and to agree on a disclosure timeline with you. We support coordinated
disclosure and will credit reporters who wish to be named.

This is a pre-1.0 project maintained on a best-effort basis; there is not yet a
formal supported-version matrix. Until a release is tagged, fixes land on `main`.

---

## Threat model (summary)

The runner is untrusted. The trust boundary is the **agent pod**: everything
inside it (the harness, the model, any code it generates or fetches) is assumed
hostile. The controls below aim to bound the blast radius of a fully
compromised runner.

| Threat | Vector | Mitigation |
|---|---|---|
| Untrusted code escapes the sandbox | Model-generated shell / exploit | Hardened pod: non-root, read-only rootfs, dropped capabilities, seccomp, no service-account token. Intended production posture adds an isolated agent-only node pool with minimal node IAM and node-level default-deny egress. Kernel isolation (gVisor/Kata via `RuntimeClass`) is deferred (see residual risks). |
| Credential theft / exfiltration | Agent reads secrets, phones home | The runner holds **no long-lived secrets**. Credentials (GitHub, Anthropic) are injected by an egress **proxy** that also enforces a destination allowlist; per-run least-privilege identity. (See residual risks for the current enforcement gap.) |
| Prompt injection (repo / MCP content) | Hijacked agent takes actions | Bounded blast radius: destination allowlist + scoped, short-lived tokens + **PR-only** output. The agent cannot merge or deploy. |
| Lateral movement between runs / users | Shared namespace or volume | Namespace-per-user, per-run PVC / config / identity, NetworkPolicy (intended posture). |
| Supply-chain (base / harness images) | Poisoned image | Signed images, pinned digests, provenance checks (target posture; image scanning ships with the release pipeline). |
| Over-spend | Runaway token usage | Budgets, hard caps with pause, quotas (roadmap). |
| Insider misuse | Unauthorized runs / config | RBAC + audit log (roadmap; see the auth residual risk below). |

---

## Residual risks — the honest list

These are known gaps in the **current** state of the codebase. They are stated
plainly on purpose; deploy accordingly.

### 1. Egress enforcement is on by default — know when you've turned it off

Egress is **hard-enforced by default**: an `egress-lockdown` init container
installs iptables OUTPUT rules (IPv4 + IPv6) in the pod's network namespace so
the untrusted runner (uid 65532, all capabilities dropped, no privilege
escalation) can reach **nothing but the in-pod egress-proxy**; only the proxy
(uid 65533) can reach the network, and it enforces the destination allowlist
and injects credentials. Every run under enforcement executes a startup
**canary** that fails the run if a direct connection ever succeeds, and carries
an `EgressEnforcement=True` condition.

Two caveats:

- **`--egress-enforcement=off` removes the containment boundary.** The lockdown
  container needs root + `NET_ADMIN`/`NET_RAW`, which some platforms refuse
  (GKE Autopilot; namespaces under a `restricted` Pod Security Admission
  profile). In `off` mode the runner routes through the proxy *cooperatively*
  and **can bypass it**; runs carry an explicit `EgressEnforcement=Disabled`
  condition. Treat the allowlist as credential isolation only, not containment,
  and pair `off` with the policies in `config/netpol/` (Cilium/FQDN-capable CNI
  recommended).
- **Inbound connections are not restricted by the lockdown** (its rules cover
  OUTPUT; replies to established flows are allowed). Agent pods accept no
  traffic by design — apply `config/netpol/default-deny-ingress.yaml` per run
  namespace so no other workload can open a channel into a runner.

### 2. Control-plane auth is a header — do not expose it to the internet

Authentication to the control-plane API is currently the `X-Wren-User` **HTTP
header** (an M0 stand-in for OIDC/SSO). There is **no cryptographic
authentication** of the caller.

**Implication:** the apiserver **must not** be exposed to untrusted networks or
the public internet. Run it inside a trusted network boundary (VPC / private
cluster) with network-level access controls in front of it. OIDC at the
apiserver is the gate to any multi-user production deployment.

### 3. Container isolation is `runc`, not a stronger sandbox

Pods run under the default `runc` runtime. Hardening (non-root, read-only
rootfs, dropped caps, seccomp, no SA token) raises the bar, but `runc` shares
the host kernel — a kernel-level container escape is not fully mitigated.
Stronger kernel isolation (gVisor / Kata via `RuntimeClass`) is a **deferred
milestone**, designed to drop in without other component changes.

**Implication:** for higher-assurance deployments, run agent pods on a dedicated,
isolated node pool with minimal node IAM, and plan for the gVisor/Kata rollout
before treating the sandbox as escape-resistant against kernel exploits.

### 4. Other M0 stand-ins with security relevance

- **Store** defaults to in-memory (dev); a Postgres impl exists
  (`--store=postgres` + `DATABASE_URL`), with managed Cloud SQL provisioning
  still to come (WS-5). With the memory store, project config and
  completed-run history are lost on apiserver restart (in-flight runs are
  re-learned from their AgentRun CRs on boot); the Postgres option is a single
  self-managed instance — treat control-plane state as not yet
  production-grade.
- **Checkpointer / gateway sidecars** are liveness stand-ins, not the real
  implementations. The checkpointer is experimental: v0.1 takes no snapshots,
  so a crash that destroys the workspace PVC (node/zone loss without a
  surviving disk) loses the in-flight workspace and the run ends `Failed`
  with diagnostics (spec §5.5).

See `AGENTS.md` §8 for the full, current list of stand-ins; when one becomes
real, its note there and here should be removed.

---

## Hardening you should apply when operating Wren

- Keep the apiserver off the public internet (residual risk #2).
- Run agent pods on an isolated node pool with minimal node IAM.
- Apply a default-deny `NetworkPolicy` at the namespace level as defense in
  depth, independent of the egress-proxy enforcement work.
- Scope GitHub and model credentials to the least privilege the workflow needs;
  the PR-only output model assumes the token cannot merge or deploy.
- Treat every repository and MCP source an agent reads as potentially
  adversarial content (prompt injection).
