#!/usr/bin/env bash
# Product-path onboarding gate: wren install -> real CLI workflow -> uninstall.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

CLUSTER="${KIND_CLUSTER:-wren-install-e2e}"
CONTEXT="kind-${CLUSTER}"
PORT="${APISERVER_LOCAL_PORT:-18100}"
WREN_CONFIG_DIR="$(mktemp -d "${TMPDIR:-/tmp}/wren-install-product.XXXXXX")"
export WREN_CONFIG_DIR
PF_PID=""

cleanup() {
  local code=$?
  if [ -n "$PF_PID" ]; then kill "$PF_PID" >/dev/null 2>&1 || true; fi
  kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
  find "$WREN_CONFIG_DIR" -type f -delete 2>/dev/null || true
  find "$WREN_CONFIG_DIR" -depth -type d -empty -delete 2>/dev/null || true
  exit "$code"
}
trap cleanup EXIT

export PATH="/opt/homebrew/bin:$PATH"
for command in kind kubectl curl jq; do
  command -v "$command" >/dev/null || { echo "missing required command: $command" >&2; exit 1; }
done

make build
./bin/wren install --kind "$CLUSTER" --skip-credentials --harness-images none --src .

kubectl --context "$CONTEXT" -n wren-system \
  port-forward svc/wren-apiserver "${PORT}:8090" >/dev/null 2>&1 &
PF_PID=$!
for _ in $(seq 1 30); do
  curl -fsS "http://127.0.0.1:${PORT}/healthz" >/dev/null 2>&1 && break
  sleep 1
done
curl -fsS "http://127.0.0.1:${PORT}/healthz" >/dev/null

./bin/wren login --control-plane "127.0.0.1:${PORT}" --user product-e2e --org install-e2e
./bin/wren project create install-e2e-project \
  --harness mock --cpu 100m --memory 128Mi --disk 1Gi
project_list="$(./bin/wren project list)"
grep -q '^install-e2e-project[[:space:]]' <<<"$project_list"
./bin/wren project get install-e2e-project | jq -e '.name == "install-e2e-project"' >/dev/null

run_id="$(./bin/wren run create --project install-e2e-project \
  --task 'product install end-to-end' | jq -r .id)"
phase=""
for _ in $(seq 1 60); do
  phase="$(./bin/wren run get "$run_id" | jq -r .phase)"
  [ "$phase" = "Succeeded" ] && break
  sleep 2
done
[ "$phase" = "Succeeded" ] || { echo "run $run_id ended at $phase" >&2; exit 1; }
./bin/wren run list --scope all -o json | jq -e --arg id "$run_id" '.[] | select(.id == $id)' >/dev/null
./bin/wren fleet --scope all -o json | jq -e --arg id "$run_id" '.[] | select(.id == $id)' >/dev/null
run_logs="$(./bin/wren run logs "$run_id" --container harness)"
grep -q 'harness complete' <<<"$run_logs"

stop_id="$(./bin/wren run create --project install-e2e-project \
  --task 'stop lifecycle' | jq -r .id)"
./bin/wren run stop "$stop_id"
stop_phase=""
for _ in $(seq 1 30); do
  stop_phase="$(./bin/wren run get "$stop_id" | jq -r .phase)"
  [ "$stop_phase" = "Canceled" ] && break
  sleep 1
done
[ "$stop_phase" = "Canceled" ] || { echo "stopped run $stop_id ended at $stop_phase" >&2; exit 1; }
./bin/wren run rm "$stop_id"
if ./bin/wren run get "$stop_id" >/dev/null 2>&1; then
  echo "removed run $stop_id is still readable" >&2
  exit 1
fi
./bin/wren run rm "$run_id"

./bin/wren uninstall --kube-context "$CONTEXT" --run-namespace wren-runs --confirm
if kubectl --context "$CONTEXT" get namespace wren-system >/dev/null 2>&1; then
  echo "wren-system survived uninstall" >&2
  exit 1
fi
if kubectl --context "$CONTEXT" get namespace wren-runs >/dev/null 2>&1; then
  echo "wren-runs survived uninstall" >&2
  exit 1
fi
if kubectl --context "$CONTEXT" get crd agentruns.wren.dev >/dev/null 2>&1; then
  echo "AgentRun CRD survived uninstall" >&2
  exit 1
fi

echo "PRODUCT INSTALL/CLI/UNINSTALL E2E PASSED"
