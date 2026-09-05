# LimitPlane — build, test, and run it on Kubernetes.
#
#   make help          list every target
#   make check         what CI runs: fmt, vet, zero-dep gate, race tests
#   make kind-demo     a whole cluster, from nothing, in one command

SHELL := /bin/bash
.DEFAULT_GOAL := help

VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
IMAGE   ?= limitplane
TAG     ?= dev
REGISTRY ?= ghcr.io/mightbeanshuu

KIND_CLUSTER ?= limitplane
NAMESPACE    ?= limitplane

LDFLAGS := -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT)

# The Dockerfile uses --platform=$$BUILDPLATFORM and TARGETOS/TARGETARCH for
# cross-compilation, which the LEGACY docker builder cannot parse — it fails
# with `"" is an invalid OS component`. BuildKit understands them, and setting
# this is enough: it needs no buildx CLI plugin for a single-architecture
# build. Multi-arch (docker-buildx below) does need the plugin.
export DOCKER_BUILDKIT := 1

##@ General

.PHONY: help
help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
		/^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2 } \
		/^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)

##@ Development

.PHONY: run
run: ## Run the gateway locally (in-memory buckets, no Kubernetes)
	go run ./cmd/gateway

.PHONY: fmt
fmt: ## Format every Go file
	gofmt -w ./cmd ./internal ./web

.PHONY: vet
vet: ## go vet
	go vet ./...

.PHONY: test
test: ## Unit and integration tests with the race detector
	go test -race -count=1 ./...

.PHONY: cover
cover: ## Test with a coverage report
	go test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -1

.PHONY: bench
bench: ## Limiter throughput benchmarks
	go test -bench=. -benchmem ./internal/limiter/

.PHONY: zero-deps
zero-deps: ## Assert the project still has no third-party dependencies
	@if grep -q '^require' go.mod; then \
		echo "go.mod gained a dependency — LimitPlane is stdlib-only by design."; \
		cat go.mod; exit 1; \
	fi
	@test ! -s go.sum || { echo "go.sum is non-empty"; exit 1; }
	@echo "stdlib only ✓"

.PHONY: check
check: fmt vet zero-deps test ## Everything CI runs

##@ Container

.PHONY: docker-build
docker-build: ## Build the image for this machine's architecture
	docker build \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(IMAGE):$(TAG) .
	@docker images $(IMAGE):$(TAG) --format '  built {{.Repository}}:{{.Tag}}  {{.Size}}'

.PHONY: docker-buildx
docker-buildx: ## Build and push a multi-architecture image
	docker buildx build \
		--platform linux/amd64,linux/arm64 \
		--build-arg VERSION=$(VERSION) \
		--build-arg COMMIT=$(COMMIT) \
		-t $(REGISTRY)/$(IMAGE):$(VERSION) \
		-t $(REGISTRY)/$(IMAGE):latest \
		--push .

.PHONY: docker-run
docker-run: docker-build ## Run the image on its own
	docker run --rm -p 3000:3000 -e JWT_SECRET=local-dev $(IMAGE):$(TAG)

.PHONY: compose-up
compose-up: ## Three replicas behind a load balancer, sharing one Redis
	docker compose up --build -d
	@echo "  gateway     http://localhost:8080"
	@echo "  dashboard   http://localhost:8080/dashboard"
	@echo "  proof       make compose-proof"

.PHONY: compose-proof
compose-proof: ## Prove three replicas share ONE budget, not three
	@./scripts/prove-shared-limit.sh

.PHONY: compose-down
compose-down: ## Tear the compose stack down
	docker compose down -v

##@ Kubernetes

.PHONY: kind-up
kind-up: ## Create the local kind cluster
	@kind get clusters 2>/dev/null | grep -qx $(KIND_CLUSTER) \
		|| kind create cluster --name $(KIND_CLUSTER) --config deploy/kind/cluster.yaml
	kubectl cluster-info --context kind-$(KIND_CLUSTER)

.PHONY: kind-load
kind-load: docker-build ## Build and side-load the image into kind
	# kind nodes have their own container runtime, so an image built on the
	# host is invisible to them. Without this step every pod sits in
	# ErrImagePull trying to reach a registry that has never heard of it.
	kind load docker-image $(IMAGE):$(TAG) --name $(KIND_CLUSTER)

.PHONY: crds
crds: ## Install the CRDs and wait for them to be established
	kubectl apply -f deploy/k8s/crds/
	# `kubectl apply` returns as soon as the object is stored, NOT when the API
	# server is serving the new endpoint. Creating a custom resource in the
	# next breath fails with "no matches for kind" — a race that shows up on
	# fast clusters and in CI far more often than on a laptop.
	kubectl wait --for=condition=Established --timeout=60s \
		crd/ratelimitpolicies.limitplane.dev crd/tenants.limitplane.dev

.PHONY: k8s-install
k8s-install: crds ## Apply the dev overlay to the current cluster
	kubectl apply -k deploy/k8s/overlays/dev
	kubectl -n $(NAMESPACE) rollout status deploy/limitplane --timeout=180s

.PHONY: k8s-samples
k8s-samples: ## Apply the sample policy and tenants
	kubectl -n $(NAMESPACE) create secret generic acme-api-key \
		--from-literal=apiKey=lp_acme_demo_key \
		--dry-run=client -o yaml | kubectl apply -f -
	kubectl apply -f deploy/k8s/samples/

.PHONY: k8s-uninstall
k8s-uninstall: ## Remove everything except the CRDs
	-kubectl delete -k deploy/k8s/overlays/dev

.PHONY: kind-demo
kind-demo: ## Full demo: cluster, image, operator, live traffic, proof
	@./scripts/kind-demo.sh

.PHONY: kind-down
kind-down: ## Delete the kind cluster
	kind delete cluster --name $(KIND_CLUSTER)

.PHONY: k8s-status
k8s-status: ## What is running, and what the operator thinks
	@echo "── pods ──"        && kubectl -n $(NAMESPACE) get pods -o wide
	@echo "── policies ──"    && kubectl -n $(NAMESPACE) get ratelimitpolicies
	@echo "── tenants ──"     && kubectl -n $(NAMESPACE) get tenants
	@echo "── leader ──"      && kubectl -n $(NAMESPACE) get lease limitplane-operator \
		-o jsonpath='{.spec.holderIdentity}{"\n"}' 2>/dev/null || echo "(no lease yet)"

.PHONY: k8s-logs
k8s-logs: ## Follow the gateway logs
	kubectl -n $(NAMESPACE) logs -l app.kubernetes.io/component=gateway --tail=100 -f

.PHONY: port-forward
port-forward: ## Expose the gateway on localhost:3000
	kubectl -n $(NAMESPACE) port-forward svc/limitplane 3000:80

##@ Helm

.PHONY: helm-lint
helm-lint: ## Lint and render the chart
	helm lint deploy/helm/limitplane
	helm template lp deploy/helm/limitplane >/dev/null && echo "template renders ✓"

.PHONY: helm-install
helm-install: ## Install the chart into the current cluster
	helm upgrade --install limitplane deploy/helm/limitplane \
		--namespace $(NAMESPACE) --create-namespace \
		--set image.repository=$(IMAGE) --set image.tag=$(TAG) \
		--set image.pullPolicy=Never \
		--set replicaCount=1 --set autoscaling.enabled=false \
		--set podDisruptionBudget.enabled=false \
		--wait --timeout 3m

.PHONY: helm-uninstall
helm-uninstall: ## Remove the Helm release
	helm uninstall limitplane -n $(NAMESPACE)

##@ Validation

.PHONY: manifests
manifests: ## Render every kustomize overlay
	@for o in base overlays/dev overlays/prod; do \
		printf '── %-14s ' "$$o"; \
		kubectl kustomize deploy/k8s/$$o >/dev/null && echo "ok"; \
	done

.PHONY: validate
validate: manifests helm-lint ## Validate all deployment manifests
	@echo "manifests ✓"
