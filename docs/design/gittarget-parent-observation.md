# GitTarget parent observations with shallow history

> **design**: deferred follow-up to PR #407; nothing in this document is implemented.
> Date: 2026-10-01.

Keep the parent branch visible after the write branch exists. Reuse remote ref advertisements to
report its current tip, then use bounded local history checks to describe the relationship where
the evidence allows it. An incomplete history produces an explicit `Unknown` result.

The cost boundary is part of the design: no extra remote connection, history fetch, or deepening
for this status feature. Parent observations are informational. Existing publication checks still
decide whether a write can proceed.

## Why keep observing the parent

An operator needs to see whether an existing write branch still includes the parent's current
tip. The parent can advance, rewind, disappear, or acquire unrelated history while the write
branch remains active. Dropping the parent from status removes that context precisely when the
branches can start to differ.

PR #407's [API types](../../api/v1alpha3/gittarget_types.go) describe `status.remote.parent` only
while the write branch is absent. Its `Found`, `Missing`, and `Unborn` states explain where a new
branch would start. This proposal retains that observation after creation and adds a separate
ancestry result.

The parent remains the branch selected by `spec.parentBranch`, or the advertised remote default
branch when that field is omitted. This describes the current comparison target. Git does not
record a branch's historical creation parent, and this status makes no such claim. A change to
the remote default branch changes the comparison target, even if its tip hash stays the same.

## What the existing operations can tell us

The [remote refresher](../../internal/git/refresh.go) already reads the write branch and parent
from one ref advertisement. Keeping both observations adds no object transfer. The advertisement
still has network and ref-listing costs, so reuse the existing refresh cadence and write-related
observations rather than adding another poll.

Two advertised hashes prove equality or inequality. They provide no history explaining the
difference. A changed parent hash can mean an advance, a rewind, or a force push. Calling all three
`ParentAhead` would give operators the wrong information.

[`SmartFetchFrom`](../../internal/git/git_smart_fetch.go) fetches with `Depth: 1`. That limits
downloaded history, but local commits can extend the fetched tip:

```text
older history unavailable <- A <- W1 <- W2
                            ^          ^
                       parent tip   write tip
```

If the objects from `W2` back to `A` remain available, their parent links prove that the write tip
includes `A`. There is no need to fetch the history before `A` for that answer.

There is a second boundary: Git's normal revision walk treats commits listed in `.git/shallow`
as roots, even though their commit objects retain parent links. See
[Git's shallow repository documentation](https://git-scm.com/docs/shallow). An exploratory check
with system Git 2.39.5 found that fetching the pushed write tip again with `--depth=1` shortened
the visible log to that tip, while an earlier base object remained readable with `git cat-file`.

This suggests inspecting locally stored commit objects directly for positive ancestry evidence.
The experiment does not establish the behavior of the repository's `go-git/v6` dependency; that
needs a focused test before implementation. Object retention is also optional: a new checkout,
repository replacement, or cleanup can remove evidence that a long-lived worker once had.

## Proposed status contract

Parent availability and ancestry answer different questions and use separate fields.

### Parent availability

Retain `status.remote.parent` after the write branch exists. Its existing fields continue to name
the selected parent and its last observed tip, with a parent-specific observation timestamp.

| State | Meaning |
|---|---|
| `Found` | The selected parent branch was advertised with a commit. |
| `Missing` | The selected parent branch was absent from a successful advertisement. |
| `Unborn` | The repository was empty and no explicit parent was configured. |

`Missing` blocks creation only while the write branch is absent, preserving PR #407's behavior
for an explicit parent. An existing write branch stays writable when its parent disappears.
`Unborn` must not become a catch-all for a nonempty repository with an unresolved default branch.
That case needs an explicit observation reason and no ancestry comparison.

A failed advertisement does not prove that a branch is missing. Keep the last successful
observation and its timestamp; existing remote-error reporting describes the failed attempt.
Before the first successful observation, leave the parent observation absent.

### Ancestry relationship

Compare the advertised remote write tip with the advertised parent tip. Local `HEAD` can include
unpublished commits and must not substitute for the remote write tip.

The recommended first implementation has four outcomes:

| Relationship | Required evidence |
|---|---|
| `SameTip` | Both advertised hashes are equal. |
| `WriteAhead` | Distinct tips; a parent-link path leads from the write tip to the parent tip. |
| `ParentAhead` | Distinct tips; a parent-link path leads from the parent tip to the write tip. |
| `Unknown` | Available evidence and the work budget do not establish one of the above. |

These values describe ancestry, not elapsed time or the number of commits. A parent rewind can
produce `WriteAhead`. Following a merge's second parent can also establish ancestry. Commit author
names and timestamps provide no substitute for parent links.

`Unknown` carries a reason, such as `HistoryIncomplete`, `BudgetExceeded`, or
`RelationshipUnclassified`. The last reason covers a completed local check whose result falls
outside the initial enum. An object read failure also leaves the relationship unknown and must
retain a diagnostic; it cannot establish a negative ancestry result.

When either branch has no observed tip, omit the comparison. The parent availability and write
branch observation already explain why comparing two commits is inapplicable.

### Bind the result to the evidence

Store the exact compared hashes and the time of their joint remote observation with the result.
An illustrative layout is below; field names remain subject to API review. Hashes are shortened
for readability, but status stores complete object IDs.

```yaml
status:
  remote:
    commit: "b4e29d1"
    lastVerifiedAt: "2026-10-01T12:00:00Z"
    verifiedBy: Fetch
    parent:
      state: Found
      branch: main
      commit: "a73c012"
      lastVerifiedAt: "2026-10-01T12:00:00Z"
      comparison:
        relationship: WriteAhead
        writeCommit: "b4e29d1"
        parentCommit: "a73c012"
        observedAt: "2026-10-01T12:00:00Z"
```

A successful push verifies the new write tip. It does not by itself verify the parent's tip at
that same time. Preserve the parent's own observation time and clear the comparison when its
inputs no longer match. The next suitable advertisement can establish a new comparison. Never
refresh the parent timestamp solely because `status.remote.lastVerifiedAt` changed.

## Bounded local comparison

Run comparison as observation work, with finite limits on visited commits, queued parent edges,
and elapsed time. Support cancellation and serialize object reads with repository replacement or
cleanup. Choose internal limits using measurements before implementation; this proposal adds no
operator configuration for graph-walk budgets.

1. Resolve both branch names and hashes from the same successful advertisement. If either tip
   is absent, publish availability and omit the comparison.
2. Return `SameTip` immediately for equal hashes. No commit objects are needed.
3. Look for a cached result for this repository identity, resolved branch pair, and exact tip pair.
4. Walk locally available parent links from the write tip, looking for the parent hash. A found
   path proves `WriteAhead`. Check the reverse direction within the same total budget for
   `ParentAhead` when needed.
5. Stop on proof or budget exhaustion. A missing object leaves that path incomplete; other queued
   paths may still prove ancestry. If neither direction supplies proof, publish `Unknown` and
   the reason for the limit on the result.

Bound the cache as well as the walk. Successful proofs for an unchanged pair can be reused after
a fresh advertisement. Cache unknown results only while the local object store has unchanged
evidence: a normal fetch may make the same pair answerable. Budget-limited results should not
start an immediate retry loop. Discard work whose repository or observed tips changed before the
result was published, and invalidate the cache when the repository identity changes.

The comparison must not extend the write critical path without a bound or block readiness.
Continue existing status-write throttling, while publishing changed facts promptly. With
`--git-refresh-interval=0`, observations age until another existing operation reads the refs;
this feature does not introduce a replacement timer.

## Why a missing overlap remains unknown

A bounded search that finds no shared commit cannot distinguish missing history from histories
that never intersected. Even finding a shared commit does not by itself prove divergence: one tip
could still be an ancestor of the other along an unexplored merge parent.

Two additional classifications are useful but deferred from the first implementation:

| Relationship | Proof required before reporting it |
|---|---|
| `Diverged` | A common ancestor exists, and neither tip is an ancestor of the other. |
| `Unrelated` | Both complete ancestor graphs have been explored with no common commit. |

Negative proofs require exploring every relevant parent edge or an equivalent sound proof. An
unresolved shallow boundary, missing object, or exhausted budget prevents that conclusion. Neither
outcome justifies downloading more history solely to fill status.

There is also no requirement to publish the first overlapping commit. Git's graph can contain
multiple best common ancestors, so the first intersection found by a walk is not a reliable
merge-base contract. See [Git's merge-base documentation](https://git-scm.com/docs/git-merge-base).
The initial status needs only the relationship evidence above.

## A cheaper alternative: compare with a remembered starting tip

A worker could remember the verified parent hash used when it creates or rebuilds an absent write
branch, then compare later parent advertisements with that hash. This needs no graph walk:
`Unchanged`, `Changed`, or `Unknown` would describe movement relative to that recorded starting tip.

This is a different question from ancestry. `Changed` cannot say whether the parent advanced,
rewound, or was replaced, nor whether the write branch later incorporated its new tip. The saved
hash also needs provenance tied to the repository and the particular branch creation. External
branch replacement, deletion and recreation, or lost worker state can invalidate that provenance.

Keep this as an alternative if bounded local ancestry proves too costly or rarely informative.
Do not add both models in the first implementation. An in-memory starting tip is lost on restart;
durable origin tracking would need a separate design and must not be reconstructed by guessing.

## Operational boundaries

Parent comparison is status only. It does not merge, rebase, reset, delete, or recreate an existing
write branch. `Unknown` and a missing parent after branch creation do not change `Ready`.
The publication checks and replay behavior from PR #407 continue to protect creation of an absent
write branch independently of these observations.

Exact ahead/behind counts, pull request management, and content-equivalence checks are outside
this proposal. A squash merge can integrate the same changes while preserving distinct ancestry;
this status must not claim that a branch is merged or that its changes remain unapplied.

Extending the lifetime of `status.remote.parent` changes its documented meaning. Implementation
must update API comments, generated CRD descriptions, and user documentation together. Consumers
must use the remote write commit to determine branch presence; parent visibility no longer
implies write-branch absence.

## Implementation and validation

Ship it in two steps, on a follow-up branch after PR #407:

1. **Parent availability and timestamps**: retain the parent observation through the existing
   refresh path after the write branch exists, and project it into status. This needs no history.
2. **Ancestry classification**, only after measuring how often the shallow checkout can answer
   it. If most results come out `Unknown`, the graph walk is worth less than it looks, and the
   remembered starting tip above may be enough.

Before step 2, test the shallow-object behavior with the pinned `go-git/v6` version.

Status is informational only. Nothing here authorizes resetting, force-pushing or deleting a write
branch; that stays with the PR-creation design.

The implementation needs these cases:

- Equal advertised tips with no local objects; an absent write branch; an empty repository.
- Parent disappearance with and without an existing write branch; failed advertisements that
  preserve the last successful observation; an unresolved default branch in a nonempty repository.
- A shallow base followed by local commits, including another depth-one fetch after publication.
- Restart with only the two tips available, yielding `Unknown` when intermediate objects are absent.
- Positive ancestry through a merge's second parent; no false divergence at a missing object.
- Both ancestry directions, a parent rewind, a force push, and a parent branch rename or default
  branch change, including different names that resolve to the same hash.
- Count, memory, time, and cancellation limits; an unchanged tip pair becoming answerable after a
  normal fetch; cache invalidation after repository replacement.
- A local unpublished commit and a later successful push, proving that comparisons never silently
  switch from remote evidence to local `HEAD` or acquire an unearned freshness timestamp.
- Remote call and fetch counts showing no additional network work, unchanged readiness, and no
  status-driven Git mutations with periodic refresh enabled or disabled.

Before implementation, settle the exact field names, the representation of an unresolved default
branch, and measured internal work limits. Those choices must preserve the cost boundary and the
distinction between an unavailable answer and a proved relationship.
