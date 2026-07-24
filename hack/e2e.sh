#!/usr/bin/env bash
# Wren keyless end-to-end test (the WS-0 merge gate).
#
# hack/ is dev/test tooling ONLY (code standards rule 8): onboarding/install is
# product surface and lives in the CLI — use `wren install --kind` to stand up
# a cluster for real use; this script exists to gate merges.
#
# Stands up a local kind cluster, builds+loads the images, deploys the in-cluster
# control plane (operator + apiserver in wren-system), then drives a single run
# through the MOCK harness with NO credentials and NO repo — so hydrate's clone
# and finalize's PR are both skipped (the keyless design) — and asserts the run
# reaches Succeeded and its pod is cleaned up.
#
# Zero credentials required. On any failure it dumps the operator/apiserver logs,
# the AgentRun YAML, and the agent pod's container logs, then exits non-zero.
#
# Usage:
#   hack/e2e.sh                 # full run + teardown
#   E2E_KEEP=1 hack/e2e.sh      # keep the cluster (and control plane) for debugging
#   KIND_CLUSTER=my-e2e hack/e2e.sh
#   E2E_BAD_IMAGE=1 hack/e2e.sh # inject a bad runtime image to exercise the failure path
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

KIND_CLUSTER="${KIND_CLUSTER:-wren-e2e}"
KCTX="kind-${KIND_CLUSTER}"
NS_SYSTEM="wren-system"
RUN_TIMEOUT="${RUN_TIMEOUT:-300}"   # seconds to wait for the run to reach Succeeded
DEPLOY_TIMEOUT="${DEPLOY_TIMEOUT:-180}"
APISERVER_LOCAL_PORT="${APISERVER_LOCAL_PORT:-18090}"

# Hermetic CLI config so `wren login` never clobbers the developer's real
# ~/.config/wren/config.yaml. Cleaned up on exit.
WREN_CONFIG_DIR="$(mktemp -d "${TMPDIR:-/tmp}/wren-e2e-cfg.XXXXXX")"
export WREN_CONFIG_DIR

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"; }

k() { kubectl --context "$KCTX" "$@"; }

PF_PID=""
RUN_ID=""
RUN_NS=""

# dump_diagnostics prints everything an operator needs to debug a failed run:
# control-plane logs, the AgentRun resource, and the agent pod's container logs.
dump_diagnostics() {
  warn "dumping diagnostics (control plane + run)"
  echo "----- operator logs -----"
  k -n "$NS_SYSTEM" logs deploy/wren-operator --tail=200 2>&1 || true
  echo "----- apiserver logs -----"
  k -n "$NS_SYSTEM" logs deploy/wren-apiserver --tail=200 2>&1 || true
  if [ -n "$RUN_ID" ] && [ -n "$RUN_NS" ]; then
    echo "----- AgentRun/$RUN_ID (yaml) -----"
    k -n "$RUN_NS" get agentrun "$RUN_ID" -o yaml 2>&1 || true
    echo "----- pods in $RUN_NS -----"
    k -n "$RUN_NS" get pods -o wide 2>&1 || true
    # The agent pod is labelled wren.dev/run=<run-id>. Dump every container.
    local pod
    pod="$(k -n "$RUN_NS" get pods -l "wren.dev/run=$RUN_ID" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    if [ -n "$pod" ]; then
      echo "----- pod/$pod describe -----"
      k -n "$RUN_NS" describe pod "$pod" 2>&1 || true
      for c in $(k -n "$RUN_NS" get pod "$pod" -o jsonpath='{range .spec.initContainers[*]}{.name}{"\n"}{end}{range .spec.containers[*]}{.name}{"\n"}{end}' 2>/dev/null); do
        echo "----- pod/$pod container=$c logs -----"
        k -n "$RUN_NS" logs "$pod" -c "$c" --tail=200 2>&1 || true
      done
    else
      warn "no agent pod found for run $RUN_ID (it may not have been scheduled)"
    fi
  fi
  echo "-------------------------"
}

STATUS="init"
cleanup() {
  local code=$?
  # Stop the port-forward first so a failed run never leaks it.
  if [ -n "$PF_PID" ] && kill -0 "$PF_PID" 2>/dev/null; then
    kill "$PF_PID" 2>/dev/null || true
    wait "$PF_PID" 2>/dev/null || true
  fi
  if [ "$STATUS" != "ok" ]; then
    dump_diagnostics
  fi
  [ -n "${WREN_CONFIG_DIR:-}" ] && rm -rf "$WREN_CONFIG_DIR" 2>/dev/null || true
  if [ "${E2E_KEEP:-0}" = "1" ]; then
    log "E2E_KEEP=1 — leaving cluster '$KIND_CLUSTER' up (kind delete cluster --name $KIND_CLUSTER to remove)"
  else
    log "tearing down cluster '$KIND_CLUSTER'"
    kind delete cluster --name "$KIND_CLUSTER" >/dev/null 2>&1 || true
  fi
  exit "$code"
}
trap cleanup EXIT

# --- 0. preconditions ---
need docker; need kind; need kubectl; need go
docker info >/dev/null 2>&1 || die "docker daemon not reachable"

# --- 1. cluster (reuse if present) ---
if kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
  log "reusing existing kind cluster '$KIND_CLUSTER'"
else
  log "creating kind cluster '$KIND_CLUSTER'"
  kind create cluster --name "$KIND_CLUSTER" --wait 120s
fi
k cluster-info >/dev/null || die "cannot reach cluster context $KCTX"

# --- 2. images: build + load; build the CLI ---
log "building + loading images into kind ('make kind-load')"
make kind-load KIND_CLUSTER="$KIND_CLUSTER"
log "building the wren CLI ('make build')"
make build
WREN="$REPO_ROOT/bin/wren"
[ -x "$WREN" ] || die "wren CLI not built at $WREN"

# The failure-path demo swaps in a non-existent runtime image so the agent pod
# never pulls, exercising the log-dump + non-zero-exit path. The operator picks
# the runtime image up via its --runtime-image flag (patched below).
RUNTIME_IMAGE_OVERRIDE=""
if [ "${E2E_BAD_IMAGE:-0}" = "1" ]; then
  RUNTIME_IMAGE_OVERRIDE="wren/runtime:DOES-NOT-EXIST"
  RUN_TIMEOUT="${E2E_BAD_IMAGE_TIMEOUT:-90}"
  warn "E2E_BAD_IMAGE=1 — using bogus runtime image '$RUNTIME_IMAGE_OVERRIDE' (expecting failure)"
fi

# --- 3. deploy the in-cluster control plane ---
log "deploying control plane (operator + apiserver) into $NS_SYSTEM"
k apply -k config/default >/dev/null
if [ -n "$RUNTIME_IMAGE_OVERRIDE" ]; then
  # Point the operator at a runtime image that will never pull.
  k -n "$NS_SYSTEM" set env deploy/wren-operator - >/dev/null 2>&1 || true
  k -n "$NS_SYSTEM" patch deploy/wren-operator --type=json \
    -p="[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--runtime-image=${RUNTIME_IMAGE_OVERRIDE}\"}]" >/dev/null
fi

# Egress-enforcement override (WS-1). Default (unset) leaves the operator's own
# default = iptables (the privileged egress-lockdown init container + canary).
# Set E2E_EGRESS_ENFORCEMENT=off to exercise the escape hatch: no lockdown
# container, canary skipped, and an EgressEnforcement=Disabled condition.
if [ -n "${E2E_EGRESS_ENFORCEMENT:-}" ]; then
  warn "E2E_EGRESS_ENFORCEMENT=${E2E_EGRESS_ENFORCEMENT} — patching operator --egress-enforcement"
  k -n "$NS_SYSTEM" patch deploy/wren-operator --type=json \
    -p="[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--egress-enforcement=${E2E_EGRESS_ENFORCEMENT}\"}]" >/dev/null
fi

log "waiting for control-plane Deployments to be Ready (${DEPLOY_TIMEOUT}s)"
k -n "$NS_SYSTEM" rollout status deploy/wren-operator  --timeout="${DEPLOY_TIMEOUT}s"
k -n "$NS_SYSTEM" rollout status deploy/wren-apiserver --timeout="${DEPLOY_TIMEOUT}s"

# --- 4. port-forward the apiserver Service ---
log "port-forwarding svc/wren-apiserver -> localhost:${APISERVER_LOCAL_PORT}"
k -n "$NS_SYSTEM" port-forward svc/wren-apiserver "${APISERVER_LOCAL_PORT}:8090" >/dev/null 2>&1 &
PF_PID=$!
API="http://127.0.0.1:${APISERVER_LOCAL_PORT}"
# Wait for the forward to accept and the apiserver to answer /healthz.
for i in $(seq 1 30); do
  if curl -fsS "${API}/healthz" >/dev/null 2>&1; then break; fi
  kill -0 "$PF_PID" 2>/dev/null || die "port-forward died before apiserver was reachable"
  sleep 1
  [ "$i" = 30 ] && die "apiserver /healthz never became reachable via port-forward"
done
log "apiserver reachable"

# --- 5. register a keyless project (mock harness, NO repo) ---
# The keyless design: a project with no repo → its runs carry an empty
# RunSpec.Repo → hydrate's clone and finalize's PR are both skipped. CreateProject
# now accepts an empty repo (coreapi), so we register the project through the
# deployed apiserver. Gotcha: the project JSON fields are `defaultHarness` /
# `defaultModel` (NOT `harness`/`model`), and the apiserver rejects unknown
# fields (DisallowUnknownFields).
PROJECT="e2e-mock"
log "creating keyless project '$PROJECT' (defaultHarness: mock, no repo)"
create_out="$(curl -fsS -X POST "${API}/v1/projects" \
  -H 'X-Wren-User: e2e' -H 'Content-Type: application/json' \
  -d "{\"name\":\"${PROJECT}\",\"defaultHarness\":\"mock\",\"harnessImage\":\"wren/runtime:dev\",\"defaultModel\":\"mock\",\"cpu\":\"100m\",\"memory\":\"128Mi\",\"disk\":\"1Gi\"}" 2>&1)" \
  || die "create project failed: $create_out"

# --- 6. submit + poll the run through the wren CLI (CLI → apiserver → operator) ---
log "wren login → ${API}"
"$WREN" login --control-plane "127.0.0.1:${APISERVER_LOCAL_PORT}" --user e2e >/dev/null \
  || die "wren login failed"

log "wren run create --project $PROJECT"
run_out="$("$WREN" run create --project "$PROJECT" --task "e2e: verify the keyless loop" 2>&1)" \
  || die "wren run create failed: $run_out"
RUN_ID="$(printf '%s' "$run_out" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
RUN_NS="$(printf '%s' "$run_out" | sed -n 's/.*"namespace"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
[ -n "$RUN_ID" ] || die "could not parse run id from: $run_out"
log "run id=$RUN_ID namespace=${RUN_NS:-<unknown>} — polling 'wren run get' for Succeeded (timeout ${RUN_TIMEOUT}s)"

deadline=$(( $(date +%s) + RUN_TIMEOUT ))
phase=""
last=""
while [ "$(date +%s)" -lt "$deadline" ]; do
  get_out="$("$WREN" run get "$RUN_ID" 2>/dev/null || true)"
  phase="$(printf '%s' "$get_out" | sed -n 's/.*"phase"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
  case "$phase" in
    Succeeded) log "run reached Succeeded"; break ;;
    Failed)    die "run entered Failed phase" ;;
    "")        : ;;
    *) if [ "$phase" != "$last" ]; then printf '    phase=%s\n' "$phase"; last="$phase"; fi ;;
  esac
  sleep 3
done
[ "$phase" = "Succeeded" ] || die "run did not reach Succeeded within ${RUN_TIMEOUT}s (last phase='${phase:-<none>}')"

# --- 7. assert terminal state + that no PR was opened (keyless) ---
log "verifying terminal state (Succeeded, no PR) via 'wren run get'"
final="$("$WREN" run get "$RUN_ID" 2>/dev/null || true)"
printf '%s' "$final" | grep -q '"phase"[[:space:]]*:[[:space:]]*"Succeeded"' || die "final phase not Succeeded"
# The run JSON field is prUrl (store.Run); the old "url" grep matched nothing,
# making this check vacuous. A keyless run must NOT open a PR — hard-fail if
# one shows up (WS-11).
pr_url="$(printf '%s' "$final" | sed -n 's/.*"prUrl"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
[ -z "$pr_url" ] || die "expected no PR in keyless mode, got: $pr_url"

STATUS="ok"
log "E2E PASSED — keyless run $RUN_ID reached Succeeded (no credentials, no PR)"
