# WS-21: real checkpointing — periodic snapshots + restore-from-checkpoint on workspace loss

**Branch:** `ws21-checkpoint-restore` · **Worktree:** `../wren-ws21` · **Size:** L
**State:** READY. **Human review gate required before merge** — unlike WS-20,
this workstream touches the reconciler's failure/retry semantics and the
checkpointer/hydrate trust boundary. Stop at "PR opened, fully validated,
live-verified" and wait for review. See "Execution rules" at the bottom.

*Context: WS-18/19 built the GCS FUSE mount plumbing (a bucket surfaced as a
POSIX path inside the checkpointer sidecar, working under default egress
enforcement) but deliberately shipped no actual checkpoint content —
`internal/blob.Store`'s own doc comment says "v0.1 ships no implementation and
takes no checkpoints." `RunCheckpointer` today only does a one-shot self-check
Put/Get/List round-trip, then idles. `ensurePVC` unconditionally fails a run
`PhaseFailed`/`WorkspaceLost` the moment its PVC is gone past `Pending` — by
design (WS-16), because resuming into a silently-empty workspace would be
worse than failing loudly. This workstream is what makes that tradeoff
unnecessary: take real periodic snapshots, and when the workspace disk is
genuinely lost, restore the latest one before the harness starts — falling
back to the same loud, deterministic failure WS-16 already built when no
checkpoint exists. This directly closes the gap identified against
`sigs.k8s.io/agent-sandbox`'s checkpoint support, using Wren's own
already-proven GCS mount rather than adopting anything external.*

*Scope note: this is workspace/file-level durability — tar the workspace
directory (including `.git`, including uncommitted edits), restore it whole.
It is explicitly NOT: (a) VM/live-process-level snapshotting (what GKE Pod
Snapshots / Agent Sandbox actually do — a genuinely harder problem, not this
one), or (b) harness-level conversation/session resume (each harness's own
`--resume`/session-transcript semantics — `RunSpec.SessionID` already exists
for that and is untouched here). Don't let either scope quietly creep in.*

## Grounding (what already exists — read before writing code)

- `internal/blob.Store` (`internal/blob/blob.go`): `Put`/`Get`/`List` scoped to
  a run's prefix, `ErrNotFound`, `Object{Key,Size,Modified}`. Already states
  the intended shape: checkpoints under `checkpoints/`, transcript fragments
  under `transcript/` (transcript mirroring is NOT this workstream — only the
  checkpoints/ half). Retention/pruning is explicitly bucket policy, not part
  of the contract — carry that forward unchanged.
- `internal/blob.MountStore` (`internal/blob/mount.go`): real, path-traversal-safe
  POSIX-filesystem-backed `Store`, already used by the WS-18 self-check.
  `NewMountStore(base, prefix)` — `prefix` is what scopes one run's objects
  away from every other run sharing the same bucket. **The self-check today
  does NOT use this scoping** — `mountSelfCheck` calls
  `blob.NewMountStore(mountPath, "")` (empty prefix) and hand-embeds the run ID
  into the key instead. That's a latent correctness gap this workstream must
  close (see Design §1) before it matters: the default checkpoint bucket
  (`coreapi.Service`'s `defaults.CheckpointBucket = "gs://wren-ckpt"`,
  `internal/coreapi/service.go:75`) is shared across every project/run unless
  overridden, so an unscoped `checkpoints/` listing would return every run's
  checkpoints, not just the caller's.
- `internal/controller/pod.go`: `gcsMount := cfg.CheckpointGCSMount &&
  run.Spec.Workspace.Checkpoint.Bucket != ""` (line ~466). Bucket is populated
  end-to-end already (`coreapi.Service` → `AgentRun.Spec.Workspace.Checkpoint.Bucket`,
  `internal/coreapi/service.go:497`) — every run gets a bucket value by
  default. The only real gate is the operator's `--checkpoint-gcs-mount` flag
  (`cmd/wren-operator/main.go:46`, off by default). **No new CLI/CRD field is
  needed to turn this feature on** — it's already an operator-level opt-in;
  this workstream makes that opt-in do something real instead of just proving
  connectivity.
- The mount today is wired into the `checkpointer` container ONLY (`pod.go`
  lines ~484-489: `checkpointer.VolumeMounts`/`checkpointer.Env`), never
  `hydrate`, never `harness`. `checkpointer` already gets
  `WREN_CHECKPOINT_INTERVAL` (line 394, from `checkpointInterval(run)` /
  `defaultCheckpointInterval = 120`) but nothing reads it today —
  `RunCheckpointer` falls through to the generic 30s-tick `RunSidecar` stub
  after its self-check.
- `checkpointer`'s `VolumeMounts` already include the workspace PVC,
  **read-only**, at `runspec.WorkspacePath` (`pod.go:396`) — it can already
  read the workspace to snapshot it; no new volume wiring needed there.
- `hydrate` (`internal/podruntime/podruntime.go:231`, `RunHydrate`) has no GCS
  mount access today. Its existing logic: if a repo+token are configured AND
  `spec.Mode != runspec.ModeResume`, do a real git clone (through the
  egress-proxy). Otherwise — including every `ModeResume` case today — it's a
  **no-op stub** ("restore-from-checkpoint skipped; no repo/token — M0").
  `Mode` comes from `runspec.Mode`, set by `buildRunSpec`/`buildAgentPod` from
  `run.Status.RestartCount > 0` (`internal/controller/agentrun_controller.go:242`,
  `pod.go:324`) — the SAME signal drives both the pod's `WREN_MODE` env and the
  RunSpec's `Mode` field, so they can't disagree.
- `ensurePVC` (`agentrun_controller.go:190-207`): a `NotFound` PVC lookup is
  disambiguated by `run.Status.Phase` — `Phase == Pending` means first-ever
  creation (create it); any later phase means the PVC genuinely existed and is
  now gone (`errWorkspaceLost`, mapped at the `Reconcile` call site,
  `agentrun_controller.go:87-98`, to `PhaseFailed`/`"WorkspaceLost"`).
  `pvcName(run)` is `run.Name + "-workspace"` — **stable across restarts**,
  unlike `podName(run)` which embeds `RestartCount`.
- `classifyTermination` (`agentrun_controller.go:436`) already inspects
  **both** `InitContainerStatuses` and `ContainerStatuses` for a non-zero exit
  and already treats any non-zero, non-`OOMKilled`, non-`ExitRetryable` exit
  from ANY container — including an init container like `hydrate` — as a
  deterministic, non-retryable failure. `cmd/wren-runtime/main.go:34-37`
  already maps a plain (non-`ErrRetryable`-wrapped) error from any role,
  including `RunHydrate`, straight to `runspec.ExitError`. **This means "no
  checkpoint to restore" already has a working, tested, zero-new-code path to
  a loud `PhaseFailed`** — `hydrate` just needs to return a real error; no new
  controller-side failure classification is needed.
- `handlePodFailure` (`agentrun_controller.go:303-344`) is the existing
  crash-resume pattern this workstream's recovery path should mirror: delete
  the dead pod, bump `RestartCount`, set `PhaseInterrupted` + a condition,
  requeue. `setCondition`/`findCondition` (lines 460, 156) are the existing
  upsert-by-type condition helpers, already used by `ensureEgressCondition`
  for exactly this kind of durable, idempotent status marker.

## Design (settled — implement as specified)

**1. `blob.RunPrefix` — fix and reuse the per-run scoping.**
Add `func RunPrefix(bucket, runID string) string` to `internal/blob/blob.go`:
parse `bucket` as `gs://<bucket-name>[/<base-prefix>]` (or bare
`<bucket-name>[/<base-prefix>]`), and return `path.Join(basePrefix, "runs",
runID)` (empty `basePrefix` is fine — `path.Join` handles it). This mirrors
`internal/controller/pod.go`'s existing `gcsBucketName()` comment ("any prefix
path within the bucket is handled by the Store's per-run prefix, not the
mount") — that promise is currently unfulfilled; this is what fulfills it.
Update `mountSelfCheck` (`podruntime.go`) to build its store with
`blob.NewMountStore(mountPath, blob.RunPrefix(bucket, runID))` and drop the
hand-embedded run ID from the self-check key. Update `blob.go`'s package doc
comment — it currently says "v0.1 ships no implementation and takes no
checkpoints"; that framing is now stale and must be corrected to describe what
actually exists, including that the example key format
(`"checkpoints/ck-000042.bundle"`) is a **tar.gz of the workspace tree**, not a
git bundle (a git bundle only captures committed refs, not the working tree —
wrong for "resume from last known state," which must include uncommitted
edits). Use extension `.tar.gz`, not `.bundle`, to avoid the misleading name.

**2. `internal/blob/archive.go` (new) — tar+gzip a directory tree and its inverse.**
Stdlib only (`archive/tar`, `compress/gzip`) — no new dependency.
- `func Archive(dst io.Writer, srcDir string) error` — walks `srcDir`,
  writing a gzip-compressed tar stream preserving relative paths (including
  `.git`), regular files, and directories. Symlinks: follow-and-skip-if-broken
  is fine (the workspace is agent-written repo content, not a place symlink
  edge cases need special handling).
- `func Unarchive(src io.Reader, destDir string) error` — the inverse; writes
  into `destDir`, which the caller (hydrate) guarantees is empty (a
  freshly-created PVC — see §5). Reject any tar entry whose cleaned path
  escapes `destDir` (mirror `MountStore.resolve`'s escape check — same
  "don't trust the archive" discipline for the same reason).
- Unit test with a small directory tree (nested dirs, a `.git`-like path, an
  empty dir) round-tripped through `Archive` → `Unarchive`, asserting content
  and structure match exactly.

**3. Real periodic snapshots — `RunCheckpointer`.**
When `WREN_CHECKPOINT_BUCKET` and `WREN_CHECKPOINT_MOUNT_PATH` are both set
(unchanged gate), after the existing self-check: instead of falling through to
`RunSidecar`'s generic 30s stub, run a new loop keyed off
`WREN_CHECKPOINT_INTERVAL` (already set by the operator, default 120s):
- Parse the interval; on each tick, `blob.Archive` the workspace
  (`runspec.WorkspacePath`, already read-only-mounted into this container)
  into an in-memory buffer or temp file, then `store.Put(ctx,
  "checkpoints/ck-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".tar.gz",
  ...)` against a `blob.NewMountStore(mountPath, blob.RunPrefix(bucket,
  runID))`.
- Log one line per successful snapshot (object key + byte size) — this is the
  hook the live-verification proof below asserts on, same pattern as the
  self-check's PASSED line.
- A failed snapshot attempt logs loudly and continues to the next tick — same
  non-fatal posture as the self-check (crash-looping this sidecar over a
  transient GCS blip would kill the whole pod for no benefit); it is not this
  workstream's job to add alerting.
- With no mount configured, behavior is unchanged (falls through to
  `RunSidecar` exactly as today).
- No change to the container's `VolumeMounts`/env beyond what WS-18 already
  wired (`WREN_CHECKPOINT_INTERVAL` was already threaded through and unused —
  this is what starts using it).

**4. Restore-from-checkpoint — `RunHydrate`.**
Add a new `RestoreRequired bool` field to `runspec.RunSpec`
(`json:"restoreRequired,omitempty"`) — see §6 for who sets it and when. In
`RunHydrate`, when `spec.Mode == runspec.ModeResume`:
- If `spec.RestoreRequired` is false (today's only `ModeResume` case — an
  ordinary crash-resume where the PVC survived): keep today's behavior
  exactly, a no-op. The workspace files are already there.
- If `spec.RestoreRequired` is true: this is a freshly-recreated, empty PVC
  after a confirmed workspace loss (§5/§6) — restore is mandatory, not
  best-effort. Build `blob.NewMountStore(mountPath, blob.RunPrefix(bucket,
  runID))` (mount path comes from the new `WREN_CHECKPOINT_MOUNT_PATH` env —
  see §5's mount extension), `List(ctx, "checkpoints/")`, and:
  - **No objects found:** this run has no checkpoint to restore — return a
    plain (non-`ErrRetryable`) error describing exactly that. This already
    flows through `cmd/wren-runtime/main.go` → `ExitError` →
    `classifyTermination` → `PhaseFailed`/non-retryable, with zero new
    controller code (see Grounding). Do not silently fall through to an empty
    workspace — that's precisely the WS-16 invariant this workstream must not
    weaken.
  - **Objects found:** pick the one with the latest `Modified` (per
    `blob.Object`'s own doc comment — "resume uses it to pick the latest
    checkpoint"), `Get` it, `blob.Unarchive` it into `spec.WorkspacePath`, log
    a clear PASSED-style line naming the restored key and byte count (the
    hook the live proof asserts on).
- `waitForProxy` is NOT needed for this path — the GCS mount is a local
  filesystem path resolved by the CSI driver before any container starts; it
  has no dependency on the egress-proxy sidecar being up (unlike the git-clone
  path, which is unchanged and still waits).

**5. Extend the GCS mount to `hydrate`, read-only. Never to `harness`.**
In `pod.go`'s existing `if gcsMount { ... }` block (~line 474), in addition to
today's `checkpointer.VolumeMounts`/`checkpointer.Env` additions, add:
```go
hydrate.VolumeMounts = append(hydrate.VolumeMounts,
    corev1.VolumeMount{Name: VolumeCheckpoints, MountPath: MountCheckpoints, ReadOnly: true})
hydrate.Env = append(hydrate.Env,
    corev1.EnvVar{Name: "WREN_CHECKPOINT_MOUNT_PATH", Value: MountCheckpoints})
```
Read-only: hydrate only ever restores FROM the store, never writes to it —
least privilege, even though it's already a trusted init container. This is
the ONLY change to the trust-tier surface this workstream makes; the "never
harness" invariant stays absolute and untested-by-assertion is not
acceptable — extend the existing `TestBuildAgentPod_GCSMount_HarnessNever*`
tests (`pod_test.go`) to also assert hydrate's mount is present-and-read-only
when `gcsMount` is true, and that harness still has it nowhere, under any
config. **No change to `internal/podruntime/lockdown.go` or
`internal/egress/proxy.go` is needed or in scope**: the GCS traffic for this
mount is generated entirely by GKE's injected `gke-gcsfuse-sidecar` process
(uid 65534, per WS-19), never by `hydrate`'s own process — WS-19's existing
uid+CIDR-scoped lockdown exemption already covers it unchanged. If your
investigation finds this assumption wrong, stop and say so rather than
touching lockdown code to route around it.

**6. Make `ensurePVC`'s failure conditional, without weakening it for anyone
who hasn't opted in.** Add one sentinel and one condition type, both in
`agentrun_controller.go`:
```go
var errWorkspaceRestoring = errors.New("workspace PVC lost; recreating for checkpoint-restore")
const workspaceRestoreConditionType = "WorkspaceRestorePending"
```
Restore eligibility is `r.PodConfig.CheckpointGCSMount &&
run.Spec.Workspace.Checkpoint.Bucket != ""` — the same two-part gate
`buildAgentPod` already uses for `gcsMount`. `ensurePVC`'s `NotFound` branch
becomes:
- `Phase == Pending` (first-ever creation) → unchanged: create and return nil.
- `Phase != Pending` and NOT eligible → unchanged: `errWorkspaceLost` (today's
  behavior, still the default for every run that hasn't opted into
  checkpointing — this must not regress).
- `Phase != Pending`, eligible, and `WorkspaceRestorePending` condition is
  **not yet True** (first time this loss is observed): delete the existing
  pod if one exists (mirror `cancel()`'s Get-then-Delete-ignoring-NotFound
  pattern, using today's `podName(run)` — i.e. BEFORE bumping
  `RestartCount`), then `RestartCount++`, `setCondition(run,
  {Type: workspaceRestoreConditionType, Status: True, Reason: "WorkspaceLost",
  Message: "..."})`, `Phase = PhaseInterrupted`, persist via
  `r.Status().Update`, and return `errWorkspaceRestoring` (do **not** create
  the PVC yet in this same call — the just-deleted pod must not race a
  same-generation PVC). Map `errWorkspaceRestoring` at the `Reconcile` call
  site (next to today's `errWorkspaceLost` handling) to
  `ctrl.Result{Requeue: true}, nil` — not a failure, not a no-op.
- `Phase != Pending`, eligible, and `WorkspaceRestorePending` condition **is**
  already True (the requeued follow-up reconcile): create the PVC exactly
  like the first-ever-creation path, and return nil. Do NOT clear the
  condition here — `buildRunSpec` (§7) still needs to read it as True for the
  pod about to be built.
- Clear the condition (`Status: False`) in `reconcilePodState`'s
  `corev1.PodRunning` case, once the new pod is confirmed running — proof
  that `hydrate` (which blocks the harness from starting) completed
  successfully, whether or not a restore was actually required for it. This
  stops the condition from lingering true into that run's later, ordinary
  crash-resumes (where the PVC will have survived and restore must NOT be
  re-attempted into a non-empty workspace).
This reuses `RestartCount` as the one true "is this pod a Resume" signal
end-to-end (unchanged from today), reuses the existing `Retry.MaxRestarts`
budget to bound restore attempts (a bucket that's permanently unreachable
does not loop forever — it eventually hits `RetryBudgetExhausted`, same as
any other crash-resume path), and needs no new field for "was this bump a
restore" beyond the condition already required for RunSpec-building in §7.

**7. Thread `RestoreRequired` into the RunSpec.**
In `buildRunSpec` (`agentrun_controller.go:230`): `rs.RestoreRequired =`
`findCondition(run, workspaceRestoreConditionType) != nil &&`
`findCondition(run, workspaceRestoreConditionType).Status == metav1.ConditionTrue`.

## Scope guards

**OUT:** harness-level session/conversation resume (`RunSpec.SessionID`,
each harness's own `--resume` semantics — untouched, orthogonal, not this
workstream); VM/live-process snapshotting (gVisor checkpoint/restore, GKE Pod
Snapshots — a different and harder problem, explicitly deferred, do not
conflate with this); checkpoint retention/pruning/lifecycle (bucket policy,
per `blob.go`'s existing, unchanged framing — an unboundedly growing
`checkpoints/` prefix per run is a known, accepted gap, not a bug to fix
here); transcript mirroring (`transcript/` — `blob.go` mentions it, this
workstream only implements the `checkpoints/` half); any new CLI flag or CRD
field to configure the bucket/interval per run (both are already
end-to-end-wired from project config — this workstream only makes the
existing fields do something real); enabling `--checkpoint-gcs-mount` by
default anywhere; changing `--egress-enforcement`'s behavior or
`internal/podruntime/lockdown.go` (see §5 — this should need zero changes
there; if it turns out to, stop and say why instead of proceeding); a
merge/diff-based restore (the PVC is guaranteed empty in the restore path —
whole-tree overwrite is correct and simpler, don't build merge logic that
isn't needed).

**Hot files:** `internal/blob/blob.go`, `internal/blob/archive.go` (new),
`internal/blob/archive_test.go` (new), `internal/podruntime/podruntime.go`,
`internal/podruntime/podruntime_test.go`, `internal/runspec/runspec.go`,
`internal/runspec/runspec_test.go`, `internal/controller/pod.go`,
`internal/controller/pod_test.go`, `internal/controller/agentrun_controller.go`,
`internal/controller/agentrun_controller_test.go`, `docs/tasks/STATUS.md`.

**Explicitly forbidden, not just out of scope:** `internal/egress/*`; any
change that mounts the checkpoint volume into `harness`'s `VolumeMounts`
under any condition; any change that makes `ensurePVC` create-or-resume on a
lost PVC when checkpointing is NOT configured for the run (the
`errWorkspaceLost` default-path behavior for everyone else must not move).

## Definition of done

- [ ] `blob.Archive`/`blob.Unarchive` round-trip a real directory tree
      (including a nested `.git`-shaped path) byte-for-byte; path-escape
      rejected on unarchive, unit-tested.
- [ ] `blob.RunPrefix` unit-tested against `gs://bucket`, `gs://bucket/pre`,
      and bare `bucket` forms; self-check updated to use it (no more
      hand-embedded run ID in the self-check key).
- [ ] `pod_test.go`: hydrate gets the checkpoints mount **read-only** exactly
      when `gcsMount` is true; harness never gets it, under any config
      (extends the existing `TestBuildAgentPod_GCSMount_HarnessNever*` tests,
      doesn't replace them).
- [ ] `ensurePVC`/`reconcilePodState` unit tests: (a) checkpointing NOT
      configured → `errWorkspaceLost` → `PhaseFailed` unchanged (regression
      guard); (b) checkpointing configured, PVC lost past Pending → old pod
      deleted, `RestartCount` bumped, condition set True, `errWorkspaceRestoring`
      → requeue, not failure; (c) the follow-up reconcile creates the PVC and
      proceeds; (d) `buildRunSpec` sets `RestoreRequired=true` exactly then;
      (e) reaching `PodRunning` clears the condition; (f) a later, ordinary
      crash-resume of the same run (PVC intact) does NOT re-trigger restore.
- [ ] `RunHydrate` unit tests: ordinary resume (`RestoreRequired=false`)
      unchanged no-op; `RestoreRequired=true` with no checkpoints → real
      error (verify it maps to `ExitError` via the existing `errors.Is`
      dispatch, not `ErrRetryable`); `RestoreRequired=true` with checkpoints
      → picks latest by `Modified`, restores correctly.
- [ ] `RunCheckpointer` unit tests: periodic snapshot loop actually calls
      `Put` on tick (fake clock or short interval + `-race`-clean channel
      sync, matching this project's existing sidecar test patterns); no
      mount configured → unchanged stub behavior.
- [ ] **Live-verified on real GKE, not just unit-tested** (this project's
      standing discipline for anything mount/cluster-related — WS-18/19/20
      all found real bugs invisible to unit tests):
  - [ ] A run with `--checkpoint-gcs-mount` on and a short interval (e.g. 10s,
        override however's easiest) takes at least 2 real periodic snapshots
        — confirm via `gcloud storage ls`/`cat` independently of the
        checkpointer's own log claim (same double-check discipline as WS-18's
        proof).
  - [ ] Positive restore path: mid-run, delete the workspace PVC directly
        (`kubectl delete pvc`) after at least one checkpoint has landed.
        Confirm: the controller recreates the PVC, `hydrate` restores the
        latest checkpoint (verify actual restored file content — e.g. a
        marker file written into the workspace before the delete reappears
        after restore, not just a log line claiming success), and the run
        reaches `Running`/`Succeeded` again rather than `Failed`.
  - [ ] Negative path: delete the PVC before any checkpoint has ever been
        taken. Confirm the run fails deterministically and promptly
        (`PhaseFailed`, a clear reason mentioning the hydrate exit) — not an
        infinite retry loop, not a silent empty-workspace resume.
  - [ ] A run WITHOUT checkpointing configured (or with
        `--checkpoint-gcs-mount` off) still fails exactly as before on PVC
        loss — the regression guard from the unit tests, re-confirmed live.
- [ ] `make test vet` + `golangci-lint run` green; `make e2e` green in both
      egress-enforcement modes, unaffected (this is additive when the flag is
      off, which `make e2e` runs with).
- [ ] Real GCP infra used for the live proof (cluster/bucket) is torn down
      after, same discipline as every prior GCP-touching workstream.
- [ ] `internal/blob/blob.go`'s package doc comment updated — the "v0.1 ships
      no implementation" framing is stale once this merges.
- [ ] `docs/tasks/STATUS.md` updated with this workstream's result, matching
      the existing row format/style for every other WS-N entry, including
      what was and wasn't live-verified (WS-18's row is the template for how
      to be honest about partial verification if something couldn't be
      proven live).

## Execution rules

- Work in the existing convention: worktree `../wren-ws21`, branch
  `ws21-checkpoint-restore`, branched from latest `main`. Read `AGENTS.md` in
  full first.
- Iterate genuinely, commit each coherent slice (archive helpers → real
  checkpointer loop → hydrate restore → controller conditional path → live
  verification), same discipline as every prior workstream.
- **No self-merge.** Unlike WS-20, this workstream changes reconciler
  failure/retry semantics and extends a trust-tier mount — genuinely
  higher-risk than fleet visibility. Open a PR (`gh pr create`) once every
  Definition of Done item is genuinely, verifiably true (not "should work" —
  actually run and observed), update `docs/tasks/STATUS.md` in the same PR,
  and **stop** — leave it for review rather than merging it yourself.
- If genuinely blocked on a decision only the owner can make (not a technical
  unknown you can resolve by investigating), stop and write the blocker down
  clearly rather than guessing past it or looping indefinitely.
- Tear down anything you stand up (kind clusters, real GCP infra, stray
  port-forwards) before considering a slice done.
