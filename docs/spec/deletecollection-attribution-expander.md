# DeleteCollection attribution & deletion-as-intent

> **spec** — current behaviour. The code depends on this document; change one, change the other. Index: [`../INDEX.md`](../INDEX.md)
>
> Status: **PARTLY SUPERSEDED.** The render rule (§2) is current behaviour and still binds — it is
> what makes the collection window short enough to be safe. The **expander (§5) and its
> `exact_deletecollection_item` reason code (§8) are DELETED**, replaced by one collection fact that
> every removal in its scope joins; §5, §6 and §8 have been rewritten to say what took their place,
> and [`attribution-fact-stream.md`](../finished/attribution-fact-stream.md) is the record that
> argues it.
> Scope: two complementary pieces — (1) a render-layer rule that treats `deletionTimestamp` as **logical
> absence** and removes the file at delete-request time, and (2) attribution for a name-less collection
> delete. State correctness for collection deletes is
> already solved by construction in watch-first (one watch event per object); this doc adds the *intent
> semantics* and the *attribution*.
> Related:
> [attribution facts as a stream](../finished/attribution-fact-stream.md),
> [watch-first ingestion architecture](../finished/watch-first-ingestion-architecture.md),
> watch-first merge readiness §4,
> [watch event ordering & attribution grace](../facts/watch-event-ordering-and-attribution-grace.md),
> [`internal/watch/target_watch.go`](../../internal/watch/target_watch.go),
> [`internal/sanitize/sanitize.go`](../../internal/sanitize/sanitize.go),
> [`internal/queue/fact_index.go`](../../internal/queue/fact_index.go),
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

```text
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
- **A name-less event produces ONE fact about the collection.** *(This bullet described the opposite
  when the expander existed: a name-less event stored nothing, and the expander was purely additive.
  The name check is now "no name AND not a collection verb", so the collection request is exactly the
  case that produces a fact.)*
- **Single deletes attribute through their own fact** (including finalizer ones). A single
  `kubectl delete foo` has a name, so it files a per-object fact under its uid; with §2's intent rule
  the finalizer single-delete is removed and attributed at intent time too. Collection facts exist
  **only** for the name-less collection case, and a removal reaches them only when no per-object fact
  about the deletion applies.
- **The conservative resolver fails closed** — no usable attribution fact → the explicit unresolved
  author. Governing rule:
  **a wrong author is worse than no author.**
- **The grace window** absorbs a watch event that arrives before its audit fact
  ([author_resolver.go:40](../../internal/watch/author_resolver.go#L40)).

## 5. RETIRED — the expander is deleted

**This section described the per-UID fan-out expander, which no longer exists.** It was deleted with
the switch to the attribution fact stream; see
[`attribution-fact-stream.md`](../finished/attribution-fact-stream.md), which argues at length why
rebuilding N per-object facts from one request was the wrong shape.

What replaces it, in one line: a `deletecollection` is published as **one fact describing the
collection** — actor, type, namespace, the selector the request URI expressed, the stage timestamp,
and the set of uids the API server named when it sent a body — and every removal in that scope joins
it. The join tries uid membership first, then scope.

The rest of this document still binds. **§2, the deletion-as-intent render rule, is untouched**, and
it is what keeps the collection window short: the removal is attributed at delete-REQUEST time, when
`deletionTimestamp` is set, so finalizers do not stretch it. §3's argument that a collection member
must join by UID rather than RV also still holds, and is why the uid tier sits above the scope tier.

## 6. The hard case — hollow / empty body (aggregated & metadata-only)

An aggregated/external API server or a `Metadata`-level policy can report `deletecollection` with **no usable
list**. We know the actor, type, maybe namespace, maybe a label selector, and roughly when — but **not which
objects**. The owner's trap: *in a few seconds you could see more than one `deletecollection` that "fits."*

- **Option A — keep it around and guess by scope.** Reject. Two independent mis-attribution modes: (1) two
  actors deleting in the same `(type, namespace)` window → honest answer is conflict→unresolved, so it degrades to
  the explicit unresolved author under exactly the load that makes the case interesting; (2) even a
  single collection delete would
  capture an *unrelated* plain `kubectl delete configmap x` in the same window. Selector re-matching narrows but
  doesn't remove it (selectors overlap; empty selector matches all). Violates "a wrong author is worse than no
  author."
- **Option B — `Co-authored-by` floor.** Honest credit under ambiguity (multiple trailers), Git author stays
  unresolved. But in watch-first the deletes are driven by watch events, not the audit cause, so it needs real
  plumbing to carry a scope-cause into the commit-window builder. A deliberate fast-follow.
- **Option C — commit as the explicit unresolved author, document the limit.** v1.

**RESOLVED, and not by Option C.** The hollow-body case was shipped as **scope matching**, which is
Option A bounded until it is safe rather than rejected outright. Three things bound the
over-attribution the rejection was about, and the third is the one that changes the verdict:

- **Namespace and selector narrow the scope.** The selector is the *intent the actor stated*, read
  off the request URI, and it is present even when the body is not. An empty selector matches
  everything of the type in the namespace, which is exactly what `--all` means.
- **Precedence keeps anything with its own fact out.** The scope tier is the weakest evidence the
  join has, so it is reached only when every more specific tier missed. The unrelated
  `kubectl delete configmap x` in the same window is claimed by its OWN fact and never reaches it.
- **The window is short**, because of §2. Under deletion-as-intent the removal happens at
  delete-request time, so the window only has to cover audit batching plus clock skew — 30s by
  default, against a fact TTL of ten minutes. That is what makes the scope match safe, and it is not
  something the original framing, where attribution chased the eventual removal, could have offered.

The result is the reverse of the old degradation: the aggregated and metadata-only cases that used
to ship committer-authored now resolve, and a production cluster with
`--audit-webhook-truncate-enabled` — the one MOST likely to send no body for a large collection
delete — is the one that gains most. Option B (`Co-authored-by`) is no longer needed for this case.

## 7. Recommendation at a glance

| Case | Behaviour | Why |
|---|---|---|
| Any delete (single or collection member) | **Remove file at intent time; never commit `deletionTimestamp`** (§2) | Git is intent; reversible invariant; manifests stay re-appliable. |
| Finalizer object | **Removed immediately, attributed to the delete-requester**; finalizer cleanup is runtime no-op in Git | Reframe dissolves the old delay/conflict; controllers still finalize in-cluster. |
| Collection delete, body present | **One collection fact**, joined by **uid membership** (`collection_uid_delete`) | API server states "these exact objects, by this user." |
| Collection delete, hollow body | **The same collection fact**, joined by **scope** — type, namespace, selector, window (`collection_scope_delete`) | Bounded by precedence and a short window (§6); resolves what used to degrade. |
| Stuck `Terminating` | **Operational status/metric** (§2.5), file already absent | Don't pollute intent with runtime state. |

## 8. Observability & diagnostics

**The `exact_deletecollection_item` result label is gone**, and so is the expander's
`op="deletecollection_expanded"` write counter. Two labels replace the one, because the match is now
two-tiered and the tiers carry different confidence:

- **`collection_uid_delete`** — the removal's uid was in the set the API server said it deleted. No
  over-attribution risk at all.
- **`collection_scope_delete`** — matched by namespace, selector, and window alone. Weaker evidence, and
  the reason the window is short.

Both flow onto `AttributionResolutionsTotal{result=…}`, so a dashboard can now separate *precise*
collection credit from *scoped* collection credit rather than seeing one bucket.
`attribution_collection_degraded_total{reason}` counts a collection fact published WITHOUT its uid
set, which is what turns the second tier from an inference into a measurement. See
[`interpreting-metrics.md`](../interpreting-metrics.md).

**Stuck-finalizer / terminating diagnostics** (§2.5) are unchanged: surface long-`Terminating`
watched objects whose files we already removed, so logical absence never hides a stuck deletion.
Optional **secondary diagnostic attribution**: record *who* cleared the finalizer as a diagnostic
signal — a metric label or debug log — **never** as the Git author (that stays the delete-requester,
and the event is a no-op commit anyway).

## 9. Tests

### 9.1 Unit

`internal/watch` (render rule, §2.6):

1. A `MODIFIED` whose object has `deletionTimestamp` set is routed as **Delete** (no body) — *logical absence*.
2. A `MODIFIED` without `deletionTimestamp` is routed as **Update** (sanitized body) — unchanged.
3. A second Delete for an already-absent path diffs to **no-op** (covers finalizer-clear + eventual `DELETED`
   folding to nothing; assert no commit). May reuse existing writer no-op tests.

`internal/queue` and `internal/watch` (the collection fact and its join, §5–§6):

1. A collection delete publishes **one** fact carrying the actor, the namespace, the selector from the
   request URI, and the uid set — never one fact per object.
2. A list body larger than the uid cap drops the set and counts
   `attribution_collection_degraded_total{reason="uid_cap"}`; the fact still publishes.
3. A hollow / `Status` / unparseable / absent body still publishes a fact — with no uid set. This is the
   case the expander produced nothing at all for.
4. Join shape, uid tier: a removal whose uid is in the set resolves to the actor as `collection_uid_delete`,
   even though its RV never matches (proves §3).
5. Join shape, scope tier: a removal with no uid set to consult resolves as `collection_scope_delete` when the
   namespace, selector and window cover it — and does NOT resolve when the selector rejects its labels,
   when it is in another namespace, or when the window has passed.
6. Precedence: an object with its own fact never reaches either collection tier.

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
   object still `Terminating`; clearing the finalizer yields no further Git change. (A single delete is
   attributed by its own per-object fact, never by a collection one — proving §2 is a general render rule.)

4. **`scopes a label-selector collection delete to matching objects and leaves siblings`.** Two matching + one
   non-matching sibling; a label-selector collection delete removes only the matching files (authored by the
   actor) and `Consistently` confirms the sibling survives untouched.

## 10. Definition of done

- **§2 render rule:** `routeLiveTargetWatchEvent` reclassifies a `deletionTimestamp`-bearing event to Delete; no
  manifest ever carries `deletionTimestamp`/`deletionGracePeriodSeconds` (already true via sanitize); later
  finalizer/`DELETED` events fold to no-ops. Unit §9.1.1–9.1.3.
- **§5 collection fact:** one fact per collection request, carrying scope, selector and (when the body
  allowed it) the uid set; finalizer items attributed like any other, since §2 removes them at intent;
  defensive parsing; `collection_uid_delete` and `collection_scope_delete` result labels wired. Unit §9.1.4–9.1.9.
- **E2E §9.2.1–9.2.4** implemented, convergence-asserted; the finalizer showcase (§9.2.2) proves removal-at-intent
  with a *different* finalizer-clearing identity.
- **Hard case:** resolved by scope matching (§6), bounded by namespace, selector, precedence and a short
  window. Option B (`Co-authored-by`) is no longer needed for it.
- **Reversibility honored:** main tree = resources intended to exist; richer `.deletions/` style records left as
  future enrichment, invariant intact.
- Full validation per AGENTS.md: `task fmt → generate → manifests → vet → lint → test → test-e2e` (e2e sequential).
</content>
