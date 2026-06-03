# Step 6: the read-only, inventory-driven reconcile

> Status: in progress (the read-only report is implemented; writer wiring is deferred)
> Related: [manifestedit-abstraction-plan.md](manifestedit-abstraction-plan.md)
> (step 6), [manifestedit-field-ownership-spike.md](manifestedit-field-ownership-spike.md),
> [manifest-inventory-file-agnostic-placement.md](manifest-inventory-file-agnostic-placement.md),
> [../architecture.md](../architecture.md),
> POC decision record: `internal/git/manifestedit/DECISION.md`

This is the integration milestone. Steps 1–5 built and proved a cluster-free,
well-tested comparison library (`internal/git/manifestedit`) plus the policy
decision (API-first, whole-object truth). Step 6 connects it to the real world —
but **read-only first**: a reconcile that *reports* what it would add, remove, or
update against a real cluster, consuming the library unchanged, before anything
is allowed to write or prune.

The point of read-only-first is trust. The comparison must be demonstrably
correct end to end — over real repositories and real cluster state — before it is
wired into the commit path, where a wrong "delete" is a destroyed file.

## What is built now

Package `internal/manifestreport` is the integration layer. It supplies the two
pieces of policy `manifestedit` deliberately refuses to own, and a read-only
reconcile:

- **`Project`** = `sanitize.Sanitize` — the Git projection (what "clean" means).
- **`Render`** = `sanitize.MarshalToOrderedYAML` — the house canonical renderer,
  the *same* function the live writer uses
  ([content_writer.go](../../internal/git/content_writer.go) `buildContentForWrite`).
  A contract test (`render_contract_test.go`) pins that whole-replace/new-file
  output is byte-identical to what the writer commits, so the two cannot drift.
- **`EditOptions`** = production options: the house renderer, index-based list
  matching (no global keyed strategy — see below), and `Owns == nil`
  (whole-object truth).
- **`BuildReport(files, desired)`** indexes the Git folder, compares it to the
  desired cluster state, and returns a `Report` of per-resource `Action`s
  (`no-change` / `update` / `create` / `delete` / `skip`). It uses
  `manifestedit.Decide` **only** — never `Apply` — so it cannot mutate Git or
  reach the writer. It is the set-difference reconcile made observable.

The report maps every cell of the two-version comparison:

| Cluster (desired) | Git (inventory) | Report action |
|---|---|---|
| present | absent | `create` (placement is upstream; only flagged) |
| present | present, equal | `no-change` |
| present | present, different | `update` |
| present | present, encrypted/disallowed | `skip` (route elsewhere; never in-place) |
| absent | present (authoritative) | `delete` (prune candidate) |
| — | present (duplicate loser) | `delete` (prune candidate) |
| — | present (non-editable) | `skip` |

## The Git transaction boundary (read this before wiring writes)

The folder index is **only** trustworthy for the exact bytes of a single
checked-out commit. An inventory built from one worktree and applied against a
different remote tip is a stale decision — and `manifestedit`'s snapshot
validation (index + identity + body hash) will reject the individual edit, but
the *set-level* verdicts (what to create, what to prune) are equally
snapshot-bound. The whole report is valid only for the commit it was built from.

So any future writing reconcile must run as one repository transaction, and this
ordering is not optional:

1. **Fetch/checkout** the target branch to a clean worktree (the existing
   `BranchWorker` clone, see [architecture.md](../architecture.md) §Git Operations).
2. **Index** that worktree's files for the GitTarget's path → `Inventory`.
3. **Compare** against the current desired cluster state → `Report` (this package).
4. **Edit** via `manifestedit.Apply` per entry, against the *same* worktree bytes
   the report was built from. `Apply`'s snapshot check is the per-document guard
   that the worktree did not shift mid-transaction.
5. **Commit** the resulting tree.
6. **Push with lease** (compare-and-swap on the remote ref): if the remote moved,
   the push is rejected — do **not** force.
7. **On rejection, discard and replay**: re-fetch, re-index, re-compare, re-edit.
   This is the project's existing "checkout fresh + replay" strategy
   ([git_atomic_push.go](../../internal/git/git_atomic_push.go),
   [git_smart_fetch.go](../../internal/git/git_smart_fetch.go)), and it is safe
   precisely because the API is the source of truth: any stale commit can be
   regenerated from current object state.

The single-writer-per-branch invariant ([BranchWorker](../../internal/git/branch_worker.go))
already serializes step 1–6 within a pod; push-with-lease covers the cross-pod /
external-writer race. The report layer must never assume it is the only writer:
it produces verdicts for a snapshot and lets the transaction boundary enforce
freshness.

## Deliberately deferred

Per the plan, step 6 does not yet take on:

- **The prune hazard.** `delete` is only *reported*. Automatically removing files
  for "absent from cluster" is dangerous when the cluster view is partial (a
  degraded discovery or an incomplete snapshot looks like mass deletion — see
  [architecture.md](../architecture.md) §Watch / Informer System on partial
  snapshots). Acting on `delete` needs the same partial-state guards the existing
  `FolderReconciler` already reasons about, wired explicitly.
- **GVK→GVR mapping.** The inventory keys on manifest identity (GVK + name +
  namespace), not the API-side GVR. Turning a desired-set difference into actual
  cluster reads/writes needs the RESTMapper-backed mapping tracked in
  `docs/TODO.md`. Read-only reporting against a caller-supplied desired set sides
  steps this until the comparison is trusted.
- **Writer wiring.** No change to `commit_executor.go` / `branch_worker.go` yet.
  When it comes, it consumes `manifestreport` + `manifestedit`; it does not change
  them.

## Notes and non-goals

- **No global keyed list matching.** Production `EditOptions` keeps list matching
  index-based. A blanket `ListMatch.KeyField: "name"` would silently change every
  named mapping list's behavior; keyed matching should arrive with a
  path/GVK-aware strategy chosen above the merge, not a global default.
- **Relationship to `FolderReconciler`.** The existing
  [FolderReconciler](../../internal/reconcile/folder_reconciler.go) diffs cluster
  vs. Git for the initial snapshot and emits a whole-file `WriteRequest`. The
  manifestedit path is the finer-grained, in-place, formatting-preserving successor
  for *editing existing documents*; this read-only report is the bridge that lets
  us validate it against the same inputs before swapping anything.
- **Whole-object truth end to end.** Because Git was written by `Render(Project(obj))`
  and the report compares against `Project(obj)`, a freshly mirrored resource
  reports `no-change` and round-trips byte-stably — the convergence property from
  step 3, now observed across the integration boundary.
