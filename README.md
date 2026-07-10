# Wren

**Wren** is the backbone of an internal Software Factory: a developer-experience
CLI plus the GCP/Kubernetes control plane behind it, so engineers can spin up
**massively parallel, durable, sandboxed coding agents in the cloud** with one
command. Submit a task, an agent (Claude Code, Codex, or your own) runs it in a
hardened cloud sandbox, survives crashes and restarts, and opens a pull request.

> Full design: [`docs/technical-spec.md`](docs/technical-spec.md).

## Status

Early development (milestone **M0 — Foundations**). Today the repo contains:

- `cmd/wren` + `internal/cli` — the CLI. `login` and `run create/list/get` are
  wired to the control-plane HTTP API; `project`/`mcp`/`fleet`/`usage` are
  milestone-tagged placeholders.
- `cmd/wren-apiserver` + `internal/{apiserver,coreapi,store,launcher}` — the
  control plane: Runs + Projects services over HTTP/JSON that create `AgentRun`
  CRs and mirror their status back. (M0: in-memory store, `X-Wren-User` auth.)
- `cmd/wren-runtime` + `internal/{harness,podruntime}` — the multi-call in-pod
  binary: the harness runner (event stream + workspace changes) and the M0
  stand-in sidecars. Adapters: `mock` (no key needed) and a `claude-code` stub.
  Image: `build/Dockerfile.runtime`.
- `internal/{github,gitwork,finalize}` — GitHub PR integration: App
  installation-token minting, go-git clone/commit/push, and the finalize step
  (commit → push branch → open PR with a rubric body). Wired into the runtime;
  opens a real PR when a repo + `GITHUB_TOKEN` are present, else skips cleanly.
- `api/v1alpha1` — the `AgentRun` / `AgentPool` CRDs (Go types + generated
  DeepCopy + CRD YAML).
- `cmd/wren-operator` + `internal/controller` — the Kubernetes operator. The
  `AgentRun` reconciler turns a run into a hardened agent pod (single untrusted
  harness container + native-sidecar egress-proxy / checkpointer / gateway, a
  hydrate init container, and a durable workspace PVC), drives the run lifecycle,
  and **auto-resumes on pod failure** (recreates the pod, bumps `restartCount`,
  fails only after the retry budget). `AgentPool` maintains warm pods (skeleton).
- `internal/runspec` — the RunSpec contract the operator hands each harness.
- `internal/config` — local CLI config (`~/.config/wren/config.yaml`).
- `config/` — kustomize manifests (CRDs, RBAC, manager). `kubectl apply -k config/default`.

Verified end-to-end on a local kind cluster: `wren run create` → apiserver →
`AgentRun` CR → operator schedules a pod → `wren run get` shows the mirrored
phase.

Still to come in M0: the harness runner + checkpointer + egress-proxy + hydrate
container images (pods currently ImagePull placeholder images), GCS
checkpointing, GitHub App integration, `run logs`, and a Postgres store.

## Repository layout

```
api/v1alpha1/       CRDs: AgentRun, AgentPool (+ generated deepcopy)
cmd/wren/           CLI entrypoint
cmd/wren-operator/  operator entrypoint (controller-runtime manager)
internal/cli/       cobra command tree
internal/client/    CLI → control-plane transport (stubbed)
internal/config/    local CLI configuration
internal/controller/ AgentRun + AgentPool reconcilers, pod builder
internal/runspec/   harness RunSpec contract
config/             kustomize deployment (crd, rbac, manager)
docs/               technical specification
```

## Operator

```sh
make manifests        # regenerate CRD + RBAC YAML from code
make generate         # regenerate DeepCopy methods
make build-operator   # -> ./bin/wren-operator
kubectl apply -k config/default   # install CRDs + RBAC + manager
```

## Build & run

Requires Go 1.24+.

```sh
make build            # -> ./bin/wren
./bin/wren --help
./bin/wren version
./bin/wren login --control-plane wren.corp.internal:443
./bin/wren run create --project payments-api --task "Add idempotency keys" --interactive
```

## Development

```sh
make test   # unit tests
make vet    # go vet
make fmt    # gofmt
make tidy   # go mod tidy
```
