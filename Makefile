# Image URL to use all building/pushing image targets
IMG ?= controller:latest

# Get the currently used golang install path (in GOPATH/bin, unless GOBIN is set)
ifeq (,$(shell go env GOBIN))
GOBIN=$(shell go env GOPATH)/bin
else
GOBIN=$(shell go env GOBIN)
endif

# CONTAINER_TOOL defines the container tool to be used for building images.
# Be aware that the target commands are only tested with Docker which is
# scaffolded by default. However, you might want to replace it to use other
# tools. (i.e. podman)
CONTAINER_TOOL ?= docker

# Setting SHELL to bash allows bash commands to be executed by recipes.
# Options are set to exit when a recipe line exits non-zero or a piped command fails.
SHELL = /usr/bin/env bash -o pipefail
.SHELLFLAGS = -ec

.PHONY: all
all: build

##@ General

# The help target prints out all targets with their descriptions organized
# beneath their categories. The categories are represented by '##@' and the
# target descriptions by '##'. The awk command is responsible for reading the
# entire set of makefiles included in this invocation, looking for lines of the
# file as xyz: ## something, and then pretty-format the target and help. Then,
# if there's a line with ##@ something, that gets pretty-printed as a category.
# More info on the usage of ANSI control characters for terminal formatting:
# https://en.wikipedia.org/wiki/ANSI_escape_code#SGR_parameters
# More info on the awk command:
# http://linuxcommand.org/lc3_adv_awk.php

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

##@ Development

.PHONY: manifests
manifests: ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
	$(CONTROLLER_GEN) rbac:roleName=manager-role crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: helm-sync-crds
helm-sync-crds: ## Sync CRDs from config/crd/bases to Helm chart crds directory (for packaging)
	@mkdir -p charts/gitops-reverser/crds
	@cp config/crd/bases/*.yaml charts/gitops-reverser/crds/

.PHONY: generate
generate: ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(shell pwd)/bin -p path)" go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

KIND_CLUSTER ?= gitops-reverser-test-e2e

.PHONY: setup-cluster
setup-cluster: ## Set up a Kind cluster for e2e tests if it does not exist
	@if ! command -v $(KIND) >/dev/null 2>&1; then \
		echo "Kind is not installed - skipping Makefile cluster creation (expected in CI runs since we use helm/kind-action)"; \
	else \
		case "$$($(KIND) get clusters)" in \
			*"$(KIND_CLUSTER)"*) \
				echo "✅ Cluster '$(KIND_CLUSTER)' already exists. Skipping creation." ;; \
			*) \
				echo "🚀 Creating cluster (with Kind) '$(KIND_CLUSTER)'..."; \
				$(KIND) create cluster --name $(KIND_CLUSTER) --wait 5m; \
				echo "✅ Kind cluster created successfully" ;; \
		esac; \
		echo "📋 Configuring kubeconfig for cluster '$(KIND_CLUSTER)'..."; \
		$(KIND) export kubeconfig --name $(KIND_CLUSTER); \
	fi

.PHONY: cleanup-cluster
cleanup-cluster: ## Tear down the Kind cluster used for e2e tests
	@$(KIND) delete cluster --name $(KIND_CLUSTER)

.PHONY: test-e2e
test-e2e: setup-cluster cleanup-webhook setup-e2e manifests setup-port-forwards ## Run end-to-end tests in Kind cluster, note that vet, fmt and generate are not run!
	KIND_CLUSTER=$(KIND_CLUSTER) go test ./test/e2e/ -v -ginkgo.v

.PHONY: cleanup-webhook
cleanup-webhook: ## Preventive cleanup of ValidatingWebhookConfiguration potenially left by previous test runs
	$(KUBECTL) delete ValidatingWebhookConfiguration gitops-reverser-validating-webhook-configuration --ignore-not-found=true

.PHONY: lint
lint: ## Run golangci-lint linter
	$(GOLANGCI_LINT) run

.PHONY: lint-fix
lint-fix: ## Run golangci-lint linter and perform fixes
	$(GOLANGCI_LINT) run --fix

.PHONY: lint-config
lint-config: ## Verify golangci-lint linter configuration
	$(GOLANGCI_LINT) config verify

##@ Build

.PHONY: build
build: manifests generate fmt vet ## Build manager binary.
	go build -o bin/manager cmd/main.go

.PHONY: run
run: manifests generate fmt vet ## Run a controller from your host.
	go run ./cmd/main.go

# If you wish to build the manager image targeting other platforms you can use the --platform flag.
# (i.e. docker build --platform linux/arm64). However, you must enable docker buildKit for it.
# More info: https://docs.docker.com/develop/develop-images/build_enhancements/
.PHONY: docker-build
docker-build: ## Build docker image with the manager.
	$(CONTAINER_TOOL) build -t ${IMG} .

.PHONY: docker-push
docker-push: ## Push docker image with the manager.
	$(CONTAINER_TOOL) push ${IMG}

# PLATFORMS defines the target platforms for the manager image be built to provide support to multiple
# architectures. (i.e. make docker-buildx IMG=myregistry/mypoperator:0.0.1). To use this option you need to:
# - be able to use docker buildx. More info: https://docs.docker.com/build/buildx/
# - have enabled BuildKit. More info: https://docs.docker.com/develop/develop-images/build_enhancements/
# - be able to push the image to your registry (i.e. if you do not set a valid value via IMG=<myregistry/image:<tag>> then the export will fail)
# To adequately provide solutions that are compatible with multiple platforms, you should consider using this option.
PLATFORMS ?= linux/arm64,linux/amd64,linux/s390x,linux/ppc64le
.PHONY: docker-buildx
docker-buildx: ## Build and push docker image for the manager for cross-platform support
	# copy existing Dockerfile and insert --platform=${BUILDPLATFORM} into Dockerfile.cross, and preserve the original Dockerfile
	sed -e '1 s/\(^FROM\)/FROM --platform=\$$\{BUILDPLATFORM\}/; t' -e ' 1,// s//FROM --platform=\$$\{BUILDPLATFORM\}/' Dockerfile > Dockerfile.cross
	- $(CONTAINER_TOOL) buildx create --name gitops-reverser-builder
	$(CONTAINER_TOOL) buildx use gitops-reverser-builder
	- $(CONTAINER_TOOL) buildx build --push --platform=$(PLATFORMS) --tag ${IMG} -f Dockerfile.cross .
	- $(CONTAINER_TOOL) buildx rm gitops-reverser-builder
	rm Dockerfile.cross

.PHONY: build-installer
build-installer: manifests generate ## Generate a consolidated YAML with CRDs and deployment.
	mkdir -p dist
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default > dist/install.yaml

##@ Deployment

.PHONY: install
install: manifests ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=true -f -

.PHONY: deploy
deploy: manifests ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config/manager && $(KUSTOMIZE) edit set image controller=${IMG}
	$(KUSTOMIZE) build config/default | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/default | $(KUBECTL) delete --ignore-not-found=true -f -

##@ Dependencies

## Tool Binaries - all pre-installed in devcontainer
KUBECTL ?= kubectl
KIND ?= kind
HELM ?= helm
KUSTOMIZE ?= kustomize
CONTROLLER_GEN ?= controller-gen
ENVTEST ?= setup-envtest
GOLANGCI_LINT ?= golangci-lint

## Tool Versions (for reference - versions defined in .devcontainer/Dockerfile)
ENVTEST_K8S_VERSION ?= $(shell go list -m -f "{{ .Version }}" k8s.io/api | awk -F'[v.]' '{printf "1.%d", $$3}')

# Gitea E2E Configuration
GITEA_NAMESPACE ?= gitea-e2e
GITEA_CHART_VERSION ?= 10.4.0	# https://gitea.com/gitea/helm-gitea

.PHONY: setup-envtest
setup-envtest: ## Setup envtest binaries for unit tests
	@echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	@mkdir -p $(shell pwd)/bin
	@$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(shell pwd)/bin -p path || { \
		echo "Error: Failed to set up envtest binaries for version $(ENVTEST_K8S_VERSION)."; \
		exit 1; \
	}

##@ E2E Test Infrastructure

.PHONY: setup-gitea-e2e
setup-gitea-e2e: ## Set up Gitea for e2e testing
	@echo "🚀 Setup Gitea for e2e testing..."
	@$(HELM) repo add gitea-charts https://dl.gitea.com/charts/ 2>/dev/null || true
	@$(HELM) repo update gitea-charts
	@$(KUBECTL) create namespace $(GITEA_NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	@$(HELM) upgrade --install gitea gitea-charts/gitea \
		--namespace $(GITEA_NAMESPACE) \
		--version $(GITEA_CHART_VERSION) \
		--values test/e2e/gitea-values.yaml

.PHONY: setup-cert-manager
setup-cert-manager:
	@echo "🚀 Setup cert-manager (no wait needed)"
	@$(KUBECTL) apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.18.2/cert-manager.yaml | grep -v "unchanged"

.PHONY: setup-port-forwards
setup-port-forwards: ## Start all port-forwards in background
	@bash test/e2e/scripts/setup-port-forwards.sh

.PHONY: cleanup-port-forwards
cleanup-port-forwards: ## Stop all port-forwards
	@echo "🛑 Stopping port-forwards..."
	@-pkill -f "kubectl.*port-forward.*3000" 2>/dev/null || true
	@-pkill -f "kubectl.*port-forward.*9090" 2>/dev/null || true
	@echo "✅ Port-forwards stopped"

.PHONY: cleanup-gitea-e2e
cleanup-gitea-e2e: cleanup-port-forwards ## Clean up Gitea e2e environment
	@echo "🧹 Cleaning up Gitea e2e environment..."
	@$(HELM) uninstall gitea --namespace $(GITEA_NAMESPACE) 2>/dev/null || true
	@$(KUBECTL) delete namespace $(GITEA_NAMESPACE) 2>/dev/null || true
	@echo "✅ Gitea cleanup completed"

.PHONY: setup-prometheus-e2e
setup-prometheus-e2e: ## Set up Prometheus for e2e metrics testing
	@echo "🚀 Setup Prometheus for e2e testing..."
	@bash test/e2e/scripts/setup-prometheus.sh

.PHONY: cleanup-prometheus-e2e
cleanup-prometheus-e2e: ## Clean up Prometheus e2e environment
	@echo "🧹 Cleaning up Prometheus e2e environment..."
	@$(KUBECTL) delete -f test/e2e/prometheus/deployment.yaml --ignore-not-found=true
	@$(KUBECTL) delete -f test/e2e/prometheus/rbac.yaml --ignore-not-found=true
	@$(KUBECTL) delete namespace prometheus-e2e --ignore-not-found=true
	@echo "✅ Prometheus cleanup completed"

.PHONY: setup-e2e
setup-e2e: setup-cert-manager setup-gitea-e2e setup-prometheus-e2e ## Setup all e2e test infrastructure
	@echo "✅ E2E infrastructure initialized"
