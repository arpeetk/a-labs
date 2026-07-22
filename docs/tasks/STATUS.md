# Workstream status

One line per workstream. States: `todo` · `ready` (brief ready to dispatch) ·
`dispatched` · `in-review` · `merged` · `blocked(<why>)`. Update on every
transition; this file is the single glance-view for the sprint.

| WS | Title | Brief | State | Worker/branch | Blocker |
|----|-------|-------|-------|---------------|---------|
| 0  | e2e validation loop | [WS-0](WS-0-e2e-loop.md) | merged | #12 | done — gate live on main |
| 1  | Egress enforcement | [WS-1](WS-1-egress-enforcement.md) | merged | #16 | done — 3 review rounds; kind e2e green both modes |
| 3  | Postgres store | [WS-3](WS-3-postgres-store.md) | merged | #15 | done — pgx/v5 + reconcile-on-boot |
| 4  | `run logs` | [WS-4](WS-4-run-logs.md) | merged | #14 | done — live-tail streaming |
| 7  | CI + community | [WS-7](WS-7-ci-community.md) | merged | #13 | done — CI/e2e/CodeQL live on main |
| 2  | GitHub App tokens | [WS-2](WS-2-github-app.md) | draft | — | test GitHub App created (human); split step-0 interface PR at dispatch |
| 5  | Helm chart | [WS-5](WS-5-helm-chart.md) | draft | — | none (WS-1 merged, manifests settled) — finalize brief + dispatch |
| 8  | Claims truthing | [WS-8](WS-8-claims-truthing.md) | ready | — | none (WS-1 outcome known: enforcement ON by default) — dispatch |
| 6  | Quickstart + releases | [WS-6](WS-6-quickstart-releases.md) | draft | — | WS-5 merged |
| 9  | Docs site | [WS-9](WS-9-docs-site.md) | draft | — | WS-2 + WS-8 merged |
| 10 | Rename + public cut | [WS-10](WS-10-rename-repo-cut.md) | draft | — | name/org/license decisions (human) |

## Human-gated items (start now — lead time)

- [ ] Name shortlist → decision (blocks WS-10; needed ~week 3)
- [ ] Create GitHub org placeholder once named
- [ ] License decision: Apache-2.0 (recommended) vs keep MIT
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
  bump `golang.org/x/crypto` → v0.52.0 (7 reachable ssh vulns via go-git),
  `golang.org/x/net` → v0.55.0, `golang.org/x/text` → v0.39.0; the stdlib
  `crypto/tls` finding needs Go ≥ 1.26.5 (setup-go "1.26" picks it up
  automatically). Owner: orchestrator cleanup — pairs with the lint fixes as
  one "green-main" PR.
- **WS-1 GKE live validation** — still not run on a real cluster. The
  round-3 fix made it runnable at defaults: `make docker-push-gke &&
  make e2e-gke` (GKE Standard cluster `wren-e2e`; Autopilot admission + PSA
  remain unverifiable locally). Owner: human with GCP access.
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
