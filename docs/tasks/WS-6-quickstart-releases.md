# WS-6: Quickstart command + release engineering

**Branch:** `ws6-quickstart-releases` · **Worktree:** `../wren-ws6` · **Size:** L · **State:** DRAFT
**Blocked on:** WS-5 merged. *Orchestrator: this one has UX judgment calls —
prefer a stronger worker model, or review the UX output closely. Split into
two PRs (quickstart; releases) if the diff grows.*

## Design (settled — implementation-plan §WS-6)

1. **`wren quickstart`** (`internal/cli/quickstart.go`):
   preflight (docker, kind, kubectl, helm — with install hints per tool) →
   kind cluster → images (`--build` for dev; GHCR pulls once releases exist) →
   `helm install` → secrets from `ANTHROPIC_API_KEY` + `gh auth token`
   (prompt interactively if absent; never echo values) → register a demo
   project → submit a demo task → poll with a progress line → print the PR
   URL. `--teardown` reverses everything. Every step idempotent and resumable.
2. **goreleaser** (`.goreleaser.yml` + `.github/workflows/release.yml`, on
   tag): `wren` binaries darwin/linux × amd64/arm64, archives + checksums,
   cosign signing, SBOM (syft), Homebrew tap (needs the human to create the
   tap repo), GitHub Release notes from CHANGELOG.
3. **Image publishing** in the same release workflow: buildx multi-arch for
   operator/apiserver/runtime/claude-code to GHCR. Keep the proven pattern:
   cross-compile Go + COPY into the image (Go-under-qemu is known-broken);
   trivy scan gate.
4. `CHANGELOG.md` (keep-a-changelog), seeded for `v0.1.0`.

**OUT:** the demo `<org>/demo-app` repo and vhs GIF (human/orchestrator task —
needs the public org); docs-site changes (WS-9); brew formula contents beyond
what goreleaser generates.

## Definition of done (finalize at dispatch)

- [ ] Fresh-machine simulation: `wren quickstart` from a clean kind-less state
      → PR URL printed, < 10 min, with only the two credentials provided.
- [ ] `wren quickstart --teardown` leaves no cluster/config residue.
- [ ] `goreleaser release --snapshot --clean` produces installable artifacts
      locally; workflow YAML reviewed (first real run happens on tag).
- [ ] `make e2e` still green.
