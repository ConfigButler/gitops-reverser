# Make Stamp Caching Analysis

## How It's Supposed to Work

The Makefile uses **stamp files** under `.stamps/cluster/<CTX>/` to cache expensive operations. A stamp file is a file whose *modification time* (mtime) is used by Make as a proxy for "this work is done". When all of a target's prerequisites are older than its stamp, Make skips the recipe — the work is cached.

### The full `prepare-e2e` dependency graph

```
make prepare-e2e
├── portforward-ensure          (PHONY — always runs setup-port-forwards.sh)
│   └── $(CS)/services.ready    (stamp — should be cached)
│       ├── $(CS)/flux-setup.ready
│       │   ├── $(CS)/flux.installed
│       │   │   └── $(CS)/ready            ← cluster stamp
│       │   │       ├── start-cluster.sh
│       │   │       ├── $(AUDIT_POLICY_PATH)
│       │   │       └── $(AUDIT_WEBHOOK_CONFIG_PATH)   ← ⚠️ BUG 1
│       │   └── files in test/e2e/setup/flux/
│       └── yaml files in test/e2e/setup/manifests/
│
└── $(CS)/$(NAMESPACE)/prepare-e2e.ready   (stamp — should be cached)
    ├── $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/install.yaml
    │   ├── $(CS)/services.ready
    │   ├── $(MANIFEST_OUTPUTS)             ← ⚠️ BUG 2
    │   └── config/ directory files
    ├── $(CS)/image.loaded
    │   ├── $(IS)/project-image.ready
    │   └── $(CS)/ready
    ├── $(CS)/$(NAMESPACE)/controller.deployed
    ├── $(CS)/$(NAMESPACE)/webhook-tls.ready  → runs inject-webhook-tls.sh ← ⚠️ BUG 1 source
    └── $(CS)/$(NAMESPACE)/sops-secret.applied
        └── $(CS)/$(NAMESPACE)/sops-secret.yaml
            └── $(CS)/age-key.txt
                └── Makefile                ← ⚠️ BUG 3
```

The `portforward-ensure` script (`hack/e2e/setup-port-forwards.sh`) has a **fast path**: if all three port-forwards are already healthy, it exits immediately. So on an ideal second run, `prepare-e2e` should complete in ~1 second.

---

## Bug 1: `inject-webhook-tls.sh` creates a feedback loop through `$(CS)/ready`

**What happens:**  
`$(CS)/$(NAMESPACE)/webhook-tls.ready` depends on `controller.deployed` and runs `inject-webhook-tls.sh`. That script writes a new `$(AUDIT_WEBHOOK_CONFIG_PATH)` (`.stamps/cluster/.../audit/webhook-config.yaml`) with actual TLS certificates, then restarts the k3d server node.

**The problem:**  
`$(CS)/ready` lists `$(AUDIT_WEBHOOK_CONFIG_PATH)` as a **normal prerequisite**:

```makefile
$(CS)/ready: test/e2e/cluster/start-cluster.sh $(AUDIT_POLICY_PATH) $(AUDIT_WEBHOOK_CONFIG_PATH) | $(CS)
```

So every time `inject-webhook-tls.sh` runs (i.e., every time `webhook-tls.ready` is rebuilt), it updates `webhook-config.yaml`'s mtime. On the **next** Make invocation:

1. `webhook-config.yaml` (just written) is **newer** than `$(CS)/ready`
2. `$(CS)/ready` is rebuilt — runs `start-cluster.sh` (cluster restart!) and `touch $@`
3. `$(CS)/ready` is now newer than `flux.installed` → `flux.installed` rebuilds (flux re-installs!)
4. `flux.installed` is newer than `flux-setup.ready` → `flux-setup.ready` rebuilds (slow Flux wait!)
5. `flux-setup.ready` is newer than `services.ready` → **`services.ready` rebuilds** (applies manifests + CRD wait)
6. The entire `prepare-e2e.ready` chain also rebuilds

This is the **primary cause** of `services.ready` not preventing a second run — it is being invalidated by the `ready` stamp being refreshed after every full `prepare-e2e` execution.

**Current observed state:**  
The current `.stamps/` shows this is mid-cycle:
- `audit/webhook-config.yaml`: Unix 1776062649 (written by inject script)
- `$(CS)/ready`: Unix 1776062686 (37 s later — `ready` was already rebuilt once)
- `image.loaded`: Apr 13 06:23 (from before the last `ready` rebuild → currently outdated)

**Fix:**  
Make `$(AUDIT_WEBHOOK_CONFIG_PATH)` an **order-only** prerequisite of `$(CS)/ready` (it must exist when `start-cluster.sh` runs, but its content being updated by TLS injection should not invalidate the cluster stamp). Use the **source** files as the real change triggers instead:

```makefile
$(CS)/ready: test/e2e/cluster/start-cluster.sh \
             $(AUDIT_POLICY_SOURCE) \
             $(AUDIT_WEBHOOK_BOOTSTRAP_SOURCE) \
             | $(AUDIT_POLICY_PATH) $(AUDIT_WEBHOOK_CONFIG_PATH) $(CS)
    export CLUSTER_NAME=$(CLUSTER_NAME)
    export AUDIT_DIR_REL=$(AUDIT_ASSET_DIR)
    bash test/e2e/cluster/start-cluster.sh
    kubectl --context $(CTX) get ns >/dev/null
    touch $@
```

Now `inject-webhook-tls.sh` can freely update `webhook-config.yaml` without ever invalidating the cluster-ready stamp.

---

## Bug 2: `config/webhook/manifests.yaml` is never created — manifests always rebuild

**What happens:**  
`MANIFEST_OUTPUTS` includes `config/webhook/manifests.yaml`:

```makefile
MANIFEST_OUTPUTS := $(CRD_BASE_OUTPUTS) \
    config/rbac/role.yaml \
    config/webhook/manifests.yaml
```

The grouped-target recipe deletes all outputs with `rm -f`, then runs `controller-gen`:

```makefile
$(MANIFEST_OUTPUTS) &: $$(MANIFEST_INPUTS)
    @rm -f config/crd/bases/*.yaml config/rbac/role.yaml config/webhook/manifests.yaml
    $(CONTROLLER_GEN) $(CONTROLLER_GEN_ARGS)
```

There are **no `//+kubebuilder:webhook` markers** in the Go source. `controller-gen` therefore never creates `config/webhook/manifests.yaml`. After the recipe runs, this file is still missing. Make sees a missing output and **always** considers `MANIFEST_OUTPUTS` outdated.

**Cascade:**  
Manifests rebuild → `config-dir/install.yaml` (depends on `MANIFEST_OUTPUTS`) is outdated → `DO_CLEANUP_INSTALLS` runs, deleting all of `.stamps/.../gitops-reverser/` → `controller.deployed`, `webhook-tls.ready`, `sops-secret.applied`, and `prepare-e2e.ready` are all deleted → everything under `prepare-e2e.ready` must be rebuilt.

**Confirmed by `make -n prepare-e2e`:**  
The dry run immediately shows `rm -f config/crd/bases/...` and `controller-gen` running before anything else.

**Fix:**  
Ensure all declared outputs exist after the recipe. Add a `touch` for missing outputs:

```makefile
$(MANIFEST_OUTPUTS) &: $$(MANIFEST_INPUTS)
    @rm -f config/crd/bases/*.yaml config/rbac/role.yaml config/webhook/manifests.yaml
    $(CONTROLLER_GEN) $(CONTROLLER_GEN_ARGS)
    @touch $(MANIFEST_OUTPUTS)
```

The `touch` is a no-op for files controller-gen already created; it creates an empty file for any declared output that wasn't generated (e.g., `webhook/manifests.yaml` when no webhooks exist). This keeps the grouped target's mtime stable across runs.

---

## Bug 3: `Makefile` as a dependency of `age-key.txt` and `sops-secret.yaml`

**What happens:**

```makefile
$(CS)/age-key.txt: Makefile test/e2e/tools/gen-age-key/main.go | $(CS)
$(CS)/$(NAMESPACE)/sops-secret.yaml: $(CS)/age-key.txt Makefile | $(CS)/$(NAMESPACE)
```

Every time the Makefile is edited (e.g., any development work on the build system), both targets' prerequisites are updated. The `gen-age-key` tool's `loadOrGenerateIdentity` function does **read** the existing key rather than regenerating it — so `age-key.txt`'s content is stable. However, `gen-age-key` **always overwrites** `sops-secret.yaml` unconditionally (no compare-and-replace). So:

1. Makefile edited → `age-key.txt` recipe runs (key content unchanged, but recipe runs)
2. `sops-secret.yaml` is now outdated (its dep `age-key.txt` was just run)
3. `sops-secret.yaml` recipe runs → **always writes** `sops-secret.yaml` with new mtime
4. `sops-secret.applied` is outdated → `kubectl apply` runs again

**Current observed state:**  
`Makefile` mtime is Apr 13 07:14; `age-key.txt` mtime is Apr 13 06:23. Makefile is 51 minutes newer — both targets will rebuild on next `make prepare-e2e`.

**Fix:**  
Remove `Makefile` from both targets. The Go tool source (`gen-age-key/main.go`) is the correct trigger:

```makefile
$(CS)/age-key.txt: test/e2e/tools/gen-age-key/main.go | $(CS)
    go run ./test/e2e/tools/gen-age-key \
        --key-file $@

$(CS)/$(NAMESPACE)/sops-secret.yaml: $(CS)/age-key.txt test/e2e/tools/gen-age-key/main.go | $(CS)/$(NAMESPACE)
    tmp=$@.tmp
    go run ./test/e2e/tools/gen-age-key \
        --key-file $(CS)/age-key.txt \
        --secret-file $$tmp \
        --namespace $(NAMESPACE) \
        --secret-name sops-age-key
    if [ -f "$@" ] && cmp -s "$$tmp" "$@"; then rm -f "$$tmp"; else mv "$$tmp" "$@"; fi
```

The compare-and-replace pattern on `sops-secret.yaml` keeps its mtime stable when the key hasn't changed (same pattern already used by `install-config-dir.sh`).

---

## Summary

| Bug | Trigger | Cascade | Fix |
|-----|---------|---------|-----|
| 1: inject-webhook-tls.sh updates `audit/webhook-config.yaml` which is a normal dep of `$(CS)/ready` | Every `prepare-e2e` run | `ready` → `flux.installed` → `flux-setup.ready` → `services.ready` → full rebuild | Make `$(AUDIT_WEBHOOK_CONFIG_PATH)` order-only in `$(CS)/ready`; use source files as real deps |
| 2: `config/webhook/manifests.yaml` never created by controller-gen | Every `make` invocation | Manifests regen → install.yaml → cleanup deletes `gitops-reverser/` stamps → full redeploy | `touch $(MANIFEST_OUTPUTS)` at end of manifests recipe |
| 3: `Makefile` listed as dep of `age-key.txt` and `sops-secret.yaml` | Every Makefile edit | sops-secret rebuilt → `kubectl apply` re-runs | Remove `Makefile` dep; add compare-and-replace for `sops-secret.yaml` |

After fixing all three, a second `make prepare-e2e` with no source changes should only run `setup-port-forwards.sh` (fast path exits in <1 s if port-forwards are healthy).
