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

### 1. Egress is proxy-based but **not yet hard-enforced**

Today the egress proxy (`internal/egress`) enforces a destination allowlist and
injects credentials, so **the runner never holds a long-lived token** — that
part of the design is real. However, the redirect through the proxy is currently
**cooperative**: a hostile runner that ignores the configured proxy can attempt
to reach the network directly. Hard enforcement — uid-based iptables redirection
(Istio-style) or a dedicated egress pod plus a default-deny `NetworkPolicy` — is
**not yet in place**.

**Implication:** until enforcement lands, treat the egress allowlist as a strong
default and a credential-isolation mechanism, **not** as a containment boundary
against a fully adversarial runner. Do not rely on it alone to prevent
exfiltration by malicious model output on untrusted repos.

> **Note for maintainers / orchestrator:** this statement reflects the state of
> this branch's base. Workstream **WS-1** (egress enforcement — iptables
> uid-match / NetworkPolicy) is being built in parallel and had **not merged**
> when this file was written. **When WS-1 lands, update this section:** egress
> becomes hard-enforced and this residual risk downgrades from "cooperative /
> bypassable" to "enforced at the pod network boundary." Re-verify the wording
> against `internal/egress` and the pod builder at that time.

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

- **Store** is in-memory (target: Postgres) — no durability or access controls
  on persisted state yet.
- **Checkpointer / gateway sidecars** are liveness stand-ins, not the real
  implementations.

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
