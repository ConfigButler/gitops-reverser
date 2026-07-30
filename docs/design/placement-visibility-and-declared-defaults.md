# Placement, made visible: naming, a declared default, and `status.layout`

> **design**: a decision record for work in flight, not a plan of record.
> Index: [`../INDEX.md`](../INDEX.md)
> Date: 2026-07-30. Written against `feat/delete-sibling-inference` (PR #291), which deleted
> Option C sibling inference and shipped the placement counters.
>
> Everything decided here lands in **that same PR**. There is no follow-up sequence below; a
> section marked "not now" is a decision not to build it, with the trigger for revisiting written
> down, not a deferral to another branch.

Three questions came out of reviewing #291, and one of them (a CRD default for
`placement.default`) is a better idea than my first answer to it gave credit for. This document
states the findings behind each, the options, and the call.

## The decisions, up front

| Question | Call | Why in one line |
|---|---|---|
| Rename `source="canonical"` to `default`? | **No.** Keep `canonical`, split `declared` into `byType` and `default`, and fix the prose | `placement.default` is a *declared* template; reusing the word for the built-in path makes one name mean both a declaration and the absence of one |
| Default `placement.default` in the CRD? | **No**, and the reason is structural | A defaulted default is never empty, so it shadows the kustomize-root step and new files in an overlay would land where nothing renders them (F9) |
| Publish the effective layout in status? | **Yes, now** | It gives the clarity the CRD default was reaching for, without freezing anything, and the seam it needs already exists |
| `{kindLower}` or a `toLower` function? | **`{kindLower}`** | A function syntax is a language; one variable answers the actual need |
| Is the kustomize root "still a little bit inference"? | **Yes, and it earns its keep anyway** | Its answer changes only when a `kustomization.yaml` changes, and ignoring it produces a file nothing renders. It gets the same visibility obligation as everything else |
| Two kustomize roots (ambiguous) | **Keep writing, make it loud** | The file is currently unreachable *and* uncounted. Refusing would protect nothing that mirroring does not |

## Findings

Each of these is checkable against the tree, and three of them changed a decision.

**F1. The word "default" already means two things in our own docs.** `spec.placement.default` is a
user-declared catch-all. [`architecture.md`](../architecture.md) calls the built-in path "the
**built-in default** path"; the CRD comment calls the same thing "the built-in canonical path". One
of those has to go, whatever we do with the metric.

**F2. `source="declared"` hides which declaration answered.** A `byType` hit and a `default` hit are
one series, so a catch-all quietly swallowing a type you meant to name explicitly looks identical to
a rule working as intended. For a metric whose job is "is a rule missing?", that is the wrong place
to lose resolution.

**F3. A CRD default for `placement.default` fails our own validation today.** Not the decisive
objection (F9 is), and not unfixable (F4 is the fix), but it is where the idea stops today.
[`validateSecretSafety`](../../internal/controller/gittarget_placement_validation.go) rejects a
`default` that is not identity-complete unless the target also declares an identity-complete
`byType["v1/secrets"]` entry. And `IdentityCompletePlacementTemplate(tmpl, false)` requires
`{groupPath}`, `{version}` **and** `{resource}`. The built-in path is deliberately **versionless**
(see [`../facts/`](../INDEX.md) and the versionless-path decision), so the template we would default
to,

```text
{namespaceOrCluster}/{groupPath}/{resource}/{name}{sensitiveSuffix}
```

is judged *not* identity-complete, and every GitTarget that did not also declare a Secret route
would go `Validated=False`. The CRD's default would be refused by the CRD's own gate.

**F4. The `{version}` requirement is wrong on its own terms.** Two versions of one group/resource
are the *same object*, which is exactly why the built-in path dropped the version segment. A
template carrying scope, group, resource and name cannot collide two distinct identities, with or
without a version. So the requirement rejects safe templates: a user who writes the
versionless canonical layout by hand is told it is a bundling path. That is a bug independent of
anything else here, and fixing it is a precondition for F3 ever being reconsidered.

**F5. Defaulting semantics, precisely, because a first draft of this document got them wrong.**
A CRD default is applied to the request object on create, update and patch, and is therefore
**persisted**. It is also applied **in memory when reading from etcd**, using the storage version's
schema, so the controller sees the value even for an object stored before the default existed. It is
**not** persisted by our own status writes: `GitTarget` has a status subresource
([gittarget_types.go](../../api/v1alpha3/gittarget_types.go)), so a status update writes only the
status stanza and leaves the stored spec alone. In a GitOps-managed cluster the answer is
nevertheless "yes, quickly", because the user's own Flux or Argo re-applies the object continuously
and each apply persists it.

Two consequences follow, and **both are smaller than an earlier draft of this document claimed**.
Freezing the built-in path per target is arguably a feature rather than a cost: placement is already
create-time and non-retroactive, so a long-lived target keeping its established layout for new types
is the stability we already promise, and a value in the spec is visible and editable in a way a
built-in is not. And "indistinguishable from a declaration" is too strong: `metadata.managedFields`
records that the API server set the field, not the applier. Nothing in our controller reads
managedFields, and placement behavior should not depend on field-ownership metadata, but the
information is there. What is left is spec bloat: every GitTarget carries a template string whether
its owner cares about layout or not. That alone would not decide anything.

**F5b. Defaulting a map is a floor that vanishes when you stand on it.** Kubernetes defaulting
applies to an **absent** field and never merges, so a defaulted
`byType: {"v1/secrets": "..."}` disappears the moment a user writes any `byType` entry of their own.
Their unrelated ConfigMap entry would silently drop the Secret route, leaving a defaulted bundling
`default` unguarded, and the object would go `Validated=False` on an edit that had nothing to do with
Secrets. This is why "default the Secret route too" (the obvious repair for F3) is worse than
fixing F4.

**F6. The data-plane to status seam already exists, and it enqueues.**
[`retention_rollup.go`](../../internal/watch/retention_rollup.go) is the pattern:
`MarkTargetRetention` records a fact from the write path into an epoch-scoped per-target roll-up and
calls `enqueueGitPathChange` **on a change only**, and the controller projects it in
`gitTargetRetentionStatus`. This retires the objection recorded in
[`open-asks-priority.md`](open-asks-priority.md) that placement facts cannot reach the GitTarget
promptly because "a refusal recorded on the data plane does not enqueue the GitTarget". One does
already. `status.retention` is proof.

**F7. `{kind}` renders capitalized and `{resource}` renders plural.** So neither gives
`deployment-simon.yaml`, and the spec's own "keep it small" rules out template functions ("no
template functions except safe path-segment sanitization").

**F8. Two supported kustomizations produce a file nothing renders, and nothing counts it.**
Ambiguity declines to the canonical path, which no root's `resources:` graph reaches. No entry is
attempted, so `placement_kustomization_entries_total` never sees it. It is the exact failure that
counter exists for, in the one case it cannot observe.

**F9. A defaulted `default` would make the kustomize-root step unreachable.** This is the objection
that decides Question 2, and it is structural rather than a trade.
[`LocateNew`](../../internal/manifestanalyzer/placement.go) resolves in this order:

```text
byType -> default -> kustomize root -> canonical
```

`resolveDeclared` returns on any non-empty `policy.Default`, so a default that is *always* set means
step 3 never runs. Every new file in a kustomize overlay would take the canonical path instead of
landing beside the `kustomization.yaml`, with no `resources:` entry: committed, looking mirrored, and
rendered by nothing. That is exactly the bug the kustomize-root step was added to prevent.

The repairs are worse than the defect. Reordering so the render root beats `default` makes a user's
real declaration lose to the folder, which inverts the precedence the whole feature rests on. Reading
`managedFields` to tell a defaulted value from a declared one makes placement depend on
field-ownership metadata. Neither is defensible later.

## Question 1: `canonical` or `default`?

The pull toward `default` is real: it is the word for "what happens when you say nothing", and
`placement.default` is the field a user reaches for. But that is precisely the collision (F1). If
the metric said `source="default"`, a reader could not tell whether their `spec.placement.default`
had matched or whether nothing had matched at all, and those are opposite situations with opposite
fixes.

So: **`canonical` keeps the built-in path**, the prose stops calling it "the built-in default", and
the resolution gains the distinction that was missing (F2):

| `source` | Means |
|---|---|
| `byType` | an exact type entry matched |
| `default` | the declared catch-all matched. Same word, same meaning as the CRD field |
| `kustomize_root` | the folder has exactly one supported kustomization, so the file went beside it |
| `canonical` | nothing above applied; the built-in path was used |

`default` then means one thing everywhere: the user's catch-all.

## Question 2: a CRD default for `placement.default`

The idea: if the built-in path were the CRD's default for `placement.default`, every GitTarget would
show its own layout, "canonical" would stop being a hidden fourth mechanism, and `source` would
collapse to `byType` and `default`. That reasoning is sound, and for a reader of one object it **is**
clearer. This section was rewritten after review, because the first version of it led with the wrong
objection.

### What is not the reason

**Not "it fails validation" on its own (F3).** It does today, but the rule doing the rejecting is
itself wrong (F4): the versionless canonical template cannot collide two identities, and demanding
`{version}` of it contradicts the decision that removed the version segment. One line fixes it.

**Not "the value gets persisted" (F5).** It does get persisted, on every spec-writing apply, and in a
GitOps-managed cluster that is continuous. But freezing the built-in path per target is arguably
*desirable*: placement is already create-time and non-retroactive, so a long-lived target keeping its
established layout for new types is the stability we already promise, and a string in the spec is
visible and editable where a built-in is neither. `metadata.managedFields` even records that the
server set it, so "we could never tell a default from a declaration" was too strong. What is left is
spec bloat, which decides nothing.

### Why not also default the Secret route?

The obvious repair for F3 is to default `byType["v1/secrets"]` as well: narrowed to one type, it only
needs `{name}` plus scope to be identity-complete, `secretRouteComplete` goes true, and the
bundling-default check stops objecting. It does work, and it is still the wrong move, because
**defaulting a map is a floor that vanishes when you stand on it** (F5b). A user who writes any
`byType` entry of their own replaces the whole map, silently dropping the Secret route and leaving a
defaulted bundling `default` unguarded, so an edit about ConfigMaps flips the object to
`Validated=False`. Fixing F4 removes the need for the workaround entirely.

### The objection that decides it

**A defaulted `default` is never empty, so it shadows the kustomize-root step (F9).** `resolveDeclared`
returns on any non-empty declared template, and the render-root step runs after it. Default the field
and every new file in a kustomize overlay takes the canonical path instead of landing beside the
`kustomization.yaml` with a `resources:` entry: in Git, looking mirrored, rendered by nothing. That is
the exact failure the render-root step exists to prevent, reintroduced by the mechanism meant to make
placement clearer.

And the repairs invert something load-bearing. Putting the render root ahead of `default` makes a
user's real declaration lose to the folder. Reading `managedFields` to distinguish a defaulted value
from a declared one makes where files land depend on field-ownership metadata. Neither is a rule worth
defending in a year.

### What we do instead

Publish the **effective** placement in status. It answers the same question ("what will happen to a
new Deployment in this folder?") from a derived field, so it cannot fork from the code and improves
when the code improves. Crucially it can show the ladder **without collapsing it**: the declared half,
the render root the operator found, and what happened, all at once. A spec default can only
express the ladder by flattening it, which is what F9 is.

**If we want the spec default later**, the order is written down: fix F4, pin the canonical template
against `ToGitPath()` byte-for-byte, and solve F9 first, because nothing else on this page matters
until a defaulted value can coexist with the render-root step. Status is worth having either way, so
nothing built now is wasted.

## `status.layout`

An **observation, not a condition**, in the sense
[`GitTargetStatus.Retention`](../../api/v1alpha3/gittarget_types.go) already establishes: nothing
here may fail a reconciliation or move a condition, because a folder with no declared placement is a
supported configuration and not a fault. It follows `status.streams`'s bounding rule too: counts and
a capped example list, never a per-type list, however many types a target watches.

```yaml
status:
  layout:
    # what the operator understood about the FOLDER
    renderRoot: overlays/production        # empty unless exactly one supported kustomization
    renderRootReason: SingleKustomization  # SingleKustomization | Ambiguous | None
    # where new files are decided FROM
    defaultSource: BuiltInCanonical        # BuiltInCanonical | Declared
    declaredTypes: 2                       # count of placement.byType entries in force
    # what actually happened, bounded
    newFiles: 14                           # documents placed since this watch epoch
    fallbackTypes: 1                       # distinct types that took the built-in path
    refusedResources: 0                    # resources NOT mirrored (see reasons below)
    examples:                              # <= 3, most recent first
      - type: apps/v1/deployments
        path: team-a/apps/deployments/api.yaml
        source: Canonical                  # ByType | Default | KustomizeRoot | Canonical
    observedTime: "2026-07-30T09:14:22Z"
```

`renderRootReason` and `defaultSource` are the two fields that make the operator's judgement
inspectable per target rather than only aggregated in a metric. `examples` is what makes it
actionable: a type plus the path it landed on is the `byType` line to write, spelled out.

### Case 1: greenfield, nothing declared

A target this operator bootstrapped. No `spec.placement`, no kustomization. Canonical is not a
fallback here, it is the layout, and status says so without implying a problem.

```yaml
status:
  layout:
    renderRootReason: None
    defaultSource: BuiltInCanonical
    declaredTypes: 0
    newFiles: 23
    fallbackTypes: 4
    refusedResources: 0
    examples:
      - type: v1/configmaps
        path: team-a/configmaps/app-config.yaml
        source: Canonical
    observedTime: "2026-07-30T09:14:22Z"
```

Nothing to do. `fallbackTypes: 4` with `renderRootReason: None` is a tidy canonical folder.

### Case 2: a kustomize overlay, nothing declared

The folder has one `kustomization.yaml`. Every new file lands beside it and joins its `resources:`
list, so nothing falls back and no declaration is needed.

```yaml
status:
  layout:
    renderRoot: overlays/production
    renderRootReason: SingleKustomization
    defaultSource: BuiltInCanonical
    declaredTypes: 0
    newFiles: 6
    fallbackTypes: 0
    refusedResources: 0
    examples:
      - type: v1/configmaps
        path: overlays/production/debug-toolbox.yaml
        source: KustomizeRoot
    observedTime: "2026-07-30T09:15:02Z"
```

This is the case a metric alone reads ambiguously. `fallbackTypes: 0` plus a named `renderRoot` says
the folder decided, and that the operator agrees with it.

### Case 3: brownfield, one type still missing a rule

The user declared their ConfigMap bundle. Deployments were not covered, and no kustomization governs
the folder, so they went to the built-in path.

```yaml
status:
  layout:
    renderRootReason: None
    defaultSource: BuiltInCanonical
    declaredTypes: 1
    newFiles: 9
    fallbackTypes: 1
    refusedResources: 0
    examples:
      - type: apps/v1/deployments
        path: team-a/apps/deployments/api.yaml
        source: Canonical
      - type: v1/configmaps
        path: all.yaml
        source: ByType
    observedTime: "2026-07-30T09:16:41Z"
```

The two examples side by side are the whole point: one type is doing what was asked, one is not
covered, and the fix reads straight off the field.

```yaml
placement:
  byType:
    v1/configmaps: "all.yaml"
    apps/v1/deployments: "{namespace}/deployments/{name}.yaml"   # the missing line
```

### Case 4: two overlays under one GitTarget (F8, silent today)

`spec.path` covers `overlays/staging` and `overlays/production`. Neither root can be assumed, so new
files take the canonical path, where **no kustomization reaches them**. They are committed, they
look mirrored, and nothing applies them.

```yaml
status:
  layout:
    renderRootReason: Ambiguous
    defaultSource: BuiltInCanonical
    declaredTypes: 0
    newFiles: 3
    fallbackTypes: 2
    refusedResources: 0
    examples:
      - type: v1/configmaps
        path: team-a/configmaps/cache.yaml
        source: Canonical
    observedTime: "2026-07-30T09:18:03Z"
```

`Ambiguous` is the word that does not exist today. The fix is the user's to choose: split the
GitTarget per overlay, or declare a `byType`/`default` that points inside one of them.

### Case 5: a refusal, from a type the static gate cannot see

`placement.default` is a bundle, and an operator-configured sensitive type (via
`--additional-sensitive-resources`) resolves onto it. The `Validated` gate cannot catch this, because
it only knows about core Secrets, so the write path refuses it fail-safe. Today that is a log line
and a resync counter; here it is on the object.

```yaml
status:
  layout:
    renderRootReason: None
    defaultSource: Declared
    declaredTypes: 0
    newFiles: 4
    fallbackTypes: 0
    refusedResources: 1
    refusedReason: SensitiveAppend      # most recent, from the counter's closed set
    examples:
      - type: v1/configmaps
        path: all.yaml
        source: Default
    observedTime: "2026-07-30T09:20:11Z"
```

`refusedResources: 1` is a resource **absent from the mirror**, which is the one number on this
field that is unambiguously a problem to fix.

## Question 3: `{kindLower}`

A variable, not a function. Once `|lower` exists, `|upper`, `|trim` and `|replace` are each one PR
away, every one of them is new validation surface, and every one is a new way to render an empty path
segment. The spec's "keep it small" section already forbids it, and one variable answers the need:

```yaml
placement:
  default: "{kindLower}-{name}{sensitiveSuffix}"    # deployment-simon.yaml
```

`{kindLower}` is `strings.ToLower` of a value we already hold, so it needs no new inputs and cannot
be empty when `{kind}` is not. The alternative, `{resourceSingular}` from discovery's
`SingularName`, is what `kubectl` uses and is more authoritative in principle, but it has to be
threaded from the type registry, needs an empty-value fallback, and I could not construct a real type
where it differs from the lowercased Kind. Not worth the plumbing unless "identical to kubectl" is a
guarantee we want to state.

**Say plainly in the docs** that this layout is not identity-complete: two namespaces holding a
Deployment named `simon` render the same path, and they would land as two documents in one file. It
is the right layout for a single-namespace folder and the wrong one for a fleet folder, and the
operator cannot tell the two apart without being told.

## Is the kustomize root inference?

Yes, in the sense that it reads the repository and its answer can change without any Kubernetes
object changing. It differs from Option C in two ways that decide whether it stays:

| | Option C | kustomize root |
|---|---|---|
| Reads | where similar documents live, an aggregate over content | one structural fact: is there exactly one supported root |
| Changes when | any document is added or removed | a `kustomization.yaml` is added or removed |
| Cost of ignoring it | a file in an unusual place | a file **nothing renders** |

The third row is why it stays; the first two are why it is safe enough to keep. But the visibility
obligation is identical, and that is what `renderRoot` plus `renderRootReason` discharge: the
judgement becomes a field an operator can read, instead of a behavior they infer from where files
appeared.

For the ambiguous case (F8) the choice was between refusing the write and writing it loudly. Refusing
protects nothing: the resource is watched, the user asked for it to be mirrored, and an unrendered
file in Git is still a better record than no file. So we keep writing, and we make it impossible to
miss: `renderRootReason: Ambiguous` on the object, and a third value on the entries counter
(`outcome="no_root"`) so the case that currently attempts no entry is counted like every other
failure to register one.

## What we build, in this PR

Ordered by risk, smallest first.

1. **Metric values.** Split `declared` into `byType` and `default`; keep `canonical`. Unify the prose
   on "canonical" and drop "built-in default" from [`architecture.md`](../architecture.md) and the
   CRD comments (F1, F2).
2. **`{kindLower}`**, plus the single-namespace recipe and its identity caveat in
   [`configuration.md`](../configuration.md) (F7).
3. **Drop the `{version}` requirement** from `IdentityCompletePlacementTemplate` for a non-narrowed
   template, with a test for the versionless canonical shape being accepted. This is a bug fix on its
   own terms and it unblocks any future spec default (F3, F4).
4. **Express the canonical path as a template constant** rendered through
   `RenderPlacementTemplate`, and pin it byte-for-byte against `ResourceIdentifier.ToGitPath()`
   across cluster-scoped, core, grouped and sensitive identities. Removes the hand-written
   `canonicalPath` duplication and is what a future default would reuse.
5. **`status.layout`**, in the shape above, over the `MarkTargetRetention` seam: an epoch-scoped
   per-target roll-up marked from the write path, enqueued on change, projected by the controller,
   asserted by an envtest (F6).
6. **The ambiguous render root**: `renderRootReason: Ambiguous` plus `outcome="no_root"` on the
   entries counter (F8).
7. **Docs**: the spec's resolution-ladder section, `configuration.md`, `interpreting-metrics.md` for
   the new label values, and the `UPGRADING.md` entry extended with the metric-value split.

**Not now, with the trigger written down:** a CRD default for `placement.default`. Revisit only once
F9 has an answer that does not invert declaration precedence or read `managedFields`, and only after
items 3 and 4 land. The freezing question (F5) is a trade we could take; the shadowing question is
not.
`spec.expect.layout` stays out too, on the config-surface doc's own rule: publish the observation
before inventing the assertion.

## Open questions

- Should `status.layout.examples` prefer the **most recent** placements or the **most actionable**
  ones (fall-backs and refusals first)? Most recent is simpler and cheaper; actionable is what
  someone reading it wants. My inclination is actionable, capped at three, with fall-backs ranked
  above successes.
- Does `refusedReason` belong on the field at all, or is the count plus the metric enough? It is the
  one value here that can go stale in a way a count cannot.
- `{resourceSingular}` as a stated "identical to kubectl" guarantee: worth the discovery plumbing, or
  is `{kindLower}` the end of it?
