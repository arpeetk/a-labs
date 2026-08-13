# Harnesses

Wren treats the agent as a pluggable **harness adapter** behind one contract.
The pod hands the adapter a `RunSpec`, the adapter drives a
headless coding CLI in the cloned workspace, and events flow back as the
newline-delimited JSON stream (`status` / `message` / `tool_call` /
`token_usage` / `pr_ready`). Exit codes decide retryability: a deterministic
failure exits 1 (no retry — retrying would just re-spend tokens); the adapter
itself never retries.

Harnesses may leave ordinary working-tree changes or create their own commits.
Finalize handles both forms: it creates or reuses the Wren run branch and opens
a PR when the resulting history is ahead of the requested hydrate-time base.
Rewritten or unrelated history is rejected instead of pushed; a clean HEAD
exactly at the requested base remains a genuine no-change run.

Credentials are never in the harness image or the runner env: the in-pod
egress proxy injects them on credentialed reverse routes. Each adapter passes
only a **placeholder** API key so its CLI starts in API-key
mode; the proxy scrubs inbound credentials and overwrites the auth header on
the way out.

Pick a harness per project (`wren project create --harness ... --harness-image
...`, or the registration API's `defaultHarness` + `harnessImage` fields —
`POST /v1/projects`) or per run: `wren run create --harness
mock|claude-code|codex|opencode|byo`.

`wren install` builds and delivers every harness image by default (in
addition to the runtime/operator/apiserver control-plane images) — a fresh
`wren install --kind` / `wren install --registry` gives you `claude-code`,
`codex`, and `opencode` images ready to use, no separate manual build step
required. Use `--harness-images=claude-code,codex` to restrict the set (faster
iterative installs) or `--harness-images=none` to skip harness images
entirely (a keyless/mock-only eval install).

The installer reads `GITHUB_TOKEN`, `ANTHROPIC_API_KEY`, and `OPENAI_API_KEY`
(or prompts without echo on an interactive terminal) and writes only the
corresponding proxy Secrets. `--skip-credentials` disables this step.

| Harness | Image (`build/`) | Model API route (proxy) | Secret → proxy env | `RunSpec.Model` → CLI flag |
|---|---|---|---|---|
| `mock` | `Dockerfile.runtime` (no CLI) | none — deterministic, keyless | none | ignored |
| `claude-code` | `Dockerfile.claude-code` | `/anthropic/` → `api.anthropic.com` (`x-api-key`) | `wren-anthropic-key` (key `key`) → `ANTHROPIC_API_KEY` | `--model <model>` |
| `codex` | `Dockerfile.codex` | `/openai/v1` → `api.openai.com/v1` (`Authorization: Bearer`) | `wren-openai-key` (key `key`) → `OPENAI_API_KEY` | `--model <model>` |
| `opencode` | `Dockerfile.opencode` | rides `/anthropic/` (no new surface) | `wren-anthropic-key` (key `key`) → `ANTHROPIC_API_KEY` | `--model <provider/model>`; a bare name defaults to `anthropic/` |
| `byo` | your own image speaking the harness event contract | your proxy config | your choice | your choice |

## codex

- **Invocation:** `codex exec --json --sandbox danger-full-access
  --skip-git-repo-check [proxy provider config] [--model M] <prompt>` (the
  CLI's non-interactive mode). In Wren, the ephemeral `wren` provider uses
  `<OPENAI_BASE_URL>/v1`, `wire_api="responses"`, and
  `supports_websockets=false`, keeping streaming on HTTP/SSE through the
  reverse proxy. Without `OPENAI_BASE_URL`, no provider overrides are passed,
  preserving normal direct/local Codex behavior.
  `danger-full-access` disables Codex's own sandbox/approvals for the same
  reason claude-code uses `--dangerously-skip-permissions`: the pod IS the
  sandbox, and Codex's landlock sandbox would otherwise also deny the agent's
  spawned commands their (proxied) network path.
- **Env:** the operator wires `OPENAI_BASE_URL` →
  `http://127.0.0.1:8099/openai`; in proxy mode the adapter ensures
  `CODEX_API_KEY` / `OPENAI_API_KEY` placeholders. The proxy injects the real
  key from the `wren-openai-key` Secret (operator flag
  `--openai-key-secret`). No placeholders are synthesized in direct mode.
- **Crash resume:** each run gets a private `CODEX_HOME` on the durable
  workspace (`/workspace/.git/wren/codex` for a repository run, so execution
  state can never be staged into its PR). A replacement pod invokes
  `codex exec resume --last`; `--last` is safe because that directory contains
  only this run's sessions. The session directory is included in workspace
  checkpoints, so checkpoint restore preserves the same capability. Runs
  created before this state existed degrade explicitly to workspace-only
  recovery instead of entering a deterministic retry loop.
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

## Image builds

All Go build stages consume BuildKit's automatic target platform arguments;
they do not default to amd64. A native build and `--platform linux/arm64`
therefore contain an arm64 `wren-runtime`, while the GKE path continues to use
explicit `--platform linux/amd64`. The Codex image pins the CLI version for
repeatable builds; override it with `--build-arg CODEX_VERSION=<version>`.

## Validation and recovery status

Codex's Responses HTTP/SSE proxy configuration has been proven by a live Wren
canary. OpenCode's command construction, event parsing, and credential wiring
have hermetic coverage but no live-provider validation. Codex has native
session resume; Claude Code and OpenCode restart a fresh model session in the
surviving or restored workspace. The remaining gaps are tracked in the
[roadmap](roadmap.md).

Project and run APIs reject unknown harness values. A test-only
`WREN_HARNESS` override with an unknown value degrades to mock and emits an
explicit note.
