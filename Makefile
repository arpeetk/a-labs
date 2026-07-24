BINARY := wren
PKG := github.com/summiteight/wren
VERSION ?= dev
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X $(PKG)/internal/cli.Version=$(VERSION) \
           -X $(PKG)/internal/cli.Commit=$(COMMIT) \
           -X $(PKG)/internal/cli.Date=$(DATE)

CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@latest

.PHONY: build build-operator generate manifests deploy deploy-manifests assets check-assets e2e e2e-gke docker-push-gke test vet fmt tidy clean

build: ## Build the wren CLI into ./bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/wren

build-operator: ## Build the wren-operator into ./bin
	go build -o bin/wren-operator ./cmd/wren-operator

build-apiserver: ## Build the wren-apiserver (control plane) into ./bin
	go build -o bin/wren-apiserver ./cmd/wren-apiserver

build-runtime: ## Build the wren-runtime (in-pod harness/sidecars) into ./bin
	go build -o bin/wren-runtime ./cmd/wren-runtime

RUNTIME_IMAGE   ?= wren/runtime:dev
OPERATOR_IMAGE  ?= wren/operator:dev
APISERVER_IMAGE ?= wren/apiserver:dev
KIND_CLUSTER    ?= wren-test

# GKE image vars — override via env or make flag. GKE_AR derives from
# GKE_PROJECT and GKE_TAG defaults to the current commit, so
# 'make docker-push-gke && make e2e-gke' agree at default settings
# (hack/e2e-gke.sh consumes the same GKE_PROJECT/GKE_AR/GKE_TAG variables).
GKE_PROJECT     ?= wren-gke-fdea81
GKE_AR          ?= us-central1-docker.pkg.dev/$(GKE_PROJECT)/wren
GKE_TAG         ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)

docker-runtime: ## Build the wren-runtime container image
	docker build -f build/Dockerfile.runtime -t $(RUNTIME_IMAGE) .

docker-operator: ## Build the wren-operator container image
	docker build --build-arg BIN=wren-operator -f build/Dockerfile.gobin -t $(OPERATOR_IMAGE) .

docker-apiserver: ## Build the wren-apiserver container image
	docker build --build-arg BIN=wren-apiserver -f build/Dockerfile.gobin -t $(APISERVER_IMAGE) .

docker-images: docker-runtime docker-operator docker-apiserver ## Build all container images

kind-load: docker-images ## Build + load all images into a kind cluster (KIND_CLUSTER)
	kind load docker-image $(RUNTIME_IMAGE) $(OPERATOR_IMAGE) $(APISERVER_IMAGE) --name $(KIND_CLUSTER)

# Build linux/amd64 images (required for GKE Standard x86 nodes) and push to AR.
# Usage: make docker-push-gke [GKE_PROJECT=…] [GKE_AR=…] [GKE_TAG=…]
docker-push-gke: ## Build linux/amd64 images and push to Artifact Registry (GKE_AR/GKE_TAG overridable)
	docker build --platform linux/amd64 -f build/Dockerfile.runtime \
	  -t $(GKE_AR)/runtime:$(GKE_TAG) . && docker push $(GKE_AR)/runtime:$(GKE_TAG)
	docker build --platform linux/amd64 --build-arg BIN=wren-operator -f build/Dockerfile.gobin \
	  -t $(GKE_AR)/operator:$(GKE_TAG) . && docker push $(GKE_AR)/operator:$(GKE_TAG)
	docker build --platform linux/amd64 --build-arg BIN=wren-apiserver -f build/Dockerfile.gobin \
	  -t $(GKE_AR)/apiserver:$(GKE_TAG) . && docker push $(GKE_AR)/apiserver:$(GKE_TAG)

deploy: ## Install CRDs + RBAC + operator + apiserver in-cluster (current kube context)
	kubectl apply -k config/default

e2e: ## Keyless end-to-end test on kind (the WS-0 merge gate); E2E_KEEP=1 keeps the cluster
	./hack/e2e.sh

e2e-gke: ## Egress-enforcement e2e on a GKE Standard cluster (existing cluster; push images first with docker-push-gke)
	./hack/e2e-gke.sh

cover: ## Run tests and print per-package coverage
	go test -cover ./...

generate: ## Regenerate DeepCopy methods
	$(CONTROLLER_GEN) object paths=./api/...

manifests: ## Regenerate CRD and RBAC YAML
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=wren-operator-role paths=./internal/controller/... output:rbac:artifacts:config=config/rbac

deploy-manifests: ## Render the full deployment (CRDs + RBAC + manager)
	kubectl kustomize config/default

# `wren install` applies the deployment from an embedded asset (go:embed) so it
# works without a repo checkout or a kustomize binary. config/ stays the source
# of truth: regenerate after changing config/, and let check-assets guard drift
# (wired into CI).
assets: ## Render config/default into the embedded install asset
	kubectl kustomize config/default > internal/install/assets/manifests.yaml

check-assets: ## Fail if the embedded install asset is stale vs config/ (run 'make assets')
	@kubectl kustomize config/default | diff - internal/install/assets/manifests.yaml >/dev/null \
	  || { echo "internal/install/assets/manifests.yaml is stale — run 'make assets'"; exit 1; }

test: ## Run tests
	go test ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format all Go source
	gofmt -w .

tidy: ## Reconcile go.mod / go.sum
	go mod tidy

clean: ## Remove build artifacts
	rm -rf bin
