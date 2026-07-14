# WS-9: Docs site

**Branch:** `ws9-docs-site` · **Worktree:** `../wren-ws9` · **Size:** M · **State:** DRAFT
**Blocked on:** WS-2 + WS-8 merged (pages must describe true behavior).
*Orchestrator: finalize the page list against what actually shipped; decide
the site name/domain only if WS-10's rename has been decided — otherwise
build under the working name and let WS-10's rename script sweep it.*

## Design (settled — implementation-plan §WS-9)

- mkdocs-material under `site/` (or `docs-site/` — avoid colliding with
  `docs/`), deployed to GitHub Pages via a workflow on tag.
- Pages: Quickstart (from WS-6's flow) · Concepts (Run/Project/Harness/Pool +
  the §4.1 lifecycle state machine) · **Security model** (the deep-dive; adapt
  spec §5.6/§9 + SECURITY.md — this page is the marketing) · Production
  install (Helm; GKE profile as one production example, not the requirement) ·
  Writing a harness (RunSpec, event protocol, exit codes — from
  `internal/runspec` + `internal/harness` doc comments) · CLI reference
  (generate from cobra) · HTTP API reference · ADR index.
- The internal spec (`docs/technical-spec.md`) stays as-is; site pages are
  user-facing rewrites, not moves.

**OUT:** rebranding/renaming; blog/launch post (human + orchestrator);
versioned docs (single version until v0.2); comparison-table page (launch
post material, keep out of docs).

## Definition of done (finalize at dispatch)

- [ ] `mkdocs build --strict` clean; local serve reviewed page-by-page.
- [ ] Every command in Quickstart copy-pasted-verified against a real run.
- [ ] Harness-contract page technically reviewed against `internal/runspec`
      (exit codes, event names — no drift).
- [ ] Pages workflow lands disabled-by-default until the repo is public.
