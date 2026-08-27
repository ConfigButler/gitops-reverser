# Investigation handoff: a GitTarget registers its event stream and never declares

Working document for PR #315 (`feat/target-watch-cell-identity`). Written so a fresh
context can continue without re-deriving anything. **Facts are separated from
hypotheses.** Nothing below is a conclusion unless it says so.

Status at the time of writing: **the branch is NOT mergeable.** A reproducible e2e
failure is open and its cause is not established.

---

## 1. What the branch does

Commits on top of `main` (`ff966f5d` is the merge-base):

| Commit | Content |
| --- | --- |
| `164adc28` | cell, not GVR, is the sweep boundary's identity |
| `e5a86a74` | one target watch per cell, not per served version |
| `4a168948` | stamp queued work with the producing cell |
| `099233e3` | key stream readiness, retention and fidelity on the cell |
| `e8bd4808` | delete the stream lease; collapse `git.Provenance` to the cell |
| `94c43263` | **change 1**: compute the per-cell plan diff and log it (no behavior) |
| `b501954d` | **prerequisite**: render-fidelity revision becomes per scope |
| `e591ebd1` | **change 2**: apply the plan per cell (per-stream cancel) |
| `060bb739`, `e2685f51` | docs |
| `eeb231ab` | write-divergence guard for an empty plan + doc fixes (**unpushed**) |

Design: [`docs/design/target-watch-plan.md`](docs/design/target-watch-plan.md).

### The behavior change that matters here (`e591ebd1`)

Before: `replaceGitTargetWatches` compared the whole rendered spec map. Equal meant an
early return; different meant one `prior.cancel()` for the entire target, then every
stream restarted.

After: `targetWatchSet` is `map[types.CellKey]*runningTargetWatch`, each with its own
`cancel`. The plan is diffed into `keep` / `start` / `restart` / `stop` and applied per
cell. `internal/watch/target_watch.go:172-215` is the whole function.

Two orderings are NEW in that function, and both happen while `targetWatchesMu` is held:

- `set.stop(cell)` calls a stream's `context.CancelFunc` **under the mutex**
  (`target_watch.go:194-199`).
- `reconcileTargetRenderFidelityLocked` takes the render-fidelity gate's own mutex
  **under `targetWatchesMu`** (`target_watch.go:204`), establishing the lock order
  `targetWatchesMu → RenderFidelityGate.mu`.

---

## 2. The failure

### 2.1 Symptom, identical in two independent runs

A WatchRule's `Ready` condition stays `False` past the 90s e2e timeout. The e2e helper
that fails is `verifyResourceStatus` → `test/e2e/e2e_test.go:234`, comparing the
condition status `False` against the expected `True`.

`Ready` on a WatchRule is a roll-up that includes `ConditionTypeStreamsRunning`
(`internal/controller/watchrule_controller.go:209`), which is projected from
`WatchManager.StreamSummaryForWatchRule`. With no declared streams, the rule's expected
cells have no entry in `targetStreamStates`, and `streamStatusesByType`
(`internal/watch/stream_readiness.go:228-245`) treats a missing entry as `Replaying`.
So the rule can never reach Ready. **That part is understood and is a consequence, not
the cause.**

### 2.2 The cause is upstream: the GitTarget never declares

In both failing runs, for the affected GitTarget only:

- `Registered GitTargetEventStream` is logged (emitted from
  `internal/controller/gittarget_controller.go:687`, inside the worker-wiring gate);
- **no `target watch plan reconciled` line is ever logged for that GitTarget.**

That log line is emitted unconditionally at `target_watch.go:212`, on every path that
reaches it, including the all-`keep` no-op case. Its absence means
`replaceGitTargetWatches` never got that far for that target.

> **Correction (review, 2026-08-27).** An earlier version of this document read the pair
> "registered, then nothing" as *one* reconcile that got past wiring and then blocked.
> That inference is **not supported**. `RegisterGitTargetEventStream` is only reached when
> the stream does not already exist (`gittarget_controller.go:682-688` returns the existing
> stream silently), so the log line marks the FIRST reconcile that passed the wiring gate
> and says nothing about any later one. What the evidence supports is only: the target
> passed wiring at least once, and **no** reconcile ever completed a declare.
>
> The review also placed a candidate EARLIER than declare:
> `resolveSourceClusterProvider` (`gittarget_controller.go:193`) is a synchronous API read
> that runs after wiring and before `DeclareForGitTarget`. An error there is ruled out (no
> `Reconciler error` in the log); a block there is not.
>
> And it found a concrete unbounded-wait mechanism in code this branch did not touch: the
> discovery call in `refreshClusterForDeclare` gets a request timeout **only for a remote
> cluster**. `cluster_context.go:695` applies `sourceClusterDialTimeout` to a config copy
> under `if !cc.isLocalLocked()`, so for the LOCAL cluster the legacy, non-context
> `ServerGroupsAndResources()` runs with no deadline at all.

Meanwhile the WatchRule reconciles successfully every 10s
(`WatchRule reconciliation via GitTarget successful`), and other GitTargets declare
normally throughout the same window.

### 2.3 Run-by-run evidence

| Run | Code | Result | Failing spec |
| --- | --- | --- | --- |
| Local full `task test-e2e` | `e2685f51` | **FAIL** 69 passed / 1 failed | Commit Signing / custom reconcile message template `[signing]` |
| CI `E2E (full-manager)` run `33057523968` | `e2685f51` | **FAIL** 54 passed / 1 failed | Manager WatchRule / delete Git file when ConfigMap is deleted `[manager]` |
| Local `[signing]` only (5 specs) | `eeb231ab` | **PASS** 5/5 | — |
| Local `[manager]` subset (55 specs) | `eeb231ab` | **PASS** 55/55 in 8m23s | — |

Both failures are the same shape, in **different specs**. The failing GitTargets were
`signing-reconcile-dest` and `watchrule-delete-test-dest` respectively.

**The two green rows do not exonerate anything, because they change two variables at
once.** They ran a smaller spec set AND a different commit (`eeb231ab` rather than
`e2685f51`). The local `[manager]` run is the same 55-spec set as CI's failing
full-manager leg and passed, so the failure is not a deterministic property of that
spec set. The only code difference between the two commits is the empty-plan
write-divergence guard in `RenderFidelityGate.Reconcile`, which has no path to the
declare code and is very unlikely to be the reason; but it is a difference, and the
comparison is confounded until a full run is repeated on one commit.

Reading the four rows together: both LARGE runs failed and both SUBSET runs passed,
which is what an intermittent, load- or timing-dependent fault looks like. Two out of
two large runs failing is a high hit rate for a flake, and no large run has yet been
made on `main` for comparison.

Verbatim, from the local run, all 19 log lines mentioning `signing-reconcile-dest`
reduce to:

```text
1  "msg":"Registered GitTargetEventStream"
11 "msg":"Starting WatchRule validation"
1  "msg":"Unregistered GitTargetEventStream"
1  "msg":"GitTarget not found, was likely deleted"
```

Timeline of the CI failure (`watchrule-delete-test-dest`):

```text
09:27:39  STEP creating GitTarget 'watchrule-delete-test-dest'
09:27:39  Registered GitTargetEventStream         <- last trace of the target
09:27:39  WatchRule reconciliation via GitTarget successful
09:27:49 .. 09:29:09  the same WatchRule reconcile, every 10s, always "successful"
09:29:09  [FAILED] Timed out after 90.000s: <string>: False to equal <string>: True
09:29:17  Unregistered GitTargetEventStream (namespace teardown)
```

### 2.4 CI green baselines on this branch

| CI run | Commit | Contents | Result |
| --- | --- | --- | --- |
| `33052920251` | `e8bd4808` | up to the lease deletion | success |
| `33055342913` | `94c43263` | + change 1 (log only) | success |
| `33057086134` | `060bb739` | + change 2 | **cancelled** (no signal) |
| `33057523968` | `e2685f51` | + docs | **failure** |

So the first CI run that contained change 2 failed, and every run before change 2 passed.
That is **one** data point, not a bisect: `060bb739` was cancelled, so change 2 has never
had a clean CI run of its own.

---

## 3. Ruled out, with the evidence

All of these were checked against the CI job log, not reasoned about:

- **No `Reconciler error`** anywhere in the job log (`grep -c` → 0). So no reconcile
  returned an error to controller-runtime.
- **No `watch-first declare skipped; surface not observable`**. That is the Info log
  `DeclareForGitTarget` writes whenever `EnsureGitTargetWatches` returns an error
  (`internal/watch/materialization.go:38-41`), so none of the four error paths in
  `EnsureGitTargetWatches` (`target_watch.go:141-170`) was taken.
- **No `aborting watch setup`** (the two error strings inside that function).
- **No global stall**: `target watch plan reconciled` lines appear steadily through the
  90s window for other targets (13 lines at 09:27:28, 17 at 09:27:39, 11 at 09:28:20,
  12 at 09:29:09, and so on).
- **`enqueueGitPathChange` cannot block**: it is a non-blocking `select` with a
  `default` (`internal/watch/gitpath_events.go:33-47`).
- **`ResourceReference.Key()` is `"namespace/name"`** and does not include the UID
  (`internal/types/reference.go:44`), so a UID-bearing reference and a rule-derived one
  map to the same key. No key mismatch there.
- **The error census** for the whole CI job, by message:
  `690 sendInitialEvents unsupported` (a known k3s fallback warning),
  `185 Resync commit failed; dropping request`, `9 Failed to get referenced GitTarget`,
  `4 Repository connectivity check failed`, `4 Remote connectivity check failed`,
  `4 Failed to build pending write; dropping open window`,
  `3 audit Redis not yet reachable`, `2 Failed to resolve GitProvider from GitTarget`,
  `1 per-type reconcile failed`, `1 Failed to build resync pending write`.

The elimination is uncomfortable: every path into `replaceGitTargetWatches` either logs
an error or reaches the unconditional log line, and neither happened.

---

## 4. Hypotheses (none confirmed)

**H1 — the reconcile blocks before reaching the log.** (Strongest.) If the GitTarget controller's
reconcile blocks inside `DeclareForGitTarget`, there is no log and no error, which is
exactly what we observe. It would also be self-sustaining: `refreshRunningTargetWatches`
(`target_watch.go`, called from `ReconcileForRuleChange`) only refreshes targets already
present in `m.targetWatches`, and a target whose first declare never completed is not in
that map, so no later WatchRule reconcile can rescue it. Candidate blocking points, in the order they are reached:
`resolveSourceClusterProvider` (an API read, BEFORE declare), `refreshClusterForDeclare`
(a discovery call with no request timeout on the local cluster, `cluster_context.go:695`),
`refreshWatchedTypeTables`, or `targetWatchesMu` itself.

**H2 — lock-order inversion introduced by change 2.** (Weakened by review: the fidelity
recording path releases `gate.mu` before taking `targetWatchesMu`, so no cycle is
demonstrated.) `targetWatchesMu → gate.mu` is new
(`target_watch.go:204`). A reverse path would deadlock. `MarkTargetRenderFidelityScopeClean`
releases `gate.mu` inside `gate.RecordScopeClean` before `recordRenderFidelityStatus`
takes `targetWatchesMu`, so that specific path looks safe, but the whole set of
`gate.mu` holders has not been enumerated. Note `sync.RWMutex` is write-preferring: a
blocked `Lock()` blocks later `RLock()`s, so a slow reader plus a waiting writer can
serialise more than it appears.

**H3 — cancelling under the mutex.** `set.stop()` runs `cancel()` while holding
`targetWatchesMu` (`target_watch.go:194-199`). Cancellation itself is non-blocking, but
it wakes stream goroutines that immediately contend for `targetWatchesMu`
(`markTargetStreamState`, `recordRenderFidelityStatus`). This should only be contention,
not deadlock, but it is new behavior under a lock and is worth ruling out.

**H4 — not caused by this branch at all.** No run of change 2 has been compared against a
`main` run under the same conditions, and the failing spec moved between runs, which is a
race signature. `docs`-level note: my memory has several pre-existing e2e flake entries,
but none matches this signature.

H1 is the only hypothesis that explains *all* the evidence including the silence. H2 and
H3 are candidate mechanisms *for* H1.

---

## 5. The decisive next step

Repeat the **full local suite** (the 73-spec default filter, which is what failed the
first time) on a single commit, and if it reproduces, take a **goroutine dump** from the
manager while the GitTarget is still stuck. That distinguishes H1/H2/H3 from H4 in one shot: either the
GitTarget reconcile goroutine is parked on a mutex or a network call, or it is not there
at all.

A goroutine dump IS possible: the image is `gcr.io/distroless/static:debug`, which ships
busybox, and the manager is PID 1. Verified on a live pod:

```bash
kubectl exec -n gitops-reverser <pod> -- /busybox/kill -QUIT 1
kubectl logs -n gitops-reverser <pod> --previous | head -200
```

SIGQUIT makes the Go runtime print every goroutine's stack and exit, so the pod restarts:
do it only once the spec has already failed. What to look for, per the review:

| Frame at the top of the GitTarget reconcile goroutine | Reading |
| --- | --- |
| `resolveSourceClusterProvider` / client GET | blocked BEFORE declare |
| `refreshClusterCatalog` / `ServerGroupsAndResources` | inside declare, discovery blocked |
| `targetWatchesMu.Lock` | plan replacement contention (H2/H3) |
| no such goroutine at all | the registration log was from an earlier attempt (H4) |

`Manager.DeclaredSourceCluster` (`cluster_context.go:317`) is a second, cheaper
discriminator: it is recorded at the START of `DeclareForGitTarget`, before any discovery.
Set, with no plan log, means the block is inside declaration; unset means the reconcile
never reached declaration.

Practical notes:

- `E2E_LABEL_FILTER` **replaces** the default filter, so re-AND the exclusions:
  `E2E_LABEL_FILTER='manager && !image-refresh && !bi-directional && !source-cluster && !ado'`.
  A zero-match filter skips both suite hooks and passes vacuously.
- The cluster survives a run; the manager pod is redeployed by the next run, so pull
  logs or dumps **before** starting another.
- Read the Ginkgo JSON report rather than the truncated console log.

A watcher can catch it without babysitting: poll every GitTarget for
`StreamsRunning=False` older than ~60s, and dump goroutines the moment one appears.

If the block is confirmed, the smallest candidate fixes are: move `set.stop()` calls out
of the critical section (collect the cancels, run them after `Unlock`), and take the
fidelity gate outside `targetWatchesMu` (compute the revisions first, then apply).

---

## 6. Also open, unrelated to the failure

Ten CodeRabbit review findings were triaged; nine were valid and are fixed in the
unpushed `eeb231ab`, one was rejected (a "dangling godoc fragment" that is a complete
sentence ending in "…authorized for."). The one real code finding was mine: `Reconcile`
cleared a write divergence when the scope set was empty, because `fresh == len(scopes)`
holds vacuously at `0 == 0`.

Two debts change 2 left, recorded in the design doc: `streamRevisions`' zero-revision
fallback for the no-gate wiring, and the retention roll-up's install-then-report
ordering requirement.
