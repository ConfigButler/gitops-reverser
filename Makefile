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

CRD_BASE_OUTPUTS := \
	config/crd/bases/configbutler.ai_clusterwatchrules.yaml \
	config/crd/bases/configbutler.ai_gitproviders.yaml \
	config/crd/bases/configbutler.ai_gittargets.yaml \
	config/crd/bases/configbutler.ai_watchrules.yaml
MANIFEST_OUTPUTS := $(CRD_BASE_OUTPUTS) \
	config/rbac/role.yaml \
	config/webhook/manifests.yaml
MANIFEST_INPUTS := $(shell find api internal cmd -type f -name '*.go' \
	! -name '*_test.go' ! -name 'zz_generated.deepcopy.go')
.PHONY: manifests
manifests: $(MANIFEST_OUTPUTS) ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
$(MANIFEST_OUTPUTS) &: $(MANIFEST_INPUTS)
	@rm -f config/crd/bases/*.yaml \
		config/rbac/role.yaml \
		config/webhook/manifests.yaml
	$(CONTROLLER_GEN) rbac:roleName=gitops-reverser crd webhook paths="./..." output:crd:artifacts:config=config/crd/bases

HELM_CRD_OUTPUTS := $(patsubst config/crd/bases/%,charts/gitops-reverser/crds/%,$(CRD_BASE_OUTPUTS))
HELM_SYNC_OUTPUTS := $(HELM_CRD_OUTPUTS) \
	charts/gitops-reverser/config/role.yaml
.PHONY: helm-sync
helm-sync: $(HELM_SYNC_OUTPUTS) ## Sync CRDs and roles from config/crd/bases to Helm chart crds directory (for packaging)
$(HELM_SYNC_OUTPUTS) &: $(MANIFEST_OUTPUTS)
	@mkdir -p charts/gitops-reverser/crds charts/gitops-reverser/config
	@rm -f charts/gitops-reverser/crds/*.yaml charts/gitops-reverser/config/*.yaml
	@cp config/crd/bases/*.yaml charts/gitops-reverser/crds/
	@cp config/rbac/role.yaml charts/gitops-reverser/config/role.yaml
GENERATE_INPUTS := $(shell find api -type f -name '*.go' ! -name '*_test.go' \
	! -name 'zz_generated.deepcopy.go') hack/boilerplate.go.txt
GENERATE_OUTPUT := api/v1alpha1/zz_generated.deepcopy.go

generate: $(GENERATE_OUTPUT) ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.

$(GENERATE_OUTPUT): $(GENERATE_INPUTS)
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

# Kind cluster names per E2E test type (kept separate to avoid cross-test contamination).
KIND_CLUSTER_E2E ?= gitops-reverser-test-e2e
KIND_CLUSTER_QUICKSTART_HELM ?= gitops-reverser-test-e2e-quickstart-helm
KIND_CLUSTER_QUICKSTART_MANIFEST ?= gitops-reverser-test-e2e-quickstart-manifest
# KIND_CLUSTER is used by cleanup-cluster; defaults to the main e2e cluster.
KIND_CLUSTER ?= $(KIND_CLUSTER_E2E)
E2E_LOCAL_IMAGE ?= gitops-reverser:e2e-local
# In CI, PROJECT_IMAGE is the pre-built image from the docker-build job; locally we build E2E_LOCAL_IMAGE.
E2E_IMAGE := $(if $(PROJECT_IMAGE),$(PROJECT_IMAGE),$(E2E_LOCAL_IMAGE))
E2E_AGE_KEY_FILE ?= /tmp/e2e-age-key.txt

# CTX: kubeconfig context for the cluster being operated on.
# Defaults to the main e2e cluster; override with CTX=kind-<name> to reuse stamp targets for other clusters.
CTX ?= kind-$(KIND_CLUSTER_E2E)
# Derive the Kind cluster name by stripping the "kind-" prefix from CTX.
CLUSTER_FROM_CTX = $(patsubst kind-%,%,$(CTX))
CS := .stamps/cluster/$(CTX)
IS := .stamps/image
GO_SOURCES := $(shell find cmd internal -type f -name '*.go') go.mod go.sum

KUBECONTEXT_QS_HELM := kind-$(KIND_CLUSTER_QUICKSTART_HELM)
KUBECONTEXT_QS_MANIFEST := kind-$(KIND_CLUSTER_QUICKSTART_MANIFEST)

.PHONY: cleanup-cluster
cleanup-cluster: ## Tear down the Kind cluster used for e2e tests and remove its stamps
	@if $(KIND) get clusters 2>/dev/null | grep -q "^$(KIND_CLUSTER)$$"; then \
		echo "🧹 Deleting Kind cluster '$(KIND_CLUSTER)'"; \
		$(KIND) delete cluster --name $(KIND_CLUSTER); \
	else \
		echo "ℹ️ Kind cluster '$(KIND_CLUSTER)' does not exist; skipping cleanup"; \
	fi
	rm -rf .stamps/cluster/kind-$(KIND_CLUSTER)

.PHONY: cleanup-e2e-clusters
cleanup-e2e-clusters: ## Tear down all E2E Kind clusters and their stamps
	@for cluster in "$(KIND_CLUSTER_E2E)" "$(KIND_CLUSTER_QUICKSTART_HELM)" "$(KIND_CLUSTER_QUICKSTART_MANIFEST)"; do \
		if $(KIND) get clusters 2>/dev/null | grep -q "^$$cluster$$"; then \
			echo "🧹 Deleting Kind cluster '$$cluster'"; \
			$(KIND) delete cluster --name "$$cluster"; \
		else \
			echo "ℹ️ Kind cluster '$$cluster' does not exist; skipping cleanup"; \
		fi; \
		rm -rf .stamps/cluster/kind-$$cluster; \
	done

cleanup-namespace-sut: $(CS)/sut.namespace.ready
	kubectl --context $(CTX) delete validatingwebhookconfiguration \
		gitops-reverser-validating-webhook-configuration --ignore-not-found=true

$(CS)/sut.namespace.ready: $(CS)/ready config/namespace.yaml
	mkdir -p $(CS)
	kubectl --context $(CTX) apply -f config/namespace.yaml
	kubectl --context $(CTX) label --overwrite ns sut pod-security.kubernetes.io/enforce=restricted
	touch $@

// This is called by the full e2e suite -> it should for now clean the sut namespace, recreate it and run the installer and build and install new image. Let's use dependecies as much as possible -> this code can't call the e2e golang stuff: this is what we will call from golang
$(CS)/$(NAMESPACE)/e2e/prepare:

// This is still what I would like to call! make test-e2e is the classic command that I'm used to
.PHONY: test-e2e
test-e2e: cleanup-namespace-sut

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

CHART_INPUTS := charts/gitops-reverser/Chart.yaml \
	charts/gitops-reverser/values.yaml \
	$(shell find charts/gitops-reverser/templates -type f)

dist/install.yaml: $(HELM_SYNC_OUTPUTS) $(CHART_INPUTS) ## Generate consolidated YAML from Helm chart.
	@mkdir -p dist
	@$(HELM) template $(INSTALL_NAME) charts/gitops-reverser \
		--namespace $(NAMESPACE) \
		--set labels.managedBy=kubectl \
		--set createNamespace=true \
		--include-crds > $@

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

##@ E2E Stamp Targets (cluster-parameterized; pass CTX=kind-<name> to target a different cluster)

$(CS)/ready: test/e2e/kind/start-cluster.sh test/e2e/kind/cluster-template.yaml
	mkdir -p $(CS)
	KIND_CLUSTER=$(CLUSTER_FROM_CTX) bash test/e2e/kind/start-cluster.sh
	kubectl --context $(CTX) get ns >/dev/null
	touch $@

CERT_MANAGER_WAIT_TIMEOUT ?= 600s
CERT_MANAGER_VERSION ?= v1.19.4
CERT_MANAGER_MANIFEST_URL ?= https://github.com/cert-manager/cert-manager/releases/download/$(CERT_MANAGER_VERSION)/cert-manager.yaml
$(CS)/cert-manager.installed: $(CS)/ready
	mkdir -p $(CS)
	kubectl --context $(CTX) apply -f $(CERT_MANAGER_MANIFEST_URL) | grep -v unchanged
	kubectl --context $(CTX) -n cert-manager rollout status deploy/cert-manager --timeout=$(CERT_MANAGER_WAIT_TIMEOUT)
	kubectl --context $(CTX) -n cert-manager rollout status deploy/cert-manager-webhook --timeout=$(CERT_MANAGER_WAIT_TIMEOUT)
	kubectl --context $(CTX) -n cert-manager rollout status deploy/cert-manager-cainjector --timeout=$(CERT_MANAGER_WAIT_TIMEOUT)
	touch $@

$(CS)/gitea.installed: $(CS)/ready test/e2e/gitea-values.yaml
	mkdir -p $(CS)
	$(HELM) repo add gitea-charts https://dl.gitea.com/charts/ 2>/dev/null || true
	$(HELM) repo update gitea-charts
	kubectl --context $(CTX) create namespace $(GITEA_NAMESPACE) --dry-run=client -o yaml \
	  | kubectl --context $(CTX) apply -f -
	$(HELM) --kube-context $(CTX) upgrade --install gitea gitea-charts/gitea \
	  --namespace $(GITEA_NAMESPACE) \
	  --version $(GITEA_CHART_VERSION) \
	  --values test/e2e/gitea-values.yaml
	kubectl --context $(CTX) -n $(GITEA_NAMESPACE) rollout status deploy/gitea --timeout=300s
	touch $@

PROMETHEUS_SETUP_MANIFESTS := $(shell find test/e2e/setup/prometheus -type f -name '*.yaml')
$(CS)/prometheus.installed: $(CS)/ready test/e2e/scripts/ensure-prometheus-operator.sh $(PROMETHEUS_SETUP_MANIFESTS)
	mkdir -p $(CS)
	KUBECONTEXT=$(CTX) bash test/e2e/scripts/ensure-prometheus-operator.sh
	kubectl --context $(CTX) apply -n prometheus-operator -f test/e2e/setup/prometheus
	kubectl --context $(CTX) wait --for=condition=Available prometheus/prometheus-shared-e2e -n prometheus-operator --timeout=180s
	kubectl --context $(CTX) wait --for=condition=ready pod -l prometheus=prometheus-shared-e2e -n prometheus-operator --timeout=180s
	touch $@

$(CS)/age-key.applied: $(CS)/sut.namespace.ready test/e2e/tools/gen-age-key/main.go
	mkdir -p $(CS)
	go run ./test/e2e/tools/gen-age-key \
	  --key-file $(CS)/age-key.txt \
	  --secret-file $(CS)/age-key-secret.yaml \
	  --namespace sut \
	  --secret-name sops-age-key
	kubectl --context $(CTX) apply -f $(CS)/age-key-secret.yaml
	touch $@

$(IS)/controller.id: $(GO_SOURCES) Dockerfile
	mkdir -p $(IS)
	$(CONTAINER_TOOL) build \
	  --build-arg TARGETOS=$(shell go env GOOS) \
	  --build-arg TARGETARCH=$(shell go env GOARCH) \
	  -t $(E2E_LOCAL_IMAGE) .
	$(CONTAINER_TOOL) inspect --format='{{.Id}}' $(E2E_LOCAL_IMAGE) > $@

$(CS)/crds.applied: $(CS)/ready $(shell find config/crd -type f)
	mkdir -p $(CS)
	kubectl --context $(CTX) apply -k config/crd
	kubectl --context $(CTX) wait --for=condition=Established crd --all --timeout=120s
	touch $@

$(CS)/image.loaded: $(IS)/controller.id $(CS)/ready
	$(KIND) load docker-image $(E2E_LOCAL_IMAGE) --name $(CLUSTER_FROM_CTX)
	touch $@

# DEPLOY_INPUTS: all config files except CRDs (tracked separately by crds.applied).
# Only relevant for the main e2e cluster (controller.deployed is not used by quickstart targets).
# When PROJECT_IMAGE is set (CI), skip the local image build stamp.
ifeq ($(PROJECT_IMAGE),)
DEPLOY_INPUTS := install \
                 $(CS)/sut.namespace.ready \
                 $(IS)/controller.id \
                 $(shell find config -type f -not -path 'config/crd/*')
else
DEPLOY_INPUTS := install \
                 $(CS)/sut.namespace.ready \
                 $(shell find config -type f -not -path 'config/crd/*')
endif
$(CS)/controller.deployed: $(DEPLOY_INPUTS)
	mkdir -p $(CS)
	@[ -z "$(PROJECT_IMAGE)" ] || $(CONTAINER_TOOL) pull $(E2E_IMAGE)
	$(KIND) load docker-image $(E2E_IMAGE) --name $(CLUSTER_FROM_CTX)	
	cd config && $(KUSTOMIZE) edit set image gitops-reverser=$(E2E_IMAGE)
	$(KUSTOMIZE) build config | kubectl --context $(CTX) apply -f -
	@if [ -z "$(PROJECT_IMAGE)" ]; then \
	  IMAGE_ID="$$(cat $(IS)/controller.id)"; \
	  kubectl --context $(CTX) -n sut patch deploy/gitops-reverser --type=merge \
	    -p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"configbutler.ai/e2e-image-id\":\"$$IMAGE_ID\"}}}}}"; \
	fi
	kubectl --context $(CTX) -n sut rollout status deploy/gitops-reverser --timeout=180s
	touch $@


.PHONY: $(CS)/portforward.running
$(CS)/portforward.running: $(CS)/gitea.installed $(CS)/prometheus.installed
	mkdir -p $(CS)
	E2E_KUBECONTEXT=$(CTX) bash test/e2e/scripts/setup-port-forwards.sh
	touch $@

$(CS)/e2e.passed: install $(CS)/controller.deployed $(CS)/portforward.running $(CS)/age-key.applied $(shell find test/e2e -name '*.go')
	mkdir -p $(CS)
	kubectl --context $(CTX) delete crd icecreamorders.shop.example.com --ignore-not-found=true
	KIND_CLUSTER=$(CLUSTER_FROM_CTX) PROJECT_IMAGE=$(E2E_IMAGE) \
	  E2E_AGE_KEY_FILE=$(CS)/age-key.txt \
	  go test ./test/e2e/ -v -ginkgo.v
	touch $@

# INSTALL_MODE controls installation workflow: helm|plain-manifests-file|config-dir
INSTALL_MODE ?= config-dir
INSTALL_NAME ?= gitops-reverser
NAMESPACE ?= gitops-reverser
INSTALL_LOCK := $(CS)/$(NAMESPACE)/install.method
VALID_INSTALL_MODES := config-dir helm plain-manifests-file
ifeq (,$(filter $(INSTALL_MODE),$(VALID_INSTALL_MODES)))
  $(error INSTALL_MODE must be one of: $(VALID_INSTALL_MODES))
endif

define ASSERT_INSTALL_METHOD
mkdir -p $(@D); \
if [ -f "$(INSTALL_LOCK)" ]; then \
	existing="$$(cat "$(INSTALL_LOCK)")"; \
	if [ "$$existing" != "$(INSTALL_MODE)" ]; then \
		echo "ERROR: Namespace $(NAMESPACE) already installed using '$$existing'."; \
		echo "Refusing to install with '$(INSTALL_MODE)'."; \
		echo "Cleanup that namespace and remove $(INSTALL_LOCK) to switch methods."; \
		exit 2; \
	fi; \
else \
	echo "$(INSTALL_MODE)" > "$(INSTALL_LOCK)"; \
fi;
endef

.PHONY: install install-helm install-plain-manifests-file install-config-dir
install: $(CS)/$(NAMESPACE)/install-$(INSTALL_MODE)

install-helm: $(CS)/$(NAMESPACE)/install-helm

install-plain-manifests-file: $(CS)/$(NAMESPACE)/install-plain-manifests-file

install-config-dir: $(CS)/$(NAMESPACE)/install-config-dir

##@ Deployments of all manifests needed to run
$(CS)/$(NAMESPACE)/install-helm: INSTALL_MODE := helm
$(CS)/$(NAMESPACE)/install-helm: $(HELM_SYNC_OUTPUTS) $(CS)/cert-manager.installed $(CS)/prometheus.installed ## Deploy quickstart controller via Helm into CTX cluster
	@set -euo pipefail; \
	$(ASSERT_INSTALL_METHOD) \
	helm_args=( \
		upgrade --install $(INSTALL_NAME) charts/gitops-reverser \
		--kube-context $(CTX) \
		--namespace $(NAMESPACE) \
		--create-namespace \
	); \
	if kubectl --context $(CTX) get crd \
		gitproviders.configbutler.ai \
		gittargets.configbutler.ai \
		watchrules.configbutler.ai \
		clusterwatchrules.configbutler.ai >/dev/null 2>&1; then \
		helm_args+=(--skip-crds); \
	fi; \
	helm "$${helm_args[@]}"; \
	touch $@

$(CS)/$(NAMESPACE)/install-config-dir: INSTALL_MODE := config-dir
$(CS)/$(NAMESPACE)/install-config-dir: $(MANIFEST_OUTPUTS) $(CS)/cert-manager.installed $(CS)/prometheus.installed ## Deploy controller to the K8s cluster specified in ~/.kube/config.
	@set -euo pipefail; \
	$(ASSERT_INSTALL_METHOD)
	(cd config && $(KUSTOMIZE) edit set image gitops-reverser=${IMG})
	(cd config && $(KUSTOMIZE) build .) | $(KUBECTL) apply -f -
	touch $@

$(CS)/$(NAMESPACE)/install-plain-manifests-file: INSTALL_MODE := plain-manifests-file
$(CS)/$(NAMESPACE)/install-plain-manifests-file: dist/install.yaml $(CS)/cert-manager.installed $(CS)/prometheus.installed ## Deploy quickstart controller via manifest into CTX cluster
	@set -euo pipefail; \
	ctx="$(CTX)"; \
	ns="$(NAMESPACE)"; \
	$(ASSERT_INSTALL_METHOD) \
	( \
		cd dist && \
		kustomize edit set namespace "$$ns" >/dev/null && \
		kustomize build . \
	) | kubectl --context "$$ctx" apply -f -; \
	touch $@

.PHONY: test-e2e-quickstart-helm
test-e2e-quickstart-helm: ## Run quickstart smoke test (Helm install) - always starts with a clean cluster
	$(MAKE) cleanup-cluster KIND_CLUSTER=$(KIND_CLUSTER_QUICKSTART_HELM)
	$(MAKE) CTX=$(KUBECONTEXT_QS_HELM) \
	  .stamps/cluster/$(KUBECONTEXT_QS_HELM)/cert-manager.installed \
	  .stamps/cluster/$(KUBECONTEXT_QS_HELM)/gitea.installed
	@if [ -z "$(PROJECT_IMAGE)" ]; then \
	  $(MAKE) CTX=$(KUBECONTEXT_QS_HELM) \
	    .stamps/cluster/$(KUBECONTEXT_QS_HELM)/image.loaded; \
	fi
	kubectl config use-context $(KUBECONTEXT_QS_HELM)
	PROJECT_IMAGE=$(E2E_IMAGE) \
	  bash test/e2e/scripts/run-quickstart.sh helm

.PHONY: test-e2e-quickstart-manifest
test-e2e-quickstart-manifest: ## Run quickstart smoke test (manifest install) - always starts with a clean cluster
	$(MAKE) cleanup-cluster KIND_CLUSTER=$(KIND_CLUSTER_QUICKSTART_MANIFEST)
	$(MAKE) CTX=$(KUBECONTEXT_QS_MANIFEST) \
	  .stamps/cluster/$(KUBECONTEXT_QS_MANIFEST)/cert-manager.installed \
	  .stamps/cluster/$(KUBECONTEXT_QS_MANIFEST)/gitea.installed
	@if [ -z "$(PROJECT_IMAGE)" ]; then \
	  $(MAKE) build-plain-manifests-installer; \
	  $(MAKE) CTX=$(KUBECONTEXT_QS_MANIFEST) \
	    .stamps/cluster/$(KUBECONTEXT_QS_MANIFEST)/image.loaded; \
	fi
	kubectl config use-context $(KUBECONTEXT_QS_MANIFEST)
	PROJECT_IMAGE=$(E2E_IMAGE) \
	  bash test/e2e/scripts/run-quickstart.sh manifest

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
