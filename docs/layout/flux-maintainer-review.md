# Review: the layout model, its worked examples, and the source-namespace authorization

> Status: external review. Findings open, nothing here binds until scheduled.
> Date: 2026-08-28. Index: [`../INDEX.md`](../INDEX.md).
>
> Second pass. The first covered status and the configuration model at `f37a7ba`; it has been
> retired into the documents that own its findings, with its unbuilt API block sequenced in
> [`api-wave.md`](api-wave.md). This one
> is `L1`-`L27` and covers the material written since: the layout model, its implementation order,
> and the six worked examples. It ends with a long section the first review did not reach, on
> `allowSourceNamespaceOverride` and whether service-account impersonation belongs here.
>
> Stance: read as if this API were proposed for the GitOps Toolkit, with Flux's own source in the
> local `external-sources/flux/` checkout as ground truth rather than recollection. Nothing was
> built, tested, or edited.

## What was read

- [`model.md`](model.md) and
  [`implementation-plan.md`](implementation-plan.md).
- All six scenarios under [`examples/README.md`](examples/README.md),
  every file, including the fixtures and the `.patch` files.
- [`../../api/v1alpha3/clusterprovider_types.go`](../../api/v1alpha3/clusterprovider_types.go),
  [`../../internal/authz/source_namespace.go`](../../internal/authz/source_namespace.go),
  [`../security-model.md`](../security-model.md).
- Flux as ground truth: `pkg/runtime/client/impersonator.go`,
  `pkg/runtime/client/impersonator_options.go`, `pkg/apis/acl`, `pkg/auth/controller_flags.go`,
  `flux-operator/internal/builder/profiles.go`, `flux-operator/internal/controller/resourceset_controller.go`.
  Those paths are in the gitignored `external-sources/` checkout, so they are named rather than
  linked.

## Findings

| # | Finding | Severity |
|---|---|---|
| L1 | `homelab-flux` targets `clusters/home/flux-system`, which is Flux's bootstrap directory | High |
| L2 | Every `input/` fixture is a Git document, not a live object, so sanitization is invisible | High |
| L3 | `layout.kind` reuses `kind`, which nested in a spec means TypeMeta | High (naming) |
| L4 | `status.layout.kind` puts a bare `kind` in status, beside `declaredKind` | High (naming) |
| L5 | `layout.scope` collides with the Kubernetes meaning of `scope` | Medium (naming) |
| L6 | `writeNamespace` mixes a source value with two adverbs | Medium (naming) |
| L7 | `allowSourceNamespaceOverride` names the mechanism, not the privilege | Medium (naming) |
| L8 | `kustomize.create` does not say what it creates | Low (naming) |
| L9 | `kustomize.fileName` has no extension and an ecosystem-foreign variable | Low (naming) |
| L10 | Scenario folder names: `external-base-overlay` and `krm-app-configuration` mislead | Low (naming) |
| L11 | `GitTarget` object names drift in register across scenarios | Low (naming) |
| L12 | `krm-app-configuration` contradicts its own patch | High (correctness) |
| L13 | Every `index` line in every `.patch` is fabricated | Medium (correctness) |
| L14 | `mode: Observe` appears in one of six scenarios, against the README's stated adoption path | Medium |
| L15 | `writeNamespace: Never` is justified by kustomizations the operator does not own | High |
| L16 | `external-base-overlay` is not a layout scenario | Medium |
| L17 | `status.layout` accumulates counters, which belong in metrics | High |
| L18 | `renderRootReason` is a condition reason wearing a field's clothes | Medium |
| L19 | The status stanza has no `conditions` and no `observedGeneration` | Medium |
| L20 | Design change 5 inverts the `spec.interval` convention | Medium |
| L21 | `_cluster` and the collapsed core group are undocumented | Low |
| L22 | `homelab-flux` does not say what happens to an existing document in a bundle | Low |
| L23 | Plan design change 2 misreads its own example | Low |
| L24 | PR 1's ancestor walk is a behavior change filed as correctness | Low |
| L25 | Three product names across the examples | Low |
| L26 | `writeNamespace` is meaningless for cluster-scoped resources and is set anyway | Low |
| L27 | The `Never` guard and the `Flat` rule want the same post-scan validation class | Medium |

The authorization section that follows the findings is not numbered, because it is a design
question rather than a defect.

## What has been addressed

The findings below are answered in this repository; the rest are open. Each entry says where, so a
reader can check the fix rather than trust this table.

| # | Where it was answered |
|---|---|
| L1 | `homelab-flux` now targets `infrastructure/home/sources` and `apps/home/media`, two targets on two layers, with the bootstrap directory shown and left alone. The rule is stated in the scenario README and recorded as [the repository-level peer](../design/support-boundary/render-root-scoping.md#5-the-repository-level-peer-paths-another-controller-writes) in the support boundary |
| L2 | `homelab-argocd` and `homelab-flux` inputs are captured objects, carrying the tracking-id annotation and the Flux finalizer respectively. The other four say they are abridged, and [`examples/README.md`](examples/README.md) states the convention and why the diff is the assertion |
| L12 | `krm-app-configuration` is one schema across `input/`, `repository/` and the patch; the patch body and the committed files are now byte-identical |
| L13 | Every `index` line is gone from all six patches, and each patch was checked to `git apply` against its scenario's stated starting state |
| L14 | Every brownfield scenario opens in `Observe` and shows the observed `status.layout` before the patch. `krm-app-configuration` stays in `Write` as the empty-repository exception, and both READMEs say so |
| L21 | `homelab-cluster-tree` names both conventions and links the grammar: the core group collapses to no segment, and `_cluster` cannot collide because an underscore is invalid in a namespace name |
| L23 | The plan no longer claims the brownfield example declares the invalid combination; it describes the latent case, which is also the argument for pinning |
| L24 | Shipped as [#319](https://github.com/ConfigButler/gitops-reverser/pull/319), separately from the layout work, so it can be released on its own |
| L7, `serviceAccountName` | Answered in [`../design/source-scope-simplification.md`](../design/source-scope-simplification.md), which declines impersonation and keeps `allowedNamespaces` as `accessFrom` |

## L1. The scenario a Flux maintainer would stop at

> **Addressed.** The paths and files named below are the fixture as it was reviewed; it has since
> been restructured along the lines this finding recommends. See
> [`examples/homelab-flux/README.md`](examples/homelab-flux/README.md) for what it is now. The
> finding is kept in its original terms because the argument, not the fixture, is the thing worth
> preserving.

`homelab-flux` points its `GitTarget` at `clusters/home/flux-system`. That path is owned by
`flux bootstrap` and by flux-operator. In a bootstrapped repository it holds `gotk-components.yaml`,
`gotk-sync.yaml`, and a `kustomization.yaml` listing both. The scenario's
`repository/kustomization.yaml` listed
neither, so no reader who has run `flux bootstrap` recognizes the folder as theirs.

The deeper problem is not recognition. An operator that adds `resources:` entries to the bootstrap
kustomization is a second writer in a folder Flux's own sync loop reconciles, which is the
two-writers-one-folder failure the support boundary exists to prevent. The example teaches the
opposite of the rule.

Two further tells in the same fixture:

- `namespace: flux-system` on the root transformer means `HelmRelease/jellyfin` in
  `repository/media.yaml` installs jellyfin
  into `flux-system`. There is no `targetNamespace` and no `storageNamespace`. Nobody runs a media
  server in `flux-system`.
- Flux's documented repository structures keep `clusters/<name>/` holding `Kustomization` pointers,
  with `apps/` and `infrastructure/` as separate trees. This fixture collapses the cluster layer
  and the application layer into one folder, which is the shape those structures exist to avoid.

### The fix, and the sentence worth more than the fix

Point the target somewhere the bootstrap does not own, and split the layers:

```text
clusters/home/flux-system/     # flux bootstrap owns this. GitOps Reverser never writes here.
  gotk-components.yaml
  gotk-sync.yaml
  kustomization.yaml

infrastructure/home/sources/   # spec.path for the GitTarget
  kustomization.yaml
  gitrepository-homelab.yaml
  helmrepository-jellyfin.yaml

apps/home/media/               # a second GitTarget, if HelmReleases are wanted too
  kustomization.yaml
  helmrelease-jellyfin.yaml
```

Then state the rule in the scenario README, because it generalizes past this example:

> A `GitTarget` must not point at a path another controller writes. Flux's bootstrap directory
> (`clusters/<name>/flux-system`) is the common case, and the `Kustomization` that reconciles a
> folder is not a licence to co-write it.

That sentence belongs in the support boundary as well. It is the repository-level peer of the
render-root scoping rule already recorded in
[`render-root-scoping.md`](../design/support-boundary/render-root-scoping.md).

## L2. The `input/` fixtures are not live objects

This is the most substantive gap across all six scenarios, and it is one edit per scenario to fix.

Every `input/*.yaml` is the desired Git document with `namespace:` added back. A captured object
carries `metadata.uid`, `resourceVersion`, `generation`, `creationTimestamp`, `managedFields`, a
populated `status`, and, in the two scenarios a Flux or Argo reviewer will look at hardest:

- `finalizers: [finalizers.fluxcd.io]` on the `HelmRepository`;
- `finalizers: [resources-finalizer.argocd.argoproj.io]` and the
  `argocd.argoproj.io/tracking-id` annotation on the `Application`.

The tracking-id one is not hypothetical for this project. A committed tracking-id hard-fails
another application's sync, which is why sanitization carries an exact-key deny rule rather than a
prefix strip. The Argo scenario is the natural place to demonstrate that the annotation does not
reach Git, and right now it demonstrates nothing, because the annotation was never in the input.

### What the fixture should look like

```yaml
# input/application-paperless.yaml, as the operator receives it
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: paperless
  namespace: argocd
  uid: 4d9f1e2c-6a1b-4c9e-9f2a-1b3c5d7e9f01
  resourceVersion: "184203"
  generation: 2
  creationTimestamp: "2026-08-12T09:41:07Z"
  finalizers:
    - resources-finalizer.argocd.argoproj.io
  annotations:
    argocd.argoproj.io/tracking-id: paperless:argoproj.io/Application:argocd/paperless
  managedFields:
    - manager: argocd-server
      operation: Update
      apiVersion: argoproj.io/v1alpha1
spec:
  project: default
  # ... as today
status:
  sync:
    status: Synced
  health:
    status: Healthy
```

The committed patch stays exactly as it is. That is the point: the diff between a realistic input
and an unchanged expected patch *is* the sanitization assertion, and the corpus harness in PR 1
gets it for free. Today the harness would assert placement and nothing else.

Do this for at least `homelab-argocd` and `homelab-flux`. The other four can keep minimal inputs if
each README says the input is abridged.

## Naming

You asked about naming specifically, so this section is longer than its severity warrants. The
ordering is by how hard a maintainer would push.

### L3. `layout.kind` should not be called `kind`

A nested `kind` in a Kubernetes spec has one established meaning: a TypeMeta kind. In Flux it
appears nested only inside object references, where it truly is one: `sourceRef.kind`, `ref.kind`,
`chart.spec.sourceRef.kind`. Reusing it for "which structural rule governs this folder" trains the
reader's eye wrong, and the collision is close enough to be cruel: `layout.kind: Kustomize` sits one
character from Flux's actual `Kustomization` kind, in a field whose sibling is spelled `kustomize`.

```yaml
# now: reads as a reference to a Kustomization object, and is not one
layout:
  kind: Kustomize
  kustomize:
    root: .

# proposed
layout:
  type: Kustomize
  kustomize:
    root: .
```

`type` is the discriminator spelling Kubernetes uses everywhere a union needs one:
`Service.spec.type`, `PersistentVolume` source unions, `Condition.type`. `strategy` is the second
choice and carries a nice connotation (`Deployment.spec.strategy.type`), but it invites a nested
`strategy.type` that this field does not need.

### L4. The status pair is worse than the spec field

```yaml
# now
status:
  layout:
    declaredKind: Auto
    kind: Kustomize

# proposed
status:
  layout:
    declaredType: Auto
    resolvedType: Kustomize
```

Two problems collapse at once. `status.layout.kind` puts a bare `kind` inside status, where a
reader scanning a `kubectl get -o yaml` dump has every reason to read it as the object's kind. And
the asymmetric pair `declaredKind` / `kind` hides the relationship: the second value is not a kind,
it is the *resolution* of the first. `declaredType` / `resolvedType` says the whole `Auto` story in
two field names, which is the story the design most wants a reader to absorb.

### L5. `layout.scope` collides with the Kubernetes meaning of scope

In Kubernetes, `scope` on an API object means cluster-versus-namespaced:
`CustomResourceDefinition.spec.scope`, `APIResource.namespaced`, the `+kubebuilder:resource:scope=Cluster`
marker on `ClusterProvider` itself. A reviewer reading `layout.scope` expects `Cluster | Namespaced`
and finds `SingleNamespace | MultiNamespace`, which is a cardinality.

```yaml
# now
layout:
  scope: SingleNamespace
  namespace: team-a

# proposed
layout:
  namespaceScope: Single
  namespace: team-a
```

`namespaceScope: Single | Multi` removes the misread and shortens the values. It also reads
correctly beside `layout.namespace`, which is the field it governs.

### L6. `writeNamespace` values are from two different categories

`FromContext | Always | Never` mixes a source (`FromContext`) with two frequency adverbs. Enums
read best when every value answers the same question in the same grammar.

| Now | Proposed | Meaning |
|---|---|---|
| `FromContext` | `Auto` | omit when the governing kustomization supplies this resource's namespace |
| `Always` | `Always` | always write `metadata.namespace` |
| `Never` | `Never` | never write it, and prove something else supplies it |

`Auto` here does echo `layout.type: Auto`, and that echo is a feature: both mean "read the folder,
record what you found". The field name `writeNamespace` is good and should stay.

### L7. `allowSourceNamespaceOverride`

Covered at length in the authorization section below, because the naming problem is downstream of
a modelling problem. The short version: "override" names the mechanism (a default being
overridden) rather than the privilege (reading namespaces you do not own), and a boolean is the
wrong shape for a question that already has two policy objects answering neighbouring halves.

### L8. `kustomize.create`

`create: true` does not say what it creates. The reader has to know that a `Kustomize` layout's
invariant is exactly one file before the field means anything.

```yaml
# now
kustomize:
  root: .
  create: true

# proposed
kustomize:
  rootPath: .
  createRoot: true
```

`rootPath` is a smaller win and optional. Flux spells this concept `path` on every object that has
one, and `root` alone reads like a boolean or a mode until you see its value.

The design's open question, "does `type: Kustomize` imply `createRoot: true`", has a firm answer
from this direction: no. Writing an unrequested file into somebody's repository is the kind of
thing an operator should never do as a side effect of a type discriminator. The design already
leans this way; close the question.

### L9. `kustomize.fileName`

```yaml
fileName: "{kindLower}-{name}"
```

Two nits. The template has no extension, so a reader cannot tell whether `.yaml` is appended or was
forgotten. And `{kindLower}` is a variable-naming convention that exists nowhere in kustomize, Flux,
or Argo. If the template engine is ours, its variable list belongs in one referenced table, and the
example should show `"{kindLower}-{name}.yaml"` or the docs should state that the extension is
appended.

### L10. Two scenario folder names mislead

`external-base-overlay` is the worse one. The base is in the same repository, one directory up. It
is not external in any sense the reader brings, and "external base" names precisely the thing the
Kustomize support boundary refuses (a remote base). Rename to `overlay-scoped-target`, which also
says what the scenario is *for*: the target owns the overlay and not the base.

`krm-app-configuration` uses an acronym that appears in neither the Flux nor the Argo
documentation. Its own entry in the scenario table already names the question better than the
folder does ("How does an empty repository become a deployable, single-namespace folder?"), so
`empty-repo-bootstrap` is right there.

`homelab-cluster-tree` is the odd member of a family otherwise named by tool
(`homelab-argocd`, `homelab-flux`). `tree-multi-namespace` says what it exercises.

| Now | Proposed |
|---|---|
| `brownfield-kustomize` | keep |
| `external-base-overlay` | `overlay-scoped-target` |
| `krm-app-configuration` | `empty-repo-bootstrap` |
| `homelab-cluster-tree` | `tree-multi-namespace` |
| `homelab-argocd` | keep |
| `homelab-flux` | keep |

### L11. The `GitTarget` names drift

`demo`, `home-state`, `flux-system`, `argocd-applications`, `shop-configuration`,
`podinfo-production`. Six scenarios, four naming registers. `GitTarget/flux-system` in namespace
`flux-system` is the one that actively confuses, because the reader cannot tell which of the two
`flux-system` tokens a message refers to.

One convention, "name the folder's contents", fixes all six: `home-cluster-state`,
`flux-declarations`, `argocd-applications`, `shop-configuration`, `podinfo-prod-overlay`, and
something for `demo` that is not the word demo. `demo` in namespace `demo` at path `apps/demo` is
three uses of one word carrying no information.

## Correctness in the examples

### L12. `krm-app-configuration` contradicts its own patch

Its README says `repository/` is the state after the first write. It is not.

```yaml
# repository/shopconfiguration-storefront.yaml, committed
spec:
  catalog:
    defaultCurrency: USD
  checkout:
    sessionTimeout: 15m
  theme:
    primaryColor: blue

# expected-first-write.patch and input/, committed
spec:
  theme: dark
  primaryColor: teal
  requests:
    cpu: 100m
    memory: 128Mi
```

Different fields, and `theme` is a scalar in one and a mapping in the other, so they are not even
two versions of one schema. The corpus harness planned for PR 1 catches this on its first run,
which is an argument for the harness, but the document is wrong in the meantime and a reviewer who
notices will discount the rest of the corpus.

### L13. Every `index` line in every patch is fabricated

Checked all ten against `git hash-object`. Not one matches:

| File | Patch claims | Actual |
|---|---|---|
| `brownfield-kustomize/repository/kustomization.yaml` | `9434b9f` | `ef97cd1` |
| `homelab-argocd/repository/kustomization.yaml` | `d3ad041` | `502b770` |
| `homelab-flux/repository/kustomization.yaml` | `617a9c0` | `ece7052` |
| `external-base-overlay/.../overlays/prod/kustomization.yaml` | `7d7f47d` | `01f1b24` |

The implementation plan already says the harness normalizes hashes and index lines out of the
comparison, which is the right call. Do it to the committed files now as well. As they stand they
carry the visual authority of `git diff` output without its guarantee, and a reader who tries to
`git apply` one gets a confusing failure.

Strip the `index` lines entirely. `diff --git`, the `---`/`+++` pair, the mode line, and the hunks
are the reviewable content.

### L14. `mode: Observe` appears once in six scenarios

The top-level README states the adoption path plainly: "`mode: Observe` is the adoption path...
Change it to `Write` only after the resolved root and prospective paths match the repository
owner's intent." Then five scenarios open at `mode: Write`, four of which are brownfield folders
with existing content.

Either every brownfield scenario starts in `Observe` and shows the resolved status before showing
the patch, or the README stops claiming that is the path. The first is better and costs four lines
of status per scenario, which is also the best advertisement `mode: Observe` will get.
`empty-repo-bootstrap` is the one honest exception, because there is nothing to observe.

### L15. `writeNamespace: Never` rests on kustomizations the operator does not own

This is the finding with the largest blast radius, and it is a gap in the model rather than in the
examples.

The model's guard reads: `Never` is valid "only when something guarantees the namespace: a
kustomization we control with `namespace:` set, or a declared downstream supplier such as a Flux
`Kustomization.spec.targetNamespace`". Three scenarios set `Never`:

| Scenario | What supplies the namespace | Who owns it |
|---|---|---|
| `krm-app-configuration` | the `kustomization.yaml` the layout creates | the operator |
| `homelab-flux` | a pre-existing `kustomization.yaml` | the user |
| `homelab-argocd` | a pre-existing `kustomization.yaml` | the user |
| `external-base-overlay` | a pre-existing overlay `kustomization.yaml` | the user |

Only the first is inside the guard as written. In the other three, a user removing one line from
their own file silently redirects every subsequent document to whatever namespace the applier
defaults to, which is the exact failure the guard exists to prevent. Nothing refuses it, nothing
reports it, and the objects land in a namespace where they are a different object with the same
name.

The fix is already half-built in the plan. Design change 2 introduces post-scan validation for
per-type field validity, because `Auto` resolving to `Flat` cannot be checked in CEL. `Never` needs
the same treatment for the same reason: its precondition is a property of the folder, observed per
scan, not a property of the spec. See L27.

### L16. `overlay-scoped-target` is not a layout scenario

Its expected patch edits an `images:` transformer. That is the write path and the render-fidelity
gate, not `LocateNew`. No file is placed, no `resources:` entry is added, and the layout type never
enters the decision.

It is a good scenario. It is filed in the wrong drawer, and the cost shows up in PR 1: the harness
described in the plan seeds a worktree, builds a write event, derives a flush policy, and compares
a patch. This scenario needs the token write-back machinery on top of all of that, so it is the one
folder that will not fit the loop.

Two options. Move it to
[`../design/support-boundary/`](../design/support-boundary/README.md) beside the token write-back
material, or keep it here and label it explicitly as exercising the write path rather than
placement, with its own harness entry point. The scenario table's question for it ("How can an
environment overlay change a supported field without claiming ownership of its base?") is a write
scope question and should be phrased as one.

## Status

### L17. `status.layout` accumulates counters

```yaml
status:
  layout:
    placedResources: 14
    overriddenTypes: 1
    refusedResources: 0
    examples:
      - type: v1/secrets
        path: clusters/prod/secrets/db.sops.yaml
        source: ByType
```

This is not how status is modelled in this ecosystem. Status is desired-versus-observed plus
conditions. Counts of things that have happened are metrics, and `placements_total` already carries
them with better labels than a status field can. The design even says so in its own metrics
section, which makes the status counters a duplicate rather than an addition.

The concrete cost is not aesthetic. A monotonic counter in status means a status write per event on
a busy target, so a folder receiving a hundred writes a minute produces a hundred etcd writes a
minute for information nobody polls at that resolution. It also interacts badly with `F3` from the
first review, which was about always-moving values creating a self-triggering reconcile edge.

Recommendation: keep the current half, drop the historical half.

```yaml
status:
  layout:
    declaredType: Auto
    resolvedType: Kustomize
    renderRoot: .
    byTypeEntries: 1
    observedRevision: 9f3c1ab
    observedTime: "2026-07-30T09:14:22Z"
```

`examples` is the one item with a real argument for staying, because "show me where a Secret would
land" is not a metric. If it stays, cap it hard (three entries), make it a fixed-size sample rather
than an accumulation, and say in the field documentation that it is illustrative.

### L18. `renderRootReason` is a condition reason

`renderRootReason: SingleKustomization | Ambiguous | None` is a reason enum in a bespoke field.
Every consumer in this ecosystem already reads reasons from one place:

```yaml
status:
  conditions:
    - type: LayoutResolved
      status: "True"
      reason: SingleKustomization
      message: "resolved Auto to Kustomize at render root '.'"
      observedGeneration: 4
    - type: Ready
      status: "True"
      reason: Succeeded
```

Same information, in the shape `kstatus`, the Flux CLI, and `kubectl wait --for=condition` already
understand. The first review's `F8` was about ad-hoc reason vocabulary; this is the same finding
arriving one document later, before the field is built.

This matters for sequencing more than for design. PR 3 ships `renderRootReason` as a status field
a release before the model that defines it, and the plan sells PR 3 as additive and non-breaking.
If it later becomes a condition reason, that is a second break on a field promised as stable.
Decide it before PR 3, not after.

### L19. The status stanza has no conditions and no `observedGeneration`

Every scenario README asserts `Ready=True`. The model's status example shows no `conditions` array
at all. The two documents disagree about what status looks like, and a reader assembling the API
from them gets neither. Add both to the model's stanza, even though they are obvious, because the
scenarios are already promising them.

### L20. Design change 5 inverts the `interval` convention

The plan argues: "The smell is one name on two objects, so name the provider's field for what it
polls. Two fields, two names, no merge."

From a Flux reading, one name on two objects is not a smell. `spec.interval` appears on
`GitRepository`, `OCIRepository`, `HelmRepository`, `Bucket`, `Kustomization`, `HelmRelease`,
`ImageRepository`, and `Receiver`, and on every one of them it means the same thing: how often this
object reconciles. Users learn it once. Naming `GitProvider`'s field something else breaks the one
transfer they get for free, and it breaks it on the object whose behavior matches Flux's most
exactly: `GitRepository.spec.interval` *is* an `ls-remote` cadence.

If the two must be distinguished, distinguish the novel one. `GitTarget` is where the meaning is
new, because an observation pass over a folder is not a thing Flux has.

| Object | Plan | Proposed |
|---|---|---|
| `GitProvider` | renamed for what it polls | `interval`, matching `GitRepository` |
| `GitTarget` | `interval` | `interval`, documented as the observation cadence |

The stronger form of this argument is that both should be `interval` and each field's
documentation should say what it drives. Two objects having a reconcile cadence is not a collision;
it is the convention.

## Smaller findings

### L21. `_cluster` and the collapsed core group are undocumented

The cluster-tree fixture shows two path shapes:

```text
clusters/home/_cluster/rbac.authorization.k8s.io/clusterroles/homelab-viewer.yaml
clusters/home/media/configmaps/jellyfin.yaml
```

Two conventions are load-bearing and neither is written down. The core group collapses to no path
segment, so `configmaps` sits where `rbac.authorization.k8s.io` sits in the other path. And
`_cluster` stands in for the namespace segment.

Every reader's first reaction to `_cluster` is "what if somebody has a namespace called that". The
answer is good: underscores are invalid in a namespace name, so the segment cannot collide. Say it
in one clause and the worry is dead. Leave it unsaid and every reviewer spends the same thirty
seconds.

### L22. `homelab-flux` describes the create case and not the update case

The README says a new declaration gets a sibling file rather than being appended to `media.yaml`,
which is the right rule stated clearly. It does not say what happens when the `jellyfin`
`HelmRepository` that already lives inside the multi-document `sources.yaml` changes. Match-first
identity presumably updates it in place, inside the bundle it is already in. That is the more
interesting half of the rule, because it is where "we do not author bundles" and "we do not move
existing files" have to coexist. One paragraph.

### L23. Plan design change 2 misreads its own example

The plan says per-type field validity is unverifiable in CEL under `Auto`, "and the brownfield
example declares exactly that combination". It does not. `brownfield-kustomize` declares
`writeNamespace: FromContext` under `Auto`, and `Auto` resolves to `Kustomize` there, where
`FromContext` is valid. The violation is hypothetical: it appears only if the `kustomization.yaml`
is later deleted and the folder re-resolves.

The finding is right and the example is not evidence for it. Either construct a scenario that does
declare the invalid combination, or reword to "the brownfield example would violate this if its
kustomization were removed", which is a better illustration of why pinning exists anyway.

### L24. PR 1's ancestor walk is a behavior change

The plan files both `#295` fixes as "direction-agnostic correctness, and neither touches the API".
True of the API, not true of behavior. Anyone today with a `byType` entry pointing into a
subdirectory gets files that no kustomization lists. After PR 1 those files are registered, so the
next `kustomize build` renders objects that were previously inert, and the next apply creates them.

That is the fix working. It is still a behavior change that can surface resources in a cluster, and
it belongs in release notes as one rather than under a correctness heading.

### L25. Three product names

The examples carry `configbutler.ai/v1alpha3` in every manifest, "GitOps Reverser" in every README,
and `gitops-reverser` in the repository path. Flux is uniformly `*.toolkit.fluxcd.io`. If the split
is settled, one line in the examples README defusing it costs nothing. If it is not settled, the
examples are where the cost is most visible, because a reader meets all three names inside one
scenario.

### L26. `writeNamespace` on cluster-scoped resources

`tree-multi-namespace` sets `writeNamespace: Always` and captures a `ClusterRole` through its
`ClusterWatchRule`. A `ClusterRole` has no namespace, so the field cannot apply. Nothing is broken,
but the enum's documentation should say the field governs namespaced resources only and is ignored
for cluster-scoped ones. Under `Tree` that is the common case, since `Tree` is the type most likely
to carry both.

### L27. One post-scan validation class, not two

L15 and plan design change 2 are the same finding. Both describe a rule whose precondition is a
property of the observed folder rather than of the spec:

| Rule | Precondition | Checkable at admission |
|---|---|---|
| `Flat` forbids `FromContext` and `Never` | resolved type is `Flat` | only when `type` is declared |
| `Never` requires a namespace supplier | a kustomization with `namespace:` governs the path | never |
| declared `Kustomize` requires one root | the folder has one root | never |

Three rules, one class. Build them as one post-scan validation pass that sets `Validated=False`
naming the resolved type and the offending field, and add a corpus scenario per row with an
`expected-status.yaml` instead of a patch. The plan already calls for exactly this shape for the
first row and for the ambiguous-root case; extending it to the `Never` guard closes the gap L15
opens and costs no new machinery.

## The authorization model

Now the part you flagged as feeling off. It does, and the naming is the symptom rather than the
disease.

### What the current shape is

Three objects participate in deciding which source namespaces a `WatchRule` may read:

```text
ClusterProvider.spec.allowedNamespaces          which CONFIG-plane namespaces may reference me
ClusterProvider.spec.allowSourceNamespaceOverride  may my consumers name a source namespace at all
GitTarget.spec.allowedSourceNamespaces          which SOURCE namespaces may write into my folder
WatchRule.spec.rules[].sourceNamespace          which one this item wants
```

The gate in [`source_namespace.go`](../../internal/authz/source_namespace.go) reads them in that
order, and its three-part contract is sound: the legacy own-namespace case stays free, any other
namespace needs both the delegation flag and an explicit target policy, and a declared policy is
exhaustive with no self-namespace carve-out. The security model doc states the consequence honestly:
on an in-cluster provider the flag deliberately bypasses live namespace RBAC.

The mechanism works. Four things about its shape are off.

**One. Two fields named `allowed*Namespaces` on adjacent objects mean different planes.**
`ClusterProvider.spec.allowedNamespaces` is control-plane (where `GitTarget` objects may live).
`GitTarget.spec.allowedSourceNamespaces` is source-plane (which watched namespaces may be
mirrored). A reader who has met one arrives at the other with the wrong model, and the only thing
distinguishing them is the word `Source` in the middle of the longer one.

Flux has a name for the first concept and this project already imports the module that defines it.
`acl.AccessFrom` with its `namespaceSelectors` is precisely "which namespaces may reference this
object", and `acl.AccessDeniedCondition` is the abnormal-true condition that goes with it.

```yaml
# now
spec:
  allowedNamespaces:
    names: [homelab-config]

# proposed, using fluxcd/pkg/apis/acl verbatim
spec:
  accessFrom:
    namespaceSelectors:
      - matchLabels:
          kubernetes.io/metadata.name: homelab-config
```

That is a real trade rather than a free win: `AccessFrom` is selector-only, and `NamespaceMatcher`
supports an exact `names` list that a homelab owner finds far easier to write. The recommendation
is therefore the name, not necessarily the type. `accessFrom` on `ClusterProvider`, keeping
`NamespaceMatcher` as its value, removes the collision and borrows a vocabulary the audience has.
Adopting `acl.AccessDeniedCondition` is separately worth it and cheap.

**Two. `allowSourceNamespaceOverride` names the mechanism, not the privilege.** "Override" describes
what happens to a default. The privilege being granted is "read namespaces this tenant does not
own, through the operator's cluster-wide credential". Nothing in the field name says credential,
read, or cross-namespace.

**Three. It is a boolean where the question is a policy.** There are already two policy objects
answering neighbouring halves of this question. A third, degenerate, boolean policy sitting between
them is the odd one out, and it is the reason the delegation cannot be expressed as "these
namespaces may be delegated" without editing every `GitTarget`.

**Four. Locality.** Reading a `WatchRule` tells you nothing about whether it will work. The answer
lives two objects away, on a cluster-scoped object the rule's author probably cannot read. That is
the same locality complaint the layout model makes against a `LayoutProfile`, and it is the
strongest internal argument for changing something here: the project has already decided that
non-local behavior is the defect worth paying to remove.

### Proposal: one enum, and the third value is the point

```yaml
# now
apiVersion: configbutler.ai/v1alpha3
kind: ClusterProvider
spec:
  allowedNamespaces:
    names: [homelab-config]
  allowSourceNamespaceOverride: true

# proposed
apiVersion: configbutler.ai/v1alpha3
kind: ClusterProvider
spec:
  accessFrom:
    names: [homelab-config]
  crossNamespaceSources: Allow      # Deny (default) | Allow | RequireServiceAccount
```

| Value | Meaning |
|---|---|
| `Deny` | a `WatchRule` may watch only its own namespace. Today's `false`, and the default |
| `Allow` | a `WatchRule` may name other namespaces, bounded by the target's policy. Today's `true` |
| `RequireServiceAccount` | as `Allow`, and every cross-namespace item must name a `ServiceAccount` whose RBAC the read is performed under |

`crossNamespaceSources` borrows Flux's vocabulary directly. Flux's controller flag is
`--no-cross-namespace-refs`, and every Flux user has met the phrase. The value `Deny` reads
correctly as a default in a way `false` does not, because `false` on a field called `allow...`
makes a reader parse a double negative.

The third value is why this is worth doing rather than a rename. It converts the delegation from
"the platform admin trusts these tenants" into "the platform admin requires the tenant to prove
authority", which is a policy a platform admin can set once and stop thinking about. That is the
bridge to the next section.

An enum also leaves room the boolean does not. If a future release wants "these namespaces may be
delegated, no others", that is a field beside the enum rather than a second boolean.

## Should we adopt `serviceAccountName`?

Worth taking seriously. Below is what Flux does, verified in its source; what it would mean here;
what it buys that the current model cannot buy at any price; what it costs, including two costs
specific to this architecture; and the multiple-service-account question, which turns out to have a
better answer than the obvious one.

### What Flux does, precisely

From `pkg/runtime/client/impersonator.go` in the local checkout:

- `Impersonator.GetClient` builds a `rest.Config` from one of three sources: the object's
  `spec.kubeConfig`, the in-cluster config with impersonation, or the plain in-cluster config.
- `setImpersonationConfig` sets `restConfig.Impersonate.UserName` to
  `system:serviceaccount:<serviceAccountNamespace>:<name>`, preferring `spec.serviceAccountName`
  over the controller-wide `--default-service-account`.
- It is called from **both** the kubeconfig path and the in-cluster path. Impersonation composes
  with a remote kubeconfig rather than being an alternative to it. That is directly relevant here,
  because `ClusterProvider.spec.kubeConfig` is the same `meta.KubeConfigReference` type.
- `serviceAccountNamespace` is always the reconciled object's own namespace. In
  `flux-operator/internal/controller/resourceset_controller.go` the call is
  `WithServiceAccount(r.DefaultServiceAccount, obj.Spec.ServiceAccountName, obj.GetNamespace())`.
- `CanImpersonate` checks only that the `ServiceAccount` exists. Authorization is left entirely to
  the API server.

The locality property is the whole security argument, and it is worth stating on its own:

> An object can impersonate only a `ServiceAccount` in its own namespace. So the authority a
> tenant can claim is bounded by the RBAC that tenant's namespace already grants, and the
> controller never has to decide anything.

The lockdown half is `--default-service-account`. When the platform admin sets it, an object with
no `serviceAccountName` still impersonates a named SA in its own namespace rather than falling back
to the controller's cluster-wide identity. `flux-operator/internal/builder/profiles.go` shows the
canonical multi-tenant profile doing three things at once: `--no-remote-bases=true`,
`--default-service-account=<name>` on kustomize-controller and helm-controller, and a kustomize
patch forcing `spec.serviceAccountName` onto every `Kustomization`.

One more fact bears directly on your multiple-SA instinct. Flux's workload-identity profile splits
the default into **three** flags: `--default-service-account`,
`--default-decryption-service-account`, and `--default-kubeconfig-service-account`. Flux has
already concluded that one identity per object is too coarse, and the axis it split along is
**concern**, not namespace.

### What it would mean here

The natural spelling, per rule kind:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: home-workloads
  namespace: homelab-config
spec:
  targetRef:
    name: home-cluster-state
  serviceAccountName: homelab-mirror     # in homelab-config, always
  rules:
    - apiGroups: [""]
      apiVersions: ["v1"]
      resources: ["configmaps"]
      sourceNamespace: media
```

with the tenant granting the authority in ordinary RBAC:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: homelab-mirror
  namespace: homelab-config
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: homelab-mirror-media
  namespace: media                      # the SOURCE namespace grants the read
subjects:
  - kind: ServiceAccount
    name: homelab-mirror
    namespace: homelab-config
roleRef:
  kind: ClusterRole
  name: view
  apiGroup: rbac.authorization.k8s.io
```

The operator's own `ClusterRole` remains the outer bound, and the effective permission becomes the
intersection of three things: what the operator may read, what the impersonated SA may read, and
what `GitTarget.spec.allowedSourceNamespaces` admits as a Git destination.

### What it buys that nothing else can

**Real authorization instead of an operator-implemented approximation.** Today the gate in
`source_namespace.go` is a careful reimplementation of an access decision, and its correctness is
this project's responsibility on every code path. Under impersonation the kube-apiserver decides,
using the same RBAC evaluation everything else in the cluster uses. Code we do not write cannot be
wrong.

**Revocation that works.** This is the property the current model cannot reach. Today, removing a
`RoleBinding` in a source namespace changes nothing: the operator reads through its own credential,
so the watch keeps streaming until somebody edits a `ClusterProvider` or a `GitTarget`. Under
impersonation the API server terminates the watch. The gate does re-run on every reconcile, so a
tightened *policy* is revoked, but tightened *RBAC* is not, and RBAC is where a cluster admin
expects to make that change.

**The bypass in the security model becomes optional.** The doc currently has to say, in plain
words, that the delegation flag deliberately bypasses live namespace RBAC on an in-cluster
provider. With `crossNamespaceSources: RequireServiceAccount` that paragraph gains a second half: a
platform admin who is not comfortable granting that can require proof instead of trust, and the
tenant's own namespace RBAC becomes the boundary again.

**It composes with `ClusterProvider.spec.kubeConfig`.** Verified above: Flux impersonates into a
remote cluster through a kubeconfig. So a remote `ClusterProvider` could hold a broad admin
kubeconfig while each tenant's reads are narrowed by an SA in the *source* cluster. That is a
better story than the current one, where a remote provider's credential is the only bound.

**It is strictly narrowing, so it can be additive.** Adding `serviceAccountName` to a `WatchRule`
never widens anything. Every existing rule keeps working unchanged. That makes this a non-breaking
feature, which matters given how much breaking change is already queued.

### What it costs, including two costs specific to this architecture

**The operator needs `impersonate`.** `impersonate` on `serviceaccounts` (and, in Flux's
configuration, on `users` and `groups`) is close to cluster-admin-equivalent: an identity that can
impersonate anybody can do anything anybody can do. Flux accepts this and documents it. This
project has a whole least-privilege posture and a
[`least-privilege-remaining-work.md`](../future/least-privilege-remaining-work.md), so the grant needs
scoping to `serviceaccounts` only, a named `resourceNames` list where practical, and a paragraph in
[`../rbac.md`](../rbac.md).

**Watch-stream sharing is the real cost.** This is where the analogy to Flux breaks and where the
decision lives. Flux impersonates per reconcile: build a client, apply, discard. Nothing
is long-lived. This operator's data plane is shared informers keyed by cluster, GVR, and namespace,
and impersonation adds identity to that key. Two `WatchRule` objects reading `v1/configmaps` in
`media` under different service accounts can no longer share one stream.

The blast radius depends on cardinality. Most clusters will have few distinct service accounts, and
identical identities still dedupe, so the practical multiplier is small. But it is a multiplier on
the resource the `TooManyStreams` cap already exists to bound (PR 6 in the plan), and the two
features should be designed in the same breath rather than a release apart.

**Failure moves from admission to runtime.** Today an unauthorized `WatchRule` is refused by a gate
that can write a precise condition naming the missing flag. Under impersonation the failure is a
`403` on a watch that may start fine and fail later, arrives asynchronously, and can be partial
(three rule items authorized, one not). That is real status work: a per-item condition, a distinct
reason, and a decision about whether one denied item makes the rule `Ready=False`. Flux sidesteps
this because an apply is a discrete operation with a discrete result.

**The audit and attribution plane is unaffected and must stay that way.** Authorship comes from
audit events the API server posts to the ingress, read through the operator's own path. Impersonation
does not apply there and should not be made to. Worth stating explicitly in the design, because a
reader will assume otherwise.

**`CanImpersonate` is a weak precheck.** Flux checks only that the SA exists. Copying that gives a
`Ready=True` on a rule that will `403` at stream start. A `SelfSubjectAccessReview` or
`SubjectAccessReview` per rule item at reconcile time is the stronger version, and this project has
already built fail-closed SAR machinery for `ClusterProvider`. Reuse it.

### Multiple service accounts

Your instinct is right, and the useful axis is not the obvious one.

**Axis A, per rule item.** `rules[].serviceAccountName`, so one `WatchRule` can read `media` under
one identity and `monitoring` under another. Defensible, and it matches the existing shape of
`rules[].sourceNamespace`. But the granularity is already available for free: `WatchRule` is
namespaced and cheap, two of them may target the same `GitTarget`, and splitting by identity gives
a clearer object than a rule whose items have different authority. **Verdict: no.** Start with one
SA per rule object.

**Axis B, per concern.** This is the one worth building, and it is the one Flux converged on.
Reading a `ConfigMap` and reading a `Secret` are different privileges by orders of magnitude, and
this project has a specific reason to care: `rbac.watchTypes.mode: any` grants the operator read on
**every Secret in the cluster**, which the security model names as the main consequence of that
setting.

```yaml
spec:
  serviceAccountName: homelab-mirror              # ordinary types
  sensitiveServiceAccountName: homelab-secrets    # types the resource capability model marks sensitive
```

The payoff is that the broad convenience of `mode: any` stops implying broad Secret access. A
platform admin can leave type-selection permissive and still require a separate, separately audited
identity for the types that matter. That maps one-to-one onto Flux's
`--default-decryption-service-account`, which exists for the same reason.

The seam is already there: the resource capability model and the sensitive-resource handling
already classify types, so "which SA does this read use" is a lookup against a classification that
exists rather than a new one.

**Axis C, per plane.** Separate identities for source reads, Git writes, and audit ingest. Git
writes are not a Kubernetes identity at all, and audit ingest is push-based. **Verdict: no**, and
worth saying so in the design so it is not re-asked.

### Recommendation

Phased, and only the first phase is in the current wave.

**Phase 1, in the breaking wave.** Rename only. `allowSourceNamespaceOverride` becomes
`crossNamespaceSources: Deny | Allow`, and `ClusterProvider.spec.allowedNamespaces` becomes
`accessFrom`. Both ride the loud-rejection pattern the project already uses for superseded fields.
No behavior change, and the enum is shaped to grow a third value. Doing the rename inside a wave
that is already breaking costs one migration note; doing it later costs a whole release.

**Phase 2, its own additive release.** `WatchRule.spec.serviceAccountName` and
`ClusterWatchRule.spec.serviceAccountName`, impersonating `system:serviceaccount:<rule-namespace>:<name>`,
with a `SubjectAccessReview` precheck per rule item and per-item conditions on failure. Strictly
narrowing, so nothing breaks. Ship the `TooManyStreams` cap first or alongside, since stream
cardinality is the cost.

**Phase 3, when a tenant asks.** `crossNamespaceSources: RequireServiceAccount`, plus an operator
flag `--default-source-service-account` mirroring Flux's, plus the multi-tenant lockdown profile
documented as a recipe. Then `sensitiveServiceAccountName` from axis B, which is the piece with the
most value per line and the least precedent, so it benefits most from having the machinery already
proven.

**What not to do.** Do not make impersonation mandatory, and do not let it replace
`GitTarget.spec.allowedSourceNamespaces`. Impersonation answers "may this identity read that
object". It cannot answer "may that object be written into this Git folder", because Git folders are
not Kubernetes resources and no RBAC rule can name one. The destination policy stays exactly where
it is, and the design should say why in one sentence, because the natural reaction to adopting
impersonation is to assume it subsumes everything.

## What holds up

Recording this deliberately, because a findings list is a distorted view of a body of work.

The `Auto` resolves-once-and-pins argument is the strongest thing in the layout model, and it is
correctly load-bearing. Declared inference with a name in the spec and the resolution in status is
a different object from the sibling inference that was deleted, and the document makes that case
without hand-waving.

Immutability with a widening exception, justified by "`GitTarget` has no finalizer, so recreating
one re-adopts every document by identity", is the kind of argument that closes a review. It prices
the constraint in the currency that matters (data loss, not convenience) and reaches a different
answer from `spec.prune` for a stated reason.

Rule 1, reachability as an invariant rather than a rung, is the right primitive, and "what the
model makes unstatable" is the section that will persuade a skeptical reader. Lead with it.

Dropping `kind: Template` from the wave is correct, and the honest accounting of what that does and
does not remove (the template engine stays for `byType`) is how a scope cut should be written down.

The `LayoutProfile` section reaches the right answer for the right reason, and the trigger for
revisiting is recorded, which makes it a decision rather than an omission.

## Suggested order

**Before PR 1 merges** (the examples are the deliverable there, so these are cheap and in scope):
L1, L12, L13, L14, L21, L23. Add L2 to at least the two GitOps-tool scenarios, since the corpus
harness is what gives those fixtures teeth.

**Before PR 3 is written**, because it decides a status field a release early: L17, L18, L19.

**Before PR 4 is planned**, because they are API surface: L3, L4, L5, L6, L8, L9, and L15/L27 as
one post-scan validation class.

**In the breaking wave, as a rename only**: L7, and `accessFrom`.

**Its own additive release**: `serviceAccountName`, phase 2 above.

**Whenever convenient**: L10, L11, L16, L20, L22, L24, L25, L26.
