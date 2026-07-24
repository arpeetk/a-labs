# WS-16: Robustness cleanup pass

**State:** READY. Two independent parts with **no file overlap** — safe to run
as two parallel workers, unlike WS-14/15.

- **Part A** — Branch `ws16a-robustness-fixes` · Worktree `../wren-ws16a` · Size M
- **Part B** — Branch `ws16b-launcher-install-coverage` · Worktree `../wren-ws16b` · Size S/M

*Context: owner asked for a whole-repo pass to make the codebase "really
strong, robust, and clean," after the onboarding/multi-harness push (WS-12
through WS-15) landed. A `deadcode` static-analysis sweep and a TODO/FIXME
grep across the whole module turned up almost nothing — the recent dead-code
sweeps (#22) and claims-truthing (WS-8) already did that job, so this is NOT
another dead-code hunt. The real remaining value is a list of items that were
already identified during WS-1/WS-7/WS-8/WS-11's review rounds and logged in
`docs/tasks/STATUS.md`'s ledger but never actually fixed, plus one new test
coverage gap found by re-checking `internal/launcher` — the exact class of
package (real-k8s-API glue, thin fake coverage only) where two live bugs
were already found this session by actually running on GKE, not by unit
tests. Every item below was spot-checked against current code before writing
this brief (2026-07-24) — still accurate. Rename to skein is deferred — keep
building as `wren`.*

---

## Part A — accumulated robustness/doc follow-ups

All four items below are independent of each other; do them in any order,
one PR (or several small ones on the same branch, your call).

### A.1 — `hack/e2e.sh` / `hack/e2e-gke.sh` duplication

Verified: 225 + 298 lines, near-identical preamble/structure/patterns
(cluster reachability checks, image build invocation, teardown trap
handling). Extract the shared parts into `hack/lib/e2e-common.sh`, sourced by
both. Don't change either script's actual behavior/flags — this is a
refactor, not a rewrite. `make e2e` must stay green; if you can exercise
`hack/e2e-gke.sh` structurally (it needs a real GKE cluster to fully run —
that's fine, static/shellcheck-level correctness is enough here, don't spin
up GCP resources for this).

### A.2 — apiserver `http.Server` timeouts

`cmd/wren-apiserver/main.go`'s `http.Server` only sets `ReadHeaderTimeout`
(10s). Add `ReadTimeout`, `WriteTimeout`, and `IdleTimeout` — standard
hardening against slow-client resource exhaustion (Go's `http.Server` doc
explicitly recommends this; a server with no `ReadTimeout`/`WriteTimeout` is
vulnerable to slowloris-style connection exhaustion). Pick reasonable
defaults (e.g. 30s read/write, 120s idle) given this serves both quick JSON
calls and the long-lived `run logs -f` streaming endpoint (WS-4) — the
streaming case must not get cut off by `WriteTimeout`; check
`internal/apiserver/server.go`'s logs handler and make sure your timeout
choice doesn't break `TestK8sStreamLogs`-style long-poll/streaming behavior
(consider whether `WriteTimeout` needs to be disabled/very large specifically
for the streaming path, e.g. via a per-handler override or `http.ResponseController`,
rather than one global value that breaks log tailing — investigate before
picking values).

### A.3 — egress proxy CONNECT resolved-IP guard

`internal/egress/proxy.go`'s CONNECT handling (the allowlist forward-proxy
path) validates the requested hostname but — per the WS-1 review follow-up —
doesn't guard against DNS rebinding (hostname resolves to an allowlisted
domain at check-time but a different, non-allowlisted IP at connect-time).
Add a check that the resolved IP the actual `net.Dial` connects to is
re-validated (or resolve once and dial the resolved IP directly, not the
hostname, so there's no TOCTOU window). Add a test proving a rebinding
attempt is rejected. This is defense-in-depth on top of the iptables
lockdown (WS-1's primary control) — don't weaken or change the lockdown
container itself, this is proxy-side only.

### A.4 — `ensurePVC` disk-loss behavior vs. documentation

`internal/controller/agentrun_controller.go:ensurePVC` (~line 156): on a
`NotFound` PVC lookup, it unconditionally creates a **fresh, empty** PVC.
But per WS-8's truthing pass, the docs say a disk-destroying loss (PVC/PD
gone) should end the run `Failed`, not silently resume into an empty
workspace with no signal anything was lost. Pick one and make code match
docs:
- **(recommended)** Detect this case — the AgentRun has already progressed
  past `Provisioning` (i.e. this isn't the PVC's first-ever creation) but its
  PVC is now `NotFound` — and transition the run to `Failed` with a clear
  condition/message instead of silently recreating. Use existing phase
  tracking (check how the reconciler already distinguishes first-run vs.
  resume elsewhere, e.g. around restartCount/PVC-name-stability logic
  mentioned in `AGENTS.md` and the controller tests) rather than inventing a
  new signal.
- Alternative: if that distinction turns out to be more invasive than it's
  worth, re-document instead (change the docs to describe actual resume-into-
  empty-workspace behavior, explain why, and note the risk) — but prefer the
  code fix; this is a real correctness/data-loss-surprise issue, not just a
  doc nit.
Add a regression test for whichever you choose.

### A.5 — three stale doc/help-text spots (quick, bundle in)

- `internal/cli/run.go` help text references a "usage" field/output that
  doesn't exist yet (token/cost usage reporting was one of the CLI commands
  removed entirely in WS-15 as unbuilt roadmap — this reference is now doubly
  stale). Fix or remove the reference.
- `internal/store` package doc comment says Postgres support "lands later" —
  stale since WS-3 shipped it. Update.
- `SETUP.md`'s framing in one section predates `config/default` (mentions a
  "Phase 2" that no longer matches the current install flow after WS-13/14/15).
  Reconcile with the current `wren install` flow described earlier in the
  same file.

## Part B — close the launcher/install real-implementation test gap

**Finding:** `go test -cover ./internal/launcher/...` shows 57.1% — the
lowest in the repo. Breaking it down with `go tool cover -func`: the **real**
`K8s` launcher's `RequestCancel`, `SecretHasKey`, `ListRuns`, and `NewK8s` are
all at **0%** coverage. `internal/coreapi`'s tests exercise these code paths
only through `launcher.Fake` (a hand-written stand-in), never through the
real k8s-API-backed implementation. This is exactly the category of gap that
produced two live-only bugs this session (a Deployment update race, and a
harness-image default that only worked by kind-specific coincidence) — code
that only "flies" because a fake behaves too politely to catch what the real
Kubernetes API does.

The package already has the right pattern to extend: `launcher_test.go`'s
`TestK8sStreamLogs` (and `TestK8sLauncher`) use `k8sfake "k8s.io/client-go/kubernetes/fake"`
/ a fake dynamic client to test the real `K8s` type without a live cluster.
Follow that exact pattern, don't invent a new one.

**Do:**
1. Add `TestK8sRequestCancel` — real `K8s.RequestCancel` against a fake
   dynamic client seeded with an AgentRun; assert it sets `CancelAnnotation`
   (`api/v1alpha1.CancelAnnotation`, added in WS-15) via the same merge-patch
   approach `internal/coreapi.StopRun`'s live GKE testing proved necessary
   (a plain `Update` raced the operator in live testing — if `RequestCancel`
   itself does a get-then-update rather than a patch, apply the same
   `retry.RetryOnConflict` or patch-based fix `internal/install/kube.go` just
   got, and prove it with a test that seeds a conflicting concurrent write).
2. Add `TestK8sSecretHasKey` — both branches (key present, key absent,
   Secret entirely absent → `false, nil` not an error, per how
   `coreapi.checkCredentials` treats it as best-effort) against a fake
   clientset.
3. Add `TestK8sListRuns` — real `ListRuns` against a fake dynamic client with
   a few seeded AgentRuns, covering the scope filtering.
4. Add a `TestNewK8s` (or fold coverage into an existing constructor test) —
   at minimum confirm it doesn't panic and wires the clients it's supposed to.
5. **Also close `internal/install`'s new gap**: the `retry.RetryOnConflict`
   wrapping just added to `OverrideImages`/`SetApiserverRunNamespace` (see
   `internal/install/kube.go`, commit `d9ede69`) has no test proving the
   retry path itself — existing `FakeKube`-based tests only exercise the
   success-on-first-try path. Check whether `internal/install/kube_test.go`
   (or wherever the real `realKube` gets tested — check current file layout,
   it may test via a fake clientset like launcher does) can seed a
   `k8sfake.NewSimpleClientset()` with a reactor that returns one `Conflict`
   error before succeeding, and assert the retry actually recovers. If
   `realKube` isn't currently tested against a fake clientset at all (only
   `FakeKube`, the hand-written interface stand-in, is used elsewhere), add
   a minimal test file for it following the launcher package's fake-clientset
   pattern rather than skipping this — an untested retry path is exactly as
   risky as an untested non-retry path.

## Scope guards (both parts)

**OUT:** any new feature work, any change to `RunSpec`/pod shape/API types
beyond what A.4 strictly requires, GitHub App (WS-2), Helm (WS-5), renaming.
Don't touch `internal/cli/*` command surface (WS-15 just finished that) except
the one help-text fix in A.5.
**Part A hot files:** `hack/e2e.sh`, `hack/e2e-gke.sh`, `hack/lib/` (new),
`cmd/wren-apiserver/main.go`, `internal/apiserver/server.go`,
`internal/egress/proxy.go`, `internal/controller/agentrun_controller.go`,
`internal/cli/run.go`, `internal/store/store.go` (or wherever its package doc
lives), `SETUP.md`.
**Part B hot files:** `internal/launcher/launcher_test.go` (or a new
`launcher_k8s_test.go`), `internal/install/kube_test.go` or a new
`internal/install/kube_realclientset_test.go`.

## Definition of done

**Part A:**
- [ ] `hack/e2e.sh`/`hack/e2e-gke.sh` share `hack/lib/e2e-common.sh`; `make e2e`
      still green; both scripts pass `shellcheck` (or note if it's not
      already in CI and whether to add it — check `.github/workflows/`).
- [ ] apiserver has real read/write/idle timeouts; `wren run logs -f` streaming
      still works end to end (verify live against a kind cluster — this is
      exactly the kind of thing a wrong timeout value silently breaks).
- [ ] CONNECT resolved-IP guard added with a rebinding-attempt test; `make e2e`
      both enforcement modes still green.
- [ ] `ensurePVC` behavior matches documentation (or docs updated to match
      code, with reasoning) — regression test included.
- [ ] All three stale doc/help-text spots fixed.
- [ ] `make test vet` + lint green; `make e2e` green.

**Part B:**
- [ ] `internal/launcher` coverage materially improves (target: real `K8s`
      type's new methods at parity with the rest of the package, not 0%).
- [ ] `internal/install`'s retry-on-conflict path has a real test proving the
      retry recovers from a genuine conflict, not just documentation-by-comment.
- [ ] `make test vet` + lint green; no behavior changes — this part is test-only.

## Suggested dispatch

Parallel-safe: Part A and Part B touch disjoint files. Dispatch as two
separate worker agents in separate worktrees. Both are mechanical/
well-specified enough for a standard-capability worker — no need for the
top-tier model reserved for judgment-heavy design work (that was WS-15).
