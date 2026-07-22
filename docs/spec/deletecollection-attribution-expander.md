# DeleteCollection attribution & deletion-as-intent

> **spec** — current behaviour. The code depends on this document; change one, change the other. Index: [`../INDEX.md`](../INDEX.md)

> Status: **IMPLEMENTED** — 2026-06-28 (rev. 2: deletion-as-intent reframe). The render rule (§2), the
> expander (§5), the `exact_deletecollection_item` reason code (§8), unit tests (§9.1), and the four e2e
> specs (§9.2) have all landed and pass `task lint`/`task test`/`task test-e2e`.
> Scope: two complementary pieces — (1) a render-layer rule that treats `deletionTimestamp` as **logical
> absence** and removes the file at delete-request time, and (2) the **DeleteCollection attribution expander**
> that lets a name-less collection delete be attributed per object. State correctness for collection deletes is
> already solved by construction in watch-first (one watch event per object); this doc adds the *intent
> semantics* and the *attribution*.
> Related:
> [watch-first ingestion architecture](../finished/watch-first-ingestion-architecture.md),
> watch-first merge readiness §4,
> [superseded `deletecollection` nudge plan](deletecollection-attribution-expander.md),
> [watch event ordering & attribution grace](../facts/watch-event-ordering-and-attribution-grace.md),
> [`internal/watch/target_watch.go`](../../internal/watch/target_watch.go),
> [`internal/sanitize/sanitize.go`](../../internal/sanitize/sanitize.go),
> [`internal/queue/attribution_index.go`](../../internal/queue/attribution_index.go),
> [`internal/webhook/audit_handler.go`](../../internal/webhook/audit_handler.go).

## 1. The question, and the reframe

When someone runs `kubectl delete configmaps --all -n team-a`, the API server emits **one** name-less
`deletecollection` audit event, but the watch delivers **N** independent events — one per object. State is correct
with no special code. Two things are *not* free:

- **Attribution.** Each per-object removal runs the resolver, finds no fact (the collection event is name-less, so
  it stores nothing today — §4), and ships with the explicit **unresolved** author. "Alice deleted these 12
  configmaps" is visibly unresolved rather than silently credited to the operator. The **expander** (§5) closes this.
- **Finalizers — and this is where the design got interesting.** An object with a finalizer is *not* removed by
  the delete; it gets a `deletionTimestamp` and lingers in `Terminating` until a controller clears the finalizer.
  An earlier revision of this doc tried to defer attribution to that eventual removal and fight the resulting
  race. The **reframe** below makes it disappear: treat the delete request itself as the moment of intent.

This doc is built on one principle — **the repository captures intent, not a byte-for-byte API mirror** — and §2
makes that the foundation both pieces stand on.

## 2. Deletion-as-intent — the foundational rule

### 2.1 Two lifecycle facts

Deletion is two distinct facts, and conflating them is what made the old design hard:

- **Deletion intent.** The API server accepted a `DELETE` / `DELETECOLLECTION` and marked the object with
  `deletionTimestamp`. The object's *desired* existence is now **absent**.
- **Final removal.** The object actually disappeared from the API after grace/finalization completed. This is
  *runtime cleanup*, and it can take 5 seconds or 3 days.

The applyable Git manifest already strips `deletionTimestamp` and `deletionGracePeriodSeconds`
([sanitize.go:103-104](../../internal/sanitize/sanitize.go#L103-L104)) because they are **server-owned runtime
metadata, not desired state** — a manifest carrying them cannot be meaningfully re-applied. The reframe simply
takes that existing truth to its conclusion: if those fields aren't desired state, then an object that *only*
differs by having them is, as desired state, **gone**.

### 2.2 The rule

> **A resource with `deletionTimestamp` set is treated as logically absent from the intent tree.** The first
> observation of `deletionTimestamp` (or a `DELETED` event) removes the resource from Git and attributes the
> removal to the actor who *requested* the deletion. Later finalizer updates and the eventual `DELETED` event do
> **not** create additional Git changes; they are runtime cleanup, observed operationally only.

So when Alice runs `kubectl delete widget foo`, the Git change is `- widgets/foo.yaml`, authored "Alice", *now* —
not a commit that sets `deletionTimestamp` on the file, and not a commit deferred until finalization finishes.

### 2.3 Why this is correct, not a shortcut

- **It does not bypass finalizers.** Removing the file from Git is a statement about *desired state*, not an act
  on the cluster. Kubernetes still keeps the object in `Terminating`, controllers still run their finalizer
  cleanup (delete external resources, drain, etc.), and the object disappears from the API only when they're
  done. The intent tree saying "absent" and the API still showing `Terminating` is exactly the
  desired-vs-observed split GitOps is built on.
- **`deletionTimestamp` is monotonic and terminal.** Once set, a user cannot clear it; the object *will* be
  removed. So "logically absent" is never a flip-flop — there is no risk of the intent oscillating.
- **It keeps the manifest re-appliable.** We never commit a file carrying `deletionTimestamp` /
  `deletionGracePeriodSeconds`; the file is simply removed. The repo's invariant stays clean: **a file present
  means the resource is intended to exist.**

### 2.4 Why immediate removal is the *reversible* default

This is the decisive reason it's the right v1, not just a defensible one. Immediate removal establishes a single
strong invariant — *the main tree contains resources intended to exist*. From there, **richer behaviour can be
added later without breaking that invariant**: `.deletions/` or `.tombstones/` records, `DeleteIntent` side
objects, commit-message trailers, status reporting. Example of an *optional, later* enrichment (explicitly not
v1):

```
.deletions/widgets/team-a/foo.yaml   # kind: DeleteIntent, requestedBy: alice, requestedAt, finalizersAtRequest
```

The **reverse is much harder**: if v1 keeps `Terminating` objects in the main tree, consumers learn that "file
exists ⇒ object still in the API," and later removing them immediately silently changes what the repository
*means*. We avoid teaching that. (Captured from the owner's "what's easiest to change later?" — immediate removal
is.)

### 2.5 Operational caveat — don't lose the terminating object

Logical absence is a *Git* statement; we must not go blind to a stuck deletion. A long-`Terminating` object is
surfaced as **operational status / metrics / diagnostics**, never by keeping its desired-state file around. Keep
observing the object after we remove its file for: metrics, debug logs, internal cache/fact cleanup, and
stuck-finalizer diagnostics. (See §8.)

### 2.6 What changes in code

One local change in the watch router, plus a dependency that already holds:

- **Reclassify on `deletionTimestamp`.** In
  [`routeLiveTargetWatchEvent`](../../internal/watch/target_watch.go#L664), after the unstructured object is in
  hand, an event whose object has `GetDeletionTimestamp() != nil` is routed as **Delete** regardless of whether
  the watch type is `MODIFIED` or `DELETED`. The Delete path already emits no body
  ([targetWatchGitEvent:719](../../internal/watch/target_watch.go#L719)), so the file is removed. (Implementation
  note: this is computed *before* `ops.Match(op)`; a WatchRule that excludes deletes will, consistently, not act
  on the logical delete — call this out in review.)
- **Later events fold to no-ops.** The eventual finalizer-clearing `MODIFIED` (still `Terminating`) and the final
  `DELETED` re-issue "delete X" against an already-absent path → the writer diffs them to **no-op → no commit**
  (merge-readiness: no-op suppression lives in the writer). No extra
  bookkeeping is needed to suppress the follow-on events; the empty diff does it.

## 3. Why collection-delete attribution joins by UID, not RV (the linchpin)

A removal cannot join the attribution index on resourceVersion:

- The watch event that removes the file carries the object's **current RV** (the `deletionTimestamp`-set RV, or
  the deletion RV).
- The `deletecollection` **response body** lists each removed object at its **pre-delete RV** — a different
  number for the same object.

The only field **stable and identical on both sides is `metadata.uid`.** So the expander writes a **UID-keyed**
fact per item, and the removal event joins it by UID — exactly "join per-object by UID, not RV" carried from the
[superseded nudge plan](deletecollection-attribution-expander.md). Concretely, the expander writes the
**uid-only** key variant (`factKeyVariants(..., uid, "")` returns just that one key — the exact and rv-only
variants are skipped when no RV is supplied, which is precisely right since the body RV is dead).

> The reframe is what makes this simple. Because removal happens at **intent time**, the matching fact is written
> from the *same* `deletecollection` event that triggered the removal — it is fresh, present within the grace
> window, and there is no later finalizer-clearing fact to conflict with it (that fact, if any, lands against an
> already-removed file and produces a no-op). The earlier revision's separate "delete-intent key namespace" and
> operation-aware lookup are **no longer needed**; a plain uid-only fact suffices.

## 4. Background: what's already true (don't re-solve these)

- **State is solved by construction** — N watch events, mark-and-sweep backstop
  (merge-readiness §4).
- **A name-less event stores nothing today.** `RecordFact` early-returns when `identity.Name == ""`
  ([attribution_index.go:216](../../internal/queue/attribution_index.go#L216)), so a `deletecollection` writes
  **zero** facts now — the expander is purely **additive**.
- **Single deletes already attribute** (including finalizer ones, now improved). A single `kubectl delete foo`
  has a name, so `RecordFact` already writes its uid-only fact; with §2's intent rule, the finalizer single-delete
  is now removed and attributed at intent time too — for free, no expander needed. The expander exists **only**
  for the name-less collection case.
- **The conservative resolver fails closed** — multiple authors on one key → no usable attribution fact → the
  explicit unresolved author ([storeFactKey](../../internal/queue/attribution_index.go#L403)). Governing rule:
  **a wrong author is worse than no author.**
- **The grace window** absorbs a watch event that arrives before its audit fact
  ([author_resolver.go:40](../../internal/watch/author_resolver.go#L40)).

## 5. The expander — body-present per-UID fan-out

**Trigger.** An accepted, mutating audit event with `verb == deletecollection` whose response body parses as a
list of objects (typed `…List`, generic `v1.List`, or items array) — the common case for etcd-backed core types
and CRDs captured at `level: RequestResponse`.

**Action.** For **every** item in the body — including finalizer-pending ones — write **one uid-only fact** keyed
on the item's own `(group, resource, namespace, name, uid)`, carrying the audit event's actor
(`resolveUserInfo`), `auditID`, `stageTimestamp`, and `Verb: "deletecollection"`. Use the *item's* namespace/name,
never the collection URL's coarse/empty ones. No skipping: a finalizer item is removed-as-intent (§2) and
attributed to the actor exactly like any other item.

**Why it's honest.** Each fact names a specific UID the API server confirmed this actor issued a collection delete
against. No guessing (contrast §6). It rides the existing lookup, grace window, conflict-collapse, and TTL — the
only new write is one key per body item.

**Shape variance — parse defensively.** List-with-items → expand. `Status` / hollow / unparseable / absent body →
no items → no-op, degrade to §6. Never assume a body.

### 5.1 Where the code goes

A sibling to `RecordFact`, called from the same accept point
([audit_handler.go:258](../../internal/webhook/audit_handler.go#L258)):

```go
// RecordDeleteCollectionFacts expands a deletecollection response body into one
// uid-only attribution fact per listed object, joined by UID against the per-object
// removal event. A no-op when the verb is not deletecollection or the body is
// absent/hollow/unparseable. Writes ONLY the uid-only key (no RV is supplied, so
// factKeyVariants yields just that variant).
func (a *AttributionIndex) RecordDeleteCollectionFacts(ctx context.Context, event auditv1.Event) error
```

`RecordFact` is unchanged (it already no-ops on the name-less collection event). The handler calls both; for a
`deletecollection`, only the expander does work. The single-object fast path is never branched.

### 5.2 Reason code

When a removal event matches an expander fact, `attributionResultForMatch`
([attribution_index.go:332](../../internal/queue/attribution_index.go#L332)) today returns `weak` (uid-only). Add
`AttributionExactDeleteCollectionItem` and return it when `fact.Verb == "deletecollection"`, so collection-member
attributions are distinct from generic weak matches in metrics — realizing the `exact-deletecollection-item`
reason code that merge-readiness §3.5 lists as unused.

## 6. The hard case — hollow / empty body (aggregated & metadata-only)

An aggregated/external API server or a `Metadata`-level policy can report `deletecollection` with **no usable
list**. We know the actor, type, maybe namespace, maybe a label selector, and roughly when — but **not which
objects**. The owner's trap: *in a few seconds you could see more than one `deletecollection` that "fits."*

- **Option A — keep it around and guess by scope.** Reject. Two independent mis-attribution modes: (1) two
  actors deleting in the same `(type, namespace)` window → honest answer is conflict→unresolved, so it degrades to
  the explicit unresolved author under exactly the load that makes the case interesting; (2) even a single collection delete would
  capture an *unrelated* plain `kubectl delete configmap x` in the same window. Selector re-matching narrows but
  doesn't remove it (selectors overlap; empty selector matches all). Violates "a wrong author is worse than no
  author."
- **Option B — `Co-authored-by` floor.** Honest credit under ambiguity (multiple trailers), Git author stays
  unresolved. But in watch-first the deletes are driven by watch events, not the audit cause, so it needs real
  plumbing to carry a scope-cause into the commit-window builder. A deliberate fast-follow.
- **Option C — commit as the explicit unresolved author, document the limit.** v1.

**v1: Option C.** The hard case is a narrow intersection (aggregated/hollow body ∧ supports `deletecollection` ∧
watched ∧ attributed-author mode), and the failure is *degraded attribution*, not wrong state or a guessed author — which
is the correct conservative outcome. **Ship §2 + §5 now; Option B is the named fast-follow; Option A is rejected.**

## 7. Recommendation at a glance

| Case | Behaviour | Why |
|---|---|---|
| Any delete (single or collection member) | **Remove file at intent time; never commit `deletionTimestamp`** (§2) | Git is intent; reversible invariant; manifests stay re-appliable. |
| Finalizer object | **Removed immediately, attributed to the delete-requester**; finalizer cleanup is runtime no-op in Git | Reframe dissolves the old delay/conflict; controllers still finalize in-cluster. |
| Collection delete, body present | **Per-UID expander (§5)** credits the actor on each removal | API server states "these exact objects, by this user." |
| Collection delete, hollow body | **Explicit unresolved author (Option C, §6)** | Narrow; degraded attribution beats a wrong author. |
| Stuck `Terminating` | **Operational status/metric** (§2.5), file already absent | Don't pollute intent with runtime state. |

## 8. Observability & diagnostics

- **`AttributionExactDeleteCollectionItem`** flows onto `AttributionResolutionsTotal{result=…}` via
  `recordAttributionResolution` ([author_resolver.go:169](../../internal/watch/author_resolver.go#L169)) — a
  dashboard can show collection-member precise attributions vs. unresolved outcomes.
- **Expander write counter** (`op="deletecollection_expanded"`) on `AttributionFactEventsTotal` via the existing
  `recordFactEvent` hook ([attribution_index.go:433](../../internal/queue/attribution_index.go#L433)).
- **Stuck-finalizer / terminating diagnostics** (§2.5): surface long-`Terminating` watched objects whose files we
  already removed, so logical absence never hides a stuck deletion. Optional **secondary diagnostic
  attribution**: record *who* cleared the finalizer (the finalizer-clearing actor) as a diagnostic signal — a
  metric label or debug log — **never** as the Git author (that stays the delete-requester, and the event is a
  no-op commit anyway). v1 may ship the metric and defer the richer reporting.

## 9. Tests

### 9.1 Unit

`internal/watch` (render rule, §2.6):

1. A `MODIFIED` whose object has `deletionTimestamp` set is routed as **Delete** (no body) — *logical absence*.
2. A `MODIFIED` without `deletionTimestamp` is routed as **Update** (sanitized body) — unchanged.
3. A second Delete for an already-absent path diffs to **no-op** (covers finalizer-clear + eventual `DELETED`
   folding to nothing; assert no commit). May reuse existing writer no-op tests.

`internal/queue` (expander, §5):

1. A list body with three items → three uid-only facts, each crediting the actor; **no** exact/rv-only keys.
2. A finalizer-pending item (`deletionTimestamp` + finalizers) **also** gets a fact crediting the actor (it is
   *not* skipped).
3. A hollow / `Status` / unparseable / absent body → **no facts**, no error (degrade to §6).
4. A partial list writes facts only for items present (watch + sweep backstop the rest).
5. Join shape: a removal event with the item's UID and a *different* RV resolves to the actor via the uid-only
   key (proves §3) and surfaces as `exact_deletecollection_item`.

### 9.2 E2E — implemented

Implemented as Ginkgo specs in `test/e2e/deletecollection_intent_e2e_test.go`
(`Describe("DeleteCollection intent & attribution")`). Attributed-author mode on (skipped in configured-author mode); a
GitTarget claims `configmaps` with a 0s commit window; deletes are issued by an **impersonated actor** carrying
OIDC name/email claims, and finalizers are cleared by a **separate** impersonated identity. Each spec scopes its
collection delete with a per-spec label selector and asserts **convergence** (these files gone/authored thus,
these survive), never a global drop count, so they run against a reused cluster.

1. **`removes every collection member and attributes each removal to the actor` (state + attribution — "do
   both").** Three configmaps; the actor runs the collection delete; all three files go and each removal commit
   is **authored by the actor**, not committer.

2. **`removes a finalizer object at intent time, authored by the actor, while it is still Terminating` (the intent
   showcase).** One plain + one finalizer-guarded configmap. After the collection delete: (a) both files are
   removed and authored by the actor; (b) the finalizer object **still exists** in-cluster with a
   `deletionTimestamp` (Terminating); (c) the finalizer is cleared **as a different identity**, the object then
   leaves the API, and `Consistently` proves **no new commit** for the path — the removal commit (authored by the
   actor) stays the last one.

3. **`removes a single finalizer object at intent time too (the rule is not collection-specific)`.** A single
   named `Delete` of a finalizer-guarded configmap, as the actor: file removed at intent, authored by the actor,
   object still `Terminating`; clearing the finalizer yields no further Git change. (Single deletes are attributed
   by the existing `RecordFact`, not the expander — proving §2 is a general render rule.)

4. **`scopes a label-selector collection delete to matching objects and leaves siblings`.** Two matching + one
   non-matching sibling; a label-selector collection delete removes only the matching files (authored by the
   actor) and `Consistently` confirms the sibling survives untouched.

## 10. Definition of done

- **§2 render rule:** `routeLiveTargetWatchEvent` reclassifies a `deletionTimestamp`-bearing event to Delete; no
  manifest ever carries `deletionTimestamp`/`deletionGracePeriodSeconds` (already true via sanitize); later
  finalizer/`DELETED` events fold to no-ops. Unit §9.1.1–9.1.3.
- **§5 expander:** additive (`RecordFact` unchanged); writes only the uid-only key per body item; finalizer items
  attributed (not skipped); defensive parsing; `AttributionExactDeleteCollectionItem` + expander counter wired.
  Unit §9.1.4–9.1.8.
- **E2E §9.2.1–9.2.4** implemented, convergence-asserted; the finalizer showcase (§9.2.2) proves removal-at-intent
  with a *different* finalizer-clearing identity.
- **Hard case:** Option C documented in README/chart ("actor named when the API server returns the deleted set;
  aggregated/hollow-body collection deletes are recorded as committer"); Option B noted as fast-follow; Option A
  rejected so it isn't re-proposed.
- **Reversibility honored:** main tree = resources intended to exist; richer `.deletions/` style records left as
  future enrichment, invariant intact.
- Full validation per AGENTS.md: `task fmt → generate → manifests → vet → lint → test → test-e2e` (e2e sequential).
</content>
