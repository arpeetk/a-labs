# Workstream status

One line per workstream. States: `todo` · `ready` (brief ready to dispatch) ·
`dispatched` · `in-review` · `merged` · `blocked(<why>)`. Update on every
transition; this file is the single glance-view for the sprint.

| WS | Title | Brief | State | Worker/branch | Blocker |
|----|-------|-------|-------|---------------|---------|
| 0  | e2e validation loop | [WS-0](WS-0-e2e-loop.md) | merged | #12 | done — gate live on main |
| 1  | Egress enforcement | [WS-1](WS-1-egress-enforcement.md) | dispatched | ws1-egress-enforcement | none (human review before merge) |
| 3  | Postgres store | [WS-3](WS-3-postgres-store.md) | dispatched | ws3-postgres-store | none |
| 4  | `run logs` | [WS-4](WS-4-run-logs.md) | dispatched | ws4-run-logs | none |
| 7  | CI + community | [WS-7](WS-7-ci-community.md) | merged | #13 | done — CI/e2e/CodeQL live on main |
| 2  | GitHub App tokens | [WS-2](WS-2-github-app.md) | draft | — | WS-1 merged (pod.go); GitHub App created (human) |
| 5  | Helm chart | [WS-5](WS-5-helm-chart.md) | draft | — | WS-1 merged (manifests settle) |
| 8  | Claims truthing | [WS-8](WS-8-claims-truthing.md) | ready | — | WS-1 outcome known |
| 6  | Quickstart + releases | [WS-6](WS-6-quickstart-releases.md) | draft | — | WS-5 merged |
| 9  | Docs site | [WS-9](WS-9-docs-site.md) | draft | — | WS-2 + WS-8 merged |
| 10 | Rename + public cut | [WS-10](WS-10-rename-repo-cut.md) | draft | — | name/org/license decisions (human) |

## Human-gated items (start now — lead time)

- [ ] Name shortlist → decision (blocks WS-10; needed ~week 3)
- [ ] Create GitHub org placeholder once named
- [ ] License decision: Apache-2.0 (recommended) vs keep MIT
- [ ] Create a test GitHub App (blocks WS-2 live validation)
- [ ] External reviewer lined up for the egress/credential path
- [ ] Stranger recruited for the 10-minute quickstart test (pre-launch)

## Deferred-verification ledger

Things hand-offs said were NOT verified; burn down before launch.

- **WS-7 CI never executed** (no `act` locally): first real run of ci/e2e/codeql
  happens on the next PR — watch that the golangci-lint@v8, govulncheck, CodeQL
  autobuild, and `make e2e`-on-runner all pass within budget.
- **Lint findings on main** (surfaced by WS-7's golangci-lint; main lint check is
  red until cleared):
  1. `internal/podruntime/podruntime.go` + `_test.go` — misspell `cancelled`→`canceled`. Owner: **WS-1** (its lane) — apply at WS-1 merge.
  2. `internal/harness/mock.go:57` — `unused` dead `truncate`. Owner: **WS-1** (touches mock.go) — remove/keep at WS-1 merge.
  3. `api/v1alpha1/scheme.go:7` — staticcheck `SA1019` `scheme.Builder` deprecated. Owner: orchestrator cleanup / **WS-8** — real fix, not mechanical.
  4. `internal/github/github_test.go:86` — `QF1002` tagged switch (cosmetic). Owner: **WS-2** (github lane).
- **WS-7 repo-settings checklist** (human, GitHub UI): branch protection on `main`
  (require CI + e2e checks), DCO app, merge queue, enable Discussions, CodeQL in
  Security tab. See WS-7 hand-off for specifics.
