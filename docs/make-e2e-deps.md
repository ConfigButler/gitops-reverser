# E2E test dependencies with Make stamps

## What the old setup did (replaced)

`make test-e2e` used to run these prerequisites unconditionally on every invocation:

```
setup-cluster → cleanup-webhook → setup-e2e → check-cert-manager → e2e-deploy → setup-port-forwards
```

Every single step was `.PHONY`. Make couldn't skip any of them. On a warm devcontainer with a running cluster that hadn't changed at all, you still paid for image rebuild, Helm reinstall, kustomize apply, and full rollout waits.

---

## The fix: stamp files per cluster

Each cluster-side step is represented as a local file. Make's timestamp logic handles the "did this need to rerun?" question automatically.

The stamp namespace:

```
.stamps/
  image/
    controller.id          ← image ID written after docker build
  cluster/
    kind-<cluster-name>/
      ready                ← cluster created and kubectl works
      cert-manager.installed
      gitea.installed
      prometheus.installed
      image.loaded         ← image loaded into this specific cluster
      crds.applied
      controller.deployed
      portforward.running
      e2e.passed           ← test result stamp
```

Each stamp is written **only after a real verification step** (rollout status, kubectl wait, etc.). If any prerequisite file is newer than the stamp, Make reruns that step — and only that step.

When `make cleanup-cluster` runs, it also `rm -rf .stamps/cluster/kind-$(KIND_CLUSTER)` so the next run starts fresh.

---

## CTX-parameterized stamp targets

All stamp targets are generic — they use a single `CTX` variable instead of hardcoded cluster names. This eliminates duplication: one set of rules serves all clusters.

### Variables

```make
KIND_CLUSTER_E2E               ?= gitops-reverser-test-e2e
KIND_CLUSTER_QUICKSTART_HELM   ?= gitops-reverser-test-e2e-quickstart-helm
KIND_CLUSTER_QUICKSTART_MANIFEST ?= gitops-reverser-test-e2e-quickstart-manifest

# CTX: kubeconfig context for the cluster being operated on.
# Defaults to the main e2e cluster; override with CTX=kind-<name> to reuse stamp targets for other clusters.
CTX ?= kind-$(KIND_CLUSTER_E2E)
# Derive the Kind cluster name by stripping the "kind-" prefix from CTX.
CLUSTER_FROM_CTX = $(patsubst kind-%,%,$(CTX))
CS := .stamps/cluster/$(CTX)    # cluster stamp directory
IS := .stamps/image              # image stamp directory

GO_SOURCES := $(shell find cmd internal -type f -name '*.go') go.mod go.sum

KUBECONTEXT_QS_HELM     := kind-$(KIND_CLUSTER_QUICKSTART_HELM)
KUBECONTEXT_QS_MANIFEST := kind-$(KIND_CLUSTER_QUICKSTART_MANIFEST)
```

> **Note**: `GO_SOURCES` covers `cmd/` and `internal/` — there is no `pkg/` directory in this project.

To target a different cluster with any stamp target, pass `CTX` on the command line:

```sh
make CTX=kind-my-other-cluster .stamps/cluster/kind-my-other-cluster/cert-manager.installed
```

---

## Dependency graph

```
make test-e2e  (phony)
  └─ $(CS)/e2e.passed
       ├─ test/e2e/**/*.go                  (any test change reruns tests)
       └─ $(CS)/portforward.running
            ├─ $(CS)/controller.deployed
            │    ├─ $(CS)/crds.applied
            │    │    └─ $(CS)/ready
            │    │         └─ test/e2e/kind/start-cluster.sh
            │    │            test/e2e/kind/cluster-template.yaml
            │    ├─ $(CS)/cert-manager.installed
            │    │    └─ $(CS)/ready
            │    ├─ $(CS)/prometheus.installed
            │    │    └─ $(CS)/ready
            │    ├─ $(IS)/controller.id
            │    │    └─ cmd/**/*.go  internal/**/*.go  go.mod  go.sum  Dockerfile
            │    └─ config/**  (excluding config/crd/*)
            ├─ $(CS)/gitea.installed
            │    └─ $(CS)/ready
            └─ $(CS)/prometheus.installed
```

What this buys you in practice:

| Change you make | Steps that actually rerun |
|---|---|
| Edit a Go source file | image rebuild → load into kind → controller redeploy → tests |
| Edit a test file only | tests only |
| Edit a CRD manifest | CRD apply → controller redeploy → tests |
| Edit RBAC/manager config | controller redeploy → tests |
| Edit `gitea-values.yaml` | gitea reinstall → port-forwards restart → tests |
| Edit `ensure-prometheus-operator.sh` | prometheus reinstall → port-forwards restart → tests |
| Change `CERT_MANAGER_VERSION` | cert-manager reinstall → controller redeploy → tests |
| Change nothing | everything skipped — only the test binary is checked |

---

## Stamp targets, step by step

### 1 — Cluster ready

```make
$(CS)/ready: test/e2e/kind/start-cluster.sh test/e2e/kind/cluster-template.yaml
	mkdir -p $(CS)
	KIND_CLUSTER=$(CLUSTER_FROM_CTX) bash test/e2e/kind/start-cluster.sh
	kubectl --context $(CTX) get ns >/dev/null
	touch $@
```

### 2 — cert-manager

```make
$(CS)/cert-manager.installed: $(CS)/ready
	mkdir -p $(CS)
	kubectl --context $(CTX) apply -f $(CERT_MANAGER_MANIFEST_URL) | grep -v unchanged
	kubectl --context $(CTX) -n cert-manager rollout status deploy/cert-manager --timeout=$(CERT_MANAGER_WAIT_TIMEOUT)
	kubectl --context $(CTX) -n cert-manager rollout status deploy/cert-manager-webhook --timeout=$(CERT_MANAGER_WAIT_TIMEOUT)
	kubectl --context $(CTX) -n cert-manager rollout status deploy/cert-manager-cainjector --timeout=$(CERT_MANAGER_WAIT_TIMEOUT)
	touch $@
```

### 3 — Gitea

```make
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
```

### 4 — Prometheus Operator

```make
$(CS)/prometheus.installed: $(CS)/ready test/e2e/scripts/ensure-prometheus-operator.sh
	mkdir -p $(CS)
	KUBECONTEXT=$(CTX) bash test/e2e/scripts/ensure-prometheus-operator.sh
	touch $@
```

### 5 — Image build

For local images (no registry push), `RepoDigests` is empty. Use the image ID instead — it changes whenever the build output changes.

```make
$(IS)/controller.id: $(GO_SOURCES) Dockerfile
	mkdir -p $(IS)
	$(CONTAINER_TOOL) build -t $(E2E_LOCAL_IMAGE) .
	$(CONTAINER_TOOL) inspect --format='{{.Id}}' $(E2E_LOCAL_IMAGE) > $@
```

### 6 — Image loaded into cluster

A separate stamp for loading the image into a specific cluster. This is used by both the main e2e flow (via `controller.deployed`) and the quickstart targets.

```make
$(CS)/image.loaded: $(IS)/controller.id $(CS)/ready
	$(KIND) load docker-image $(E2E_LOCAL_IMAGE) --name $(CLUSTER_FROM_CTX)
	touch $@
```

### 7 — CRDs applied

```make
$(CS)/crds.applied: $(CS)/ready $(shell find config/crd -type f)
	mkdir -p $(CS)
	kubectl --context $(CTX) apply -k config/crd
	kubectl --context $(CTX) wait --for=condition=Established crd --all --timeout=120s
	touch $@
```

### 8 — Controller deployed

`DEPLOY_INPUTS` includes `prometheus.installed` because the controller's ServiceMonitor depends on the Prometheus Operator CRDs being present.

```make
DEPLOY_INPUTS := $(CS)/crds.applied $(CS)/cert-manager.installed $(CS)/prometheus.installed \
                 $(IS)/controller.id \
                 $(shell find config -type f -not -path 'config/crd/*')

$(CS)/controller.deployed: $(DEPLOY_INPUTS)
	mkdir -p $(CS)
	$(KIND) load docker-image $(E2E_LOCAL_IMAGE) --name $(CLUSTER_FROM_CTX)
	kubectl --context $(CTX) delete validatingwebhookconfiguration \
	  gitops-reverser-validating-webhook-configuration --ignore-not-found=true
	cd config && $(KUSTOMIZE) edit set image gitops-reverser=$(E2E_LOCAL_IMAGE)
	$(KUSTOMIZE) build config | kubectl --context $(CTX) apply -f -
	kubectl --context $(CTX) -n sut rollout status deploy/gitops-reverser --timeout=180s
	touch $@
```

### 9 — Port-forwards

`setup-port-forwards.sh` uses bare `kubectl` (no `--context` flag), so the current kubeconfig context must be set first with `kubectl config use-context $(CTX)`.

This target stays a **file target, not `.PHONY`**. The file target skips the recipe when the stamp is newer than all prerequisites. The health-check-first pattern inside the recipe handles the case where forwards are already alive from a previous run but the stamp needed refreshing.

```make
$(CS)/portforward.running: $(CS)/controller.deployed $(CS)/gitea.installed $(CS)/prometheus.installed
	mkdir -p $(CS)
	if curl -fsS http://localhost:13000/api/healthz >/dev/null 2>&1 && \
	   curl -fsS http://localhost:19090/-/healthy    >/dev/null 2>&1; then \
	  echo "port-forwards already healthy, skipping restart"; \
	  touch $@; exit 0; \
	fi
	kubectl config use-context $(CTX)
	bash test/e2e/scripts/setup-port-forwards.sh
	curl -fsS http://localhost:13000/api/healthz >/dev/null || { echo "Gitea health check failed"; exit 1; }
	curl -fsS http://localhost:19090/-/healthy    >/dev/null || { echo "Prometheus health check failed"; exit 1; }
	touch $@
```

The three execution paths this covers:

| Situation | What happens |
|---|---|
| Stamp newer than all prerequisites | Make skips the recipe entirely — zero curl calls |
| Stamp stale, but forwards still alive (e.g. controller redeployed, forwards survived) | Fast path: two curls pass → touch stamp → done |
| Stamp stale and forwards are dead | Slow path: restart script → two curls verify → touch stamp |

### 10 — E2E tests with result stamp

```make
$(CS)/e2e.passed: $(CS)/portforward.running $(shell find test/e2e -name '*.go')
	mkdir -p $(CS)
	KIND_CLUSTER=$(CLUSTER_FROM_CTX) PROJECT_IMAGE=$(E2E_LOCAL_IMAGE) \
	  go test ./test/e2e/ -v -ginkgo.v
	touch $@

.PHONY: test-e2e
test-e2e: $(CS)/e2e.passed
```

### 11 — Cluster teardown invalidates all stamps

```make
.PHONY: cleanup-cluster
cleanup-cluster:
	@if $(KIND) get clusters 2>/dev/null | grep -q "^$(KIND_CLUSTER)$$"; then \
	  $(KIND) delete cluster --name $(KIND_CLUSTER); \
	fi
	rm -rf .stamps/cluster/kind-$(KIND_CLUSTER)
```

`KIND_CLUSTER` defaults to `$(KIND_CLUSTER_E2E)` but can be overridden: `make cleanup-cluster KIND_CLUSTER=my-other-cluster`.

---

## Quickstart tests

Quickstart tests (`test-e2e-quickstart-helm`, `test-e2e-quickstart-manifest`) validate the user-facing install paths (Helm chart and generated `dist/install.yaml`). They use dedicated clusters to keep them isolated from the main e2e suite.

### Design: always start with a clean cluster

Quickstart tests **always delete and recreate their cluster** at the start of every run. This avoids a class of failures where Helm tries to install on top of resources that were previously applied with `kubectl apply` (server-side apply), which causes `metadata.managedFields must be nil` errors. Since `run-quickstart.sh` resets install state but doesn't delete CRDs or ClusterRoles, a clean cluster is the only reliable guarantee.

### CTX override pattern

Quickstart targets reuse the generic stamp targets via recursive Make with `CTX` overridden:

```sh
$(MAKE) CTX=kind-gitops-reverser-test-e2e-quickstart-helm \
  .stamps/cluster/kind-gitops-reverser-test-e2e-quickstart-helm/cert-manager.installed
```

The inner Make re-evaluates `CS` and `CLUSTER_FROM_CTX` with the overridden `CTX`, so the stamp file rules match correctly.

### Helm quickstart target

```make
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
	PROJECT_IMAGE=$(if $(PROJECT_IMAGE),$(PROJECT_IMAGE),$(E2E_LOCAL_IMAGE)) \
	  bash test/e2e/scripts/run-quickstart.sh helm
```

### Manifest quickstart target

```make
.PHONY: test-e2e-quickstart-manifest
test-e2e-quickstart-manifest: ## Run quickstart smoke test (manifest install) - always starts with a clean cluster
	$(MAKE) cleanup-cluster KIND_CLUSTER=$(KIND_CLUSTER_QUICKSTART_MANIFEST)
	$(MAKE) CTX=$(KUBECONTEXT_QS_MANIFEST) \
	  .stamps/cluster/$(KUBECONTEXT_QS_MANIFEST)/cert-manager.installed \
	  .stamps/cluster/$(KUBECONTEXT_QS_MANIFEST)/gitea.installed
	@if [ -z "$(PROJECT_IMAGE)" ]; then \
	  $(MAKE) build-installer; \
	  $(MAKE) CTX=$(KUBECONTEXT_QS_MANIFEST) \
	    .stamps/cluster/$(KUBECONTEXT_QS_MANIFEST)/image.loaded; \
	fi
	kubectl config use-context $(KUBECONTEXT_QS_MANIFEST)
	PROJECT_IMAGE=$(if $(PROJECT_IMAGE),$(PROJECT_IMAGE),$(E2E_LOCAL_IMAGE)) \
	  bash test/e2e/scripts/run-quickstart.sh manifest
```

The manifest target also calls `make build-installer` (regenerates `dist/install.yaml`) before loading the image if `PROJECT_IMAGE` is not set.

### `run-quickstart.sh` is self-contained

`run-quickstart.sh` manages its own Gitea port-forward via an internal `ensure_gitea_api_port_forward()` function. It does **not** call `setup-port-forwards.sh` and does not need Prometheus. The Makefile only needs to ensure cert-manager, Gitea, and the image are loaded before invoking the script.

---

## Targeted test runs with Ginkgo focus flags

The entire e2e suite lives in one `Describe("Manager", ...)` block. You can run subsets without touching the test file:

```make
.PHONY: test-e2e-gitprovider
test-e2e-gitprovider: $(CS)/portforward.running
	KIND_CLUSTER=$(KIND_CLUSTER_E2E) PROJECT_IMAGE=$(E2E_LOCAL_IMAGE) \
	  go test ./test/e2e/ -v -ginkgo.v -ginkgo.focus="GitProvider"

.PHONY: test-e2e-watchrule
test-e2e-watchrule: $(CS)/portforward.running
	KIND_CLUSTER=$(KIND_CLUSTER_E2E) PROJECT_IMAGE=$(E2E_LOCAL_IMAGE) \
	  go test ./test/e2e/ -v -ginkgo.v -ginkgo.focus="WatchRule"

.PHONY: test-e2e-encryption
test-e2e-encryption: $(CS)/portforward.running
	KIND_CLUSTER=$(KIND_CLUSTER_E2E) PROJECT_IMAGE=$(E2E_LOCAL_IMAGE) \
	  go test ./test/e2e/ -v -ginkgo.v -ginkgo.focus="encrypt|SOPS|age"
```

These are `.PHONY` (always rerun the test binary) but still benefit from stamp-based infra: they won't rebuild the image or reinstall Gitea unless something actually changed.

---

## What stamps cannot protect you from

Stamps assume the world doesn't change behind Make's back:

- **Manual `kubectl delete` of a CRD or deployment**: the stamp still exists. Fix: `make cleanup-cluster` to reset stamps.
- **Kind cluster restart / node failure**: `$(CS)/ready` still exists. The recipe guards against this with a live `kubectl get ns` call.
- **Port-forward dies between test runs**: the `.portforward.running` stamp is stale. The curl health-check at the start of the recipe catches this.
- **cert-manager version bump without touching `CERT_MANAGER_VERSION`**: the stamp won't invalidate. Encoding the version in the stamp path (`.../cert-manager-$(CERT_MANAGER_VERSION).installed`) would prevent this, but is not currently implemented.

---

## Summary

| What | Current (after stamps) |
|---|---|
| `make test-e2e` with nothing changed | Runs tests only |
| Go code change | Rebuilds image → redeploys → tests |
| Test file change only | Tests only |
| CRD change | Reapplies CRDs → redeploys → tests |
| Gitea config change | Reinstalls Gitea → tests |
| `make test-e2e-gitprovider` | Runs GitProvider tests only (infra skipped if unchanged) |
| `make test-e2e-quickstart-helm` | Deletes cluster → fresh cluster → cert-manager + gitea + image → quickstart helm test |
| `make test-e2e-quickstart-manifest` | Deletes cluster → fresh cluster → cert-manager + gitea + image → quickstart manifest test |
