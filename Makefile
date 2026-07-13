BINARY := wren
PKG := github.com/summiteight/wren
VERSION ?= dev
COMMIT := $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS := -X $(PKG)/internal/cli.Version=$(VERSION) \
           -X $(PKG)/internal/cli.Commit=$(COMMIT) \
           -X $(PKG)/internal/cli.Date=$(DATE)

CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@latest

.PHONY: build build-operator generate manifests deploy-manifests test vet fmt tidy clean

build: ## Build the wren CLI into ./bin
	go build -ldflags "$(LDFLAGS)" -o bin/$(BINARY) ./cmd/wren

build-operator: ## Build the wren-operator into ./bin
	go build -o bin/wren-operator ./cmd/wren-operator

build-apiserver: ## Build the wren-apiserver (control plane) into ./bin
	go build -o bin/wren-apiserver ./cmd/wren-apiserver

build-runtime: ## Build the wren-runtime (in-pod harness/sidecars) into ./bin
	go build -o bin/wren-runtime ./cmd/wren-runtime

RUNTIME_IMAGE ?= wren/runtime:dev
docker-runtime: ## Build the wren-runtime container image
	docker build -f build/Dockerfile.runtime -t $(RUNTIME_IMAGE) .

kind-load: docker-runtime ## Load the runtime image into a kind cluster (KIND_CLUSTER)
	kind load docker-image $(RUNTIME_IMAGE) --name $(or $(KIND_CLUSTER),wren-test)

cover: ## Run tests and print per-package coverage
	go test -cover ./...

generate: ## Regenerate DeepCopy methods
	$(CONTROLLER_GEN) object paths=./api/...

manifests: ## Regenerate CRD and RBAC YAML
	$(CONTROLLER_GEN) crd paths=./api/... output:crd:artifacts:config=config/crd/bases
	$(CONTROLLER_GEN) rbac:roleName=wren-operator-role paths=./internal/controller/... output:rbac:artifacts:config=config/rbac

deploy-manifests: ## Render the full deployment (CRDs + RBAC + manager)
	kubectl kustomize config/default

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
