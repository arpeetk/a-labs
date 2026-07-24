# WS-15: Onboarding friction pass + CLI surface cleanup

**Branch:** `ws15-onboarding-cli-cleanup` · **Worktree:** `../wren-ws15` · **Size:** L
**State:** READY · **Blocked on:** WS-14 merged to `main` — this workstream edits
the same hot files (`internal/install/steps.go`, `internal/coreapi/service.go`,
`internal/cli/project.go`, `internal/cli/run.go`) and must branch from the
post-WS-14 `main`, not before.

*Context: owner's bar is "obvious to set up... as few steps as possible." This
workstream is a from-scratch re-walk of the onboarding path plus a CLI-surface
audit — both done by re-reading the actual code (not docs) and running
`wren --help` end to end, findings below. This is judgment work (what to cut,
how to fix a namespace default without breaking multi-tenant isolation, error
message design) — run this as one agent, not split across parallel workers;
every finding below touches overlapping files. Rename to skein is deferred —
keep building as `wren`.*

## Part A — the namespace credential footgun (highest priority)

**The bug, verified in code:** `wren install --run-namespace` (default
`wren-runs`) is where `wren install` writes the `GITHUB_TOKEN`/
`ANTHROPIC_API_KEY` Secrets. But `coreapi.DefaultDefaults().NamespacePrefix`
gives every project registered without an explicit `--namespace` a *different*
namespace: `user-<sanitized-email>`. If an engineer follows the onboarding
hand-off and omits `--namespace`, their project's runs land in a namespace with
no credential Secrets. `internal/controller/pod.go:secretEnv` mounts those
Secrets with `Optional: true` ("optional so a missing Secret does not block the
pod") — so the pod starts fine, the egress-proxy just has no real credentials
to inject, and the run fails later in a way that does not point back at the
actual cause. This is exactly the kind of thing that burns a team's first 20
minutes and produces a support ping instead of a working demo.

**Fix (two parts, do both):**

1. **Make the install's run-namespace the server's actual default**, not just
   documentation. `wren install` already knows `--run-namespace` at install
   time and already patches the apiserver Deployment (see
   `internal/install/kube.go:OverrideImages` for the existing pattern of
   patching container args/env on the Deployment). Extend that pattern: have
   install set the apiserver's default project namespace (an env var or flag
   read by `coreapi.DefaultDefaults()` / `internal/apiserver` at startup) to
   match `--run-namespace`. Result: `wren project create` with no `--namespace`
   flag lands in the same namespace install put the credentials in, for the
   common single-shared-namespace case. `--namespace` remains available as an
   explicit override for anyone who wants per-user/per-team namespace
   isolation — don't remove that capability, just fix the default.
2. **Fail loud, not silent, when it's still wrong.** Add a check — at
   `run create` time is the highest-leverage point, since that's the last
   moment before a doomed pod gets scheduled — that the resolved run namespace
   actually has the Secret(s) the resolved harness needs (skip for `mock`
   harness and keyless/no-repo projects, which legitimately need nothing).
   Use the apiserver's existing k8s clientset (it already has one for CR
   creation) to do a Secret existence/key check and return a clear 4xx with a
   message like: `project "payments-api" has no GitHub token in namespace
   "user-you@corp.com" — did you mean --namespace wren-runs (the install's
   --run-namespace)?`. This turns a silent multi-minute debugging session into
   an immediate, actionable error.

## Part B — cut required flags to the true minimum

`coreapi.DefaultDefaults()` already supplies harness (`claude-code`), model,
cpu (`2`), memory (`4Gi`), disk (`10Gi`) — none of these need to appear in the
onboarding example. Once WS-14 also fixes the harness-image default, the
minimum viable `wren project create` becomes just `<name> --repo owner/repo`.
Update:
- `internal/install/steps.go:handOff()` — the printed example command — to the
  true minimum, not the fully-spelled-out version it prints today.
- `SETUP.md` and `README.md`'s engineer-onboarding examples to match (they must
  agree with what `handOff()` actually prints — that's the copy-pasted source
  of truth, keep both in lockstep with it, not the other way around).

## Part C — CLI surface cleanup

Verified by running `wren --help`, `wren project --help`, `wren run --help`
directly:

| Scope | Currently fake (`(not implemented yet)`) |
|---|---|
| top-level | `fleet`, `mcp` (+ its 3 children), `usage` — 3 of 11 top-level entries |
| `wren project` | `get`, `config` — 2 of 4 |
| `wren run` | `attach`, `resume`, `rm`, `steer`, `stop` — **5 of 9**, more than half |

An engineer reading `wren run --help` today has better than even odds of
picking a command that just errors. Split these by what they actually need:

**Implement for real (small, and genuinely useful for v1 — don't just delete
the promise, ship it):**
- `wren project get <name>` — likely a thin CLI wrapper; check whether the
  apiserver already supports a by-name lookup (`GET /v1/projects/{name}`) or
  needs one added — should be cheap either way.
- `wren run rm <run-id>` — delete a run's store record + its `AgentRun` CR (the
  CR delete already cascades pod/PVC cleanup via owner refs — confirm this,
  don't re-derive cleanup logic).
- `wren run stop <run-id>` — mark a run Cancelled / delete its current pod
  without triggering auto-resume (the reconciler's restart logic needs to know
  the difference between "pod died, retry" and "user asked to stop" — check
  `internal/controller/agentrun_controller.go`'s phase handling for the
  cleanest way to signal this, e.g. a phase or condition the reconciler treats
  as terminal).
- `wren run resume <run-id>` — **only if cheap.** The operator already
  auto-resumes retryable failures internally; check whether exposing a manual
  trigger for a `Failed`/terminal run is just "reset restartCount, flip phase
  back to Provisioning" (reuse of existing reconciler logic) or a real new
  feature. If it's the latter, defer it — note why in the hand-off — rather
  than half-building it under time pressure.

**Remove from the CLI entirely** (true post-v1 roadmap, zero backing
implementation, nothing in this milestone plans to build the server side):
`wren mcp` (all 3 subcommands), `wren fleet`, `wren usage`, `wren run attach`,
`wren run steer`, `wren project config`. These are reversible — trivial to
re-add once actually built. Move them into a "Roadmap" list in
`README.md`/`SETUP.md` (partially already there) instead of shipping as
commands that exist only to error. Delete the `placeholder()`/`notImplemented()`
helper machinery in `internal/cli/root.go`/`version.go` once nothing references
it — that's the "dead code" the owner asked to be rid of.

**Also fix while touching `run create`'s flags:** `--runtime runc|gvisor|kata`
is fully wired end-to-end in the operator (`internal/controller/pod.go:
runtimeClassName`) but gVisor/Kata are architecturally deferred to M4 and no
v1 cluster provisions those `RuntimeClass` objects — passing `--runtime gvisor`
today produces a confusing pod-admission failure, not a clear message. Either
validate client-side and reject non-`runc` values with a message pointing at
M4, or make the `--help` text explicit that only `runc` works today. Don't
remove the plumbing — it's correctly built ahead of M4, just mislabeled as
available now.

## Scope guards

**OUT:** any change to `RunSpec`/pod shape beyond what Part A/C require;
automatic port-forwarding or any change to how engineers reach the apiserver
(`--expose=LoadBalancer` + manual port-forward stays the model — a single
documented background command is not an obviousness problem worth the added
complexity of managing a child process); GitHub App (WS-2); Helm (WS-5);
interactive steering itself (`run attach`/`steer` — only remove the fake
commands, don't build the real feature); renaming.
**Hot files:** `internal/install/steps.go`, `internal/install/install.go`,
`internal/install/kube.go`, `internal/coreapi/service.go`,
`internal/apiserver/server.go`, `internal/cli/root.go`, `internal/cli/run.go`,
`internal/cli/project.go`, `internal/cli/misc.go`, `internal/cli/version.go`,
`internal/controller/agentrun_controller.go` (only if `run stop` needs a
reconciler-side signal), `SETUP.md`, `README.md`.

## Definition of done

- [ ] `wren project create <name> --repo owner/repo` (no other flags) lands runs
      in a namespace that actually has the install's credentials, by default.
- [ ] `run create` against a harness/namespace combo missing its required
      Secret fails immediately with a clear, actionable error — not a confusing
      downstream failure.
- [ ] `wren --help` / `wren project --help` / `wren run --help` contain zero
      commands that error with "not implemented yet" — every listed command
      either works or isn't listed.
- [ ] `project get`, `run rm`, `run stop` are real; `run resume` is real if
      cheap, otherwise explicitly deferred with reasoning in the hand-off.
- [ ] `--runtime gvisor|kata` gives a clear "not available until M4" signal
      instead of a confusing pod-admission failure.
- [ ] `SETUP.md`/`README.md` onboarding examples match exactly what
      `wren install`'s hand-off text prints (the hand-off is the source of
      truth — verify by actually running `wren install --kind` and diffing its
      output against the docs).
- [ ] `make test vet` + lint green; `make e2e` green.
- [ ] `wren install --kind` → `project create` (minimal flags) → `run create`
      → `Succeeded` run end to end on a real kind cluster, pasted in the
      hand-off, proving Part A's fix actually closes the loop (not just unit
      tested).
