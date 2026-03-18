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

# Requires GNU Make >= 4.3 (grouped-target &: syntax used for correlated outputs).
SHELL := bash
.ONESHELL:
.SHELLFLAGS := -euo pipefail -c
.SECONDEXPANSION:
# Requires GNU Make >= 4.3 (grouped-target &: syntax used for correlated outputs).

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
MANIFEST_INPUTS = $(shell find api internal cmd -type f -name '*.go' \
	! -name '*_test.go' ! -name 'zz_generated.deepcopy.go')
CONTROLLER_GEN_ARGS := \
	rbac:roleName=gitops-reverser \
	crd \
	webhook \
	paths="./..." \
	output:crd:artifacts:config=config/crd/bases
.PHONY: manifests
manifests: $(MANIFEST_OUTPUTS) ## Generate WebhookConfiguration, ClusterRole and CustomResourceDefinition objects.
$(MANIFEST_OUTPUTS) &: $$(MANIFEST_INPUTS)
	@rm -f config/crd/bases/*.yaml config/rbac/role.yaml config/webhook/manifests.yaml
	$(CONTROLLER_GEN) $(CONTROLLER_GEN_ARGS)

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
GENERATE_INPUTS = $(shell find api -type f -name '*.go' ! -name '*_test.go' \
	! -name 'zz_generated.deepcopy.go') hack/boilerplate.go.txt
GENERATE_OUTPUT := api/v1alpha1/zz_generated.deepcopy.go

.PHONY: generate
generate: $(GENERATE_OUTPUT) ## Generate code containing DeepCopy, DeepCopyInto, and DeepCopyObject method implementations.

$(GENERATE_OUTPUT): $$(GENERATE_INPUTS)
	$(CONTROLLER_GEN) object:headerFile="hack/boilerplate.go.txt" paths="./..."

.PHONY: fmt
fmt: ## Run go fmt against code.
	go fmt ./...

.PHONY: vet
vet: ## Run go vet against code.
	go vet ./...

.PHONY: test
test: manifests generate fmt vet setup-envtest ## Run tests.
	export KUBEBUILDER_ASSETS="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) \
		--bin-dir $(shell pwd)/bin \
		-p path)"
	go test $$(go list ./... | grep -v /e2e) -coverprofile cover.out

E2E_LOCAL_IMAGE ?= gitops-reverser:e2e-local
# Normalize PROJECT_IMAGE once, then consume only normalized variables below.
# - If PROJECT_IMAGE is explicitly provided (CI / caller), use it and skip local source-triggered build dependency.
# - Otherwise default to E2E_LOCAL_IMAGE and treat it as local-build managed.
PROJECT_IMAGE_INPUT := $(strip $(PROJECT_IMAGE))
PROJECT_IMAGE := $(if $(PROJECT_IMAGE_INPUT),$(PROJECT_IMAGE_INPUT),$(E2E_LOCAL_IMAGE))
# Treat PROJECT_IMAGE as "provided" only when it differs from the local default.
PROJECT_IMAGE_PROVIDED := $(if $(filter-out $(E2E_LOCAL_IMAGE),$(PROJECT_IMAGE_INPUT)),true,)
export PROJECT_IMAGE
export PROJECT_IMAGE_PROVIDED
E2E_AGE_KEY_FILE ?= /tmp/e2e-age-key.txt

# CTX: kubeconfig context for the cluster being operated on.
# Defaults to the main e2e cluster; override with CTX=<name> to reuse stamp targets for other clusters.
CTX ?= k3d-gitops-reverser-test-e2e
# Derive the cluster name by stripping a known context prefix from CTX.
CLUSTER_NAME ?= $(patsubst kind-%,%,$(patsubst k3d-%,%,$(CTX)))
CS := .stamps/cluster/$(CTX)
IS := .stamps/image
GO_SOURCES = $(shell find cmd internal api -type f -name '*.go' \
	! -name '*_test.go' ! -name 'zz_generated.deepcopy.go') go.mod go.sum

# INSTALL_MODE controls installation workflow: helm|plain-manifests-file|config-dir
# These defaults must be defined before any target names that reference $(NAMESPACE).
INSTALL_MODE ?= config-dir
INSTALL_NAME ?= gitops-reverser
NAMESPACE ?= gitops-reverser
HELM_CHART_SOURCE ?= charts/gitops-reverser
CONTROLLER_CONTAINER ?= manager
CONTROLLER_DEPLOY_SELECTOR ?= app.kubernetes.io/part-of=gitops-reverser
VALID_INSTALL_MODES := config-dir helm plain-manifests-file
INSTALL_CRDS := \
	gitproviders.configbutler.ai \
	gittargets.configbutler.ai \
	watchrules.configbutler.ai \
	clusterwatchrules.configbutler.ai
ifeq (,$(filter $(INSTALL_MODE),$(VALID_INSTALL_MODES)))
  $(error INSTALL_MODE must be one of [$(VALID_INSTALL_MODES)])
endif

# Called by the Go e2e suite (BeforeSuite) to prepare prerequisites once, including port-forwards + age key.
.PHONY: prepare-e2e
prepare-e2e: $(CS)/$(NAMESPACE)/prepare-e2e.ready portforward-ensure ## Prepare E2E prerequisites for Go tests

##@ E2E Gitea Setup (split bootstrap + run setup; produces concrete artifacts under $(CS))

GITEA_ADMIN_USER ?= giteaadmin
GITEA_ADMIN_PASS ?= giteapassword123
GITEA_ORG_NAME ?= testorg
E2E_GIT_SECRET_HTTP ?=
E2E_GIT_SECRET_SSH ?=
E2E_GIT_SECRET_INVALID ?=

.PHONY: e2e-gitea-bootstrap
e2e-gitea-bootstrap: $(CS)/gitea/bootstrap/org-$(GITEA_ORG_NAME).ready ## Bootstrap shared Gitea prerequisites for the cluster context

.PHONY: e2e-gitea-run-setup
e2e-gitea-run-setup: $(CS)/$(NAMESPACE)/git-$(REPO_NAME)/checkout.ready ## Create active repo+creds+checkout for this run

$(CS)/gitea/bootstrap/org-$(GITEA_ORG_NAME).ready: $(CS)/$(NAMESPACE)/prepare-e2e.ready hack/e2e/gitea-bootstrap.sh | $(CS)/gitea/bootstrap
	$(MAKE) CTX=$(CTX) INSTALL_MODE=$(INSTALL_MODE) INSTALL_NAME=$(INSTALL_NAME) NAMESPACE=$(NAMESPACE) portforward-ensure
	export BOOTSTRAP_DIR=$(CS)/gitea/bootstrap
	export API_URL=http://localhost:$(GITEA_PORT)/api/v1
	export GITEA_ADMIN_USER=$(GITEA_ADMIN_USER)
	export GITEA_ADMIN_PASS=$(GITEA_ADMIN_PASS)
	export ORG_NAME=$(GITEA_ORG_NAME)
	bash hack/e2e/gitea-bootstrap.sh
	@test -f $@

$(CS)/$(NAMESPACE)/git-$(REPO_NAME)/checkout.ready: $(CS)/gitea/bootstrap/org-$(GITEA_ORG_NAME).ready hack/e2e/gitea-run-setup.sh | $(CS)/$(NAMESPACE)
	@[ -n "$(REPO_NAME)" ] || { echo "ERROR: REPO_NAME must be set for e2e-gitea-run-setup" >&2; exit 2; }
	mkdir -p $(@D)
	export CTX=$(CTX)
	export CS=$(CS)
	export NAMESPACE=$(NAMESPACE)
	export REPO_NAME=$(REPO_NAME)
	export CHECKOUT_DIR=$(CHECKOUT_DIR)
	export API_URL=http://localhost:$(GITEA_PORT)/api/v1
	export GITEA_ADMIN_USER=$(GITEA_ADMIN_USER)
	export GITEA_ADMIN_PASS=$(GITEA_ADMIN_PASS)
	export ORG_NAME=$(GITEA_ORG_NAME)
	export E2E_GIT_SECRET_HTTP=$(E2E_GIT_SECRET_HTTP)
	export E2E_GIT_SECRET_SSH=$(E2E_GIT_SECRET_SSH)
	export E2E_GIT_SECRET_INVALID=$(E2E_GIT_SECRET_INVALID)
	bash hack/e2e/gitea-run-setup.sh
	@test -f $@

# Called by the full e2e suite.
# For now: clean the sut namespace, recreate it, run the installer, and deploy the controller image.
# Prefer depending on stamps; this target must not invoke Go e2e tests (Go calls this target).
$(CS)/$(NAMESPACE)/prepare-e2e.ready: $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/install.yaml \
	$(CS)/image.loaded \
	$(CS)/$(NAMESPACE)/controller.deployed \
	$(CS)/$(NAMESPACE)/sops-secret.applied \
	| $(CS)/$(NAMESPACE)
	touch $@

.PHONY: test-e2e
test-e2e: ## Run the full e2e test suite
	export CTX=$(CTX)
	export INSTALL_MODE=$(INSTALL_MODE)
	export NAMESPACE=$(NAMESPACE)
	export E2E_AGE_KEY_FILE=$(CS)/age-key.txt
	go test ./test/e2e/ -v -ginkgo.v

.PHONY: lint
lint: ## Run golangci-lint linter
	$(GOLANGCI_LINT) run
	$(CHECKMAKE) Makefile

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

CHART_INPUTS = charts/gitops-reverser/Chart.yaml \
	charts/gitops-reverser/values.yaml \
	$(shell find charts/gitops-reverser/templates -type f)

dist/install.yaml: $(HELM_SYNC_OUTPUTS) $$(CHART_INPUTS) | dist ## Generate consolidated YAML from Helm chart.
	$(HELM) template $(INSTALL_NAME) charts/gitops-reverser \
		--namespace $(NAMESPACE) \
		--set labels.managedBy=kubectl \
		--set createNamespace=true \
		--include-crds \
		> $@

##@ Dependencies

## Tool Binaries - all pre-installed in devcontainer
KUBECTL ?= kubectl
K3D ?= k3d
HELM ?= helm
FLUX ?= flux
KUSTOMIZE ?= kustomize
CONTROLLER_GEN ?= controller-gen
ENVTEST ?= setup-envtest
GOLANGCI_LINT ?= golangci-lint
CHECKMAKE ?= checkmake

## Tool Versions (for reference - versions defined in .devcontainer/Dockerfile)
ENVTEST_K8S_VERSION ?= $(shell \
	go list -m -f "{{ .Version }}" k8s.io/api \
	| awk -F'[v.]' '{printf "1.%d", $$3}' \
)

# Gitea E2E Configuration
GITEA_PORT ?= 13000
PROMETHEUS_PORT ?= 19090

# Valkey E2E Configuration
VALKEY_PORT ?= 16379

FLUX_NAMESPACE ?= flux-system
FLUX_WAIT_TIMEOUT ?= 300s
FLUX_SERVICES_WAIT_TIMEOUT ?= 600s
FLUX_SERVICES_DIR ?= test/e2e/setup/flux

ENVTEST_STAMP = .stamps/envtest-$(ENVTEST_K8S_VERSION).ready

.PHONY: setup-envtest
setup-envtest: $(ENVTEST_STAMP) ## Setup envtest binaries for unit tests

$(ENVTEST_STAMP): Makefile
	echo "Setting up envtest binaries for Kubernetes version $(ENVTEST_K8S_VERSION)..."
	mkdir -p $(shell pwd)/bin
	$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(shell pwd)/bin -p path
	mkdir -p $(@D)
	touch $@

##@ E2E Stamp Targets (cluster-parameterized; pass CTX=k3d-<name> to target a different cluster)

$(CS) $(IS) $(CS)/$(NAMESPACE) $(CS)/gitea/bootstrap \
$(CS)/$(NAMESPACE)/helm $(CS)/$(NAMESPACE)/config-dir $(CS)/$(NAMESPACE)/plain-manifests-file dist:
	mkdir -p $@

$(CS)/ready: test/e2e/cluster/start-cluster.sh | $(CS)
	export CLUSTER_NAME=$(CLUSTER_NAME)
	bash test/e2e/cluster/start-cluster.sh
	kubectl --context $(CTX) get ns >/dev/null
	touch $@

FLUX_SERVICES_INPUTS = $(shell find $(FLUX_SERVICES_DIR) -type f)
SHARED_MANIFESTS_DIR = test/e2e/setup/manifests
SHARED_MANIFESTS = $(shell find $(SHARED_MANIFESTS_DIR) -type f -name '*.yaml')
DEMO_MANIFESTS_DIR = test/e2e/setup/demo-only
DEMO_MANIFESTS = $(shell find $(DEMO_MANIFESTS_DIR) -type f -name '*.yaml')
DEMO_TUNNEL_CREDENTIALS = $(DEMO_MANIFESTS_DIR)/cloudflared-public/tunnel-credentials.yaml
DEMO_PULL_SECRET = $(DEMO_MANIFESTS_DIR)/vote/pull-secret.yaml
FLUX_SETUP_READY_INPUTS = $(CS)/flux.installed $$(FLUX_SERVICES_INPUTS)
SERVICES_READY_INPUTS = $(CS)/flux-setup.ready $$(SHARED_MANIFESTS)
$(CS)/flux.installed: $(CS)/ready | $(CS)
	$(FLUX) install --context $(CTX) --namespace $(FLUX_NAMESPACE)
	kubectl --context $(CTX) -n $(FLUX_NAMESPACE) wait \
		deployment \
		-l app.kubernetes.io/part-of=flux \
		--for=condition=Available \
		--timeout=$(FLUX_WAIT_TIMEOUT)
	kubectl --context $(CTX) wait --for=condition=Established \
		crd/helmrepositories.source.toolkit.fluxcd.io \
		crd/helmreleases.helm.toolkit.fluxcd.io \
		--timeout=$(FLUX_WAIT_TIMEOUT)
	$(FLUX) version --client > $@

# Aggregate stamp for Flux-managed external E2E services required by tests.
.SILENT: $(CS)/flux-setup.ready $(CS)/services.ready
$(CS)/flux-setup.ready: $(FLUX_SETUP_READY_INPUTS) | $(CS)
	kubectl --context $(CTX) apply -k $(FLUX_SERVICES_DIR)
	flux_ready_count=0
	echo "⏳ Waiting for Flux-managed installations to become ready..."
	for kind in \
		helmreleases.helm.toolkit.fluxcd.io \
		kustomizations.kustomize.toolkit.fluxcd.io
	do
		resources="$$(kubectl --context $(CTX) get $$kind --all-namespaces -o jsonpath='{range .items[*]}{.metadata.namespace}{" "}{.metadata.name}{"\n"}{end}' 2>/dev/null)"
		[ -z "$$resources" ] && continue

		resource_count="$$(printf '%s\n' "$$resources" | sed '/^$$/d' | wc -l | tr -d ' ')"
		flux_ready_count="$$(($$flux_ready_count + $$resource_count))"

		printf '%s\n' "$$resources" | while read -r namespace name; do
			[ -n "$$namespace" ] || continue
			kubectl --context $(CTX) -n "$$namespace" wait "$$kind/$$name" --for=condition=Ready --timeout=$(FLUX_SERVICES_WAIT_TIMEOUT)
		done
	done
	[ "$$flux_ready_count" -gt 0 ] || { echo "ERROR: no Flux-managed e2e ready-check resources found" >&2; exit 1; }
	echo "✓ Flux-managed installations ready: $$flux_ready_count"
	touch $@

# Aggregate stamp for external E2E services required by tests.
$(CS)/services.ready: $(SERVICES_READY_INPUTS) | $(CS)
	kubectl --context $(CTX) wait --for=condition=Established crd/prometheuses.monitoring.coreos.com crd/servicemonitors.monitoring.coreos.com --timeout=180s
	kubectl --context $(CTX) apply -k $(SHARED_MANIFESTS_DIR)
	touch $@

.PHONY: prepare-e2e-demo
prepare-e2e-demo: $(CS)/demo.ready ## Prepare demo-only prerequisites for talk/demo e2e runs

$(CS)/demo.ready: $(CS)/services.ready $$(DEMO_MANIFESTS) | $(CS)
	$(call REQUIRE_FILE,$(DEMO_TUNNEL_CREDENTIALS),demo tunnel credentials,Create it from '$(DEMO_TUNNEL_CREDENTIALS).example' and set stringData.token before running this target.)
	$(call REQUIRE_FILE,$(DEMO_PULL_SECRET),demo image pull secret,Create it from '$(DEMO_PULL_SECRET).example' before running this target.)
	kubectl --context $(CTX) apply -f $(DEMO_MANIFESTS_DIR)/vote/ns.yaml
	kubectl --context $(CTX) apply -f $(DEMO_MANIFESTS_DIR)/cloudflared-public/ns.yaml
	kubectl --context $(CTX) wait --for=jsonpath='{.status.phase}'=Active \
		namespace/vote \
		namespace/cloudflared-public \
		--timeout=120s
	kubectl --context $(CTX) apply -f $(DEMO_MANIFESTS_DIR)/vote/crds
	kubectl --context $(CTX) wait --for=condition=Established \
		crd/quizsessions.examples.configbutler.ai \
		crd/quizsubmissions.examples.configbutler.ai \
		--timeout=180s
	kubectl --context $(CTX) apply -k $(DEMO_MANIFESTS_DIR)
	touch $@

# Step 1: Generate age key file — no cluster/namespace dependency; safe to run before installation.
$(CS)/age-key.txt: Makefile test/e2e/tools/gen-age-key/main.go | $(CS)
	go run ./test/e2e/tools/gen-age-key \
		--key-file $@

# Step 2: Derive Kubernetes Secret manifest from the key file — still no cluster dependency.
$(CS)/$(NAMESPACE)/sops-secret.yaml: $(CS)/age-key.txt Makefile | $(CS)/$(NAMESPACE)
	go run ./test/e2e/tools/gen-age-key \
		--key-file $(CS)/age-key.txt \
		--secret-file $@ \
		--namespace $(NAMESPACE) \
		--secret-name sops-age-key

# Step 3: Apply the secret into the namespace — requires the namespace to already exist.
# Explicit dep on install.yaml ensures the namespace is created before kubectl apply.
$(CS)/$(NAMESPACE)/sops-secret.applied: $(CS)/$(NAMESPACE)/sops-secret.yaml $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/install.yaml
	kubectl --context "$(CTX)" apply -f $(CS)/$(NAMESPACE)/sops-secret.yaml
	touch $@

$(IS)/controller.id: $$(GO_SOURCES) Dockerfile | $(IS)
	$(CONTAINER_TOOL) build \
		--build-arg TARGETOS=$(shell go env GOOS) \
		--build-arg TARGETARCH=$(shell go env GOARCH) \
		-t $(E2E_LOCAL_IMAGE) \
		.
	$(CONTAINER_TOOL) inspect --format='{{.Id}}' $(E2E_LOCAL_IMAGE) > $@

$(IS)/project-image.ready: $(if $(PROJECT_IMAGE_PROVIDED),,$(IS)/controller.id) | $(IS)
	if [ -n "$(PROJECT_IMAGE_PROVIDED)" ]; then
		if ! $(CONTAINER_TOOL) image inspect "$(PROJECT_IMAGE)" >/dev/null 2>&1; then
			echo "Pulling PROJECT_IMAGE=$(PROJECT_IMAGE)"
			$(CONTAINER_TOOL) pull "$(PROJECT_IMAGE)"
		fi
	fi
	echo "$(PROJECT_IMAGE)" > "$@"

CRD_INPUTS = $(shell find config/crd -type f)
$(CS)/crds.applied: $(CS)/ready $$(CRD_INPUTS) | $(CS)
	kubectl --context $(CTX) apply -k config/crd
	kubectl --context $(CTX) wait --for=condition=Established crd --all --timeout=120s
	touch $@

$(CS)/image.loaded: $(IS)/project-image.ready $(CS)/ready | $(CS)
	if ! $(CONTAINER_TOOL) image inspect "$(PROJECT_IMAGE)" >/dev/null 2>&1; then
		if [ -z "$(PROJECT_IMAGE_PROVIDED)" ]; then
			echo "Local image $(PROJECT_IMAGE) missing; rebuilding..."
			$(MAKE) $(IS)/controller.id
		else
			echo "ERROR: PROJECT_IMAGE=$(PROJECT_IMAGE) not found locally" >&2
			exit 2
		fi
	fi
	img_id="$$( $(CONTAINER_TOOL) inspect --format='{{.Id}}' $(PROJECT_IMAGE) )"
	echo "Loading $(PROJECT_IMAGE) ($$img_id) into $(CTX)"
	$(K3D) image import $(PROJECT_IMAGE) -c $(CLUSTER_NAME)
	echo "$$img_id" > "$@"

$(CS)/$(NAMESPACE)/controller.deployed: $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/install.yaml $(CS)/image.loaded | $(CS)/$(NAMESPACE)
	ctx="$(CTX)"
	ns="$(NAMESPACE)"
	img="$(PROJECT_IMAGE)"
	container="$(CONTROLLER_CONTAINER)"
	selector="$(CONTROLLER_DEPLOY_SELECTOR)"
	[ -n "$$img" ] || { echo "ERROR: PROJECT_IMAGE must be non-empty" >&2; exit 2; }
	[ -n "$$container" ] || { echo "ERROR: CONTROLLER_CONTAINER must be non-empty" >&2; exit 2; }
	count="$$(kubectl --context "$$ctx" -n "$$ns" get deploy -l "$$selector" --no-headers 2>/dev/null | wc -l | tr -d ' ')"
	[ "$$count" -eq 1 ] || {
		echo "ERROR: Expected exactly 1 Deployment matching '$$selector' in namespace '$$ns', found $$count" >&2
		kubectl --context "$$ctx" -n "$$ns" get deploy -l "$$selector" -o wide || true
		exit 1
	}
	deploy="$$(kubectl --context "$$ctx" -n "$$ns" get deploy -l "$$selector" -o jsonpath='{.items[0].metadata.name}')"
	echo "Setting deployment/$$deploy container '$$container' to image '$$img'"
	kubectl --context "$$ctx" -n "$$ns" set image "deployment/$$deploy" "$$container=$$img"
	kubectl --context "$$ctx" -n "$$ns" rollout status "deployment/$$deploy" --timeout=180s
	touch "$@"

.PHONY: portforward-ensure
portforward-ensure: $(CS)/services.ready ## Ensure port-forwards are running (always checks)
	export E2E_KUBECONTEXT=$(CTX)
	export GITEA_PORT=$(GITEA_PORT)
	export PROMETHEUS_PORT=$(PROMETHEUS_PORT)
	export VALKEY_PORT=$(VALKEY_PORT)
	bash hack/e2e/setup-port-forwards.sh


.PHONY: install install-helm install-plain-manifests-file install-config-dir
install: $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/install.yaml

install-helm: $(CS)/$(NAMESPACE)/helm/install.yaml

install-plain-manifests-file: $(CS)/$(NAMESPACE)/plain-manifests-file/install.yaml

install-config-dir: $(CS)/$(NAMESPACE)/config-dir/install.yaml

# Shared cleanup logic — inlined at the start of every install recipe so it always
# runs whenever the recipe runs, regardless of stamp freshness.
define DO_CLEANUP_INSTALLS
	@CTX="$(CTX)" KUBECTL="$(KUBECTL)" bash hack/cleanup-installs.sh
endef

# Replace $@ only if the newly rendered file differs.
# This keeps $@’s mtime stable on no-op re-runs, so downstream targets don’t
# rebuild just because we rewrote identical content.
define UPDATE_IF_CHANGED
	if [ -f "$@" ] && cmp -s "$(1)" "$@"; then
		rm -f "$(1)"
	else
		mv "$(1)" "$@"
	fi
endef

define REQUIRE_FILE
	@[ -f "$(1)" ] || { \
		echo "ERROR: missing required $(2) file '$(1)'" >&2; \
		echo "$(3)" >&2; \
		exit 1; \
	}
endef

##@ Deployments of all manifests needed to run
$(CS)/$(NAMESPACE)/helm/install.yaml: $(CS)/services.ready $(HELM_SYNC_OUTPUTS) $$(CHART_INPUTS) | $(CS)/$(NAMESPACE)/helm
	$(DO_CLEANUP_INSTALLS)
	mkdir -p "$(@D)" # keep: cleanup script can delete this directory during the same recipe
	skip_crds_arg=""
	if kubectl --context $(CTX) get crd $(INSTALL_CRDS) >/dev/null 2>&1; then
		skip_crds_arg="--skip-crds"
	fi
	$(HELM) --kube-context $(CTX) upgrade --install $(INSTALL_NAME) "$(HELM_CHART_SOURCE)" \
		--namespace $(NAMESPACE) \
		--create-namespace \
		$$skip_crds_arg
	tmp_manifest="$(@D)/.$(@F).tmp"
	$(HELM) --kube-context $(CTX) get manifest $(INSTALL_NAME) \
		--namespace $(NAMESPACE) \
		> "$$tmp_manifest"
	$(call UPDATE_IF_CHANGED,$$tmp_manifest)

CONFIG_DIR_INPUTS = $(shell find config -type f)
$(CS)/$(NAMESPACE)/config-dir/install.yaml: $(CS)/services.ready $(MANIFEST_OUTPUTS) $$(CONFIG_DIR_INPUTS) | $(CS)/$(NAMESPACE)/config-dir
	$(DO_CLEANUP_INSTALLS)
	mkdir -p "$(@D)" # keep: cleanup script can delete this directory during the same recipe
	tmpdir="$$(mktemp -d)"
	trap 'rm -rf "$$tmpdir"' EXIT
	cp -R config "$$tmpdir/config"
	(cd "$$tmpdir/config" && $(KUSTOMIZE) edit set namespace "$(NAMESPACE)")
	(cd "$$tmpdir/config" && $(KUSTOMIZE) edit set image gitops-reverser="$(PROJECT_IMAGE)")
	(cd "$$tmpdir/config" && $(KUSTOMIZE) build .) \
		| tee "$$tmpdir/install.yaml" \
		| $(KUBECTL) --context "$(CTX)" apply -f -
	$(call UPDATE_IF_CHANGED,$$tmpdir/install.yaml)

$(CS)/$(NAMESPACE)/plain-manifests-file/install.yaml: $(CS)/services.ready dist/install.yaml | $(CS)/$(NAMESPACE)/plain-manifests-file
	$(DO_CLEANUP_INSTALLS)
	mkdir -p "$(@D)" # keep: cleanup script can delete this directory during the same recipe
	ctx="$(CTX)"
	ns="$(NAMESPACE)"
	tmpdir="$$(mktemp -d)"
	trap 'rm -rf "$$tmpdir"' EXIT
	cp dist/install.yaml "$$tmpdir/install.yaml"
	printf '%s\n' \
		'apiVersion: kustomize.config.k8s.io/v1beta1' \
		'kind: Kustomization' \
		'resources:' \
		'- install.yaml' \
		> "$$tmpdir/kustomization.yaml"
	(
		cd "$$tmpdir"
		$(KUSTOMIZE) edit set namespace "$$ns" >/dev/null
		$(KUSTOMIZE) build .
	) | tee "$$tmpdir/rendered-install.yaml" \
		| $(KUBECTL) --context "$$ctx" apply -f -
	$(call UPDATE_IF_CHANGED,$$tmpdir/rendered-install.yaml)

.PHONY: test-e2e-quickstart-helm
test-e2e-quickstart-helm: ## Run quickstart smoke test (Helm install)
	export CTX=$(CTX)
	export INSTALL_MODE=helm
	export NAMESPACE=$(NAMESPACE)
	export E2E_ENABLE_QUICKSTART_FRAMEWORK=true
	export E2E_QUICKSTART_MODE=helm
	export E2E_AGE_KEY_FILE=$(CS)/age-key.txt
	go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter=quickstart-framework

.PHONY: test-e2e-demo
test-e2e-demo: prepare-e2e-demo ## Prepare a reusable talk/demo repo and leave demo resources in place
	export CTX=$(CTX)
	export INSTALL_MODE=$(INSTALL_MODE)
	export NAMESPACE=$(NAMESPACE)
	export E2E_ENABLE_TALK_FRAMEWORK=true
	export E2E_AGE_KEY_FILE=$(CS)/age-key.txt
	go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter=talk-demo

.PHONY: test-e2e-audit-redis
test-e2e-audit-redis: ## Run dedicated audit->Redis enqueue e2e scenario
	export CTX=$(CTX)
	export INSTALL_MODE=$(INSTALL_MODE)
	export NAMESPACE=$(NAMESPACE)
	export E2E_AGE_KEY_FILE=$(CS)/age-key.txt
	go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter=audit-redis

.PHONY: test-e2e-bi
test-e2e-bi: ## Run dedicated bi-directional Flux + gitops-reverser e2e scenario
	export CTX=$(CTX)
	export INSTALL_MODE=$(INSTALL_MODE)
	export NAMESPACE=$(NAMESPACE)
	export E2E_ENABLE_BI_DIRECTIONAL=true
	export E2E_AGE_KEY_FILE=$(CS)/age-key.txt
	go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter=bi-directional

.PHONY: test-e2e-quickstart-manifest
test-e2e-quickstart-manifest: ## Run quickstart smoke test (manifest install)
	export CTX=$(CTX)
	export INSTALL_MODE=plain-manifests-file
	export NAMESPACE=$(NAMESPACE)
	export E2E_ENABLE_QUICKSTART_FRAMEWORK=true
	export E2E_QUICKSTART_MODE=plain-manifests-file
	export E2E_AGE_KEY_FILE=$(CS)/age-key.txt
	go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter=quickstart-framework




##@ Demo

# Join code shown on the presenter screen. Auto-extracted from the auth-service log
# when not provided, so `make loadtest` just works from inside the devcontainer.
# Override at the command line: make loadtest LOADTEST_CODE=xxxx
LOADTEST_CODE     ?= $(shell kubectl -n vote logs deploy/vote-auth-service --tail=5 2>/dev/null | grep 'join-code:' | tail -1 | awk '{print $$NF}' | cut -d= -f2)
LOADTEST_USERS    ?= 250
LOADTEST_RAMP     ?= 10s
LOADTEST_BASE_URL ?= https://vote.reversegitops.dev
LOADTEST_SESSION  ?= kubecon-2026
LOADTEST_NS       ?= vote

.PHONY: loadtest
loadtest: ## Simulate LOADTEST_USERS participants against the live quiz (override: LOADTEST_CODE=xxxx LOADTEST_USERS=50 LOADTEST_RAMP=30s)
	@[ -n "$(LOADTEST_CODE)" ] || { \
		echo "ERROR: could not determine join code automatically." >&2; \
		echo "Pass it explicitly: make loadtest LOADTEST_CODE=xxxx" >&2; \
		exit 1; \
	}
	go run ./test/loadtest \
		--code $(LOADTEST_CODE) \
		--users $(LOADTEST_USERS) \
		--ramp-duration $(LOADTEST_RAMP) \
		--base-url $(LOADTEST_BASE_URL) \
		--session $(LOADTEST_SESSION) \
		--namespace $(LOADTEST_NS)

##@ Cleanup
.PHONY: clean
clean: ## Remove build artifacts (binaries, coverage, generated dist files, build stamps)
	rm -rf bin/ cover.out dist/ .stamps/

.PHONY: clean-installs
clean-installs:
	$(DO_CLEANUP_INSTALLS)

.PHONY: clean-port-forwards
clean-port-forwards: ## Stop all port-forwards
	@echo "Stopping port-forwards..."
	@-pkill -f "kubectl.*port-forward.*$(GITEA_PORT)" 2>/dev/null || true
	@-pkill -f "kubectl.*port-forward.*$(PROMETHEUS_PORT)" 2>/dev/null || true
	@-pkill -f "kubectl.*port-forward.*$(VALKEY_PORT)" 2>/dev/null || true
	@echo "Port-forwards stopped"

.PHONY: clean-cluster
clean-cluster: ## Tear down the E2E cluster used for tests and remove its stamps
	if $(K3D) cluster list 2>/dev/null | awk '{print $$1}' | grep -q "^$(CLUSTER_NAME)$$"; then
		echo "🧹 Deleting k3d cluster '$(CLUSTER_NAME)'"
		$(K3D) cluster delete $(CLUSTER_NAME)
	else
		echo "ℹ️ k3d cluster '$(CLUSTER_NAME)' does not exist; skipping cleanup"
	fi
	rm -rf .stamps/cluster/$(CTX)
	
