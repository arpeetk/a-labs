#!/usr/bin/env bash
# Durable control-plane chaos gate: two apiservers share Postgres, accept a run
# while every dispatcher is disabled, are replaced, then recover the committed
# launch exactly once and retain its ordered event journal.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"
# shellcheck source=hack/lib/e2e-common.sh
source "$REPO_ROOT/hack/lib/e2e-common.sh"

KIND_CLUSTER="${KIND_CLUSTER:-wren-control-plane-recovery}"
KCTX="kind-${KIND_CLUSTER}"
NS_SYSTEM="wren-system"
RUN_TIMEOUT="${RUN_TIMEOUT:-300}"
DEPLOY_TIMEOUT="${DEPLOY_TIMEOUT:-240}"
APISERVER_LOCAL_PORT="${APISERVER_LOCAL_PORT:-18092}"
APISERVER_RECOVERY_PORT="${APISERVER_RECOVERY_PORT:-18093}"
WREN_CONFIG_DIR="$(mktemp -d "${TMPDIR:-/tmp}/wren-recovery-cfg.XXXXXX")"
export WREN_CONFIG_DIR
PF_PID="" RUN_ID="" RUN_NS="wren-runs" STATUS="init"
PF_LOG="$WREN_CONFIG_DIR/port-forward.log"

start_port_forward() {
  local port="$1" pod
  pod="$(k -n "$NS_SYSTEM" get pods -l app.kubernetes.io/name=wren-apiserver \
    --field-selector=status.phase=Running --sort-by=.metadata.creationTimestamp -o name | tail -1)"
  [ -n "$pod" ] || die "no Running apiserver pod available for port-forward"
  : >"$PF_LOG"
  k -n "$NS_SYSTEM" port-forward "$pod" "${port}:8090" >"$PF_LOG" 2>&1 &
  PF_PID=$!
}

cleanup() {
  local code=$?
  cleanup_common
  if [ "${E2E_KEEP:-0}" = "1" ]; then
    log "E2E_KEEP=1 — leaving cluster '$KIND_CLUSTER' running"
  else
    kind delete cluster --name "$KIND_CLUSTER" >/dev/null 2>&1 || true
  fi
  exit "$code"
}
trap cleanup EXIT

need docker; need kind; need kubectl; need curl; need go
docker info >/dev/null 2>&1 || die "docker daemon not reachable"
if ! kind get clusters 2>/dev/null | grep -qx "$KIND_CLUSTER"; then
  kind create cluster --name "$KIND_CLUSTER" --wait 120s
fi
k cluster-info >/dev/null

log "building and loading Wren images"
if [ "${E2E_SKIP_BUILD:-0}" = "1" ]; then
  kind load docker-image wren/runtime:dev wren/operator:dev wren/apiserver:dev --name "$KIND_CLUSTER"
else
  make kind-load KIND_CLUSTER="$KIND_CLUSTER"
fi
make build
WREN="$REPO_ROOT/bin/wren"

log "installing Postgres and authenticated gateway prerequisites"
k create namespace "$NS_SYSTEM" --dry-run=client -o yaml | k apply -f - >/dev/null
k apply -f config/apiserver/service_account.yaml >/dev/null
k create namespace "$RUN_NS" --dry-run=client -o yaml | k apply -f - >/dev/null
k -n "$NS_SYSTEM" create secret generic wren-gateway-token --from-literal=token=recovery-e2e-token --dry-run=client -o yaml | k apply -f - >/dev/null
k -n "$RUN_NS" create secret generic wren-gateway-token --from-literal=token=recovery-e2e-token --dry-run=client -o yaml | k apply -f - >/dev/null
k apply -f - >/dev/null <<'YAML'
apiVersion: apps/v1
kind: Deployment
metadata: {name: postgres, namespace: wren-system}
spec:
  replicas: 1
  selector: {matchLabels: {app: postgres}}
  template:
    metadata: {labels: {app: postgres}}
    spec:
      containers:
      - name: postgres
        image: postgres:17-alpine
        env:
        - {name: POSTGRES_DB, value: wren}
        - {name: POSTGRES_USER, value: wren}
        - {name: POSTGRES_PASSWORD, value: recovery-password}
        ports: [{name: postgres, containerPort: 5432}]
        readinessProbe:
          exec: {command: [pg_isready, -U, wren, -d, wren]}
          periodSeconds: 2
---
apiVersion: v1
kind: Service
metadata: {name: postgres, namespace: wren-system}
spec:
  selector: {app: postgres}
  ports: [{name: postgres, port: 5432, targetPort: 5432}]
YAML
k -n "$NS_SYSTEM" rollout status deploy/postgres --timeout="${DEPLOY_TIMEOUT}s"

log "starting two apiservers with durable dispatch intentionally paused"
k apply -k config/default >/dev/null
k -n "$NS_SYSTEM" patch deploy/wren-apiserver --type=strategic -p "$(cat <<'YAML'
spec:
  replicas: 2
  template:
    spec:
      containers:
      - name: apiserver
        args: ["--addr=:8090", "--store=postgres", "--inline-dispatch=false", "--outbox-worker=false"]
        env:
        - name: DATABASE_URL
          value: postgresql://wren:recovery-password@postgres.wren-system.svc:5432/wren?sslmode=disable
YAML
)" >/dev/null
k -n "$NS_SYSTEM" patch deploy/wren-operator --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--mock-delay=20s"}]' >/dev/null
k -n "$NS_SYSTEM" rollout status deploy/wren-operator --timeout="${DEPLOY_TIMEOUT}s"
k -n "$NS_SYSTEM" rollout status deploy/wren-apiserver --timeout="${DEPLOY_TIMEOUT}s"

start_port_forward "$APISERVER_LOCAL_PORT"
API="http://127.0.0.1:${APISERVER_LOCAL_PORT}"
wait_for_apiserver_healthz 1 45
curl -fsS "$API/readyz" >/dev/null || die "database-aware readiness failed"

PROJECT="recovery-$(date +%s)"
curl -fsS -X POST "$API/v1/projects" -H 'X-Wren-User: e2e' -H 'Content-Type: application/json' \
  -d "{\"name\":\"${PROJECT}\",\"defaultHarness\":\"mock\",\"harnessImage\":\"wren/runtime:dev\",\"cpu\":\"100m\",\"memory\":\"128Mi\",\"disk\":\"1Gi\",\"namespace\":\"wren-runs\"}" >/dev/null
run_json="$(curl -fsS -X POST "$API/v1/runs" -H 'X-Wren-User: e2e' -H 'Content-Type: application/json' \
  -d "{\"project\":\"${PROJECT}\",\"task\":\"prove restart-safe durable launch\"}")"
RUN_ID="$(printf '%s' "$run_json" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
[ -n "$RUN_ID" ] || die "could not parse durable run: $run_json"
sleep 3
if k -n "$RUN_NS" get agentrun "$RUN_ID" >/dev/null 2>&1; then
  die "run launched while both dispatch paths were disabled"
fi
curl -fsS "$API/v1/runs/$RUN_ID/events" -H 'X-Wren-User: e2e' | grep -q 'run.submitted' \
  || die "submission was not journaled before process replacement"

before_uids="$(k -n "$NS_SYSTEM" get pods -l app.kubernetes.io/name=wren-apiserver -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' | sort)"
log "replacing every API process and enabling competing leased workers"
k -n "$NS_SYSTEM" patch deploy/wren-apiserver --type=strategic -p "$(cat <<'YAML'
spec:
  template:
    spec:
      containers:
      - name: apiserver
        args: ["--addr=:8090", "--store=postgres", "--inline-dispatch=true", "--outbox-worker=true"]
YAML
)" >/dev/null
k -n "$NS_SYSTEM" rollout status deploy/wren-apiserver --timeout="${DEPLOY_TIMEOUT}s"
after_uids="$(k -n "$NS_SYSTEM" get pods -l app.kubernetes.io/name=wren-apiserver -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' | sort)"
[ "$before_uids" != "$after_uids" ] || die "apiserver processes were not replaced"

if kill -0 "$PF_PID" 2>/dev/null; then kill "$PF_PID" 2>/dev/null || true; wait "$PF_PID" 2>/dev/null || true; fi
start_port_forward "$APISERVER_RECOVERY_PORT"
APISERVER_LOCAL_PORT="$APISERVER_RECOVERY_PORT"
API="http://127.0.0.1:${APISERVER_LOCAL_PORT}"
wait_for_apiserver_healthz 1 45

deadline=$(( $(date +%s) + 60 ))
until k -n "$RUN_NS" get agentrun "$RUN_ID" >/dev/null 2>&1; do
  [ "$(date +%s)" -lt "$deadline" ] || die "restarted workers did not publish the durable run"
  sleep 1
done
[ "$(k -n "$RUN_NS" get agentruns -o name | grep -c "$RUN_ID")" = "1" ] || die "durable replay created duplicate AgentRuns"

"$WREN" login --control-plane "127.0.0.1:${APISERVER_LOCAL_PORT}" --user e2e >/dev/null
poll_run_until_succeeded 2
events="$(curl -fsS "$API/v1/runs/$RUN_ID/events?limit=1000" -H 'X-Wren-User: e2e')"
printf '%s' "$events" | grep -q 'run.launch_accepted' || die "launch acknowledgement missing from journal"
printf '%s' "$events" | grep -Eq '"source"[[:space:]]*:[[:space:]]*"gateway"' || die "gateway did not deliver harness events"
[ "$(printf '%s' "$events" | grep -Eo '"sourceId"[[:space:]]*:[[:space:]]*"launch/'"$RUN_ID"'"' | wc -l | tr -d ' ')" = "1" ] \
  || die "launch acknowledgement was not exactly-once"

STATUS="ok"
log "CONTROL-PLANE RECOVERY E2E PASSED — HA migration, process replacement, outbox replay, and gateway journal verified"
