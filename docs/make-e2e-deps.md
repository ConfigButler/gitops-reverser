# Improving E2E test dependencies with Make stamps

## What the current setup does

`make test-e2e` today runs these prerequisites unconditionally on every invocation:

```
setup-cluster → cleanup-webhook → setup-e2e → check-cert-manager → e2e-deploy → setup-port-forwards
```

Every single step is `.PHONY`. Make cannot skip any of them. On a warm devcontainer with a running cluster that hasn't changed at all, you still pay:

| Step | What it does every time | Cost |
|---|---|---|
| `setup-cluster` | Checks if cluster exists (fast if reusing) | Low |
| `cleanup-webhook` | `kubectl delete validatingwebhookconfiguration` | Low |
| `setup-e2e` → `setup-cert-manager` | `kubectl apply -f <remote URL>` | Slow (network) |
| `setup-e2e` → `setup-gitea-e2e` | `helm repo update` + `helm upgrade --install` | Slow |
| `setup-e2e` → `ensure-prometheus-operator` | Full script with kustomize apply | Slow |
| `check-cert-manager` | `kubectl rollout status` x3 | Medium |
| `e2e-deploy` | `docker build` + `kind load` + `kubectl apply` | Very slow |
| `setup-port-forwards` | Kill old forwards, start new ones | Low |

Then it runs `go test ./test/e2e/`. If you only changed a single test file, you still paid for all of the above.

There are two other costly patterns inside the test suite itself:

- `BeforeSuite` in [test/e2e/e2e_suite_test.go](test/e2e/e2e_suite_test.go) calls `make setup-cluster e2e-build-load-image` when `PROJECT_IMAGE` is not set — duplicating the cluster/image setup already done by the Makefile target.
- `BeforeAll` in [test/e2e/e2e_test.go](test/e2e/e2e_test.go) runs `make install` (CRD apply) and a full deployment restart on every suite run, regardless of whether anything changed.

---

## The core idea: stamp files per cluster

As described in [docs/a-tree.md](docs/a-tree.md), the fix is to represent each cluster-side step as a local file. Make's timestamp logic then handles the "did this need to rerun?" question automatically.

The stamp namespace I'd propose:

```
.stamps/
  image/
    controller.id          ← image ID written after docker build
  cluster/
    kind-gitops-reverser-test-e2e/
      ready                ← cluster created and kubectl works
      cert-manager.installed
      gitea.installed
      prometheus.installed
      crds.applied
      controller.deployed
      portforward.running
      portforward.pid
      e2e.passed           ← test result stamp
```

Each stamp is written **only after a real verification step** (rollout status, kubectl wait, etc.). If any prerequisite file is newer than the stamp, Make reruns that step — and only that step.

When `make cleanup-cluster` runs, it should also `rm -rf .stamps/cluster/$(KUBECONTEXT)/` so the next run starts fresh.

---

## Proposed dependency graph

```
make test-e2e  (phony)
  └─ .stamps/cluster/$(KUBECONTEXT)/e2e.passed
       ├─ test/e2e/**/*.go                  (any test change reruns tests)
       ├─ .stamps/cluster/$(KUBECONTEXT)/portforward.running
       │    ├─ .stamps/cluster/$(KUBECONTEXT)/controller.deployed
       │    ├─ .stamps/cluster/$(KUBECONTEXT)/gitea.installed
       │    └─ .stamps/cluster/$(KUBECONTEXT)/prometheus.installed
       └─ .stamps/cluster/$(KUBECONTEXT)/controller.deployed
            ├─ .stamps/cluster/$(KUBECONTEXT)/crds.applied
            │    └─ .stamps/cluster/$(KUBECONTEXT)/ready
            │         └─ test/e2e/kind/start-cluster.sh
            │            test/e2e/kind/cluster-template.yaml
            ├─ .stamps/cluster/$(KUBECONTEXT)/cert-manager.installed
            │    └─ .stamps/cluster/$(KUBECONTEXT)/ready
            ├─ .stamps/image/controller.id
            │    └─ cmd/**/*.go  internal/**/*.go  pkg/**/*.go
            │       go.mod  go.sum  Dockerfile
            └─ config/crd/**  config/rbac/**  config/manager/**
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

## Makefile changes, step by step

### Variables

```make
KIND_CLUSTER_E2E  ?= gitops-reverser-test-e2e
KUBECONTEXT_E2E   := kind-$(KIND_CLUSTER_E2E)
CS                := .stamps/cluster/$(KUBECONTEXT_E2E)   # cluster stamp dir
IS                := .stamps/image                         # image stamp dir

GO_SOURCES := $(shell find cmd internal pkg -type f -name '*.go') go.mod go.sum
```

### 1 — Cluster ready

```make
$(CS)/ready: test/e2e/kind/start-cluster.sh test/e2e/kind/cluster-template.yaml
	mkdir -p $(CS)
	KIND_CLUSTER=$(KIND_CLUSTER_E2E) bash test/e2e/kind/start-cluster.sh
	kubectl --context $(KUBECONTEXT_E2E) get ns >/dev/null
	touch $@
```

### 2 — cert-manager

The cert-manager version is embedded into a versioned stamp path so changing `CERT_MANAGER_VERSION` automatically invalidates the stamp.

```make
$(CS)/cert-manager.installed: $(CS)/ready
	mkdir -p $(CS)
	kubectl --context $(KUBECONTEXT_E2E) apply -f $(CERT_MANAGER_MANIFEST_URL) | grep -v unchanged
	kubectl --context $(KUBECONTEXT_E2E) -n cert-manager rollout status deploy/cert-manager --timeout=$(CERT_MANAGER_WAIT_TIMEOUT)
	kubectl --context $(KUBECONTEXT_E2E) -n cert-manager rollout status deploy/cert-manager-webhook --timeout=$(CERT_MANAGER_WAIT_TIMEOUT)
	kubectl --context $(KUBECONTEXT_E2E) -n cert-manager rollout status deploy/cert-manager-cainjector --timeout=$(CERT_MANAGER_WAIT_TIMEOUT)
	touch $@
```

If you ever need to force a re-install after a version bump, encode the version in the stamp name: `.stamps/cluster/.../cert-manager-$(CERT_MANAGER_VERSION).installed` and have the generic `cert-manager.installed` depend on that.

### 3 — Gitea

```make
$(CS)/gitea.installed: $(CS)/ready test/e2e/gitea-values.yaml
	mkdir -p $(CS)
	$(HELM) repo add gitea-charts https://dl.gitea.com/charts/ 2>/dev/null || true
	$(HELM) repo update gitea-charts
	kubectl --context $(KUBECONTEXT_E2E) create namespace $(GITEA_NAMESPACE) --dry-run=client -o yaml \
	  | kubectl --context $(KUBECONTEXT_E2E) apply -f -
	$(HELM) --kube-context $(KUBECONTEXT_E2E) upgrade --install gitea gitea-charts/gitea \
	  --namespace $(GITEA_NAMESPACE) \
	  --version $(GITEA_CHART_VERSION) \
	  --values test/e2e/gitea-values.yaml
	kubectl --context $(KUBECONTEXT_E2E) -n $(GITEA_NAMESPACE) rollout status deploy/gitea --timeout=300s
	touch $@
```

### 4 — Prometheus Operator

```make
$(CS)/prometheus.installed: $(CS)/ready test/e2e/scripts/ensure-prometheus-operator.sh
	mkdir -p $(CS)
	KUBECONTEXT=$(KUBECONTEXT_E2E) bash test/e2e/scripts/ensure-prometheus-operator.sh
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

### 6 — CRDs applied

```make
$(CS)/crds.applied: $(CS)/ready $(shell find config/crd -type f)
	mkdir -p $(CS)
	kubectl --context $(KUBECONTEXT_E2E) apply -k config/crd
	kubectl --context $(KUBECONTEXT_E2E) wait --for=condition=Established crd --all --timeout=120s
	touch $@
```

### 7 — Controller deployed

```make
DEPLOY_INPUTS := $(CS)/crds.applied $(CS)/cert-manager.installed $(IS)/controller.id \
                 $(shell find config/manager config/rbac config/default -type f 2>/dev/null)

$(CS)/controller.deployed: $(DEPLOY_INPUTS)
	mkdir -p $(CS)
	# Load the image into the cluster only if digest changed
	$(KIND) load docker-image $(E2E_LOCAL_IMAGE) --name $(KIND_CLUSTER_E2E)
	# Preventive webhook cleanup before deploy
	kubectl --context $(KUBECONTEXT_E2E) delete validatingwebhookconfiguration \
	  gitops-reverser-validating-webhook-configuration --ignore-not-found=true
	cd config && $(KUSTOMIZE) edit set image gitops-reverser=$(E2E_LOCAL_IMAGE)
	$(KUSTOMIZE) build config | kubectl --context $(KUBECONTEXT_E2E) apply -f -
	kubectl --context $(KUBECONTEXT_E2E) -n sut rollout status deploy/gitops-reverser --timeout=180s
	touch $@
```

### 8 — Port-forwards

`setup-port-forwards.sh` forks its own background `kubectl port-forward` processes and exits. The recipe uses the application health endpoints as a two-phase check: probe first, only (re)start if the probe fails, then probe again to confirm.

This target should stay a **file target, not `.PHONY`**. Making it `.PHONY` would force Make to run the recipe on every `make test-e2e` invocation regardless of whether the cluster or infra changed. The file target already handles the common case: if the stamp is newer than all prerequisites, Make skips the recipe entirely. The health-check-first pattern inside the recipe handles the less common case where the forwards are already alive from a previous run but the stamp needed refreshing.

```make
$(CS)/portforward.running: $(CS)/controller.deployed $(CS)/gitea.installed $(CS)/prometheus.installed
	mkdir -p $(CS)
	@# Fast path: if both endpoints already respond, the forwards are alive — skip the restart.
	if curl -fsS http://localhost:13000/api/healthz >/dev/null 2>&1 && \
	   curl -fsS http://localhost:19090/-/healthy    >/dev/null 2>&1; then \
	  echo "port-forwards already healthy, skipping restart"; \
	  touch $@; exit 0; \
	fi
	@# Slow path: (re)start the port-forwards and verify.
	bash test/e2e/scripts/setup-port-forwards.sh
	curl -fsS http://localhost:13000/api/healthz >/dev/null || { echo "Gitea health check failed";      exit 1; }
	curl -fsS http://localhost:19090/-/healthy    >/dev/null || { echo "Prometheus health check failed"; exit 1; }
	touch $@
```

The three execution paths this covers:

| Situation | What happens |
|---|---|
| Stamp newer than all prerequisites | Make skips the recipe entirely — zero curl calls |
| Stamp stale, but forwards still alive (e.g. controller redeployed, forwards survived) | Fast path: two curls pass → touch stamp → done |
| Stamp stale and forwards are dead | Slow path: restart script → two curls verify → touch stamp |

### 9 — E2E tests with result stamp

```make
$(CS)/e2e.passed: $(CS)/portforward.running $(shell find test/e2e -name '*.go')
	mkdir -p $(CS)
	KIND_CLUSTER=$(KIND_CLUSTER_E2E) PROJECT_IMAGE=$(E2E_LOCAL_IMAGE) \
	  go test ./test/e2e/ -v -ginkgo.v
	touch $@

.PHONY: test-e2e
test-e2e: $(CS)/e2e.passed
```

### 10 — Cluster teardown invalidates all stamps

```make
.PHONY: cleanup-cluster
cleanup-cluster:
	@if $(KIND) get clusters 2>/dev/null | grep -q "^$(KIND_CLUSTER_E2E)$$"; then \
	  $(KIND) delete cluster --name $(KIND_CLUSTER_E2E); \
	fi
	rm -rf $(CS)
```

---

## Making tests more targeted with Ginkgo focus flags

The entire e2e suite lives in one `Describe("Manager", ...)` block. You can run subsets without touching the test file by passing `--ginkgo.focus` from Make:

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

These are `.PHONY` (they always rerun the test binary) but they still benefit from the stamp-based infra: they won't rebuild the image or reinstall Gitea unless something actually changed. The `BeforeSuite`/`BeforeAll` hooks in the test file will still run, but that's unavoidable without splitting the suite.

Note: Ginkgo `-focus` filters skip `AfterAll` cleanup for skipped specs, so shared resources (like `gitprovider-normal` that multiple tests reuse) won't be cleaned up mid-suite when running a subset. That's fine for dev inner-loop runs.

---

## What to change in the test files

### `e2e_suite_test.go` — remove the duplicate setup

`BeforeSuite` currently calls `make setup-cluster e2e-build-load-image` when `PROJECT_IMAGE` is not set. With the new stamp-based flow, the Makefile target already ensures the cluster and image exist before `go test` runs. The `BeforeSuite` hook can be simplified or removed entirely:

```go
var _ = BeforeSuite(func() {
    if img := os.Getenv("PROJECT_IMAGE"); img == "" {
        // In local runs, the Makefile guarantees the cluster and image are ready
        // before go test is invoked. Nothing to do here.
        By("local run: cluster and image prepared by Makefile")
    } else {
        By(fmt.Sprintf("using pre-built image: %s", img))
    }
})
```

This prevents the double cluster-check and double image-build that currently happens on local runs.

### `e2e_test.go` — `BeforeAll` CRD and rollout

`BeforeAll` currently calls `make install` (CRD re-apply) and `kubectl rollout restart` unconditionally. With the stamp for `crds.applied` and `controller.deployed`, CRDs are already current by the time tests run. The `BeforeAll` block can drop those two commands and keep only:

- namespace creation / label enforcement
- SOPS age secret setup (genuinely per-run because it generates a fresh key)
- Gitea repository setup (genuinely per-run because the repo name is time-based)
- Prometheus client setup

---

## Migration approach

Rather than rewriting the Makefile in one go, you can migrate incrementally:

1. **Start with the image stamp.** This is the highest-value change: `docker build` is the most expensive step and runs even when no Go code changed. Add `.stamps/image/controller.id` and update `e2e-build-load-image` to depend on it.

2. **Add the cluster ready stamp.** `setup-cluster` already short-circuits if the cluster exists, but Make doesn't know that. Stamping it lets Make skip the shell check.

3. **Stamp cert-manager, Gitea, and Prometheus.** These three together account for most of the "cold" infrastructure time. Once stamped, a warm run skips all three.

4. **Stamp the controller deployment.** Depends on image stamp + CRD files. This is where the image digest trick pays off most: if Go code didn't change, the image stamp is unchanged, and the controller isn't redeployed.

5. **Add the `e2e.passed` stamp last.** Only do this once you're confident the earlier stamps are reliable; otherwise you might skip tests silently when the cluster has drifted.

---

## What stamps cannot protect you from

As noted in [docs/a-tree.md](docs/a-tree.md), stamps assume the world doesn't change behind Make's back. Specific drift scenarios you should know about:

- **Manual `kubectl delete` of a CRD or deployment**: the stamp still exists, so Make thinks it's fine. Fix: `make cleanup-cluster` to reset stamps, or add a pre-test verification step.
- **Kind cluster restart / node failure**: `$(CS)/ready` would still exist. The cluster-ready stamp should verify with a live `kubectl get ns`, not just presence of the stamp file.
- **Port-forward dies between test runs**: the `.portforward.running` stamp is stale. The `ss -ltn` check in the stamp recipe guards against this.
- **cert-manager version bump without touching `CERT_MANAGER_VERSION`**: the stamp won't invalidate. Encoding the version in the stamp path (`.../cert-manager-$(CERT_MANAGER_VERSION).installed`) prevents this.

---

## Summary

| What | Current | After stamps |
|---|---|---|
| `make test-e2e` with nothing changed | Rebuilds image, redeploys, reinstalls Gitea/cert-manager/prometheus | Runs tests only |
| Go code change | Rebuilds image, redeploys, reinstalls everything | Rebuilds image → redeploys → tests |
| Test file change only | Rebuilds image, redeploys, reinstalls everything | Tests only |
| CRD change | Rebuilds image, redeploys, reinstalls everything | Reapplies CRDs → redeploys → tests |
| Gitea config change | Reinstalls Gitea (among everything else) | Reinstalls Gitea → tests |
| `make test-e2e-gitprovider` | N/A (no such target today) | Runs GitProvider tests only |
