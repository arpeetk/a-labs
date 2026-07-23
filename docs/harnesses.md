# Harnesses

Wren treats the agent as a pluggable **harness adapter** behind one contract
(spec §5.4): the pod hands the adapter a `RunSpec`, the adapter drives a
headless coding CLI in the cloned workspace, and events flow back as the
newline-delimited JSON stream (`status` / `message` / `tool_call` /
`token_usage` / `pr_ready`). Exit codes decide retryability: a deterministic
failure exits 1 (no retry — retrying would just re-spend tokens); the adapter
itself never retries.

Credentials are never in the harness image or the runner env: the in-pod
egress-proxy injects them on credentialed reverse routes (spec §5.6). Each
adapter passes only a **placeholder** API key so its CLI starts in API-key
mode; the proxy scrubs inbound credentials and overwrites the auth header on
the way out.

Pick a harness per project (the registration API's `defaultHarness` +
`harnessImage` fields — `POST /v1/projects`; a `wren project` CLI is WS-13) or
per run: `wren run create --harness mock|claude-code|codex|opencode|byo`.

| Harness | Image (`build/`) | Model API route (proxy) | Secret → proxy env | `RunSpec.Model` → CLI flag |
|---|---|---|---|---|
| `mock` | `Dockerfile.runtime` (no CLI) | none — deterministic, keyless | none | ignored |
| `claude-code` | `Dockerfile.claude-code` | `/anthropic/` → `api.anthropic.com` (`x-api-key`) | `wren-anthropic-key` (key `key`) → `ANTHROPIC_API_KEY` | `--model <model>` |
| `codex` | `Dockerfile.codex` | `/openai/` → `api.openai.com` (`Authorization: Bearer`) | `wren-openai-key` (key `key`) → `OPENAI_API_KEY` | `--model <model>` |
| `opencode` | `Dockerfile.opencode` | rides `/anthropic/` (no new surface) | `wren-anthropic-key` (key `key`) → `ANTHROPIC_API_KEY` | `--model <provider/model>`; a bare name defaults to `anthropic/` |
| `byo` | your own image speaking the §5.4 contract | your proxy config | your choice | your choice |

## codex

- **Invocation:** `codex exec --json --sandbox danger-full-access
  --skip-git-repo-check [--model M] <prompt>` (the CLI's non-interactive mode).
  `danger-full-access` disables Codex's own sandbox/approvals for the same
  reason claude-code uses `--dangerously-skip-permissions`: the pod IS the
  sandbox, and Codex's landlock sandbox would otherwise also deny the agent's
  spawned commands their (proxied) network path.
- **Env:** the operator wires `OPENAI_BASE_URL` →
  `http://127.0.0.1:8099/openai`; the adapter ensures `CODEX_API_KEY` /
  `OPENAI_API_KEY` placeholders (the non-interactive docs name `CODEX_API_KEY`
  the `codex exec` automation key). The proxy injects the real key from the
  `wren-openai-key` Secret (operator flag `--openai-key-secret`).
- **Events:** parses the `codex exec --json` JSONL stream — `item.completed`
  (`agent_message` → message; `command_execution` / `mcp_tool_call` /
  `file_change` / `web_search` → tool_call), `turn.completed.usage` →
  token_usage, `turn.failed` / `error` → deterministic failure.

## opencode

- **Invocation:** `opencode run --format json --auto [--model P/M] <prompt>`.
  `--auto` auto-approves permission prompts (pod-is-the-sandbox rationale, as
  above).
- **Env/config:** the adapter renders a per-run `opencode.json` into a temp dir
  (never the workspace — a config file there would end up committed in the PR)
  pointing the `anthropic` provider's `baseURL` at the injected
  `ANTHROPIC_BASE_URL` (`http://127.0.0.1:8099/anthropic`, with `/v1`
  appended — opencode expects the full API base), plus an
  `ANTHROPIC_API_KEY` placeholder. The proxy injects the real `x-api-key` from
  the `wren-anthropic-key` Secret. `OPENCODE_DISABLE_AUTOUPDATE` /
  `OPENCODE_DISABLE_MODELS_FETCH` keep opencode's own update/catalog traffic
  off the egress path the lockdown would block.
- **Events:** parses `--format json` events — `text` → message, `tool_use` →
  tool_call, `step_finish.tokens` → token_usage, `error` → deterministic
  failure.

## Known limitations (both new adapters)

- **Not yet validated against the live providers** — no keys in CI. Command
  construction, event parsing, and the credential wiring are unit-tested; a
  live-key smoke run per harness is the remaining validation (see the WS-12
  hand-off for the exact recipe).
- **No session/resume:** `RunSpec.mode=resume` restarts the agent fresh in the
  surviving workspace (same as claude-code today; transcript-restore is
  post-launch with the checkpointer, spec §5.5).
- **MCP config is not wired** into either CLI.
- An unknown harness kind falls back to `mock` with a note in the event stream
  (M0 behavior).
