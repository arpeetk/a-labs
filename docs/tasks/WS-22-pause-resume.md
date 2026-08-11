# WS-22 — Durable pause/resume and verified checkpoints

## Outcome

An operator can pause a running agent without losing accepted work, observe the
durable checkpoint that makes the pause safe, and resume from that exact state.
The lifecycle remains correct across duplicate requests, controller restarts,
pod/PVC loss, and corrupt or missing checkpoint data.

This workstream turns checkpointing from periodic best-effort recovery into a
user-visible durability contract. It includes the CLI and desktop surfaces and
must be proven through the real kind control-plane path, not only unit tests.

## Lifecycle contract

`Running -> Pausing -> Paused -> Provisioning -> Running`

- Pause is accepted only from `Running` and only when checkpoint storage is
  configured. Duplicate pause requests are idempotent.
- `Pausing` means the harness is quiesced while a forced checkpoint is written,
  read back, checksummed, and atomically published.
- `Paused` is reachable only after the verified checkpoint reference is stored
  in `AgentRun.status` and the agent pod has been removed. No compute continues
  while a run is paused.
- A failed forced checkpoint must not destroy the live workspace. The operator
  unquiesces the harness, returns the run to `Running`, and records an actionable
  condition.
- Resume from `Paused` restores the exact checkpoint recorded by the pause, not
  whichever object happens to list last. A surviving PVC may be reattached only
  because it contains the same quiesced state.
- Resume is idempotent and advances `attemptGeneration` exactly once. It does not
  consume the automatic crash-retry budget.
- Existing manual resume from `Failed` remains supported.

## Checkpoint contract

Each new checkpoint is a versioned manifest published after its archive:

1. write the archive under an immutable object key;
2. read it back and verify SHA-256 and byte size;
3. publish the manifest as the commit marker;
4. expose it in status and prune checkpoint pairs beyond retention.

Restore lists manifests only, validates their schema and archive checksum, and
falls back from a corrupt newest periodic checkpoint to an older valid one. An
explicit pause checkpoint never silently falls back: corruption or absence is a
deterministic failure naming the checkpoint. Legacy WS-21 `.tar.gz` objects stay
readable during migration.

The harness must never receive checkpoint-store credentials or a checkpoint
mount. Forced snapshots execute only in the trusted checkpointer container.

## Failure matrix

| Failure | Required behavior |
|---|---|
| Duplicate pause while `Pausing`/`Paused` | success; no phase regression or duplicate attempt |
| Controller restart after publication | recover from status condition/reference and finish pausing |
| Checkpoint write/read-back failure | unquiesce; `Running`; `PauseCheckpointFailed` condition |
| Pod disappears during pause | use a verified recorded checkpoint or fail loudly; never claim an unproven pause |
| Duplicate resume | one attempt-generation increment and one replacement pod |
| PVC lost after pause | restore the exact recorded checkpoint |
| Explicit checkpoint missing/corrupt | deterministic terminal failure; no fallback |
| Latest periodic checkpoint corrupt | restore the newest older valid checkpoint |
| Publication interrupted before manifest | incomplete archive is invisible to restore |
| Retention cleanup failure | keep the valid checkpoint and report condition; never invalidate the new snapshot |

## Product surface

- HTTP API: `POST /v1/runs/{id}/pause`; existing resume endpoint accepts
  `Paused` and `Failed` with the lifecycle rules above.
- CLI: `wren run pause <id>` and truthful resume/help/output.
- Desktop: Pause from a running detail view; show `Pausing`/`Paused`, resume,
  checkpoint identity/time/integrity, and recovery conditions in the timeline.
- Fleet/list/detail projections expose the checkpoint and relevant conditions.

## Verification and definition of done

- API, store, launcher, controller, pod builder, runtime, checkpoint format,
  CLI, and desktop tests ship with the implementation.
- Tests cover the failure matrix, authorization/validation, monotonic attempts,
  and the invariant that the harness has no checkpoint mount.
- Blob implementations pass shared conformance tests including deletion and
  malicious-path rejection.
- A repeatable kind chaos gate drives CLI -> apiserver -> operator -> runtime
  through pause/resume, duplicate requests, controller restart, PVC loss, and
  corrupt/missing checkpoint cases.
- The desktop app is built and exercised against the live local control plane.
- `make test`, `make vet`, coverage, lint, assets, frontend build/tests, and the
  existing keyless e2e gate are green.
- A live Codex-backed run is paused and resumed when credentials are available;
  secrets are never printed or committed.
- `AGENTS.md`, the technical spec, desktop docs, and this status ledger describe
  the shipped behavior without retaining the old stand-in claim.
- Branch is pushed, PR opened, and required CI is green before merge.

