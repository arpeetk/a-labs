# WS-14: Harness images in onboarding

**Branch:** `ws14-harness-image-onboarding` · **Worktree:** `../wren-ws14` · **Size:** M
**State:** READY · **Blocked on:** PR #23 (`chore/harness-crossbuild`) merged to `main` —
`build/Dockerfile.codex` needs the buildx cross-compile fix before this workstream
builds/pushes it for real.

*Context: WS-12 shipped codex + opencode harness adapters and images; WS-13 shipped
`wren install`/`wren project create` as first-class onboarding commands. Neither
closed the seam between them — `wren install` never builds or pushes a harness
image, only the 3 control-plane images (runtime/operator/apiserver). A fresh team
following the printed onboarding hand-off today gets a broken first run. This
workstream closes that gap. Rename to skein is deferred — keep building as `wren`.*

## The gap (verified in code, 2026-07-24)

- `internal/install/steps.go:registryImages`/`kindImages` build+push/load exactly
  3 images. Harness images (`Dockerfile.claude-code`, `Dockerfile.codex`,
  `Dockerfile.opencode`) are never touched by `wren install`.
- `steps.go:harnessImageHint()` — the string the install hand-off prints as the
  example `--harness-image` value — resolves to `<registry>/runtime:<tag>` (GKE)
  or `wren/runtime:dev` (kind). The runtime image has no agent CLI in it; using it
  as a harness image is a guaranteed deterministic failure.
- `internal/coreapi/service.go:50` — the control plane's fallback
  `HarnessImage: "wren/claude-code-runner:latest"` (used when a project registers
  with no `--harness-image`) doesn't match any image this repo builds. Dead
  placeholder, predates WS-12/WS-13.
- `internal/cli/project.go:40` — `--harness` flag help text says
  `"claude-code|mock"`, stale since WS-12 added `codex`/`opencode`.

## Design (settled)

1. **Build+push/load harness images from `wren install`.** Extend
   `steps.go`'s image step (both `--registry` and `--kind` paths) to also handle
   `build/Dockerfile.claude-code`, `build/Dockerfile.codex`,
   `build/Dockerfile.opencode`. Default: build all three (a team shouldn't have to
   discover a separate manual step to unlock `codex`/`opencode` later — "seamless"
   is the bar). Add a flag to restrict the set for faster iterative installs,
   e.g. `--harness-images=claude-code,codex,opencode` (comma list; accepts `none`
   to skip entirely — useful for a keyless/mock-only eval install). Reuse the
   existing `build`/`push` helpers in `steps.go` rather than duplicating docker
   invocation logic — they're already Dockerfile-path-parameterized.
2. **Fix `harnessImageHint()`** to reference the *actually built* image for the
   project's intended default harness (claude-code, since that's `DefaultHarness`
   when unset) — `<registry>/claude-code:<tag>` (GKE) / `wren/claude-code:dev`
   (kind) — not `runtime:<tag>`.
3. **Fix `coreapi`'s hardcoded default** (`service.go:50`) to match the real
   naming scheme (`wren/claude-code:dev` — consistent with the kind zero-config
   path) or decide deliberately to require an explicit `--harness-image` with a
   clear error instead of a silently-wrong default; document whichever is chosen.
4. **Fix stale `--harness` flag help** in `internal/cli/project.go` and
   `internal/cli/run.go` (if it also lists harnesses) to `mock|claude-code|codex|opencode|byo`.
5. **Docs:** update `docs/harnesses.md` and `SETUP.md` onboarding examples so the
   documented flow matches what the CLI actually does end-to-end.

## Scope guards

**OUT:** live-provider-key validation of codex/opencode (needs real API keys —
separate, human-gated; WS-12 hand-off already carries the recipe); GitHub App
(WS-2); Helm chart (WS-5); any change to `RunSpec`, the pod shape, or the harness
adapters themselves (WS-12 territory, frozen); renaming.
**Hot files:** `internal/install/steps.go`, `internal/install/install_test.go`,
`internal/install/kube_test.go` (or wherever the install-step tests live —
check current file layout), `internal/coreapi/service.go`,
`internal/cli/project.go`, `internal/cli/run.go`, `docs/harnesses.md`,
`SETUP.md`.

## Definition of done

- [ ] `wren install --kind` (default flags) builds/loads all 3 harness images in
      addition to the 3 control-plane images; `--harness-images=claude-code`
      restricts to one; `--harness-images=none` skips harness images entirely.
- [ ] `wren install --registry <prefix>` (code-reviewed line by line — cannot run
      without GCP, same as WS-13's registry path; record as NOT verified and list
      the exact live command for the owner) builds+pushes the same set.
- [ ] Install hand-off prints a `--harness-image` example that is the real,
      correct image for the chosen default harness — not `runtime:<tag>`.
- [ ] `coreapi`'s default `HarnessImage` is either a real buildable image ref or
      replaced with an explicit required-field error — no more silent wrong
      default.
- [ ] `--harness` flag help text lists all real harnesses everywhere it appears.
- [ ] Unit tests via the existing `FakeRunner`/`FakeKube` assert the new
      build/push/load calls happen (and are skippable via the new flag).
- [ ] `make test vet` + lint green; `make e2e` green (mock harness path
      unchanged — e2e stays keyless, don't wire live keys into it).
- [ ] Hand-off documents the exact `wren install --registry ... && wren project
      create --harness codex --harness-image <ref>` sequence a team would run to
      light up a non-default harness, end to end.
