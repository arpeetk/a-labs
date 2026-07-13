# Setting up Wren (handover guide)

How a new engineer stands up Wren against an **existing** Kubernetes cluster
(GKE or kind) and runs their first agent task. This is the **Phase 1, PAT-first**
flow — the fastest path to a working handover. See the "Phase 2" notes for the
production hardening (GitHub App, in-cluster control plane, SSO).

The design behind this is in [`docs/technical-spec.md`](docs/technical-spec.md)
§5, §11; contributor conventions are in [`AGENTS.md`](AGENTS.md).

## What you need

| Thing | Why |
|---|---|
| A Kubernetes cluster + `kubectl` access | runs the agent pods (GKE or local kind) |
| `docker` | build the runtime + harness images |
| A **GitHub token** (PAT or `gh auth token`) | so agents can push branches + open PRs |
| An **Anthropic API key** | so the Claude agent can do the work |
| `go` (1.26), and for GKE: `gcloud` + a registry | build/publish; cluster auth |

## The three identities (mental model)

Wren juggles three separate credentials — knowing which is which removes all the
confusion:

1. **You → the cluster.** Your `kubectl` context (for GKE: `gcloud container
   clusters get-credentials`). Used once, to install Wren.
2. **Agents → GitHub + the model.** A **GitHub token** and an **Anthropic key**,
   stored as Kubernetes Secrets and mounted **into the egress-proxy sidecar** —
   never the agent container. The runner routes through the proxy, which injects
   them. So a compromised agent never sees a raw credential.
3. **The control plane → the cluster.** In Phase 1 you run the operator +
   apiserver locally against your kube context; in Phase 2 they run in-cluster
   with their own ServiceAccount (RBAC shipped in `config/`).

## One-command setup

```sh
# kind (local):
KIND_CLUSTER=wren-test WREN_NS=user-me \
GITHUB_TOKEN=$(gh auth token) ANTHROPIC_API_KEY=sk-ant-... \
  hack/setup.sh

# GKE (existing cluster + a registry):
GKE_PROJECT=my-proj GKE_CLUSTER=wren GKE_ZONE=us-central1-a \
REGISTRY=us-central1-docker.pkg.dev/my-proj/wren WREN_NS=user-me \
GITHUB_TOKEN=$(gh auth token) ANTHROPIC_API_KEY=sk-ant-... \
  hack/setup.sh
```

`hack/setup.sh` (idempotent) does:
1. **Cluster access** — for GKE, `get-credentials`; otherwise uses your current context.
2. **Images** — builds `runtime` + `claude-code-runner` and either `kind load`s them or pushes to your `REGISTRY`.
3. **Install** — applies the CRDs (`config/crd`) and RBAC (`config/rbac`).
4. **Secrets** — creates `wren-github-token` and `wren-anthropic-key` in `WREN_NS`.
5. Prints how to start the control plane and submit a task.

> **GKE note:** grant the node service account `roles/artifactregistry.reader`
> so pods can pull from your registry (`<projnum>-compute@developer...`), or the
> images will `ImagePullBackOff`.

## Submit a task

```sh
# 1. run the control plane (Phase 1: locally against the cluster)
go run ./cmd/wren-operator  --leader-elect=false --runtime-image=<runtime image from setup>
go run ./cmd/wren-apiserver --addr :8090

# 2. register a project (repo + the claude-code harness image)
curl -s -X POST localhost:8090/v1/projects -H 'X-Wren-User: admin' \
  -d '{"name":"myrepo","repo":"owner/repo","harnessImage":"<claude-code image>",
       "cpu":"500m","memory":"1Gi","disk":"2Gi",
       "egressAllowlist":["github.com","api.github.com"]}'

# 3. submit — the Claude agent clones the repo, does the task, and opens a PR
wren login --control-plane localhost:8090 --user you
wren run create --project myrepo --task "Add input validation to the signup endpoint"
wren run get <run-id>     # → Succeeded, with the PR URL
```

Under the hood: apiserver → `AgentRun` CR → operator schedules a hardened pod →
egress-proxy (holds the creds) → hydrate clones the repo → **Claude Code runs the
task and edits files** → finalize commits + pushes + opens the PR → status flows
back to `run get`.

## Phase 2 (production hardening — not yet built)

- **GitHub App** instead of a PAT: per-run, repo-scoped installation tokens minted
  by the control plane and injected at the proxy (the minter exists in
  `internal/github`; wiring is pending).
- **In-cluster control plane**: operator + apiserver as Deployments (needs their
  images published + an apiserver Service/Ingress).
- **Workload Identity** for the operator/pods → GCP; **SSO/OIDC** for `wren login`.
- **Egress bypass enforcement**: NetworkPolicy / iptables so the runner *cannot*
  skip the proxy (today it cooperatively routes through it).
