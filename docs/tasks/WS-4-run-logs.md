# WS-4: `wren run logs`

**Branch:** `ws4-run-logs` · **Worktree:** `../wren-ws4` · **Size:** M · **State:** READY (after WS-0 merges)

## Context (read first)

- `AGENTS.md` in full.
- `docs/implementation-plan.md` §WS-4 — the design.
- `internal/launcher/launcher.go` (the cluster bridge you're extending),
  `internal/apiserver/server.go` (handler patterns), `internal/cli/run.go` +
  `internal/client/client.go` (CLI/client patterns).

## Scope

**IN:**

1. `launcher.Launcher` gains
   `StreamLogs(ctx, namespace, runID, container string, follow bool) (io.ReadCloser, error)`:
   - Resolve the run's **current** pod via the `wren.dev/run` label selector
     (pod names embed the restart count — never reconstruct them).
   - Default container `harness`; validate against the known container names.
   - Requires a `kubernetes.Interface` clientset for the `pods/log`
     subresource (the controller-runtime client doesn't serve it) — add it to
     the K8s launcher construction; extend the Fake accordingly.
2. RBAC: add `pods/log` get to `config/apiserver/role.yaml` (and the operator
   role only if genuinely needed — it should not be).
3. apiserver: `GET /v1/runs/{id}/logs?follow=&container=` — resolve the run
   from the store (namespace), stream plaintext with per-line flush
   (`http.Flusher`), sensible status codes: 404 unknown run, 409 with a JSON
   hint when no pod exists yet ("run is Pending") or anymore.
4. client + CLI: `wren run logs <id> [-f] [--container <name>]`, streaming to
   stdout; non-zero exit on 4xx.
5. Tests: launcher fake-based tests; apiserver httptest with a stubbed
   launcher; CLI test per existing patterns.
6. Spec living-status + README status update (`run logs` lands).

**OUT:** SSE/WebSocket framing; historical/aggregated logs (GCS); multi-pod
`--previous` restarts (note as candidate follow-up); `run attach`/steering.

## Hot files

You own: `internal/launcher/*`, `internal/apiserver/*`, `internal/client/*`,
`internal/cli/*`, `config/apiserver/role.yaml`.
Shared (append-only): `cmd/wren-apiserver/main.go` (only if wiring requires),
`config/rbac/role.yaml`.
Do NOT touch: `internal/controller/*`, `internal/egress/*`,
`internal/store/*` internals (read via the existing interface only),
`api/v1alpha1/*`.

## Definition of done

- [ ] `make test vet` green.
- [ ] Live check during `make e2e` (E2E_KEEP=1): `wren run logs -f <id>`
      tails the mock harness event stream while the run executes; paste a
      snippet in the hand-off.
- [ ] Pending-run and finished-run edge cases return the designed responses.
- [ ] Hand-off note.
