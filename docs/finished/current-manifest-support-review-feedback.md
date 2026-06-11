# Architecture Review Feedback: Current Manifest Support Review

> Status: feedback on
> [current-manifest-support-review.md](current-manifest-support-review.md),
> captured 2026-06-04. Grounded against the actual code, including the parts the
> reviewed document does not mention.

## Verdict

The recommended direction — materialized model, dual identity index, first-class
plan, plan→apply→dirty-flush as the one write mechanism — is **right**, and it
should be pursued. The low-level reads are accurate: `manifestedit` is a good
mechanism layer; the writer is the part that "feels off."

But the document has **one major blind spot that undermines its own diagnosis**,
plus an **internal contradiction** in its most dangerous policy decision, and it
over-flattens settled decisions with open questions. Fix those three and this
becomes a strong directional doc.

---

## 1. The blind spot: the review never mentions `FolderReconciler`, which is where the real disease lives

The doc frames the problem as "the live writer in
[git.go](../../internal/git/git.go) processes events one at a time" and
proposes making the inventory authoritative *in the writer*. But the writer is
**downstream** of the actual reconcile engine, which the doc never names:
[folder_reconciler.go](../../../internal/reconcile/folder_reconciler.go).

What is actually happening today is worse than "event-by-event," and in a way
that strengthens the case for the rewrite. There are **three** independent
"compare git ↔ desired" engines using **two incompatible identity models**:

| Engine | Identity model | Git side sourced from |
|---|---|---|
| `FolderReconciler.findDifferences` (the *real* production diff) | **GVR** `ResourceIdentifier` | **path-parsed** via `parseIdentifierFromPath` |
| `manifestLocator` + `reconcileAgainstExisting` (the writer) | **content GVK** `manifestedit.Identity` | `manifestedit.Inventory` (content scan) |
| `manifestreport.BuildReport` (read-only, ~dead in prod) | content GVK | `manifestedit.Inventory` |

The headline problem is not "event-by-event." It is that **the authoritative
diff (`FolderReconciler`) decides creates and deletes from path-derived GVR
identity** (`listResourceIdentifiersInPath` → `parseIdentifierFromPath`), then
**hands a flat `[]git.Event` to the writer, which re-scans the same tree by
content identity** to place each one. The two scans happen at different layers,
at different times, against possibly different commits, with two different
notions of "what resource is this."

That is the actual reason "DELETE placement is incomplete when delete events only
carry GVR/name" (Cons & Gaps in the reviewed doc): a manifest moved off its
canonical path is *invisible to the reconciler's git scan entirely* —
`parseIdentifierFromPath` derives identity from the path, so a moved file either
parses to the wrong identity or not at all. The reconciler then emits a CREATE
(duplicate at the canonical path) while never seeing the moved copy. The writer's
content-based locator cannot save it because the reconciler already made the
wrong decision upstream.

**The proposed `ManifestStore` with both `ByManifestIdentity` and
`ByResourceIdentity` is exactly the fix** — but the doc presents it as "promote
the locator cache," when what it really does is **collapse three engines and two
identity models into one**. Say that. It is a much stronger justification than
the one currently written, and it tells the reader that `parseIdentifierFromPath`
and `listResourceIdentifiersInPath` are the things being deleted.

The "Current Architecture" mermaid reinforces the blind spot: it draws the
writer, `manifestedit`, and `manifestreport`, but omits `reconcile`, `watch`, and
`events` — i.e. it omits the layer that actually owns the diff. Redraw it to
include `FolderReconciler` and the cluster/repo-state control events, or the
diagram is describing a subsystem, not the system.

## 2. Where the store and plan must live — the layering question the doc doesn't resolve

Because the doc skips `FolderReconciler`, it never confronts a hard fact: **the
desired/cluster state lives in the reconcile layer** (`clusterObjects`, already
"cluster-as-source-of-truth"), while **the worktree lives in the writer**. The
doc's data structures put `ManifestStore` "owned by the writer or a new
integration package" and place the plan there too — but the writer only receives
events; it has no cluster snapshot to plan against.

The clean answer that matches the existing `fs.FS` purity in
[analyzer.go](../../internal/manifestanalyzer/analyzer.go) is to make the
**Plan the cross-layer contract**, computed from pure inputs:

```text
ManifestStore = f(fs.FS)                              // pure, no cluster, no runtime — already exists
Plan          = f(ManifestStore, desiredSet, policy)  // pure — this is BuildReport, graduated
applied       = f(ManifestStore, Plan)                // pure mutation
Flush         = f(applied, worktree)                  // the only side effect
```

Then the *reconcile layer* (which has the cluster snapshot) builds the store and
computes the plan; the *writer* becomes a dumb "apply plan + flush"; *scan mode*
is "compute plan, render, don't flush"; the *CLI* is "compute plan, render";
*status* is "summarize plan." All four consumers fall out of one pure function.
The doc gestures at this ("the plan is already a first-class value") but its
struct placement quietly contradicts it by nesting store+plan inside the writer.
Resolve it in favor of the pure boundary and push it **up** to the layer that
owns desired state.

Corollary the doc should state: **the plan is valid only for a `(commit SHA,
cluster snapshot revision)` pair.** Today `FolderReconciler` waits for two
independently-delivered async events (`OnClusterState`, `OnRepoState`) and
reconciles when both have *ever* arrived — there is no shared revision. The
"single repository transaction" trust model borrowed from `BuildReport` is the
right instinct; extend it to pin both sides to one snapshot, or plan-then-flush
will flush a plan computed against a stale tree.

And say it plainly: **`manifestreport.BuildReport` is not a throwaway — it
graduates into the Plan computation.** It is already 80% of the planner
(create/update/delete/skip, duplicate losers, non-editable handling) and is
currently near-dead in production (only `EditInPlace` is called live). The doc
treats the *analyzer* as the seed of the model; `BuildReport` is the seed of the
*plan*. Connect them.

## 3. The internal contradiction: "prune unwatched KRM by default" vs "refuse by default"

This is the riskiest decision in the doc and it contradicts itself:

- **Acceptance Checks:** "**Unwatched (bucket 4) is pruned.** … We deliberately
  choose pruning over the safer-looking 'leave it inert' option." Stated as *the
  rule*.
- **Adoption Policy:** "`refuse` (safest, good default for first materialization)"
  and "`prune` … should be opt-in."

These cannot both be the default. As written, a literal reader implements "delete
every KRM doc with no matching watched API resource" — which **deletes
`kustomization.yaml`, every unwatched CRD, every Flux `Kustomization`/`HelmRelease`
not watched, sealed secrets, etc.** in any real GitOps repo. The doc notices the
`kustomization.yaml` edge but treats destruction as the default and preservation
as the escape-hatch allowlist. That is the safety polarity inverted.

Strong recommendation: **make unwatched-KRM pruning opt-in per GVK (a
prune-allowlist of kinds you are willing to delete), not opt-out with an
exemption-allowlist.** Reasons: (a) it matches the doc's own `refuse` default;
(b) the analyzer already proved how fragile "valid KRM" classification is — the
`generateName`-only object gets misfiled as non-KRM today, and a misclassification
on the prune path *deletes data*; (c) the source-of-truth conviction justifies
pruning resources you *manage*, but an unwatched kind is by definition one you
have made no claim over — deleting it asserts authority you explicitly declined to
take. Keep the strong conviction for *watched* GVKs; downgrade unwatched to
"report, prune only on explicit per-kind opt-in."

Separately: the conviction "the API is the source of truth, git is a pure
projection" is stated as "non-negotiable," but it is a **product decision**, not
an architectural one, and it is in tension with the tool being usable on a
Flux-consumable repo that mixes generated and hand-authored manifests. The
architecture should *support* the strict-projection mode without *mandating* it.
Frame it as a configurable posture (the `refuse`/`scan`/`prune` settings already
exist), not a conviction the data model bakes in.

## 4. Smaller but real

- **The RESTMapper / GVK↔GVR layer does not exist yet.** Grep finds `RESTMapper`
  only in comments and docs, never in code. The doc leans on "resolve GVK to
  watched GVR using the watch/catalog/RESTMapper layer" as if it is a layer to
  call; it is a layer to *build*, and it must satisfy the analyzer's no-cluster
  constraint (so it is an injected interface with a live-informer impl, a
  kubeconfig impl, a static-snapshot impl, and a nil "structure-only" impl). This
  is phase 2 in the doc but it is underweighted — it is the hardest
  dependency-injection problem in the whole plan and everything in phases 3–7
  depends on it. Promote it and spec the interface.

- **Full materialization has an unbounded memory/CPU cost the doc waves away.**
  `FileModel` holds `Original` + `Current` full bytes for *every* file, and the
  plan says build node trees for all documents. For the cluster-wide CRD watch
  the doc itself cites as the perf motivation, that is the entire tree in memory
  and fully parsed per batch. Recommend bounded/lazy materialization: identity
  indexing needs only a cheap header parse (`apiVersion`/`kind`/`metadata`), and
  the expensive `manifestedit` node tree should be built **only for documents a
  plan action touches**. Keep this as an explicit design constraint, not a
  "phase 9 optimization" — it changes the `DocumentModel` shape (a
  `SnapshotRef`/lazy handle rather than eager bytes), so retrofitting it later is
  exactly the kind of rewrite the doc says it wants to avoid.

- **The doc reads at uniform confidence across settled and unsettled decisions.**
  Plan-then-flush, dual index, dirty-flush, "BuildReport graduates" — these are
  settled and well-argued. Prune-unwatched default, kustomization allowlist,
  refuse-vs-prune default — these are open product questions. Right now they are
  written with the same authority, which makes the genuinely strong parts easier
  to dismiss. Split into a short **Decisions (non-negotiable)** block and an
  **Open policy questions** block. For a doc whose goal is "strong architectural
  direction," that separation *is* the strength.

## What to change in the reviewed doc

1. Add `FolderReconciler` + the path-derived/content-derived dual-scan problem as
   the headline diagnosis (and fix the architecture mermaid to include the
   reconcile/watch/events layer).
2. Resolve the store/plan layering explicitly toward the pure `f(fs.FS)` → `Plan`
   → apply → flush boundary, with the plan as the cross-layer contract and
   `BuildReport` named as its origin.
3. Fix the prune default contradiction and invert the unwatched-prune safety
   polarity to opt-in-per-GVK.
4. Pull the RESTMapper/source abstraction and bounded materialization up into
   first-class constraints.
5. Split Decisions vs Open Questions.

## Code references

- [`internal/reconcile/folder_reconciler.go`](../../../internal/reconcile/folder_reconciler.go)
  — the real production diff engine (cluster-as-source-of-truth), unmentioned by
  the reviewed doc.
- [`internal/git/helpers.go`](../../internal/git/helpers.go) —
  `parseIdentifierFromPath`, the path-derived GVR identity to be deleted.
- [`internal/git/branch_worker.go`](../../internal/git/branch_worker.go) —
  `listResourceIdentifiersInPath`, the path-derived git scan.
- [`internal/git/git.go`](../../internal/git/git.go) — `manifestLocator` and
  `reconcileAgainstExisting`, the content-derived second scan.
- [`internal/manifestreport/report.go`](../../internal/manifestreport/report.go)
  — `BuildReport`, the near-dead planner prototype to graduate.
- [`internal/manifestanalyzer/analyzer.go`](../../internal/manifestanalyzer/analyzer.go)
  — the existing pure `f(fs.FS)` boundary to extend.
