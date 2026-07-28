# WS-18: GCS mount support for the checkpointer (`internal/blob`)

**Branch:** `ws18-gcs-mount` · **Worktree:** `../wren-ws18` · **Size:** L
**State:** READY. **Security-relevant: treat with WS-1's level of care** — this
adds a new trust boundary (which container gets bucket credentials) and a new
IAM mechanism (Workload Identity) this codebase has never used before.

*Context: owner asked "do we support GCS mount?" — answer today is no:
`internal/blob.Store` is an interface with zero implementations ("the socket
only"), the workspace is a regular PVC (Persistent Disk), and the checkpointer
sidecar is a liveness stub that does nothing with the `CheckpointBucket` value
already threaded through `RunSpec`/env. This workstream connects that socket
for real: mount a GCS bucket into the pod via GKE's Cloud Storage FUSE CSI
driver, implement `blob.Store` as plain file I/O against that mount, and prove
a real Put/Get/List round-trip against a real bucket on real GKE.*

## Explicit scope decision (read before starting)

**"Mount" was chosen deliberately over an API-based Store client** (i.e., not
`cloud.google.com/go/storage`). GKE's Cloud Storage FUSE CSI driver mounts the
bucket as a POSIX filesystem inside the container; `blob.Store`'s
`Put`/`Get`/`List` shape already matches plain file I/O, so the GCS-specific
part (auth, the actual API calls) stays entirely in Google's CSI driver
sidecar — the Go implementation in this repo never touches the GCS API or SDK
directly. Simpler, smaller surface, no new Go dependency.

**This workstream builds the mount + the Store implementation — NOT the
checkpointer feature itself.** The periodic-snapshot-and-restore-on-resume
logic was deliberately de-scoped to post-launch (WS-8); building it now would
be a much larger, separate decision (interval semantics, snapshot format,
restore-path changes to hydrate) that the owner hasn't asked for. What "test
end to end" means here: prove the plumbing works — a real file written
through the mount from inside a real pod is visible in the real bucket, and
reads/lists it back correctly. The checkpointer container stays a stub in
every way except one: a startup self-check (below) that proves the mount is
live, logged, nothing more.

## Design (settled — implement as specified; the one open question is
explicitly called out, don't guess past it)

1. **GKE cluster prerequisite:** the Cloud Storage FUSE CSI driver
   (`gcsfuse.csi.storage.gke.io`) must be enabled on the cluster
   (`gcloud container clusters update --update-addons GcsFuseCsiDriver=ENABLED`,
   or `--addons GcsFuseCsiDriver` at creation). This is **not** wired into
   `wren install --create-cluster`'s defaults — this feature is opt-in/
   experimental, `--create-cluster` stays focused on the core task→PR loop.
   Document the addon-enable command in the hand-off and in a new SETUP.md
   subsection; don't touch `internal/install/gke.go`'s defaults.

2. **New `PodConfig` field + operator flag**, matching the existing
   `--egress-enforcement`-style pattern (`internal/controller/pod.go`'s
   `PodConfig`, `cmd/wren-operator/main.go`): `CheckpointGCSMount bool`,
   flag `--checkpoint-gcs-mount` (default `false`). Off by default — most
   clusters won't have the CSI driver enabled, and this stays experimental
   until the real checkpointer lands.

3. **Pod builder change** (`internal/controller/pod.go`, `buildAgentPod`):
   when `cfg.CheckpointGCSMount` is true AND `run.Spec.Workspace.Checkpoint.Bucket`
   is non-empty:
   - Add a CSI volume: driver `gcsfuse.csi.storage.gke.io`, `volumeAttributes:
     {bucketName: <bucket, with the "gs://" prefix stripped>}`.
   - Mount it **only into the checkpointer container**, at a fixed path
     (`/mnt/checkpoints` — pick one, document it, use it consistently in both
     the pod builder and the checkpointer's env/dispatch).
   - Set the pod annotation the CSI driver's sidecar-injection webhook
     requires (`gke-gcsfuse/volumes: "true"`) — only when the mount is
     actually added, not unconditionally.
   - **The harness/runner container must never see this volume.** This is
     the load-bearing invariant (code standards rule #1: pin it in code, test
     the pin) — same trust-tier reasoning as the egress-proxy/runner uid
     separation: the checkpointer is a trusted native sidecar, the harness
     runs untrusted model-generated code and must never hold a credential or
     a writable path to durable storage outside its own workspace PVC.
     **Required test:** assert the harness container's `VolumeMounts` never
     contains the GCS volume, with the mount enabled, mirroring
     `TestBuildAgentPod_ProxyUIDSeparation`'s pattern.

4. **The open question — investigate and prove against a real cluster,
   don't assume:** the agent pod today sets `AutomountServiceAccountToken:
   false` and never sets an explicit `ServiceAccountName` (defaults to
   `default` in the run namespace). GKE Workload Identity Federation
   typically authenticates a pod via its Kubernetes ServiceAccount identity
   exchanged for a GCP identity — whether that exchange still works with
   `automountServiceAccountToken: false` (newer GKE versions route this
   through a node-level metadata-server interception rather than the
   classic mounted SA token, which may make it independent of that setting —
   or may not) is something to verify hands-on on a real cluster, not
   assume from documentation. Whatever you find, the end state must be:
   - A dedicated GCP Service Account (GSA) with `roles/storage.objectAdmin`
     scoped to **the specific checkpoint bucket** (a bucket IAM binding, not
     a project-wide role) — least privilege, matching this repo's existing
     posture everywhere else (the node-pull IAM grant in `internal/install/gke.go`
     is bucket/registry-scoped the same way).
   - A dedicated Kubernetes ServiceAccount (KSA), annotated
     `iam.gke.io/gcp-service-account: <GSA email>`, used **only** by pods
     that have the GCS mount enabled — do not change the default KSA every
     agent pod uses today; introduce a new one gated on
     `CheckpointGCSMount`, so pods without the feature enabled are
     completely unaffected (no new identity, no behavior change).
   - Document in the hand-off exactly what you found about the
     `automountServiceAccountToken` interaction — this is genuinely useful
     information for anyone touching pod identity in this repo later.

5. **`internal/blob` implementation** — new file (e.g. `internal/blob/mount.go`):
   a `MountStore` (name your call) implementing `blob.Store` via `os.*` file
   I/O rooted at a configurable base path + the run's prefix subdirectory
   (`Put`=write file — create parent dirs as needed; `Get`=open file, map
   "not exist" to `blob.ErrNotFound`; `List`=`filepath.WalkDir` under the
   prefix, returning `Object{Key, Size, Modified}` from `os.FileInfo`).
   Hermetically unit-testable against a local temp dir — no GCS or cluster
   needed for this part. Follow `internal/blob/blob.go`'s existing doc
   conventions and the interface exactly as specified (don't change the
   interface).

6. **Checkpointer startup self-check** (`internal/podruntime`, wherever the
   checkpointer role currently just sleeps-until-SIGTERM): when
   `WREN_CHECKPOINT_BUCKET` and the mount path are both present, do one
   `Put`+`Get`+`List` round-trip against a small self-check object (e.g.
   `_wren-mount-check/<run-id>.txt` with a timestamp) at startup, log the
   result clearly (pass/fail), then fall through to the existing
   sleep-until-SIGTERM liveness behavior. This is the "prove it end to end"
   hook — it is **not** the checkpoint feature, just a mount smoke test. If
   the mount isn't configured (flag off / no bucket), behavior is completely
   unchanged from today.

## Scope guards

**OUT:** the real periodic checkpointer (interval snapshots, restore-on-resume
via hydrate, `workspace.checkpoint.*` CRD fields becoming non-no-op) — all
explicitly deferred (WS-8's decision stands); wiring the GCS mount into
`wren install --create-cluster`'s defaults or preflight; any change to the
`blob.Store` interface itself; S3-compatible / MinIO implementations
(the interface supports them later, not this workstream); mounting into any
container other than checkpointer.
**Hot files:** `internal/controller/pod.go`, `internal/controller/pod_test.go`,
`cmd/wren-operator/main.go`, `internal/blob/*.go` (new file), `internal/podruntime/*`
(checkpointer role), `SETUP.md` (new subsection), `docs/technical-spec.md`
(update the checkpointer/blob status note — don't claim more than what's
built: the mount works, the real checkpointer still doesn't exist).

## Definition of done

- [ ] `internal/blob`'s new mount-backed `Store` passes unit tests against a
      local temp dir (Put/Get/List, `ErrNotFound` mapping, prefix isolation).
- [ ] `TestBuildAgentPod`-style test proves: mount absent when the flag is
      off (default); present only on the checkpointer container when the
      flag is on and a bucket is set; **harness container never has it,
      enabled or not** (the required invariant test from item 3).
- [ ] **Live GCP proof, not just unit tests:** on a real GKE cluster with the
      CSI driver addon enabled, a real GCS bucket, and the Workload Identity
      binding set up — deploy a run with `--checkpoint-gcs-mount` (however
      you wire the CLI/operator flag through for a single test run — a
      project/run-level override is fine if that's simpler than exposing a
      new CLI flag; your call, note which you did), confirm the checkpointer
      container's startup self-check passes (logged), and independently
      verify via `gcloud storage cat`/`ls` that the self-check object is
      really in the bucket — don't just trust the pod's own log line.
      Tear the cluster down after (or reuse an existing one and clean up the
      bucket object) — this is real, billable infrastructure.
- [ ] The `AutomountServiceAccountToken`/Workload-Identity interaction is
      documented in the hand-off with what was actually observed, not
      assumed.
- [ ] `make test vet` + lint green; `make e2e` unaffected (additive, flag
      defaults to off).
- [ ] `docs/technical-spec.md`'s checkpointer status note updated to be
      accurate: mount infrastructure works and is proven; the real
      checkpointer (interval snapshots + restore) still does not exist.
