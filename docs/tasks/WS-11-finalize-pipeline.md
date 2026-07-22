# WS-11: Finalize pipeline — idempotent finalize, retry classification, run results → status

**Branch:** `ws11-finalize-pipeline` · **Worktree:** `../wren-ws11` · **Size:** M · **State:** READY
**Blocked on:** nothing. (Coordinates with WS-2 only via `config/` RBAC files — rebase before hand-off.)
**Found by:** WS-1 review rounds (code-quality pass on main, 2026-07-22). These three bugs
undercut the flagship "durable → opens a PR" story; they gate the public launch demo.

## Design (settled — do not re-litigate; questions go in the hand-off)

1. **Idempotent finalize.** Today `gitwork.CommitAll` does `Checkout{Create: true}`
   (`internal/gitwork/gitwork.go`), which errors "branch already exists" when a
   resume pod re-runs finalize on a workspace where the run branch was already
   created — so a crash *after* `git commit` but *before/during* push turns a
   perfectly good run terminal-`Failed` on retry. Fix: if the run branch
   already exists, check it out instead of creating it; if its HEAD tree
   already equals the worktree's (nothing new to commit), treat as
   `ErrNoChanges` and continue to push/PR (do NOT fail). `findExisting`
   already makes OpenPR idempotent — keep that. Add a regression test that
   runs CommitAll twice against the same repo (second run must succeed or
   return ErrNoChanges, never "branch already exists").
2. **Retry-classify transient finalize errors.** `podruntime.RunHarness`
   returns push/OpenPR errors verbatim → harness exits 1 →
   `classifyTermination` marks non-retryable → a GitHub 502 or a dropped
   push connection kills a run with budget to spare, despite the
   `ExitRetryable` machinery. Fix: in `internal/finalize` (or at the
   podruntime boundary), classify transport-class failures — network errors,
   HTTP 429/5xx, EOF/timeout — as retryable (wrap so podruntime returns
   `ErrRetryable`, exiting `runspec.ExitRetryable`); permanent classes
   (401/403 auth, 422 validation, non-fast-forward from a *human* push to
   the branch) stay deterministic. Table-driven tests over the error matrix.
3. **Run results → status (v0.1: operator log-scrape).** The harness emits
   `pr_ready` / `token_usage` events (`internal/harness/event.go`) to stdout,
   but nothing consumes them: `AgentRun.Status.PR` / `.Usage` / `.SessionID`
   have no writer, so `wren run get` can never show `prUrl`. v0.1 design
   (settled): the **operator** reads the harness container's logs
   (`pods/log`, add to the operator's RBAC + `make manifests`) when a pod
   reaches a terminal state **and before deleting a failed pod for resume**,
   extracts the JSON event lines per `internal/harness/event.go`, and writes
   Status.PR / Status.Usage / Status.SessionID. Then verify the apiserver
   mirror path (CR → store → `GET /v1/runs/{id}`) surfaces `prUrl` on a live
   `wren run get` — if the mirror only happens at boot, add a get/list-time
   refresh (CR authoritative for status, never blank known store data — same
   rule as WS-3's `runFromCR`).
   Rationale (record in the PR body): the pod holds no SA token by design, so
   the runner cannot write status; this adds no credentials, endpoints, or
   attack surface. The spec's gateway event-bridge remains the v0.2 target —
   the event schema does not change, so the swap is internal to the pod.
   Edge cases: log tail limits (use a generous tail / since-time), keyless
   runs emit no events (no-op), event lines survive container termination
   while the pod object lives.
4. **Make the e2e no-PR assertion real.** `hack/e2e.sh` greps `"url"` but the
   JSON field is `prUrl` — the assertion is vacuous today. Fix the field
   name; keep asserting empty in keyless mode; add a unit/httptest that a
   run whose CR has `Status.PR` set returns `prUrl` from the apiserver.

## Scope guards

**OUT:** the gateway event bridge (v0.2); session-resume for the claude-code
adapter (`--resume` wiring — separate workstream); usage aggregation/`wren
usage`; event-schema changes; AgentPool fixes.
**Hot files you will own:** `internal/gitwork/gitwork.go`,
`internal/finalize/*`, `internal/podruntime/podruntime.go`,
`internal/controller/agentrun_controller.go`, `config/rbac/*`,
`internal/coreapi/*` (mirror/refresh only), `hack/e2e.sh`.
Do NOT touch: `api/v1alpha1` (Status fields already exist), `internal/egress`,
`internal/harness/event.go` (schema is frozen this round).

## Definition of done

- [ ] Unit: CommitAll-twice idempotency; finalize error-classification
      matrix; operator event-extraction from pod logs (fake clientset with a
      log fixture); apiserver surfaces `prUrl` from a CR-backed status.
- [ ] `make test vet` + `golangci-lint` green; coverage held or better on
      touched packages.
- [ ] `make e2e` green with the corrected assertion (proves it isn't vacuous:
      temporarily assert a bogus field locally and watch it fail — note the
      result in the hand-off).
- [ ] Hand-off lists anything found emitting events that this design misses
      (e.g. mid-run `token_usage` increments — v0.1 records terminal values
      only; say so in the spec status block if it changes wording).
