# Task briefs — how to run a worker

This directory holds one dispatch-ready brief per workstream (see
[`../implementation-plan.md`](../implementation-plan.md) for the designs and
[`../agent-workflow.md`](../agent-workflow.md) for the process). Track state in
[`STATUS.md`](STATUS.md).

## Dispatching a worker (any Claude Code session, cheaper models OK)

1. Create the worktree (from the repo root of the main checkout):

   ```sh
   git worktree add ../wren-ws<N> -b ws<N>-<slug> origin/main
   ```

2. Start a Claude Code session **in that worktree** and paste:

   > You are a worker agent on the Wren project. Read `AGENTS.md` in full
   > (especially the Go PATH gotcha), then read and execute
   > `docs/tasks/WS-<N>-<slug>.md` exactly. Do not expand scope, do not touch
   > files the brief marks as off-limits, and do not re-litigate design
   > decisions — if the brief seems wrong or you get blocked, stop and write
   > your question into the hand-off note instead of improvising. When done,
   > commit to the branch and write the hand-off note (template at the bottom
   > of the brief) as your final message.

3. When the worker finishes: update `STATUS.md`, run `/code-review` on the
   branch, run `make e2e` on the rebased result, then merge (see
   agent-workflow §2.3).

## Model guidance

- **Fine on a cheaper model (well-bounded, mechanical):** WS-0, WS-3, WS-4,
  WS-5, WS-7, WS-8, WS-9, WS-11.
- **Use a stronger model or add a careful human/strong-model review pass:**
  WS-1 and WS-2 (security boundary), WS-12 (credentialed egress routes),
  WS-13 (user-facing UX judgment), WS-6 (UX judgment),
  WS-10 (destructive/mechanical but irreversible — human-gated anyway).
- The **orchestrator/review role** should not be downgraded: the workflow's
  quality control is concentrated there by design.

## Brief states

- **READY** — dispatch as-is.
- **DRAFT** — design is settled (see implementation-plan) but file-level
  details depend on an earlier batch merging; the orchestrator finalizes the
  brief (10–15 min) before dispatch. Do not hand a DRAFT to a worker.

## Hand-off note template (workers: end your session with this)

```markdown
## Hand-off: WS-<N>
- Branch: ws<N>-<slug> @ <commit>
- What changed: <3–6 bullets>
- Validation: <commands run + results; per-brief DoD checklist state>
- NOT verified: <anything the brief asked for that you could not test, and why>
- Questions / candidate cleanups: <or "none">
```
