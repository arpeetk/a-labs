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

## GKE quickstart — provision the cluster too (`--create-cluster`)

The headline GKE path above assumes you already have a cluster. For a fast
eval — or a genuinely brand-new GCP project — `--create-cluster` provisions
one for you in a single command, the cloud equivalent of `--kind`:

```sh
GITHUB_TOKEN=$(gh auth token) ANTHROPIC_API_KEY=sk-ant-... \
  wren install --create-cluster \
    --gcp-project my-proj \
    --registry us-central1-docker.pkg.dev/my-proj/wren
```

This enables the `container` + `artifactregistry` APIs, creates a **GKE
Standard** cluster (defaults: zone `us-central1-a`, name `wren`, one
`e2-standard-4` node — override with `--gcp-zone`/`--gcp-cluster-name`/
`--gcp-machine-type`/`--gcp-num-nodes`; `e2-standard-2` looks like the cheaper
eval choice but doesn't leave enough allocatable CPU past GKE's own system
pods for even one default 2-CPU run to schedule — found live, not guessed),
fetches its credentials, wires docker
→ Artifact Registry auth, and grants the node service account
`roles/artifactregistry.reader` — so the `ImagePullBackOff` gap the GKE note
above warns about simply can't happen. It then runs the normal install into
the new cluster. Re-running is idempotent (an existing cluster of that name is
reused, not recreated).

> **This creates real, billable Google Cloud infrastructure.** `--create-cluster`
> is a quickstart/eval convenience, **not** the recommended path for a real
> team — sizing, region, and cost are decisions a team should usually make
> deliberately, so the bring-your-own-cluster flow above stays the headline.
> **GKE Standard only:** Autopilot is unsupported because it forbids the
> privileged init container Wren's egress lockdown (WS-1) needs. Preflight
> requires `gcloud` on `PATH` and an active account (`gcloud auth login`).
> Tear the cluster down when you're done — `wren uninstall --delete-cluster
> --gcp-project my-proj --confirm` (still gated by `--confirm`; deletes the
> namespaces/CRDs first, then the GKE cluster itself), or by hand:
> `gcloud container clusters delete wren --zone us-central1-a --project my-proj`.

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

## Checkpoint and restore (GCS; WS-18–WS-21)

Off by default and independent of the core task→PR loop. This mounts a run's
checkpoint bucket into the **checkpointer container only** (never the untrusted
harness) via GKE's Cloud Storage FUSE CSI driver, and exposes it to Go as a
`blob.Store` of plain files. The sidecar performs a startup mount self-check,
then writes full-workspace tar.gz snapshots at `intervalSeconds` (120 seconds
by default). If the live PVC is later confirmed lost, the controller provisions
a new PVC and hydrate restores the newest snapshot before starting the harness.

Prerequisites on the cluster and project (all one-time):

```sh
PROJECT=my-proj ; ZONE=us-central1-a ; CLUSTER=my-cluster
BUCKET=my-checkpoint-bucket ; NS=<run-namespace>
GSA=wren-ckpt-gsa@$PROJECT.iam.gserviceaccount.com

# 1. Enable the Cloud Storage FUSE CSI driver addon (not enabled by
#    --create-cluster; this feature is opt-in). The cluster must also have
#    Workload Identity enabled (--workload-pool=$PROJECT.svc.id.goog — set at
#    creation, or `clusters update` to add it).
gcloud container clusters update $CLUSTER --zone $ZONE --project $PROJECT \
  --update-addons GcsFuseCsiDriver=ENABLED

# 2. Bucket + a dedicated GCP SA with objectAdmin ON THE BUCKET ONLY (least
#    privilege — a bucket IAM binding, never a project-wide role).
gcloud storage buckets create gs://$BUCKET --project $PROJECT --location <region> \
  --uniform-bucket-level-access
gcloud iam service-accounts create wren-ckpt-gsa --project $PROJECT
gcloud storage buckets add-iam-policy-binding gs://$BUCKET \
  --member="serviceAccount:$GSA" --role="roles/storage.objectAdmin"

# 3. A dedicated Kubernetes SA (default name `wren-checkpointer`) used ONLY by
#    pods that enable the mount, annotated with the GSA, plus the Workload
#    Identity binding that lets it impersonate the GSA.
kubectl create namespace $NS 2>/dev/null || true
kubectl create serviceaccount wren-checkpointer -n $NS
kubectl annotate serviceaccount wren-checkpointer -n $NS \
  iam.gke.io/gcp-service-account=$GSA
gcloud iam service-accounts add-iam-policy-binding $GSA --project $PROJECT \
  --role roles/iam.workloadIdentityUser \
  --member "serviceAccount:$PROJECT.svc.id.goog[$NS/wren-checkpointer]"
```

Then enable the operator flag and point a project at the bucket:

```sh
# operator: --checkpoint-gcs-mount (default off); --checkpoint-ksa overrides the
# KSA name (default wren-checkpointer). The mount is added to a run's pod only
# when this flag is on AND the run's project sets a checkpoint bucket.
wren-operator --checkpoint-gcs-mount ...

# give the project a checkpoint bucket, then run:
wren project create demo --repo owner/repo --checkpoint-bucket gs://$BUCKET
wren run create --project demo --task "..."
```

The checkpointer container's logs will show
`checkpointer: mount self-check PASSED — wrote+read+listed
_wren-mount-check/<run-id>.txt ...` followed by periodic
`checkpoint snapshot PASSED` events. Objects are visible in the bucket:

```sh
gcloud storage cat gs://$BUCKET/_wren-mount-check/<run-id>.txt
```

For a single-node kind/dev recovery test, the operator also accepts
`--checkpoint-local-path=/absolute/existing/node/path`. This uses a Kubernetes
`hostPath` only in the trusted checkpointer (write) and hydrate (read) containers.
It is deliberately not a production backend: the data disappears with that
node and does not protect a multi-node cluster from node loss.

Current checkpoint limitations are explicit: snapshots are full archives and
interval-triggered only. Incremental/git-aware bundles, retention/garbage
collection, `checkpoint_hint` snapshots, and a final SIGTERM flush are not yet
implemented, so worst-case workspace loss is approximately one interval.

**Trust boundary:** the CSI volume and the bucket-scoped credential live on the
checkpointer sidecar only. The harness — which runs untrusted model-generated
code — never gets the volume mount (pinned in the pod builder and asserted by
`TestBuildAgentPod_GCSMount_HarnessNeverMounts`), the same trust-tier split as
the egress-proxy/runner uid boundary.

**Egress enforcement (WS-19):** the mount now works under the **default**
`--egress-enforcement=iptables` — `off` is no longer required. The GKE-injected
`gke-gcsfuse-sidecar` runs as its own uid (65534 on GKE 1.35 / CSI driver
v1.22.16) and talks directly to Cloud Storage + the metadata server, so the WS-1
lockdown (which otherwise rejects all egress but the proxy's uid) would block it.
It cannot be routed through the egress-proxy — the CSI injection webhook discards
any env/args set on a user-declared sidecar container. So the pod instead pins
`storage.googleapis.com` to the **restricted Google APIs VIP** (`199.36.153.4/30`)
and `metadata.google.internal` to `169.254.169.254` via `hostAliases`, and the
lockdown grants a narrow exemption scoped to **both** the sidecar's uid **and**
those two fixed CIDRs — never the runner's uid, so the untrusted harness still
cannot reach them. This requires **Private Google Access** on the node subnet
(so the VIP routes):

```sh
# One-time, on the cluster's node subnet (usually already on for GKE Standard):
gcloud compute networks subnets update <subnet> --region <region> \
  --project $PROJECT --enable-private-ip-google-access
```

Without it the VIP is unreachable and the mount fails closed (the self-check logs
a failure, non-fatally). Runs that do not use the mount are entirely unaffected —
no `hostAliases`, no exemption.

## Uninstall

```sh
wren uninstall --kube-context <cluster-context> --confirm
```

Removes the `wren-system` + run namespaces and the Wren CRDs (every AgentRun
goes with them — hence the confirmation gate).

For a `--create-cluster` install, add `--delete-cluster` (+ `--gcp-project`,
and `--gcp-zone`/`--gcp-cluster-name` if you overrode them) to also
permanently delete the underlying GKE cluster in the same step — still gated
by the same `--confirm`:

```sh
wren uninstall --kube-context <cluster-context> \
  --delete-cluster --gcp-project my-proj --confirm
```

Idempotent: re-running against an already-deleted cluster succeeds (it's
treated as already gone, not an error).

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
  minted by the control plane and injected at the proxy.
- **SSO/OIDC** for the apiserver front-door (replacing the `X-Wren-User`
  header) and managed **Postgres** provisioning + a **Helm chart** (WS-5).
- **Workload Identity** for the operator/pods → GCP. First use landed in WS-18:
  the experimental GCS checkpoint mount binds a dedicated checkpointer KSA to a
  GCP SA (see "Experimental: GCS checkpoint mount" above); the operator/apiserver
  and the general pod→GCP path are still ambient-credential / node-scoped.
- **Roadmap CLI surface** (deliberately not shipped yet — the CLI lists only
  commands that work, so these are absent rather than stubbed): `wren run
  attach` / `wren run steer` (interactive steering), `wren run resume` (manual
  re-run of a terminally-Failed run — the operator already auto-resumes
  *infrastructure* crashes; a manual trigger that resets the retry budget and
  clears the leftover Failed pod is a real feature, deferred), `wren usage`
  (token/cost/compute reporting), `wren mcp add|list|test` (per-project MCP
  servers), `wren project config` (editing defaults/rubric/egress in place).
  Each is trivial to re-add once its server side lands. (`wren fleet` — a
  cross-run dashboard — shipped for real in WS-20: `run list`/`fleet` render a
  table with `--project`/`--phase` filters and `--watch` for a live view.)
- **Sandbox runtimes** `gvisor` / `kata` for `wren run create --runtime`: wired
  end-to-end in the operator but not provisioned by any v1 cluster, so the CLI
  rejects them today (only `runc` works) until M4.
