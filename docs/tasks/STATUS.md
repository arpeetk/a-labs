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
| 7  | CI + community | [WS-7](WS-7-ci-community.md) | dispatched | ws7-ci-community | none (e2e job active now WS-0 merged) |
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

- (empty)
