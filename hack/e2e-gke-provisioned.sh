#!/usr/bin/env bash
# Provision a disposable GKE Standard cluster, run the real-cluster gate, and
# remove the cluster again. Candidate order intentionally crosses both machine
# families and zones because GCE stockouts are capacity failures, not quota
# failures, and a create operation can take ~35 minutes to report one.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

# shellcheck source=hack/lib/e2e-common.sh
source "$REPO_ROOT/hack/lib/e2e-common.sh"

need gcloud; need docker; need make

GKE_PROJECT="${GKE_PROJECT:-wren-gke-fdea81}"
GKE_CLUSTER="${GKE_CLUSTER:-wren-e2e}"
GKE_TAG="${GKE_TAG:-$(git rev-parse --short HEAD 2>/dev/null || echo dev)}"
GKE_AR="${GKE_AR:-us-central1-docker.pkg.dev/${GKE_PROJECT}/wren}"
GKE_CANDIDATES="${GKE_CANDIDATES:-us-west1-b:n2-standard-2 us-west1-c:n2-standard-2 us-central1-c:n2-standard-2}"
GKE_DISK_SIZE="${GKE_DISK_SIZE:-40}"
GKE_E2E_GATE="${GKE_E2E_GATE:-egress}"
GKE_BUILD_IMAGES="${GKE_BUILD_IMAGES:-1}"

case "$GKE_E2E_GATE" in
  egress) gate_script="$REPO_ROOT/hack/e2e-gke.sh" ;;
  checkpoint) gate_script="$REPO_ROOT/hack/e2e-gke-checkpoint.sh" ;;
  *) die "unknown GKE_E2E_GATE '$GKE_E2E_GATE' (want egress or checkpoint)" ;;
esac

created_zone=""
cleanup_cluster() {
  local code=$?
  if [ -n "$created_zone" ]; then
    log "deleting disposable GKE cluster ${GKE_CLUSTER} (${created_zone})"
    if ! gcloud container clusters delete "$GKE_CLUSTER" \
      --project="$GKE_PROJECT" --zone="$created_zone" --quiet; then
      warn "failed to delete disposable cluster ${GKE_CLUSTER} in ${created_zone}"
      code=1
    elif gcloud container clusters describe "$GKE_CLUSTER" \
      --project="$GKE_PROJECT" --zone="$created_zone" >/dev/null 2>&1; then
      warn "disposable cluster ${GKE_CLUSTER} is still present after deletion"
      code=1
    fi
  fi
  exit "$code"
}
trap cleanup_cluster EXIT

if gcloud container clusters list --project="$GKE_PROJECT" \
  --format='value(name)' | grep -Fxq "$GKE_CLUSTER"; then
  die "cluster ${GKE_CLUSTER} already exists; refusing to adopt or delete it"
fi

# Publish the exact commit under test before starting a billable cluster. Set
# GKE_BUILD_IMAGES=0 only when the caller has already pushed all three images
# under GKE_AR/GKE_TAG; the existing-cluster gates intentionally retain that
# faster, explicit workflow.
if [ "$GKE_BUILD_IMAGES" = "1" ]; then
  registry_host="${GKE_AR%%/*}"
  log "building and publishing linux/amd64 images tagged ${GKE_TAG}"
  gcloud auth configure-docker "$registry_host" --quiet >/dev/null
  make docker-push-gke GKE_PROJECT="$GKE_PROJECT" GKE_AR="$GKE_AR" GKE_TAG="$GKE_TAG"
elif [ "$GKE_BUILD_IMAGES" != "0" ]; then
  die "GKE_BUILD_IMAGES must be 0 or 1, got '${GKE_BUILD_IMAGES}'"
fi

for candidate in $GKE_CANDIDATES; do
  zone="${candidate%%:*}"
  machine="${candidate#*:}"
  log "creating ${GKE_CLUSTER} in ${zone} with ${machine}"
  create_log="$(mktemp "${TMPDIR:-/tmp}/wren-gke-create.XXXXXX")"
  if gcloud container clusters create "$GKE_CLUSTER" \
    --project="$GKE_PROJECT" --zone="$zone" --release-channel=regular \
    --machine-type="$machine" --num-nodes=1 --disk-size="$GKE_DISK_SIZE" \
    --enable-ip-alias --no-enable-basic-auth \
    --workload-pool="${GKE_PROJECT}.svc.id.goog" --quiet >"$create_log" 2>&1; then
    created_zone="$zone"
    rm -f "$create_log"
    break
  fi

  create_error="$(<"$create_log")"
  rm -f "$create_log"
  warn "cluster create failed for ${zone}/${machine}: ${create_error}"
  # Failed creates can leave a cluster shell behind. Remove only the exact
  # disposable name before trying the next capacity pool.
  if gcloud container clusters describe "$GKE_CLUSTER" \
    --project="$GKE_PROJECT" --zone="$zone" >/dev/null 2>&1; then
    gcloud container clusters delete "$GKE_CLUSTER" \
      --project="$GKE_PROJECT" --zone="$zone" --quiet
  fi
done

[ -n "$created_zone" ] || die "all GKE capacity candidates failed: ${GKE_CANDIDATES}"

log "running the ${GKE_E2E_GATE} GKE gate on ${created_zone} with image tag ${GKE_TAG}"
GKE_PROJECT="$GKE_PROJECT" GKE_CLUSTER="$GKE_CLUSTER" GKE_ZONE="$created_zone" \
  GKE_AR="$GKE_AR" GKE_TAG="$GKE_TAG" E2E_PROVISIONED=1 "$gate_script"

log "GKE PROVISIONED E2E PASSED — ${GKE_E2E_GATE} gate succeeded on ${created_zone}"
