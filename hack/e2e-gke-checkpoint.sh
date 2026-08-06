#!/usr/bin/env bash
# Wren GKE checkpoint/restore end-to-end test (WS-21) — validates real periodic
# checkpoint snapshots and restore-from-checkpoint on a real cluster.
#
# hack/ is dev/test tooling ONLY (code standards rule 8): installing Wren on a
# real cluster is product surface — use `wren install --registry <prefix>`.
#
# Assumes a GKE Standard cluster already exists (this script does NOT create
# or delete it — same convention as hack/e2e-gke.sh) with Workload Identity
# enabled (--workload-pool set at creation) and Private Google Access on the
# node subnet (SETUP.md prerequisite for the gcs-fuse egress exemption, WS-19).
# It DOES idempotently: enable the GcsFuseCsiDriver addon, create the
# checkpoint bucket + a dedicated GSA + its bucket IAM binding, and create/bind
# the run-namespace's wren-checkpointer KSA — the one-time SETUP.md recipe,
# scripted so this gate is repeatable without hand-typed gcloud commands.
#
# What this tests beyond hack/e2e-gke.sh:
#   - RunCheckpointer's real periodic snapshot loop actually Puts tar.gz
#     objects on an interval, independently verified via `gcloud storage`.
#   - The full wren run create -> controller -> hydrate pipeline reacts to a
#     genuine workspace loss (pod AND PVC deleted together — deleting only the
#     PVC does not work against a live pod: the pvc-protection finalizer
#     blocks real deletion until the referencing pod is also gone) by
#     recreating the PVC and restoring the latest real checkpoint, with the
#     restored file CONTENT verified, not just a log line.
#   - The same loss with zero checkpoints ever taken fails deterministically
#     (PhaseFailed, not an infinite retry loop).
#   - The same loss with --checkpoint-gcs-mount OFF is unaffected (the
#     pre-WS-21 errWorkspaceLost/"WorkspaceLost" behavior, unchanged).
#
# The mock harness completes in well under a second, so "delete the PVC
# mid-run" is a real race against the run reaching Succeeded (once a run's
# Status.Phase is terminal, the reconciler never looks at the PVC again — by
# design, see agentrun_controller.go's isTerminal check). This script catches
# it by polling tightly and deleting the pod+PVC together the instant Running
# is observed, retrying the whole attempt (a fresh run) up to
# CHECKPOINT_RACE_RETRIES times if the race is lost. This is inherent to
# testing against the deterministic, near-instant mock harness — not a
# product flakiness.
#
# Usage:
#   hack/e2e-gke-checkpoint.sh
#   GKE_CLUSTER=wren-e2e GKE_ZONE=us-central1-a hack/e2e-gke-checkpoint.sh
#   E2E_KEEP=1 hack/e2e-gke-checkpoint.sh   # skip namespace teardown
#
# Images come from Artifact Registry — push them first:
#   make docker-push-gke
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# shellcheck source=lib/e2e-common.sh
source "$REPO_ROOT/hack/lib/e2e-common.sh"

GKE_PROJECT="${GKE_PROJECT:-wren-gke-fdea81}"
GKE_ZONE="${GKE_ZONE:-us-central1-a}"
GKE_CLUSTER="${GKE_CLUSTER:-wren-e2e}"
GKE_AR="${GKE_AR:-us-central1-docker.pkg.dev/${GKE_PROJECT}/wren}"
GKE_TAG="${GKE_TAG:-$(git rev-parse --short HEAD 2>/dev/null || echo dev)}"
NS_SYSTEM="wren-system"
NS_RUNS="${NS_RUNS:-wren-runs}"
CHECKPOINT_BUCKET="${CHECKPOINT_BUCKET:-wren-ckpt-e2e-${GKE_PROJECT}}"
CHECKPOINT_GSA="${CHECKPOINT_GSA:-wren-ckpt-gsa}"
CHECKPOINT_KSA="${CHECKPOINT_KSA:-wren-checkpointer}"
RUN_TIMEOUT="${RUN_TIMEOUT:-60}"
DEPLOY_TIMEOUT="${DEPLOY_TIMEOUT:-300}"
APISERVER_LOCAL_PORT="${APISERVER_LOCAL_PORT:-18093}"
CHECKPOINT_RACE_RETRIES="${CHECKPOINT_RACE_RETRIES:-6}"

WREN_CONFIG_DIR="$(mktemp -d "${TMPDIR:-/tmp}/wren-gke-ckpt-e2e-cfg.XXXXXX")"
export WREN_CONFIG_DIR

KCTX="gke_${GKE_PROJECT}_${GKE_ZONE}_${GKE_CLUSTER}"

PF_PID=""
RUN_ID=""
RUN_NS="$NS_RUNS"

STATUS="init"
cleanup() {
  local code=$?
  cleanup_common
  if [ "${E2E_KEEP:-0}" = "1" ]; then
    warn "E2E_KEEP=1 — leaving wren-system/${NS_RUNS} namespaces and the checkpoint bucket in place"
  else
    log "tearing down wren-system + ${NS_RUNS} namespaces"
    k delete namespace "$NS_SYSTEM" --ignore-not-found --timeout=60s 2>/dev/null || true
    k delete namespace "$NS_RUNS" --ignore-not-found --timeout=60s 2>/dev/null || true
    log "clearing checkpoint bucket contents (bucket itself is left for reuse across runs)"
    gcloud storage rm -r "gs://${CHECKPOINT_BUCKET}/**" --quiet 2>/dev/null || true
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

# --- 2. one-time checkpoint prerequisites (idempotent — SETUP.md's recipe) ---
log "ensuring GcsFuseCsiDriver addon is enabled"
gcloud container clusters update "$GKE_CLUSTER" --zone "$GKE_ZONE" --project "$GKE_PROJECT" \
  --update-addons GcsFuseCsiDriver=ENABLED --quiet 2>&1 | tail -5

log "ensuring checkpoint bucket gs://${CHECKPOINT_BUCKET} exists"
gcloud storage buckets create "gs://${CHECKPOINT_BUCKET}" --project "$GKE_PROJECT" \
  --location "${GKE_ZONE%-*}" --uniform-bucket-level-access 2>&1 | tail -3 || true

log "ensuring GSA ${CHECKPOINT_GSA} exists with objectAdmin on the bucket"
gcloud iam service-accounts create "$CHECKPOINT_GSA" --project "$GKE_PROJECT" 2>&1 | tail -3 || true
gcloud storage buckets add-iam-policy-binding "gs://${CHECKPOINT_BUCKET}" \
  --member="serviceAccount:${CHECKPOINT_GSA}@${GKE_PROJECT}.iam.gserviceaccount.com" \
  --role="roles/storage.objectAdmin" 2>&1 | tail -3

log "ensuring ${NS_RUNS}/${CHECKPOINT_KSA} exists and is Workload-Identity-bound"
k create namespace "$NS_RUNS" 2>/dev/null || true
k create serviceaccount "$CHECKPOINT_KSA" -n "$NS_RUNS" 2>/dev/null || true
k annotate serviceaccount "$CHECKPOINT_KSA" -n "$NS_RUNS" \
  "iam.gke.io/gcp-service-account=${CHECKPOINT_GSA}@${GKE_PROJECT}.iam.gserviceaccount.com" --overwrite 2>&1
gcloud iam service-accounts add-iam-policy-binding \
  "${CHECKPOINT_GSA}@${GKE_PROJECT}.iam.gserviceaccount.com" --project "$GKE_PROJECT" \
  --role roles/iam.workloadIdentityUser \
  --member "serviceAccount:${GKE_PROJECT}.svc.id.goog[${NS_RUNS}/${CHECKPOINT_KSA}]" 2>&1 | tail -3

# --- 3. build the wren CLI ---
log "building wren CLI"
make build
WREN="$REPO_ROOT/bin/wren"
[ -x "$WREN" ] || die "wren CLI not found at $WREN"

# --- 4. deploy the control plane with --checkpoint-gcs-mount ---
log "deploying control plane (config/default) with ${GKE_AR} images tagged ${GKE_TAG}"
k apply -f config/crd/bases/ >/dev/null
k apply -k config/default

k -n "$NS_SYSTEM" set image deploy/wren-operator  operator="${GKE_AR}/operator:${GKE_TAG}"
k -n "$NS_SYSTEM" set image deploy/wren-apiserver apiserver="${GKE_AR}/apiserver:${GKE_TAG}"
k -n "$NS_SYSTEM" patch deploy/wren-operator --type=json -p="[
  {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":[
    \"--leader-elect\",
    \"--health-probe-bind-address=:8081\",
    \"--metrics-bind-address=:8080\",
    \"--runtime-image=${GKE_AR}/runtime:${GKE_TAG}\",
    \"--checkpoint-gcs-mount\",
    \"--checkpoint-ksa=${CHECKPOINT_KSA}\"
  ]}
]"
k -n "$NS_SYSTEM" patch deploy/wren-operator --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Always"}]'
k -n "$NS_SYSTEM" patch deploy/wren-apiserver --type=json \
  -p='[{"op":"replace","path":"/spec/template/spec/containers/0/imagePullPolicy","value":"Always"}]'

log "waiting for control plane to be Ready (${DEPLOY_TIMEOUT}s)"
k -n "$NS_SYSTEM" rollout status deploy/wren-operator  --timeout="${DEPLOY_TIMEOUT}s"
k -n "$NS_SYSTEM" rollout status deploy/wren-apiserver --timeout="${DEPLOY_TIMEOUT}s"

# --- 5. port-forward + register the checkpoint-enabled project ---
log "port-forwarding svc/wren-apiserver -> localhost:${APISERVER_LOCAL_PORT}"
k -n "$NS_SYSTEM" port-forward svc/wren-apiserver "${APISERVER_LOCAL_PORT}:8090" >/dev/null 2>&1 &
PF_PID=$!
API="http://127.0.0.1:${APISERVER_LOCAL_PORT}"
wait_for_apiserver_healthz 2 30
log "apiserver reachable at ${API}"

PROJECT="gke-e2e-checkpoint"
log "creating project '$PROJECT' with checkpointBucket=gs://${CHECKPOINT_BUCKET}"
curl -fsS -X POST "${API}/v1/projects" \
  -H 'X-Wren-User: e2e' -H 'Content-Type: application/json' \
  -d "{\"name\":\"${PROJECT}\",\"defaultHarness\":\"mock\",\"harnessImage\":\"${GKE_AR}/runtime:${GKE_TAG}\",\"defaultModel\":\"mock\",\"cpu\":\"100m\",\"memory\":\"128Mi\",\"disk\":\"1Gi\",\"checkpointBucket\":\"gs://${CHECKPOINT_BUCKET}\"}" \
  >/dev/null || die "create project failed"

log "wren login -> ${API}"
"$WREN" login --control-plane "127.0.0.1:${APISERVER_LOCAL_PORT}" --user e2e >/dev/null || die "wren login failed"

# --- helpers specific to this gate ---

# create_run <task-label> -> sets RUN_ID, returns once the AgentRun exists.
create_run() {
  local task="$1" out
  out="$("$WREN" run create --project "$PROJECT" --task "$task" 2>&1)" || die "run create failed: $out"
  RUN_ID="$(printf '%s' "$out" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
  [ -n "$RUN_ID" ] || die "could not parse run id from: $out"
}

# seed_checkpoint_pod <run-id> <marker-content> -> launches a real
# checkpointer-role pod scoped to run-id, which writes marker-content into an
# emptyDir workspace and Puts real tar.gz checkpoints every 3s via the SAME
# production RunCheckpointer code path (not a direct `gcloud storage cp` —
# that bypasses the GCS FUSE mount's own directory-creation semantics and is
# invisible to a later mount's listing, a real quirk found live during this
# workstream's manual verification, not a product bug).
seed_checkpoint_pod() {
  local run_id="$1" marker="$2"
  local name="wren-e2e-seed-${run_id}"
  cat <<YAML | k apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${NS_RUNS}
  annotations:
    gke-gcsfuse/volumes: "true"
spec:
  serviceAccountName: ${CHECKPOINT_KSA}
  automountServiceAccountToken: false
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    fsGroup: 10001
  initContainers:
  - name: seed-workspace
    image: busybox:1.36
    command: ["sh", "-c", "echo '${marker}' > /workspace/marker.txt"]
    securityContext:
      runAsUser: 65532
      runAsNonRoot: true
      allowPrivilegeEscalation: false
      capabilities: {drop: ["ALL"]}
      seccompProfile: {type: RuntimeDefault}
    volumeMounts:
    - {name: workspace, mountPath: /workspace}
  containers:
  - name: checkpointer
    image: ${GKE_AR}/runtime:${GKE_TAG}
    args: ["checkpointer"]
    imagePullPolicy: Always
    env:
    - {name: WREN_RUN_ID, value: "${run_id}"}
    - {name: WREN_CHECKPOINT_BUCKET, value: "gs://${CHECKPOINT_BUCKET}"}
    - {name: WREN_CHECKPOINT_MOUNT_PATH, value: "/mnt/checkpoints"}
    - {name: WREN_CHECKPOINT_INTERVAL, value: "3"}
    securityContext:
      runAsUser: 65532
      runAsNonRoot: true
      readOnlyRootFilesystem: true
      allowPrivilegeEscalation: false
      capabilities: {drop: ["ALL"]}
      seccompProfile: {type: RuntimeDefault}
    volumeMounts:
    - {name: workspace, mountPath: /workspace, readOnly: true}
    - {name: checkpoints, mountPath: /mnt/checkpoints}
    - {name: tmp, mountPath: /tmp}
  volumes:
  - {name: workspace, emptyDir: {}}
  - {name: tmp, emptyDir: {}}
  - name: checkpoints
    csi:
      driver: gcsfuse.csi.storage.gke.io
      volumeAttributes: {bucketName: "${CHECKPOINT_BUCKET}"}
YAML
}

# catch_running_and_break <task-label> -> creates a run, polls tightly for
# Running, then deletes its pod AND PVC together (not just the PVC — a live
# pod's PVC has the kubernetes.io/pvc-protection finalizer, so it won't
# actually delete until the referencing pod is gone too). Retries with a
# fresh run up to CHECKPOINT_RACE_RETRIES times if the mock harness reaches
# Succeeded before Running is observed. Leaves RUN_ID set to the winning run.
# catch_running_and_break's 2nd arg, if non-empty, is eval'd immediately after
# a fresh RUN_ID is known — BEFORE the Running-polling loop, not after it —
# so a seed-checkpoint pod started there runs concurrently with the run's own
# race to Running. That ordering matters: this workstream's manual live
# verification found that waiting for a checkpoint to land AFTER catching
# Running (i.e. after break_workspace) reliably loses the race — the mock
# harness reaches Succeeded well within a checkpoint's tick interval, so the
# run is already terminal by the time the workspace gets broken, and the
# reconciler never revisits a terminal run's PVC. Starting the seed pod
# concurrently with (not after) the Running-race means it already has a
# checkpoint or two banked by the time Running is caught and the workspace is
# broken immediately after.
catch_running_and_break() {
  local task="$1" hook="${2:-}" attempt phase
  for attempt in $(seq 1 "$CHECKPOINT_RACE_RETRIES"); do
    create_run "$task (attempt $attempt)"
    log "  run $RUN_ID created, racing to catch it Running (attempt $attempt/$CHECKPOINT_RACE_RETRIES)"
    if [ -n "$hook" ]; then
      eval "$hook"
    fi
    phase=""
    for _ in $(seq 1 150); do
      phase="$(k -n "$NS_RUNS" get agentrun "$RUN_ID" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
      [ "$phase" = "Running" ] && break
      [ "$phase" = "Succeeded" ] && break
      sleep 0.15
    done
    if [ "$phase" = "Running" ]; then
      return 0
    fi
    warn "  lost the race for $RUN_ID (phase=$phase before Running observed) — retrying"
    [ -n "$hook" ] && k -n "$NS_RUNS" delete pod "wren-e2e-seed-${RUN_ID}" --wait=false >/dev/null 2>&1 || true
  done
  die "never caught a run Running after $CHECKPOINT_RACE_RETRIES attempts — mock harness may be slower/faster than expected"
}

break_workspace() {
  local run_id="$1"
  local pod_pid pvc_pid
  k -n "$NS_RUNS" delete pod "${run_id}-0" --wait=false 2>&1 &
  pod_pid=$!
  k -n "$NS_RUNS" delete pvc "${run_id}-workspace" --wait=false 2>&1 &
  pvc_pid=$!
  # Wait on these two PIDs specifically — a bare `wait` waits for EVERY
  # background job of this shell, including the long-lived apiserver
  # port-forward (PF_PID) started in step 5, which never exits on its own.
  # That hung this exact call for 40+ minutes the first time this script ran
  # live: found and fixed during this workstream's own e2e-script bring-up.
  wait "$pod_pid" "$pvc_pid"
}

wait_for_phase() {
  local run_id="$1" timeout="$2" deadline phase
  deadline=$(( $(date +%s) + timeout ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    phase="$(k -n "$NS_RUNS" get agentrun "$run_id" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
    case "$phase" in Succeeded|Failed) echo "$phase"; return 0 ;; esac
    sleep 2
  done
  echo "${phase:-<timeout>}"
}

# --- 6. scenario A: real periodic snapshots ---
log "scenario A: real periodic checkpoint snapshots"
SEED_RUN_ID="wren-e2e-periodic-proof"
seed_checkpoint_pod "$SEED_RUN_ID" "wren e2e periodic-snapshot proof"
deadline=$(( $(date +%s) + 60 ))
count=0
# `gcloud storage ls` on this prefix also returns a zero-byte directory-marker
# object whose key is the bare prefix itself (gcsfuse creates one when it
# MkdirAlls the directory) — it sorts lexicographically BEFORE any
# "checkpoints/ck-....tar.gz" key ('/' < alnum in ASCII), so anything
# selecting "the first entry" must filter to real .tar.gz objects or it grabs
# an empty placeholder instead of a checkpoint (found live while writing this
# gate — not a product bug, a listing-format footgun).
while [ "$(date +%s)" -lt "$deadline" ]; do
  count="$( (gcloud storage ls "gs://${CHECKPOINT_BUCKET}/runs/${SEED_RUN_ID}/checkpoints/" 2>/dev/null || true) | grep -c '\.tar\.gz$' || true)"
  [ "$count" -ge 2 ] 2>/dev/null && break
  sleep 5
done
[ "$count" -ge 2 ] 2>/dev/null || die "scenario A: expected >=2 checkpoints, found ${count}"
log "  [PASS] ${count} checkpoint object(s) landed — independently verifying via gcloud storage"
gcloud storage ls -l "gs://${CHECKPOINT_BUCKET}/runs/${SEED_RUN_ID}/checkpoints/"
first_ck="$(gcloud storage ls "gs://${CHECKPOINT_BUCKET}/runs/${SEED_RUN_ID}/checkpoints/" 2>/dev/null | grep '\.tar\.gz$' | head -1)"
tmpfile="$(mktemp)"
gcloud storage cat "$first_ck" > "$tmpfile"
tar xzOf "$tmpfile" marker.txt | grep -q "periodic-snapshot proof" \
  || die "scenario A: checkpoint content did not match the seeded marker"
rm -f "$tmpfile"
log "  [PASS] checkpoint content verified byte-for-byte (not just the checkpointer's own log claim)"
k -n "$NS_RUNS" delete pod "wren-e2e-seed-${SEED_RUN_ID}" --wait=false >/dev/null 2>&1 || true

# --- 7. scenario B: positive restore path (full real pipeline) ---
log "scenario B: positive restore — mid-run workspace loss with a real checkpoint"
# The seed pod is started concurrently with the Running-race (via the hook),
# not after catching Running — see catch_running_and_break's comment for why
# that ordering is load-bearing, not cosmetic.
catch_running_and_break "e2e-checkpoint: positive restore" \
  'seed_checkpoint_pod "$RUN_ID" "wren e2e positive-restore marker $RUN_ID"'
POS_RUN_ID="$RUN_ID"
MARKER="wren e2e positive-restore marker ${POS_RUN_ID}"
break_workspace "$POS_RUN_ID"
# The seed pod's job is done — stop it now so it doesn't keep ticking (and
# racking up objects/cost) for the rest of this script's run.
k -n "$NS_RUNS" delete pod "wren-e2e-seed-${POS_RUN_ID}" --wait=false >/dev/null 2>&1 || true
log "  workspace broken for $POS_RUN_ID; waiting for the controller to recover"
final_phase="$(wait_for_phase "$POS_RUN_ID" 120)"
[ "$final_phase" = "Succeeded" ] || die "scenario B: expected Succeeded after restore, got ${final_phase}"
log "  [PASS] run reached Succeeded after workspace loss + restore"

restart_count="$(k -n "$NS_RUNS" get agentrun "$POS_RUN_ID" -o jsonpath='{.status.restartCount}')"
[ "$restart_count" = "1" ] || die "scenario B: expected restartCount=1, got ${restart_count}"

# Verify actual restored file content — attach a throwaway pod to the same
# PVC, not just a log line claiming success.
new_pod="$(k -n "$NS_RUNS" get pods -l "wren.dev/run=${POS_RUN_ID}" -o jsonpath='{.items[0].metadata.name}')"
k -n "$NS_RUNS" delete pod "$new_pod" --wait=true --timeout=30s 2>&1 || true
cat <<YAML | k apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: wren-e2e-verify-restore
  namespace: ${NS_RUNS}
spec:
  restartPolicy: Never
  securityContext: {runAsNonRoot: true, fsGroup: 10001}
  containers:
  - name: verify
    image: busybox:1.36
    command: ["cat", "/workspace/marker.txt"]
    securityContext:
      runAsUser: 65532
      runAsNonRoot: true
      allowPrivilegeEscalation: false
      capabilities: {drop: ["ALL"]}
      seccompProfile: {type: RuntimeDefault}
    volumeMounts: [{name: workspace, mountPath: /workspace}]
  volumes:
  - name: workspace
    persistentVolumeClaim: {claimName: ${POS_RUN_ID}-workspace}
YAML
for _ in $(seq 1 20); do
  vp="$(k -n "$NS_RUNS" get pod wren-e2e-verify-restore -o jsonpath='{.status.phase}' 2>/dev/null || true)"
  [ "$vp" = "Succeeded" ] || [ "$vp" = "Failed" ] && break
  sleep 3
done
restored="$(k -n "$NS_RUNS" logs wren-e2e-verify-restore 2>/dev/null || true)"
k -n "$NS_RUNS" delete pod wren-e2e-verify-restore --wait=false >/dev/null 2>&1 || true
[ "$restored" = "$MARKER" ] || die "scenario B: restored content mismatch: got '${restored}', want '${MARKER}'"
log "  [PASS] restored file content verified byte-for-byte: ${restored}"

# --- 8. scenario C: negative restore path (no checkpoint ever taken) ---
log "scenario C: negative restore — workspace loss with zero checkpoints"
catch_running_and_break "e2e-checkpoint: negative restore"
NEG_RUN_ID="$RUN_ID"
break_workspace "$NEG_RUN_ID"
final_phase="$(wait_for_phase "$NEG_RUN_ID" 120)"
[ "$final_phase" = "Failed" ] || die "scenario C: expected Failed (no checkpoint to restore), got ${final_phase}"
reason="$(k -n "$NS_RUNS" get agentrun "$NEG_RUN_ID" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}')"
[ "$reason" = "HarnessError" ] || die "scenario C: expected reason=HarnessError, got ${reason}"
log "  [PASS] deterministic PhaseFailed/HarnessError — not an infinite retry loop"

# --- 9. scenario D: regression — checkpointing NOT configured ---
log "scenario D: regression — --checkpoint-gcs-mount off behaves exactly as before WS-21"
k -n "$NS_SYSTEM" patch deploy/wren-operator --type=json -p="[
  {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":[
    \"--leader-elect\",
    \"--health-probe-bind-address=:8081\",
    \"--metrics-bind-address=:8080\",
    \"--runtime-image=${GKE_AR}/runtime:${GKE_TAG}\"
  ]}
]"
k -n "$NS_SYSTEM" rollout status deploy/wren-operator --timeout="${DEPLOY_TIMEOUT}s"

catch_running_and_break "e2e-checkpoint: regression (flag off)"
REG_RUN_ID="$RUN_ID"
break_workspace "$REG_RUN_ID"
final_phase="$(wait_for_phase "$REG_RUN_ID" 60)"
[ "$final_phase" = "Failed" ] || die "scenario D: expected Failed (unchanged pre-WS-21 behavior), got ${final_phase}"
reason="$(k -n "$NS_RUNS" get agentrun "$REG_RUN_ID" -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}')"
[ "$reason" = "WorkspaceLost" ] || die "scenario D: expected reason=WorkspaceLost (pre-WS-21 path), got ${reason}"
restart_count="$(k -n "$NS_RUNS" get agentrun "$REG_RUN_ID" -o jsonpath='{.status.restartCount}')"
[ -z "$restart_count" ] || [ "$restart_count" = "0" ] || die "scenario D: expected no restart (fails outright), got restartCount=${restart_count}"
log "  [PASS] unchanged pre-WS-21 WorkspaceLost failure with checkpointing off"

# Restore the flag so a re-run (or a human poking at the cluster after) sees
# the feature on, matching this script's own precondition.
k -n "$NS_SYSTEM" patch deploy/wren-operator --type=json -p="[
  {\"op\":\"replace\",\"path\":\"/spec/template/spec/containers/0/args\",\"value\":[
    \"--leader-elect\",
    \"--health-probe-bind-address=:8081\",
    \"--metrics-bind-address=:8080\",
    \"--runtime-image=${GKE_AR}/runtime:${GKE_TAG}\",
    \"--checkpoint-gcs-mount\",
    \"--checkpoint-ksa=${CHECKPOINT_KSA}\"
  ]}
]"

STATUS="ok"
log "GKE checkpoint E2E PASSED — periodic snapshots, positive restore, negative restore, and the no-checkpointing regression all verified live"
