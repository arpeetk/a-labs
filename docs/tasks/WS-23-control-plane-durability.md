# WS-23 — Crash-safe control plane, durable event journal, and operations UX

## Outcome

Make an accepted run recoverable across API crashes and replica failover, make
the harness event stream queryable without relying on a live pod, and make the
native fleet UI comfortable to operate for long sessions.

## Durability contract

- A run row, its submission event, and its Kubernetes launch intent commit in
  one Postgres transaction.
- Replicas claim launch work with `FOR UPDATE SKIP LOCKED` and finite leases.
- Replaying a launch against an existing identical `AgentRun` succeeds; a
  conflicting immutable specification fails loudly.
- Harness events use `(run, source, source_id)` idempotency keys, so gateway or
  process restart replays are exactly-once in the journal.
- Schema migration is serialized across HA API replicas with a transaction-level
  advisory lock. Readiness fails when Postgres is unavailable.
- The untrusted harness receives no gateway credential. It writes JSONL to pod
  IPC; the gateway forwards through the trusted proxy, which injects a token and
  fixes the run identity.

## Production shape

`config/production-gcp` runs two apiservers against Postgres through Cloud SQL
Auth Proxy, two leader-elected operators, topology spreading, and disruption
budgets. See `docs/production.md` for the required secret and Workload Identity
contract. Cloud SQL instance/database provisioning itself remains external.

## Verification

- Store conformance runs against memory and real Postgres, including exclusive
  claims, lease loss, retry, completion, and event replay.
- Concurrent fresh-schema `NewPostgres` calls prove migration serialization.
- `make e2e-control-plane-recovery` starts two apiservers on Postgres, pauses all
  dispatch, commits a run, replaces every API process, enables competing
  workers, and proves one `AgentRun`, terminal success, and gateway journal
  delivery.
- Frontend unit/build/audit and native Computer Use validation cover the new
  segmented filters, readable log surface, and recovery journal.
- `make e2e-gke-provisioned` creates a disposable Standard cluster and crosses
  explicit zone/machine-family candidates after a GCE stockout, runs the real
  egress-enforcement gate, and deletes the exact test cluster on exit.
- Live GKE validation passed on `us-west1-b`/`n2-standard-2`: the keyless run
  reached `Succeeded`, `EgressEnforcement=True`, the IPv4/IPv6 lockdown init
  completed, and the canary proved direct egress blocked with the proxy usable.
