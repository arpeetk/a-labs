# Wren

**Wren is the backbone of an internal Software Factory** — a developer-experience
CLI plus the GCP/Kubernetes control plane behind it, so an engineer can spin up
**massively parallel, durable, sandboxed coding agents in the cloud** with one
command.

Submit a task; a coding agent (Claude Code, Codex, or OpenCode) clones the
repo and does the work in a hardened cloud pod, auto-resumes from
infrastructure crashes, and **opens a pull request** — all without the agent
ever holding a credential.

> 📄 **Design & internals:** [`docs/technical-spec.md`](docs/technical-spec.md) ·
> 🛠 **Install / handover:** [`SETUP.md`](SETUP.md) ·
> 🤝 **Contributing:** [`AGENTS.md`](AGENTS.md)

---

## What works today

The core of milestone **M0 — submit a task, get a pull request** — is complete and
**validated end-to-end on both a local `kind` cluster and real GKE**:

```
$ wren run create --project payments-api \
    --task "Add input validation to the signup endpoint"
{ "id": "r-9d4c09a", "project": "payments-api", "phase": "Pending", "harness": "claude-code" }

$ wren run get r-9d4c09a
{ "id": "r-9d4c09a", "phase": "Succeeded",
  "prUrl": "https://github.com/acme/payments-api/pull/128" }
```

A real run: the engineer submits a task → the control plane creates an `AgentRun`
→ the operator schedules a hardened pod → **the real Claude agent explores the
repo and edits files** → the change is committed, pushed, and **a real PR is
opened**. The GitHub token and model API key live only on a trusted egress-proxy
sidecar; **the untrusted agent container holds no secrets**. Infrastructure
crashes (OOM, eviction) auto-resume by recreating the pod and reattaching the
surviving workspace disk; deterministic failures fail fast. A node/zone loss
that destroys the disk ends the run cleanly (`Failed`, with diagnostics) —
object-store checkpoints are post-launch (spec §5.5).

## How a run flows

```mermaid
flowchart LR
    A["👩‍💻 Engineer<br/>wren run create --task …"] --> B["Control plane<br/>(apiserver)"]
    B --> C["Operator<br/>schedules a pod"]
    C --> D["Hardened pod on GKE<br/>Claude agent does the work"]
    D -->|"clone · push · model calls<br/>via egress-proxy (creds injected here)"| E["GitHub<br/>Pull Request"]
    E -.->|"wren run get → PR url"| A
```

The **credential boundary** is the heart of the security model: the agent
(untrusted, running model-generated code) routes all network access through the
in-pod **egress-proxy**, which enforces a domain allowlist and injects the
GitHub/Anthropic credentials on the way out. See the full sequence diagram and
threat model in the [spec](docs/technical-spec.md#25-end-to-end-workflow-journey-a).

## Using Wren (engineer)

```sh
wren login --control-plane wren.corp.internal --user you   # SSO lands in M1
wren run create --project payments-api --task "Fix the flaky retry in checkout"
wren run get    r-9d4c09a          # phase, PR url, restart count (token usage lands with wren usage, M1)
wren run list   --scope mine
wren run logs   r-9d4c09a -f        # tail the agent's live logs (--container to pick a sidecar)
```

Each run is attributable, resumable, and produces a reviewable PR — not a mystery
diff. (`attach`/`steer`, `fleet`, and `usage` are milestone-tagged in the CLI and
land in M1–M2.)

## Installing Wren (admin / handover)

Full runbook in [`SETUP.md`](SETUP.md). Install is a first-class CLI command —
preflight, CRDs/RBAC/Deployments, image build+deliver, credential Secrets, and
a Ready wait in one idempotent step:

```sh
# GKE: build + push linux/amd64 images to your registry, install in-cluster
GITHUB_TOKEN=$(gh auth token) ANTHROPIC_API_KEY=sk-ant-... \
  wren install --registry us-central1-docker.pkg.dev/<proj>/wren

# kind (local eval): build + kind load, zero credentials
wren install --kind wren-eval --skip-credentials
```

The credential Secrets are read only by the egress-proxy (never the runner).
`wren uninstall --confirm` removes the install. Engineers then port-forward,
`wren login`, `wren project create`, `wren run create` — see
[`SETUP.md`](SETUP.md#engineer-onboarding).

## Architecture at a glance

```
 wren CLI ──HTTP──▶ control plane ──creates──▶ AgentRun CR ──watch──▶ operator
                    (Runs/Projects)                                      │
                                                                    schedules
                                                                        ▼
   ┌──────────────────────── hardened agent pod (per run) ─────────────────┐
   │  egress-proxy (creds + allowlist) ◀── harness runner (Claude, no creds)│
   │  + checkpointer (experimental stub) + gateway sidecars                 │
   │  + hydrate init  + workspace PVC                                       │
   └───────────────────────────────────────────────────────────────────────┘
```

- **CLI** talks only to the control plane (never to Kubernetes).
- **Control plane** resolves project config and translates a task into an
  `AgentRun` custom resource.
- **Operator** (controller-runtime) reconciles each `AgentRun` into a hardened
  pod, owns the lifecycle, and auto-resumes infrastructure crashes by
  reattaching the surviving workspace PVC (resume-mode; no checkpoints yet).
- **Agent pod** is the sandbox: one untrusted harness container + trusted
  sidecars (egress-proxy holds the credentials); the runner reaches the internet
  only through the proxy.

Full architecture, domain model, lifecycle state machine, security/threat model,
and module map: [`docs/technical-spec.md`](docs/technical-spec.md).

## Status (built vs. designed)

The spec (§1–§9) describes the **target** design; M0 is the first working slice.

| Area | M0 (as built) | Target |
|---|---|---|
| Task → PR (Journey A) | ✅ real Claude agent → PR, on kind **and** GKE | same |
| Harnesses | ✅ `claude-code` (proven e2e) + `mock` (keyless gate); `codex` + `opencode` adapters, images, and the `/openai/` egress route built — **not yet run against live providers** ([docs/harnesses.md](docs/harnesses.md)) | + BYO conformance suite |
| Crash-resume | ✅ infra crashes (OOM/eviction) resume via PVC reattach + resume-mode; deterministic failures fail fast; a disk-destroying node/zone loss = clean `Failed` | + object-store checkpoints (`workspace.checkpoint.*` accepted, **no-op** until the checkpointer lands post-launch; `internal/blob.Store` is the socket) |
| Egress-proxy | ✅ injects creds (github.com, api.github.com, api.anthropic.com, api.openai.com) + allowlist; runner holds no secret; **bypass enforced** (iptables uid-lockdown + per-run canary; `--egress-enforcement=off` escape hatch with `config/netpol/` FQDN policies) | verify enforcement on GKE Standard (privileged init-container node policy) |
| Control plane | ✅ runs in-cluster (operator + apiserver Deployments, `config/default`; `make e2e` rides them) — local-against-cluster remains the dev loop | published images + Ingress/OIDC front-door |
| GitHub creds | ✅ PAT in the proxy secret | per-run **GitHub App** tokens |
| API transport | HTTP/JSON | gRPC + Connect |
| Store | ✅ in-memory (default, dev) **or** Postgres (`--store=postgres` + `DATABASE_URL`; reconcile-on-boot re-learns in-flight runs) | managed Cloud SQL, Helm-provisioned (WS-5) |
| Auth | `X-Wren-User` header | OIDC / SSO |
| Isolation | hardened `runc` pods | + gVisor/Kata (deferred, M4) |

Next up: per-run **GitHub App** tokens (the minter is built; wiring is next),
verifying egress enforcement on GKE Standard, and the object-store checkpointer
behind `internal/blob.Store` (post-launch).

## Repository layout

```
api/v1alpha1/         CRD: AgentRun (+ generated deepcopy + YAML)
cmd/
  wren/               CLI entrypoint
  wren-apiserver/     control-plane HTTP API
  wren-operator/      Kubernetes operator (controller-runtime manager)
  wren-runtime/       multi-call in-pod binary (harness + sidecars)
internal/
  cli/ client/ config/     CLI command tree, HTTP client, local config
  install/                  wren install/uninstall (embedded config/default render)
  apiserver/ coreapi/       control-plane HTTP handlers + Runs/Projects logic
  store/ launcher/          persistence (in-memory + Postgres) + AgentRun CR creation
  controller/               AgentRun reconciler + pod builder
  harness/ podruntime/      harness adapters (claude-code, codex, opencode, mock) + in-pod roles
  egress/                   the credential-injecting allowlist proxy
  blob/                     object-store Store interface for checkpoints (impls post-launch)
  github/ gitwork/ finalize/  GitHub PR client, go-git ops, commit→push→PR
  runspec/                  the RunSpec contract handed to each harness
build/                Dockerfiles (runtime, claude-code, codex, opencode, generic gobin)
config/               kustomize manifests (crd, rbac, manager) + samples
hack/                 dev/test tooling only (e2e gates) — onboarding lives in the CLI
docs/                 technical specification
```

## Build, test, run

Requires **Go 1.26+** and Docker. (A stale system Go may shadow it — see
[`AGENTS.md`](AGENTS.md) for the PATH note.)

```sh
make build            # -> ./bin/wren
make build-operator   # -> ./bin/wren-operator
make build-apiserver  # -> ./bin/wren-apiserver
make build-runtime    # -> ./bin/wren-runtime

make test             # unit tests (fake k8s client, httptest, local git repos)
make cover            # per-package coverage
make vet fmt tidy
make manifests generate   # regenerate CRD/RBAC YAML + DeepCopy from code
```

For a full local end-to-end (kind + operator + apiserver + a real task), see the
recipe in [`AGENTS.md`](AGENTS.md#7-local-end-to-end-on-kind) and
[`SETUP.md`](SETUP.md).
