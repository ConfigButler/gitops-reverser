# E2E Namespace Label Cleanup — Investigation & Analysis

## The Problem

Running the following sequence fails intermittently:

```bash
make cleanup-cluster
make test-e2e                    # INSTALL_MODE=config-dir (default)
make test-e2e-quickstart-helm    # INSTALL_MODE=helm
```

The failure: when switching from `config-dir` to `helm`, the cleanup step cannot find the
`gitops-reverser` namespace to delete it. The namespace has no `e2e=true` label, so the label
selector in `cleanup-e2e-installs.done` finds nothing and skips deletion. Helm then tries to
install into an already-provisioned namespace, causing a conflict.

---

## Makefile Architecture (Stamp Files)

The Makefile uses stamp files (empty files whose existence/mtime tracks build state) to express
a dependency graph. The relevant chain for switching install modes:

```
$(CS)/$(NAMESPACE)/$(INSTALL_MODE)/cleanup-e2e-installs.done
  └─ deletes all "e2e=true" namespaces in the cluster; removes their stamp dirs
$(CS)/$(NAMESPACE)/$(INSTALL_MODE)/install.done
  └─ installs controller into namespace (creates namespace if needed)
$(CS)/$(NAMESPACE)/label-namespace.ready
  └─ kubectl label --overwrite ns $(NAMESPACE) e2e=true ...
$(CS)/$(NAMESPACE)/sops-secret.applied
$(CS)/$(NAMESPACE)/controller.deployed
$(CS)/$(NAMESPACE)/prepare-e2e.ready
```

Where `$(CS) = .stamps/cluster/$(CTX)` and `$(INSTALL_MODE)` is one of `helm`, `config-dir`,
or `plain-manifests-file`.

### Why this should work

`label-namespace.ready` depends on `$(CS)/$(NAMESPACE)/$(INSTALL_MODE)/install.done`. When
`INSTALL_MODE` changes (e.g. config-dir → helm):

- `helm/install.done` does **not** exist
- `label-namespace.ready` is therefore **out-of-date** (its prerequisite is missing)
- GNU Make rebuilds it **after** `helm/install.done` is built
- `helm/install.done` depends on `helm/cleanup-e2e-installs.done` (which runs first)

So the intended order is: **cleanup → install → label**.

---

## Investigation Findings

### 1. GNU Make correctly handles deleted prerequisites

When `cleanup-e2e-installs.done` runs, it executes:
```bash
rm -rf ".stamps/cluster/$ctx/$ns"
```
This deletes **all** stamps under `gitops-reverser/`, including `label-namespace.ready` and
`sops-secret.yaml`. These files were considered up-to-date at **plan time** (before any recipes
ran) but are gone when their recipe would execute.

**Empirically verified**: GNU Make evaluates the dependency graph once at the start, marks
targets for rebuild, then executes in order. When a prerequisite is deleted mid-build by another
recipe, Make correctly re-creates it — it does not skip it just because its own prerequisites
haven't changed since plan time.

### 2. Dry-run output shows correct ordering

Running `make -n prepare-e2e INSTALL_MODE=helm` (with config-dir stamps present) produces:

```
# helm/cleanup-e2e-installs.done  ← deletes gitops-reverser namespace + stamps
# helm/install.done                ← helm installs, creates fresh namespace
# label-namespace.ready            ← kubectl label e2e=true applied
# sops-secret.applied              ← secret applied into namespace
# prepare-e2e.ready
```

The logical order is correct per the dry-run.

### 3. `sops-secret.yaml` is NOT in the dry-run plan

At plan time, `sops-secret.yaml` is considered up-to-date (its deps `age-key.txt` and `Makefile`
are unchanged). But the cleanup deletes it. GNU Make re-creates it correctly because
`sops-secret.applied` depends on it and is in the rebuild plan.

Additionally, `gen-age-key` calls `os.MkdirAll` before writing, so it can recreate the file
even after the stamp directory was removed.

### 4. The `config/namespace.yaml` does not include `e2e=true`

```yaml
# config/namespace.yaml
metadata:
  labels:
    app.kubernetes.io/managed-by: kustomize
    app.kubernetes.io/name: gitops-reverser
    control-plane: gitops-reverser
    pod-security.kubernetes.io/enforce: restricted
  name: sut   # replaced by kustomize to the actual NAMESPACE
```

When `config-dir/install.done` runs `kubectl apply` (via kustomize), it sets
`kubectl.kubernetes.io/last-applied-configuration` on the namespace **without** `e2e=true`.

The `e2e=true` label is only applied by the **subsequent** `label-namespace.ready` step via
`kubectl label --overwrite`. Client-side apply preserves externally-added labels (they are not in
the `last-applied-configuration` so `kubectl apply` does not remove them). However, this relies
on `label-namespace.ready` always running **after** any `kubectl apply` that touches the namespace.

### 5. `setup-gitea.sh` creates the namespace without `e2e=true`

In `test/e2e/scripts/setup-gitea.sh`, the `setup_credentials()` function does:
```bash
TARGET_NAMESPACE=${TARGET_NAMESPACE:-${SUT_NAMESPACE:-${QUICKSTART_NAMESPACE:-${NAMESPACE:-sut}}}}
kubectl create namespace "$TARGET_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -
```

During `make test-e2e` (INSTALL_MODE=config-dir, NAMESPACE=gitops-reverser):
- `NAMESPACE=gitops-reverser` is in the environment
- `SUT_NAMESPACE` and `QUICKSTART_NAMESPACE` are **not** set
- So `TARGET_NAMESPACE=gitops-reverser`
- `kubectl apply` is called **without `--context`**, using the default kubeconfig context

This `kubectl apply` on the namespace spec has **no `e2e=true` label**. Whether this strips
the label depends on exact timing (before or after `label-namespace.ready` ran) and on
server-side vs client-side apply semantics.

During `make test-e2e-quickstart-helm`, the quickstart framework sets `SUT_NAMESPACE=sut` in
the subprocess environment before calling `setup-gitea.sh`, so `TARGET_NAMESPACE=sut` — the
`gitops-reverser` namespace is not touched here.

### 6. Potential server-side apply conflict

If the namespace was first created by `kubectl apply` (kustomize) and later `helm install
--create-namespace` tries to manage the same namespace object via server-side apply, there
can be field ownership conflicts. Helm uses SSA which might overwrite or drop labels managed
by other field managers.

---

## Unresolved Root Cause

Despite the above investigation, the **exact mechanism** causing `e2e=true` to be absent when
`helm/cleanup-e2e-installs.done` runs has not been definitively identified without a live
execution trace. The strongest candidates are:

| Hypothesis | Likelihood | How to verify |
|---|---|---|
| `setup-gitea.sh` applies namespace without `e2e=true` before `label-namespace.ready` runs | Medium | Add `--context` to every kubectl in setup-gitea.sh; check timing in logs |
| Helm SSA strips labels when reinstalling without deletion first | Medium | Check if cleanup actually deleted the namespace before helm install |
| `label-namespace.ready` stamp survives but Kubernetes label was somehow reverted | Low | Add a check in `prepare-e2e.ready` recipe that verifies the label |
| INSTALL_MODE variable not correctly propagated to child make | Low | Print INSTALL_MODE in cleanup recipe |

---

## The User's Pragmatic Notes

From `notes-whats-going-wrnog.txt`:

> Perhaps I should go a bit smarter about this: or more pragmatic. I could also have the same
> namespace, always, for now. And just make sure that I have a task that always first deletes the
> old namespace that would also prevent me from sitting in this rabbit hole.

This is a sound alternative. Instead of relying on the `e2e=true` label to find-and-delete the
namespace, the cleanup target could unconditionally delete the **known** namespace (`gitops-reverser`)
regardless of labels. The label mechanism adds indirection that is fragile when cluster state and
stamp state diverge.

Other notes the user captured:
- Move Gitea installation to a higher (cluster-level) stamp, similar to how `age-key.txt` moved
  to cluster level — Gitea is cluster-wide infrastructure, not per-namespace
- Move Gitea credential creation (`setup-gitea.sh` secret creation) to a higher level stamp so it
  runs once, not per test invocation
- Restructure e2e tests for better parallelism (long-term goal)

---

## Recommended Next Step

Replace the label-based namespace discovery with a **direct, unconditional namespace delete** in
`cleanup-e2e-installs.done`. The current target already knows `$(NAMESPACE)` — use it directly:

```makefile
# Instead of: find namespaces by e2e=true label, then delete
# Do: unconditionally delete the known namespace
$(CS)/$(NAMESPACE)/$(INSTALL_MODE)/cleanup-e2e-installs.done: Makefile $(CS)/ready
    @set -euo pipefail; \
    ctx="$(CTX)"; ns="$(NAMESPACE)"; \
    if kubectl --context "$$ctx" get ns "$$ns" >/dev/null 2>&1; then \
        echo "🧹 Deleting namespace '$$ns' in '$$ctx'"; \
        kubectl --context "$$ctx" delete ns "$$ns" --ignore-not-found=true; \
        kubectl --context "$$ctx" wait --for=delete "ns/$$ns" --timeout=300s || true; \
        rm -rf ".stamps/cluster/$$ctx/$$ns" 2>/dev/null || true; \
    else \
        echo "ℹ️ Namespace '$$ns' does not exist in '$$ctx'"; \
    fi; \
    ...
```

**Trade-off**: loses the ability to detect "orphaned" namespaces from a previous run with a
*different* `NAMESPACE` value. But since `NAMESPACE` defaults to `gitops-reverser` and that
rarely changes, this is an acceptable trade-off for reliability.

The `e2e=true` label can be kept for informational / debugging purposes without being load-bearing
for the cleanup logic.
