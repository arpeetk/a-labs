# Wren — Parallel Agent Workflow

> **Status:** v1 · **Date:** 2026-07-14 · **Companion:**
> [`implementation-plan.md`](implementation-plan.md) defines *what* to build
> (WS-0…WS-10); this document defines *how* we execute it fast with multiple
> Claude Code agents working in parallel — which is fitting, since it's exactly
> the workflow Wren itself productizes.

---

## 1. The shape: hub-and-spoke, not a swarm

One **orchestrator** session (interactive, with you) plus N **worker** agents,
each owning exactly one workstream in an isolated git worktree. Workers never
talk to each other; all integration flows through the orchestrator and `main`.
This avoids the two classic failure modes of agent parallelism: merge-conflict
thrash (solved by the hot-file map, implementation-plan §12) and quality drift
(solved by a single review/validation gate).

| Role | Who | Owns |
|---|---|---|
| **Orchestrator** | the main Claude Code session + you | task briefs, dispatch, batch sequencing, review, merges, keeping `main` green, spec/README truth-keeping |
| **Worker (implementer)** | one background agent per workstream, in its own worktree | the branch, the code, tests, self-validation, a PR-shaped hand-off |
| **Reviewer** | `/code-review` on each hand-off (orchestrator-driven) | correctness bugs, simplification |
| **Validator** | `make e2e` on the merge candidate | the keyless kind e2e (WS-0) — the objective gate |
| **Human (you)** | decisions & credentials | name/license/org, GitHub App creation, `gcloud auth login`, merge button on security-sensitive PRs |

**Prerequisite: WS-0 before parallelism.** The e2e loop is the force
multiplier — without it every worker "validates" by reasoning instead of by
running, and the orchestrator becomes the bottleneck re-testing everything.
Build `make e2e` first, alone, then fan out.

---

## 2. Mechanics

### 2.1 Worktrees, one per workstream

```sh
git worktree add ../wren-ws1 -b ws1-egress-enforcement origin/main
git worktree add ../wren-ws3 -b ws3-postgres-store    origin/main
...
```

Each worker runs in its own worktree — dispatched from the orchestrator as a
background agent with worktree isolation, or as separate terminal
sessions/`claude` instances if you prefer watching them. Worktrees share the
object store, so branches and rebases are cheap. **Always branch from
`origin/main`** (the local-main gotcha in AGENTS.md is load-bearing here).

### 2.2 Task briefs are files, not prompts

Each dispatch is a markdown brief at `docs/tasks/WS-<n>-<slug>.md`, checked in
so it survives sessions and is auditable. The worker's prompt is essentially
"read AGENTS.md, then execute docs/tasks/WS-1-egress-enforcement.md". Template:

```markdown
# WS-<n>: <title>
**Branch:** ws<n>-<slug> · **Worktree:** ../wren-ws<n> · **Size:** S/M/L

## Context (read first)
- AGENTS.md (toolchain, PATH gotcha, conventions), then:
- implementation-plan.md §WS-<n> — the design; do not re-litigate decisions
- Spec sections: §…

## Scope
IN:  <the design bullets from the implementation plan>
OUT: <explicitly excluded — the adjacent work someone else owns>

## Hot files
You own: <files>. Do NOT touch: <files owned by other live workstreams>.
If you believe you must, STOP and report back instead.

## Definition of done
- [ ] `make test vet` green; new logic has tests at the package's existing bar
- [ ] `make e2e` green (if the run path is touched)
- [ ] spec living-status + README status table updated (or noted for orchestrator)
- [ ] hand-off note: what changed, how validated, what you're unsure about
```

The "OUT" and "hot files" sections are what make parallel dispatch safe —
write them carefully; they are the orchestrator's real job.

### 2.3 The loop (per batch)

```
orchestrator: write briefs ─► dispatch workers (parallel, background)
     ▲                                        │
     │                              workers: implement → self-validate
     │                                        │ (make test vet [e2e])
     │                              hand-off: branch + note
     │                                        │
     └── merge ◄─ validator: make e2e ◄─ reviewer: /code-review ◄─┘
         │
         └─► rebase remaining live branches on new main → next dispatch
```

- **Review:** run `/code-review` against each hand-off branch; apply or bounce
  findings back to the *same* worker (continue its session — it has the
  context) rather than fixing in the orchestrator, except for trivia.
- **Merge order within a batch:** whoever touches shared wiring files merges
  first (per the hot-file map); others rebase. Small PRs (< ~600 lines diff)
  keep rebases trivial.
- **Cadence:** integrate at least daily. A worktree that drifts >1 day from
  main is a liability; rebase it or kill and re-dispatch (briefs make
  re-dispatch cheap — that's another reason they're files).
- **Security-sensitive diffs** (WS-1, WS-2, anything under `internal/egress`)
  get a human read before merge, always.

### 2.4 Validation is layered, cheapest first

1. Worker: `make test vet` (+ package coverage at the existing bar — see the
   coverage norms in AGENTS.md).
2. Worker, if run-path: `make e2e` in its own kind cluster
   (`KIND_CLUSTER=wren-ws<n>` so parallel workers don't collide).
3. Orchestrator: `/code-review` + `make e2e` on the rebased merge candidate.
4. CI (WS-7) runs the same commands — local green ⇒ CI green, by construction.
   Never let CI check things the local loop doesn't; that's how "works in my
   worktree" happens.

---

## 3. Batch dispatch plan (mirrors implementation-plan §11)

| Batch | Dispatch in parallel | Serialized because |
|---|---|---|
| 0 | WS-0 alone (orchestrator or one worker) | it's the gate everything else uses |
| 1 | WS-1 · WS-3 · WS-4 · WS-7 | disjoint packages; WS-1 owns `pod.go` |
| 2 | WS-2 · WS-5 · WS-8 | WS-2 waits for `pod.go`; WS-5 for stable manifests |
| 3 | WS-6 · WS-9 | need chart / true claims respectively |
| 4 | WS-10 alone | mechanical rename + repo cut; human-gated |

Practical concurrency: **3–4 live workers max.** Beyond that, orchestrator
review becomes the bottleneck and quality drops — the constraint is review
bandwidth, not compute. If a batch has more workstreams than slots, dispatch
by risk (security first) so the scary diffs get the most soak time on main.

Kick off human-gated items **at batch-1 time**, they have lead time: name
shortlist + org creation, GitHub App creation, lining up an external reader
for the egress path, recruiting the quickstart stranger-tester.

---

## 4. Working agreements (what keeps this fast)

1. **Briefs decide, workers execute.** Design questions discovered mid-build
   come back to the orchestrator as a question in the hand-off, not as an
   unrequested design change in the diff.
2. **No drive-by refactors.** Adjacent-code cleanups get a line in the
   hand-off note ("candidate cleanup: …") and become future S-sized tasks.
3. **Tests ride with the change, at the package's existing standard** — this
   codebase's bar (fake clients, httptest, local bare repos, ~85%+ on logic
   packages) is a launch asset; parallelism must not erode it.
4. **The spec stays true.** Any behavior change updates the living status
   block in the same PR (or flags it for the orchestrator if the file is
   contended). The built-vs-designed honesty is part of the product.
5. **Every hand-off states what was *not* verified.** "e2e green, but I could
   not test the GKE Autopilot path" beats silent confidence — the
   orchestrator maintains the list of deferred verifications and burns it
   down before launch.
6. **Kill stuck workers early.** If a worker is spinning (wrong approach,
   fighting the toolchain), stop it, improve the brief with what you learned,
   re-dispatch fresh. Sunk context is cheaper to abandon than to steer.

---

## 5. Bootstrap checklist (first session of the sprint)

- [ ] Merge this doc set; create `docs/tasks/`.
- [ ] Orchestrator (or first worker) builds WS-0 (`make e2e`); merge.
- [ ] Write briefs WS-1/3/4/7 from implementation-plan §§WS-1–7; create the
      four worktrees; dispatch batch 1.
- [ ] You: start the name shortlist + create the GitHub org placeholder +
      GitHub App (test instance) in parallel.
- [ ] Daily: review hand-offs → merge → rebase → re-dispatch. Track batch
      state in the task list or a pinned `docs/tasks/STATUS.md` one-liner
      table (workstream · state · blocker).
