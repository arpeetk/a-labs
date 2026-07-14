# WS-3: Postgres store

**Branch:** `ws3-postgres-store` · **Worktree:** `../wren-ws3` · **Size:** M · **State:** READY (after WS-0 merges)

## Context (read first)

- `AGENTS.md` in full.
- `docs/implementation-plan.md` §WS-3 — the design.
- `internal/store/{store.go,memory.go,memory_test.go}` — the interface is
  frozen; you implement it, you do not change it.

## Scope

**IN:**

1. `internal/store/postgres.go` — Postgres implementation of `store.Store`
   on **pgx/v5** (add the dependency). Two tables (`projects`, `runs`);
   `egress_allowlist` as `text[]`; timestamps; `ErrNotFound`/`ErrExists`
   semantics identical to the memory impl.
2. Migrations: SQL files under `internal/store/migrations/`, `//go:embed`ed,
   applied by a small in-code migrator on `NewPostgres` (schema_version
   table; forward-only). No migration framework dependency.
3. **Conformance suite:** refactor the existing memory store tests into a
   shared `testStore(t, func() store.Store)` conformance function run against
   both implementations. Postgres runs via testcontainers-go **or** a
   `STORE_TEST_DSN` env (skip with a clear message when neither Docker nor a
   DSN is available) — pick one, justify in the hand-off.
4. Wiring in `cmd/wren-apiserver/main.go`: `--store=memory|postgres`
   (default `memory`) + `DATABASE_URL` env for the DSN. Keep the change to
   this file minimal and append-shaped (WS-4 also adds a flag here; merge
   order handles the trivial conflict).
5. **Reconcile-on-boot:** on apiserver start, list existing AgentRun CRs (via
   the launcher's existing read path; extend `Launcher` with a `ListRuns`-like
   method only if none exists) and upsert their store rows so a restarted
   apiserver re-learns in-flight runs. Works for both store types.
6. Update spec living-status + README status table (Store row).

**OUT:** SQLite; Helm/DB provisioning (WS-5); changing the `Store` interface
or the `Run`/`Project` structs (if you believe a field is missing, stop and
ask in the hand-off); any CLI change.

## Hot files

You own: `internal/store/*`, `go.mod`/`go.sum`.
Shared (append-only, expect trivial rebase): `cmd/wren-apiserver/main.go`.
Do NOT touch: `internal/controller/*`, `internal/egress/*`,
`api/v1alpha1/*`, `internal/apiserver/*` handlers.

## Definition of done

- [ ] `make test vet` green; conformance suite passes against memory always,
      and against real Postgres locally (state how you ran it).
- [ ] Manual durability check on kind: start apiserver with
      `--store=postgres` (postgres via `docker run`), create a run, kill and
      restart the apiserver, `wren run get` still returns it with the right
      phase. Describe the exact commands in the hand-off.
- [ ] `make e2e` green (defaults unchanged — memory store).
- [ ] Hand-off note.
