# Label selection for watch rules

> **Design requirements; implementation pending.** All three prune modes, including `Always`
> with supported Kustomize layouts, are required from the first release.
> **Step 2 of 2.** Step 1, the [collection rename and event-filter removal](collection-terminology-rename.md),
> has shipped.
> [Issue #146](https://github.com/ConfigButler/gitops-reverser/issues/146).
> Index: [`../INDEX.md`](../INDEX.md). Checked: 2026-09-28, source at `69836e70`.
> Related: [Kubernetes watch facts](../facts/kubernetes-watch-options.md),
> [Definitions](../definitions.md), [Ordering](../spec/watch-event-ordering-and-attribution-grace.md).

**Kubernetes is the source of truth. Git follows the cluster closely.** Within a GitTarget's
managed type and namespace scope, `Always` removes resource documents absent from a complete
server-selected snapshot, including documents that never matched. Git labels and historical
membership do not exempt a document. An object outside the selection is absent from this mirror
even if it still exists in Kubernetes. `OnEvent` and `Never` explicitly limit deletion.

The kube-apiserver selects objects through native LIST/WATCH `labelSelector` parameters. The
operator joins their identities to Git and uses its existing source-editing machinery. It
performs no local label matching for mirror membership or pruning. This contract makes filtered
`Always` possible without admission emulation or a durable membership inventory.

## The API

Add optional `objectSelector: metav1.LabelSelector` to
[`ResourceRule`](../../api/v1alpha3/watchrule_types.go) and
[`ClusterResourceRule`](../../api/v1alpha3/clusterwatchrule_types.go):

```yaml
rules:
  - apiGroups: [""]
    resources: ["secrets"]
    objectSelector:
      matchLabels:
        internal.cozystack.io/tenantresource: "true"
```

The field follows the existing admission-style rule vocabulary, but uses LIST/WATCH matching
semantics; admission's old-or-new matching does not apply. Validate at admission and compilation
with `metav1.LabelSelectorAsSelector`. Omitted and `{}` both select everything: normalize nil
explicitly because the helper otherwise means “nothing.” Invalid selectors refuse the rule;
a valid zero-match selector initializes an empty collection.

Selectors may use labels that [`sanitize`](../../internal/sanitize/types.go) strips before writing
Git. Selection happens on stored API objects, and labels need not survive in Git for identity
comparison. Preserve unsanitized watch provenance for attribution.

## One collection is one list/watch request

Extend [`CollectionKey`](../../internal/types/collection.go) within a GitTarget:

```text
collection: group, resource, namespace ("" = all namespaces), canonical label selector
watch spec: served version (watchSpec)
```

Rules have no operation filter. Deduplicate identical collections and choose their served
version deterministically; every collection observes all object event types. Served version remains
data, so changing it does not move Git files. Keep each selector attached to its own collection.

### Canonical selector identity

Validate, normalize equality in `matchLabels` to singleton `In`, sort and deduplicate values,
deduplicate requirements, then sort by the complete `(key, operator, values)` tuple and serialize.
Omitted and empty selectors yield the empty string. This is syntactic normalization, with no
general Boolean simplification. Reuse the identity for requests, planning, cursors, resync
coalescing, render fidelity, and retention scopes.

`labels.Selector.String()` alone is insufficient:
[`ByKey.Less`](https://github.com/kubernetes/apimachinery/blob/v0.37.0/pkg/labels/selector.go#L158)
compares only keys. Test both orders of `team in (a,b),team notin (b)`, plus reordered values,
duplicates, and equivalent `matchLabels`/singleton-`In` forms; none may restart a watch or cause
an overlap refusal.

### One selector per overlapping collection

Collections overlap when their group/resource matches and their namespaces are equal or one is
all-namespaces. **Overlapping collections must have the same canonical selector.** Different
selectors in disjoint namespaces are allowed. For a conflict, the oldest rule by creation
timestamp, then name, keeps the collection; the newer rule reports refusal.

Without this restriction, one watch could delete an object another still selects, and independent
snapshots could prune each other's documents. Use a single set-based selector for several values.
The existing same-selector all-namespace/named-namespace overlap remains supported with its
[known ordering limitation](../spec/watch-event-ordering-and-attribution-grace.md#overlapping-watches-are-a-separate-ordering-problem).

## Membership follows Kubernetes

Use the server's [membership events](../facts/kubernetes-watch-options.md#event-types-describe-membership-in-the-selected-collection):

| Source observation | Watch event | Git operation |
|---|---|---|
| Matching object created, or existing object enters selection | `ADDED` | `CREATE` |
| Selected object changes and remains selected | `MODIFIED` | `UPDATE` |
| Selected object acquires `deletionTimestamp` | `MODIFIED` | `DELETE` (existing deletion-intent policy) |
| Object disappears or leaves selection | `DELETED` | `DELETE` |

Label exit and physical deletion have the same Git membership effect. No follow-up GET or local
label check distinguishes them. Prune policy decides whether the document is removed:

| Mode | Observed exit/deletion | Absent from a complete snapshot |
|---|---|---|
| `Never` | Retain | Retain |
| `OnEvent` (default) | Remove | Retain |
| `Always` | Remove | Remove |

Every observed removal reaches the normal processing path; there is no rule operation filter.
`Never` disables both Git deletion paths while continuing to mirror creates and updates. All
modes cover plain YAML and supported Kustomize layouts.

## Snapshot and prune boundary

For one target and source cluster:

```text
C = managed Git identities of this group/resource in the requested namespace scope
D = identities in the complete server-selected snapshot, excluding terminating objects
Always removals = C - D
OnEvent/Never retained documents = C - D
```

Update identities present in both sets; create Git representations for identities only in `D`.
The target path and write boundaries constrain `C`. Other types and sibling namespaces stay
outside this sweep. No raw or rendered Git-label predicate narrows `C`.

If Git holds Secrets A and B in `tenant-a`, but the request returns only A, `Always` removes B,
even if B never matched. The retaining modes keep it. Intentionally keeping unselected objects
requires a retaining mode or a separate target path; ownership is not inferred from old labels.

Match group/resource, effective namespace, and name within the source-cluster context. Name and
namespace alone collide across kinds. API version is representation data; UID distinguishes
incarnations for attribution, while Git identity survives recreation. See
[`ResourceIdentifier`](../../internal/types/identifier.go) and
[Kubernetes object identity](https://kubernetes.io/docs/reference/using-api/api-concepts/#object-names).

### Retention reporting

Keep orphan classification for `OnEvent` and `Never`. Under this ownership contract, every
in-scope identity in `C - D` is a document that `Always` would remove, including never-selected
ones. Counting it as retained is intentional; suppressing the count would hide non-convergence.
The count means “absent from the selected mirror,” without claiming physical cluster deletion.

[`planGitOnly`](../../internal/manifestanalyzer/plan.go) already counts these suppressed drops.
`applyResyncPlan` carries `RetainedOrphans` into its result, and `tallyPruneRetention` publishes
per-type metrics in [`resync_flush.go`](../../internal/git/resync_flush.go). Status uses the
[`retention roll-up`](../../internal/watch/retention_rollup.go), consistent with
[`RetainedDocuments`](../../api/v1alpha3/gittarget_types.go)'s converged-mirror definition.
Include the selector in scope identity and preserve revision checks so retired reports cannot
restore stale counts. A failed observation must not publish a new zero count.
The current roll-up sums per-scope counts, so all-namespace/named-namespace overlap can count a
document twice. That existing limitation is separate from counting never-selected documents.

### Complete observation and installation timing

Initial events, every LIST page, the fallback watch, and resumed watches must carry the same
selector. Only completed initialization or a complete consistent LIST supplies `D`; see the
[watch facts](../facts/kubernetes-watch-options.md). Failure, denial, expired pagination, or
unavailable discovery produces no sweep. A successful empty snapshot authorizes removing all
`C` under `Always`. Keep [`desiredFromObject`](../../internal/watch/scope_resolve.go)'s exclusion
of terminating objects. Each snapshot covers its own structural scope; RV is no cross-type clock.

A complete API observation does not prove that a deployment controller finished installing a
Git revision. Current [`mappingRefusals`](../../internal/manifestanalyzer/acceptance.go) checks
followability/scope, and [`RenderTokenDivergences`](../../internal/manifestanalyzer/render_fidelity.go)
checks rendered tokens against live fields. Neither proves every Git object was installed; see
[render fidelity](support-boundary/render-fidelity.md). Requiring every object to remain present
would also prevent cleanup after offline deletions.

Enable mirroring when the cluster is ready to be authoritative. Under `Always`, objects awaiting
installation and new Git-first additions can be pruned before deployment. Coordinating that
handoff is an operating responsibility; this design adds no orchestrator reconciliation barrier.

### Kustomize support boundary

Use the existing [Kustomize editing boundary](support-boundary/kustomize-support-boundary.md).
Rendering resolves effective identities and source locations and verifies edits. It performs no
label-based prune selection. Existing machinery includes
[`renderRoot`](../../internal/manifestanalyzer/kustomize_render.go),
[`DocumentModel.Rendered`](../../internal/manifestanalyzer/store.go), and
[`VerifyBatchRenders`](../../internal/manifestanalyzer/render_verify.go).

For Kustomize, `C` describes objects represented by the target's render roots. Patch documents
and build directives are context; an inherited object already suppressed by a deletion patch
is absent from `C` despite its base file remaining. Resolve overlay-supplied namespaces before
identity comparison. Unavailable rendering is an error, never an empty object set.

Live [`applyDelete`](../../internal/git/plan_flush.go) already handles inherited objects through
an overlay-local `$patch: delete`. Snapshot `applyResyncPlan` instead calls `dropDocument`, which
only removes YAML documents. Day-one `Always` requires a shared semantic removal path that:

- Resolves effective identities back to source documents and preserves multi-document siblings.
- Cleans `resources:` references when the final document leaves a file.
- Removes inherited objects through overlay-local patches, preserving bases and sibling overlays.
- Records removal intent and verifies intended absence plus unchanged unrelated rendered objects.
- Handles re-entry by retiring a recognizable operator-owned deletion patch and its reference,
  then upserting and verifying presence. Persist artifact ownership in Git; an edited patch,
  uncertain ownership, or path collision refuses the batch. Retries must be idempotent.

Retain separate prune gates, attribution, and live/sweep counters. Refuse ambiguous identity or
source mapping, escaped write boundaries, and failed render verification before commit. Nil
`Rendered` is not proof of plain-YAML context. Report the affected candidate and retry after
relevant inputs change; skipped removals must not count as convergence. Build and apply against
one Git revision, rebuilding mappings after fetch/conflict changes. Existing unsupported features
remain unsupported, and no shared base becomes writable through this feature.

Admission/controller labels can differ from Git labels in either direction: trust the selected
identity set. Removing a representation can cause downstream GitOps pruning to delete the live
object. Rule narrowing or a valid but mistaken selector has the same consequence; the operator
cannot distinguish that mistake from an intentionally smaller mirror.

### Attribution for filtered removals

The current [`attachAuthor`](../../internal/watch/target_watch.go) sets `ExactCapable=false` for
all Git deletions. That **does not skip the exact join**: [`FactIndex.Lookup`](../../internal/queue/fact_index.go)
tries the sticky deletion pointer first, then UID/RV, then weaker fallbacks. If Bob's label-exit
fact is missing, Alice's earlier write can author the removal after grace expires.

There is also an avoidable delay. When an exact PATCH/UPDATE wins lookup, `awaitsBetterEvidence`
still treats it as a write fallback and waits the remaining grace for delete evidence (default
`3s`). This is not true of every exit: eligible delete evidence can finish the wait earlier.

Filtered `DELETED` needs an explicit lookup policy, retaining raw event type, selector presence,
UID/RV, and deletion timestamp. Require matching audit route, group/resource, UID, and RV;
eligible evidence returns immediately, including a late arrival during grace. A nonterminating
previous object permits an exact PATCH/UPDATE to name a label exit. A terminating previous object
requires exact deletion evidence; a finalizer PATCH must not name the deletion initiator.
Without eligible evidence, expire as unresolved. Do not use last-writer, sticky, collection,
RV-only, or name-only fallbacks for these filtered removals.

This deliberately extends the [current attribution contract](../spec/attribution.md#three-rules-that-are-easy-to-miss):
physical-removal fallbacks cannot establish who changed a label. Keep existing unfiltered and
`MODIFIED` deletion-intent behavior. Merely changing `ExactCapable` is insufficient; weaker
fallbacks remain. Update the attribution contract with the implementation. Resolution stays
inline and ordered; uncertainty affects authorship only. Resyncs retain configured authorship.

### Scope integration and queue ordering

Keep [`ResyncScope.Matches`](../../internal/git/types.go) structural: target, group/resource, and
namespace. The collection identity additionally carries the selector. Update the `CollectionKey`/scope
comments to distinguish the selected collection from the Git objects it owns.
[`markResyncTailForWriteLocked`](../../internal/git/branch_worker.go) must keep matching identifiers:
a `DELETE` may have no object payload, and different selector generations can affect the same file.
A live write prevents a newer snapshot from coalescing ahead of it; selector identities must not
share coalescing keys.

For replacement, quiesce the retired producer's enqueue path before the new producer enqueues
its initial snapshot. Accepted old work drains ahead of it; none may arrive afterward and restore
an excluded object. Current `targetWatchSet.stop` only cancels. Add producer completion or enqueue
serialization, without making the branch worker consult watch configuration. Preserve the
[ordering regression cases](../spec/watch-event-ordering-and-attribution-grace.md#regression-cases).

## Rule edits, recovery, and status

A selector edit starts a new collection and complete snapshot over the same type/namespace
ownership scope. Narrowing `team in (a,b)` to `team=a` removes excluded identities under `Always`,
regardless of stale Git labels or missed UPDATEs; retaining modes keep them on that snapshot.
Widening initializes newly selected objects without waiting for mutations. Equivalent canonical
selectors keep the watch. Removing a type/namespace entirely enqueues no cleanup of its retired
scope; existing [namespace retention](../configuration.md#deletion-policy-specprunemode) still applies.

Add the canonical selector to [`watchCursorKey`](../../internal/queue/redis_store.go); never resume
under another selector. Render-fidelity and retention scopes follow that same identity and watch
revision. `status.streams` continues counting resource types, rather than watch connections.

## Documentation and boundaries

The configuration reference must lead with cluster authority, the prune table and `OnEvent`
default, and the removal of never-selected documents under `Always`. Explain rule narrowing,
installation timing, downstream GitOps deletion, overlap refusals, Kustomize write limits,
retention counts, and unresolved attribution. These are consequences of the chosen contract.

Filtered requests reduce transferred objects but add watches; measure the cost. Keep requests
filtered because [authorization can depend on selectors](../facts/kubernetes-watch-options.md#selectors-and-authorization).
A denial cannot become an empty snapshot. Field selectors remain separate work. Different selectors over
overlapping scopes need union membership and stale-observation handling; independent sweeps are
insufficient. The existing same-selector overlap ordering gap also remains outside this change.

When this step ships, remove this design's line from [`.docs-lint-scope`](../../.docs-lint-scope).
It is gated only while the design is active; afterwards it is a historical record.

## Tests

Preserve step 1's version, duplicate-collection, unchanged-watch, and replacement checks in
[`target_watch_test.go`](../../internal/watch/target_watch_test.go) and
[`target_watch_plan_test.go`](../../internal/watch/target_watch_plan_test.go), plus sibling-namespace
protection in [`resync_scope_test.go`](../../internal/git/resync_scope_test.go). Add:

### Selection and pruning

- Omitted, empty, invalid, zero-match, and sanitizer-stripped-label selectors. Assert identical
  query propagation on every initialization, fallback, pagination, and resume path.
- The canonicalization cases above preserve cursor/watch identity; changed selections initialize
  afresh. Disjoint namespaces permit distinct selectors; overlaps refuse the newer rule.
- Equal names across resource types/namespaces remain distinct. Version changes and new UIDs do
  not orphan Git representations. Missing Git objects use placement; mapping failures refuse.
- Observe entry, label exit, and physical deletion under all modes, with no operation filter.
  Simulate missed deletes and updates during disconnection: a fresh snapshot restores present
  object state and prunes absent identities only under `Always`. Keep terminating-object behavior.
- With `C={A,B}` and `D={A}`, including never-selected B, `OnEvent`/`Never` retain B and report one
  retained document; `Always` removes B and reports zero retained. Check status and per-type
  metrics. Other namespaces/types stay untouched. An empty `D` removes all `C` only under `Always`.
- Narrow a selector with matching, stale, and missing Git labels; the same absent identities are
  removed under `Always`. Widening adds unchanged newly selected objects. Retiring coverage does
  not sweep it, and retired reports cannot overwrite current retention counts.
- Interrupted initialization, a failed/expired LIST page, denied authorization, and unavailable
  discovery enqueue no sweep or new zero retention report. A complete empty snapshot does sweep.
- Pre-installation and new Git-first documents are removable under `Always`. Switching prune mode
  or introducing a supported Kustomize layout keeps full support without silently retaining.

### Kustomize

- Overlay-injected/overridden labels with positive, negative, and `DoesNotExist` selectors;
  admission/controller drift in both directions; stripped labels. Only server membership decides
  `D`, for both plain YAML and Kustomize. Resolve effective namespaces before matching.
- Partial multi-document deletion preserves siblings; final-document deletion cleans references
  and leaves the root buildable. An inherited removal authors an overlay patch, with base and
  sibling overlay unchanged. Require parity between live and snapshot removals.
- Restart and re-enter a previously removed inherited object: retire its owned patch, verify
  presence, and prove idempotency. Edited/unowned patches and occupied paths refuse without loss.
- Conflicting roots, absent attribution, unavailable renders, out-of-bound writes, and changes to
  unrelated rendered objects refuse before commit. Nil `Rendered` cannot imply plain YAML; keep
  a valid plain-YAML control. Git revision changes invalidate old mappings before writes.

### Attribution and ordering

- Alice's RV 10 fact plus Bob's exact RV 11 label-exit PATCH resolves Bob immediately, without
  consuming the full grace, even with unrelated sticky/collection evidence available. Repeat
  with Bob arriving during grace and both PATCH and UPDATE.
- Without Bob's fact, expire unresolved despite Alice's fact and available sticky, collection,
  RV-only, and name-only fallbacks. Exact filtered deletion evidence resolves immediately;
  insufficient evidence remains unresolved. A terminating finalizer PATCH cannot author removal.
- Preserve unfiltered/deletion-intent attribution. Cancel during grace and refuse a full queue
  without advancing its cursor. Later events cannot overtake an earlier attribution wait.
- Queue R100 snapshot, payload-free R101 exit, then R103 snapshot where the object re-enters.
  R103 cannot coalesce into R100's position; final content contains the object.
- Pause a retired producer before enqueue, replace its selector, and initialize an empty snapshot.
  Old work enters ahead of the replacement or is canceled, never afterward. Repeat with old
  snapshots and a full queue while unrelated collections continue.
- Run e2e against a real kube-apiserver for initial events, label entry/exit, and offline `Always`
  recovery; unit fakes cannot establish these server semantics.
