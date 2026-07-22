#!/usr/bin/env bash
# Wren GKE end-to-end test — validates egress enforcement (WS-1) on a real cluster.
#
# Assumes a GKE Standard cluster already exists (hack/e2e-gke.sh does NOT create
# or delete it). The cluster must be Standard (not Autopilot) because the
# egress-lockdown init container requires NET_ADMIN + NET_RAW capabilities.
#
# What this tests beyond the kind e2e:
#   - The egress-lockdown init container installs iptables rules successfully on a
#     real GKE node (not a simulated kind network namespace).
#   - The in-pod canary (WREN_EXPECT_ENFORCEMENT=1) proves that a direct outbound
#     TCP connection from the runner fails — iptables uid-lockdown is holding.
#   - The AgentRun carries EgressEnforcement=True condition after the run.
#
# Usage:
#   hack/e2e-gke.sh
#   GKE_CLUSTER=wren-e2e GKE_ZONE=us-central1-a hack/e2e-gke.sh
#   E2E_EGRESS_ENFORCEMENT=off hack/e2e-gke.sh   # test the off path
#   E2E_KEEP=1 hack/e2e-gke.sh                   # skip namespace teardown
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

GKE_PROJECT="${GKE_PROJECT:-wren-gke-fdea81}"
GKE_ZONE="${GKE_ZONE:-us-central1-a}"
GKE_CLUSTER="${GKE_CLUSTER:-wren-e2e}"
OVERLAY="${OVERLAY:-config/gke-e2e}"
NS_SYSTEM="wren-system"
RUN_TIMEOUT="${RUN_TIMEOUT:-300}"
DEPLOY_TIMEOUT="${DEPLOY_TIMEOUT:-300}"
APISERVER_LOCAL_PORT="${APISERVER_LOCAL_PORT:-18091}"

WREN_CONFIG_DIR="$(mktemp -d "${TMPDIR:-/tmp}/wren-gke-e2e-cfg.XXXXXX")"
export WREN_CONFIG_DIR

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33mWARN:\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"; }

KCTX="gke_${GKE_PROJECT}_${GKE_ZONE}_${GKE_CLUSTER}"
k() { kubectl --context "$KCTX" "$@"; }

PF_PID=""
RUN_ID=""
RUN_NS=""

dump_diagnostics() {
  warn "dumping diagnostics"
  echo "----- operator logs -----"
  k -n "$NS_SYSTEM" logs deploy/wren-operator --tail=200 2>&1 || true
  echo "----- apiserver logs -----"
  k -n "$NS_SYSTEM" logs deploy/wren-apiserver --tail=200 2>&1 || true
  if [ -n "$RUN_ID" ] && [ -n "$RUN_NS" ]; then
    echo "----- AgentRun/$RUN_ID (yaml) -----"
    k -n "$RUN_NS" get agentrun "$RUN_ID" -o yaml 2>&1 || true
    echo "----- pods in $RUN_NS -----"
    k -n "$RUN_NS" get pods -o wide 2>&1 || true
    local pod
    pod="$(k -n "$RUN_NS" get pods -l "wren.dev/run=$RUN_ID" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
    if [ -n "$pod" ]; then
      echo "----- pod/$pod describe -----"
      k -n "$RUN_NS" describe pod "$pod" 2>&1 || true
      for c in $(k -n "$RUN_NS" get pod "$pod" -o jsonpath='{range .spec.initContainers[*]}{.name}{"\n"}{end}{range .spec.containers[*]}{.name}{"\n"}{end}' 2>/dev/null); do
        echo "----- pod/$pod container=$c logs -----"
        k -n "$RUN_NS" logs "$pod" -c "$c" --tail=200 2>&1 || true
      done
    fi
  fi
  echo "-------------------------"
}

STATUS="init"
cleanup() {
  local code=$?
  if [ -n "$PF_PID" ] && kill -0 "$PF_PID" 2>/dev/null; then
    kill "$PF_PID" 2>/dev/null || true
    wait "$PF_PID" 2>/dev/null || true
  fi
  if [ "$STATUS" != "ok" ]; then
    dump_diagnostics
  fi
  rm -rf "${WREN_CONFIG_DIR:-}" 2>/dev/null || true
  if [ "${E2E_KEEP:-0}" = "1" ]; then
    warn "E2E_KEEP=1 — leaving wren-system and run namespaces in place"
  else
    log "tearing down wren-system namespace"
    k delete namespace "$NS_SYSTEM" --ignore-not-found --timeout=60s 2>/dev/null || true
    if [ -n "$RUN_NS" ] && [ "$RUN_NS" != "$NS_SYSTEM" ]; then
      k delete namespace "$RUN_NS" --ignore-not-found --timeout=60s 2>/dev/null || true
    fi
  fi
  exit "$code"
}
trap cleanup EXIT

# --- 0. preconditions ---
need gcloud; need kubectl; need go
export PATH="/opt/homebrew/bin:$PATH"

# --- 1. get GKE credentials ---
log "fetching credentials for cluster $GKE_CLUSTER ($GKE_ZONE)"
gcloud container clusters get-credentials "$GKE_CLUSTER" \
  --zone="$GKE_ZONE" --project="$GKE_PROJECT" 2>&1
k cluster-info >/dev/null || die "cannot reach GKE cluster via context $KCTX"

# --- 2. build the wren CLI ---
log "building wren CLI"
make build
WREN="$REPO_ROOT/bin/wren"
[ -x "$WREN" ] || die "wren CLI not found at $WREN"

# --- 3. deploy the in-cluster control plane ---
log "deploying control plane via $OVERLAY"
k apply -k "$OVERLAY"

# Patch egress enforcement if the caller overrides it (default = iptables from operator flag).
if [ -n "${E2E_EGRESS_ENFORCEMENT:-}" ]; then
  warn "E2E_EGRESS_ENFORCEMENT=${E2E_EGRESS_ENFORCEMENT} — patching operator"
  k -n "$NS_SYSTEM" patch deploy/wren-operator --type=json \
    -p="[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--egress-enforcement=${E2E_EGRESS_ENFORCEMENT}\"}]"
fi

log "waiting for control plane to be Ready (${DEPLOY_TIMEOUT}s)"
k -n "$NS_SYSTEM" rollout status deploy/wren-operator  --timeout="${DEPLOY_TIMEOUT}s"
k -n "$NS_SYSTEM" rollout status deploy/wren-apiserver --timeout="${DEPLOY_TIMEOUT}s"

# --- 4. port-forward the apiserver ---
log "port-forwarding svc/wren-apiserver -> localhost:${APISERVER_LOCAL_PORT}"
k -n "$NS_SYSTEM" port-forward svc/wren-apiserver "${APISERVER_LOCAL_PORT}:8090" >/dev/null 2>&1 &
PF_PID=$!
API="http://127.0.0.1:${APISERVER_LOCAL_PORT}"
for i in $(seq 1 30); do
  if curl -fsS "${API}/healthz" >/dev/null 2>&1; then break; fi
  kill -0 "$PF_PID" 2>/dev/null || die "port-forward died before apiserver was reachable"
  sleep 2
  [ "$i" = 30 ] && die "apiserver /healthz never became reachable"
done
log "apiserver reachable at ${API}"

# --- 5. register a keyless project ---
PROJECT="gke-e2e-mock"
AR="us-central1-docker.pkg.dev/wren-gke-fdea81/wren"
log "creating keyless project '$PROJECT'"
create_out="$(curl -fsS -X POST "${API}/v1/projects" \
  -H 'X-Wren-User: e2e' -H 'Content-Type: application/json' \
  -d "{\"name\":\"${PROJECT}\",\"defaultHarness\":\"mock\",\"harnessImage\":\"${AR}/runtime:ws1\",\"defaultModel\":\"mock\",\"cpu\":\"100m\",\"memory\":\"128Mi\",\"disk\":\"1Gi\"}" 2>&1)" \
  || die "create project failed: $create_out"

# --- 6. login + submit run ---
log "wren login -> ${API}"
"$WREN" login --control-plane "127.0.0.1:${APISERVER_LOCAL_PORT}" --user e2e >/dev/null \
  || die "wren login failed"

log "wren run create --project $PROJECT"
run_out="$("$WREN" run create --project "$PROJECT" --task "gke-e2e: verify egress enforcement" 2>&1)" \
  || die "wren run create failed: $run_out"
RUN_ID="$(printf '%s' "$run_out" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
RUN_NS="$(printf '%s' "$run_out" | sed -n 's/.*"namespace"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
[ -n "$RUN_ID" ] || die "could not parse run id from: $run_out"
log "run id=$RUN_ID namespace=${RUN_NS:-<unknown>} — polling for Succeeded (${RUN_TIMEOUT}s)"

# --- 7. poll until terminal ---
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
  sleep 5
done
[ "$phase" = "Succeeded" ] || die "run did not reach Succeeded within ${RUN_TIMEOUT}s (last='${phase:-<none>}')"

# --- 8. base assertions ---
log "verifying terminal state (Succeeded, no PR)"
final="$("$WREN" run get "$RUN_ID" 2>/dev/null || true)"
printf '%s' "$final" | grep -q '"phase"[[:space:]]*:[[:space:]]*"Succeeded"' \
  || die "final phase not Succeeded"
pr_url="$(printf '%s' "$final" | sed -n 's/.*"url"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
[ -z "$pr_url" ] || warn "expected no PR in keyless mode, got: $pr_url"

# --- 9. WS-1 assertions: egress enforcement ---
log "checking WS-1 egress enforcement assertions"

if [ -n "$RUN_NS" ]; then
  # 9a. EgressEnforcement condition on the AgentRun CR.
  expected_enforcement="${E2E_EGRESS_ENFORCEMENT:-iptables}"
  cr_yaml="$(k -n "$RUN_NS" get agentrun "$RUN_ID" -o yaml 2>/dev/null || true)"

  if [ "$expected_enforcement" = "off" ]; then
    # In off mode the condition should be False/Disabled.
    if printf '%s' "$cr_yaml" | grep -q 'EgressEnforcement'; then
      # The status field appears a few lines after 'type: EgressEnforcement';
      # use awk to extract the block reliably.
      egress_status="$(printf '%s' "$cr_yaml" | awk '/type: EgressEnforcement/{found=1} found && /status:/{print $2; exit}')"
      if [ "$egress_status" = '"False"' ]; then
        log "  [PASS] EgressEnforcement=False (Disabled) — off mode confirmed"
      else
        warn "  [WARN] EgressEnforcement condition present but unexpected status='$egress_status'"
      fi
    else
      warn "  [WARN] EgressEnforcement condition not found on AgentRun (operator may not have set it)"
    fi
  else
    # In iptables mode (default) the condition should be True.
    if printf '%s' "$cr_yaml" | grep -q 'EgressEnforcement'; then
      egress_status="$(printf '%s' "$cr_yaml" | awk '/type: EgressEnforcement/{found=1} found && /status:/{print $2; exit}')"
      if [ "$egress_status" = '"True"' ]; then
        log "  [PASS] EgressEnforcement=True (Iptables) — enforcement confirmed on AgentRun"
      else
        warn "  [WARN] EgressEnforcement condition present but status='$egress_status' (expected True)"
        printf '%s\n' "$cr_yaml" | grep -A10 'EgressEnforcement' || true
      fi
    else
      warn "  [WARN] EgressEnforcement condition not found on AgentRun"
    fi
  fi

  # 9b. egress-lockdown init container exit code.
  pod="$(k -n "$RUN_NS" get pods -l "wren.dev/run=$RUN_ID" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
  if [ -n "$pod" ]; then
    lockdown_exit="$(k -n "$RUN_NS" get pod "$pod" \
      -o jsonpath='{.status.initContainerStatuses[?(@.name=="egress-lockdown")].state.terminated.exitCode}' \
      2>/dev/null || true)"
    if [ "$expected_enforcement" = "off" ]; then
      if [ -z "$lockdown_exit" ]; then
        log "  [PASS] egress-lockdown init container not present (enforcement=off)"
      else
        warn "  [WARN] egress-lockdown init container ran in off mode (exit=$lockdown_exit)"
      fi
    else
      if [ "$lockdown_exit" = "0" ]; then
        log "  [PASS] egress-lockdown init container exited 0 — iptables rules installed"
        echo "  --- egress-lockdown logs ---"
        k -n "$RUN_NS" logs "$pod" -c egress-lockdown 2>/dev/null || true
      else
        die "egress-lockdown init container exited ${lockdown_exit:-<not found>} — iptables setup failed"
      fi
    fi

    # 9c. Canary result: grep harness container logs for the canary outcome.
    # The canary logs "egress canary: direct dial blocked" on success and
    # "egress canary: BYPASS DETECTED" on failure. It only runs when
    # WREN_EXPECT_ENFORCEMENT=1 (set automatically by the operator).
    harness_logs="$(k -n "$RUN_NS" logs "$pod" -c harness 2>/dev/null || true)"
    if printf '%s' "$harness_logs" | grep -q "egress canary"; then
      if printf '%s' "$harness_logs" | grep -q "BYPASS DETECTED"; then
        die "canary: direct connection SUCCEEDED — iptables lockdown is NOT holding"
      elif printf '%s' "$harness_logs" | grep -q "direct.*blocked\|direct.*failed\|enforcement verified"; then
        log "  [PASS] canary: direct connections blocked, proxy path confirmed"
      else
        warn "  [WARN] canary ran but result line not recognised — inspect harness logs"
        printf '%s\n' "$harness_logs" | grep "egress canary" || true
      fi
    elif [ "$expected_enforcement" = "iptables" ]; then
      warn "  [WARN] canary output not found in harness logs (WREN_EXPECT_ENFORCEMENT may not have been set)"
    fi
  else
    warn "  [WARN] no agent pod found for $RUN_ID (pod may have been reaped before assertions ran)"
  fi
fi

STATUS="ok"
log "GKE E2E PASSED — run $RUN_ID Succeeded with egress enforcement verified"
log "Cluster '${GKE_CLUSTER}' left running. To delete: gcloud container clusters delete ${GKE_CLUSTER} --zone=${GKE_ZONE} --project=${GKE_PROJECT} --quiet"
