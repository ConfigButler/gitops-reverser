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
	@rm -f config/crd/bases/*.yaml
	$(CONTROLLER_GEN) rbac:roleName=gitops-reverser crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

.PHONY: helm-sync
helm-sync: ## Sync CRDs and roles from config/crd/bases to Helm chart crds directory (for packaging)
	@rm -f charts/gitops-reverser/crds/*.yaml
	@cp config/crd/bases/*.yaml charts/gitops-reverser/crds/
	@rm -f charts/gitops-reverser/config/*.yaml
	@cp config/rbac/cluster-role.yaml charts/gitops-reverser/config/

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
E2E_LOCAL_IMAGE ?= gitops-reverser:e2e-local
CERT_MANAGER_WAIT_TIMEOUT ?= 600s
CERT_MANAGER_VERSION ?= v1.19.1
CERT_MANAGER_MANIFEST_URL ?= https://github.com/cert-manager/cert-manager/releases/download/$(CERT_MANAGER_VERSION)/cert-manager.yaml

.PHONY: setup-cluster
setup-cluster: ## Set up a Kind cluster for e2e tests if it does not exist
	@if ! command -v $(KIND) >/dev/null 2>&1; then \
		echo "Kind is not installed - skipping Makefile cluster creation (expected in CI runs since we use helm/kind-action)"; \
	else \
		KIND_CLUSTER=$(KIND_CLUSTER) bash test/e2e/kind/start-cluster.sh; \
	fi

.PHONY: cleanup-cluster
cleanup-cluster: ## Tear down the Kind cluster used for e2e tests
	@if $(KIND) get clusters 2>/dev/null | grep -q "^$(KIND_CLUSTER)$$"; then \
		echo "🧹 Deleting Kind cluster '$(KIND_CLUSTER)'"; \
		$(KIND) delete cluster --name $(KIND_CLUSTER); \
	else \
		echo "ℹ️ Kind cluster '$(KIND_CLUSTER)' does not exist; skipping cleanup"; \
	fi

.PHONY: e2e-build-load-image
e2e-build-load-image: ## Build local image and load it into the Kind cluster used by local e2e flows
	@if [ -n "$(PROJECT_IMAGE)" ]; then \
		echo "🐳 Building local image $(PROJECT_IMAGE)"; \
		$(CONTAINER_TOOL) build -t $(PROJECT_IMAGE) .; \
		echo "📦 Loading image $(PROJECT_IMAGE) into Kind cluster $(KIND_CLUSTER)"; \
		$(KIND) load docker-image $(PROJECT_IMAGE) --name $(KIND_CLUSTER); \
	else \
		echo "🐳 Building local image $(E2E_LOCAL_IMAGE)"; \
		$(CONTAINER_TOOL) build -t $(E2E_LOCAL_IMAGE) .; \
		echo "📦 Loading image $(E2E_LOCAL_IMAGE) into Kind cluster $(KIND_CLUSTER)"; \
		$(KIND) load docker-image $(E2E_LOCAL_IMAGE) --name $(KIND_CLUSTER); \
	fi

.PHONY: test-e2e
test-e2e: setup-cluster cleanup-webhook setup-e2e check-cert-manager manifests setup-port-forwards ## Run end-to-end tests in Kind cluster, note that vet, fmt and generate are not run!
	@echo "ℹ️ test-e2e reuses the existing Kind cluster (no cluster cleanup in this target)"; \
	if [ -n "$(PROJECT_IMAGE)" ]; then \
		echo "ℹ️ Entry point selected pre-built image (CI-friendly): $(PROJECT_IMAGE)"; \
		echo "ℹ️ Skipping local image build/load for pre-built image path"; \
		KIND_CLUSTER=$(KIND_CLUSTER) PROJECT_IMAGE="$(PROJECT_IMAGE)" go test ./test/e2e/ -v -ginkgo.v; \
	else \
		echo "ℹ️ Entry point selected local fallback image: $(E2E_LOCAL_IMAGE)"; \
		echo "ℹ️ Building/loading local image into existing cluster"; \
		$(MAKE) e2e-build-load-image KIND_CLUSTER=$(KIND_CLUSTER); \
		KIND_CLUSTER=$(KIND_CLUSTER) PROJECT_IMAGE="$(E2E_LOCAL_IMAGE)" go test ./test/e2e/ -v -ginkgo.v; \
	fi

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
build-installer: manifests helm-sync ## Generate a consolidated YAML from Helm chart for easy installation.
	@echo "📦 Generating install.yaml from Helm chart..."
	@mkdir -p dist
	@$(HELM) template gitops-reverser charts/gitops-reverser \
		--namespace gitops-reverser \
		--set labels.managedBy=kubectl \
		--set createNamespace=true \
		--include-crds > dist/install.yaml
	@echo "✅ Generated dist/install.yaml ($(shell wc -l < dist/install.yaml) lines)"

##@ Deployment

.PHONY: install
install: manifests ## Install CRDs into the K8s cluster specified in ~/.kube/config.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) apply -f -

.PHONY: uninstall
uninstall: manifests ## Uninstall CRDs from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config/crd | $(KUBECTL) delete --ignore-not-found=true -f -

.PHONY: deploy
deploy: manifests ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	cd config && $(KUSTOMIZE) edit set image gitops-reverser=${IMG}
	$(KUSTOMIZE) build config | $(KUBECTL) apply -f -

.PHONY: undeploy
undeploy: ## Undeploy controller from the K8s cluster specified in ~/.kube/config. Call with ignore-not-found=true to ignore resource not found errors during deletion.
	$(KUSTOMIZE) build config | $(KUBECTL) delete --ignore-not-found=true -f -

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
GITEA_CHART_VERSION ?= 12.5.0	# https://gitea.com/gitea/helm-gitea

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
	@echo "🚀 Setting up cert-manager..."
	@$(KUBECTL) apply -f $(CERT_MANAGER_MANIFEST_URL) | grep -v "unchanged"

.PHONY: setup-port-forwards
setup-port-forwards: ## Start all port-forwards in background
	@bash test/e2e/scripts/setup-port-forwards.sh

.PHONY: cleanup-port-forwards
cleanup-port-forwards: ## Stop all port-forwards
	@echo "🛑 Stopping port-forwards..."
	@-pkill -f "kubectl.*port-forward.*13000" 2>/dev/null || true
	@-pkill -f "kubectl.*port-forward.*19090" 2>/dev/null || true
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

.PHONY: wait-cert-manager
wait-cert-manager: ## Wait for cert-manager pods to become ready
	@echo "⏳ Waiting for cert-manager deployments to become available (timeout=$(CERT_MANAGER_WAIT_TIMEOUT))..."
	@set -e; \
	for dep in cert-manager cert-manager-webhook cert-manager-cainjector; do \
		echo "   - waiting for deployment/$$dep"; \
		if ! $(KUBECTL) -n cert-manager rollout status deploy/$$dep --timeout=$(CERT_MANAGER_WAIT_TIMEOUT); then \
			echo "❌ Timed out waiting for cert-manager readiness (deployment=$$dep)"; \
			echo "📋 cert-manager deployments and pods:"; \
			$(KUBECTL) -n cert-manager get deploy,pod -o wide || true; \
			echo "📋 recent cert-manager events:"; \
			$(KUBECTL) -n cert-manager get events --sort-by=.metadata.creationTimestamp | tail -n 80 || true; \
			echo "📋 recent cert-manager logs:"; \
			$(KUBECTL) -n cert-manager logs -l app.kubernetes.io/instance=cert-manager --all-containers=true --tail=120 || true; \
			exit 1; \
		fi; \
	done
	@echo "✅ cert-manager is ready"

.PHONY: check-cert-manager
check-cert-manager: wait-cert-manager ## Explicit readiness check for cert-manager
	@echo "✅ cert-manager check passed"

## Smoke test: install from local Helm chart and validate first quickstart flow
.PHONY: test-e2e-quickstart
test-e2e-quickstart: ## Install + quickstart smoke with E2E_INSTALL_MODE=helm|manifest
	@MODE="$(E2E_INSTALL_MODE)"; \
	if [ "$$MODE" != "helm" ] && [ "$$MODE" != "manifest" ]; then \
		echo "❌ Invalid E2E_INSTALL_MODE='$$MODE' (expected: helm|manifest)"; \
		exit 1; \
	fi; \
	PROJECT_IMAGE_VALUE="$(PROJECT_IMAGE)"; \
	if [ -n "$$PROJECT_IMAGE_VALUE" ]; then \
		echo "ℹ️ Entry point selected pre-built image (probably running in CI): $$PROJECT_IMAGE_VALUE"; \
		echo "ℹ️ Skipping cluster cleanup for pre-built image path"; \
		KIND_CLUSTER=$(KIND_CLUSTER) $(MAKE) setup-cluster setup-e2e check-cert-manager; \
	else \
		PROJECT_IMAGE_VALUE="$(E2E_LOCAL_IMAGE)"; \
		echo "🧹 Local fallback path: cleaning cluster to test a clean install"; \
		KIND_CLUSTER=$(KIND_CLUSTER) $(MAKE) cleanup-cluster; \
		echo "ℹ️ Entry point selected local fallback image: $$PROJECT_IMAGE_VALUE"; \
		KIND_CLUSTER=$(KIND_CLUSTER) PROJECT_IMAGE="$$PROJECT_IMAGE_VALUE" $(MAKE) setup-cluster setup-e2e check-cert-manager e2e-build-load-image; \
	fi; \
	echo "ℹ️ Running install quickstart smoke mode: $$MODE"; \
	PROJECT_IMAGE="$$PROJECT_IMAGE_VALUE" bash test/e2e/scripts/run-quickstart.sh "$$MODE"; \

## Smoke test: install from local Helm chart and validate first quickstart flow
.PHONY: test-e2e-quickstart-helm
test-e2e-quickstart-helm:
	@$(MAKE) test-e2e-quickstart E2E_INSTALL_MODE=helm PROJECT_IMAGE="$(PROJECT_IMAGE)" KIND_CLUSTER="$(KIND_CLUSTER)"

## Smoke test: install from generated dist/install.yaml and validate first quickstart flow
.PHONY: test-e2e-quickstart-manifest
test-e2e-quickstart-manifest:
	@if [ -n "$(PROJECT_IMAGE)" ]; then \
		echo "ℹ️ test-e2e-quickstart-manifest using existing artifact (PROJECT_IMAGE set, CI/pre-built path)"; \
	else \
		echo "ℹ️ test-e2e-quickstart-manifest local path: regenerating dist/install.yaml via build-installer"; \
		$(MAKE) build-installer; \
	fi
	@$(MAKE) test-e2e-quickstart E2E_INSTALL_MODE=manifest PROJECT_IMAGE="$(PROJECT_IMAGE)" KIND_CLUSTER="$(KIND_CLUSTER)"
