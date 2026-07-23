# Code standards

How we write and add code in this repo. Incidents cited so rules stay honest.
Read with [testing.md](testing.md) and [review.md](review.md).

## The rules

1. **Security boundaries hold by construction, not by convention.** The
   egress uid-boundary was "the image happens to run as 65532" — until a
   review showed a harness image with `USER 65533` would inherit the proxy's
   iptables exemption (PR #16, F1). Pin invariants in code (`RunAsUser` in
   the pod spec), and have a test assert the pin — never trust an image
   default, an env var, or a comment to carry a security property.

2. **Fail closed, and fail loud.** On security paths, an error means stop:
   lockdown aborts the pod on any rule error; a missing ip6tables on a live
   IPv6 stack is an error, not a skip. And a failure nobody can see is a
   failure doubled: an invalid `--egress-enforcement` logged before the
   logger existed and exited 1 *silently*. Validation errors go to stderr;
   permanent admission rejections (Forbidden) fail the run deterministically
   with the remedy in the message — never requeue-and-hang.

3. **Single source of truth for every coordinate.** The GKE e2e once had the
   image registry/tag in three places (Makefile var, committed overlay,
   script literal) that disagreed by default. Compute coordinates once, pass
   them explicitly, and never bake environment-specific literals (registries,
   projects, tags) into committed manifests.

4. **Errors:** wrap with `%w` and context; export sentinels (`ErrNotFound`,
   `ErrValidation`, `ErrNoChanges`, `ErrRetryable`); map them deliberately at
   the transport boundary (`apiserver.writeServiceErr`); use `errors.Is/As`,
   never `==`, on sentinels — third-party wrappers break `==`.

5. **Status writes fill, never blank.** The CR is authoritative for status;
   the store mirrors it. Scraped/mirrored data only adds information
   (`runFromCR`, the WS-11 log scrape). A consumer that can blank a field is
   a data-loss bug waiting for a restart.

6. **Comments explain *why*, not *what* — and carry the load-bearing
   invariant.** Reference the spec section (e.g. "spec §5.6"); name the
   invariant ("never collapse these two — the uid gap is the security
   boundary"); document the escape hatch where the escape is. Match
   surrounding density.

7. **Minimal, surgical diffs.** Do the thing the change is for. Drive-by
   refactors go in their own PR with "chore" in the title. When you must
   exceed the brief to fix the root cause (WS-11's `ensureBranch`), fix the
   root cause and flag the expansion in the hand-off.

8. **Dead code dies.** Stubs that never ran (AgentPool), scripts superseded
   by real features (`hack/setup.sh`), unreachable flags and placeholder
   paths: remove them in the same sweep that makes their replacement real.
   Onboarding/install/setup are product surface — they live in the CLI as
   first-class commands, never in `hack/` (`hack/` is dev/test tooling only:
   e2e gates, codegen helpers).

9. **M0 stand-ins are labelled.** Every deliberate simplification gets an
   explicit note in `AGENTS.md` §8 and the spec status block, with its
   target. When you make one real, remove the note in the same PR (this is a
   written repo rule — it has been forgotten twice).

10. **API stability once fields exist.** Accepted-but-unimplemented CRD
    fields stay and are documented as no-op (checkpoint fields); they are not
    removed on a whim. Removing any API surface is an orchestrator-level
    decision, recorded in the PR.
