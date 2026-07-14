# WS-8: Claims truthing + checkpointing de-scope

**Branch:** `ws8-claims-truthing` · **Worktree:** `../wren-ws8` · **Size:** S · **State:** READY (dispatch once WS-1's outcome is known — its result changes the wording)

## Context (read first)

- `AGENTS.md`; `docs/implementation-plan.md` §WS-8 (the decision: v0.1 resumes
  via PVC re-attach only; GCS/S3 checkpointer is post-launch).
- `docs/technical-spec.md` §5.5 (what's claimed) vs `internal/podruntime`
  (checkpointer is a sleep-stub).

## Scope

**IN:**

1. **Docs truthing** across README (status table + prose), spec living-status,
   and spec §5.5: crash-resume = PVC survives → reattach + session-resume
   flag; node/zone loss without a surviving PVC = clean `Failed` with
   diagnostics; `workspace.checkpoint.*` fields are accepted but **no-op in
   v0.1** (say so where the fields are documented, including
   `config/samples/agentrun.yaml` comments).
2. Label the checkpointer sidecar experimental in code comment + spec; do NOT
   remove it (pod shape stays stable).
3. **`internal/blob/blob.go`:** define the `Store` interface only (Put/Get/List
   under a run-scoped prefix — mirror what spec §5.5 needs), with a doc comment
   naming the intended impls (S3-compatible, GCS; MinIO for e2e). No
   implementations, no wiring.
4. Sweep for other claim/reality gaps while you're in there (e.g. rubric
   *validation* is not implemented; `attach`/`steer`/`fleet`/`usage` are
   milestone-tagged) — fix wording only where it overclaims, list anything
   ambiguous in the hand-off rather than rewording unilaterally.

**OUT:** implementing any checkpointing; removing CRD fields (API stability —
they stay, documented as no-op); README restructuring beyond accuracy edits
(WS-9/launch owns the rewrite).

## Hot files

You own: `README.md`, `docs/technical-spec.md`, `internal/blob/` (new),
`config/samples/agentrun.yaml` (comments only).
Do NOT touch: any behavior code, `api/v1alpha1/*`, `internal/controller/*`.
Coordinate: README/spec are contended files — rebase immediately before
hand-off and keep edits surgical.

## Definition of done

- [ ] `make test vet` green (blob interface compiles, has a doc example).
- [ ] Grep-audit in the hand-off: every occurrence of "checkpoint",
      "durable", "survives" in README/spec quoted with its post-edit state —
      demonstrating no overclaim remains.
- [ ] Hand-off note with the ambiguous-wording list.
