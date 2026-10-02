# GitTarget parent branch: keep a target on standby until a write needs a branch

> **design**: built in #407, with #408's `spec.parentBranch` folded in and the hardening pass
> ([`gittarget-parent-hardening.md`](gittarget-parent-hardening.md), done) applied. This page
> records the contract as built and the reasoning behind it. Deferred:
> [`gittarget-parent-empty-repository.md`](gittarget-parent-empty-repository.md) and
> [`gittarget-parent-observation.md`](gittarget-parent-observation.md).
> Related: [`push-notification-and-reconcile-trigger.md`](push-notification-and-reconcile-trigger.md),
> [`../definitions.md`](../definitions.md), and `architecture.md` (Ground rules; "A branch that does
> not exist yet").

**The use case.** A `GitTarget` stays on standby while the cluster matches its parent branch. Its
watches run, its write branch does not exist on the remote, and normal Git changes keep flowing
through the cluster's GitOps reconciler. When a captured cluster edit needs a commit, the target
creates its write branch from the parent's current tip. A target that waited a week must not
publish that edit on a week-old checkout.

```yaml
spec:
  branch: reverser/prod-edits   # the write branch (immutable)
  parentBranch: env/prod        # optional, immutable; omitted = the remote's default branch
```

**The constraint.** An idle, converged target creates no remote branch and no commit. Publication
checks the parent against the push session's advertisement and replays the captured writes if it
moved. If the replay leaves no difference, the target stays on standby. Commit windows and push
scheduling still apply. "No branch" means none on the remote; local working refs may exist.

**Independent of polling.** The contract holds with `--git-refresh-interval=0`, before the first
refresh tick and between ticks. Periodic refresh improves the idle view of Git; the publication
check is what protects the first write.

## Use cases

- **Capture an operational edit for review.** The cluster follows `main` and `reverser/prod-edits`
  is absent. An operator changes a Deployment during an incident. The edit is captured, the parent
  is checked when publishing, and the write branch is created for review. Commits that landed on
  `main` in the meantime are part of its starting history, and the replay re-plans the edit
  against that tree, including a changed file layout. Keeping the edit active in the cluster still
  depends on the GitOps reconciler's own policy.
- **Keep normal deployments quiet.** A deployment advances `main`, and the reconciler applies the
  same manifests. The watch events can first look like edits against the cached parent; after the
  check and replay they produce no difference, so no empty review branch and no catch-up commit
  is created. An idle refresh only reads and reports.
- **A release or environment branch.** A cluster following `release-1.x` or `env/prod` needs its
  proposed changes based on that branch. `parentBranch` says so. A set parent that is missing
  blocks branch creation.
- **Return to standby after review.** When the PR is merged and the write branch deleted, the next
  edit recreates it from the parent's current tip. While the write branch exists, new commits build
  on it; it is never rebased onto the parent.

## The contract as built

| Write branch on remote | Parent branch on remote | Behavior |
|---|---|---|
| yes | any | work on the write branch; the parent is not followed |
| no | yes | compare against the parent; create the write branch from its tip, checked at publication |
| no | no, **set** | refuse: `Ready=False`, `Stalled=True`, reason `ParentBranchNotFound`; nothing written, no orphan branch |
| no | no, omitted, not empty (`HEAD` resolves to no branch, or only tags) | refuse the same way: `ParentBranchNotFound`, with a message to fix `HEAD` or set the field |
| no | no, omitted, empty repository (no refs at all) | unborn `HEAD`; the first commit is a root commit on the write branch |

- **Recovery from a missing parent** is an obligation on the branch worker, not an observation. It
  latches when an attempt fails on the parent and clears only when the work is published. Writes,
  saves and resyncs decided while the parent is missing are kept in the worker's log, not dropped,
  and admission backpressure bounds the log
  ([`gittarget-branch-worker-log.md`](gittarget-branch-worker-log.md), step 3c). The worker probes
  with one advertisement after 10s, doubling to 5m, on its one retry schedule shared by every target
  on it; before a deadline nothing spends a connection on the parent, and reconciles never probe.
  Once found, it publishes the log. Meanwhile the target reports `RecoveringParentBranch`.
- **A parent change on a live worker** takes effect at a push admission boundary: every operation
  reads one `{name, generation}` snapshot, trust is scoped to the generation, and a push is admitted
  only when its root was chosen under the current one. A push already admitted is not recalled.
- **Default-branch resolution follows go-git**, which rewrites a hash-only `HEAD` before
  `Remote.List` returns (to `master` when it matches, else the alphabetically first branch at that
  hash). With the parent omitted, the parent is the default branch as last discovered; a refresh
  that sees `HEAD` switch follows it with one fetch, and a push, which cannot see `HEAD`, does not.
- **Cost.** Unchanged remotes keep the ledger's rows 1–12. The new rows: a standby no-op live write
  is 1 connection, the first write after the parent moved is 6 (row 6), the parent probe is 1 with
  no fetch, and a write branch created during our upload is 8.

- **Immutable**, including adding or removing it, like `branch` and `path`. It decides what history
  a new write branch gets. Delete and recreate the `GitTarget` to change it.
- **Not in `allowedBranches`.** It is only read.
- **`parentBranch == branch`** is legal when that branch exists. When it is absent, creation is
  refused, including in an empty repository. Only an omitted parent bootstraps.
- **One parent per write branch.** One worker owns one checkout, so two targets on the same
  provider and write branch that name different parents are a `TargetConflict`; the later one
  loses. Values are compared as written: omitted and `main` conflict even when the remote's `HEAD`
  is `main`. The answer needs no round trip and does not flip when the default branch changes.
- **`status.remote.parent`** shows, while the write branch is absent, what it would be created
  from: `state` (`Found`, `Missing` or `Unborn`), `branch`, and `commit` when found. It disappears
  once the write branch exists.

## The defect this started from

Before #407 the push already recorded the parent as the cycle's root, so that it could refuse a
moved parent. But `validatePushState` compared the root with its advertised tip only when the write
branch existed: the one case the root was recorded for was the one it skipped. That left three
holes, each now fixed and tested:

1. **Stale parent.** A warm worker that trusted its checkout of `main` at A created the write
   branch on A after `main` moved to B.
2. **A concurrently created branch.** If somebody created the write branch between our fetch and
   our push, our non-descendant commit could overwrite theirs.
3. **A no-op created the branch.** A window with no difference is still pushed, because only the
   remote can confirm "already present", and it created the write branch at the parent's commit.

The fix compares the root whether or not the pushed branch exists, rejects a write branch that
appeared, and settles a new branch whose tip is still the parent's as "no branch". Both
rejections take the existing replay path, and no connection was added: the advertisement is read
on the push session anyway.

**Rejected: distrust the base while the write branch is absent.** An earlier version did that and
was reverted for its cost, and it would still leave the gap between a fetch and the push.

**What is and is not guaranteed.** The parent is checked against the advertisement of the
successful push attempt. Creation is protected by the server's compare-and-swap with `Old = zero`.
Neither locks the parent, which can still move after the advertisement.

## Prior art

- **Flux `ImageUpdateAutomation`.** `spec.git.push.branch` "is created using
  `.spec.checkout.branch` as the starting point, if it doesn't already exist". The starting point
  matters only at creation; there is no rebase, and the usual remedy is to delete the branch after
  the PR merges.
- **Argo CD source hydrator.** Free-form revisions appear only where Argo reads; everything it
  writes to or creates a branch from is a typed `targetBranch`. `CheckoutOrNew(target, base)` uses
  the base at creation only. With no difference there is no commit (a git note instead).
- **git-town and Graphite** use "parent branch" for a relationship that includes synchronization.
  Ours is narrower: follow the parent only while the write branch is absent.

We took a branch-typed field at a write position (Argo), and the parent as a creation-time input
(both).

## Naming

[`definitions.md`](../definitions.md) binds here.

| Candidate | Problem |
|---|---|
| `sourceRevision` | "source" is the source *cluster* (`clusterProviderRef`, `rules[].sourceNamespace`) |
| `baseRevision`, `baseRef`, `baseBranch` | "base" already means a kustomize read-only base and base trust; `revision` is reserved for a polymorphic identifier; a `...Ref` field must reference an object |
| `checkout.ref.branch` | verbatim Flux, but a one-field block nested twice, and "checkout" names an action |
| `startPoint` | Git's own word, but it means creation only, and the field also governs what is followed |

The cost of the name: it can never hold a commit or a tag.

## Implicit when omitted, strict when set

Omitted is lenient: the default branch, and a root commit when the repository is empty, because
that is the first-install case. Set is strict: it must exist, or the target refuses. Which branch
an empty repository's first commit may create is the open question in
[`gittarget-parent-empty-repository.md`](gittarget-parent-empty-repository.md).

**Deferred, additive when someone asks:**

- **A deliberately empty write branch in a non-empty repository** (the `gh-pages` pattern; Argo's
  `CheckoutOrOrphan`). It would get its own field.
- **A pinned commit or tag.** Small value: the starting point only decides where the write branch is
  born, and a pin leaves nothing to follow. Fetching one commit by hash also needs server support.
  It would be a sibling field, never `parentBranch`.

## After a merge

Deleting the write branch after its PR merges returns the target to standby. A write branch left
on the remote is a separate policy question: its next change builds on that branch, which may have
fallen behind the parent, and after a squash merge a later PR can show old history. A candidate
rule is to reset the write branch to the parent's tip when it adds nothing to it, but resetting,
force-pushing or deleting is a mutation that needs its own authorization. It belongs with the
PR-creation design, not with status. What the operator can see about the relationship in the
meantime is [`gittarget-parent-observation.md`](gittarget-parent-observation.md).

## Open questions

1. Should each `GitProvider.status.branches` entry show its write branch's parent?
