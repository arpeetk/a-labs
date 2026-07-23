# AGENTS.md — Working guide for Wren

This file tells a coding agent (or a new engineer) how to work in this repo: the
layout, how to build/test, and the standards to follow. Read it before making
changes. It complements the design doc at [`docs/technical-spec.md`](docs/technical-spec.md)
— keep both current as you work.

**Wren** is the backbone of an internal *Software Factory*: a CLI + GCP/Kubernetes
control plane that runs parallel, durable, sandboxed coding agents in the cloud.
A run is submitted, executes in a hardened pod, survives crashes, and opens a PR.

---

## 1. Toolchain & prerequisites

| Tool | Version | Notes |
|---|---|---|
| Go | **1.26+** | ⚠️ see PATH gotcha below |
| Docker | any recent | for building the runtime image + kind |
| kind | v0.32+ | local Kubernetes for e2e |
| kubectl | 1.27+ | talk to the cluster |
| gh | any | GitHub auth for live-PR testing |
| controller-gen | pinned via `go run` | codegen; no install needed |

> **PATH gotcha (important):** a stale Go 1.17 lives at `/usr/local/go/bin/go` and
> **shadows** the usable Homebrew Go 1.26 at `/opt/homebrew/bin/go`. Prefix every
> Go/build command with:
>
> ```sh
> export PATH="/opt/homebrew/bin:$PATH"
> ```
>
> (Or fix your profile so `/opt/homebrew/bin` precedes `/usr/local/go/bin`.)

> **zsh gotcha:** zsh does **not** word-split unquoted variables, so
> `CMD="kubectl ..."; $CMD` fails. Inline commands instead of storing them in a var.

**Module path:** `github.com/summiteight/wren` (the project identity). The repo is
hosted at `arpeetk/a-labs`; the module path intentionally differs and is not
`go get`-able externally. All imports use the `github.com/summiteight/wren/...`
prefix.

---

## 2. Repository layout

```
api/v1alpha1/          CRDs: AgentRun, AgentPool (types + generated deepcopy)
cmd/
  wren/                CLI entrypoint
  wren-apiserver/      control-plane HTTP API server
  wren-operator/       Kubernetes controller (controller-runtime manager)
  wren-runtime/        multi-call in-pod binary (harness + sidecars)
internal/
  cli/                 cobra command tree
  client/              CLI → control-plane HTTP client
  config/              CLI config (~/.config/wren/config.yaml)
  apiserver/           HTTP/JSON handlers (spec §5.2 REST mapping)
  coreapi/             control-plane business logic (Runs + Projects services)
  store/               persistence: Store interface + in-memory impl
  launcher/            creates AgentRun CRs (Launcher interface + K8s impl + Fake)
  controller/          AgentRun/AgentPool reconcilers + pod builder
  runspec/             the RunSpec contract handed to a harness
  blob/                object-store contract for checkpoints (interface only — no impls yet)
  harness/             harness adapters (mock, claude-code) + event protocol
  podruntime/          in-pod role runners (harness/hydrate/sidecars) + dispatch
  github/              GitHub PR client + App installation-token minter + Fake
  gitwork/             go-git clone/commit/push (no git binary needed)
  finalize/            commit → push branch → open PR (+ rubric)
config/                kustomize manifests (crd, rbac, manager) + samples
build/                 Dockerfile.runtime
docs/technical-spec.md the living design spec (Draft v0.2)
```

**Component flow (Journey A):**
`wren CLI` → `wren-apiserver` (coreapi/store/launcher) → creates an `AgentRun` CR
→ `wren-operator` reconciles → hardened pod (`wren-runtime`: hydrate → harness →
finalize) → opens a PR → status mirrored back to the CLI.

---

## 3. Build

```sh
export PATH="/opt/homebrew/bin:$PATH"
make build            # ./bin/wren            (CLI)
make build-operator   # ./bin/wren-operator
make build-apiserver  # ./bin/wren-apiserver
make build-runtime    # ./bin/wren-runtime
make docker-runtime   # wren/runtime:dev image
```

## 4. Test & coverage

```sh
make test             # go test ./...
make cover            # go test -cover ./...
make vet              # go vet ./...
make fmt              # gofmt -w .
```

**Coverage bar:** keep coverage **high** on every logic package, and ship tests in
the *same change* as new code — do not defer. Typical numbers: store ~97, config
~91, finalize ~88, coreapi/api/controller ~87, apiserver/launcher ~83, github ~79,
gitwork/harness/podruntime ~72–74. `cmd/*` `main` wiring and real-network glue
(real GitHub client, real `claude` CLI) are the only intentionally-uncovered spots
— call those out explicitly if you add more.

## 5. Code generation (when you change `api/v1alpha1`)

```sh
make generate    # regenerate zz_generated.deepcopy.go
make manifests   # regenerate config/crd/bases + config/rbac
```

Adding a plain scalar field does not strictly require `generate` (shallow copy
covers it), but **always run `manifests`** so the CRD schema accepts the field,
and re-apply the CRD to any running cluster (`kubectl apply -f config/crd/bases/`).

---

## 6. Conventions & standards

- **Standards (read these):** [`docs/standards/testing.md`](docs/standards/testing.md),
  [`docs/standards/code.md`](docs/standards/code.md),
  [`docs/standards/review.md`](docs/standards/review.md) — the repo's testing,
  code, and review rules, each with the incident that taught it.
- **Interface + real impl + fake.** External dependencies (Kubernetes, GitHub,
  the store) are behind a small interface, with a real implementation and an
  in-memory `Fake`/`Memory` for tests. Business logic depends on the interface,
  never the concrete client. See `store`, `launcher`, `github`.
- **Hermetic tests.** Prefer tests with no network: controller-runtime `fake`
  client for reconcilers, `httptest` for HTTP and mocked GitHub APIs, local bare
  git repos for `gitwork`/`finalize`. Inject `now`/`idgen`/clients via unexported
  seams (see `coreapi.Service`, `podruntime.newGitHubClient`).
- **Errors:** wrap with `%w` and context (`fmt.Errorf("do x: %w", err)`); export
  sentinel errors (`ErrNotFound`, `ErrValidation`, `ErrNoChanges`) and map them at
  the transport boundary (`apiserver.writeServiceErr`).
- **Security posture (do not regress):** the agent runner is untrusted. Pods are
  hardened (non-root, read-only rootfs, dropped caps, seccomp, no SA token). The
  runner must hold **no long-lived secrets** in the target design — the current
  `GITHUB_TOKEN`-in-env is a labelled **M0 stand-in** for egress-proxy injection.
- **Comments** explain *why*, not *what*; match the surrounding density. Reference
  the spec section a piece implements (e.g. "spec §5.7").
- **Definition of done:** `gofmt` clean, `go vet` clean, `go test ./...` green,
  new code covered, and the spec's living "Implementation status" block updated if
  behavior/scope changed.

---

## 7. Local end-to-end on kind

```sh
export PATH="/opt/homebrew/bin:$PATH"

# 1. cluster + CRDs
kind create cluster --name wren-test
kubectl --context kind-wren-test apply -f config/crd/bases/

# 2. runtime image
make docker-runtime
kind load docker-image wren/runtime:dev --name wren-test

# 3. operator (against the kind context)
make build-operator
kubectl config use-context kind-wren-test
./bin/wren-operator --leader-elect=false --health-probe-bind-address=:8081 --metrics-bind-address=:8082 &

# 4. control plane
make build-apiserver
./bin/wren-apiserver --addr :8090 &

# 5. (optional) real PR — inject a GitHub token as a Secret in the run namespace
#    kubectl create secret generic wren-github-token -n <ns> --from-literal=token="$(gh auth token)"

# 6. drive it
make build
curl -s -X POST localhost:8090/v1/projects -H 'X-Wren-User: admin' \
  -d '{"name":"demo","repo":"owner/repo","harnessImage":"wren/runtime:dev","cpu":"200m","memory":"256Mi","disk":"1Gi"}'
./bin/wren login --control-plane localhost:8090 --user you
./bin/wren run create --project demo --task "..."
./bin/wren run get <run-id>

# teardown
kind delete cluster --name wren-test
```

Without a `GITHUB_TOKEN` Secret the run still reaches `Succeeded` (finalize skips
the PR). With one, it opens a real PR.

### Testing

Unit tests: `make test vet` (see §4). For the full loop, `make e2e` is the
**keyless end-to-end gate** — the objective merge check every workstream rides:

```sh
export PATH="/opt/homebrew/bin:$PATH"
make e2e                 # kind cluster → build+load images → deploy control plane
                         # → keyless mock run → assert Succeeded → teardown
E2E_KEEP=1 make e2e      # keep the cluster + control plane up for debugging
E2E_BAD_IMAGE=1 make e2e # failure-path demo: bad runtime image → log dump, non-zero exit
```

It needs **Docker + kind** and runs in <10 min with **zero credentials**. It
registers a **repo-less** project through the deployed apiserver and submits the
run via the `wren` CLI (`login` → `run create` → poll `run get`), so the gate
drives the real path CLI → apiserver → operator. With no repo the run carries an
empty `RunSpec.Repo`, so hydrate's clone and finalize's PR are both skipped (the
keyless design). It is idempotent (creates or reuses the `${KIND_CLUSTER:-wren-e2e}`
cluster; uses a throwaway `WREN_CONFIG_DIR` so it never touches your real CLI
config) and, on failure, dumps the operator/apiserver logs, the AgentRun YAML, and
every agent-pod container's logs before exiting non-zero.

The egress-proxy's credentialed upstreams are env-overridable (`WREN_GITHUB_UPSTREAM`,
`WREN_GITHUB_API_UPSTREAM`, `WREN_ANTHROPIC_UPSTREAM`; default to the real
endpoints) — the enabler for a later gitea-backed `e2e-pr` tier that asserts a
real PR without touching github.com.

---

## 8. Status & M0 stand-ins (things deliberately not "real" yet)

- **Harness:** the **mock** adapter (deterministic, no key) is the default; the
  real Claude Code adapter needs `ANTHROPIC_API_KEY` + the egress path.
- **Egress-proxy:** real — enforces the allowlist and injects github/anthropic
  credentials (`internal/egress`); the runner holds no token. Bypass is
  **enforced** (WS-1): an `egress-lockdown` init container iptables-rejects all
  runner egress except via the proxy's uid (runner/proxy uids are pinned in the
  pod spec; a startup canary proves it per run). `--egress-enforcement=off` is
  the escape hatch for clusters that forbid privileged init containers (e.g.
  GKE Autopilot) — `config/netpol/` has a weaker NetworkPolicy layer for that
  path. Residual: a runc escape to the node (gVisor/Kata, M4).
  **checkpointer** is an experimental liveness stub (no snapshots; crash-resume
  is PVC reattach + resume-mode — spec §5.5 v0.1); **gateway** is still a
  liveness stand-in (run results reach status via the operator's pods/log
  scrape, WS-11; the event bridge is the v0.2 target).
- **Transport:** control-plane API is HTTP/JSON (target: gRPC + Connect).
- **Store:** in-memory (default) **or** Postgres (`--store=postgres` + `DATABASE_URL`, `internal/store.Postgres`; pgx/v5, embedded migrations, reconcile-on-boot). Managed Cloud SQL provisioning is the remaining target (WS-5 Helm).
- **Auth:** `X-Wren-User` header (target: OIDC/SSO).
- **Kernel isolation:** `runc` (gVisor/Kata deferred to M4).

When you make one of these real, remove its stand-in note here and in the spec.
