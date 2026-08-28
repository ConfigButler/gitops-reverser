# The template was the right primitive. Two things were missing

> **design**: a proposal, not a plan of record. Nothing here binds until scheduled.
> Index: [`../INDEX.md`](../INDEX.md)
> Date: 2026-08-28. Supersedes this document's own earlier thesis, which argued that a path
> template is the wrong primitive and should be replaced by a `spec.layout` discriminated union.
> That argument no longer holds; [why it stopped holding](#what-changed) is the first section,
> because a reversal is worth more than a quiet edit.
>
> Concrete repository folders and matching configurations live in
> [`examples/README.md`](examples/README.md).

Placement today is a ladder of four rungs, three of which are path templates and one of which is not:

```text
byType -> default -> the folder's one kustomize root -> canonical
```

The proposal here is to **keep that**, and add the two things a path genuinely cannot express: whether
`metadata.namespace` is written into the document, and what to do about the `kustomization.yaml` that
decides whether anyone ever renders it.

```yaml
spec:
  path: apps/demo
  placement:                                        # unchanged, as shipped
    byType:
      v1/secrets: "secrets/{name}{sensitiveSuffix}"
    default: "{namespace}/{resource}/{name}.yaml"
  serializeNamespace: Auto                          # Auto | Always | Never
  kustomizeRoot: Adopt                              # Adopt | Create | Require
```

Two new fields. No discriminator, no `spec.layout`, no `kind`, no `scope`, no new CRD.

## What changed

The earlier thesis rested on five arguments. Three of them were retired by
[#319](https://github.com/ConfigButler/gitops-reverser/pull/319), which shipped the ancestor walk:
a new file is registered with the nearest kustomization that governs it, whatever chose its path.

| The argument against templates | Still true? |
|---|---|
| `byType` into a subdirectory produces a file no kustomization lists | **No.** That was [#295](https://github.com/ConfigButler/gitops-reverser/issues/295), and it is fixed |
| `placement.default` does the same to every type at once | **No.** Same bug, same fix |
| A CRD default cannot be added: a non-empty template shadows the kustomize-root rung | **Weakened**, and [`placement-visibility-and-declared-defaults.md`](placement-visibility-and-declared-defaults.md) already concedes the fix turns this from a correctness wall into a legibility trade |
| "Where do my files go" needs a metric and a status field to be legible | **True** — but that is an argument for status, not against templates |
| Nothing can **create** structure, so an empty repository cannot be bootstrapped | **True**, and untouched by either design. Creating a `kustomization.yaml` is not a path question |

The sentence the whole redesign rested on was *"a path template cannot express 'beside this folder's
one kustomization'"*. That is still true and no longer matters, because the template is no longer
asked to express it. Registration became an **invariant** rather than a rung — which was always the
best idea in the layout model, and it is the part that already shipped.

What is left is one real gap (bootstrap), one status gap, and one field that was always missing.

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
   directory fails unless it contains its own kustomization (`must resolve to a file: unable to find
   one of 'kustomization.yaml'`). Every file is named explicitly or lives under a nested root.
4. **An unlisted file in a listed subdirectory renders nothing.** The #295 class, unchanged.

Fact 3 is why registration must be an invariant: in a kustomize folder there is no other way for a
new file to be applied. Fact 1 is why the path may be anything the user wants. Together they say the
two questions are independent, which is exactly what a single `kind` discriminator could not express.

## `serializeNamespace`

The one thing a path cannot say. A path decides where the file sits; it cannot decide whether the
document inside carries its own `metadata.namespace`, and kustomize needs that from one of exactly
two places — the document, or a governing kustomization's `namespace:`.

| Value | Meaning |
|---|---|
| `Auto` (default) | omit `metadata.namespace` when the governing kustomization already sets this resource's namespace. Today's inferred behavior, now named |
| `Always` | always write it. The only safe choice when nothing downstream supplies it |
| `Never` | never write it, and prove something else does |

The name deliberately avoids `writeNamespace`. "Write" is the most loaded word in this API —
the write boundary, the write jail, `WriteBoundaryRefused` — so `writeNamespace:
Never` invites the reading *"never write to this namespace"*, a permission, which is precisely what
the neighbouring `sourceNamespace` fields are. `serializeNamespace` names the moment the decision is
made (when the document is produced) and cannot be read as policy.

### The guard on `Never`, and why it is a post-scan check

`Never` hands the object to whatever namespace the applier happens to be pointed at, which is a
different object with the same name. So it is valid only when something guarantees the namespace.
Today three of the six worked examples set it against a `kustomization.yaml` **the user owns**: they
delete one line from their own file and every subsequent document silently relocates.

That precondition is a property of the observed folder, not of the spec, so no CEL rule can check it.
It joins the same post-scan validation class as the ambiguous-root case: one pass that sets
`Validated=False` naming the offending field and what the folder actually contains.

| Rule | Precondition | Checkable at admission |
|---|---|---|
| `Never` requires a namespace supplier | a kustomization with `namespace:` governs the path | never |
| `kustomizeRoot: Require` needs a root | the folder has one | never |
| a declared single-root assertion | the folder has exactly one root | never |

Three rules, one pass, one condition shape. Each gets a corpus scenario with an
`expected-status.yaml` instead of a patch.

**`serializeNamespace` governs namespaced resources only.** A `ClusterRole` has no namespace, so the
field is ignored for cluster-scoped documents rather than being an error — worth stating in the field
documentation, because a tree folder is the type most likely to carry both.

## `kustomizeRoot`

The field that answers "do we want kustomize". It is one question with one axis, and the axis is
**what to do when no kustomization governs the path** — because when one does govern, all values
register, which is the invariant.

| Value | A kustomization governs the path | No kustomization governs the path |
|---|---|---|
| `Adopt` (default) | register the file in its `resources:` | write the file, touch nothing |
| `Create` | register | create `kustomization.yaml` at `spec.path`, then register |
| `Require` | register | **refuse the write** |

`Adopt` is today's behavior after #319, so a user who says nothing gets what they already have.

`Create` is the empty-repository bootstrap — the last surviving argument from the earlier thesis, now
one value of one field rather than a reason to redesign the primitive. It closes its own loop with
`serializeNamespace`: a root the operator creates can carry `namespace:`, so `Never` becomes
provable rather than trusted. That is the difference between establishing a convention and guessing
one, and it is the thing inference structurally cannot do on an empty folder.

`Require` is the safety value, and it is the one that earns its place from the `Never` guard above.
It says: this folder is a kustomize folder; if the root disappears, stop writing rather than commit
files nothing renders. Without it, the only responses to a deleted root are to carry on silently or
to hard-code a refusal nobody asked for.

### Values considered and not taken

**`Ignore` — never touch a kustomization, even when one governs the path.** Rejected. The scenario
for it is a tree with two consumers, where the root belongs to somebody else's build and our
documents feed a different one. But `spec.path` already expresses that: the ancestor walk is bounded
by the write jail, so a kustomization **above** `spec.path` is never touched, and the fix for "do not
edit that root" is to root the target at the folder you own. A value whose entire job is duplicated
by an existing field is a value that will be set by mistake more often than on purpose. It is also
the only candidate that does not fit the axis above — it changes what happens when a root **is**
present, which is the half that should be invariant. Revisit if someone produces a folder they must
target, containing a kustomization they must not edit, that cannot be split.

**`CreatePerDirectory` — a nested root in every directory the template writes into, each wired into
its parent, each carrying its own `namespace:`.** Not rejected; deferred, and recorded here because
fact 2 above proves it works. It is what would make `serializeNamespace: Never` safe in a
multi-namespace tree, since the operator would own every root the omission depends on. It is also
materially more machinery than the other three values, and it should wait until someone wants a
multi-namespace folder without namespaces in its documents. The trigger is written down so this is a
decision rather than an omission.

### The name

`kustomization: Adopt` was the first spelling and it collides with a real Kubernetes kind — Flux's
`Kustomization` — in a field whose values look like a mode. That is the same complaint the maintainer
review makes about `layout.kind`, applied to this proposal, so it should not survive it.
`kustomizeRoot` uses the vocabulary this project already has (`renderRoot`, "the folder's one
kustomize root") and names a folder concept rather than an object.

## Collisions are already decided

The remaining question a template raises — what happens when two resources resolve to the same path —
is not open. It is specified and shipped in
[`new-file-placement-rules.md`](new-file-placement-rules.md): a unique path is a new file, and a
colliding path **appends** into a plaintext multi-document file. The hard cases come with it, and
they are the reason this is worth citing rather than reinventing:

- a sensitive resource whose path already holds a document is **refused**, never appended;
- no append into an encrypted file, in either direction;
- no cold-bundle mixing when several new resources land on one path in a single flush;
- existing documents stay match-first, so an object already living inside a bundle is updated where
  it is rather than moved out of it.

So "one file per object" and "a bundle per type or per namespace" are both expressible today, by
writing a template that distinguishes identities or one that deliberately does not.

## The four questions a user actually asks

| Question | Answered by |
|---|---|
| Do my documents carry `metadata.namespace`? | `serializeNamespace` |
| Is this folder one namespace or many? | whether `{namespace}` appears in the template. A template without it is single-namespace by construction |
| Which folder do new files go in? | the directory part of the template. Fact 1: this is not constrained to flat |
| A root, with children? | `spec.path` is the root, the template's directories are the children, and the ancestor walk keeps every child reachable |

No field in that table exists to answer a question a user did not ask, which is the test the earlier
`kind`/`scope` pair failed: `scope: SingleNamespace` restated in an enum what the template already
said, and then required an admission rule to keep the two in agreement.

## What this deletes

Against the earlier proposal in this document, and against the maintainer review's response to it:

- **`spec.layout`** and its discriminator. With it go `kind`, `type`, `Auto`/`Kustomize`/`Tree`/
  `Flat`/`Template`, and the review's L3 and L4 naming findings — not renamed, gone.
- **`layout.scope`** and the admission rule keeping it in agreement with `allowedSourceNamespaces`,
  and with them the review's L5. The template says it.
- **`kustomize.create`** and the review's L8, folded into `kustomizeRoot: Create`.
- **The `LayoutProfile` question.** It was already answered no; without a `layout` block the only
  thing left to share is the `byType` map, which was the earlier document's own conclusion about
  where reuse pressure actually lives. Whatever generates thirty GitTargets repeats two fields for
  free.
- **The migration.** `spec.placement` keeps its meaning, so there is no loud rejection, no
  `feat(api)!` on this axis, and no coordinated consumer bump for the layout work. The two new fields
  are additive with defaults equal to today's behavior.

That last one is the point. The layout model was the largest breaking change in the queue. On this
shape it is not a breaking change at all.

## Status

Three things the maintainer review asked for, taken:

```yaml
status:
  observedGeneration: 4
  conditions:
    - type: LayoutResolved
      status: "True"
      reason: SingleKustomization        # SingleKustomization | Ambiguous | None
      message: "render root '.' governs new files"
      observedGeneration: 4
    - type: Ready
      status: "True"
      reason: Succeeded
  placement:
    renderRoot: .
    serializeNamespace: Auto             # what it resolved to for this folder
    byTypeEntries: 1
    observedRevision: 9f3c1ab
    observedTime: "2026-07-30T09:14:22Z"
    examples:                            # capped at three, illustrative, not a tally
      - type: v1/secrets
        path: apps/demo/secrets/db.sops.yaml
        source: ByType
```

- **`renderRootReason` is a condition reason, not a field.** It was a reason enum wearing a field's
  clothes, and every consumer in this ecosystem already reads reasons from `conditions`. Deciding it
  now avoids shipping a status field one release before the model that defines it and then breaking
  it.
- **No accumulating counters.** `placedResources`, `overriddenTypes` and `refusedResources` are
  metrics; `placements_total` already carries them with better labels. A monotonic counter in status
  means a status write per event, which is a hundred etcd writes a minute on a busy target for
  something nobody polls at that resolution — and it re-creates the self-triggering reconcile edge
  the status work already fixed once. `examples` stays, capped and fixed-size, because "show me where
  a Secret would land" is not a metric.
- **`conditions` and `observedGeneration` are shown**, because every scenario README already asserts
  `Ready=True` and the two documents should not disagree about what status looks like.

The current half must never depend on a placement having happened: `renderRoot` is a fact about the
folder from the last scan, available before anything is ever written.

## Metrics

`placements_total` keeps `source` — `byType`, `default`, `kustomizeRoot`, `canonical` — which now
names the rung that answered rather than a resolved layout kind. No label break, because the ladder
survives.

## Mutability

`spec.placement` is mutable today and stays mutable. Existing files never move, so changing a
template affects only files written after the change, and a folder can hold documents placed under
two different templates. That is worth saying plainly rather than fixing: match-first identity means
those documents are still found and updated in place, and the alternative — an immutable field with a
CEL widening exception — was machinery invented to protect a discriminator that no longer exists.

## What this changes about the work already queued

- **The ancestor walk** shipped, and it is the invariant everything here rests on.
- **`{kindLower}` and the versionless identity fix** are template features and stay queued.
- **`status.placement`** replaces `status.layout`, in the shape above. Build it next; it is the
  legibility gap, and it is the only surviving argument from the earlier thesis that has not been
  answered by a field.
- **The post-scan validation pass** is now the home for three rules rather than one, and it is what
  makes `serializeNamespace: Never` and `kustomizeRoot: Require` honest.
- **The breaking wave** loses its largest member. What remains breaking on `GitTarget` is unrelated
  to placement, and is sequenced in [`api-wave.md`](api-wave.md).

## Open questions

- Does `serializeNamespace: Never` need to **name** its supplier (`KustomizeRoot`,
  `FluxTargetNamespace`, `Asserted`) so the post-scan pass can check the guarantee rather than infer
  which one was meant?
- Is `Require` the right default for a folder that already contains a kustomization when the target
  is created? Adopting a kustomize folder and then silently continuing after its root is deleted is
  the failure `Require` exists to prevent, and the folder itself is evidence of intent.
- `CreatePerDirectory`, above: what is the smallest scenario that actually needs it?
- Should `placement.default` gain a CRD default now that a defaulted template no longer produces
  unrendered files? The remaining objection is legibility, not correctness, which is a materially
  weaker case than the one that was refused.
