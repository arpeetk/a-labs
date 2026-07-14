# Wren — OSS v0.1 Implementation Plan

> **Status:** v1 · **Date:** 2026-07-14 · **Companions:**
> [`oss-review.md`](oss-review.md) (evaluation & verdict) ·
> [`oss-plan.md`](oss-plan.md) (launch phases & positioning) ·
> [`agent-workflow.md`](agent-workflow.md) (how we execute this plan with
> parallel agents).
>
> This document decomposes the launch plan into **workstreams (WS-0…WS-10)** with
> file-level scope, design decisions, acceptance criteria, and a dependency
> graph. Each workstream is sized to be one agent-executable unit of work
> (one branch → one PR). Sizes: **S** ≤ half day · **M** ≈ 1 day · **L** ≈ 2–3
> days of focused (agent-assisted) work.

---

## 0. Ground rules

- **Everything merges through the keyless e2e gate** (WS-0). No workstream is
  "done" until `make test vet` passes and, where it touches the run path,
  `make e2e` passes on kind.
- **Hot files are serialized** (see §12): `internal/controller/pod.go`,
  `cmd/wren-apiserver/main.go`, `Makefile`, `api/v1alpha1/agentrun_types.go`.
  Two workstreams never hold the same hot file concurrently.
- **CRD/API changes land first, tiny, alone** ("interface-freeze PRs") so
  parallel tracks build against a settled contract.
- **The rename (WS-10) is deferred to the end** and done mechanically in one
  commit at the public-repo cut. All feature work proceeds under
  `github.com/summiteight/wren` until then.
- Spec truth-keeping: every workstream that changes behavior updates the spec's
  living status block and the README status table in the same PR.

---

## WS-0 — The validation loop (`make e2e`) — **build first, size M**

Everything else goes faster once this exists; it is the merge gate and every
implementer agent's self-check. The deterministic, keyless **mock harness** is
the asset that makes it possible.

**Deliverables**

- `hack/e2e.sh` + `make e2e`: create (or reuse) a kind cluster → `make
  kind-load` → deploy `config/default` → create a `mock`-harness project via
  the in-cluster apiserver (port-forward) → `wren run create` → poll until
  `Succeeded` → assert phase + pod cleanup. Idempotent; `E2E_KEEP=1` to keep
  the cluster; exits non-zero with the operator/pod logs dumped on failure.
- Tiered targets:
  - `make e2e` — mock harness, **no repo configured** (finalize skipped): no
    keys, no network beyond image pulls. This is the CI gate.
  - `make e2e-pr` — adds an in-cluster **gitea** and asserts a real PR. Needs
    two small enablers: env-overridable upstreams in the egress-proxy role
    (`WREN_GITHUB_UPSTREAM`, `WREN_GITHUB_API_UPSTREAM` — the reverse-proxy
    routes currently hardcode github.com) and the `internal/github` client's
    already-injectable `BaseURL`. Local/nightly, not per-PR CI (slower).
- GitHub Actions job running `make e2e` on every PR (see WS-7).

**Files:** `hack/e2e.sh`, `Makefile`, `internal/podruntime` (upstream env
overrides), `.github/workflows/e2e.yml`.

**Acceptance:** a fresh clone on a machine with Docker+kind runs `make e2e`
green in <10 min with zero credentials.

---

## WS-1 — Egress bypass enforcement — **size L, security-critical**

Close the documented gap: the runner must *physically* be unable to reach the
network except through the proxy. In-pod iptables UID isolation is the
mechanism (the Istio-proven pattern); NetworkPolicy alone cannot distinguish
containers sharing the pod netns.

**Design**

1. **UID separation.** Runner/harness stays at uid `65532` (image default).
   The egress-proxy container gets an explicit `RunAsUser: 65533` in
   `buildAgentPod`. (Hydrate and finalize run in runner-side containers and go
   *through* the proxy already — they stay at 65532.)
2. **`egress-lockdown` init container**, first in the init order, running as
   root with `NET_ADMIN`+`NET_RAW` (drop everything else), applies iptables:
   - `OUTPUT -o lo -p tcp --dport <egressPort> -j ACCEPT` (anyone → proxy)
   - `OUTPUT -m owner --uid-owner 65533 -j ACCEPT` (proxy → world)
   - `OUTPUT -o lo -j ACCEPT` scoped as needed for kubelet probes
   - `OUTPUT -j REJECT` (default: everyone else, including DNS — the runner
     resolves nothing; the proxy does the resolving)
   Ships as a new `wren-runtime` role (`egress-lockdown`) so no new image; the
   runtime image gains a static `iptables-nft`/`xtables-nft-multi` binary (or,
   if distroless friction is high, a dedicated tiny Alpine-based
   `Dockerfile.lockdown` — decide in-branch, prefer one image).
3. **Escape hatch:** operator flag `--egress-enforcement=iptables|off`
   (default `iptables`). `off` is for clusters that forbid privileged init
   containers (GKE Autopilot); when off, the operator writes a
   `EgressEnforcement=Disabled` condition on every run so the posture is
   visible, and docs say what that means.
4. **Second layer (manifest-only):** optional default-deny egress
   `NetworkPolicy` for user namespaces + an FQDN-policy example for Cilium /
   GKE Dataplane V2, shipped under `config/netpol/` and referenced in docs.
   Belt-and-suspenders; not the primary control.
5. **Proxy tightening (same WS, small):** restrict `CONNECT` to port 443;
   strip hop-by-hop + incoming `Authorization`/`Proxy-Authorization` headers on
   the forward path; add dial/read/idle timeouts to `p.fwd`.

**Validation**

- Unit: pod-builder test asserting the lockdown init container, UIDs, caps.
- e2e: extend the mock harness with a **canary step** — attempt a direct
  `net.Dial("1.1.1.1:443")` and a direct HTTPS to github.com (must fail),
  then fetch through the proxy (must succeed). Runs inside `make e2e`.
- Manual on GKE Standard once, before launch (record in spec status).

**Files:** `internal/controller/pod.go` (+test), `cmd/wren-runtime` +
`internal/podruntime` (new role), `build/Dockerfile.runtime`,
`internal/egress/proxy.go` (+test), `internal/harness/mock.go` (canary),
`config/netpol/`, docs.

**Acceptance:** canary e2e green; README/spec claims updated from
"cooperatively routes" to "enforced"; SECURITY.md documents the `off` mode.

---

## WS-2 — GitHub App per-run tokens — **size L, sequenced after WS-1** (shares `pod.go`)

The minter exists (`internal/github/app.go`); this wires it end-to-end and
fixes credential *transport* so short-lived tokens can rotate.

**Design**

1. **Env → volume.** The proxy currently gets `GITHUB_TOKEN`/`ANTHROPIC_API_KEY`
   via env (`secretEnv` in `pod.go`) — baked at pod start, unusable for 1-hour
   installation tokens. Switch both to **Secret volume mounts**; the proxy's
   `Authorizer` reads the file per-request (kubelet refreshes mounted Secrets
   within ~1 min of an update). This also removes creds from `kubectl describe`
   env output.
2. **Interface-freeze PR (tiny, first):** add
   `spec.credentials.githubTokenSecret` to `AgentRunSpec` (+deepcopy,
   manifests). Operator prefers the per-run secret; falls back to the
   operator-level `PodConfig.GitHubTokenSecret` (PAT path, kept for
   quickstart).
3. **Control plane:** apiserver loads App credentials (App ID + PEM) from a
   mounted Secret (`--github-app-secret`). On `CreateRun`, coreapi resolves the
   installation for the project's repo (`GET /repos/{owner}/{repo}/installation`),
   mints a token scoped `repositories: [repo]`, `permissions: {contents:write,
   pull_requests:write, metadata:read}`, and writes it as Secret
   `<run>-github-token` in the run namespace, **owner-ref'd to the AgentRun**
   (GC'd with the run). New `launcher` capability: `EnsureSecret`.
4. **Refresh loop:** a goroutine in the apiserver re-mints tokens for
   non-terminal runs at ~45 min and updates the Secret in place (volume refresh
   does the rest). On apiserver restart the loop re-derives its worklist from
   the store (WS-3 makes that durable).
5. **Setup docs:** `SETUP.md` gains "create the GitHub App" (permissions,
   install on org/repos, drop App ID + key into the Secret); PAT demoted to the
   quickstart-only path.

**Validation:** unit (fake App API already exists); e2e-pr against gitea stays
on the PAT path; **one live validation** against a real GitHub App on kind
(needs you to create the App — human-gated step).

**Files:** `api/v1alpha1/agentrun_types.go`, `internal/controller/pod.go`,
`internal/egress/auth.go`, `internal/podruntime`, `internal/github/app.go`,
`internal/coreapi/service.go`, `internal/launcher`, `cmd/wren-apiserver`,
`config/apiserver/`, `SETUP.md`.

---

## WS-3 — Postgres store — **size M, fully parallel** (touches only `internal/store` + apiserver wiring)

**Design**

- `internal/store/postgres.go` on **pgx/v5**. Two tables (`projects`, `runs`),
  `text[]`/JSONB for the allowlist, `updated_at` triggers. Migrations embedded
  via `//go:embed` + a tiny in-code migrator (two tables don't justify a
  framework; revisit at v0.3).
- Selection in `cmd/wren-apiserver`: `--store=memory|postgres` +
  `DATABASE_URL`. Memory stays the default for dev/tests — the quickstart can
  run memory-backed with a documented caveat until the Helm chart (WS-5) makes
  Postgres one flag away.
- Tests: the existing `store` suite becomes a conformance suite run against
  both implementations; Postgres via testcontainers-go, skipped unless
  `STORE_TEST_DSN`/Docker present; CI runs it with a service container.
- **Reconcile-on-boot:** on start, the apiserver lists AgentRun CRs and
  reconciles store rows (the CR is already the source of truth for status —
  this closes the "restarted apiserver forgets runs" hole even mid-migration).

**Acceptance:** kill/restart the apiserver mid-run on kind; `wren run get`
still returns the run with correct phase.

---

## WS-4 — `wren run logs` — **size M, fully parallel** (apiserver/launcher/cli/client)

- `launcher.Launcher` gains `StreamLogs(ctx, namespace, runID, container string,
  follow bool) (io.ReadCloser, error)`: resolve the current pod via the
  `wren.dev/run` label (pod name embeds restartCount — don't reconstruct it),
  default container `harness`, use a kubernetes clientset (`pods/log`
  subresource — add to RBAC roles in `config/rbac` and
  `config/apiserver/role.yaml`).
- apiserver: `GET /v1/runs/{id}/logs?follow=&container=` — plaintext chunked
  stream (curl-friendly; SSE adds nothing here), flush-per-line.
- CLI: `wren run logs <id> [-f] [--container]`; client streams to stdout.
- Nice failure modes: run exists but pod gone → last known phase + hint;
  Pending → "no pod yet".

**Acceptance:** during `make e2e`, `wren run logs -f` tails the mock harness
event stream live.

---

## WS-5 — Helm chart — **size M, after WS-1/WS-2 manifests settle**

- `charts/wren/`: CRDs in `crds/` (install-once semantics), templates for
  operator + apiserver Deployments, Service, SAs, RBAC, optional netpol
  (WS-1); values: images/tags, `github.pat.secretName` | `github.app.*`,
  `anthropic.secretName`, `store.type`/`store.dsnSecret`, `egressEnforcement`.
- Keep kustomize under `config/` for contributors; chart is the user-facing
  install. CI: `helm lint` + `ct install` against kind; publish OCI chart to
  GHCR on tag.
- `SETUP.md` rewritten around `helm install wren ...`.

---

## WS-6 — Quickstart + release engineering — **size L, after WS-5**

- **`wren quickstart`** (new `internal/cli/quickstart.go`): preflight
  (docker/kind/kubectl/helm) → kind cluster → images (pull GHCR release, or
  `--build` for dev) → `helm install` → secrets from `ANTHROPIC_API_KEY` +
  `gh auth token` (prompt if absent) → register a demo project → submit a demo
  task → poll → print the PR URL. `--teardown` reverses everything.
  This *replaces* the env-var incantation as the documented path;
  `hack/setup.sh` remains for CI/dev.
- **goreleaser:** `wren` binaries (darwin/linux × amd64/arm64), Homebrew tap,
  checksums + cosign signing, SBOM; buildx workflows publish multi-arch
  `operator`/`apiserver`/`runtime`/`claude-code` images to GHCR (the amd64
  cross-compile+COPY lesson from GKE testing is already the Dockerfile
  pattern — keep it).
- **Demo assets:** vhs tape → README GIF; `<org>/demo-app` example repo with
  seeded good-demo issues.

**Acceptance (the launch bar):** stranger-test — someone who isn't you, on a
clean Mac, `brew install` → PR in <10 minutes, no repo clone.

---

## WS-7 — CI + community files — **size S–M, fully parallel, start immediately**

- `.github/workflows/ci.yml`: `go build ./...`, `go test -race ./...`,
  `golangci-lint`, `govulncheck`, `gofmt` check — on every PR. Keep it <5 min.
- `e2e.yml`: WS-0's kind job (per-PR); `e2e-pr` nightly.
- `codeql.yml`, trivy image scan on release builds.
- Community: `CONTRIBUTING.md` (distilled from AGENTS.md — which stays, it's
  the deep guide and agent-contributors are on-brand), `CODE_OF_CONDUCT.md`
  (Contributor Covenant), `SECURITY.md` (threat model from spec §9 + residual
  risks + disclosure contact), issue/PR templates, DCO check, branch
  protection once CI is stable.

---

## WS-8 — Claims truthing & checkpointing de-scope — **size S**

Decision (recommended): **v0.1 resumes via PVC re-attach only**; the GCS/S3
checkpointer ships post-launch behind the `BlobStore` interface.

- Update README/spec/SECURITY.md: crash-resume = PVC survives → reattach;
  node/zone loss without checkpoint = run fails cleanly with diagnostics.
- The checkpointer sidecar stays (harmless stub, keeps the pod shape stable)
  but is labeled experimental; `checkpoint.bucket` documented as no-op v0.1.
- Define `internal/blob.Store` interface now (S3-compatible + GCS impls later,
  MinIO in e2e) so the post-launch work has a socket to plug into — interface
  only, no implementation pre-launch.

---

## WS-9 — Docs site — **size M, after WS-1/WS-8 (claims must be true first)**

mkdocs-material, deployed to GitHub Pages on tag: Quickstart · Concepts
(Run/Project/Harness/Pool, lifecycle state machine) · **Security model** (the
deep-dive page — this is the marketing) · Production install (Helm; GKE
profile with the de-GCP'd framing from `oss-plan.md` Phase 3) · Writing a
harness (the RunSpec/event/exit-code contract) · CLI + HTTP API reference ·
ADRs (from Phase 1 of the launch plan). The existing spec remains the
internal design doc; the site is user-facing and smaller.

---

## WS-10 — Rename + public repo cut — **size M, strictly last, human-gated**

Blocked on your decisions: name, org, license (Apache-2.0 recommended).

1. `hack/rename.sh` (write it early, run it last): module path, import paths,
   CRD group `wren.dev` → `<name>.dev` (+ `make manifests generate`), labels
   (`wren.dev/run`), branch prefix (`wren/`), binary/image names, env-var
   prefixes (`WREN_*`), Helm chart name, docs. One mechanical commit,
   `make test e2e` green after.
2. Fresh repo in the new org; copy the tree at HEAD (no history — the archive
   stays private in `a-labs`); clean `.gitignore`; Apache-2.0 + NOTICE;
   curated initial commit(s).
3. Gate: `gitleaks` on the final tree; full CI green in the new repo; the
   stranger-test quickstart against the *public* artifacts.
4. Flip public alongside the Phase-7 launch checklist in `oss-plan.md`.

---

## 11. Dependency graph & batch schedule

```
WS-0 e2e loop ──────────────┬──────────────────────────────► (gate for all)
                            │
     Batch 1 (parallel):    ├─ WS-1 egress enforcement (pod.go owner)
                            ├─ WS-3 postgres store
                            ├─ WS-4 run logs
                            └─ WS-7 CI + community
                            │
     Batch 2 (parallel):    ├─ WS-2 github app   (after WS-1 frees pod.go)
                            ├─ WS-5 helm chart   (after WS-1 manifests settle)
                            └─ WS-8 claims truthing (after WS-1 outcome known)
                            │
     Batch 3 (parallel):    ├─ WS-6 quickstart + releases (needs WS-5)
                            └─ WS-9 docs site            (needs WS-2/WS-8)
                            │
     Final (serial):        └─ WS-10 rename + public cut (needs everything;
                                blocked on name/org/license decisions)
```

Indicative calendar with the parallel-agent workflow
([`agent-workflow.md`](agent-workflow.md)): **Week 1** WS-0 + Batch 1 ·
**Week 2** finish Batch 1, run Batch 2 · **Week 3** Batch 3 · **Week 4**
WS-10 + launch prep (external security read, stranger test, launch post).
Solo-serial this is ~7–8 weeks; the batches are what compress it to ~4.

**Human-gated items to schedule early** (agents can't do these): pick
name/org/license (WS-10 inputs, needed by week 3); create the GitHub App
(WS-2 live test); `gcloud auth login` for the one pre-launch GKE validation
(WS-1); recruit one stranger for the quickstart test; external security
reviewer for the egress path.

## 12. Hot-file ownership map

| File | Workstreams | Rule |
|---|---|---|
| `internal/controller/pod.go` | WS-1 → WS-2 | strictly sequential, in that order |
| `api/v1alpha1/agentrun_types.go` | WS-2 | interface-freeze PR lands alone first |
| `cmd/wren-apiserver/main.go` | WS-2, WS-3, WS-4 | each adds a flag/wire — rebase-and-merge in merge order, conflicts are trivial |
| `Makefile`, `hack/` | WS-0, WS-5, WS-6 | WS-0 owns first; later additions append-only |
| `README.md` / spec status | all | orchestrator merges these hunks; workers write their status update in the PR description if the file is contended |
