# WS-17: `wren install --create-cluster` — bounded, opt-in GKE provisioning

**Branch:** `ws17-create-cluster` · **Worktree:** `../wren-ws17` · **Size:** L
**State:** READY

*Context: after WS-14/WS-15 made `wren install` seamless for a team that
already has a Kubernetes cluster, the owner found the one step still left
manual — actually getting a GKE cluster in the first place — is genuinely
rough: it requires knowing Standard-vs-Autopilot (Autopilot forbids the
privileged `NET_ADMIN` init container WS-1's egress lockdown needs), and a
node-service-account IAM grant for Artifact Registry pulls that fails silent
and late (`ImagePullBackOff`, no clear link back to the missing grant — see
the just-shipped diagnostic in `internal/install/kube.go`'s
`diagnosePullFailure`, commit `84a4337`, which is the fallback safety net for
teams bringing their own cluster; this workstream's job is to make that whole
manual sequence unnecessary for the fast/eval path).*

*Decision (owner, this session): don't make cluster auto-provisioning the
default or the recommended team-setup path — creating a GKE cluster is a real
infra decision (region, sizing, cost) a team should usually make deliberately.
Instead, add it as a clearly-labeled, explicitly opt-in quickstart path,
parallel to how `--kind` already exists for local eval. `--create-cluster` is
the GCP equivalent of `--kind`: fast, sane defaults, not what SETUP.md's
headline path recommends for a real team.*

## Design (settled — implement as specified, don't re-derive)

1. **New flag on `wren install`:** `--create-cluster` (bool). When set,
   requires `--registry` (still needed — the cluster gets created, but images
   still need somewhere to push to) and a new required `--gcp-project` flag.
   New optional flags with sane defaults: `--gcp-zone` (default
   `us-central1-a`), `--gcp-cluster-name` (default `wren`),
   `--gcp-machine-type` (default `e2-standard-2`), `--gcp-num-nodes` (default
   `1`). Mutually exclusive with `--kind` (same validation pattern
   `--registry`/`--kind` already use).
2. **Shell out to `gcloud` via the existing `Runner` interface**
   (`internal/install/install.go`'s `Runner`, already used for
   docker/kubectl/`gh`) — don't pull in a GCP Go SDK. This keeps the same
   architecture as every other external tool this package drives. Preflight:
   check `gcloud` is on `PATH` (via `Runner.LookPath`) and authenticated
   (`gcloud auth list --filter=status:ACTIVE --format='value(account)'` via
   `Runner.Output` returning a non-empty account) before doing anything else
   — fail with a clear remedy (`gcloud auth login`) rather than a confusing
   failure three steps in.
3. **The provisioning sequence** (new `steps` method, called before
   `applyManifests` when `--create-cluster` is set):
   - Enable the two APIs a fresh GCP project won't have on yet:
     `gcloud services enable container.googleapis.com
     artifactregistry.googleapis.com --project <project>` (idempotent if
     already enabled — don't skip this check, "1-click" should work against a
     genuinely brand-new project, not just a pre-warmed one).
   - Create the cluster: `gcloud container clusters create <name> --zone
     <zone> --project <project> --num-nodes <n> --machine-type <type>`
     — **must be GKE Standard, not Autopilot** (the default subcommand shape
     already is Standard; do not use `clusters create-auto`). If a cluster
     with that name already exists in that project/zone, treat it as success
     and proceed (idempotent re-install, matching the rest of `wren install`'s
     philosophy) rather than erroring "already exists" — check via `gcloud
     container clusters describe` first, or parse the create error.
   - Fetch credentials: `gcloud container clusters get-credentials <name>
     --zone <zone> --project <project>` — this is what makes the cluster
     reachable for the rest of the install flow. The resulting kubeconfig
     context name follows GKE's fixed pattern
     `gke_<project>_<zone>_<name>` — feed that into the same
     `installKubeContext` resolution `--kind` already uses (see
     `internal/cli/install.go`), so the rest of `Install()` (applyManifests,
     images, credentials, WaitDeployments) needs **no changes** — it should
     just work against whatever context is now active, exactly like it does
     for `--kind`.
   - Configure docker→Artifact Registry auth:
     `gcloud auth configure-docker <zone-prefix>-docker.pkg.dev --quiet`
     (derive the region prefix from `--gcp-zone`, e.g. `us-central1-a` →
     `us-central1`) — needed before the image push step that follows.
   - Grant the node service account pull access — the exact sequence
     `internal/install/kube.go`'s new `imagePullRemedy` already prints as the
     manual remedy: resolve the project number
     (`gcloud projects describe <project> --format='value(projectNumber)'`
     via `Runner.Output`), then `gcloud projects add-iam-policy-binding
     <project> --member="serviceAccount:<projnum>-compute@developer.gserviceaccount.com"
     --role="roles/artifactregistry.reader"`. This closes the exact gap the
     diagnostic in commit `84a4337` exists to catch — with `--create-cluster`,
     nobody should ever actually see that diagnostic fire.
4. **Cost transparency, not a confirmation gate.** `--create-cluster` is
   already an explicit, unambiguous signal of intent (unlike `wren uninstall`,
   which is destructive and irreversible and thus gated behind `--confirm`).
   Creating infrastructure is reversible (`gcloud container clusters delete`)
   and the flag itself is the confirmation. Do print a clear one-line notice
   before starting — project, zone, machine type, node count, and that this
   incurs real cloud cost — so the output is honest about what's about to
   happen, but do not add a second confirmation prompt/flag on top of
   `--create-cluster` itself.
5. **Docs:** `SETUP.md`'s GKE section gets a new subsection for this path
   (clearly framed as the quickstart/eval option, with the existing
   "bring-your-own-cluster" flow staying the headline/recommended path for
   real teams) and README's install section gets a one-line mention.
   `--help` text for the new flags must make the Standard-not-Autopilot and
   cost implications clear, not just describe the mechanics.

## Scope guards

**OUT:** any non-GKE cloud (AWS/Azure — v1 is GCP-only per the locked
architecture decision); Autopilot support (explicitly excluded — it can't run
the egress lockdown); a `wren uninstall --delete-cluster` counterpart (out of
scope for this workstream — note it as a natural follow-up in the hand-off,
don't build it); changing anything about the `--kind` path; changing
`diagnosePullFailure`/`imagePullRemedy` (just added, commit `84a4337`) beyond
what's needed to keep it working as the fallback for bring-your-own-cluster
installs.
**Hot files:** `internal/install/install.go` (`Options`, `Install()`),
`internal/install/steps.go` or a new `internal/install/gke.go` (your call —
keep the new gcloud-provisioning logic in its own file rather than growing
`steps.go` further, it's a meaningfully distinct concern), `internal/cli/install.go`
(flag wiring), `internal/install/fake.go` (extend `FakeRunner` if the existing
one doesn't already cover what you need — check first), `SETUP.md`, `README.md`.

## Definition of done

- [ ] `wren install --create-cluster --registry <prefix> --gcp-project <proj>`
      against a project with **no existing cluster** produces a working
      control plane end to end — this is the core claim, verify it live
      against real GCP (the `wren-gke-fdea81` project + `arpeetkale@gmail.com`
      gcloud auth are already set up in this environment from prior sessions —
      confirm with `gcloud config list`/`gcloud auth list` before assuming,
      state doesn't necessarily match older memory). **Tear the cluster down
      after** (`gcloud container clusters delete`) — this is real, billable
      infrastructure, don't leave it running. Paste the real transcript in
      the hand-off, not a description of what should happen.
- [ ] Idempotent: running the same command again against an existing cluster
      of that name succeeds (doesn't error "already exists") and doesn't
      recreate anything unnecessarily.
- [ ] `--create-cluster` without `--gcp-project`, or combined with `--kind`,
      fails fast with a clear message (unit-testable, no GCP needed).
- [ ] Preflight catches missing/unauthenticated `gcloud` before attempting
      any gcloud call (unit-testable via `FakeRunner`).
- [ ] Unit tests cover the provisioning sequence's command construction via
      `FakeRunner` (mirroring how the existing `--registry`/`--kind` paths are
      tested) — real GCP is only for the one live end-to-end proof above, not
      every branch.
- [ ] `make test vet` + lint green; `make e2e` unaffected (this is a new,
      additive path — the keyless kind gate shouldn't need to change at all).
- [ ] SETUP.md/README updated; `--help` text is honest about cost and the
      Standard-only constraint.
