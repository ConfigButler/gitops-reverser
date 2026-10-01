# GitTarget parent branch: keep a target on standby until a write needs a branch

> **design**: implemented in #407 (with #408's `spec.parentBranch` folded in). Open follow-ups:
> [`gittarget-parent-hardening.md`](gittarget-parent-hardening.md) and
> [`gittarget-parent-empty-repository.md`](gittarget-parent-empty-repository.md).
> Date: 2026-10-01.
> Related: [`push-notification-and-reconcile-trigger.md`](push-notification-and-reconcile-trigger.md),
> [`gittarget-configuration-freshness.md`](gittarget-configuration-freshness.md),
> [`../definitions.md`](../definitions.md)

**The use case.** A `GitTarget` stays on standby while the cluster matches `main`. Its watches are
running, its write branch does not exist on the remote, and normal Git changes can keep flowing
through the cluster's GitOps reconciler. When a captured cluster edit needs a commit, the target
creates its write branch using a freshly checked parent. A target that waited a week must not
publish that edit on a week-old checkout.

**The proposal.** Let the target name its parent branch. Omitting the field keeps today's default
of the remote's default branch. Setting `release-1.x` or `env/prod` supports the same standby flow
against another branch. The write branch can later serve as a PR head, with the parent as its base;
creating PRs is separate work.

```yaml
spec:
  branch: reverser/prod-edits   # the write branch (unchanged, immutable)
  parentBranch: main           # optional; omitted = the remote's default branch
```

**The constraint.** An idle, converged target creates no remote branch and no commit. Publication
validates the parent against the push session's advertisement and replays captured writes if it
moved. If replay leaves no difference, the target stays on standby. Existing commit windows and
push scheduling still apply: activation needs no manual switch or refresh tick, but publication
does not bypass those timers. Here, "no branch" means no branch on the remote; local working refs
may exist.

**Independent of polling.** We already refresh remote observations with a **ten-minute default**
(`--git-refresh-interval`, Helm `git.refreshInterval`). Build this contract as though that refresher
were absent: it must hold with `--git-refresh-interval=0`, before the first tick, and between ticks.
Periodic refresh improves the idle view of Git; the publication check protects the first write.

**Before the feature, a defect to fix (§2).** Code review identifies an untested defect: the push is
designed to refuse a parent branch that moved (the parent is recorded as the push root), but
`validatePushState` only makes that comparison when the write branch already exists. So when it does
not, a cycle that records the parent as its root can publish from a stale checkout. The exact worker
setup matters (§2.3); the push primitive's missing check is visible in the code.

## Use cases and scope

### Capture an operational edit for review

The cluster normally follows `main`, and `reverser/prod-edits` is absent. An operator changes a
Deployment during an incident. Reverser captures that change, checks the parent when publishing,
and creates the write branch for review. Any intervening commits to `main` belong in its starting
history. Replay re-plans the captured edit against that new tree, including any changed file layout.

This records the edit; keeping it active in the cluster still depends on the GitOps reconciler's
existing reconciliation policy. The parent-branch feature does not change that policy.

### Keep normal deployments quiet

A deployment advances `main`, and the reconciler applies the same manifests to the cluster. The
resulting watch events can initially look like edits against the cached parent. After checking and
replaying against the newer parent, they produce no difference. Reverser creates neither an empty
review branch nor a commit merely to record that it caught up.

An idle remote refresh only reads and reports. Parent movement alone must not start a write or
turn the cluster's temporarily older state into a proposed rollback. Explicit resyncs retain their
existing cluster-to-Git semantics; this proposal does not resolve deployment-order races for them.

### Capture changes against a release or environment branch

A cluster following `release-1.x` or `env/prod` needs its proposed changes based on that branch.
`parentBranch` expresses this directly. Branching from `main` could otherwise include unrelated
release changes in the eventual PR. An explicit parent that is missing blocks branch creation.

### Return to standby after review

After a PR is merged and its write branch is deleted, the same target can use the parent again
when the next edit arrives. This depends on detecting deletion and replaying against the parent,
so it needs a lifecycle test. Automatic deletion, PR management, and deciding whether a surviving
write branch is safe to reset remain separate work (§7). While the write branch exists, new
commits build on it; this proposal does not continuously rebase it onto the parent.

## 1. What happens today

The default branch is already the parent branch, implicitly.

- `SmartFetch` ([`git_smart_fetch.go`](../../internal/git/git_smart_fetch.go)) lists the remote refs.
  If `spec.branch` exists it fetches it; otherwise it falls back to the ref the remote `HEAD` points at.
  If neither exists (an empty repository) the worker makes `HEAD` unborn and the first commit is a root
  commit.
- The worker compares the cluster against whatever it checked out. A resync fetches first
  ([`resync_flush.go:132`](../../internal/git/resync_flush.go)), so a resync compares against the
  default branch's current tip.
- `ensureWriteBranch` prepares a local `spec.branch` from the checked-out commit before executing
  the writes. This can happen even when execution produces no commit. A later push creates the
  branch on the remote.
- Once `spec.branch` exists on the remote, the default branch no longer matters. Nothing ever rebases
  the write branch onto it.
- The [refresher](../../internal/git/refresh.go) checks aging remote observations on a reconcile
  tick. The [refresh setting](../configuration.md#keeping-an-idle-target-fresh---git-refresh-interval)
  defaults to ten minutes and sets the observation-age threshold for that check.
  An unchanged branch costs one advertisement, and a moved branch also costs a fetch and reset.
  Today it returns early when the write branch is absent, so the ten-minute setting does not keep
  that target's parent checkout current. Parent following in the refresher is proposed in §2.4.

## 2. Defect: a live event on an absent write branch commits onto a stale parent

### 2.1 The path

1. A worker for `branch: feature`, absent on the remote, finishes a fetch and reset with local
   `HEAD` still on `main` at A. `SmartFetch` selected `main` as the fallback. This is the starting
   state to establish explicitly; a completed snapshot resync can change it (§2.3).
2. `updateBranchMetadataFromPullReport`
   ([`branch_worker.go:2767`](../../internal/git/branch_worker.go)) marks the base **trusted**, on
   purpose, including for an absent branch. Its comment gives the reasoning: "for a branch the remote
   does not have, a worktree based on the default branch IS that state. There is nothing on the remote
   left to learn", and "a push onto a branch we believe is absent declares Old = zero, which the server
   rejects if somebody has since created it".
3. Nothing changes in the cluster for a week. Meanwhile `main` moves to commit B. The refresher looks
   only at `feature`, finds it absent, and returns without touching trust.
4. A live event arrives. `ensureBaseForCycle` ([`branch_worker.go:1824`](../../internal/git/branch_worker.go))
   sees a trusted, clean base and skips the fetch. The write is planned and committed on top of **A**.
   `ensureWriteBranch` records the push-cycle root as `(main, A)` (`pushCycleRootBranch`/`Hash`,
   [`branch_worker.go:218`](../../internal/git/branch_worker.go): "for a brand new branch it is the
   default branch we based the new branch on. PushAtomic uses this pair to detect concurrent updates").
5. `PushAtomic` receives that root. But `validatePushState`
   ([`git_atomic_push.go:165`](../../internal/git/git_atomic_push.go)) compares the root's recorded
   hash with its advertised tip **only inside `if found`**, that is, only when the write branch
   already exists on the remote. For a new branch the only root check is "the root still exists". So
   `main` at B versus the recorded A is never compared, the push goes out with `Old = zero`, and
   `feature` is born on A, a week stale.

So the design is right and already in place: the parent is recorded as the push root precisely so the
push can refuse a moved parent. The implementation skips that comparison in the one case it was
recorded for. `TestAtomicPush_PushToOther`
([`git_atomic_push_test.go:72`](../../internal/git/git_atomic_push_test.go)) is the only test of a
root that differs from the pushed branch, and its `main` never moves.

### 2.1a A second hole in the same branch of code

When the root differs from the pushed branch and the pushed branch **is** found (somebody created
`feature` between our fetch and our push), `oldHash` becomes their commit and the root check compares
`main` against `main`. If `main` did not move, the push sends `Old = <their commit>`,
`New = <ours>`, and nothing checks that ours descends from theirs: their commit is overwritten by a
non-fast-forward update on any server that allows one. The comment in
`updateBranchMetadataFromPullReport` ("a push onto a branch we believe is absent declares Old = zero,
which the server rejects if somebody has since created it") describes behavior this code does not
enforce. Also from reading only.

### 2.2 Consequences

- The write was planned against A's folder. If B changed that folder (another writer, a merged PR),
  the plan is wrong. At best the new branch lacks B's changes. At worst it re-writes a document B
  restructured, and the eventual merge conflicts or quietly undoes B.
- The PR flow is where this surfaces: a freshly created PR branch that is already behind its base.
- A new worker starts untrusted and fetches before planning, so a restart can mask the stale state.

### 2.3 Why the existing test does not catch it

`TestBranchWorker_CommitAndPushRequest_NewBranchStartsFromLatestMain`
([`branch_worker_test.go:668`](../../internal/git/branch_worker_test.go)) checks out A, moves `main`
to B, then commits through a **newly constructed** worker. A new worker starts with the base untrusted,
so it fetches and lands on B. The test proves the cold path; the defect is on the warm path, where the
same worker already trusts its checkout of A.

The primitive's missing comparison is clear from reading; no reproduction has been executed here.
Step 1 of the plan (§6) proves it with a failing unit test.

There is a second setup detail: a completed no-op resync calls `ensureWriteBranch` before learning
that there is no difference. It can leave local `HEAD` on `feature`, although `feature` is absent
on the remote. The next cycle then records `(feature, A)`, and the existing missing-root check can
already force a fetch. The original assumption that any warm resync leaves `(main, A)` as the next
push root is therefore too broad. Test both states, and inspect the recorded root before claiming
that the worker or e2e reproduction must fail.

### 2.3a A no-difference live event likely creates the write branch today

Verified by reading on 2026-10-01, not executed. This breaks the core constraint more directly than
§2.1 does, so it gets its own red test. A live window that plans no change is **still retained and
pushed**: `finalizeOpenWindowWithReason` appends it "including for a no-diff window", because only
the remote can settle "already present". `commitPendingWrites` has already run `ensureWriteBranch`,
so local `feature` exists at the parent's commit. `runPushCycle` has no zero-commit short cut, and
`validatePushState` returns `PushNoBranch` only when the local hash is zero. With `feature` absent
and the local hash nonzero, the plan is `Old = zero, New = <parent commit>`. The push creates
`feature` on the remote with no commit of its own. If HEAD was already on a local `feature` (§2.3),
the missing-root rejection replays first, and the result is the same branch created at the new
parent tip.

The "keep normal deployments quiet" use case hits exactly this: the reconciler applies `main`, the
watch sees the apply, and the window finds no difference. §2.4 item 2 is the fix; §6.2 item 4 and
§6.3 step 3b are the tests, and both should be red today.

### 2.4 Fix

**Keep publication responsible for freshness.** Use the existing advertisement and replay path:

1. **`validatePushState` (the fix).** Compare `rootHash` with the root's advertised tip whether or not
   the pushed branch exists. And when the root is a different branch from the one being pushed (a new
   branch), require the pushed branch to still be absent: if it was found, return `RemoteMovedError`
   for the pushed branch instead of pushing over it (§2.1a). Both rejections already flow into the
   existing path: `remoteMovedDuringPush` invalidates the base, fetches, rebuilds the retained writes,
   and pushes again. No extra connection, since the advertisement is read on the push session anyway.
   Keep `Old = zero` for branch creation so the server also rejects a branch created after the
   advertisement. The parent comparison is a client-side check: it is not atomic with the server's
   update of the write branch. A parent can move during upload, with no fixed time bound on that
   gap. The contract is the parent tip observed in the successful attempt's advertisement.
2. **Preserve standby after a no-op.** Live writes still reach the advertisement when they produce
   no commit. After validation and any replay, if no pending write produced a commit and the write
   branch remains absent, finish without creating it. Today `PushNoBranch` only covers an unborn
   local `HEAD`; a local branch pointing at a nonzero parent needs explicit coverage too. Keep the
   expected remote absence and resolved parent available across local branch switches and no-op
   cycles. Local `HEAD` alone does not describe that relationship (§2.3).
3. **In the refresher (the folder view).** While the write branch is absent, check the parent branch
   in the same advertisement. If it moved past the local checkout, fetch, reset, and re-read the
   folder, so `status.placement` and refusals describe what the next commit will build on.
   Still one connection when nothing moved; a moved parent also costs the fetch path. No commit.
   This improves idle observations independently of the publication guarantee. Delaying or
   disabling it must not change which parent a new write branch uses.

Replay means re-planning the retained cluster writes against the fetched tree. It preserves
unrelated content and applies the existing conflict policy for the captured objects; it is not a
Git rebase of a previously published branch. If the remote cannot be checked, retain the pending
work for retry. If the parent keeps moving, use the existing bounded retry behavior and retain the
work rather than publishing an unchecked result.

Rejected: removing trust for an absent write branch. The comment in step 2 records that an earlier
version did exactly that and was reverted for cost, and it would still leave the window between a
fetch and the push.

This fix stands on its own and should ship first, without the new field: it is a correctness bug in
today's default-branch behavior.

## 3. Prior art

The original survey read both projects from `external-sources/` and the module cache.

### Flux: `ImageUpdateAutomation`

`image-automation-controller/api@v1.2.3`, `api/v1/git.go`: `spec.git.checkout.ref` (a full
`GitRepositoryRef`: branch, tag, semver, name or commit) and `spec.git.push.branch`, which "is created
using `.spec.checkout.branch` as the starting point, if it doesn't already exist". Omitted `checkout`
falls back to the `GitRepository`'s ref, and that to `master`.

The starting point matters only when the push branch is created. `SwitchBranch`
(`external-sources/flux/pkg/git/gogit/client.go:462` in the local checkout): push
branch on the remote, build on it; absent, create it from the checked-out HEAD. There is no rebase.
Once the push branch exists, new commits on `main` are invisible to it; the usual remedy is to delete
the branch after the PR merges.

### Argo CD: the source hydrator

`pkg/apis/application/v1alpha1/types.go:428`: `drySource.targetRevision` (what it reads, any
revision), `syncSource.targetBranch` (the starting point), and an optional `hydrateTo.targetBranch`
(a staging branch "an external system would then have to move ... e.g. by pull request").

- Free-form revisions appear only where Argo reads. Everything it writes to, or creates a branch from,
  is typed `targetBranch`.
- `CheckoutOrNew(target, base)` (`util/git/client.go:1637`): check out `hydrateTo` if it exists, else
  create it from `syncSource`. Starting point at creation only, never a rebase.
- No difference, no commit: `WriteForPaths` returns `shouldCommit=false` and the commit server records
  a git note instead (`commitserver/commit/commit.go:176`).
- `CheckoutOrOrphan` (`util/git/client.go:154`) creates an orphan branch when the sync branch is
  absent: the explicit "empty branch" case of §5.

### What we take from them

1. A branch-typed field at a write position (Argo).
2. Both treat the starting point as a creation-time input only. We already compare against the parent
   while the write branch is absent, and §7 is where we can go further than either.

### Git tools that use "parent branch"

git-town (`git town set-parent`, `git town sync`) and Graphite's stacks use **parent branch** for a
branch relationship that also includes synchronization. Our initial contract is narrower: follow
the parent while the write branch is absent, then build on the write branch. The name must not
promise automatic synchronization of an existing write branch; that remains deferred (§7).

## 4. Proposal

### 4.1 API

```go
// GitTargetSpec

    // ParentBranch is the branch the write branch (spec.branch) is created from, and that the target
    // compares against while the write branch does not exist on the remote. It is only read, so it
    // does not need to be in the GitProvider's allowedBranches. Omitted: the branch the remote's HEAD
    // points at (its default branch). When the write branch is absent, an explicitly set parent
    // must exist or branch creation is refused. An existing write branch is used independently.
    // +optional
    // +kubebuilder:validation:MinLength=1
    ParentBranch string `json:"parentBranch,omitempty"`
```

- **Mutable.** It says where a branch starts; changing it abandons nothing already written (unlike
  `branch`/`path`, which are the destination). A change takes effect the next time the write branch is
  absent.
- **Not in `allowedBranches`.** We never write to it.
- **`parentBranch == branch` is legal** when that branch exists. If it is absent, the explicit
  parent requirement blocks creation, including in an empty repository. Omission retains today's
  bootstrap behavior.

### 4.2 Naming

[`definitions.md`](../definitions.md) binds here. The candidates it rules out:

| Candidate | Problem |
|---|---|
| `sourceRevision` | "source" is the source *cluster* (`clusterProviderRef`, `rules[].sourceNamespace`) |
| `baseRevision`, `baseRef`, `baseBranch` | "base" already means a kustomize **read-only base** and **base trust** (rule 1: one concept, one word); `revision` is reserved for a polymorphic Flux-style identifier; a `...Ref` field must reference an object (rule 2) |
| `checkout.ref.branch` | verbatim Flux, but a one-field block nested twice, and "checkout" names the action while this field names a relationship |
| `startPoint` | Git's own word (`git switch -c <new> <start-point>`), but it means creation only, and the field also governs what is followed |

`parentBranch` is a single fact, so it is flat (rule 2), and it names the standby relationship. The
word "parent" appears in the subresource specs ("parent resource"), but "parent branch" always carries
its noun. In prose: **the parent branch** and **the write branch**. Both go into `definitions.md`
under "Write side" when this ships.

The cost of the name: it can never hold a commit or tag. That is accepted (§5).

### 4.3 One parent branch per write branch

The branch worker is keyed by `(GitProvider namespace, GitProvider name, branch)` and owns one
checkout, so every target on that write branch must agree on its parent. `checkForConflicts`
([`gittarget_controller.go:1087`](../../internal/controller/gittarget_controller.go)) gains a second
clause: same provider and write branch, different `parentBranch` is a `TargetConflict`, and the
later-created target loses, as it does for overlapping paths.

Values are compared as written, with omitted as its own value, so omitted and `main` conflict even
when the remote's `HEAD` is `main`. The controller decides without a remote round trip, and the answer
must not flip when somebody changes the remote's default branch.

The worker receives the parent branch alongside `RepoIdentity`, and `EnsureWorker` treats a change
like a GitProvider repoint: invalidate the base, keep the worker.

### 4.4 Worker behavior

| Write branch on remote | Parent branch on remote | Behavior |
|---|---|---|
| yes | any | unchanged: fetch and work on the write branch |
| no | yes | compare against the parent branch; create the write branch from its current tip on the first commit (§2.4) |
| no | no, **set explicitly** | **refuse**: `Ready=False`, reason `ParentBranchNotFound`, nothing written |
| no | no, omitted, remote `HEAD` unset | unchanged: unborn `HEAD`, root commit (empty repository) |

`SmartFetch` takes the parent branch instead of always using remote `HEAD`. When a configured parent
branch is missing it returns a sentinel error rather than `""`, so the worker never starts an orphan
branch because of a typo.

`status.remote` keeps meaning the write branch (empty `commit` = not created yet).

## 5. Implicit when omitted, strict when set; deferred explicit forms

**The principle.** Omitted is lenient: the default branch, and an empty branch with no history when
the repository itself is empty, because that is the first-install case and requiring configuration for
it is friction. Set is strict: it must exist, or the target refuses. Making the empty-repository case
explicit would force everyone to configure today's default.

**Deferred, additive when someone asks:**

- **A deliberately empty write branch in a non-empty repository.** A dedicated `cluster-state` branch
  with no history from `main`, the `gh-pages` pattern; Argo's `CheckoutOrOrphan`. Legitimate, unasked.
  It would have its own field.
- **A pinned commit or tag** ("branch off exactly what is deployed"). Small value: the starting point
  only decides where the write branch is born, after which it carries on independently, and a pin
  leaves the refresher nothing to follow. Fetching one commit by hash also needs server support
  (`uploadpack.allowReachableSHA1InWant` or similar); support varies between servers, which is why
  Flux lets `commit` be paired with `branch`. It cannot go in `parentBranch`; it would be a sibling
  field.

## 5a. Handoff: executing this from a fresh context

Read this section first.

- **Historical.** This was the handoff for #407 and #408, both implemented. The argument lives in the
  PR bodies, and the lasting text in `architecture.md` (§8).
- **Do not disturb the commit-window work** on `feat/commit-window-surface` in the main checkout. Work
  in a separate git worktree on a new branch off `origin/main`. Before running `task test-e2e`, ask
  the user: the e2e cluster is shared and a second run dies on its per-cluster lock.
- **Two PRs, in this order.**
  - **PR 1, `fix(git)`:** §6.1, the §6.3 e2e spec, the §2.4 fix, the §6.2 tests, and the §8.1 to 8.3
    `architecture.md` edits. Also correct the comment in `updateBranchMetadataFromPullReport` that
    claims an absent branch is pushed with `Old = zero` and that the server rejects it if somebody
    created it.
  - **PR 2, `feat(api)`:** the field, §4 and §6.4, plus §8.4.
- **This is the Git write path** (`internal/git/`), which AGENTS.md treats as high risk. Follow the
  repo's validation sequence; the coverage ratchet bump (`.coverage-baseline`) is committed with the
  change.
- **Show red before green for the defect.** The two §6.1 atomic-push tests should fail before the
  fix. Worker tests must distinguish the local states in §2.3; an acceptance test that already
  passes through missing-root recovery does not reproduce the primitive defect. Inspect the
  recorded root and replay path, and retain that test as lifecycle coverage.

### End-to-end test pitfalls found while planning §6.3

- **The Gitea repository starts empty.** Seed `main` with commit A from outside before creating the
  target, or there is no parent to go stale.
- **Every helper hard-codes `main`.** `remoteBranchHead` (`repo_assertions_test.go:46`) runs
  `ls-remote --heads origin main`, and `pushKustomizationFromOutside` (`refresh_remote_e2e_test.go:175`)
  fetches and pushes `main`. The spec needs a branch parameter for the first, and a "commit file X to
  `main` from outside" variant of the second.
- **The spec can pass without exercising the defect.** The stale-parent path needs a **warm** worker
  that checked out A and trusts it *before* B is pushed. If the worker has not cloned yet when the
  ConfigMap event arrives, it fetches on the cold path, lands on B, and the spec passes without
  proving anything. So step 2 must observe that the worker fetched at A: candidates are the per-branch
  fetch metric (the `fetchReason*` series in `branch_worker.go`, scraped by the e2e Prometheus) or the
  controller log line `Preparing branch for operations` for that branch. Also account for a local
  branch created by a no-op resync (§2.3). Do not use `status.remote` as proof of the checked-out
  parent: it describes the write branch, and publication has its own cadence.
- **The target's own initial snapshot must not create the branch.** Use a `WatchRule` with a label
  selector that matches nothing (the namespace always holds `kube-root-ca.crt`), so the snapshot is
  an in-sync no-op that still clones.
- **The GitProvider's `allowedBranches`** must include the write branch.

## 6. Plan and tests

Order matters: prove today's behavior, fix the defect on today's default, then add the field.

### 6.1 Reproduce the defect (unit, fails today)

At the push primitive, next to `TestAtomicPush_PushToOther`:

- `TestAtomicPush_PushToOther_DetectsMovedRoot`: local `feature` built on `main` at A, `main` moved to
  B on the remote, `feature` absent. Expect `RemoteMovedError{Branch: main, Expected: A, Advertised: B}`.
- `TestAtomicPush_PushToOther_DetectsConcurrentlyCreatedBranch`: `main` unmoved, but `feature` created
  on the remote by somebody else. Expect `RemoteMovedError` for `feature`, and their commit untouched.

At the worker, through the whole cycle, cover both local states:

`TestBranchWorker_LiveWriteOnAbsentBranchStartsFromTheParentsCurrentTip`: one worker, seeded `main`
at A, write branch `feature` absent on the remote. First exercise a trusted fetch/reset that leaves
`HEAD` on `main`; separately exercise a completed no-op resync that leaves a local `feature`.
Assert the setup in each case. Push B to `main` from a second clone, then send a live write through
the **same** worker, with no intervening refresh or resync. Assert the first published commit on
`feature` has parent B and contains B's file. The first variant targets the missing parent check;
the second guards the normal standby lifecycle and can already pass through missing-root recovery.

### 6.2 Fix (§2.4), and cover the "no commit while idle" side

All with a seeded `main` and `feature` absent; every existing test of these rules runs on `main` or on
an empty remote:

| Existing test | Why it does not cover the case |
|---|---|
| `TestResync_WorkerNoopDoesNotRetainOrPush` | write branch is `main` |
| `TestBranchWorker_DoesNotCreateBootstrapOnlyCommit` | empty remote |
| `TestRefresh_AnAbsentBranchCostsOneConnection` | empty remote, nothing to follow |
| `TestBranchWorker_CommitAndPushRequest_NewBranchStartsFromLatestMain` | cold worker only (§2.3) |
| e2e `commit_request_e2e_test.go:97` ("branch is not created") | empty repository, branch `main` |

1. An in-sync snapshot creates no `feature`, and no push is attempted.
2. `main` moves (an unrelated file); a second in-sync snapshot still creates no `feature`.
3. `main` moves so the cluster now differs; the resync diffs against the new `main`.
4. `main` gains exactly the document the cluster has; replay leaves no commit and `feature` is
   never created on the remote. Also cover a live write that already matches an unmoved parent.
   Both still verify the remote before reporting completion. The live-write variant is expected red
   today (§2.3a).
5. Push-time check: parent moved between plan and push; the push is treated as rejected, replays, and
   `feature` parents on the new tip. Happy path: no extra connection (count with the existing fetch
   and connection metrics, as `refresh_test.go` does).
6. Refresher: parent moved while `feature` is absent: advertisement followed by the fetch/reset
   path and folder re-read, no commit. Parent unmoved: one connection, no fetch.
7. Publication with periodic refresh disabled, and with the last refresh completed immediately before
   `main` advances: both start from the parent checked during publication. Exercise parent movement
   while a commit window is open too. A refresh tick is never a precondition for activation.
8. Merge and delete the write branch, advance `main`, then send another edit through the same
   worker. The recreated branch contains the new parent history and only the new pending edits.
9. Parent movement during replay retries: each attempt checks again. An unavailable remote or
   exhausted retries retain the pending work and leave the remote write branch uncreated.

### 6.3 e2e: the first commit lands on the latest default branch (required)

This spec is the acceptance test for the standby lifecycle and is not optional. Write it after 6.1,
before the fix. If it already passes, inspect whether it used missing-root recovery (§2.3); the
primitive tests still have to reproduce the missing comparison.

There is none today: apart from a negative `allowedBranches` check
(`gitprovider_validation_e2e_test.go:99`), every e2e `GitTarget` writes to `main`. New spec,
`Label("manager")`, no `parentBranch` set:

1. Seed the Gitea repo's `main` with commit A. The `GitProvider` allows `main` and
   `reverser-e2e-<seed>`.
2. Create a `WatchRule` whose label selector matches nothing yet, and a `GitTarget` writing to
   `reverser-e2e-<seed>`. Wait until its initial snapshot has completed, which is what makes the
   worker check out A and trust it.
3. Assert the write branch does not exist (`ls-remote`), and still does not after a steady tick:
   idle means no branch and no commit.
   3b. **A live event with no difference.** Seed A with the folder already holding a ConfigMap
   document, then create that ConfigMap in the cluster with matching content and the selector's
   label. Assert the write branch is still absent once the commit window and push cooldown have
   passed. This is the "keep normal deployments quiet" case, and §2.3a expects it to fail today.
4. Push commit B to `main` from outside, touching a file inside the target's folder, the way
   `pushKustomizationFromOutside` does in `refresh_remote_e2e_test.go`.
5. Create a `ConfigMap` that matches the selector. Avoid a new resync between B and this live event.
   Prove that recovery happened on the **publication** path by asserting which fetch fixed it: the
   per-branch `GitFetchesTotal` series must gain a `contention` fetch (the rejected push) and no
   `refresh` fetch between steps 4 and 6. That removes the refresher from the proof without a
   dedicated deployment. The e2e controller runs with `--git-refresh-interval=30s` for every spec,
   and redeploying it with `0` for one spec would disturb the rest of its leg. The
   refresh-disabled case is cheap at unit level, where §6.2 item 7 covers it.
6. Assert the write branch now exists, its tip's parent is B, B's file is present, and the
   `ConfigMap` document is in the folder.

The bring-up costs one Gitea repository and one target; it fits an existing `manager` leg.

### 6.4 The field

- `parentBranch: release` with `release` ≠ default: the new write branch parents on `release`'s
  tip checked during publication.
- Configured parent missing: `ParentBranchNotFound`, no orphan branch, no push; recovers once the
  branch is pushed.
- `parentBranch == branch`: existing branch works; absent branch refuses, including on an empty
  remote. An omitted parent still allows empty-repository bootstrap.
- Once the write branch exists, moving the parent changes nothing (the Flux/Argo rule we keep for now).
- `TargetConflict` for two targets on one write branch with different parents; created-later loses;
  omitted versus `main` conflicts.
- Changing `parentBranch` on a live target while the write branch is absent re-reads the folder on the
  next refresh.
- CRD: `parentBranch` non-empty when set; mutable.
- e2e: the §6.3 spec repeated with `parentBranch` naming a non-default branch.

## 7. Next phase: following again after a merge

Deleting the write branch after merge lets the target return to standby (§6.2). A write branch
left on the remote needs a separate policy: its next change builds on that surviving branch, which
may have fallen behind the parent. With a squash merge, a later PR can also show old commit history.

Candidate rule: **when the write branch adds nothing to the parent branch, reset it to the parent's
tip** (force-push, or delete it and let the next commit recreate it). "Adds nothing" is easy for a
merge commit or fast-forward: the write tip is an ancestor of the parent tip. A squash merge is
different: the commits never enter `main`'s history, so the test has to be content-based, for example
"the folder's tree at the write tip equals the folder's tree at the parent tip". Open points: compare
the folder only, or every path the branch ever touched; and whether resetting a branch with an open PR
is acceptable on forges that treat it as closing the PR. Settle this with the PR-creation design.

## 8. Changes to `docs/architecture.md`, applied with the fix

Not applied yet, because `architecture.md` describes what the code does and today it does not do this.
Each edit lands in the PR that makes it true: 8.1 to 8.3 with the §2.4 fix, 8.4 with the field.

`architecture.md` says nothing today about where a new write branch starts, and its ground rule
overstates the guarantee for a branch that does not exist yet: "a stale 'trusted' is caught by the
compare-and-swap on the next push" (line 73). The rule to state, briefly:

> An idle target creates no branch and no commit. When the write branch does not exist yet, the
> target compares against its parent branch. Publication checks that parent against the push
> session's advertisement and replays retained writes when it moved. If replay leaves no commit,
> the write branch stays absent. This works with periodic remote refresh disabled.

### 8.1 Ground rules (after the "stale trusted" sentence, around line 73)

> For a new write branch, the worker checks the parent's advertised tip before sending the push.
> A moved parent triggers fetch, reset, and replay. The server's compare-and-swap protects creation
> of the write branch with `Old = zero`; it does not lock the parent. The parent can still move
> after the advertisement, so the guarantee is freshness at that observation, not at server acceptance.

### 8.2 Local clones and conflict retry (next to the "deleted branch" paragraph, around line 1234)

> **A branch that does not exist yet** is created from the remote's default branch (its parent)
> only when a write has something to commit, so an idle or in-sync target never creates it.
> Until then the worker compares the folder against the
> parent. Periodic refresh, ten minutes by default, updates the idle view independently of the
> publication check. The push records the parent and the commit it started from as its root, and
> treats a moved parent like a moved branch: fetch, reset, replay, check again. Proved end to end
> by the e2e spec `<name from §6.3>` with periodic refresh disabled.

### 8.3 "Remote moved while we were writing" flowchart (around line 570)

The decision node becomes `Write branch (or, while it is absent, its parent) still at the expected
commit?`, and the prose above it gains half a sentence: "...or, for a branch not created yet, if its
parent branch moved".

### 8.4 Configuration model, GitTarget key fields (around line 358)

> - `spec.parentBranch`: optional, mutable branch the write branch is created from and compared
>   against while it does not exist. Omitted means the remote's default branch. An explicit parent
>   must exist when creating the write branch. It does not need to be in `allowedBranches` because
>   it is only read.

Plus one line beside the existing overlap rule: "...and rejects two GitTargets on one branch that name
different parent branches." Update §8.2's default-branch wording to include the configured parent
in the same feature PR.

## 9. Open questions

1. Report the parent branch's observed commit in status (`status.remote.parent.commit`?) so an
   operator can see what an absent write branch would be created from. Useful, but it grows a block the
   refresher samples on the steady tick.
2. `ParentBranchNotFound` as a `Ready=False` reason versus its own condition. It gates writing, like
   `BranchNotAllowed`, so a `Ready` reason looks right; confirm against
   [`../spec/status-conditions-guide.md`](../spec/status-conditions-guide.md).
3. `GitProvider.status.branches` lists write branches; should each entry show its parent branch?
4. An omitted parent resolves through remote `HEAD` during fetch. Define when to re-resolve it if
   the default branch changes while the old parent still exists: the current push session does
   not advertise symbolic `HEAD`, so checking the recorded parent's hash alone cannot detect that
   change. Explicit `parentBranch: main` avoids this ambiguity for the primary standby use case.
