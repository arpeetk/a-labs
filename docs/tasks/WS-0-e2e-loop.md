# WS-0: Keyless e2e loop (`make e2e`)

**Branch:** `ws0-e2e-loop` · **Worktree:** `../wren-ws0` · **Size:** M · **State:** READY
**Dispatch alone — this is the merge gate every other workstream uses.**

## Context (read first)

- `AGENTS.md` in full — toolchain (Go 1.26 PATH gotcha, zsh word-split gotcha),
  build/test conventions, and §7 "local end-to-end on kind" (this brief
  automates that recipe).
- `docs/implementation-plan.md` §WS-0 — the design; do not re-litigate.
- `SETUP.md` and `hack/setup.sh` — the existing manual path you are scripting.

## Scope

**IN:**

1. `hack/e2e.sh` — a non-interactive script that, end to end:
   - Creates kind cluster `${KIND_CLUSTER:-wren-e2e}` if absent (reuse if present).
   - Builds + loads images: `make docker-operator docker-apiserver` and the
     runtime image, then `make kind-load` (reuse existing targets; add missing
     ones only if a gap exists).
   - Deploys the in-cluster control plane: `kubectl apply -k config/default`
     (operator + apiserver in `wren-system`), waits for both Deployments Ready.
   - Port-forwards `svc/wren-apiserver` (background, cleaned up on exit).
   - Creates a project with `defaultHarness: mock` and **no repo** (finalize
     is skipped without a repo — that is the keyless design). Gotcha: the
     project JSON field is `defaultModel`, not `model`; the apiserver rejects
     unknown fields.
   - Creates a run via `bin/wren` (or curl with the `X-Wren-User` header),
     polls `run get` until `Succeeded` (timeout 5 min).
   - On failure: dump operator logs, apiserver logs, the AgentRun YAML, and
     the agent pod's containers' logs, then exit non-zero.
   - Teardown (delete cluster) unless `E2E_KEEP=1`. Trap-based cleanup so a
     failed run doesn't leak the port-forward.
2. `Makefile`: `e2e` target invoking the script; keep existing targets untouched
   except additions.
3. **Env-overridable proxy upstreams** (enabler for the later gitea-backed
   `e2e-pr` tier): where the egress-proxy role builds its reverse-proxy routes
   (in `internal/podruntime`), allow `WREN_GITHUB_UPSTREAM`,
   `WREN_GITHUB_API_UPSTREAM`, `WREN_ANTHROPIC_UPSTREAM` to override the
   hardcoded upstream URLs. Default behavior unchanged. Unit-test the override
   plumbing.
4. Docs: a short "Testing" subsection in `AGENTS.md` pointing at `make e2e`.

**OUT (do not build):** the gitea `e2e-pr` tier itself; any GitHub Actions
workflow (WS-7 owns CI); any change to controller/apiserver logic; checkpoint
or egress-enforcement work.

## Hot files

You own: `hack/e2e.sh` (new), `Makefile`, `internal/podruntime/*`.
Do NOT touch: `internal/controller/pod.go`, `api/v1alpha1/*`,
`cmd/wren-apiserver/main.go`, `internal/store/*`.

## Definition of done

- [ ] `make test vet` green.
- [ ] `make e2e` green twice in a row from a fresh clone state (idempotency),
      with zero credentials set.
- [ ] `make e2e` finishes in <10 min on this machine.
- [ ] Failure path demonstrated once (e.g. bad image tag) — verify the log
      dump fires and exit code is non-zero.
- [ ] Hand-off note (template in `README.md`).
