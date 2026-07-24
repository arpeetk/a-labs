# Setting up Wren

How a team stands up Wren on an **existing** Kubernetes cluster and how its
engineers get from zero to a running agent in minutes. Install is product
surface: it lives in the CLI as `wren install` — not in scripts.

The design behind this is [`docs/technical-spec.md`](docs/technical-spec.md)
§5, §7, §11; contributor conventions are in [`AGENTS.md`](AGENTS.md).

## What you need

| Thing | Why |
|---|---|
| A Kubernetes cluster (≥ 1.27) + `kubectl` access | runs the control plane and the agent pods |
| `docker` (daemon running) | `wren install` builds the control-plane images + the harness images |
| A **GitHub token** (PAT, or just `gh` logged in) | agents push branches + open PRs |
| An **Anthropic API key** | the Claude agent does the work |
| For GKE: `gcloud` + an Artifact Registry repo | cluster auth + image publishing |
| The `wren` CLI | see [Getting the CLI](#getting-the-cli) |

## The three identities (mental model)

Wren juggles three separate credentials — knowing which is which removes all the
confusion:

1. **You → the cluster.** Your `kubectl` context (for GKE: `gcloud container
   clusters get-credentials`). Used once, by `wren install`.
2. **Agents → GitHub + the model.** The GitHub token and Anthropic key, stored
   as Kubernetes Secrets and read **only by the egress-proxy sidecar** — never
   the agent container. The agent routes through the proxy, which injects them,
   so a compromised agent never sees a raw credential.
3. **The control plane → the cluster.** The operator + apiserver run in-cluster
   (Deployments in `wren-system`) with their own ServiceAccounts (RBAC shipped
   in `config/`).

## GKE (headline path)

```sh
# 1. cluster access (your identity → the cluster)
gcloud container clusters get-credentials wren --zone us-central1-a --project my-proj

# 2. docker can push to Artifact Registry (once per machine)
gcloud auth configure-docker us-central1-docker.pkg.dev

# 3. install: preflight → apply CRDs/RBAC/Deployments → build+push linux/amd64
#    control-plane images + harness images (claude-code, codex, opencode by
#    default) → store credentials as proxy Secrets → wait for Ready
GITHUB_TOKEN=$(gh auth token) ANTHROPIC_API_KEY=sk-ant-... \
  wren install --registry us-central1-docker.pkg.dev/my-proj/wren
```

`wren install` is idempotent — re-run it to rotate credentials or re-push
images. Without the env vars it falls back to `gh auth token` and then asks
interactively (input is never echoed); `--skip-credentials` installs keyless
(mock harness works; claude-code runs and PRs need the Secrets).

By default `wren install` builds/pushes **all** harness images
(`claude-code`, `codex`, `opencode`) so any of them is ready to use
immediately — pass `--harness-images=claude-code,codex` to restrict the set,
or `--harness-images=none` to skip harness images entirely (see
[docs/harnesses.md](docs/harnesses.md)).

> **GKE note:** grant the node service account `roles/artifactregistry.reader`
> so pods can pull from your registry (`<projnum>-compute@developer...`), or the
> images will `ImagePullBackOff`.

For a **team setup** add `--expose=LoadBalancer` to give the apiserver a stable
address; without it the control plane is reached by port-forward (below). The
apiserver's only auth today is a trusted `X-Wren-User` header (M0 stand-in;
SSO/OIDC is a later milestone) — keep it on a trusted network either way.

## kind (local eval path)

Same flow against a throwaway local cluster — images are built and `kind
load`ed, nothing is pushed:

```sh
wren install --kind wren-eval --skip-credentials
```

(Use any cluster name; omit `--skip-credentials` to also store real
credentials in the local cluster.)

## Engineer onboarding

Once install prints "Wren control plane is Ready", each engineer:

```sh
# 1. reach the control plane (skip with --expose=LoadBalancer — use <ip>:8090)
kubectl --context <cluster-context> -n wren-system port-forward svc/wren-apiserver 8090:8090 &

# 2. log in (identity is a trusted header for now — see the M0 note above)
wren login --control-plane localhost:8090 --user you@corp.com

# 3. register a project. Harness (claude-code), model, cpu/memory/disk and the
#    run namespace all take control-plane defaults — install already pointed the
#    default namespace at where it stored the credential Secrets. On a registry
#    install the project still names the pushed harness image (the built-in
#    default wren/claude-code:dev is only present on kind).
wren project create payments-api \
  --repo acme/payments-api \
  --harness-image us-central1-docker.pkg.dev/my-proj/wren/claude-code:<tag>
# (on a `--kind` install, even simpler: `wren project create demo --repo owner/repo`)

# 4. submit a task — the agent clones, does the work, opens a PR
wren run create --project payments-api --task "Add input validation to the signup endpoint"
wren run get <run-id>        # → Succeeded, with the PR URL
```

Under the hood: apiserver → `AgentRun` CR → operator schedules a hardened pod →
egress-proxy (holds the creds) → hydrate clones the repo → **Claude Code runs
the task and edits files** → finalize commits + pushes + opens the PR → status
flows back to `run get`.

> A project with no `--repo` is **keyless**: runs skip the clone and the PR —
> pair it with `--harness mock` for a zero-credential smoke test of the whole
> pipeline (this is what `make e2e` drives).

### Using codex or opencode instead of claude-code

`wren install` builds all three harness images by default (see above), so
lighting up a non-default harness for a project is just pointing it at the
image `install` already pushed — no separate build step:

```sh
# install already pushed .../wren/{claude-code,codex,opencode}:<tag> —
# find <tag> from the install output, or `git rev-parse --short HEAD` if you
# didn't pass --tag.
wren project create payments-api-codex \
  --repo acme/payments-api --harness codex \
  --harness-image us-central1-docker.pkg.dev/my-proj/wren/codex:<tag>

wren run create --project payments-api-codex --task "Add input validation to the signup endpoint"
```

Swap `codex`/`OPENAI_API_KEY` for `opencode` the same way (opencode rides the
Anthropic route, so it reuses the same `wren-anthropic-key` Secret `wren
install` already wrote — no extra credential needed). Codex/opencode are
**not yet validated against the live providers** in CI — see
[docs/harnesses.md](docs/harnesses.md) for what's tested (command
construction, event parsing, credential wiring) versus what still needs a
live-key smoke run.

## Uninstall

```sh
wren uninstall --kube-context <cluster-context> --confirm
```

Removes the `wren-system` + run namespaces and the Wren CRDs (every AgentRun
goes with them — hence the confirmation gate).

## Getting the CLI

Releases are cut from tags on this repo (private — `gh` auth required):

```sh
gh release download --repo arpeetk/a-labs <tag>   # wren_<tag>_<os>_<arch>.tar.gz + checksums
```

or build from source: `make build` → `./bin/wren` (Go 1.26+; see the PATH note
in [`AGENTS.md`](AGENTS.md)). Release tags also publish the control-plane
images to `ghcr.io/arpeetk/wren/{runtime,operator,apiserver}` — pass
`--registry ghcr.io/arpeetk/wren --tag <tag>` to `wren install` to use them
instead of building locally.

## Later milestones (not yet built)

- **GitHub App** instead of a PAT: per-run, repo-scoped installation tokens
  minted by the control plane and injected at the proxy (the minter exists in
  `internal/github`; wiring is WS-2).
- **SSO/OIDC** for the apiserver front-door (replacing the `X-Wren-User`
  header) and managed **Postgres** provisioning + a **Helm chart** (WS-5).
- **Workload Identity** for the operator/pods → GCP.
- **Roadmap CLI surface** (deliberately not shipped yet — the CLI lists only
  commands that work, so these are absent rather than stubbed): `wren run
  attach` / `wren run steer` (interactive steering), `wren run resume` (manual
  re-run of a terminally-Failed run — the operator already auto-resumes
  *infrastructure* crashes; a manual trigger that resets the retry budget and
  clears the leftover Failed pod is a real feature, deferred), `wren fleet`
  (cross-run dashboard), `wren usage` (token/cost/compute reporting), `wren mcp
  add|list|test` (per-project MCP servers), `wren project config` (editing
  defaults/rubric/egress in place). Each is trivial to re-add once its server
  side lands.
- **Sandbox runtimes** `gvisor` / `kata` for `wren run create --runtime`: wired
  end-to-end in the operator but not provisioned by any v1 cluster, so the CLI
  rejects them today (only `runc` works) until M4.
