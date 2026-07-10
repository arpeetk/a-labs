#!/usr/bin/env bash
# Wren setup (Phase 1: existing cluster, PAT-first).
#
# Configures access to an already-provisioned cluster, installs the CRDs + RBAC,
# builds/publishes the runtime images, and creates the per-namespace credential
# secrets (GitHub PAT + Anthropic key). It does NOT create the cluster.
#
# Usage:
#   # against a kind cluster (images loaded locally):
#   KIND_CLUSTER=wren-test WREN_NS=user-me \
#   GITHUB_TOKEN=$(gh auth token) ANTHROPIC_API_KEY=sk-... \
#     hack/setup.sh
#
#   # against GKE (images pushed to a registry):
#   GKE_PROJECT=my-proj GKE_CLUSTER=wren GKE_ZONE=us-central1-a \
#   REGISTRY=us-central1-docker.pkg.dev/my-proj/wren WREN_NS=user-me \
#   GITHUB_TOKEN=... ANTHROPIC_API_KEY=... \
#     hack/setup.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

WREN_NS="${WREN_NS:-wren-runs}"
RUNTIME_TAG="${RUNTIME_TAG:-dev}"

log() { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
die() { printf '\033[1;31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }
need() { command -v "$1" >/dev/null 2>&1 || die "missing required tool: $1"; }

# --- 0. preconditions ---
need kubectl; need docker; need go
[ -n "${GITHUB_TOKEN:-}" ] || die "set GITHUB_TOKEN (e.g. GITHUB_TOKEN=\$(gh auth token))"

# --- 1. cluster access ---
if [ -n "${GKE_CLUSTER:-}" ]; then
  need gcloud
  log "fetching credentials for GKE cluster $GKE_CLUSTER"
  gcloud container clusters get-credentials "$GKE_CLUSTER" \
    --zone "${GKE_ZONE:?set GKE_ZONE}" --project "${GKE_PROJECT:?set GKE_PROJECT}"
fi
CTX="$(kubectl config current-context)"
log "using kube context: $CTX"

# --- 2. images ---
if [ -n "${REGISTRY:-}" ]; then
  RUNTIME_IMG="$REGISTRY/runtime:$RUNTIME_TAG"
  CLAUDE_IMG="$REGISTRY/claude-code-runner:$RUNTIME_TAG"
  log "cross-compiling wren-runtime (linux/amd64) and pushing images to $REGISTRY"
  GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o bin/wren-runtime-amd64 ./cmd/wren-runtime
  printf 'FROM gcr.io/distroless/static:nonroot\nCOPY bin/wren-runtime-amd64 /usr/local/bin/wren-runtime\nUSER 65532:65532\nENTRYPOINT ["/usr/local/bin/wren-runtime"]\n' > /tmp/wren-runtime.Dockerfile
  docker build --platform linux/amd64 -f /tmp/wren-runtime.Dockerfile -t "$RUNTIME_IMG" .
  docker build --platform linux/amd64 -f build/Dockerfile.claude-code -t "$CLAUDE_IMG" .
  docker push "$RUNTIME_IMG"; docker push "$CLAUDE_IMG"
elif [ -n "${KIND_CLUSTER:-}" ]; then
  need kind
  RUNTIME_IMG="wren/runtime:$RUNTIME_TAG"
  CLAUDE_IMG="wren/claude-code-runner:$RUNTIME_TAG"
  log "building images and loading into kind cluster $KIND_CLUSTER"
  docker build -f build/Dockerfile.runtime -t "$RUNTIME_IMG" .
  docker build -f build/Dockerfile.claude-code -t "$CLAUDE_IMG" .
  kind load docker-image "$RUNTIME_IMG" "$CLAUDE_IMG" --name "$KIND_CLUSTER"
else
  die "set REGISTRY (GKE) or KIND_CLUSTER (kind) so images are available to the cluster"
fi

# --- 3. install CRDs + RBAC ---
log "installing CRDs and RBAC"
kubectl apply -f config/crd/bases/
kubectl apply -k config/rbac >/dev/null 2>&1 || kubectl apply -f config/rbac/

# --- 4. namespace + credential secrets ---
log "creating namespace $WREN_NS and credential secrets"
kubectl create namespace "$WREN_NS" --dry-run=client -o yaml | kubectl apply -f -
kubectl -n "$WREN_NS" create secret generic wren-github-token \
  --from-literal=token="$GITHUB_TOKEN" --dry-run=client -o yaml | kubectl apply -f -
if [ -n "${ANTHROPIC_API_KEY:-}" ]; then
  kubectl -n "$WREN_NS" create secret generic wren-anthropic-key \
    --from-literal=key="$ANTHROPIC_API_KEY" --dry-run=client -o yaml | kubectl apply -f -
fi

# --- 5. next steps ---
cat <<EOF

$(log "setup complete")
  context:      $CTX
  namespace:    $WREN_NS
  runtime img:  $RUNTIME_IMG
  harness img:  $CLAUDE_IMG   (set as a project's harnessImage)

Run the control plane (Phase 1: locally against the cluster):
  go run ./cmd/wren-operator   --leader-elect=false --runtime-image="$RUNTIME_IMG"
  go run ./cmd/wren-apiserver  --addr :8090

Then:
  wren login --control-plane localhost:8090 --user you
  # register a project with repo + harnessImage="$CLAUDE_IMG", then:
  wren run create --project <name> --task "..."

(In-cluster operator/apiserver Deployments are the Phase 2 handover; see SETUP.md.)
EOF
