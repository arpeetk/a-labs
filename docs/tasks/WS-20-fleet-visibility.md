# WS-20: Fleet visibility — a real `run list` + `wren fleet`

**Branch:** `ws20-fleet-visibility` · **Worktree:** `../wren-ws20` · **Size:** M
**State:** READY. **Designed for autonomous execution (no human review gate)** —
see "Autonomous execution rules" at the bottom before starting. Everything
above that section is the same kind of brief every other WS-N in this
directory follows; read the whole thing before writing code.

*Context: the owner's stated mission for Wren is "any engineer can easily
spin up coding agents in the cloud, offload tasks, track their progress, and
orchestrate them." Measured against that, this is currently Wren's biggest
gap — bigger than sandbox isolation or checkpointing. Today an engineer can
only look at one run at a time (`run get`/`run list` dumps raw JSON, no
table, no live status). `wren fleet` existed once as a CLI placeholder and
was deliberately removed in WS-15 for printing "not implemented yet" instead
of working — this workstream is what earns that command back, for real.*

## Design (settled — implement as specified)

1. **`wren run list` gets a real table by default.** Columns: `ID`,
   `PROJECT`, `PHASE`, `HARNESS`, `RESTARTS`, `AGE`, `PR` (dash if none).
   Use `text/tabwriter` the same way `internal/cli/project.go`'s
   `newProjectListCmd` already does — match that exact style, don't invent a
   new table-rendering approach. Sort newest-first. Add `--output json` (only
   accepted value besides the default) to get the old raw-JSON behavior back
   for scripting.
2. **New filters, wired end-to-end, not just accepted and ignored:**
   - `--project <name>` — **server-side**. `store.RunFilter` already has a
     `Project` field (`internal/store/store.go`) that nothing currently sets
     end-to-end — `coreapi.ListRuns` never populates it, the apiserver never
     reads a `project` query param, the CLI never has a flag. Wire all four
     layers (CLI → `internal/client` → `internal/apiserver` → `coreapi.ListRuns`
     → `store.RunFilter.Project`). This is threading an existing field
     through, not adding new storage.
   - `--phase <phase>` — client-side filter (in the CLI, after fetching) is
     fine; don't add a new store query dimension for this, it's not worth the
     surface area yet.
3. **Fix the known staleness gap — this is the part that actually matters.**
   `coreapi.ListRuns` today just reads whatever's in the store, and the
   store's copy of `Phase`/`RestartCount`/`PRURL` is only as fresh as the
   last time something wrote to it — there's no per-list refresh. For a
   feature whose whole point is "track progress," showing stale phase
   information defeats the purpose. **Before designing a fix from scratch,
   check `internal/launcher`'s CR-listing capability built for WS-3's
   reconcile-on-boot** (`ReconcileFromCluster`/`launcher.ListRuns` per
   `docs/tasks/STATUS.md`'s WS-3 history) — it already does a bulk read of
   live `AgentRun` CRs, which is exactly the "one bulk call, not N+1" shape
   this needs. Reuse it if it fits; only build something new if it genuinely
   doesn't (and say why in the hand-off if so). The store stays authoritative
   for fields CRs don't carry (`Prompt`, `CreatedAt`, `User`); CR data wins
   for live status fields when both are available.
4. **`wren fleet` — real command, not a stub.** Registered at the top level
   (`internal/cli/root.go`), same table renderer as `run list`, defaulting to
   `--scope all` (see everything visible, matching "orchestrate them" — M0's
   auth is a trusted header with no real RBAC yet, so "all" is already
   viewable by anyone today; this doesn't create a new exposure, it's
   consistent with the existing M0 stand-in, documented as such if you touch
   that doc). Accepts the same `--project`/`--phase` filters as `run list`.
5. **`--watch` / `-w` on both commands.** Polls on an interval (default
   3s, override via a flag or env if you want, your call) and redraws the
   table in place. Keep it dependency-free — a simple "clear + reprint"
   loop against the existing table renderer is enough, don't pull in a TUI
   library for this. Exits cleanly on Ctrl-C (no partial-line garbage left
   in the terminal).

## Scope guards

**OUT:** interactive steering (`run attach`/`run steer` — M2, a real feature,
not this one); `wren usage` / token-cost aggregation (separate, needs data
this workstream doesn't touch); any change to `RunSpec`, the pod shape, or
any file under `internal/podruntime`/`internal/controller` beyond what's
listed below; any change to `api/v1alpha1` (no new CRD fields — everything
this needs already exists on `AgentRun`); renaming.
**Hot files:** `internal/cli/run.go`, `internal/cli/root.go` (register
`fleet`), `internal/client/client.go`, `internal/apiserver/server.go`,
`internal/coreapi/service.go`, `internal/launcher/*` (only if reusing/
extending the WS-3 CR-listing path), `SETUP.md`, `README.md` (the "Interactive
`attach`/`steer`, the `fleet` dashboard... are on the roadmap" line becomes
false once `fleet` ships — fix it, same discipline as the markdown-accuracy
pass earlier this project), `docs/tasks/STATUS.md`.
**Explicitly forbidden, not just out of scope:** `internal/podruntime/lockdown.go`,
`internal/egress/*`, anything with uid pinning or the security hardening
this project has spent multiple workstreams proving. There is no legitimate
reason this workstream touches those files. If you find yourself needing to,
stop and write down why in the PR instead of proceeding — that's a sign
something has drifted, not a green light.

## Definition of done

- [ ] `wren run list` default output is a clean, sorted table (tabwriter,
      matching `project list`'s existing style exactly).
- [ ] `--output json` reproduces the previous raw-JSON behavior.
- [ ] `--project <name>` filters server-side, wired through all four layers.
- [ ] `--phase <phase>` filters correctly (client-side is fine).
- [ ] **Live-verified freshness, not just unit-tested:** on a real kind
      cluster, submit a run and watch its phase transitions (Pending →
      Provisioning → Running → Succeeded) actually show up in `run list`/
      `fleet` output as they happen — not the last value the store happened
      to have mirrored. This is the load-bearing claim of the whole
      workstream; don't consider it done on unit tests alone.
- [ ] `wren fleet` is real: registered, documented in `--help`, shows all
      visible runs in the same table format, accepts the same filters.
- [ ] `--watch`/`-w` works on both commands: polls, redraws cleanly, exits
      cleanly on Ctrl-C, verified by actually running it against a live
      cluster and watching it update, not just reading the code.
- [ ] Unit tests: the project-filter plumbing at each layer, the phase
      client-side filter, table-rendering output (a golden-output-style
      test is fine), all hermetic (no live cluster needed for these).
- [ ] `make test vet` + `golangci-lint run` green.
- [ ] `make e2e` green, unaffected (this is additive — the keyless gate's
      own path shouldn't need to change).
- [ ] `README.md`'s "fleet dashboard... on the roadmap" line and any similar
      stale claims are corrected now that it's real.
- [ ] `docs/tasks/STATUS.md` updated with this workstream's result, matching
      the existing row format/style for every other WS-N entry.

## Autonomous execution rules (read this before starting)

This brief is written for a **build → test → validate → commit loop with no
human review gate** — unlike every security-critical workstream in this
project's history (egress enforcement, the GCS mount work), which waited for
an orchestrator's review before merging. That's a deliberate choice here
because this workstream is additive, read-mostly, and doesn't touch anything
security-sensitive — not a general license to skip validation rigor.

- **Work in the existing convention:** worktree `../wren-ws20`, branch
  `ws20-fleet-visibility`, branched from latest `main`. Read `AGENTS.md` in
  full first, same as every other workstream.
- **Iterate genuinely:** implement one coherent slice (e.g., the table
  renderer; then the project filter; then the freshness fix; then `fleet`;
  then `--watch`), validate that slice, commit, move to the next. Don't
  write the whole thing in one uncommitted pass.
- **The bar for "safe to merge" is the full Definition of Done above,
  including the two live-cluster checks (freshness, `--watch`) — not unit
  tests alone.** Several real bugs in this project's history were invisible
  to unit tests and only surfaced by actually running the thing on a live
  cluster (a Deployment-update race, a harness image that only resolved on
  kind by coincidence, a DNS-rebinding gap). Assume the same is possible
  here and check for real rather than trusting that the code looks right.
- **You may merge your own PR once every Definition of Done item is
  genuinely, verifiably true** (not "should work" — actually run and
  observed). Still open a PR (`gh pr create`) rather than pushing straight
  to `main`, so there's a normal diff and description to inspect afterward;
  update `docs/tasks/STATUS.md` in the same merge, matching how every prior
  workstream closed out.
- **If genuinely blocked on a decision only the owner can make** (not a
  technical unknown you can resolve by investigating — an actual product or
  scope judgment call), stop and write the blocker down clearly rather than
  guessing past it or looping indefinitely.
- **Tear down anything you stand up** — kind clusters, stray port-forwards —
  before considering a slice done, same discipline as everywhere else in
  this project.
