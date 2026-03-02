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

# E2E cluster names per test type (kept separate to avoid cross-test contamination).
KIND_CLUSTER_E2E ?= gitops-reverser-test-e2e
KIND_CLUSTER_QUICKSTART_HELM ?= gitops-reverser-qs-helm
KIND_CLUSTER_QUICKSTART_MANIFEST ?= gitops-reverser-qs-manifest
# KIND_CLUSTER is used by cleanup-cluster; defaults to the main e2e cluster.
KIND_CLUSTER ?= $(KIND_CLUSTER_E2E)
E2E_LOCAL_IMAGE ?= gitops-reverser:e2e-local
# Normalize PROJECT_IMAGE once, then consume only normalized variables below.
# - If PROJECT_IMAGE is explicitly provided (CI / caller), use it and skip local source-triggered build dependency.
# - Otherwise default to E2E_LOCAL_IMAGE and treat it as local-build managed.
PROJECT_IMAGE_INPUT := $(strip $(PROJECT_IMAGE))
PROJECT_IMAGE := $(if $(PROJECT_IMAGE_INPUT),$(PROJECT_IMAGE_INPUT),$(E2E_LOCAL_IMAGE))
PROJECT_IMAGE_PROVIDED := $(if $(PROJECT_IMAGE_INPUT),true,)
export PROJECT_IMAGE
export PROJECT_IMAGE_PROVIDED
E2E_AGE_KEY_FILE ?= /tmp/e2e-age-key.txt

# CTX: kubeconfig context for the cluster being operated on.
# Defaults to the main e2e cluster; override with CTX=k3d-<name> to reuse stamp targets for other clusters.
CTX ?= k3d-$(KIND_CLUSTER_E2E)
# Derive the cluster name by stripping a known context prefix from CTX.
CLUSTER_FROM_CTX = $(patsubst kind-%,%,$(patsubst k3d-%,%,$(CTX)))
CS := .stamps/cluster/$(CTX)
IS := .stamps/image
GO_SOURCES := $(shell find cmd internal -type f -name '*.go') go.mod go.sum

# INSTALL_MODE controls installation workflow: helm|plain-manifests-file|config-dir
# These defaults must be defined before any target names that reference $(NAMESPACE).
INSTALL_MODE ?= config-dir
INSTALL_NAME ?= gitops-reverser
NAMESPACE ?= gitops-reverser
E2E_NAMESPACE_LABEL_KEY ?= e2e
E2E_NAMESPACE_LABEL_VALUE ?= true

KUBECONTEXT_QS_HELM := k3d-$(KIND_CLUSTER_QUICKSTART_HELM)
KUBECONTEXT_QS_MANIFEST := k3d-$(KIND_CLUSTER_QUICKSTART_MANIFEST)
HELM_CHART_SOURCE ?= charts/gitops-reverser

.PHONY: cleanup-cluster
cleanup-cluster: ## Tear down the E2E cluster used for tests and remove its stamps
	@if $(K3D) cluster list 2>/dev/null | awk '{print $$1}' | grep -q "^$(KIND_CLUSTER)$$"; then \
		echo "🧹 Deleting k3d cluster '$(KIND_CLUSTER)'"; \
		$(K3D) cluster delete $(KIND_CLUSTER); \
	else \
		echo "ℹ️ k3d cluster '$(KIND_CLUSTER)' does not exist; skipping cleanup"; \
	fi
	rm -rf .stamps/cluster/k3d-$(KIND_CLUSTER)

.PHONY: cleanup-e2e-clusters
cleanup-e2e-clusters: ## Tear down all E2E clusters and their stamps
	@for cluster in "$(KIND_CLUSTER_E2E)" "$(KIND_CLUSTER_QUICKSTART_HELM)" "$(KIND_CLUSTER_QUICKSTART_MANIFEST)"; do \
		if $(K3D) cluster list 2>/dev/null | awk '{print $$1}' | grep -q "^$$cluster$$"; then \
			echo "🧹 Deleting k3d cluster '$$cluster'"; \
			$(K3D) cluster delete "$$cluster"; \
		else \
			echo "ℹ️ k3d cluster '$$cluster' does not exist; skipping cleanup"; \
		fi; \
		rm -rf .stamps/cluster/k3d-$$cluster; \
	done


# Called by the Go e2e suite (BeforeSuite) to prepare prerequisites once, including port-forwards + age key.
.PHONY: prepare-e2e
prepare-e2e: $(CS)/$(NAMESPACE)/prepare-e2e.ready portforward-ensure ## Prepare E2E prerequisites for Go tests

# Called by the full e2e suite.
# For now: clean the sut namespace, recreate it, run the installer, and deploy the controller image.
# Prefer depending on stamps; this target must not invoke Go e2e tests (Go calls this target).
$(CS)/$(NAMESPACE)/prepare-e2e.ready: $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/install.done \
	$(CS)/$(NAMESPACE)/label-namespace.ready \
	$(CS)/image.loaded \
	$(CS)/$(NAMESPACE)/controller.deployed \
	$(CS)/$(NAMESPACE)/age-key.applied
	mkdir -p $(@D)
	touch $@


# Keep `make test-e2e` as the classic entrypoint.
.PHONY: test-e2e
test-e2e: $(CS)/e2e.passed

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
K3D ?= k3d
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

##@ E2E Stamp Targets (cluster-parameterized; pass CTX=k3d-<name> to target a different cluster)

$(CS)/ready: test/e2e/cluster/start-cluster.sh
	mkdir -p $(CS)
	K3D_CLUSTER=$(CLUSTER_FROM_CTX) bash test/e2e/cluster/start-cluster.sh
	kubectl --context $(CTX) get ns >/dev/null
	touch $@

CERT_MANAGER_WAIT_TIMEOUT ?= 300s
CERT_MANAGER_VERSION ?= v1.19.4
CERT_MANAGER_MANIFEST_URL ?= https://github.com/cert-manager/cert-manager/releases/download/$(CERT_MANAGER_VERSION)/cert-manager.yaml
$(CS)/cert-manager.installed: $(CS)/ready
	mkdir -p $(CS)
	kubectl --context $(CTX) apply -f $(CERT_MANAGER_MANIFEST_URL) | grep -v unchanged
	kubectl --context $(CTX) -n cert-manager rollout status deploy/cert-manager --timeout=$(CERT_MANAGER_WAIT_TIMEOUT)
	kubectl --context $(CTX) -n cert-manager rollout status deploy/cert-manager-webhook --timeout=$(CERT_MANAGER_WAIT_TIMEOUT)
	kubectl --context $(CTX) -n cert-manager rollout status deploy/cert-manager-cainjector --timeout=$(CERT_MANAGER_WAIT_TIMEOUT)
	echo $(CERT_MANAGER_VERSION) > $@

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
	kubectl --context $(CTX) wait --for=condition=Established \
	  crd/prometheuses.monitoring.coreos.com \
	  crd/servicemonitors.monitoring.coreos.com \
	  --timeout=180s
	kubectl --context $(CTX) apply -n prometheus-operator -f test/e2e/setup/prometheus
	kubectl --context $(CTX) wait --for=condition=Available prometheus/prometheus-shared-e2e -n prometheus-operator --timeout=180s
	kubectl --context $(CTX) wait --for=condition=ready pod -l prometheus=prometheus-shared-e2e -n prometheus-operator --timeout=180s
	touch $@

$(CS)/$(NAMESPACE)/age-key.applied: Makefile $(CS)/$(NAMESPACE)/label-namespace.ready test/e2e/tools/gen-age-key/main.go
	mkdir -p $(@D)
	go run ./test/e2e/tools/gen-age-key \
	  --key-file $(CS)/$(NAMESPACE)/age-key.txt \
	  --secret-file $(CS)/$(NAMESPACE)/age-key-secret.yaml \
	  --namespace $(NAMESPACE) \
	  --secret-name sops-age-key
	kubectl --context $(CTX) apply -f $(CS)/$(NAMESPACE)/age-key-secret.yaml
	touch $@

$(IS)/controller.id: $(GO_SOURCES) Dockerfile
	mkdir -p $(IS)
	$(CONTAINER_TOOL) build \
	  --build-arg TARGETOS=$(shell go env GOOS) \
	  --build-arg TARGETARCH=$(shell go env GOARCH) \
	  -t $(E2E_LOCAL_IMAGE) .
	$(CONTAINER_TOOL) inspect --format='{{.Id}}' $(E2E_LOCAL_IMAGE) > $@

ifeq ($(PROJECT_IMAGE_PROVIDED),)
$(IS)/project-image.ready: $(IS)/controller.id
endif
$(IS)/project-image.ready: Makefile
	mkdir -p $(IS)
	@set -euo pipefail; \
	if [ -n "$(PROJECT_IMAGE_PROVIDED)" ]; then \
		if ! $(CONTAINER_TOOL) image inspect "$(PROJECT_IMAGE)" >/dev/null 2>&1; then \
			echo "Pulling PROJECT_IMAGE=$(PROJECT_IMAGE)"; \
			$(CONTAINER_TOOL) pull "$(PROJECT_IMAGE)"; \
		fi; \
	fi; \
	echo "$(PROJECT_IMAGE)" > "$@"

$(CS)/crds.applied: $(CS)/ready $(shell find config/crd -type f)
	mkdir -p $(CS)
	kubectl --context $(CTX) apply -k config/crd
	kubectl --context $(CTX) wait --for=condition=Established crd --all --timeout=120s
	touch $@

$(CS)/image.loaded: $(IS)/project-image.ready $(CS)/ready
	@set -euo pipefail; \
	mkdir -p $(CS); \
	img_id="$$( $(CONTAINER_TOOL) inspect --format='{{.Id}}' $(PROJECT_IMAGE) )"; \
	echo "Loading $(PROJECT_IMAGE) ($$img_id) into $(CLUSTER_FROM_CTX)"; \
	$(K3D) image import $(PROJECT_IMAGE) -c $(CLUSTER_FROM_CTX); \
	echo "$$img_id" > "$@"

$(CS)/$(NAMESPACE)/controller.deployed: $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/install.done $(CS)/image.loaded
	@set -euo pipefail; \
	ctx="$(CTX)"; ns="$(NAMESPACE)"; img="$(PROJECT_IMAGE)"; c="$(CONTROLLER_CONTAINER)"; sel="$(CONTROLLER_DEPLOY_SELECTOR)"; \
	[ -n "$$img" ] || { echo "ERROR: PROJECT_IMAGE must be non-empty" >&2; exit 2; }; \
	[ -n "$$c" ] || { echo "ERROR: CONTROLLER_CONTAINER must be non-empty" >&2; exit 2; }; \
	count="$$(kubectl --context "$$ctx" -n "$$ns" get deploy -l "$$sel" --no-headers 2>/dev/null | wc -l | tr -d ' ')"; \
	[ "$$count" -eq 1 ] || { \
		echo "ERROR: Expected exactly 1 Deployment matching '$$sel' in namespace '$$ns', found $$count" >&2; \
		kubectl --context "$$ctx" -n "$$ns" get deploy -l "$$sel" -o wide || true; \
		exit 1; \
	}; \
	deploy="$$(kubectl --context "$$ctx" -n "$$ns" get deploy -l "$$sel" -o jsonpath='{.items[0].metadata.name}')"; \
	echo "Setting deployment/$$deploy container '$$c' to image '$$img'"; \
	kubectl --context "$$ctx" -n "$$ns" set image "deployment/$$deploy" "$$c=$$img" --record=false; \
	kubectl --context "$$ctx" -n "$$ns" rollout status "deployment/$$deploy" --timeout=180s; \
	touch "$@"

.PHONY: portforward-ensure
portforward-ensure: $(CS)/gitea.installed $(CS)/prometheus.installed ## Ensure port-forwards are running (always checks)
	mkdir -p $(CS)
	E2E_KUBECONTEXT=$(CTX) bash test/e2e/scripts/setup-port-forwards.sh

E2E_TEST_INPUTS := $(CS)/$(NAMESPACE)/age-key.applied $(shell find test/e2e -type f \( -name '*.go' -o -name '*.sh' -o -name '*.yaml' -o -name '*.tmpl' \))
$(CS)/e2e.passed: $(E2E_TEST_INPUTS) Makefile
	mkdir -p $(CS)
	CTX=$(CTX) KIND_CLUSTER=$(CLUSTER_FROM_CTX) K3D_CLUSTER=$(CLUSTER_FROM_CTX) NAMESPACE=$(NAMESPACE) \
	  E2E_AGE_KEY_FILE=$(CS)/$(NAMESPACE)/age-key.txt \
	  go test ./test/e2e/ -v -ginkgo.v
	touch $@

CONTROLLER_CONTAINER ?= manager
CONTROLLER_DEPLOY_SELECTOR ?= app.kubernetes.io/part-of=gitops-reverser

# INSTALL_MODE, INSTALL_NAME, NAMESPACE defaults are defined near the E2E variables section above.
VALID_INSTALL_MODES := config-dir helm plain-manifests-file
ifeq (,$(filter $(INSTALL_MODE),$(VALID_INSTALL_MODES)))
  $(error INSTALL_MODE must be one of: $(VALID_INSTALL_MODES))
endif

.PHONY: install install-helm install-plain-manifests-file install-config-dir
install: $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/install.done

install-helm: $(CS)/$(NAMESPACE)/helm/install.done

install-plain-manifests-file: $(CS)/$(NAMESPACE)/plain-manifests-file/install.done

install-config-dir: $(CS)/$(NAMESPACE)/config-dir/install.done

CONFIG_DIR_INPUTS := $(shell find config -type f)

##@ Deployments of all manifests needed to run
$(CS)/$(NAMESPACE)/helm/install.done: INSTALL_MODE := helm
$(CS)/$(NAMESPACE)/helm/install.done: $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/cleanup-e2e-installs.done $(HELM_SYNC_OUTPUTS) $(CS)/cert-manager.installed $(CS)/prometheus.installed
	@set -euo pipefail; \
	helm_args=( \
		upgrade --install $(INSTALL_NAME) "$(HELM_CHART_SOURCE)" \
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

$(CS)/$(NAMESPACE)/config-dir/install.done: INSTALL_MODE := config-dir
$(CS)/$(NAMESPACE)/config-dir/install.done: $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/cleanup-e2e-installs.done $(MANIFEST_OUTPUTS) $(CONFIG_DIR_INPUTS) $(CS)/cert-manager.installed $(CS)/prometheus.installed
	@set -euo pipefail; \
	tmpdir="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmpdir"' EXIT; \
	cp -R config "$$tmpdir/config"; \
	(cd "$$tmpdir/config" && $(KUSTOMIZE) edit set namespace "$(NAMESPACE)"); \
	(cd "$$tmpdir/config" && $(KUSTOMIZE) edit set image gitops-reverser="$(PROJECT_IMAGE)"); \
	(cd "$$tmpdir/config" && $(KUSTOMIZE) build .) | $(KUBECTL) --context "$(CTX)" apply -f -
	touch $@

$(CS)/$(NAMESPACE)/plain-manifests-file/install.done: INSTALL_MODE := plain-manifests-file
$(CS)/$(NAMESPACE)/plain-manifests-file/install.done: $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/cleanup-e2e-installs.done dist/install.yaml $(CS)/cert-manager.installed $(CS)/prometheus.installed
	@set -euo pipefail; \
	ctx="$(CTX)"; \
	ns="$(NAMESPACE)"; \
	tmpdir="$$(mktemp -d)"; \
	trap 'rm -rf "$$tmpdir"' EXIT; \
	cp -R dist "$$tmpdir/dist"; \
	( \
		cd "$$tmpdir/dist" && \
		kustomize edit set namespace "$$ns" >/dev/null && \
		kustomize build . \
	) | kubectl --context "$$ctx" apply -f -; \
	touch $@

.PHONY: test-e2e-quickstart-helm
test-e2e-quickstart-helm: ## Run quickstart smoke test (Helm install) - always starts with a clean cluster
	CTX=$(KUBECONTEXT_QS_HELM) KIND_CLUSTER=$(KIND_CLUSTER_QUICKSTART_HELM) K3D_CLUSTER=$(KIND_CLUSTER_QUICKSTART_HELM) INSTALL_MODE=helm NAMESPACE=$(NAMESPACE) \
	  E2E_ENABLE_QUICKSTART_FRAMEWORK=true E2E_QUICKSTART_MODE=helm \
	  E2E_AGE_KEY_FILE=.stamps/cluster/$(KUBECONTEXT_QS_HELM)/age-key.txt \
	  go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter=quickstart-framework

.PHONY: test-e2e-quickstart-manifest
test-e2e-quickstart-manifest: ## Run quickstart smoke test (manifest install) - always starts with a clean cluster
	CTX=$(KUBECONTEXT_QS_MANIFEST) KIND_CLUSTER=$(KIND_CLUSTER_QUICKSTART_MANIFEST) K3D_CLUSTER=$(KIND_CLUSTER_QUICKSTART_MANIFEST) INSTALL_MODE=plain-manifests-file NAMESPACE=$(NAMESPACE) \
	  E2E_ENABLE_QUICKSTART_FRAMEWORK=true E2E_QUICKSTART_MODE=plain-manifests-file \
	  E2E_AGE_KEY_FILE=.stamps/cluster/$(KUBECONTEXT_QS_MANIFEST)/age-key.txt \
	  go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter=quickstart-framework

.PHONY: cleanup-port-forwards
cleanup-port-forwards: ## Stop all port-forwards
	@echo "🛑 Stopping port-forwards..."
	@-pkill -f "kubectl.*port-forward.*13000" 2>/dev/null || true
	@-pkill -f "kubectl.*port-forward.*19090" 2>/dev/null || true
	@echo "✅ Port-forwards stopped"

$(CS)/$(NAMESPACE)/label-namespace.ready: Makefile $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/install.done
	mkdir -p $(@D)
	kubectl --context $(CTX) label --overwrite ns $(NAMESPACE) \
		$(E2E_NAMESPACE_LABEL_KEY)=$(E2E_NAMESPACE_LABEL_VALUE) \
		app.kubernetes.io/name=gitops-reverser control-plane=gitops-reverser \
		pod-security.kubernetes.io/enforce=restricted
	@echo "✅ Added $(E2E_NAMESPACE_LABEL_KEY)=$(E2E_NAMESPACE_LABEL_VALUE) label to namespace $(NAMESPACE)"
	touch $@

$(CS)/$(NAMESPACE)/$(INSTALL_MODE)/cleanup-e2e-installs.done: Makefile $(CS)/ready
	@set -euo pipefail; \
	ctx="$(CTX)"; \
	ns_list="$$(kubectl --context "$$ctx" get ns -l $(E2E_NAMESPACE_LABEL_KEY)=$(E2E_NAMESPACE_LABEL_VALUE) -o jsonpath='{.items[*].metadata.name}' || true)"; \
	if [ -z "$$ns_list" ]; then \
		echo "ℹ️ No e2e=true namespaces in '$$ctx'"; \
	else \
		echo "🧹 Deleting $(E2E_NAMESPACE_LABEL_KEY)=$(E2E_NAMESPACE_LABEL_VALUE) namespaces in '$$ctx': $$ns_list"; \
		kubectl --context "$$ctx" delete ns $$ns_list --ignore-not-found=true; \
		for ns in $$ns_list; do \
			kubectl --context "$$ctx" wait --for=delete "ns/$$ns" --timeout=300s || true; \
			rm -rf ".stamps/cluster/$$ctx/$$ns" 2>/dev/null || true; \
		done; \
	fi; \
	# Clear single-instance blockers (safe always)
	kubectl --context "$$ctx" delete svc --all-namespaces -l app.kubernetes.io/name=gitops-reverser --ignore-not-found=true || true; \
	kubectl --context "$$ctx" delete validatingwebhookconfiguration gitops-reverser-validating-webhook-configuration --ignore-not-found=true || true; \
	mkdir -p "$(@D)"; \
	touch "$@"
