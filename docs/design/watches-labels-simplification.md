# Label selection for watch rules

> **proposed**: nothing here is built. Design for
> [issue #146](https://github.com/ConfigButler/gitops-reverser/issues/146).
> Index: [`../INDEX.md`](../INDEX.md)
> Date: 2026-09-28, checked against `main` at `b43ecf61`.
> Related: [Kubernetes watch options](../facts/kubernetes-watch-options.md),
> [Definitions](../definitions.md),
> [Watch event ordering](../spec/watch-event-ordering-and-attribution-grace.md),
> [Target watch plan](target-watch-plan.md)

Issue #146 asks for a label selector on each watch rule, so that a GitTarget can mirror, for
example, only the Secrets labeled `internal.cozystack.io/tenantresource=true`.

**The kube-apiserver does the selecting.** A rule's selector becomes the `labelSelector` of
the list and watch requests the operator already makes. One resource collection is exactly one
Kubernetes list/watch request: group, resource, namespace, and label selector. The operator
evaluates no label logic on the event path, and it adopts the Kubernetes membership semantics
as they are rather than modeling membership itself.

The cost is more watches: one per distinct selector. That cost is accepted.

## The API

Add an optional `objectSelector` of type `metav1.LabelSelector` to
[`ResourceRule`](../../api/v1alpha3/watchrule_types.go) and
[`ClusterResourceRule`](../../api/v1alpha3/clusterwatchrule_types.go):

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: tenant-handoff-secrets
  namespace: tenant-a
spec:
  gitTargetRef:
    name: tenant-config
  rules:
    - apiGroups: [""]
      resources: ["secrets"]
      objectSelector:
        matchLabels:
          internal.cozystack.io/tenantresource: "true"
```

The name and type follow the admission webhook `objectSelector`, which sits beside the same
`apiGroups`/`resources`/`operations` rule shape that `ResourceRule` already copies. The
matching semantics are those of list and watch, described below; admission's old-or-new match
does not apply to a watch.

The structured type is the Kubernetes convention for a selector in an API object, so it gets
schema validation and reads the same as every other selector in a cluster. It converts
losslessly to the query string with `metav1.LabelSelectorAsSelector(sel).String()`, and that
string is what the operator sends. A raw string field would save one conversion and lose the
schema.

Validation, at admission and again at compile time:

- **Omitted and `{}` both select everything.** `LabelSelectorAsSelector` maps nil to "nothing",
  so the compiler normalizes an omitted selector explicitly rather than inheriting that.
- **An unparsable selector refuses the rule.** A valid selector that matches no objects is not an
  error: its watch initializes to an empty collection.
- **A selector may not use a label that sanitize strips.** `isOperationalLabel` in
  [`internal/sanitize/types.go`](../../internal/sanitize/types.go) removes the Flux, kro, and
  applyset labels before a document reaches Git. The prune boundary below reads labels from Git,
  so a selector on a stripped key could never be evaluated there.

## One collection is one list/watch request

Today a collection (in code, [`CellKey`](../../internal/types/cell.go)) is
`(group, resource, namespace)` within a GitTarget, and the served version is carried as data.
The selector joins the identity:

```text
collection:  group, resource, namespace ("" = all namespaces), canonical label selector
watch spec:  served version + operation filter
```

[`targetWatchStreams`](../../internal/watch/target_watch.go) currently unions the operation
filters of every rule that lands on one collection. That union stays correct, because every
rule on one collection now selects exactly the same objects. Rules with different selectors
land on different collections, each with its own operation filter. Separate unions of
selectors and operations would have let `team=a` rules' operations apply to `team=b` objects;
keying on the selector makes that impossible without any clause machinery.

The canonical string comes from the parsed `labels.Selector`, which sorts requirements by key,
so reordering `matchLabels` or `matchExpressions` in a rule does not restart a watch.

### One selector per overlapping collection

Two collections of one group/resource in one GitTarget overlap when their namespaces are equal,
or when one of them is all-namespaces. **Overlapping collections must use the same selector.**
Disjoint ones are unrestricted, so the multi-tenant case works: the `tenant-a` and `tenant-b`
WatchRules may each carry their own selector for Secrets.

The restriction exists because two watches over the same objects are not ordered against each
other; see [Overlapping watches](../spec/watch-event-ordering-and-attribution-grace.md#overlapping-watches-are-a-separate-ordering-problem).
With different selectors, one object can leave one collection (`DELETED`) while it stays in the
other (`MODIFIED`), and the Git outcome would depend on which event arrives first.

When a rule would violate this, the oldest rule (by creation timestamp, then name) keeps the
collection, and the newer rule reports the refusal on its own status. Keys that need several
values stay expressible in one selector with a set-based requirement, such as
`team in (a,b)`.

The all-namespaces-plus-named-namespace overlap that exists today, both unselected, is
unchanged by this proposal.

## Membership follows Kubernetes

A label-filtered watch reports membership transitions; see
[event types](../facts/kubernetes-watch-options.md#event-types-describe-membership-in-the-selected-collection).
The operator maps them through the existing operation mapping without reinterpreting them:

| Source change | Watch event | Operation |
|---|---|---|
| A matching object is created, or an existing object gains the label | `ADDED` | `CREATE` |
| A matching object changes and still matches | `MODIFIED` | `UPDATE` |
| A matching object gets a `deletionTimestamp` | `MODIFIED` | `DELETE` (deletion intent, as today) |
| A matching object is deleted, or loses the label | `DELETED` | `DELETE` |

The last row is the consequence to accept: **removing the label removes the document**, under
the GitTarget's prune policy. With the default `OnEvent`, the file is deleted; with `Never`, it
is retained; a rule without `DELETE` in its operations ignores both causes alike. The Git folder
then shows what `kubectl get -l <selector>` shows, which is the point of offloading selection.

Telling a deselection apart from a deletion would need a follow-up GET after each `DELETED`,
and a failed GET would leave the answer unknown. That is out of scope; it can be added later if
users need deselection to retain.

## Snapshot and prune boundary

Initial events, and the LIST fallback, carry the same selector, so a snapshot is exactly the
collection. The writer's sweep must then be bounded by the same collection, or objects that
were never selected would look absent and `Always` would remove their documents.

[`ResyncScope`](../../internal/git/types.go) gains the selector, and its match reads the
**labels on the Git document**: a document is inside the collection when its group, resource,
and namespace match and its labels match the selector. Git labels are the last mirrored
membership. A document whose object lost the label while no watch was running still carries the
label, is absent from the snapshot, and is swept under `Always`. That is the same result the live
`DELETED` would have produced. A document that never matched is never swept.

Today `ResyncScope.Matches` receives only a `ResourceIdentifier`, and `resyncPlan` passes it to
`BuildScopedPlan`. The scope predicate has to see the document's labels, so that signature
changes.

## Rule edits

A selector edit changes the collection key. The planner stops the old watch and starts a new
one, which initializes from initial events, so objects that match the new selector without
having changed since are still mirrored.

Documents that only the old selector covered are retained, as documents are today when a rule
stops covering a namespace. Removing or narrowing a rule never deletes content.

## Recovery and status

- **Cursors.** [`watchCursorKey`](../../internal/queue/redis_store.go) persists target UID,
  group/resource, and namespace. It adds the canonical selector (or a hash of it), so a cursor
  is never resumed under a different selection. A new selector is a new key, and the first
  attempt of a new watch initializes regardless; see the cursor entry in
  [Definitions](../definitions.md).
- **Render fidelity** is already per collection and per watch revision, and it follows the key.
- **`status.streams`** counts resource types. The overlap restriction keeps a type from
  appearing under two selectors in one namespace, but a type watched in two namespaces with two
  selectors still folds into one entry, as a type in two namespaces does today.

## Cost and authorization

Each distinct selector is one more watch on the kube-apiserver's watch cache, which is cheap on
the server. The operator receives only selected objects, which matters most for large
populations such as Helm release Secrets.

The operator's credential needs `list` and `watch` on the resource either way; RBAC does not
match objects by label. An authorization webhook can see the selector; see
[selectors and authorization](../facts/kubernetes-watch-options.md#selectors-and-authorization).
A filtered request is therefore the least-privilege request, and a denial is an observation
failure, never an empty snapshot.

## Out of scope

- **Different selectors on overlapping collections.** This needs per-object membership across
  collections and a per-object `resourceVersion` guard. `resourceVersion` is comparable within
  one group/resource on kube-apiserver; see [resource versions](../facts/resource-versions.md).
  The same mechanism would also close the existing all-namespaces-plus-named overlap.
- **Retaining deselected documents** through a follow-up GET.
- **Field selectors.** They use the same request machinery, but supported fields vary by type.
- **Identifier renames.** [Definitions](../definitions.md) already uses the Kubernetes terms in
  prose. The code follows in its own change: `CellKey` → `CollectionKey`, `SourceCell` →
  `SourceCollection`, `cellSpec` → `watchSpec`, and the stream, replay, and cursor names.

## Tests

Keep the invariants the current tests pin:

| Invariant | Test |
|---|---|
| Two served versions resolve to one collection watch | `TestTargetWatchStreams_OneStreamPerCellAcrossServedVersions` |
| All-namespace and named-namespace collections keep their own filters | `TestTargetWatchSpecs_NamedAndClusterWideScopesStayDistinctStreams` |
| Unchanged watches survive unrelated rule changes | `TestReplaceGitTargetWatches_AddingARuleStartsOnlyTheNewCell` |
| Replacement initializes before live delivery | `TestReplaceGitTargetWatches_ReusesUnchangedSetAndRestartsOnSpecChange` |
| A served-version change keeps the collection | `TestTargetWatchPlanFor_AServedVersionBumpKeepsTheSameCell` |
| A sweep leaves sibling namespaces alone | `TestResync_NamespaceScopedSweepLeavesSiblingNamespacesAlone` |

The first four live in `internal/watch/target_watch_test.go`, the fifth in
`target_watch_plan_test.go`, and the last in `internal/git/resync_scope_test.go`.

Add:

- Omitted, `{}`, invalid, zero-match, and stripped-key selectors.
- The selector reaches initial events, the LIST fallback, and the live watch alike.
- Two collections of one type with different selectors in disjoint namespaces both run; an
  overlapping pair refuses the newer rule.
- Reordered selector requirements keep the watch; a semantic edit restarts it and mirrors
  unchanged objects that now match.
- Label entry and exit produce `CREATE` and `DELETE` under each prune mode, and the author of a
  label-removing PATCH is attributed or explicitly unresolved.
- An `Always` sweep removes a document whose label matches but whose object is gone, and leaves a
  never-selected document in the same namespace.
- A cursor recorded under one selector is not resumed under another.
- An e2e run against a real kube-apiserver, since label-filtered initial events are server
  behavior that unit fakes do not reproduce.
