# The template was the right primitive. Two things were missing

> **design**: a proposal, not a plan of record. Nothing here binds until scheduled.
> Index: [`../INDEX.md`](../INDEX.md)
> Date: 2026-08-29. Supersedes this document's own earlier thesis, which argued that a path template
> is the wrong primitive and should be replaced by a `spec.layout` discriminated union. That argument
> no longer holds; why it stopped holding is the first section, because a reversal is worth more than
> a quiet edit.
>
> Concrete repository folders and matching configurations live in
> [`specific-examples/README.md`](specific-examples/README.md), for the Argo CD and Flux cases, and in
> [`shapes/README.md`](shapes/README.md), which is the cross-product of folder shapes with the
> decision flow drawn out and an empty-folder column for every one of them.

Placement today is a ladder of four rungs, three of which are path templates and one of which is not:

```text
byType -> default -> the folder's one kustomize root -> canonical
```

The proposal is to **keep that**, and add the two things a path genuinely cannot express: whether
this folder maintains a `kustomization.yaml`, and whether `metadata.namespace` is written into the
document.

```yaml
spec:
  path: apps/demo
  serializeNamespace: false                         # optional bool, unset = infer
  placement:
    byType:                                         # unchanged, as shipped
      v1/secrets: "secrets/{name}{sensitiveSuffix}"
    default: "{namespace}/{resource}/{name}.yaml"   # unchanged, as shipped
    useKustomize: true                              # bool, default false
```

Two booleans. No discriminator, no `spec.layout`, no `kind`, no `scope`, no new CRD, and no enum.
Both are additive: an object that says nothing behaves exactly as it does today.

**They sit at different levels, and the line between them is retroactivity.** `spec.placement`
decides where a **new** document goes; everything already written is match-first and never moves,
which is the guarantee that makes a template safe to change. `useKustomize` keeps that property — it
decides whether a new file's directory has a root to join, and creates one if not — so it belongs
inside `placement`.

`serializeNamespace` does not. It governs the bytes of **every** write, not just the first one, and
the code already has two separate paths for it: [`plan_flush.go`](../../internal/git/plan_flush.go)
strips `metadata.namespace` from a new document using the placement result, and strips it from an
**update** using the document's own observed namespace source. It also decides how a managed
document is *found* — a document whose namespace is inherited is located in the file bytes by a
namespace-less identity. A field with those effects nested inside a struct documented as "new files
only" is a trap: changing it rewrites existing documents as they are next touched. So it is
`spec.serializeNamespace`, one level up, where its blast radius is legible.

## What changed

The earlier thesis rested on five arguments. Three were retired by
[#319](https://github.com/ConfigButler/gitops-reverser/pull/319), which shipped the ancestor walk: a
new file is registered with the nearest kustomization that governs it, whatever chose its path.

| The argument against templates | Still true? |
|---|---|
| `byType` into a subdirectory produces a file no kustomization lists | **No.** That was [#295](https://github.com/ConfigButler/gitops-reverser/issues/295), and it is fixed |
| `placement.default` does the same to every type at once | **No.** Same bug, same fix |
| A CRD default cannot be added: a non-empty template shadows the kustomize-root rung | **Weakened**, and [`placement-visibility-and-declared-defaults.md`](../design/placement-visibility-and-declared-defaults.md) already concedes the fix turns this from a correctness wall into a legibility trade |
| "Where do my files go" needs a metric and a status field to be legible | **True** — but that is an argument for status, not against templates |
| Nothing can **create** structure, so an empty repository cannot be bootstrapped | **True**, and untouched by either design. Creating a `kustomization.yaml` is not a path question |

The sentence the whole redesign rested on was *"a path template cannot express 'beside this folder's
one kustomization'"*. That is still true and no longer matters, because the template is no longer
asked to express it. Registration became an **invariant** rather than a rung — which was always the
best idea in the layout model, and it is the part that already shipped.

What is left is one real gap (bootstrap), one status gap, and one flag that was always missing.

## What kustomize actually requires

Four facts, measured against kustomize v5.8.1 rather than recalled, because three of them contradict
assumptions the earlier model was built on.

1. **A root does not require a flat folder.** A `kustomization.yaml` listing `configmaps/cache.yaml`
   and `apps/deployments/web.yaml` builds, and the root's `namespace:` transformer applies to both.
   So "`Kustomize` means flat files beside one root" was our rule, never kustomize's.
2. **Nested roots work, one per subfolder.** A parent listing `media` and `monitoring`, each holding
   its own `kustomization.yaml` with its own `namespace:`, renders each document into its own
   namespace. A multi-namespace folder can therefore omit `metadata.namespace` safely — if something
   owns those child roots.
3. **There is no ambient pickup.** `resources: [cms/*.yaml]` fails (`evalsymlink failure`), and a bare
   directory fails unless it contains its own kustomization. Every file is named explicitly or lives
   under a nested root.
4. **An unlisted file in a listed subdirectory renders nothing.** The #295 class, unchanged.

Fact 3 is why registration must be an invariant: in a kustomize folder there is no other way for a
new file to be applied. Fact 1 is why the path may be anything the user wants. Together they say the
two questions are independent, which is exactly what a single `kind` discriminator could not express.

## `useKustomize`

The flag that says this folder is a kustomize folder and the operator maintains its root.

| | A kustomization governs the path | Nothing governs the path |
|---|---|---|
| unset / `false` (default) | register the new file in its `resources:` | write the file, touch nothing |
| `true` | register | **create `kustomization.yaml` at `spec.path`**, then register |

**Registering into a root that is already there is not what this flag controls.** It happens in both
columns, because a file a kustomization does not list is a file nothing renders — that was #295, and
[#319](https://github.com/ConfigButler/gitops-reverser/pull/319) made it an invariant. The flag has
exactly one job: what to do when there is no root.

That is also the honest reading of the name, which says less than the field does: `useKustomize:
false` does not mean "leave kustomize alone". If you do not want a folder's root touched at all, do
not point a `GitTarget` at that folder — the ancestor walk is bounded by the write jail, so a
kustomization **above** `spec.path` is never edited, and rooting the target lower is the existing,
better-tested way to say it.

Creating the root is the only genuinely new machinery in this proposal, and it is what makes an empty
repository bootstrappable — the last surviving argument from the earlier thesis, now one boolean
rather than a reason to redesign the primitive. A created root carries `resources:`, and
`namespace:` when the folder is single-namespace, which is what makes it a **meaningful**
kustomization rather than an empty file. That pairing is the whole point of the second flag.

## `serializeNamespace`

Whether the committed document carries its own `metadata.namespace`. A path decides where the file
sits; it cannot decide what is inside it, and kustomize takes the namespace from exactly one of two
places — the document, or a governing kustomization's `namespace:`.

| Value | Meaning |
|---|---|
| unset (default) | infer, which is today's behavior |
| `true` | always write it |
| `false` | never write it |

**It is an optional boolean, and unset is not the same as `false`.** No plain default preserves
today's behavior: defaulting to `false` breaks a flat folder, whose documents must carry their own
namespace or they are ambiguous, and defaulting to `true` writes a redundant line into every
kustomize folder that already supplies one. So the field is a `*bool` and nil means infer — the
ordinary Kubernetes shape for a three-state switch that has to keep an existing default.

**What "infer" already does, and why it is worth keeping as the default.** The inference is not a
guess. [`placement.go`](../../internal/manifestanalyzer/placement.go) omits `metadata.namespace`
only when the governing kustomization sets a `namespace:` **and sets it to this resource's own
namespace**; in every other case it writes the namespace explicitly, because omitting it there would
hand the document to a different namespace and the mirror would claim to hold an object it does not.
An explicit setting is therefore an override of a correctness rule, which is why `false` needs a
guard and unset does not.

The two settings exist for the two shapes a user actually declares:

- **`true` for a flat folder.** Nothing downstream supplies a namespace, so every namespaced document
  has to carry one. It also keeps a document portable: it means the same thing pasted anywhere.
- **`false` beside a root that supplies it.** With `useKustomize: true` the operator owns that root
  and writes `namespace:` into it, so the omission is **provable** rather than trusted. That is the
  difference between establishing a convention and guessing one, and it is what inference
  structurally cannot do on an empty folder — there is nothing there to infer from.

The name deliberately avoids `writeNamespace`. "Write" is the most loaded word in this API — the
write boundary, the write jail, `WriteBoundaryRefused` — so `writeNamespace: false` invites the
reading *"never write to this namespace"*, a permission, which is precisely what the neighbouring
`sourceNamespace` fields are. `serializeNamespace` names the moment the decision is made, and cannot
be read as policy.

**It governs namespaced resources only.** A `ClusterRole` has no namespace, so the field is ignored
for cluster-scoped documents rather than being an error — worth stating in the field documentation,
because a tree folder is the type most likely to carry both.

### Is a folder-wide claim reasonable?

Setting the flag says something about *every* namespaced document in the folder, which sounds like a
strong claim until you notice it is the claim the ecosystem's two commonest folder shapes already
make.

- **"No document carries its namespace" is the portable-artifact convention.** It is what a kustomize
  base is: written namespace-free, with an overlay's `namespace:` transformer stamping one at build
  time. Flux says the same thing from outside the repository with `Kustomization.spec.targetNamespace`,
  Argo CD with `Application.spec.destination.namespace`, and a Helm chart by templating
  `.Release.Namespace` rather than hard-coding a namespace. A folder that follows it can be deployed
  into any namespace; a folder that half-follows it cannot, and nothing warns you.
- **"Every document carries its namespace" is the convention for a folder applied directly.** A flat
  mirror handed to `kubectl apply -f`, or a cluster-state tree, has nothing downstream to supply one,
  so a document without a namespace is ambiguous rather than portable.

Two things keep the claim from being coarser than reality. **Cluster-scoped resources are exempt**,
so a folder holding both a `ClusterRole` and a `Deployment` is ordinary rather than mixed. And a
folder is genuinely allowed to be non-uniform when it is a **tree of nested roots** — fact 2 above —
each subtree taking its namespace from its own `kustomization.yaml`. That case is exactly what the
default handles: inference runs per document against the root that governs *that* path, so a tree
resolves each subtree correctly without anyone declaring anything.

So the uniform claim is what an explicit setting is *for*, and the non-uniform folder is what unset
is for. That is also why unset cannot be spelled `false`.

### The guard on `false`, and why it is a post-scan check

`false` where nothing supplies the namespace hands the object to whatever namespace the applier
happens to be pointed at, which is a different object with the same name. It is honest only when
something guarantees the namespace, and where the guarantee is a `kustomization.yaml` **the user
owns**, they can delete one line from their own file and every subsequent document silently
relocates.

That precondition is a property of the observed folder, not of the spec, so no CEL rule can check it.
It is one post-scan rule, on the scan that already runs, setting `Validated=False` with a message
naming the field and what the folder actually contains. With `useKustomize: true` the rule is
satisfied by construction, because the operator wrote the supplier.

**Nothing here is the last line of defence, which is why the guard can be a report rather than a
refusal.** The render check at the write path already refuses a write whose document does not render
to the live object, and it holds for both shapes where the store's view and kustomize's disagree: a
document naming a namespace the governing transformer overrides, and a folder whose nested roots both
assign one. Both are measured, not assumed, and both are pinned by
[`namespace_context_refusal_test.go`](../../internal/git/namespace_context_refusal_test.go) with the
read-side halves in the contextual-namespace corpus. What the post-scan rule adds is not safety but
legibility: today those failures surface as an opaque render error rather than as the one fixable
thing that is wrong.

### The second guard: one source namespace, and this one refuses

**A `GitTarget` with an explicit `serializeNamespace: false` admits exactly one source namespace.
The second is refused.**

The guard above asks whether a supplier exists. This one asks how many namespaces that supplier is
being asked to speak for, and the answer can only ever be one: a document with no
`metadata.namespace` takes its namespace from a single supplier, so two source namespaces reaching
the folder is a contradiction in the setting itself. What follows is not a collision but a **match** —
`shop/config` and `billing/config` both resolve to a `config.yaml` whose bytes carry no namespace, so
their manifest identities are equal, the bundling rule never fires, and each write flips one document
between two live objects. Everywhere else in this model, losing a distinction produces a refusal or a
bundle; only here does it produce a match, which is why this guard refuses where the other reports.

It is **derived from one setting, not inferred from two.** An earlier draft proposed a separate
`enforceSingleNamespace` boolean, on the objection that a refusal derived from other fields is the
kind of inference that got deleted from placement. That objection applied to a rule keyed on
`serializeNamespace: false` *plus* a template with no `{namespace}`. Drop the template half — the
path is irrelevant, because a deployer applies bytes rather than filenames — and what is left is the
field's own meaning rather than a correlation between two fields. So there is no new field.

**Explicit `false` only. Inference is never constrained by it.** That asymmetry is what makes the
refusal safe next to kustomize, and it is the same line [`Is a folder-wide claim
reasonable?`](#is-a-folder-wide-claim-reasonable) already draws. A tree of nested roots (fact 2) is
legitimately multi-namespace *and* namespace-free in its documents, and it is the case unset exists
for: inference resolves each document against the root governing its own path. Such a folder must
never declare `false`, because `false` is a folder-wide uniform claim and that folder is not uniform.
The refusal therefore costs that user nothing — it pushes them onto the setting that was already
correct for them.

**With `useKustomize: true` the multi-namespace case is worse, not better.** A created root carries
`namespace:` only when the folder is single-namespace; with two namespaces reaching the target the
operator can write no `namespace:` at all, so it creates a root that supplies nothing and then places
namespace-less documents beneath it — the operator actively constructing the silent-mislabel folder.
This is the one place refusing is not merely permissible but the only defensible behavior.

**It needs no scan and no repository state**, which is what separates it from the guard above. The
set of source namespaces reaching a target is `{the target's own namespace} ∪ {the explicit
rules[].sourceNamespace names of every WatchRule pointing at it}`, all of it in the config cluster.
A `sourceNamespace: "*"` item is refused outright and statically, with no enumeration, and that
holds under **either** reading of `*` — the shipped one or the one the wave replaces it with
([definition of record](../design/source-scope-simplification.md#sourcenamespace--needs-its-own-decision)).
Neither can be proven to be one namespace from the spec alone, which is all this rule needs.

This is deliberately **not** the job `GitTarget.spec.allowedSourceNamespaces` does, and it does not
depend on that field, which the same document deletes. That field is an authorization fence, deleted
because the chain from a folder back to the object that fills it never leaves one namespace, so RBAC
on `watchrules` already answers who may write. This rule asks whether the folder still means what it
claims — a correctness question about the bytes, untouched by that argument and homeless after it.

**`fails` needs a subject, and it is both.** Refusing only the write leaves a target that refuses
forever with no obvious fix; refusing the second `WatchRule` at admission is atomic feedback at the
moment the mistake is made, and is what the fail-open webhook is for. So: an admission check for the
feedback, and a write-plan precondition as the correctness layer, since admission is one-shot and
cannot see a `serializeNamespace` flipped to `false` after the rules were created — the same
reasoning [`../spec/where-validation-lives.md`](../spec/where-validation-lives.md) applies to every
other gate in this repository.

## Collisions are already decided

What happens when two resources resolve to the same path is specified and shipped in
[`new-file-placement-rules.md`](new-file-placement-rules.md): a unique path is a new file, a colliding
path **appends** into a plaintext multi-document file, a sensitive resource whose path already holds a
document is **refused** rather than appended, encrypted files are never appended into in either
direction, and existing documents stay match-first, so an object living inside a bundle is updated
where it is rather than moved out of it.

So "one file per object" and "a bundle per type or per namespace" are both expressible today, by
writing a template that distinguishes identities or one that deliberately does not.

## The four questions a user actually asks

| Question | Answered by |
|---|---|
| Is this a kustomize folder? | `useKustomize` |
| Do my documents carry `metadata.namespace`? | `serializeNamespace` |
| Which folder do new files go in? | the directory part of the template. Fact 1: this is not constrained to flat |
| Is this folder one namespace or many? | whether `{namespace}` appears in the template. A template without it is single-namespace by construction |

No field in that table exists to answer a question a user did not ask, which is the test the earlier
`kind`/`scope` pair failed: `scope: SingleNamespace` restated in an enum what the template already
said, and then required an admission rule to keep the two in agreement.

## What this deletes

`spec.layout` and its discriminator, `kind`, `type`, the `Auto`/`Kustomize`/`Tree`/`Flat`/`Template`
values, `layout.scope` and the admission rule keeping it in agreement with `allowedSourceNamespaces`,
`kustomize.create`, and with them four findings of the maintainer review (L3, L4, L5, L8) — not
renamed, gone. The `LayoutProfile` question goes too: without a `layout` block the only thing left to
share is the `byType` map, and whatever generates thirty GitTargets repeats two booleans for free.

**And the migration.** `spec.placement` keeps its meaning and gains two optional members, so there is
no loud rejection, no `feat(api)!` on this axis, and no coordinated consumer bump for the layout work.
The layout model was the largest breaking change in the queue; on this shape it is not a breaking
change at all.

## What it leaves standing

- **`spec.placement` is mutable and stays mutable.** Existing files never move, so a template change
  affects only files written afterwards, and a folder can hold documents placed under two templates.
  Match-first identity keeps finding and updating them in place. The immutability-plus-CEL-widening
  machinery an earlier draft proposed was invented to protect a discriminator that no longer exists.
- **`placements_total` keeps its `source` label** — today `declared`, `kustomize_root` and
  `canonical`, with the `declared` split into `byType`/`default` still queued. It names the rung
  that answered rather than a resolved layout kind, so nothing here breaks a label.
- **`{kindLower}` and the versionless identity fix** are template features and stay queued.

## `status.placement`, and the post-scan pass

The legibility gap is the one surviving argument from the earlier thesis that no field answers, and
it is worth building **before** either flag: placement only ever affects *new* documents, so there is
nothing to preview by inspection, and a suspended target plus this stanza is what turns adoption from
declare-and-hope into a dry run.

```yaml
status:
  observedGeneration: 4
  conditions:
    - type: LayoutResolved
      status: "True"
      reason: SingleKustomization        # SingleKustomization | Ambiguous | None
      message: "render root '.' governs new files"
      observedGeneration: 4
  placement:
    renderRoot: .
    serializeNamespace: false            # what it resolved to for this folder
    byTypeEntries: 1
    observedRevision: 9f3c1ab
    observedTime: "2026-07-30T09:14:22Z"
    examples: []                         # capped at three, illustrative, not a tally
```

Three decisions are taken here rather than deferred, because each is cheaper to take before the field
exists than after:

- **The resolution reason is a condition reason, not a field.** `renderRootReason` would have been a
  reason enum in a bespoke field, and every consumer in this ecosystem already reads reasons from
  `conditions`.
- **No accumulating counters.** `placedResources`, `overriddenTypes` and `refusedResources` are
  metrics; `placements_total` carries them with better labels. A monotonic counter in status is a
  status write per event, which re-creates the self-triggering reconcile edge the status work already
  fixed once. `examples` stays, capped and fixed-size, because "show me where a Secret would land" is
  not a metric.
- **`conditions` and `observedGeneration` are in the stanza**, because every scenario README already
  asserts `Ready=True`.

The current half must never depend on a placement having happened: `renderRoot` is a fact about the
folder from the last scan, available before anything is ever written.

**The post-scan validation pass ships with it**, because it is the same scan. Two rules today, whose
precondition is a property of the observed folder rather than of the spec, so no CEL rule can reach
them: `serializeNamespace: false` requires a namespace supplier, and a folder covering two roots is
`Ambiguous` rather than silently picking one. One pass, one condition shape, `Validated=False` naming
the offending field and what the folder actually contains.

The [one-source-namespace rule](#the-second-guard-one-source-namespace-and-this-one-refuses) is
**not** part of this pass, though it guards the same field. Its input is the set of `WatchRule`
objects naming the target, which is in the config cluster and needs no scan, and its outcome is a
refusal rather than a report. It ships with the field, in PR 2.

## How it gets built

`LocateNew` is not rewritten. The four-rung ladder is a single function,
[`LocateNew`](../../internal/manifestanalyzer/placement.go), with a single caller in
[`plan_flush.go`](../../internal/git/plan_flush.go), and everything downstream of the path decision —
registration, the render fidelity gate, refusal accounting, the metrics — already exists and stays
where it is. The two flags sit beside the ladder.

These two PRs are **track A** of [`../design/build-order.md`](../design/build-order.md), which is
the only page that carries the cross-track order and the authority on the cut. Two other tracks are
in flight and neither is covered here: the breaking source-scope wave (PR 3 there), and patch
authoring. Nothing below waits for either — see
[the couplings that do not exist](../design/build-order.md#three-couplings-people-expect-and-that-do-not-exist).

| PR | Content | Breaking |
|---|---|---|
| 1 | The worked examples as an executable corpus, `spec.suspend` and the reconcile-request annotation, `status.placement`, and the post-scan pass's `Ambiguous` rule | no |
| 2 | `useKustomize` and `serializeNamespace`, with the post-scan pass's supplier rule and the one-source-namespace refusal ([#322](https://github.com/ConfigButler/gitops-reverser/issues/322)) | no |

PR 1's four parts are one review, because none of them changes what the operator writes: a suspended
target that publishes what it resolved is the whole feature, and the corpus is what proves it. The
post-scan pass is the one thing that does **not** land whole — its `Ambiguous` rule reads only the
scan, while its supplier rule reads a field PR 2 introduces.

Neither PR is breaking, so neither waits for a coordinated consumer bump. What is breaking on
`GitTarget` is unrelated to placement and is sequenced in
[`gittarget-api-wave.md`](../design/gittarget-api-wave.md).

**PR 1 is the corpus, and it is the reason the rest is reviewable.**
[`shapes/README.md`](shapes/README.md) already has the shape of a golden-file suite —
`repository/`, `config/`, `input/`, `expected-*.patch` — and is read by nobody but a human. Wiring it
up converts the PR 2 review from "does this prose hold together" into "does the diff match the
patch". The seam exists: `newWorktreeForTest` and `flushEventsToWorktree` in
[`internal/git`](../../internal/git/placement_test.go) already do this at a smaller scale. Per
scenario: seed a worktree from `repository/`, build the write event from `input/`, derive the flush
policy from `config/gittarget.yaml`, flush, and compare the normalized diff with `expected-*.patch`.
Blob hashes and index lines are noise; a `-update` flag that rewrites the patches keeps the corpus
cheap to extend. Scenarios describing behavior PR 2 introduces are written now and skipped with the
PR that unskips them named in the skip message, so **PR 2 is finished when the last skip is gone.**
`config/gittarget.yaml` uses fields that do not exist yet, so it parses into a harness-local struct
until PR 2 deletes that mapping — which is itself a check that the API the examples describe is the
API that got built.

**PR 2 builds the `true` half of each flag.** Everything else is already there: registration into an
existing root shipped in #319, and inference is what
[`namespaceIsInheritedFromContext`](../../internal/manifestanalyzer/placement.go) already does. What
is new is writing a `kustomization.yaml` that does not exist, with `namespace:` set when the folder
is single-namespace, and registering into it in the same commit. Build that last and on its own: it
is the one thing that writes a file nobody asked for by name.

**The one-source-namespace refusal ships in PR 2 too, and it is the cheapest thing in it**: no scan,
no repository state, no new field. Build the write-plan precondition first, because it is the
correctness layer and it holds whatever admission did; the admission check on `WatchRule` is the
feedback half and can follow in the same PR. It also decides the `useKustomize` question above — a
created root's `namespace:` is written when the folder is single-namespace, and under this rule an
explicit `serializeNamespace: false` guarantees it is.

The refusal scenarios are part of PR 1 even where the rule is not: every worked example used to be
a happy path, and the post-scan pass had the least coverage of anything here. Four exist now, each
asserting an `expected-*-status.yaml` instead of a patch — a `serializeNamespace: false` with no
supplier, a `serializeNamespace: false` target a second source namespace reaches, a folder covering
two roots, and the base-owned field edit. Only the two-roots one asserts a rule PR 1 ships; the
other two are written and skipped naming PR 2, and the supplier one additionally needs a third
assertion mode in the harness, because it reports rather than refuses.

## Open questions

- Does `serializeNamespace: false` need to **name** its supplier (`KustomizeRoot`,
  `FluxTargetNamespace`, `Asserted`) so the post-scan pass can check the guarantee rather than infer
  which one was meant?
- Should a `useKustomize: true` folder create a **nested** root per directory the template writes
  into, each carrying its own `namespace:`? Fact 2 proves it works. Deferred, and now more firmly:
  it was the thing that would have made `serializeNamespace: false` safe in a multi-namespace tree,
  and the [one-source-namespace rule](#the-second-guard-one-source-namespace-and-this-one-refuses)
  removes that motivation by refusing the combination outright. Such a tree is an unset folder, which
  inference already handles per document. Re-open trigger: someone who needs a multi-namespace folder
  whose documents omit their namespaces, and for whom leaving the field unset is not enough.
- Should the operator ever **refuse** a write when a root that used to govern the path is gone,
  rather than reporting `LayoutResolved: None` and carrying on? Report first; escalate if someone
  says the status was not enough.
- Should `placement.default` gain a CRD default now that a defaulted template no longer produces
  unrendered files? The remaining objection is legibility, not correctness.
- Namespace-local `GitProvider`: the homelab examples put a `GitTarget` in `argocd`, `flux-system`
  and `homelab-config`, each needing its own `GitProvider` — three copies of one credential in a
  single-owner cluster. Recorded as a gap, not a blocker.
