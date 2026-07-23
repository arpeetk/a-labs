# WS-13: Onboarding — `wren install` + `wren project` + private releases

**Branch:** `ws13-onboarding` · **Worktree:** `../wren-ws13` · **Size:** L · **State:** READY
**Blocked on:** nothing. **Main track.** UX judgment calls — orchestrator
reviews the flow closely. *Owner's bar: onboarding/install/setup is product
surface — first-class CLI commands, not scripts. "A team stands up a GKE
cluster and its engineers are running agents in minutes." Rename deferred —
build as `wren`.*

## Design (settled)

1. **`wren install`** (`internal/cli/install.go` + `internal/install/`):
   - Preflight: `kubectl` present, chosen `--kube-context` reachable, server
     ≥ 1.27; clear remediation messages (not stack traces).
   - Manifests: `config/` kustomize stays the source of truth; a
     `make assets` target renders `kubectl kustomize config/default` into
     `internal/install/assets/manifests.yaml` (go:embed) so install works
     with no repo clone and no kustomize binary. CI check: rendered asset is
     up-to-date (fail if `config/` drifts without re-render).
   - Images: `--registry <prefix>` builds linux/amd64 ×3 (runtime, operator,
     apiserver), pushes, and overrides image refs imperatively (the proven
     `hack/e2e-gke.sh` pattern — `kubectl set image` + arg patch — moved into
     Go). `--kind` builds and `kind load`s instead (local eval path).
   - Credentials: `GITHUB_TOKEN` (default `gh auth token`) + `ANTHROPIC_API_KEY`
     from env or interactive prompt (never echoed); creates the proxy Secrets
     via the typed client. PAT path documented; GitHub App is WS-2 (later).
   - Finish: wait for both Deployments Ready; print the engineer hand-off —
     apiserver address (port-forward command; `--expose=LoadBalancer` for
     team setups), how to get the CLI, `wren login`, register-a-project.
   - `wren uninstall`: remove the install (namespace + CRDs) behind a
     confirmation flag.
2. **`wren project create` / `wren project list`** — make the `misc.go`
   placeholders real against the existing client (`POST/GET /v1/projects`):
   name, repo, harness image, cpu/memory/disk; list prints a table.
3. **Private releases (dogfood the team *now*, pre-org):** `.goreleaser.yml`
   (darwin/linux × amd64/arm64 binaries + checksums) and
   `.github/workflows/release.yml` on tag, publishing to **this repo's**
   releases + GHCR (`ghcr.io/arpeetk/*` — works pre-rename; the public
   skein-sh tap is WS-10). Engineer install: `gh release download` or
   `GOPRIVATE` `go install`.
4. **Cleanup:** delete `hack/setup.sh` (superseded by `wren install`); rewrite
   `SETUP.md` around the new flow (GKE headline path, kind eval path,
   engineer onboarding); keep `hack/e2e*.sh` (dev/test tooling — say so in
   their headers and in `AGENTS.md` §2).

## Scope guards

**OUT:** Helm chart (WS-5, after this lands); public tap/org (WS-10); GitHub
App (WS-2); Postgres provisioning (`--store-dsn-secret` accepted, not
provisioned); apiserver auth (header-auth warning printed by install — real
auth is a later milestone).
**Hot files:** `internal/cli/*`, `internal/install/` (new), `Makefile`,
`.goreleaser.yml`, `.github/workflows/release.yml`, `SETUP.md`, `README.md`
(install section), `AGENTS.md` §2/§7, `hack/setup.sh` (delete).

## Definition of done

- [ ] `wren install --kind` from a clean kind-less state → control plane
      Ready → `wren project create` → `wren run create` reaches `Succeeded`
      — scripted verification (extend or reuse `make e2e` machinery), run
      and pasted in the hand-off.
- [ ] `wren uninstall` leaves no residue; install is idempotent (run twice).
- [ ] `goreleaser release --snapshot --clean` produces artifacts locally.
- [ ] Prompts never echo secrets; `--registry` path code-reviewed line by
      line (it is the GKE story; it cannot be run without GCP — record as
      NOT verified and list the exact live command for the owner).
- [ ] `make test vet` + lint green; `make e2e` green. SETUP.md read end-to-end
      by the orchestrator as the acceptance test.
