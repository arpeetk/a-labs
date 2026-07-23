# Testing standards

How we test in this repo. Each rule exists because a specific bug taught it —
the incident is cited so the rule never becomes ceremony. Read with
[code.md](code.md) and [review.md](review.md).

## The rules

1. **Every assertion must be proven capable of failing.** A check that cannot
   fail is worse than no check — it documents a guarantee it doesn't make.
   `hack/e2e.sh` grepped `"url"` while the JSON field is `prUrl`, so the
   "no PR in keyless mode" assertion was vacuous *from the day it was written*
   (WS-0 → WS-11). For any assertion you add — e2e grep, test comparison,
   canary probe — prove it fails: point it at a bogus value, watch it fail,
   revert. Record the proof in the PR.

2. **Fakes can't model production — know the blind spot.** The
   controller-runtime fake client performs **no apiserver validation**: the
   AgentPool controller built a pod a real apiserver would reject
   (`RestartPolicy` on a non-init container) and its tests were green for
   weeks. When behavior depends on server-side validation/admission, either
   cover it in `make e2e` (kind runs a real apiserver) or write the
   assumption in a comment at the seam. "Green against fakes" ≠ "works."

3. **Hermetic tests.** No network, no credentials, no ambient state.
   controller-runtime `fake` client for reconcilers, `httptest` for HTTP and
   mocked GitHub APIs, local bare git repos for gitwork/finalize, injected
   `now`/`idgen`/clients via unexported seams. If a test needs the internet,
   it belongs in the e2e gate, not `go test`.

4. **Interface + real impl + fake.** Every external dependency (Kubernetes,
   GitHub, the store, pod logs) sits behind a small interface with a real
   implementation and an in-memory fake; business logic depends on the
   interface. When you add a dependency seam, you add all three in the same
   change (`store`, `launcher`, `github`, `controller.LogReader`).

5. **Idempotency is a tested property, not a hope.** Anything a resume,
   requeue, or retry can re-execute must have a "run it twice" test:
   `TestCommitAllTwiceIsIdempotent` exists because a crash between commit and
   push turned "branch already exists" into a terminal failure (WS-11).
   Reconcilers, finalize, CreateOrUpdate paths — write the second-call test.

6. **Error classification gets a matrix.** Retryable-vs-permanent is a
   taxonomy, and taxonomies get table-driven tests (WS-11's 24-case
   `retry_test.go`: network/429/5xx/EOF → retryable; 401/403/422/
   non-fast-forward → deterministic). A misclassified transient error kills a
   run with budget to spare; a misclassified permanent one re-spends agent
   tokens.

7. **Conformance suites for contracts.** One suite, every implementation:
   `internal/store/conformance_test.go` runs identical semantics against
   memory and Postgres. When you add a second implementation of anything,
   factor the first's tests into a shared suite in the same change.

8. **Security controls get tripwires with teeth.** The egress canary attempts
   a direct connection that *must* fail, and a bypass fails the run (WS-1).
   Enforcement code also fails closed on its own errors (missing ip6tables on
   a live v6 stack aborts the pod). Test both directions: the control holds,
   and the control's own failure is loud.

9. **The gate is `make e2e`, run on the rebased result.** Unit tests are
   necessary, not sufficient. Every PR rides the keyless kind e2e in CI; run
   it locally before hand-off (`KIND_CLUSTER` / `APISERVER_LOCAL_PORT`
   overrides allow parallel runs on one machine). Coverage bar: logic
   packages stay ≥ mid-70s and only move up; tests ship in the same change as
   the code — never deferred.
