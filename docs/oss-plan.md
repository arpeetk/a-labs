# Wren — OSS Launch Plan

> **Update (2026-07-23):** the owner reprioritized — the main track is now
> **seamless onboarding** (WS-13: `wren install`, private releases) and
> **multi-harness** (WS-12: codex + opencode); rename/App/docs-site
> (Phases 0–1, parts of 4–5) are secondary. Live state: [`tasks/STATUS.md`](tasks/STATUS.md).
> The phasing below remains the shape of the *public* launch when it resumes.

> **Status:** v1 · **Date:** 2026-07-14 · **Companions:** the evaluation and
> verdict live in [`oss-review.md`](oss-review.md); the engineering-level
> decomposition (workstreams, file-level scope, dependency graph) is in
> [`implementation-plan.md`](implementation-plan.md), and the parallel-agent
> execution process is in [`agent-workflow.md`](agent-workflow.md). This
> document is the *launch plan*: phased, with explicit exit criteria, sequenced
> so the project could launch at the end of Phase 4 and everything after
> compounds.
>
> Rough calendar (focused solo + agent-assisted work): Phases 0–1 ≈ 1 week,
> Phase 2 ≈ 1–2 weeks, Phase 3 ≈ 1–2 weeks, Phase 4 ≈ 1 week, Phase 5 in
> parallel, **launch at ~4–6 weeks**, Phase 6–7 post-launch.

---

## Phase 0 — Decisions (do first; everything downstream bakes these in)

| Decision | Recommendation | Why |
|---|---|---|
| **Name** | Rename. Shortlist 3–5 candidates; check: GitHub org, domain, `brew` formula, crates/npm squatting, trademark quick-search, and collision with wren.io + WrenAI | "Wren" collides twice (language + AI project). The spec already calls it a placeholder. Renaming after launch costs 10× |
| **License** | **Apache-2.0** (switch from MIT) | Patent grant; k8s-ecosystem convention; enterprise legal teams pre-approve it; keeps CNCF sandbox open as a future option |
| **Repo home** | Dedicated GitHub **org** (not personal account), fresh repo, clean history | History contains an unrelated work-email project (see review §4); an org survives maintainer changes and looks like infrastructure |
| **Module path / CRD group** | `github.com/<org>/<name>`; CRD group `<name>.dev` or `<name>.sh` (own the domain) | Baked into every import, manifest, and installed CRD — change exactly once |
| **v0 scope statement** | One paragraph in README: what v0.1 *is* (task→PR on your k8s, Claude Code harness, credential isolation) and *is not* (no steering, no web UI, no multi-tenant SaaS, GitHub-only) | Manages expectations; the spec's non-goals section shows you already think this way |
| **Positioning line** | "Run coding agents on **your own Kubernetes** — durable, sandboxed, and the agent never holds a credential." | Leads with the differentiators in order of defensibility |

Also decide: DCO (recommended — lightweight) vs CLA; semver from `v0.1.0`;
support matrix (k8s ≥1.30, kind, GKE at launch; EKS/AKS "should work,
untested").

---

## Phase 1 — Fresh repo & hygiene

**Exit criterion: a private repo that could be flipped public without embarrassment.**

- [ ] Create the new org + repo; copy the tree (not the history) at the current
      commit; single initial commit or a curated short history.
- [ ] Keep `a-labs` private as the archive; note the provenance in its README.
- [ ] Rename module path everywhere (`go.mod`, imports, manifests, Dockerfiles,
      Makefile, docs, CRD group + regenerate manifests via `make manifests generate`).
- [ ] Replace `.gitignore` (drop the Python/Expo remnants; Go + IDE + OS only).
- [ ] Swap LICENSE to Apache-2.0 + add NOTICE.
- [ ] Run `gitleaks` / `trufflehog` on the final tree as a gate.
- [ ] Verify authorship: all commits under your personal identity.
- [ ] Move the spec's "M0 deviations" list into `docs/adr/` as numbered ADRs
      (transport, store, auth, PAT-vs-App, egress enforcement, runc-vs-gVisor).
      ADRs age better than a living status block and show engineering judgment.

---

## Phase 2 — Security honesty (the launch-credibility work)

**Exit criterion: every security claim in the README is enforced, not cooperative
— or explicitly footnoted. This is the #1 pre-launch engineering item.**

- [ ] **Egress bypass enforcement.** Default-deny `NetworkPolicy` on agent pods
      (egress only to kube-dns + the proxy path) **plus** in-pod enforcement,
      since sidecars share the netns: privileged init container installs
      iptables rules redirecting/blocking all egress except from the proxy's UID
      (Istio's proven pattern). Runner runs as a distinct non-proxy UID (images
      already run as 65532 — give the proxy its own UID).
      *Fallback if this slips:* reframe every claim (see review §5.2) — do not
      ship the current wording with the current gap.
- [ ] **Wire the GitHub App token minter** (already built in `internal/github/app.go`)
      into the control plane: per-run, repo-scoped installation tokens handed to
      the proxy; PAT demoted to a documented quickstart-only fallback. Cheap win,
      closes a standard enterprise objection.
- [ ] **Tighten the proxy:** restrict `CONNECT` to port 443; scrub hop-by-hop
      and auth headers on the forward path; add read/idle timeouts; (post-launch)
      per-run allowlists from the CR instead of one static list.
- [ ] **Verify pod hardening defaults in `buildAgentPod`** against the spec §5.6
      checklist (non-root, read-only rootfs, drop ALL caps, seccomp
      RuntimeDefault, no SA token automount) — and add a unit test that asserts
      them so regressions fail CI.
- [ ] **`SECURITY.md`:** threat model summary (lift spec §9), the honest
      residual-risk list (runc escape → node, header-auth apiserver must not be
      internet-exposed), disclosure policy + contact.
- [ ] Fix the token-usage accumulation nit in `internal/harness/claudecode.go`
      (sum across turns rather than keeping the last event).

---

## Phase 3 — Minimum credible product

**Exit criterion: a platform team can run this for a week without hitting a
"toy" wall.**

- [ ] **Durable store.** Postgres implementation of the existing `Store`
      interface (pgx; schema migrations via goose/atlas embedded in the binary).
      Optionally SQLite for the single-node quickstart so Postgres isn't a
      quickstart dependency. *The in-memory store loses every run on restart —
      indefensible given the "durable" pitch.*
- [ ] **`wren run logs`.** Server-side: stream/aggregate the pod's container
      logs through the control plane (the CLI already reserves the command).
      Without it, the first debugging session dead-ends into `kubectl`, which
      the CLI's whole premise is to avoid.
- [ ] **Checkpointer decision.** Either implement the GCS/S3 workspace snapshot
      (git-aware bundle per spec §5.5) or **de-scope**: v0.1 resumes via PVC
      reattach only, README says so. Don't ship stub-backed claims.
- [ ] **Helm chart** (`charts/<name>`) as the primary install path: CRDs,
      operator, apiserver, RBAC, values for images/namespaces/creds. Keep
      kustomize for contributors. Publish to GHCR as an OCI chart.
- [ ] **Object-storage abstraction:** a small `BlobStore` interface (S3-compatible
      + GCS impls; MinIO in kind for local e2e). This is the main de-GCP-ing
      needed — GKE-specific bits (Workload Identity, Regional PD) become a
      *production guide*, not a requirement.
- [ ] **De-GCP the docs:** "any Kubernetes" is the headline path; GKE is one
      production profile. The spec's GCP table moves to `docs/production-gcp.md`.

---

## Phase 4 — Developer experience (the 10-minute wow)

**Exit criterion: a stranger on a Mac/Linux box gets from `brew install` to a
real PR opened by a real agent in under 10 minutes — and CI proves the path
continuously.**

- [ ] **`<cli> quickstart`** command (or one `curl | sh`-able script): checks
      Docker/kind/kubectl, creates the kind cluster, installs the chart, prompts
      for `ANTHROPIC_API_KEY` + `gh auth token`, registers a sample project,
      submits a demo task, prints the PR URL. This replaces the env-var
      incantation in `hack/setup.sh` — every removed flag doubles the audience.
- [ ] **Releases via goreleaser:** darwin/linux × amd64/arm64 binaries, Homebrew
      tap, `go install` support, multi-arch images on GHCR (operator, apiserver,
      runtime, claude-code), cosign-signed, SBOM attached.
- [ ] **Keyless CI e2e (the mock harness is the asset):** GitHub Actions job —
      kind + chart install + `run create --harness mock` → asserts `Succeeded` +
      a PR against a local gitea (or a fixture remote). No API keys in CI, real
      coverage of the whole control loop. Make this the merge gate.
- [ ] **README rewrite for the public audience:** 90-second pitch, animated
      demo (vhs/asciinema GIF of submit→PR), the credential-boundary diagram,
      quickstart, comparison table (vs hosted agents / OpenHands / agent-sandbox),
      honest status table (keep it — it's a trust signal).
- [ ] **Docs site** (mkdocs-material, versioned): Quickstart · Concepts
      (Run/Project/Harness/Pool) · Security model (the deep-dive — this page is
      the marketing) · Production install (GKE profile) · Writing a harness ·
      CLI + API reference · ADRs.
- [ ] **Example repo** (`<org>/demo-app`): a small app with seeded issues that
      make satisfying demo tasks.

---

## Phase 5 — Community & engineering infrastructure (parallel with 2–4)

- [ ] **CI:** build + test + `golangci-lint` + `govulncheck` + `go vet` on PR;
      the kind e2e above; CodeQL; trivy scan on images; branch protection.
- [ ] **Community files:** CONTRIBUTING.md (adapt AGENTS.md — it's already a
      strong contributor guide; keep AGENTS.md itself, agent-contributors are on
      brand here), CODE_OF_CONDUCT (Contributor Covenant), issue templates
      (bug/feature/security-redirect), PR template, DCO check.
- [ ] **Releases:** CHANGELOG (keep-a-changelog), release notes automation,
      `v0.1.0` tagged at launch; document the support matrix.
- [ ] **Comms channels:** GitHub Discussions at minimum; Discord/Slack only when
      there's traffic (an empty Discord is worse than none).
- [ ] **Governance-lite:** MAINTAINERS.md, roadmap as a GitHub Project fed by
      the spec's milestones (M1 breadth, M2 interactive, M3 scale, M4 isolation).

---

## Phase 6 — Architecture & code improvements (start pre-launch, finish after)

Sequenced by leverage:

1. **Public harness contract.** Promote `internal/runspec` + the event protocol
   to an importable, versioned package (e.g. `pkg/harness/v1`): RunSpec schema,
   event stream, exit-code semantics, resume contract. BYO harnesses are a core
   promise — the contract must be a stable artifact people can build against,
   with a conformance test (`harnesstest`) they can run. *This is the API that
   makes Wren a platform rather than a tool.*
2. **Transport: Connect-RPC.** One schema → gRPC + gRPC-Web + plain JSON/HTTP;
   typed clients; streaming for `logs`/`attach` later. Do it before third-party
   clients exist; it also replaces the hand-rolled HTTP client.
3. **API conventions:** structured error model, request IDs, OpenAPI doc
   published from the schema.
4. **CRD polish:** status subresource conventions already good — add printer
   columns (`kubectl get agentruns` showing PHASE/PR/RESTARTS/AGE), field
   validation via CEL, and a conversion-ready `v1alpha1→v1beta1` posture.
5. **Auth: OIDC** at the apiserver (device flow in the CLI is already stubbed by
   `wren login`) — the gate to any multi-user production deployment; until then
   SECURITY.md carries the header-auth warning.
6. **Observability:** structured logs are in; add Prometheus metrics for the
   operator (runs by phase, restarts, durations) and OTEL traces on the API path.
   `wren fleet` and `usage` build on this (M1).
7. **Then the spec's M1/M2 roadmap** (Codex + BYO adapters, MCP config, usage
   metering, attach/steer) — sequenced by user demand once real users exist.

---

## Phase 7 — Launch

- [ ] **Pre-flight:** external security review of the egress/credential path (even
      an informal one by a respected practitioner — quote it); fresh-machine
      quickstart test by someone who isn't you; gitleaks final pass; flip public.
- [ ] **Launch content:** an architecture blog post as the centerpiece — *"the
      agent never holds a credential"* is the hook (the egress-proxy
      credential-injection pattern, the crash-resume state machine, the honest
      threat model). Show HN + r/kubernetes + r/devops + k8s Slack + CNCF
      TAG-Security list; a 3-minute demo video.
- [ ] **Target the stated audience directly:** platform/infra engineers at
      AI-forward SaaS companies. The pitch that lands: *"your engineers already
      run Claude Code; here's how to run it at fleet scale inside your VPC
      without handing it your GitHub org."*
- [ ] **First-90-days metrics that matter:** number of orgs with a working
      install (ask via a lightweight adopter file/telemetry-free survey), issues
      filed by non-you, first external contributor, first BYO harness built by
      someone else. Stars are vanity; a platform team running it in staging is
      signal.
- [ ] **Post-launch cadence:** respond to issues < 24h for the first month;
      biweekly releases; publish the roadmap; write down what M1 users actually
      ask for before building M1.

---

## Cut list (explicitly *not* before launch)

To protect the timeline, these stay post-v0.1 no matter how tempting: gRPC
migration completion (Connect can land at v0.2), web dashboard, Codex/BYO
adapters, MCP service, steering/attach, warm pools, gVisor/Kata, Terraform
modules, non-GitHub SCMs, usage metering. The spec already defers most of these
— hold that line under launch pressure.
