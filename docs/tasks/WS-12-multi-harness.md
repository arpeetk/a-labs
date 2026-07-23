# WS-12: Codex + OpenCode harnesses

**Branch:** `ws12-multi-harness` · **Worktree:** `../wren-ws12` · **Size:** L · **State:** READY
**Blocked on:** nothing. **Main track.** Review: strong-model/orchestrator pass
before merge (touches the credentialed egress routes).
*Context: the RunSpec/event contract already makes harnesses pluggable; this
proves it by shipping two more. Rename to skein is deferred — keep building as
`wren`; WS-10 sweeps names later.*

## Design (settled)

1. **Adapters** (`internal/harness/`): add `codex.go` and `opencode.go` beside
   `claudecode.go`/`mock.go`, registered in the existing `switch kind`
   (`harness.go:36`). Same contract: translate `RunSpec` → agent invocation
   (headless/non-interactive mode — `codex exec` / `opencode run`), emit the
   existing event stream (`pr_ready`, `token_usage`, status lines), honor the
   exit-code semantics (`ExitRetryable` passthrough). `RunSpec.Model` maps to
   each CLI's model flag. Reuse the claudecode adapter's streaming/parse
   patterns; factor shared helpers rather than copy-pasting three times.
2. **Images** (`build/Dockerfile.codex`, `build/Dockerfile.opencode`): same
   shape as `Dockerfile.claude-code` — the agent CLI installed, `wren-runtime`
   as entrypoint dispatcher, uid **65532** aligned with the pod's pinned
   runner uid (the WS-1 boundary depends on it — assert in a comment).
3. **Egress/OpenAI route** (`internal/egress`, `internal/controller/pod.go`,
   `cmd/wren-operator/main.go`): Codex needs `api.openai.com` — add a
   credentialed reverse route `/openai/` → `https://api.openai.com`
   (Bearer injection via the existing `HeaderAuth`), an operator flag
   `--openai-key-secret` mirroring `--anthropic-key-secret`, and the runner
   env mapping (`OPENAI_BASE_URL` → the proxy route, like
   `ANTHROPIC_BASE_URL`). The Director already scrubs inbound creds on every
   route (WS-1 round 2) — add route-level tests proving it for the new one.
   OpenCode rides the existing Anthropic route — no new surface; its adapter
   writes an opencode config pointing at the injected `ANTHROPIC_BASE_URL`.
4. **Docs:** new `docs/harnesses.md` (per harness: image, required
   secret/keys, model flag mapping, known limitations) + README status-table
   row + spec status-block line. `wren run create --harness codex` must be
   discoverable from `--help` (harness choices listed).

## Scope guards

**OUT:** session/resume support for the new adapters; streaming-fidelity
beyond the existing event schema (frozen); MCP config; live runs against real
providers (no keys in CI — unit + keyless only, record as NOT verified);
changing RunSpec or the pod shape.
**Hot files:** `internal/harness/*`, `internal/egress/proxy.go` + auth,
`internal/controller/pod.go`, `cmd/wren-operator/main.go`,
`internal/podruntime/*` (dispatch), `build/Dockerfile.*`, `docs/harnesses.md`,
`internal/cli/run.go` (help text), README/spec status blocks.

## Definition of done

- [ ] Unit: adapter command-construction + event-parse matrices for both
      harnesses (table-driven, mirroring `harness_test.go` patterns); proxy
      `/openai/` route injects + scrubs; pod env wiring per harness kind.
- [ ] `make test vet` + `golangci-lint` green; coverage held or better on
      touched packages.
- [ ] `make e2e` green (mock harness path unchanged).
- [ ] `docker build` succeeds for both new images (note sizes in hand-off).
- [ ] Hand-off lists exactly what a live-key validation must run later
      (per harness: secret name, env, smoke task).
