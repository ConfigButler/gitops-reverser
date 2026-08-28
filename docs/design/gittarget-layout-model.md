# A layout is the declared thing, not a path

> **design**: a proposal, not a plan of record. Nothing here binds until scheduled.
> Index: [`../INDEX.md`](../INDEX.md)
> Date: 2026-07-30. Supersedes Question 2 of
> [`placement-visibility-and-declared-defaults.md`](placement-visibility-and-declared-defaults.md),
> which argued about whether to default a path template. The answer is that a path template is the
> wrong primitive to be defaulting.
>
> This is a `feat(api)!` change to `GitTarget`. It does not fit in PR #291 and is not proposed for it.
> How it sequences with the other breaking work on the same object (`spec.suspend`, `spec.mode`, the
> `commitWindow` move, CommitRequest lifecycle) is in
> [`gittarget-api-wave.md`](gittarget-api-wave.md), which also records the two places where those
> items change this design rather than merely accompanying it.
>
> Concrete repository folders and matching proposed configurations live in
> [`layout-examples/`](layout-examples/README.md). They are intended for reviewing this model, not
> as manifests for the current API.

## Why the current shape keeps producing dead ends

Placement today is a ladder of four rungs, three of which are path templates and one of which is not:

```text
byType -> default -> the folder's one kustomize root -> canonical
```

Every unresolved question in the review of #291 traces to that one mismatch:

- a CRD default for `placement.default` cannot be added, because a non-empty template consumes the
  slot in front of the rung that is **not** a template (F9 in the other document);
- `byType: {v1/configmaps: "configmaps/{name}.yaml"}` in a kustomize folder produces a file no
  kustomization lists, so it is committed and never rendered (F10). One line of user config;
- `placement.default` set on a kustomize folder does the same thing to every type at once, which is
  the same bug with a bigger blast radius;
- "where do my files go" is answered by simulating a four-rung ladder against a folder, which is why
  it needed a metric and a status field to be legible at all;
- nothing in the model can **create** structure, so a repository that needs a `kustomization.yaml`
  before it can render cannot be bootstrapped by the thing that writes into it.

The primitive is wrong. A path template cannot express "beside this folder's one kustomization",
cannot be read at a glance, and cannot bring a folder into existence. What a user wants to declare is
what the folder **is**.

## The model

One field, a discriminated union on `kind`, plus per-type overrides that are valid under every kind.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
spec:
  providerRef:
    name: platform
  branch: main
  path: clusters/prod
  layout:
    kind: Kustomize            # Auto | Kustomize | Tree | Flat | Template
    scope: SingleNamespace     # SingleNamespace | MultiNamespace
    namespace: team-a          # required when scope is SingleNamespace
    writeNamespace: Never      # FromContext | Always | Never
    kustomize:
      root: .                  # relative to spec.path; where the kustomization.yaml lives
      create: true             # write it if absent, so an empty repo becomes a buildable folder
      fileName: "{kindLower}-{name}"   # optional; the default
    byType:                    # optional overrides, valid under every kind
      v1/secrets: "secrets/{name}{sensitiveSuffix}"
```

| `kind` | Where a new document goes | Exists because |
|---|---|---|
| `Auto` (default) | one supported kustomization in the subtree: `Kustomize`; else exactly one namespace in scope: `Flat`; else `Tree`. Resolved once and pinned (see below) | it is what most folders want, and it is honest about reading the repository |
| `Kustomize` | beside the declared root, registered in its `resources:` list | a file that root cannot reach is never applied |
| `Tree` | the built-in `{namespaceOrCluster}/{groupPath}/{resource}/{name}` path | identity-complete by construction; right for a fleet folder |
| `Flat` | `{kindLower}-{name}.yaml` at the root of `spec.path` | the legible single-namespace folder people hand-author |
| `Template` | the declared `default` template | the escape hatch. Everything expressible today stays expressible |

Two rules complete it, and they are where the value is:

1. **Whatever chose the path, the file must be reachable.** If any kustomization governs the
   destination, the write registers the file in it, in the same commit. This is an invariant of the
   layout rather than a special case of one rung, which is what turns F10 from a bug into something
   the model cannot express.
2. **A structural kind and a blanket template are mutually exclusive.** `default` is valid only under
   `kind: Template`. You cannot ask for a kustomize folder and a nested canonical tree at the same
   time.

## What the model makes unstatable

This is the headline, and it is worth more than the readability.

| Today | Under the model |
|---|---|
| `placement.default` on a kustomize folder silently disables the render root and produces unrendered files | unstatable: `default` requires `kind: Template`, and `Template` asserts there is no structural rule |
| `byType` into a subdirectory produces a file no kustomization lists | unstatable: registration is an invariant, not a rung |
| Defaulting the fallback would shadow the structural rung | dissolved: the default is `kind: Auto`, which **names** the structural rule instead of standing in front of it |
| "Which of four rungs answered?" needs a metric to be legible | one word in the spec, and status says what `Auto` resolved to |
| An empty repository cannot be given the structure it needs to render | `kind: Kustomize` with `create: true` |

The defaulting problem is the interesting one. It was never that defaults are bad; it was that we
tried to default a **path**, and a path is the one thing that cannot say "look at the folder".
`kind: Auto` is a safe CRD default precisely because it is a mode: it declares that the folder will be
read, which is the difference between this and the sibling inference we deleted. That inference was
undeclared. This one has a name, appears in the spec, and reports what it resolved to.

## Namespace scope belongs to the layout

A folder that omits the namespace from its paths is a folder for **one named namespace**. That is
what makes `Flat` legible. It is also what lets a newly created Kustomize root establish a namespace
convention without learning it from the first object that arrives.

```yaml
spec:
  allowedSourceNamespaces:        # AUTHORIZATION: who may be mirrored here
    names: [team-a]
  layout:
    kind: Flat
    scope: SingleNamespace        # STRUCTURE: folder cardinality
    namespace: team-a             # STRUCTURE: the folder's namespace identity
```

**These are two different questions and they must agree.** `allowedSourceNamespaces` remains the
permission bound owned by the destination
([`gittarget_types.go`](../../api/v1alpha3/gittarget_types.go)). Its name is therefore correct and
should not be repurposed to mean folder structure. `layout.scope` says how many source namespaces
the folder can represent; `layout.namespace` says which namespace a single-namespace folder means.

For `scope: SingleNamespace`, admission requires an exact one-name
`allowedSourceNamespaces.names` list, no selector, and equality between that name and
`layout.namespace`. The duplication is deliberate: a reviewer can see the authorization decision
and the folder's structural identity where each belongs. A target that authorizes `team-a` while
declaring `layout.namespace: team-b` is refused rather than waiting for an event to reveal the
contradiction.

**Why the values must be stated.** Two reasons, and the second is the one that matters:

- `allowedSourceNamespaces` is an upper bound and may be **absent**, which
  [`NamespaceMatcher`](../../api/v1alpha3/namespace_matcher.go) defines as "no policy declared" rather
  than as "one namespace". There is often nothing safe to derive from.
- the namespaces that do arrive come from **N WatchRule objects that do not own the folder**. A
  rule-derived single-namespace assumption could be invalidated later by a rule created in another
  object, which would turn a layout guarantee into a path collision. Declaring the scope and value
  makes that invalidation a **refusal** instead: a document from a second namespace is declined with
  a message naming both namespaces and counted as a placement refusal, rather than landing on a
  path another object already occupies.

That is the same distinction the whole redesign rests on. Reading the world is fine when the reading
is declared and its failure is loud; it is not fine when it silently re-decides.

## Whether the namespace is written into the file

Today this is **inferred**, and it cannot be inferred in the case bootstrapping cares about most.
`namespaceIsInheritedFromContext` omits `metadata.namespace` exactly when the governing kustomization's
`namespace:` equals the resource's own. An empty folder has no kustomization to read, so a folder we
are about to create cannot inherit a convention that does not exist yet.

```yaml
layout:
  writeNamespace: FromContext     # FromContext (default, today's behavior) | Always | Never
```

| Value | Meaning | When it is valid |
|---|---|---|
| `FromContext` | omit when the governing kustomization sets this resource's namespace | always; today's behavior |
| `Always` | always write `metadata.namespace` | always; the only safe choice when nothing downstream supplies it |
| `Never` | never write it | only when something guarantees the namespace: a kustomization we control with `namespace:` set, or a declared downstream supplier such as a Flux `Kustomization.spec.targetNamespace` |

`Never` needs that guard because omitting the namespace hands the object to whatever namespace the
applier happens to be pointed at, which is a different object with the same name.

**And this is where bootstrapping closes its own loop.** `kind: Kustomize` with `create: true` and
`scope: SingleNamespace` writes `layout.namespace` into the `kustomization.yaml` it creates, and
then legitimately omits `metadata.namespace` from every file it places. The convention is
**established** rather than guessed, which is the thing inference structurally cannot do on an empty
folder.

## The layout is immutable, with one widening exception

An earlier draft of the wave document put `layout` with `prune` as a mutable field. That was wrong.

**Existing files never move**, so a mutable `kind` leaves a folder that is permanently half one layout
and half another, with nothing in the folder recording which file came from which. The structure of a
folder should be a property of the folder, not of the last edit to an object.

**The cost of immutability is lower than it looks, and this is the fact that decides it:** `GitTarget`
has **no finalizer**, so deleting one leaves the folder in Git untouched, and re-creating it at the same
path re-adopts every document by identity (match-first). Changing a layout by recreating the object
costs the object's status and a moment of mirroring. It does not cost data. That is a materially
different bargain from `spec.prune`, where the review's argument for mutability was that a
delete-and-recreate would destroy the one thing that cannot be rebuilt.

**The exception is widening.** `Flat` to `Tree` cannot break the folder: old flat files stay and remain
match-first, new files get identity-complete paths, and nothing collides. Narrowing (`Tree` to `Flat`)
is what can put two namespaces' objects on one path. So the CEL rule is "immutable except a transition
that cannot lose the identity-completeness the folder already had", which in practice means you may
widen and may not narrow.

### `Auto` resolves once and is then pinned

Immutability of the *field* does not pin the *resolution*, because `Auto` says "look at the folder". If
someone deletes the `kustomization.yaml`, `Auto` would silently become `Tree` and the folder would grow
a second layout without any object changing. That is the defect this project spent a release deleting,
re-entering through the default value.

So `Auto` resolves on first observation, `status.layout.kind` records what it became, and a later
folder state that would resolve differently raises a condition rather than re-laying-out the folder.
Declared inference is fine. Silent re-decision is not.

That also settles the earlier open question about whether `Auto` should be the default at all: it can
be, because pinning removes the harm, and it keeps the quickstart to four fields.

## Examples

### 1. `Auto` on a brownfield kustomize repo

No layout declared, so the CRD default applies. The subtree has one supported `kustomization.yaml`.

```yaml
spec:
  path: clusters/prod
  # layout: {kind: Auto}   <- defaulted, so the field is visible in the object
```

A new ConfigMap `cache` in `team-a` lands at `clusters/prod/configmap-cache.yaml` and is added to
`resources:`. `metadata.namespace` is omitted if and only if the kustomization's `namespace:` is
`team-a`. Status reports what `Auto` became:

```yaml
status:
  layout:
    declaredKind: Auto
    kind: Kustomize
    renderRoot: .
    renderRootReason: SingleKustomization
    observedRevision: 9f3c1ab
```

### 2. `Kustomize` with `create: true` on an empty repository

The bootstrapping case. The folder does not exist yet, so nothing can be inferred from it.

```yaml
spec:
  path: clusters/prod
  layout:
    kind: Kustomize
    scope: SingleNamespace
    namespace: team-a
    kustomize:
      create: true
```

The first write commits a folder that builds, rather than a file that happens to be YAML:

```text
clusters/prod/
  kustomization.yaml          # created, listing what was written
  configmap-cache.yaml
```

```yaml
# clusters/prod/kustomization.yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - configmap-cache.yaml
```

`kubectl apply -k clusters/prod` works on the first commit. This is the part the current model cannot
do at all, and it is the difference between mirroring into a directory and producing a GitOps folder.

### 3. `Tree`, declared

A fleet folder holding many namespaces, where a flat layout would collide names.

```yaml
spec:
  layout:
    kind: Tree
```

`clusters/prod/team-a/apps/deployments/api.yaml`. Identity-complete by construction, so two
namespaces holding `api` never share a file. Naming it in the spec is the point: it is a choice now
rather than what is left when nothing matched.

### 4. `Flat`, the layout you asked for by name

```yaml
spec:
  allowedSourceNamespaces:
    names: [team-a]
  layout:
    kind: Flat
    scope: SingleNamespace
    namespace: team-a
    writeNamespace: Always     # nothing here supplies the namespace, so the file must carry it
```

`clusters/prod/deployment-simon.yaml`. Legible, and the reason it cannot be the built-in is that it is
**not identity-complete**: two namespaces with a Deployment named `simon` render one path.

**`Flat` is also the kind that may not omit the namespace**, and this is the table above applied rather
than an exception to it. `Never` is legal only when something guarantees the namespace, and under
`kind: Flat` there is no kustomization for us to write `namespace:` into: a flat directory has no build
step at all. So the only guarantor left would be a downstream applier pointed at the right namespace,
which is a promise made outside this object and invisible to it. `Always` is the honest setting here.
An earlier draft of this example wrote `Never` with the comment "the build supplies it, or the applier
does", which is exactly the hand-wave the `Never` guard exists to refuse.

As a *kind* rather than a template, that is checkable instead of a caveat in prose. `Flat` requires
`scope: SingleNamespace` and `layout.namespace`. Admission requires the matching one-name
`allowedSourceNamespaces.names` list, and a document arriving from a second namespace is refused at
the write boundary with a message naming both namespaces. A template can only document the hazard;
a kind can decline it.

### 5. `Template`, the escape hatch, with the invariant still in force

```yaml
spec:
  layout:
    kind: Template
    default: "{namespace}/{resource}.yaml"    # a per-namespace bundle per type
    byType:
      v1/secrets: "secrets/{namespace}/{name}{sensitiveSuffix}"
```

Everything today's `placement` can express, this can express. The difference is rule 1: if a
kustomization governs `team-a/configmaps.yaml`, the file is registered in it. Today the same
declaration produces an unrendered file (F10) whenever the governing kustomization is an ancestor
rather than a sibling.

### 6. `byType` under a structural kind

Overrides are not exclusive to `Template`, because "kustomize folder, but Secrets in their own
directory" is an ordinary thing to want.

```yaml
spec:
  layout:
    kind: Kustomize
    byType:
      v1/secrets: "secrets/{name}{sensitiveSuffix}"
```

A Secret goes to `clusters/prod/secrets/db.sops.yaml`, and rule 1 registers it in the root's
`resources:`. What is refused is a blanket `default` here, because that would be an assertion that the
folder has no structural rule alongside an assertion that it has one.

### 7. Two roots, and the answer ambiguity has been missing

```yaml
spec:
  path: clusters            # holds overlays/staging AND overlays/production
  layout:
    kind: Kustomize
```

The user asserted a single-root folder. The folder disagrees, so this is a **misconfiguration of the
GitTarget**, not a placement puzzle: `Validated=False`, naming both roots, with the fix being one
GitTarget per overlay. Under `kind: Auto` the same folder resolves to `Tree`, and status says
`renderRootReason: Ambiguous` so nobody has to guess why files stopped landing in the overlays.

This is why the refuse-or-write question was so hard to settle in the current model: without a
declaration there was nothing to contradict. With one, refusing is not a policy preference, it is
honoring what the user said.

## Bootstrapping, and where it stops

`create: true` is deliberately narrow: **the layout may create only what its own invariant requires.**
For `Kustomize` that is exactly one file, the `kustomization.yaml` the layout claims exists, plus the
`resources:` entries for what we write.

Everything else people mean by "bootstrap a GitOps repo" is a different concern: per-environment
directories, an app-of-apps root, a Flux `Kustomization`, a README, a `.sops.yaml`. The last two we
already write ([`bootstrapped_repo_template.go`](../../internal/git/bootstrapped_repo_template.go)),
which is a useful precedent and also a warning: that mechanism is per-path bootstrap staging, and it
is where a "repository template" belongs if we ever grow one. Folding a folder skeleton into
`spec.layout` would make the layout responsible for the shape of a repository it does not own.

So the boundary is: the layout creates what it needs to be true. A repository template, if it ever
exists, is a separate object with a separate lifecycle.

## Should the layout be its own CRD?

The reuse instinct is real: a platform team with thirty GitTargets should not paste the same four
lines thirty times. Three shapes, and I would not build the second one yet.

### The argument for a `LayoutProfile`

- **Reuse.** One house layout, referenced by every target.
- **Guardrails.** A cluster-scoped profile owned by the platform team, referenced by namespaced
  GitTargets, is an RBAC boundary: tenants pick a layout, they do not invent one.
- **One place to change it.** A fleet-wide layout change becomes one edit.

### The argument against, which I find decisive today

**A shared object that changes where N folders write, with nothing on the GitTarget recording it, is
structurally the same defect this project spent a release deleting.** Sibling inference was
removed because a human's edit to a repository changed the operator's behavior with no Kubernetes
object changing and nothing in status recording the move. A profile edited in another namespace is
that same shape with a different actor: the GitTarget that owns the folder is unchanged, unreviewed,
and yet its next new file lands somewhere else. Being an API object rather than a folder makes it
auditable, which is better, but it does not make it *local*, and locality is what made the deletion
worth doing.

The rest is ordinary cost, and it is not small:

- **A third place to look.** The redesign exists to make "where do my files go" answerable from the
  object. A `layoutRef` re-splits the answer across two objects, plus status to reconcile them.
- **Another readiness chain.** `GitProvider` already teaches this: a missing or invalid reference is a
  new `Ready=False` mode, and a profile deleted while targets reference it needs an answer (freeze the
  last resolved layout, or stop writing).
- **Cross-namespace authorization, again.** A namespaced profile referenced across namespaces needs
  what `ClusterProvider` needed: an `allowedNamespaces` selector and a fail-closed SAR. That was
  expensive to build and is expensive to keep correct.
- **The thing being shared is four lines.** `kind: Kustomize` plus two options does not carry enough
  weight to justify an object. The reuse pressure is concentrated entirely in one place: a large
  `byType` map. That is worth remembering, because it means if we ever do share something, we should
  share **the type map**, not the kind.
- **Reuse is already solved one layer up.** Whatever creates thirty GitTargets (Helm, kustomize, a
  Flux `ResourceSet`) repeats four lines for free, and it does so in a place the user already reviews.
  A CRD that exists to avoid repetition in generated YAML is solving a problem the generator does not
  have.

### The middle shape, if evidence demands it

Keep the layout inline and authoritative, add an optional `layoutRef` whose **resolved content is
projected into `GitTarget.status`**, stamped with the profile's `generation`. Reuse without
invisibility: the target still shows what it is doing, and a fleet-wide change is observable per
target rather than only at the profile.

**Recommendation: inline now.** Revisit when someone has a fleet-wide layout they want to
change centrally, and revisit for the `byType` map first, since that is the only part that grows. The
trigger is written down so this is a decision rather than an omission.

## Status under the model

The two-halves rule from the other document still applies: a **current** half derived from the last
repository scan and stamped with the revision it came from, and a **historical** half accumulated
since. Placement is sparse, so the current half must never depend on a placement having happened.

```yaml
status:
  layout:
    # current, from the last scan
    declaredKind: Auto                     # what the spec says
    kind: Kustomize                        # what it resolved to
    renderRoot: .
    renderRootReason: SingleKustomization  # SingleKustomization | Ambiguous | None
    byTypeEntries: 1
    observedRevision: 9f3c1ab
    observedTime: "2026-07-30T09:14:22Z"
    # historical, since observedRevision
    placedResources: 14                    # appends included
    overriddenTypes: 1                     # types a byType entry routed
    refusedResources: 0                    # resources NOT mirrored
    examples:
      - type: v1/secrets
        path: clusters/prod/secrets/db.sops.yaml
        source: ByType
```

`declaredKind` beside `kind` is the pair that makes `Auto` honest: it says both what was asked for and
what the folder produced, so declared inference never looks like a decision the user made.

## Metrics under the model

`placements_total` keeps `source` (which mechanism produced the path) and gains `layout` (the resolved
kind), because they answer different questions and both are one label:

```promql
# is any target's declared layout not the one its folder produces?
sum by (gittarget_name, layout) (increase(gitopsreverser_placements_total[24h]))
```

`source` values follow the model: `byType`, `layout` (the kind's own rule), and nothing else. The
current `kustomize_root` and `canonical` values become `layout` with the kind in the other label, and
`declared`/`default` collapse into `byType` plus `Template`. That is a metric-label break, and it is
free while nothing consumes them.

## Migration

Mechanical, and every current configuration has an exact image:

| Today | Under the model |
|---|---|
| no `spec.placement` | `layout: {kind: Auto}` (defaulted). Same behavior |
| `placement.byType` only | `layout: {kind: Auto, byType: {...}}`. Same behavior, plus rule 1 fixing F10 |
| `placement.default` set | `layout: {kind: Template, default: "..."}`. The declared default no longer shadows the render root, because the kind now says there is no structural rule |
| `placement.byType` + `default` | `kind: Template` with both |

The one behavior change is the good one: a declared template stops silently disabling the render root,
and starts registering its files instead.

`spec.placement` becomes a loud rejection for one release rather than a silent alias, following the
pattern `ClusterWatchRule.spec.rules[].scope` set: refusing a stored field the user can see beats
translating it behind their back. It rides the Tier 2 breaking wave in
[`open-asks-priority.md`](open-asks-priority.md), so the consumer pays one coordinated bump.

## What this changes about the work already queued

Nothing already planned is wasted, and one item should wait:

- **F10's ancestor walk** is rule 1's implementation. Build it now; the model makes it an invariant
  rather than a fix.
- **`{kindLower}`** is `Flat`'s file name and a `Template` variable. Build it now.
- **The `{version}` identity fix** is needed by `Flat`'s validation and by any versionless template.
  Build it now.
- **Canonical as a template constant** is `Tree`'s implementation. Build it now.
- **`status.layout`** is the same field, one release early, and `declaredKind`/`kind` slot into it.
  Build it now.
- **The ambiguity policy** should wait. In this model it follows from whether the user asserted a
  root, and deciding it before the model exists would bake in an answer to a question the model asks
  differently.

## Open questions

- ~~Is `Auto` the right default?~~ **Yes**, given that it resolves once and pins the result. Pinning
  is what removes the harm, and the quickstart stays four fields.
- ~~Should `Flat` be refused for a multi-namespace target?~~ **Refuse the writes, not the target.** A
  scope-widening edit in another object must not break a folder; the second namespace's documents are
  declined with a counted refusal naming both namespaces, and the fix is a widening layout change.
- Should `scope` be **derived and materialized** at creation (write `SingleNamespace` into the spec
  when exactly one namespace is admitted) rather than declared? It would make the common case
  zero-config, at the price of a mutating webhook writing spec, which this project has deliberately
  avoided outside identity capture.
- Does `writeNamespace: Never` need to name its supplier (`Kustomize`, `FluxTargetNamespace`,
  `Asserted`) so validation can check the guarantee rather than trust it?
- Does `kind: Kustomize` imply `create: true`? Asserting a folder is a kustomize folder arguably
  asserts the file exists, and requiring both feels like ceremony. The argument for keeping them
  separate is that creating a file in someone's repository should always be something they asked for.
- Should `Tree` remain identity-complete-by-definition, or become configurable (with or without the
  version segment)? The versionless decision is deliberate and this document does not reopen it.
