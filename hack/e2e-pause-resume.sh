#!/usr/bin/env bash
# Repeatable keyless pause/resume chaos gate.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

KIND_CLUSTER="${KIND_CLUSTER:-wren-pause-e2e}"
KCTX="kind-${KIND_CLUSTER}"
NS_SYSTEM="wren-system"
APISERVER_LOCAL_PORT="${APISERVER_LOCAL_PORT:-18091}"
RUN_TIMEOUT="${RUN_TIMEOUT:-300}"
WREN_CONFIG_DIR="$(mktemp -d "${TMPDIR:-/tmp}/wren-pause-e2e-cfg.XXXXXX")"
export WREN_CONFIG_DIR
PF_PID="" RUN_ID="" RUN_NS="" STATUS="init"

# shellcheck source=hack/lib/e2e-common.sh
source "$REPO_ROOT/hack/lib/e2e-common.sh"

cleanup() {
  local code=$?
  cleanup_common
  if [ "${E2E_KEEP:-0}" = "1" ]; then
    warn "leaving kind cluster $KIND_CLUSTER up"
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

log "building/loading images and CLI"
make kind-load KIND_CLUSTER="$KIND_CLUSTER"
make build
WREN="$REPO_ROOT/bin/wren"

# hostPath uses Directory (not DirectoryOrCreate): provision it explicitly on
# the single kind node and make it writable by the trusted non-root sidecar.
docker exec "${KIND_CLUSTER}-control-plane" mkdir -p /var/local/wren-checkpoints
docker exec "${KIND_CLUSTER}-control-plane" chmod 0777 /var/local/wren-checkpoints

k apply -k config/default >/dev/null
k -n "$NS_SYSTEM" patch deploy/wren-operator --type=json -p='[
  {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--checkpoint-local-path=/var/local/wren-checkpoints"},
  {"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--mock-delay=5m"}
]' >/dev/null
k -n "$NS_SYSTEM" rollout status deploy/wren-operator --timeout=240s
k -n "$NS_SYSTEM" rollout status deploy/wren-apiserver --timeout=240s

k -n "$NS_SYSTEM" port-forward svc/wren-apiserver "${APISERVER_LOCAL_PORT}:8090" >/dev/null 2>&1 &
PF_PID=$!
API="http://127.0.0.1:${APISERVER_LOCAL_PORT}"
wait_for_apiserver_healthz 1 45
"$WREN" login --control-plane "127.0.0.1:${APISERVER_LOCAL_PORT}" --user e2e >/dev/null

PROJECT="pause-e2e"
curl -fsS -X POST "$API/v1/projects" -H 'X-Wren-User: e2e' -H 'Content-Type: application/json' \
  -d "{\"name\":\"$PROJECT\",\"defaultHarness\":\"mock\",\"harnessImage\":\"wren/runtime:dev\",\"defaultModel\":\"mock\",\"cpu\":\"100m\",\"memory\":\"128Mi\",\"disk\":\"1Gi\"}" >/dev/null

phase_of() {
  "$WREN" run get "$1" 2>/dev/null | sed -n 's/.*"phase"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1
}

wait_phase() {
  local id="$1" want="$2" deadline phase
  deadline=$(( $(date +%s) + RUN_TIMEOUT ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    phase="$(phase_of "$id")"
    [ "$phase" = "$want" ] && return 0
    [ "$phase" = "Failed" ] && [ "$want" != "Failed" ] && die "$id failed while waiting for $want"
    sleep 1
  done
  die "$id did not reach $want (last=${phase:-unknown})"
}

create_run() {
  local task="$1" out
  out="$("$WREN" run create --project "$PROJECT" --task "$task")"
  RUN_ID="$(printf '%s' "$out" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
  RUN_NS="$(printf '%s' "$out" | sed -n 's/.*"namespace"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
  [ -n "$RUN_ID" ] && [ -n "$RUN_NS" ] || die "cannot parse run: $out"
}

log "scenario 1: verified pause, controller restart, duplicate requests, PVC-loss exact restore"
create_run "pause chaos happy path"
wait_phase "$RUN_ID" Running
"$WREN" run pause "$RUN_ID" >/dev/null

# Restart during the Pausing journal. Depending on scheduling the forced
# checkpoint may already be committed; both sides of that boundary are safe.
for _ in $(seq 1 30); do
  phase="$(phase_of "$RUN_ID")"
  [ "$phase" = "Pausing" ] || [ "$phase" = "Paused" ] && break
  sleep 0.1
done
k -n "$NS_SYSTEM" rollout restart deploy/wren-operator >/dev/null
k -n "$NS_SYSTEM" rollout status deploy/wren-operator --timeout=180s

# A duplicate pause after restart is an accepted idempotent request.
"$WREN" run pause "$RUN_ID" >/dev/null
wait_phase "$RUN_ID" Paused
detail="$("$WREN" run get "$RUN_ID")"
printf '%s' "$detail" | grep -q '"sha256"' || die "Paused run lacks checkpoint integrity metadata"
pod_count="$(k -n "$RUN_NS" get pods -l "wren.dev/run=$RUN_ID" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
[ "$pod_count" = "0" ] || die "Paused run still has $pod_count agent pod(s)"

# Destroy the PVC while paused. Two concurrent resume requests must collapse
# into one annotation/attempt, and hydrate must restore the exact pause object.
k -n "$RUN_NS" delete pvc "${RUN_ID}-workspace" --wait=true >/dev/null
curl -fsS -X POST "$API/v1/runs/$RUN_ID/resume" -H 'X-Wren-User: e2e' >/dev/null & resume_a=$!
curl -fsS -X POST "$API/v1/runs/$RUN_ID/resume" -H 'X-Wren-User: e2e' >/dev/null & resume_b=$!
wait "$resume_a"; wait "$resume_b"
wait_phase "$RUN_ID" Running
detail="$("$WREN" run get "$RUN_ID")"
restart_count="$(printf '%s' "$detail" | sed -n 's/.*"restartCount"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p' | head -1)"
[ "${restart_count:-0}" = "0" ] || die "pause/resume consumed crash retry budget: $detail"
k -n "$RUN_NS" logs -l "wren.dev/run=$RUN_ID" -c hydrate --tail=100 | grep -q 'restore-from-checkpoint PASSED' || die "hydrate did not prove exact checkpoint restore"
"$WREN" run stop "$RUN_ID" >/dev/null

log "scenario 2: missing explicit pause checkpoint fails deterministically"
create_run "pause chaos missing checkpoint"
wait_phase "$RUN_ID" Running
"$WREN" run pause "$RUN_ID" >/dev/null
wait_phase "$RUN_ID" Paused
detail="$("$WREN" run get "$RUN_ID")"
manifest="$(printf '%s' "$detail" | sed -n 's/.*"uri"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
[ -n "$manifest" ] || die "cannot parse pause manifest URI: $detail"
docker exec "${KIND_CLUSTER}-control-plane" rm -f "/var/local/wren-checkpoints/runs/$RUN_ID/$manifest"
k -n "$RUN_NS" delete pvc "${RUN_ID}-workspace" --wait=true >/dev/null
"$WREN" run resume "$RUN_ID" >/dev/null
wait_phase "$RUN_ID" Failed
k -n "$RUN_NS" logs -l "wren.dev/run=$RUN_ID" -c hydrate --tail=100 | grep -q 'exact checkpoint' || die "missing exact checkpoint failure was not diagnostic"

STATUS="ok"
log "PAUSE/RESUME E2E PASSED — verified pause, restart recovery, duplicate requests, exact PVC restore, and missing-checkpoint failure"
