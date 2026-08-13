# Wren technical specification

> Current implementation contract · updated 2026-08-13

Wren is a self-hosted software-factory control plane. It turns an engineering
task into a durable Kubernetes run that executes a coding harness in a hardened
pod and, for repository-backed work, opens a pull request.

This document describes the system that exists. Intentional gaps live in
[`roadmap.md`](roadmap.md); deployment instructions live in
[`../SETUP.md`](../SETUP.md); security residual risks live in
[`../SECURITY.md`](../SECURITY.md).

## 1. Scope and invariants

Wren currently provides:

- CLI and native desktop clients;
- an HTTP/JSON control plane with memory or Postgres persistence;
- a Kubernetes `AgentRun` operator;
- mock, Claude Code, Codex, OpenCode, and bring-your-own harness selection;
- isolated per-run pods and workspaces with bounded automatic retry;
- verified checkpoint/restore and durable pause/resume when checkpoint storage
  is configured;
- a credential-injecting egress proxy with default iptables confinement;
- idempotent git finalization and GitHub pull-request creation;
- a durable run-event journal and terminal status projection;
- product installation onto kind or GKE Standard.

The load-bearing invariants are:

1. The harness is untrusted and receives no GitHub, model-provider, cloud, or
   Kubernetes credential.
2. Every run has a monotonic attempt identity. Manual resume never reuses a pod
   name or changes a resumed attempt back into a fresh start.
3. Retryable infrastructure failure and deterministic task failure are
   distinct; only the former consumes the bounded automatic retry budget.
4. A surviving PVC is reattached. A destroyed PVC is restored only from a
   verified checkpoint; otherwise the run fails `WorkspaceLost` instead of
   silently continuing on an empty disk.
5. Run creation and its Kubernetes launch intent are atomic in a durable store.
   Replayed launch work and event delivery are idempotent.
6. The agent produces a branch and pull request. Wren does not merge or deploy.

## 2. Architecture

```text
CLI / desktop
    │ HTTP/JSON
    ▼
wren-apiserver
    ├── coreapi ── store (memory or Postgres)
    ├── launch outbox worker ── launcher ── Kubernetes API
    └── run-event ingest/query
                          │
                          ▼
                    AgentRun CR
                          │
                          ▼
                    wren-operator
                          │
               PVC + ConfigMap + hardened pod
                          │
       ┌──────────────────┼───────────────────┐
       ▼                  ▼                   ▼
    hydrate            harness            trusted sidecars
 restore/clone      coding agent       proxy/checkpoint/gateway
                          │                   │
                          └── finalize ───────┘
                              branch + PR
```

### Component ownership

| Component | Responsibility |
|---|---|
| `internal/apiserver` | HTTP decoding, auth-header extraction, status/error mapping, streaming |
| `internal/coreapi` | validation, default resolution, run/project rules, cluster reconciliation |
| `internal/store` | projects, runs, launch outbox, leases, immutable events |
| `internal/launcher` | `AgentRun` publication, lifecycle annotations, pod logs |
| `internal/controller` | run state machine, PVC/RunSpec/pod construction, status projection |
| `internal/runspec` | operator-to-harness JSON contract |
| `internal/harness` | adapter selection, process/API invocation, normalized events |
| `internal/podruntime` | hydrate, harness, egress, gateway, checkpoint, lockdown roles |
| `internal/blob` | checkpoint store contract, archive integrity, secure restore |
| `internal/finalize` | commit/branch/push/PR idempotency and retry classification |
| `internal/install` | cluster preflight, images, manifests, credentials, uninstall |

Dependencies point inward through small interfaces (`store.Store`,
`launcher.Launcher`, `github.Client`, controller log/exec seams). Transport and
Kubernetes details do not leak into business rules.

## 3. Domain model

### Project

A project is a registered repository plus run defaults:

- name and optional GitHub `owner/repo`;
- default harness, image, and model;
- runtime class and CPU/memory/disk requests;
- run namespace and egress allowlist;
- optional checkpoint bucket.

Projects without a repository are valid and support keyless mock runs. The
installer configures the control plane's default run namespace to match the
namespace holding proxy credentials.

### Run

The store's `Run` is the user-facing projection: submission metadata, resolved
harness/runtime/namespace, phase, restart count, PR URL, conditions, and latest
checkpoint. The Kubernetes `AgentRun` is authoritative for execution status;
the control plane merges CR state into the store without blanking information
that is already known.

### AgentRun

`api/v1alpha1.AgentRun` contains the desired harness/task/sandbox/workspace/
egress/retry configuration. Status contains:

- phase and current pod name;
- monotonic `attemptGeneration` and resettable automatic `restartCount`;
- last verified checkpoint;
- harness session id, PR, and token usage;
- Kubernetes-style conditions for readiness, recovery, checkpoint storage, and
  egress posture.

Lifecycle requests are one-shot annotations:

- `wren.dev/cancel` stops the pod and makes the run terminal `Canceled`;
- `wren.dev/pause` enters the durable pause state machine;
- `wren.dev/resume` resumes `Paused` or terminal `Failed` work and is cleared by
  the operator after acceptance.

### Run events and launch operations

Run events are immutable and ordered by store id. `(run_id, source, source_id)`
is unique, so a restarted gateway or reconciliation loop may replay safely.

Run creation also writes a `launch_run` operation in the same transaction. A
worker claims operations with a finite lease, publishes the `AgentRun`
idempotently, and completes/retries/fails only while it owns that lease. This
prevents an API crash between accepting a run and publishing it from losing the
request.

## 4. Run lifecycle

```text
new → Pending → Provisioning → Running → Finalizing → Succeeded
                                  │
                                  ├─ deterministic error ─────────→ Failed
                                  ├─ user stop ───────────────────→ Canceled
                                  ├─ user pause → Pausing → Paused
                                  │                         │
                                  │                         └─ resume
                                  └─ retryable interruption → Interrupted
                                                               │
                                                               └─ resume pod
```

`Succeeded`, `Failed`, and `Canceled` are terminal. An explicit resume request
may reopen only `Failed`; a stray annotation cannot resurrect success or
cancellation. `Paused` is quiescent and has no agent pod.

### Automatic recovery

The operator records the exact current pod before interpreting its phase. A
retryable exit, eviction, node loss, or disappearance of that recorded pod:

1. advances `attemptGeneration`;
2. increments `restartCount` once for the incident;
3. marks the run `Interrupted` with a `Resuming` condition;
4. creates a new pod whose RunSpec mode is `resume`.

The default automatic budget is five restarts. Exit code 75 explicitly requests
a retry; ordinary non-zero harness exits are deterministic. OOM and
infrastructure pod reasons are retryable. Exhaustion produces
`RetryBudgetExhausted`.

The workspace PVC has a stable run identity. Ordinary pod replacement reuses
it. If Kubernetes reports pod and PVC loss as separate events, recovery
conditions prevent double-charging the same incident.

### Pause and manual resume

Pause requires mounted checkpoint storage. The operator:

1. marks `Pausing`;
2. execs `quiesce` in the harness container;
3. execs `checkpoint-once` in the trusted checkpointer;
4. validates the returned id, manifest key, checksum, size, and format;
5. commits `status.lastCheckpoint` and a verified condition;
6. removes the pod and marks `Paused`.

Any pre-publication failure attempts to unquiesce the harness and records an
actionable condition without deleting the live pod. Resume from `Paused`
preserves the automatic retry budget and pins the exact pause checkpoint if the
PVC must be recreated. Resume from `Failed` resets that budget and starts a new
monotonic attempt.

## 5. Control-plane API

The API is HTTP/JSON. External routes identify the caller with the trusted
`X-Wren-User` header; this is not secure public authentication.

| Method and path | Contract |
|---|---|
| `GET /healthz` | process liveness |
| `GET /readyz` | readiness, including store health where supported |
| `POST /v1/projects` | register a project |
| `GET /v1/projects[/{name}]` | list/get projects |
| `POST /v1/runs` | validate, persist, and enqueue a run |
| `GET /v1/runs[/{id}]` | list/get CR-refreshed run projections |
| `DELETE /v1/runs/{id}` | delete store record and `AgentRun` |
| `POST /v1/runs/{id}/stop` | request terminal cancellation |
| `POST /v1/runs/{id}/pause` | request durable pause |
| `POST /v1/runs/{id}/resume` | resume paused/failed work |
| `GET /v1/runs/{id}/logs` | stream current pod container logs |
| `GET /v1/runs/{id}/events` | page the durable event journal |
| `POST /v1/internal/runs/{id}/events` | authenticated gateway ingest |

The internal route accepts identity only from the trusted proxy. The proxy
injects the gateway token and fixes `X-Wren-Run-ID`; the untrusted harness and
gateway containers hold neither that token nor a service-account token.

## 6. Agent pod and harness contract

The operator creates one pod per attempt with a durable workspace PVC and a
read-only RunSpec ConfigMap. Pod roles are implemented by the same
`wren-runtime` binary:

| Role | Trust | Purpose |
|---|---|---|
| `egress-lockdown` init | trusted/privileged | install uid-scoped iptables rules |
| `hydrate` init | trusted | restore checkpoint if required, then clone/validate repo |
| harness | untrusted | execute the selected coding agent and finalize |
| egress proxy | trusted | allowlist destinations and inject credentials |
| checkpointer | trusted | publish periodic/forced workspace snapshots |
| agent gateway | trusted transport, no credential | forward durable JSONL events through proxy |

The RunSpec supplies run/project/user, harness/model/prompt/base ref, workspace,
mode, optional session/checkpoint/MCP data, and branch prefix. Harnesses emit
newline-delimited normalized events (`status`, `message`, `tool_call`,
`token_usage`, `pr_ready`, `error`) and use the exit-code contract:

- `0`: completed;
- `1`: deterministic failure;
- `75`: transient failure eligible for operator retry.

Codex persists a private per-run `CODEX_HOME` inside the workspace and uses
`codex exec resume --last` on a resumed attempt. Claude Code and OpenCode
currently recover workspace state but start a new model session. Detailed
adapter behavior is in [`harnesses.md`](harnesses.md).

## 7. Checkpoint storage

`internal/blob.Store` is scoped to a run prefix. Production-shaped storage is a
GCS bucket mounted into trusted containers through the GCS FUSE CSI driver;
kind tests may use an explicit single-node local path.

A checkpoint consists of an immutable gzip/tar archive plus a versioned JSON
manifest written last. Publication computes and reads back SHA-256 before the
manifest becomes visible. Restore considers manifests newest-first, rejects
invalid or corrupt candidates, and uses an exact manifest when pause/resume
requires it. Retention removes older manifest/archive pairs after publication.

Archives include dotfiles and safe relative symlinks. Absolute/escaping links
are omitted during creation, and extraction uses `os.Root` plus local-path and
symlink validation to prevent traversal or write-through attacks.

Without a configured checkpoint mount, the checkpointer is liveness-only and
durability is limited to the PVC. This degraded posture is recorded in a
`CheckpointStorage` condition.

## 8. Egress and credentials

The harness is uid 65532 and the proxy uid 65533. In default enforcement mode,
iptables permits the harness to contact only the local proxy and rejects direct
IPv4/IPv6 egress including DNS. The proxy resolves allowlisted CONNECT hosts
once and dials the resolved address, avoiding an allowlist/dial DNS-rebinding
window. A startup canary proves direct dial and direct HTTPS are blocked before
the model can spend tokens.

Credentialed reverse routes are:

| Prefix | Upstream | Injected secret |
|---|---|---|
| `/github/` | `github.com` git HTTPS | GitHub token as basic auth |
| `/github-api/` | `api.github.com` | GitHub bearer token |
| `/anthropic/` | `api.anthropic.com` | Anthropic `x-api-key` |
| `/openai/` | `api.openai.com` | OpenAI bearer token |
| internal control-plane route | apiserver | gateway token + run identity |

The installer writes these Secrets in the configured run namespace. GitHub is
currently a PAT; short-lived per-run GitHub App tokens remain a roadmap item.
See [`../SECURITY.md`](../SECURITY.md) for the consequences of enforcement-off,
shared credentials, `runc`, and header authentication.

## 9. Finalization

Hydrate records the requested remote base. Finalize:

1. accepts working-tree changes or harness-created commits descended from that
   base;
2. rejects unrelated or replacement history;
3. creates or reuses `wren/<sanitized-user>/<run-id>`;
4. commits outstanding changes;
5. pushes idempotently through the proxy;
6. opens or reuses a pull request with the project rubric body.

No changes produces `ErrNoChanges` and a successful run without a PR.
Transport errors, EOF/timeouts, HTTP 429, and 5xx are retryable. Authentication,
authorization, validation, repository-not-found, and non-fast-forward errors
are deterministic. The classification is table-tested because it controls both
token spend and recovery.

## 10. Persistence and HA

Memory and Postgres implement the same store contract. Memory is suitable for
tests/evaluation and supports the durable semantics in-process, but it loses
projects and history on restart. The API reconciles existing AgentRuns on boot
to recover cluster execution state.

Postgres uses embedded forward-only migrations protected by an advisory lock.
Run creation atomically inserts the run, submission event, and launch
operation. Workers use `FOR UPDATE SKIP LOCKED` plus expiring ownership leases;
completion and retry are conditional on the lease owner. Event source ids make
gateway and CR replay idempotent. `/readyz` pings the database.

`config/production-gcp` provides two API replicas, disruption protection, and
Cloud SQL Auth Proxy wiring. Provisioning/backups/IAM for the database remain
operator responsibilities.

## 11. Installation and verification

`wren install` is the product path. It validates local tools and cluster
version, optionally creates kind or GKE Standard, applies embedded CRD/RBAC/
deployment manifests, builds and delivers control-plane plus selected harness
images, writes GitHub/Anthropic/OpenAI proxy Secrets, waits for readiness, and
prints the next commands. Re-running is idempotent. `wren uninstall --confirm`
removes namespaces and cluster-scoped resources; cluster deletion requires a
separate explicit flag.

The objective local system gate is `make e2e`: a keyless mock run through CLI,
deployed API, operator, and a real kind apiserver. Separate gates cover product
install/uninstall, pause/resume, control-plane recovery, and real GKE egress/
checkpoint behavior. The maintained matrix is in
[`verification.md`](verification.md).

## 12. Extension boundaries and non-goals

- New persistence backends implement `store.Store` and, for crash-safe launch
  semantics, `store.Durable`.
- New harnesses implement the normalized RunSpec/event/exit-code contract and
  ship a pinned image; unknown kinds must not silently gain credentials.
- New SCMs belong behind the git/finalize and PR-client seams; GitHub is the
  only implementation today.
- Wren does not currently provide autonomous merge/deploy, public SaaS
  multi-tenancy, authenticated SSO, interactive steering, managed database
  provisioning, kernel isolation, or cross-cloud infrastructure.

Future work is intentionally not duplicated here. See
[`roadmap.md`](roadmap.md).
