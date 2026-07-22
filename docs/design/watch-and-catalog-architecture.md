# Watch & Catalog Architecture — Requirements, Current State, Target Design

> **design** — open, not yet built. Index: [`../INDEX.md`](../INDEX.md)

Status: **vision / proposed** — supersedes the framing in
[watchrule-wildcard-support-plan.md](../spec/type-followability.md),
[watchrule-wildcard-and-resolution-semantics.md](../spec/type-followability.md),
and [rule-set-snapshot-discovery-lag-fix.md](../spec/typeset-owns-discovery-grace.md);
those become implementation details of the model described here.

This document is in three parts:

1. **Requirements** — how it *should* work, including requirements not yet
   written down anywhere.
2. **Current architecture** — what exists today and where it falls short.
3. **Target architecture** — one clean model and how we get there. A large
   refactor is acceptable; correctness and simplicity win over diff size.

---

## Part 1 — Requirements: how it should work

## 1.0 One sentence

The application keeps a **live, authoritative model of the cluster's API
surface**, and every `GitTarget` mirrors a **deterministic projection** of that
surface (selected by its rules) into git, reacting to surface changes
**incrementally** and **never writing an uncertain or partial view**.

## 1.1 The catalog is the single source of truth

There must be exactly one component that knows "what does this cluster currently
serve, and can I trust that knowledge right now." Everything else
(resolution, informers, snapshots, status) asks the catalog; nothing else talks
to discovery or guesses.

The catalog must:

- **Maintain last-known-good.** A `GroupVersion` that blips out during an
  apiserver rollout or CRD upgrade must not vanish from the model the instant
  discovery hiccups. The catalog keeps the prior entries and marks the GV
  *degraded*, not *gone*.
- **Distinguish the three reasons a resource can be "absent":**
  1. *Never served* / not yet served (stable).
  2. *Degraded* — discovery temporarily can't confirm it (transient).
  3. *Withdrawn* — confirmed removed from the surface (e.g. CRD uninstalled).
  These three drive completely different downstream actions, so the catalog must
  label them, not collapse them to "empty".
- **Debounce withdrawal.** A GV is only promoted from *degraded* to *withdrawn*
  after a confirmation window / repeated clean discovery that still omits it.
  This is what "abstracts away shortly-unavailable CRDs" means concretely.
- **Expose confidence.** A consumer can always ask "is the surface authoritative
  enough to make a destructive (delete) decision right now?"
- **Emit deltas.** Consumers react to *changes* (GVR added / withdrawn / degraded
  / recovered), not by recomputing the world each tick.

## 1.2 Wildcards behave like Kubernetes policy APIs (non-negotiable)

Skipping wildcards is **not** an option. `WatchRule`/`ClusterWatchRule` selectors
must follow the admission-webhook / RBAC / audit-policy idiom that users already
know:

- `apiGroups: ["*"]` matches all groups; `resources: ["*"]` matches all
  resources; `apiVersions: ["*"]` matches all versions.
- A `*` rule is a *live* selection: when a new CRD appears that matches it, the
  target starts watching it automatically; when one is withdrawn, it stops.
- The selector is a **projection of the catalog**, evaluated continuously — not a
  one-time literal expansion frozen at apply time.

Decisions to lock (see open questions): the meaning of *empty* vs `*` (proposal:
empty `apiGroups` = "resolve to the single served group, else surface an error";
`["*"]` = "all served groups" — a strict mode and a wildcard mode, both useful),
and that *subresources* (`pods/log`, `pods/*`) are **out of scope** for the
list/watch+mirror model and must not be advertised.

## 1.3 Incremental, relaxed reconcile

An active `GitTarget` must respond to a newly added or removed CRD by changing
**only the part that changed**:

- New GVR matches a target's plan → start one informer for that GVR, seed **only
  that GVR's objects** into the mirror, done. No re-snapshot of the rest of the
  target.
- GVR withdrawn → stop that one informer; apply the configured withdrawal policy
  to only that GVR's files.

The unit of reconciliation is the **(GitTarget, GVR) cell**, not the whole
GitTarget. Rationale (the user's, and worth stating as a requirement): because
live audit/event coverage is high-quality, a full-target re-list is rarely needed
— the only thing a new GVR genuinely needs is its *initial seed*; steady state is
maintained by events. A full re-snapshot should be a **fallback**, not the
normal path.

## 1.4 The authoritative-write invariant (the core safety property)

The mirror must **never** be written from a partial or uncertain view, because a
resource that is merely *unconfirmed* is indistinguishable from a *deleted* one,
and writing the difference would mirror phantom deletions into git.

Concretely:

- A delete is only ever written for a cell whose source-of-truth was
  **authoritatively observed** (a successful list of that GVR, or a real DELETE
  event for that object) **or** whose removal was **intent-driven** (the rules no
  longer select it — see 1.6).
- If a GVR's list fails, its GV is degraded, or the controller lacks permission,
  that cell is **left untouched and retried** — an *inability to observe* must
  never be read as a deletion.
- The dividing line is **intent vs observability**: a cell leaving the desired
  set because of a rule/scope/policy change is an intent-driven mutation and may
  delete; a cell that is merely unobservable right now must hold.
- This invariant is per-cell, so one flaky GVR cannot corrupt an entire target.

## 1.5 Convergence, determinism, idempotency

- **Deterministic projection.** A target's desired mirror is a pure function of
  (its rules, the authoritative catalog surface, cluster object state). Same
  inputs → same files. This is what makes the plan-hash and tests trustworthy.
- **Convergence.** After any transient disruption (discovery gap, list failure,
  permission blip), the system returns to the correct mirror automatically within
  a bounded time, with no operator action and no spurious churn commits.
- **Idempotency.** Re-running a reconcile with unchanged inputs produces no
  commit.

## 1.6 A cell can disappear for several reasons; the cause decides the action

This is the most important semantic to get right. "Cell removed" is **not** one
case — and collapsing them all into one policy is wrong. A cell can leave a
target's desired set because of an **intent** change or because of an
**observability** change, and the mirror must treat those oppositely:

| Cause | Class | Mirror action |
|---|---|---|
| Rule deleted / narrowed / ops changed so the cell is no longer selected | intent | **Delete** the cell's managed files — required to keep the mirror a deterministic projection of the rules. |
| Namespace / scope change | intent | Old cell deselected → delete its managed files; new cell selected → seed it. |
| GitTarget rewiring (provider/branch/path change) | intent | Projection moves: delete from the old path, seed at the new one. |
| Resource policy now excludes the kind | intent/config | Likely **delete** (open question 6) — it is a config-driven deselection, not an outage. |
| Surface withdrawal (CRD uninstalled) | observability | **Keep** files (proposed) and record withdrawal in status/log. Absence is an operator surface change, not GitOps intent; deleting hundreds of files on a CRD upgrade/reinstall would be destructive and noisy. |
| Object deleted while kind still served | observability (authoritative) | Real deletion, driven by DELETE event / authoritative re-list → **delete** the file. |
| GV degraded / list failed / permission denied | observability (non-authoritative) | **Hold** the cell — never delete (see 1.4). |

The principle: **only intent-driven removals and authoritatively-observed
deletions mutate the mirror; non-authoritative absences hold.** The reconciler
must therefore know *why* a cell left the desired set, not merely *that* it did.

This requires a notion of **managed projection** (see 1.7): the set of files a
given cell owns, so "delete the cell's files" is well-defined and scoped.

## 1.7 Requirements you may be missing

Beyond the four beliefs above, a complete design needs:

- **Deterministic file ownership (managed projection).** There must be a
  deterministic mapping from a cell `{GitTarget, GVR, scope, namespaces}` to the
  set of repo paths it owns, and a way to enumerate/diff *only* those paths. This
  underpins two things at once: deleting exactly a deselected cell's files (1.6),
  and **cell-scoped repo diffing** so a per-cell reconcile never has to read or
  rewrite the whole GitTarget path (which would smuggle whole-target behavior
  back in). Files not owned by any current cell are "unmanaged" and are never
  touched unless an intent change brought them into scope.

- **RBAC / least privilege.** "Watch everything" means the controller's
  ServiceAccount must `list`/`watch` every selected GVR. A wildcard implies broad
  RBAC. Required: either a documented broad ClusterRole, or per-GVR
  permission-denied handling that degrades that *cell* gracefully (treat as
  "cannot observe" → never delete, surface in status) instead of failing the
  target.
- **Scale bounds & backpressure.** One informer per selected GVR across the whole
  surface is real memory + apiserver load, and a CRD burst can cause a list
  storm. Required: a soft cap with observability, and coalescing of surface
  bursts (the existing single-slot refresh channel is a start).
- **Startup gating.** On cold start, do not snapshot (and certainly do not delete)
  until the catalog has reached initial trust — otherwise an empty early surface
  could wipe the mirror.
- **Observability / status conditions.** `WatchRule`/`ClusterWatchRule`/`GitTarget`
  status must show: resolved GVR count (incl. wildcard expansion), degraded
  surface, withdrawn types, permission failures, and unseeded/retrying cells.
- **Version selection.** A resource served at multiple versions must mirror
  exactly one (preferred) to avoid duplicate files; define wildcard-version
  behavior (proposal: still collapse to preferred unless a specific version is
  named).
- **Resource policy under wildcard.** The default denylist
  (pods/events/leases/…) still applies to `*`; define whether users can opt
  excluded kinds back in.
- **Sensitive resources.** Wildcards will select Secrets and similar. This must
  compose with the existing sensitive-resource classification work
  ([sensitive-resource-classification-plan.md](../finished/sensitive-resource-classification-plan.md)).
- **Migration / backward compatibility.** Changing selector semantics changes
  what *existing* rules watch. Required: no silent expansion of an existing user's
  scope on upgrade — a migration story or explicit opt-in.
- **Causal ordering per cell.** A GVR's initial seed must be reconciled before its
  live events are flushed, so the seed isn't overwritten or double-counted. Today
  this is done per-target; it must hold per-cell.

---

## Part 2 — Current architecture

## 2.1 Components

- **`APIResourceCatalog`** ([api_resource_catalog.go](../../internal/watch/api_resource_catalog.go))
  — since the typeset-grace relocation
  ([typeset-owns-discovery-grace.md](../spec/typeset-owns-discovery-grace.md)): a thin
  per-scan normalizer producing a `typeset.Scan` (entries + scanned/failed
  group/versions + completeness) with only mechanical state (last scan as the
  change fingerprint, `generation`, readiness). Degraded-GV retention and the
  removal grace for omitted GVs both live in `typeset.Registry.UpdateFromScan`.
- **`RuleGVRResolver`** (rule_gvr_resolver.go)
  — maps one rule selector to concrete GVRs via the catalog, returning
  `ResolveMiss` values classified by reason. **Refuses wildcards** in
  `preflightMisses`.
- **`Manager`** ([manager.go](../../internal/watch/manager.go)) — owns the
  reconcile loop, the active informer set (`activeInformers` keyed by
  `GVR → namespace → cancel`), GVR resolution for both the watch plan and the
  snapshot, and snapshot emission.
- **Event surface triggers** ([manager_catalog.go:278-303](../../internal/watch/manager_catalog.go#L278-L303))
  — informers on CRDs and APIServices that coalesce changes into
  `catalogRefreshCh`.
- **`EventRouter` / per-GitTarget streams / reconcilers** — translate informer
  events and control events (`RequestRepoState`, `RequestClusterState`) into git
  commits, with a RECONCILING→LIVE buffering handshake per target.

## 2.2 Control flow today

```text
discovery / CRD trigger / 30s tick
        │
        ▼
ReconcileForRuleChange  (manager.go:740)
   ├─ RefreshAPIResourceCatalog            → catalog updated
   ├─ computeRequestedGVRs                  → resolve ALL rules → desired GVR set
   ├─ compareGVRs                           → added / removed (per GVR+namespace)
   ├─ snapshotTargetsNeedingDelivery        → per-target plan-hash diff
   ├─ stopInformer(removed)
   ├─ beginReconciliationForTargets         → buffer live events
   ├─ startInformersForGVRs(added)          → seed via informer cache
   ├─ emitSnapshotForRuleChange             → RequestRepoState + RequestClusterState
   │     └─ whole-target cluster list, ABORTS on any blocking miss/list error
   └─ completeReconciliationForTargets      → flush buffered events
```

## 2.3 What already works in our favor

- **Dynamic surface reaction exists.** The CRD/APIService triggers + reconcile
  loop already start/stop informers as the surface changes. Reacting to a new
  GVR is the steady state, not a new capability.
- **Per-(GVR, namespace) informer diffing exists** (`compareGVRs`,
  `findAddedGVRs`/`findRemovedGVRs`).
- **Degraded-aware catalog exists** — last-known-good is already preserved.
- **Per-target snapshot isolation exists** (plan-hash selection).

## 2.4 Where it falls short (the gaps Part 3 must close)

1. **Two resolution call sites, two truths.** The watch-plan path
   (`currentRuleSetSnapshots`, manager.go:1248/1264) and the snapshot path
   (manager.go:559-594) both resolve rules independently — duplicated logic that
   has already drifted (the bug that birthed
   [watchrule-gvr-resolution-plan.md](../spec/type-followability.md)).
2. **Snapshots are whole-target, not per-cell.** Adding one GVR re-lists and
   re-diffs the *entire* target (`emitSnapshotForRuleChange` →
   `RequestClusterState` over all GVRs). Violates requirement 1.3.
3. **One failure aborts the whole target.** A single GVR list failure or blocking
   miss aborts the entire target snapshot (manager.go:596-628). Safe but coarse;
   at wildcard scale it means a target may rarely snapshot. Violates the
   *per-cell* form of requirement 1.4.
4. **Wildcards refused** at planning (requirement 1.2 unmet) — and the CRD field
   docs advertise the opposite, so a `*` rule applies cleanly and silently
   watches nothing.
5. **Degraded handling is bolted on, not central.** The discovery-lag fix
   reintroduces "is this authoritative?" logic at the snapshot-selection layer
   because the catalog doesn't expose confidence/deltas as first-class outputs.
   Many `ResolveMiss` reasons + special cases = the exception sprawl the user
   wants gone.
6. **Withdrawal semantics are emergent**, not decided (requirement 1.6).

---

## Part 3 — Target architecture

The strategy: make the **catalog** a proper live model that emits deltas and
confidence, make the **watch plan** a single deterministic projection consumed in
one place, and make **reconciliation per-cell and incremental**. This removes the
duplicate resolution, the whole-target snapshots, and most of the miss/degraded
special-casing in one move.

## 3.1 Layer 1 — `ClusterSurface` (evolve the catalog)

Promote `APIResourceCatalog` into the authoritative, observable
`ClusterSurface`:

- **State per GVR:** `served | degraded | withdrawn`, plus the existing
  allowed/listable/scope/preferred metadata. (`served`/`degraded` already exist;
  add explicit `withdrawn` with a debounce before a degraded GV is declared
  withdrawn.)
- **Per-cell confidence, not a global flag.** Expose
  `AuthoritativeFor(GVR) Confidence` (and a startup-trust gate), **not** a global
  `Authoritative() bool`. A single boolean would either block healthy targets
  because one unrelated APIService is degraded, or get bypassed and become unsafe.
  Confidence travels with each planned cell so destructive decisions are gated
  per cell (1.4). Degradation is observed at GV granularity (discovery fails per
  GroupVersion) and projected down to the GVRs in that GV — see open question 7
  on GV-vs-GVR granularity.
- **Current surface + generation is the source of truth; deltas are only
  triggers.** Expose a queryable current surface snapshot (with its `generation`)
  as the authority, and a `Subscribe() <-chan SurfaceDelta`
  (`{GVR, Change: Added|Withdrawn|Degraded|Recovered}`) purely as an
  *optimization* that tells consumers *when* and *roughly what* to re-evaluate.
  Consumers must establish correctness by **re-diffing desired cells against
  active cells** against the current surface, never by replaying deltas as if they
  were complete — deltas can be missed across restart, backpressure, or
  subscription churn. Deltas make reconvergence cheap; the re-diff makes it
  correct.
- **Absorbs transience centrally.** No consumer ever sees a flapping CRD as
  add/remove churn; the surface only reports `Withdrawn` after the debounce. This
  is requirement 1.1, in one place, deleting the need for scattered
  `CatalogUnavailable`/`DiscoveryDegraded` handling elsewhere.

## 3.2 Layer 2 — `WatchPlan` (one resolver, projection semantics)

A single `WatchPlan` projects rules onto the surface, used by **both** informers
and snapshots (kills gap 2.4.1):

- `Plan(rules, surface) → map[GitTarget]TargetPlan`, where a `TargetPlan` is a
  set of **cells**: `{GVR, scope, namespaces, ops}`.
- **Wildcards are projection, not expansion-at-apply** (requirement 1.2): `*`
  selects all matching `served` cells from the surface *now*; the plan is
  recomputed on every surface delta, so membership tracks the cluster live.
- **Empty vs `*`**: empty = strict unique-or-error; `*` = all. One small, explicit
  rule — not a pile of miss reasons.
- **Resolution errors still exist and still matter.** Strict-empty ambiguity,
  not-served, and disallowed are real errors the resolver must report. The
  critical rule: an unresolved or errored selector must mark its cells
  **unresolved/held**, never collapse them into an *empty desired set* — otherwise
  a failed resolve looks like "the user wants nothing here" and the reconciler
  deletes the projection (1.4). **Resolution failure ≠ empty desired state.**
  What this design removes is the *duplicated, drifting* blocking-miss handling
  spread across two resolution sites and the snapshot layer — not error handling
  itself, which consolidates into one place with one meaning.

## 3.3 Layer 3 — per-cell reconciler (incremental)

Replace whole-target snapshotting with a **cell reconciler** driven by two delta
sources — rule changes and surface deltas — joined against current plans:

```
SurfaceDelta{GVR, …}  ─┐   (triggers only)
RuleChange{target}    ─┼─►  RE-DIFF desired cells (WatchPlan over current
periodic tick         ─┘     surface+generation) vs active cells + managed files
                                     │
                                     ▼
                          per-cell actions:
   cell appeared   → start informer + SEED ONLY THIS GVR (cell-scoped) → additions
   cell removed    → classify cause (1.6):
                       intent (rule/scope/policy)  → stop informer + DELETE the
                                                     cell's managed files
                       surface withdrawal          → stop informer + KEEP files,
                                                     record in status
                       non-authoritative (degraded/
                       denied/list-failed)         → HOLD: do not stop as removed,
                                                     do not delete, retry
   cell unchanged  → nothing
```

Two things this diagram makes explicit, both from review:

- **Correctness is the re-diff, not the delta.** Desired cells are recomputed
  from the `WatchPlan` over the *current* surface snapshot (and its generation)
  and diffed against active cells and the managed-file set. Deltas and the
  periodic tick only *schedule* a re-diff; missing a delta can never cause
  divergence because the next re-diff is authoritative (§3.1).
- **Repo-side scoping must match.** Seeding/diffing/deleting a cell operates only
  on that cell's managed files (1.7). `RequestRepoState` must be answerable at
  cell granularity — list/diff just `{target, GVR, scope, namespaces}` — or the
  per-cell path silently degrades back into whole-target snapshotting.

Properties this gives us for free:

- **Incremental** (requirement 1.3): a new CRD seeds exactly one GVR's objects on
  the targets that select it; everything else is untouched.
- **Per-cell safety** (requirement 1.4): seeding lists only that GVR; on failure
  the cell stays `unseeded/retrying` and writes nothing — no other cell, no
  deletions. The "partial view = phantom deletes" failure mode becomes
  structurally impossible because deletes are scoped to a cell that just listed
  successfully.
- **Convergence** (requirement 1.5): a failed/degraded cell is simply retried on
  the next relevant delta or tick; no target-wide churn.
- **Full re-snapshot becomes the fallback**, triggered only by explicit
  resync/repair, not routine surface changes.

Per-cell causal ordering reuses the existing RECONCILING→LIVE buffering, scoped to
the cell (requirement 1.7).

**Seed transport — streaming-list with a per-GVR fallback.** A cell seeds by
opening a single streaming-list watch (`SendInitialEvents=true`,
`ResourceVersionMatch=NotOlderThan`, `AllowWatchBookmarks=true`) rather than a
mega-LIST: the apiserver replays each existing object as a synthetic `ADDED` and
closes the initial set with a `k8s.io/initial-events-end` bookmark, whose
resourceVersion becomes the live resume cursor
([target_watch.go](../../internal/watch/target_watch.go)). That replay window
*is* the cell's seed — exactly the cell-scoped, one-watch-per-GVR shape this model
wants, with no re-list race that could miss an in-flight delete.

This only holds where the serving apiserver honors `SendInitialEvents` and emits
the terminating bookmark. Standard kube-apiserver and `genericregistry.Store`-backed
resources do; hand-written `rest.Storage` in aggregated servers may not, and the
hazard is **per-GVR, not per-APIService** — one aggregated binary can serve a
conformant group beside a non-conformant one. So the fallback is keyed by GVR and
is structural, not a kill switch: when the streaming option is unsupported the
cell drops to a plain **LIST + buffered WATCH** for that one GVR
([`watchListUnsupported` → `targetWatchListAndStream`](../../internal/watch/target_watch.go)),
yielding the same seed-then-stream result while every other cell keeps the
streaming path. There is deliberately no binary-level "disable streaming-list"
switch — the per-GVR fallback is the emergency valve.

Operationally this is an audit per unfamiliar cluster, not a one-time task: for
each non-local `APIService` a target watches, confirm it is
`genericregistry.Store`-based, handles `SendInitialEvents` in its `Watch` (grep
its source for `SendInitialEvents` / `k8s.io/initial-events-end`), or rides the
plain-LIST fallback. The runtime fallback means a missed audit degrades one cell
gracefully instead of hanging its seed.

## 3.4 How the existing docs fold in

- **Discovery-lag fix** → disappears as a special case: the surface never reports
  a degraded GVR as withdrawn, so a transient gap can't drop a cell or erase a
  baseline. The plan-hash machinery is replaced by cell-level delta diffing.
- **Wildcard semantics doc** → becomes the spec for `WatchPlan` projection.
- **Wildcard support plan** → its phases map onto building Layers 1–3; the
  "snapshot robustness" risk (its Phase 2) is resolved structurally by per-cell
  seeding rather than needing a bespoke list-failure policy.

## 3.5 Suggested build order

Each step is independently shippable and leaves the system correct.

1. **Unify resolution** behind one `WatchPlan` used by both informer and snapshot
   paths (removes drift; no behavior change). Pure refactor, fully unit-testable.
2. **Add surface state + deltas** to the catalog (`served/degraded/withdrawn`,
   `Authoritative()`, `Subscribe()`), with withdrawal debounce. Internal; no
   consumer change yet.
3. **Per-cell seeding** for *added* cells (incremental seed), keeping whole-target
   snapshot as the fallback path. Closes requirement 1.3 for additions.
4. **Per-cell withdrawal policy** (requirement 1.6) for removed cells.
5. **Enable wildcards** in `WatchPlan` projection (requirement 1.2) — now safe
   because Layers 1–3 handle dynamic membership, per-cell safety, and scale.
6. **Fix the field docs + admission validation** to match (and add status
   conditions, RBAC guidance, scale caps).

## 3.6 Cleanliness check (the user's steer)

This design *removes* code as much as it adds: one resolver instead of two; one
authority for "is the surface trustworthy" instead of miss-reason handling spread
across resolver, snapshot, and selection layers; structural per-cell safety
instead of the whole-target abort + discovery-lag patch. The number of distinct
"cases" a maintainer must hold in their head drops from "many `ResolveMiss`
reasons × two resolution sites × whole-target abort rules × discovery-lag
bookkeeping" to "a surface with three states (queried as the source of truth,
with deltas as cheap triggers), projected by one resolver into cells that each
reconcile independently and classify their own removal cause."

---

## Open decisions (need a human call before building)

1. **Empty vs `*`**: adopt the strict-`[]` / wildcard-`*` split, or make `[]`
   also mean "all" to match RBAC exactly? (Proposal: strict `[]`, wildcard `*`.)
2. **Surface-withdrawal default**: keep files on CRD uninstall (proposed) or
   delete them? Per-GitTarget override? (This is the *observability* class in 1.6.)
3. **RBAC model for wildcard**: ship a broad ClusterRole, or require users to
   grant it and degrade cells gracefully on permission-denied?
4. **Migration**: how do existing rules behave on upgrade — frozen semantics, or
   opt-in to the new projection?
5. **Scale cap**: hard limit on informer count per controller, or
   observe-and-warn only?
6. **Resource-policy deselection** (ref 1.6): when the default denylist newly
   excludes a kind that was being mirrored, delete its managed files (treat as a
   config-driven intent change) or keep them (treat as observability)?
7. **`withdrawn` granularity** (ref 3.1): is withdrawal a GV-level state, a
   GVR-level state, or both? Degradation is naturally GV-level (discovery fails
   per GroupVersion) while deltas are GVR-level — the model must state how the two
   compose rather than switching between them.
8. **Preferred-version change**: when a resource's preferred version changes, is
   that `Withdrawn(old)+Added(new)` (risks a delete+reseed and file churn), an
   `Updated` delta, or an explicit **migration** action (move/rewrite files,
   never delete)? Affects whether a routine apiserver upgrade churns the mirror.
9. **Rule-deselection disposition** (ref 1.6): when a rule stops selecting a cell,
   delete its managed files immediately (proposed — preserves the deterministic
   mirror) or leave them as *unmanaged historical* files? If the latter, define
   how unmanaged files are marked and ever reclaimed.
