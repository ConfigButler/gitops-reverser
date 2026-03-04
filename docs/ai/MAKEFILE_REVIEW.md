# Makefile & E2E Infrastructure Review

> Reviewed against the stated design principles:
> 1. On test abortion, nothing is deleted — state is preserved for investigation
> 2. Cluster, install, and image are all reused; only changed parts are rebuilt
> 3. E2E environment fully mimics real user usage

---

## Summary

The Makefile is well-structured for a Kubernetes operator project. The stamp-based
incremental build system is a sound architectural decision. The issues below range from
**bugs that can cause silent test failures** to **organisational improvements** and
**style consistency**.

---

## Bugs / Correctness Issues

### 1. `GO_SOURCES` missing `api/` — image not rebuilt on type changes (HIGH)

```makefile
# current
GO_SOURCES := $(shell find cmd internal -type f -name '*.go') go.mod go.sum
```

The `api/v1alpha1/` package contains six source files that are compiled into the
controller binary. If a CRD type changes (e.g. a new field on `WatchRule`), the
`$(IS)/controller.id` stamp will **not** be invalidated and the e2e cluster will run
with a stale image. Fix:

```makefile
GO_SOURCES := $(shell find cmd internal api -type f -name '*.go' \
    ! -name '*_test.go' ! -name 'zz_generated.deepcopy.go') \
    go.mod go.sum
```

---

### 2. Implicit ordering dependency between `sops-secret.applied` and namespace creation (MEDIUM)

```makefile
$(CS)/$(NAMESPACE)/prepare-e2e.ready: \
    $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/install.yaml \   # creates namespace
    $(CS)/image.loaded \
    $(CS)/$(NAMESPACE)/controller.deployed \
    $(CS)/$(NAMESPACE)/sops-secret.applied              # requires namespace to exist
```

`sops-secret.applied` only declares a dependency on `sops-secret.yaml`, not on
`install.yaml` which creates the namespace. GNU Make without `-j` happens to process
prerequisites left-to-right in practice, but this is an implementation detail and not
a guarantee. With `make -j2` this is a definite race.

Fix — add the explicit prerequisite:

```makefile
$(CS)/$(NAMESPACE)/sops-secret.applied: \
    $(CS)/$(NAMESPACE)/sops-secret.yaml \
    $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/install.yaml
    kubectl --context $(CTX) apply -f $(CS)/$(NAMESPACE)/sops-secret.yaml
    touch $@
```

---

### 3. Gitea bootstrap stamp recipe duplicated verbatim (LOW)

Both `$(CS)/gitea/bootstrap/api.ready` and
`$(CS)/gitea/bootstrap/org-$(GITEA_ORG_NAME).ready` contain identical recipe bodies
(same script invocation, same env vars). The script is idempotent, so the only effect
is running it twice when both stamps are stale. More importantly, the stamps are
**written by the script itself**, not by the Makefile recipe — and the Makefile lacks
a `touch $@` safety net. If the script succeeds but doesn't write the stamp (e.g. a
future code path), the target will re-run every time.

Consider:
```makefile
$(CS)/gitea/bootstrap/api.ready \
$(CS)/gitea/bootstrap/org-$(GITEA_ORG_NAME).ready &: \
    $(CS)/$(NAMESPACE)/prepare-e2e.ready \
    test/e2e/scripts/gitea-bootstrap.sh
    mkdir -p $(@D)
    BOOTSTRAP_DIR=$(CS)/gitea/bootstrap \
      API_URL=http://localhost:13000/api/v1 \
      GITEA_ADMIN_USER=$(GITEA_ADMIN_USER) \
      GITEA_ADMIN_PASS=$(GITEA_ADMIN_PASS) \
      ORG_NAME=$(GITEA_ORG_NAME) \
      bash test/e2e/scripts/gitea-bootstrap.sh
    @test -f $(CS)/gitea/bootstrap/api.ready
    @test -f $(CS)/gitea/bootstrap/org-$(GITEA_ORG_NAME).ready
```

---

## Design / Architecture Issues

### 4. Variable definitions far from first use (MEDIUM)

`CONTROLLER_CONTAINER`, `CONTROLLER_DEPLOY_SELECTOR`, and `VALID_INSTALL_MODES` are
defined at lines 426–433, well after the recipes that use them (line 398+). While Make
expands recipe variables lazily so this technically works, it makes the file harder to
reason about and edit. Move all configuration knobs to the top of the file, grouped
with the other defaults.

```makefile
# near the top, with other E2E configuration
CONTROLLER_CONTAINER ?= manager
CONTROLLER_DEPLOY_SELECTOR ?= app.kubernetes.io/part-of=gitops-reverser
VALID_INSTALL_MODES := config-dir helm plain-manifests-file
```

---

### 5. `setup-envtest` is always-run phony (LOW)

```makefile
.PHONY: setup-envtest
setup-envtest:
    @$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(shell pwd)/bin -p path ...
```

Every `make test` call invokes `envtest use`, even when the binaries are already
present for the right version. Stamp it:

```makefile
ENVTEST_STAMP := .stamps/envtest-$(ENVTEST_K8S_VERSION).ready

.PHONY: setup-envtest
setup-envtest: $(ENVTEST_STAMP)

$(ENVTEST_STAMP): Makefile
    @echo "Setting up envtest $(ENVTEST_K8S_VERSION)..."
    @mkdir -p $(shell pwd)/bin
    @$(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(shell pwd)/bin -p path
    @mkdir -p $(@D) && touch $@
```

Add `$(ENVTEST_STAMP)` to the `clean` recipe.

---

### 6. Helm CRD-skip logic is a complex inline script (LOW)

The `$(CS)/$(NAMESPACE)/helm/install.yaml` recipe embeds a multi-line bash conditional
to check whether CRDs already exist and add `--skip-crds` accordingly. This is the
right intent (idempotent install) but hard to read and test in isolation.

Consider extracting it to `hack/helm-upgrade.sh` which accepts the same parameters —
this would also make it testable and reusable for manual operator deployments.

---

### 7. GNU Make 4.3+ grouped target (`&:`) is undocumented (LOW)

```makefile
$(MANIFEST_OUTPUTS) &: $(MANIFEST_INPUTS)
$(HELM_SYNC_OUTPUTS) &: $(MANIFEST_OUTPUTS)
```

The `&:` grouped target syntax requires GNU Make ≥ 4.3 (released 2020). This is fine
for the current devcontainer (GNU Make 4.3 is installed), but add a version check or a
comment near the top so future contributors aren't surprised:

```makefile
# Requires GNU Make >= 4.3 (for grouped-target &: syntax)
# Verify: make --version
```

---

### 8. `dist/install.yaml` target missing from `.PHONY` guard but tracked by `helm-sync` (LOW)

`dist/install.yaml` depends on `$(HELM_SYNC_OUTPUTS)` which is a grouped stamp target.
If `dist/install.yaml` is stale but `$(HELM_SYNC_OUTPUTS)` stamps are fresh, Make will
correctly regenerate it. This is fine. However, `helm-sync` is marked `.PHONY` which
means it **always** runs, but its stamp outputs are real files — the phony attribute
causes Make to always consider them out-of-date. The `.PHONY: helm-sync` should be
removed because `helm-sync` is a stamp target, not a command:

```makefile
# Remove this line:
# .PHONY: helm-sync
helm-sync: $(HELM_SYNC_OUTPUTS) ## Sync CRDs and roles from config/crd/bases to Helm chart
```

Similarly audit `generate` — it is also declared `.PHONY` but delegates to a real
output file.

---

## Script Organisation

### 9. `test/e2e/scripts/` vs `hack/`

The four scripts in `test/e2e/scripts/` are **cluster infrastructure** scripts, not
test code:

| Script | Role |
|---|---|
| `ensure-prometheus-operator.sh` | Installs Prometheus Operator |
| `gitea-bootstrap.sh` | Creates Gitea org (cluster-scoped) |
| `gitea-run-setup.sh` | Creates repos, SSH keys, k8s Secrets |
| `setup-port-forwards.sh` | Manages port-forwards |

The Go community convention (`kubebuilder`, `controller-runtime`, `cluster-api` all
follow this) is:

- `test/` — Go test files, test fixtures, test data
- `hack/` — Scripts that support development, CI, and test *infrastructure*

The user asked about this directly. Suggested layout:

```
hack/
├── boilerplate.go.txt           (existing)
├── cleanup-installs.sh          (existing)
├── e2e/
│   ├── ensure-prometheus-operator.sh
│   ├── gitea-bootstrap.sh
│   ├── gitea-run-setup.sh
│   └── setup-port-forwards.sh
test/e2e/
├── cluster/                     (keep — cluster config is test fixture data)
├── setup/                       (keep — prometheus manifests are fixtures)
├── templates/                   (keep)
├── tools/                       (keep — Go tool, not a script)
├── gitea-values.yaml            (keep — Helm values fixture)
└── *.go                         (keep)
```

Makefile references update from `test/e2e/scripts/X.sh` to `hack/e2e/X.sh`.

This also makes the MANIFEST_INPUTS exclusion list slightly cleaner — scripts in
`hack/` are naturally excluded from `find api internal cmd -type f -name '*.go'`.

---

## Style / Maintainability

### 10. Category comments (`##@`) are mis-ordered

The current order:
1. General
2. Development (manifests, generate, fmt, vet, test, lint)
3. E2E Gitea Setup
4. Build
5. Dependencies
6. E2E Stamp Targets
7. Deployments
8. Cleanup

A more logical flow for a reader:

1. General (help)
2. Development (fmt, vet, generate, manifests, lint)
3. Build (binary + docker)
4. Test (unit + e2e)
5. E2E Infrastructure (cluster, services, gitea, install modes)
6. Dependencies / Tool setup
7. Cleanup

---

### 11. Port numbers are hardcoded in `clean-port-forwards` (LOW)

```makefile
@-pkill -f "kubectl.*port-forward.*13000" 2>/dev/null || true
@-pkill -f "kubectl.*port-forward.*19090" 2>/dev/null || true
```

These numbers also appear in `setup-port-forwards.sh` and `gitea-values.yaml`. Define
Makefile variables:

```makefile
GITEA_PORT ?= 13000
PROMETHEUS_PORT ?= 19090
```

and use them in `clean-port-forwards` and pass them to `setup-port-forwards.sh`.

---

### 12. `$(CS)/$(NAMESPACE)/controller.deployed` uses deprecated `--record` flag explicitly (COSMETIC)

```makefile
kubectl ... set image "deployment/$$deploy" "$$c=$$img" --record=false
```

`--record=false` is the default. The flag itself was deprecated in Kubernetes 1.22.
Remove it — it adds noise and will eventually become an error.

---

### 13. Inconsistent shell quoting in Makefile recipes (LOW)

Some recipes quote `$(CTX)` and `$(NAMESPACE)`, others don't. While these values are
unlikely to contain spaces in practice, consistent quoting is safer and expresses
intent:

```makefile
kubectl --context "$(CTX)" -n "$(NAMESPACE)" ...
```

---

### 14. `$(CS)/$(NAMESPACE)/repo/repo.ready` is a no-op stamp (COSMETIC)

```makefile
$(CS)/$(NAMESPACE)/repo/repo.ready: $(CS)/$(NAMESPACE)/repo/checkout.ready
    @test -f $@
```

This target does nothing except verify the stamp was written by the script. It's never
depended on by any other target (only `checkout.ready` is used downstream). Remove it
or fold it into `checkout.ready`.

---

## What Is Working Well

- **Stamp-based incremental system** — The three-tier stamp model
  (`.stamps/image/`, `.stamps/cluster/<ctx>/`, per-namespace) is well-designed and
  correctly implements "reuse what hasn't changed".

- **`CTX` parameterisation** — Full support for targeting different clusters via `CTX=`
  is clean and consistent throughout. The `$(patsubst ...)` for deriving cluster name
  from context is a nice touch.

- **`DO_CLEANUP_INSTALLS` macro** — Running cleanup before each install ensures tests
  start from a known clean state without destroying the cluster or Gitea (expensive
  operations). This directly implements design principle #1.

- **`$(MANIFEST_OUTPUTS) &:` grouped targets** — Correct use of grouped targets to
  express that `controller-gen` writes multiple outputs atomically.

- **Config-dir install copies to tmpdir** — Using a tmpdir to avoid mutating the
  working tree's `config/` during `kustomize edit set namespace/image` is correct and
  safe.

- **`$(IS)/project-image.ready` handles both local and provided images** — The
  `PROJECT_IMAGE_PROVIDED` flag cleanly separates the "CI provides an image" path from
  the "local build" path.

- **`test -f $@` at end of script-delegated stamps** — Good defensive check that the
  script actually wrote the marker.

- **`.stamps/*` in `.gitignore`** — Correct.

---

## Recommended Action Order

| Priority | Item | Effort |
|---|---|---|
| High | Fix `GO_SOURCES` to include `api/` | 1 line |
| High | Add install.yaml dependency to `sops-secret.applied` | 2 lines |
| Medium | Move `test/e2e/scripts/` → `hack/e2e/` | File move + Makefile refs |
| Medium | Move late variable definitions to top | Reorganise |
| Low | Stamp `setup-envtest` | ~8 lines |
| Low | Remove `.PHONY: helm-sync` and `.PHONY: generate` | 2 lines |
| Low | Define `GITEA_PORT`/`PROMETHEUS_PORT` variables | 2 lines |
| Low | Drop `--record=false` from `kubectl set image` | 1 line |
| Low | Add GNU Make 4.3 version comment | 1 line |
| Low | Remove `repo.ready` no-op stamp | 3 lines |
