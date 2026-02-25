# E2E “second run” failure at `test/e2e/e2e_test.go:598` — investigation summary (2026-02-25)

## Symptom you reported
- `make test-e2e` passes on a **fresh** Kind cluster / first run.
- Running `make test-e2e` (or `make test-e2e-encryption`) **again** in the same devcontainer (same cluster still running) fails reproducibly at:
  - `test/e2e/e2e_test.go:598`
  - inside `It("should commit encrypted Secret manifests when WatchRule includes secrets", ...)`

## Latest findings (added 2026-02-25)
### Controller restart test (proves “pod lifetime” involvement)
We ran this sequence (same cluster, no cluster recreation):
1. `make test-e2e-encryption` (after a clean setup) → **PASS**
2. Restart controller only: `kubectl -n sut rollout restart deploy/gitops-reverser` → wait for rollout
3. `make test-e2e-encryption` again → **PASS**

This strongly suggests the failure is tied to **controller pod lifetime state** (either in-memory state and/or container-local disk such as `/tmp`).

### Deleting `/tmp/gitops-reverser-workers` inside the running pod (do not use as a workaround)
We attempted to discriminate “memory vs disk” by deleting the controller’s on-disk git cache while the controller stayed running:
- `rm -rf /tmp/gitops-reverser-workers` inside the controller pod

Result: the next `make test-e2e-encryption` failed earlier in the same spec at line 598, but now because:
- `.sops.yaml` was **missing** in the test checkout:
  - `.../e2e/secret-encryption-test/.sops.yaml: no such file or directory`
- controller logs showed repeated:
  - `Failed to write event ... failed to open repository: repository does not exist`

This is still useful evidence:
- the controller assumes the on-disk repo cache exists and does not reliably recreate it if removed
- deleting the directory mid-run introduces a new failure mode and is **not** a stable “cleanup” strategy

### Decision: remove focused e2e selectors
We will remove the “focused” e2e Make targets/selectors (e.g. `-ginkgo.focus=...`) because the suite relies on behaviors/state that these focused runs accidentally depend on or interfere with (especially with ordered containers and shared resources). Running subsets has been confusing and can produce non-representative results compared to full `make test-e2e`.

## What line 598 is actually doing
At `test/e2e/e2e_test.go:598`, the test does:

1. Create `GitTarget` with path `e2e/secret-encryption-test` and a `WatchRule`.
2. Create + patch Secret `sut/test-secret-encryption` to force an update event.
3. `Eventually(...)`:
   - `git pull` in the test checkout dir (e.g. `/tmp/gitops-reverser/e2e-test-416880`).
   - Read the committed file:
     - `e2e/secret-encryption-test/v1/secrets/sut/test-secret-encryption.sops.yaml`
   - Assert:
     - it contains `sops:`
     - it does **not** contain plaintext
   - Read `.sops.yaml` at:
     - `e2e/secret-encryption-test/.sops.yaml`
   - Derive an age recipient from the local private key file:
     - `/tmp/e2e-age-key.txt`
   - Assert `.sops.yaml` contains that derived recipient.
   - Decrypt the committed secret YAML by exec’ing `sops --decrypt` **inside the controller pod**, passing the age private key.
   - If decryption fails, the Eventually fails at line 598.

So line 598 is really “**repo contains an encrypted secret that is decryptable using the e2e-generated age key**”.

## Key evidence we captured (the “smoking gun”)
On the failing **second run**, the checked-out repo directory for the new Gitea repo (example shown below) contains a mismatch:

- `.sops.yaml` contains a **new** recipient (derived from the current `/tmp/e2e-age-key.txt`).
- but the committed secret file contains an **old** `sops.age[].recipient` from the previous run.

Example from the failing run checkout:
- Checkout dir: `/tmp/gitops-reverser/e2e-test-416880`

`.sops.yaml` (new recipient):
- `e2e/secret-encryption-test/.sops.yaml` contains: `age1wkkp...`

Secret file (old recipient + old `lastmodified`):
- `e2e/secret-encryption-test/v1/secrets/sut/test-secret-encryption.sops.yaml` contains: `age13vmf...`
- `sops.lastmodified` stayed at the **previous run’s timestamp** (e.g. `2026-02-25T11:59:30Z`), which strongly indicates it was **not newly generated** this run.

We also verified the same mismatch exists **inside the controller pod’s local repo**:
- `/tmp/gitops-reverser-workers/sut/gitprovider-normal/main/e2e/secret-encryption-test/.sops.yaml` → new recipient
- `/tmp/gitops-reverser-workers/sut/gitprovider-normal/main/e2e/secret-encryption-test/v1/secrets/sut/test-secret-encryption.sops.yaml` → old recipient

This mismatch leads to the decryption failure the test sees (typically: “no identity matched any of the recipients”).

## What this implies (root cause direction)
This is not “the controller didn’t commit the secret”.
It’s “the controller committed **a secret encrypted to the wrong recipient**”.

And the pattern matches “**stale repo state being reused across runs**”:

- The controller uses a persistent on-disk working repo path:
  - `/tmp/gitops-reverser-workers/<namespace>/<gitprovider-name>/<branch>`
  - In e2e: `/tmp/gitops-reverser-workers/sut/gitprovider-normal/main`
- In e2e, the GitProvider URL changes every run because the test creates a unique repo name:
  - e.g. `.../e2e-test-416879.git` → next run `.../e2e-test-416880.git`
- The controller *does* detect this and logs:
  - “Updating remote origin URL … old … new …”
- But after switching to a new empty remote repo, the controller ends up pushing content that still contains files (or history) from the previous repo, including the old encrypted secret.

In short:
- **remote URL changes** + **local repo directory is reused** + **cleanup/reset is incomplete** ⇒ stale encrypted secret gets into the new repo ⇒ `.sops.yaml` and secret recipients diverge ⇒ decrypt fails at line 598.

## The “wanderings” / hypotheses we tried first (and why)
### 1) “Maybe deduplication drops events on run #2”
Because the failure looked like “secret isn’t updated”, we investigated dedup/state contamination in watcher/reconcile code.

Changes made:
- Added `Key()` to `types.ResourceIdentifier` for stable fully-qualified identifiers.
- Switched several internal dedup/diff maps to use `Key()` instead of `String()`.
- Added cleanup when `GitTarget` is NotFound to remove in-memory state:
  - `GitTargetReconciler.cleanupDeletedGitTarget(...)` calls:
    - `EventRouter.UnregisterGitTargetEventStream(gitDest)`
    - `ReconcilerManager.DeleteReconciler(gitDest)` (best-effort)

Outcome:
- This helped tighten correctness, but **did not fix** the encryption failure at line 598.

### 2) “Maybe stamp-based port-forwarding is stale”
We found a stamp target that could skip re-establishing port-forwards across runs.

Change made:
- `Makefile`: added a phony `portforward-check` dependency so the `portforward.running` recipe executes every time (and it restarts port-forwards if health checks fail).

Outcome:
- Improves developer ergonomics and reduces unrelated flakiness, but **not the root cause** of the encryption mismatch.

## The git-cleanup fix we attempted (and why it wasn’t enough)
Given the evidence, we focused on the “empty remote/unborn branch” path.

### What we changed
- In `internal/git/git.go`, `cleanWorktree()` used to call go-git’s `worktree.Clean(...)`.
- We replaced it with an explicit wipe of all top-level worktree entries except `.git`, using billy’s filesystem + `billyutil.RemoveAll`.
- Added unit test:
  - `internal/git/git_operations_test.go`: `TestMakeHeadUnborn_CleansWorktreeIncludingTrackedFiles`
  - It creates a repo with tracked + untracked files and asserts `makeHeadUnborn()` removes them.

### What passed
- `make fmt` ✅
- `make vet` ✅
- `make lint` ✅ (after fixing a `funcorder` complaint by reordering methods in `internal/types/identifier.go`)
- `make test` ✅
- `make test-e2e-encryption` on a fresh cluster ✅ (22/22 passed)

### What still failed
- Running `make test-e2e-encryption` a second time (without deleting the cluster) **still failed** at `test/e2e/e2e_test.go:598` with the same fundamental symptom: `.sops.yaml` recipient != secret file recipient.

So: “wipe worktree contents” improved the code, but the reproduction indicates **something else still carries old content across the remote switch** (likely refs/history, or the repo not being recreated when origin URL changes).

## Additional important observations
- The controller runtime image is distroless and does **not** contain `git`, so debugging inside the pod required inspecting the filesystem directly (not `git status/log`).
- `SmartFetch()` returns `""` on an empty remote and does **not** fetch/prune anything. That means old `refs/remotes/origin/*` may remain locally unless explicitly removed.
- `makeHeadUnborn()` currently removes `refs/heads/<branch>` but does **not** remove remote-tracking refs or reset the entire repository identity.
- The bootstrap code writes `.sops.yaml` **only if missing**. So a reliable “clean slate” is essential whenever the age recipient set changes between runs.

## Resolution (final, 2026-02-25)
This ended up being **two** independent “state leaks across runs” that combine into the line-598 failure.

### Root cause A: Secret encryption cache reused stale ciphertext across runs
`internal/git/content_writer.go` caches encrypted Secret bytes to avoid re-running SOPS. The cache key did **not** include
anything about the *encryption context* (repo/path/recipients/identities). When events lacked stable marker fields (UID /
resourceVersion), the cache could be hit across separate e2e runs even though the suite regenerated the age key.

That produces the exact symptom we observed:
- `.sops.yaml` contains the *current* recipient
- the committed Secret file contains an *old* `sops.age[].recipient` (and old `sops.lastmodified`)

Fix:
- Partition the encryption cache by a stable **scope** derived from `(provider, workDir, recipients, identities)` and
  include it in the cache key, so ciphertext never crosses runs/paths/keys.
- Added a unit test asserting that changing scope forces re-encryption.

### Root cause B: BranchWorker repo identity and GitProvider snapshots
Separately, we tightened worker correctness around “repo identity” and runtime GitProvider changes:

1. Local git repo paths are now keyed by remote URL hash
   - New layout: `/tmp/gitops-reverser-workers/<ns>/<provider>/<branch>/repos/<url-hash>`
   - Prevents cross-run contamination when the remote URL changes.

2. `GitProvider` is re-fetched before each commit/push batch
   - `commitAndPush()` calls `getGitProvider()` each time instead of using a long-lived snapshot.

3. Serialize `syncWithRemote()` with other git operations
   - Added the repo mutex to `syncWithRemote()` to prevent concurrent ops on the same on-disk repo.

### Supporting e2e robustness changes
- `Makefile`: force a controller rollout when building locally by patching a pod-template annotation with the built image ID.
- `test/e2e/e2e_test.go`: Prometheus metrics assertions use aggregated queries (`sum(...)`) to avoid rollout flake.

### Validation (2026-02-25)
- `make lint` ✅
- `make test` ✅
- `make test-e2e` ✅
- `make -B test-e2e` ✅ (confirms “second run” no longer fails)
- `make test-e2e-quickstart-manifest` ✅
- `make test-e2e-quickstart-helm` ✅

Note: quickstart Helm validation can fail with “No space left on device” if the devcontainer is nearly full; deleting
unused Kind clusters and pruning Docker build cache resolves that.
