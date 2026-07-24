# Workstream status

One line per workstream. States: `todo` · `ready` (brief ready to dispatch) ·
`dispatched` · `in-review` · `merged` · `blocked(<why>)`. Update on every
transition; this file is the single glance-view for the sprint.

> **Priority call (2026-07-22, owner):** the main track is **(1) seamless
> onboarding** — a team stands up a GKE cluster and its engineers are running
> agents in minutes, via first-class CLI commands (not scripts) — and
> **(2) multi-harness** (Codex + OpenCode alongside Claude Code). Rename
> (WS-10), GitHub App (WS-2), docs site (WS-9) are **secondary** — they ride
> after the dogfood.

| WS | Title | Brief | State | Worker/branch | Blocker |
|----|-------|-------|-------|---------------|---------|
| 0  | e2e validation loop | [WS-0](WS-0-e2e-loop.md) | merged | #12 | done — gate live on main |
| 1  | Egress enforcement | [WS-1](WS-1-egress-enforcement.md) | merged | #16 | done — 3 review rounds; kind e2e green both modes |
| 3  | Postgres store | [WS-3](WS-3-postgres-store.md) | merged | #15 | done — pgx/v5 + reconcile-on-boot |
| 4  | `run logs` | [WS-4](WS-4-run-logs.md) | merged | #14 | done — live-tail streaming |
| 7  | CI + community | [WS-7](WS-7-ci-community.md) | merged | #13 | done — CI/e2e/CodeQL live on main |
| 8  | Claims truthing | [WS-8](WS-8-claims-truthing.md) | merged | #19 | done — README/spec/SECURITY truthed; `internal/blob` socket |
| 11 | Finalize pipeline | [WS-11](WS-11-finalize-pipeline.md) | merged | #18 | done — idempotent finalize, retry classification, prUrl live |
| **12** | **Codex + OpenCode harnesses** | [WS-12](WS-12-multi-harness.md) | merged | #20 | done — adapters+images+/openai/ route; live-key runs unverified (recipe in ledger) |
| **13** | **Onboarding: `wren install` + project CLI + releases** | [WS-13](WS-13-onboarding.md) | merged | #21 | done — kind DoD transcript in hand-off; `--registry` GKE path unverified (command in ledger) |
| 5  | Helm chart | [WS-5](WS-5-helm-chart.md) | deferred | — | GitOps install path; after WS-13's CLI-native path lands |
| 2  | GitHub App tokens | [WS-2](WS-2-github-app.md) | deferred | — | secondary (owner call); PAT documented as the path meanwhile |
| 6  | Quickstart + releases | [WS-6](WS-6-quickstart-releases.md) | superseded | — | folded into WS-13 (install cmd + private releases) |
| 9  | Docs site | [WS-9](WS-9-docs-site.md) | deferred | — | secondary |
| 10 | Rename + public cut | [WS-10](WS-10-rename-repo-cut.md) | deferred | — | secondary; decisions locked (skein/skein-sh/skein.dev/Apache-2.0) |
| 11 | Finalize pipeline | [WS-11](WS-11-finalize-pipeline.md) | merged | #18 | done — idempotent finalize, retry classification, prUrl live |
| —  | Harness image cross-build fix | (chore, no brief) | merged | #23 | done — buildx `--platform=$BUILDPLATFORM` applied to all 3 harness Dockerfiles |
| **14** | **Harness images in onboarding** | [WS-14](WS-14-harness-image-onboarding.md) | merged | #24 | done — `--harness-images` build/push/kind-load, correct hand-off image ref, dead placeholder default fixed; live kind validation in hand-off; `--registry` GKE path code-reviewed only |
| **15** | **Onboarding friction pass + CLI surface cleanup** | [WS-15](WS-15-onboarding-cli-cleanup.md) | merged | #25 | done — install-configured default run-namespace closes the footgun (live-validated), pre-flight credential check (400 not silent failure), `project get`/`run rm`/`run stop` real, `run resume`/`mcp`/`fleet`/`usage`/`attach`/`steer`/`project config` removed from CLI (zero "not implemented yet" left), `--runtime gvisor|kata` rejected client-side with an M4 pointer |

## Human-gated items (start now — lead time)

- [x] Name decision: **skein** (2026-07-22). Org name **skein-sh** (bare `skein`
      taken by a dormant personal account). `skein.sh`/`.io` are third-party —
      docs/site live at **`skein.dev`** (unregistered — **register it now**;
      it is also the CRD group). PyPI/npm squatting irrelevant (Go binary);
      nearest project collision is `jcrist/skein` (dormant YARN deploy tool).
- [x] License decision: **Apache-2.0** (+ NOTICE at cut; WS-10).
- [ ] **Register `skein.dev`** (domain; ~$12 — do today, it backs the CRD group)
- [ ] Create GitHub org **skein-sh** (+ `homebrew-tap` repo for WS-6)
- [ ] Create a test GitHub App (blocks WS-2 live validation)
- [ ] External reviewer lined up for the egress/credential path (WS-1 had 3
      agent review rounds + e2e proof; a human security read is still the
      pre-launch gate per oss-plan §7)
- [ ] Stranger recruited for the 10-minute quickstart test (pre-launch)

## Deferred-verification ledger

Things hand-offs said were NOT verified; burn down before launch.

- **WS-7 CI executed** (was "never executed"): first real runs on PR #16 +
  main. **Green:** build/test/vet, kind e2e (4m26s, well inside the 20-min
  budget), CodeQL. **Red:** golangci-lint + govulncheck — findings below.
- **Lint findings on main** (golangci-lint job red until cleared):
  1. ~~podruntime misspell `cancelled`~~ — **cleared by WS-1** (#16).
  2. ~~`internal/harness/mock.go` unused `truncate`~~ — **cleared by WS-1** (#16).
  3. `api/v1alpha1/scheme.go:7` — staticcheck `SA1019` `scheme.Builder`
     deprecated (use `runtime.NewSchemeBuilder`). Owner: **WS-8** / orchestrator.
  4. `internal/github/github_test.go:86` — `QF1002` tagged switch (cosmetic).
     Owner: **WS-2** (github lane).
  5. `internal/coreapi/service_test.go:245` — `SA9003` empty branch (vestigial
     test block; found during WS-1 review). Owner: orchestrator cleanup.
- **govulncheck red** (surfaced by CI's first run; deps drifted since WS-7):
  ~~bump `golang.org/x/crypto` → v0.52.0 (7 reachable ssh vulns via go-git),
  `golang.org/x/net` → v0.55.0, `golang.org/x/text` → v0.39.0; the stdlib
  `crypto/tls` finding needs Go ≥ 1.26.5 (setup-go "1.26" picks it up
  automatically)~~ — **fixed in #17** (chore/green-main; govulncheck green there).
- **Lint findings 3–5 above** (SA1019, QF1002, SA9003) — **all fixed in #17**;
  the golangci-lint job is green on that branch. Merge #17 → main fully green →
  enable branch protection.
- ~~**WS-1 GKE live validation**~~ — **DONE (2026-07-24)**: `wren-e2e` GKE
  Standard (1.35.6), installed via `wren install --registry`. Lockdown exited
  0 on a real node (IPv4 + IPv6 rules applied); canary **PASSED** ("direct
  dial to 1.1.1.1:443 blocked", "direct HTTPS to https://github.com/ blocked",
  "via egress-proxy succeeded"); run `r-6df35890` Succeeded with
  `EgressEnforcement=True/Iptables`. Autopilot/PSA admission remains
  untestable here (covered by design: deterministic `PodAdmissionForbidden`).
- ~~**WS-13 `--registry` live run**~~ — **DONE (2026-07-24)**: same session —
  install built + pushed `799451d` images to AR, stored the GitHub token via
  the env path (no prompt echo), control plane Ready, engineer flow
  (login → project create → run create → Succeeded) exercised over
  port-forward. `--expose=LoadBalancer` still untried.
- **WS-12 live-key harness validation** — codex + opencode are unit-tested
  and flag-verified against the real CLIs, but never run live (no keys in
  CI). Per-harness recipe in the #20 hand-off (secret, project config, smoke
  task, expected events). Candidate follow-up: keyless stub-upstream e2e via
  the `WREN_OPENAI_UPSTREAM`/`WREN_ANTHROPIC_UPSTREAM` seams.
- **WS-13 release.yml** — untriggered until the first `v*` tag (goreleaser
  half proven via --snapshot; GHCR buildx half reuses e2e-built Dockerfiles).
- **WS-7 repo-settings checklist** (human, GitHub UI): branch protection on
  `main` (require CI + e2e checks), DCO app, merge queue, enable Discussions,
  CodeQL in Security tab. See WS-7 hand-off for specifics. Note: PR #16
  merged with lint/govulncheck red (findings pre-existed on main) — possible
  only because branch protection isn't on yet.

## Review-round findings folded into WS-1 (#16)

So they don't get lost — fixed pre-merge in rounds 2–3, no follow-up needed:
runner uid pinned in `hardened()` · IPv6 lockdown fails closed on missing
binary · reverse-proxy Director scrubs inbound creds · admission-Forbidden
fails the run deterministically · operator flag validation prints to stderr ·
GKE e2e image coords single-sourced (config/gke-e2e/ deleted).

Open follow-ups spun out of review (not blockers): `hack/lib/e2e-common.sh`
extraction (e2e.sh/e2e-gke.sh duplication); apiserver http.Server timeouts;
proxy resolved-IP guard for CONNECT.

## Follow-ups from WS-8/WS-11 hand-offs (triage before launch)

- **Behavior/doc gap (needs a decision):** docs now say a disk-destroying
  loss ends the run `Failed`, but `ensurePVC` creates a *fresh empty* PVC —
  the code resumes into an empty workspace instead. Enforcing the documented
  semantic is a small controller change; ticket as its own fix (candidate
  for the next batch) or consciously re-document. (WS-8 hand-off #1)
- **WS-11 deferred by design:** `run list` does no per-item CR refresh (N+1
  reads — add if list must show fresh prUrl); `Status.SessionID` stays empty
  until an adapter emits a session id (event schema frozen in WS-11);
  `token_usage` records terminal values only (documented in spec).
- **Small stale spots (batch into any cleanup PR):** `internal/cli/run.go`
  help text promises "usage" (no field yet); `internal/store` package doc
  says Postgres "lands later" (stale since WS-3); `SETUP.md` Phase-2 framing
  predates `config/default` (WS-9 territory).
- **AGENTS.md** layout + checkpointer note updated by orchestrator at merge
  (WS-8 hand-off #5/#6 — done).
