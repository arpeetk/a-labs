# WS-7: CI + community files

**Branch:** `ws7-ci-community` · **Worktree:** `../wren-ws7` · **Size:** S–M · **State:** READY (dispatch with batch 1; the e2e job activates once WS-0 merges)

## Context (read first)

- `AGENTS.md` in full (CI must mirror its commands).
- `docs/implementation-plan.md` §WS-7 — the design.
- `docs/oss-plan.md` Phase 5 (community scaffolding intent).

## Scope

**IN:**

1. `.github/workflows/ci.yml` — on PR + main push: setup Go **1.26**, cache,
   `go build ./...`, `go test -race ./...`, `gofmt` check, `go vet`,
   `golangci-lint` (add a minimal `.golangci.yml` — start from defaults plus
   `govet`, `staticcheck`, `errcheck`, `ineffassign`, `misspell`; do NOT chase
   a huge linter list), `govulncheck`. Target <5 min wall clock.
   Note: the local PATH gotcha is irrelevant in CI; pin Go via `setup-go`.
2. `.github/workflows/e2e.yml` — on PR: kind (via `helm/kind-action` or
   manual), then `make e2e`. If WS-0 has not merged when you finish, land the
   workflow with the job marked `if: false` + a TODO so flipping it on is a
   one-line change.
3. `.github/workflows/codeql.yml` — default Go CodeQL config.
4. Community files at repo root: `CONTRIBUTING.md` (distill AGENTS.md: setup,
   build/test, PR expectations, link to it for depth — AGENTS.md stays),
   `CODE_OF_CONDUCT.md` (Contributor Covenant v2.1, contact = repo owner),
   `SECURITY.md` (summarize spec §9 threat model; the honest residual-risk
   list: egress enforcement status, `X-Wren-User` header auth must not be
   internet-exposed, runc containment; private disclosure instructions).
5. `.github/ISSUE_TEMPLATE/` (bug, feature, config-question; security →
   redirect to SECURITY.md) and `PULL_REQUEST_TEMPLATE.md` (what/why/how
   validated — mirror the hand-off note shape).

**OUT:** release workflows / goreleaser (WS-6); DCO bot and branch-protection
settings (repo settings, not files — list them in the hand-off for the human);
trivy/image scanning (WS-6, needs published images); any Go code change.
If lint findings require code edits, fix ONLY mechanical ones (gofmt,
misspell) and list the rest in the hand-off — do not refactor.

## Hot files

You own: `.github/**`, `.golangci.yml`, `CONTRIBUTING.md`,
`CODE_OF_CONDUCT.md`, `SECURITY.md`.
Do NOT touch: any `*.go` beyond the mechanical-fix rule above, `Makefile`,
`config/**`, `docs/**` (except adding links).

## Definition of done

- [ ] `make test vet` green locally; `golangci-lint run` clean or the
      residual findings listed in the hand-off.
- [ ] Workflows validated with `act` if available, else careful YAML review +
      a note that first-run validation happens on the PR itself.
- [ ] SECURITY.md accurately reflects the CURRENT enforcement state on your
      branch's base (check whether WS-1 has merged; write for what is true,
      flag for the orchestrator if WS-1 lands after you).
- [ ] Hand-off note incl. the repo-settings checklist (branch protection,
      DCO, merge queue) for the human.
