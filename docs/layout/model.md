# The template was the right primitive. Two things were missing

> **design**: a proposal, not a plan of record. Nothing here binds until scheduled.
> Index: [`../INDEX.md`](../INDEX.md)
> Date: 2026-08-28. Supersedes this document's own earlier thesis, which argued that a path template
> is the wrong primitive and should be replaced by a `spec.layout` discriminated union. That argument
> no longer holds; why it stopped holding is the first section, because a reversal is worth more than
> a quiet edit.
>
> Concrete repository folders and matching configurations live in
> [`examples/README.md`](examples/README.md).

Placement today is a ladder of four rungs, three of which are path templates and one of which is not:

```text
byType -> default -> the folder's one kustomize root -> canonical
```

The proposal is to **keep that**, and add the two things a path genuinely cannot express: whether
`metadata.namespace` is written into the document, and what to do about the `kustomization.yaml` that
decides whether anyone ever renders it.

```yaml
spec:
  path: apps/demo
  placement:
    byType:                                         # unchanged, as shipped
      v1/secrets: "secrets/{name}{sensitiveSuffix}"
    default: "{namespace}/{resource}/{name}.yaml"   # unchanged, as shipped
    serializeNamespace: Auto                        # Auto | Always | Never
    kustomizeRoot: Adopt                            # Adopt | Create | Require
```

Two new fields, inside the struct that already holds the placement axis. No discriminator, no
`spec.layout`, no `kind`, no `scope`, no new CRD. `spec.placement` is an existing optional struct, so
the two fields are additive: an object that says nothing behaves exactly as it does today. Why they
nest here rather than sitting beside `placement` at the top of the spec is
[`api-wave.md`](api-wave.md)'s "Where the fields live" — briefly, they are placement concerns, and
grouping them is free only before they exist.

## What changed

The earlier thesis rested on five arguments. Three were retired by
[#319](https://github.com/ConfigButler/gitops-reverser/pull/319), which shipped the ancestor walk: a
new file is registered with the nearest kustomization that governs it, whatever chose its path.

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
   directory fails unless it contains its own kustomization. Every file is named explicitly or lives
   under a nested root.
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

The name deliberately avoids `writeNamespace`. "Write" is the most loaded word in this API — the
write boundary, the write jail, `WriteBoundaryRefused` — so `writeNamespace: Never` invites the
reading *"never write to this namespace"*, a permission, which is precisely what the neighbouring
`sourceNamespace` fields are. `serializeNamespace` names the moment the decision is made, and cannot
be read as policy.

**It governs namespaced resources only.** A `ClusterRole` has no namespace, so the field is ignored
for cluster-scoped documents rather than being an error — worth stating in the field documentation,
because a tree folder is the type most likely to carry both.

### The guard on `Never`, and why it is a post-scan check

`Never` hands the object to whatever namespace the applier happens to be pointed at, which is a
different object with the same name. So it is valid only when something guarantees the namespace.
Three of the six worked examples set it against a `kustomization.yaml` **the user owns**: they delete
one line from their own file and every subsequent document silently relocates.

That precondition is a property of the observed folder, not of the spec, so no CEL rule can check it.
It joins a post-scan validation class of three rules with one condition shape, specified where it is
built — [`implementation-plan.md`](implementation-plan.md)'s PR 3 — and each row gets a corpus
scenario with an `expected-status.yaml` instead of a patch.

## `kustomizeRoot`

The field that answers "do we want kustomize". One question with one axis, and the axis is **what to
do when no kustomization governs the path** — because when one does govern, all values register,
which is the invariant.

| Value | A kustomization governs the path | No kustomization governs the path |
|---|---|---|
| `Adopt` (default) | register the file in its `resources:` | write the file, touch nothing |
| `Create` | register | create `kustomization.yaml` at `spec.path`, then register |
| `Require` | register | **refuse the write** |

`Adopt` is today's behavior after #319, so a user who says nothing gets what they already have.

`Create` is the empty-repository bootstrap — the last surviving argument from the earlier thesis, now
one value of one field rather than a reason to redesign the primitive. It closes its own loop with
`serializeNamespace`: a root the operator creates can carry `namespace:`, so `Never` becomes provable
rather than trusted. That is the difference between establishing a convention and guessing one, and
it is the thing inference structurally cannot do on an empty folder.

`Require` is the safety value, and it earns its place from the `Never` guard above: this folder is a
kustomize folder; if the root disappears, stop writing rather than commit files nothing renders.
Without it, the only responses to a deleted root are to carry on silently or to hard-code a refusal
nobody asked for.

The name matters because `kustomization: Adopt` was the first spelling, and it collides with a real
Kubernetes kind — Flux's `Kustomization` — in a field whose values look like a mode. `kustomizeRoot`
uses the vocabulary this project already has (`renderRoot`, "the folder's one kustomize root") and
names a folder concept rather than an object.

### Values considered and not taken

**`Ignore` — never touch a kustomization, even when one governs the path.** Rejected. The scenario
for it is a tree with two consumers, where the root belongs to somebody else's build. But `spec.path`
already expresses that: the ancestor walk is bounded by the write jail, so a kustomization **above**
`spec.path` is never touched, and the fix for "do not edit that root" is to root the target at the
folder you own. It is also the only candidate that does not fit the axis — it changes what happens
when a root **is** present, which is the half that should be invariant. Revisit if someone produces a
folder they must target, containing a kustomization they must not edit, that cannot be split.

**`CreatePerDirectory` — a nested root in every directory the template writes into, each wired into
its parent, each carrying its own `namespace:`.** Deferred, not rejected, and recorded because fact 2
proves it works: it is what would make `serializeNamespace: Never` safe in a multi-namespace tree,
since the operator would own every root the omission depends on. It is materially more machinery than
the other three values. **Trigger**: someone who wants a multi-namespace folder without namespaces in
its documents.

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
| Do my documents carry `metadata.namespace`? | `serializeNamespace` |
| Is this folder one namespace or many? | whether `{namespace}` appears in the template. A template without it is single-namespace by construction |
| Which folder do new files go in? | the directory part of the template. Fact 1: this is not constrained to flat |
| A root, with children? | `spec.path` is the root, the template's directories are the children, and the ancestor walk keeps every child reachable |

No field in that table exists to answer a question a user did not ask, which is the test the earlier
`kind`/`scope` pair failed: `scope: SingleNamespace` restated in an enum what the template already
said, and then required an admission rule to keep the two in agreement.

## What this deletes

`spec.layout` and its discriminator, `kind`, `type`, the `Auto`/`Kustomize`/`Tree`/`Flat`/`Template`
values, `layout.scope` and the admission rule keeping it in agreement with `allowedSourceNamespaces`,
`kustomize.create`, and with them four findings of the maintainer review (L3, L4, L5, L8) — not
renamed, gone. The `LayoutProfile` question goes too: without a `layout` block the only thing left to
share is the `byType` map, and whatever generates thirty GitTargets repeats two fields for free.

**And the migration.** `spec.placement` keeps its meaning and gains two optional members, so there is
no loud rejection, no `feat(api)!` on this axis, and no coordinated consumer bump for the layout work.
The layout model was the largest breaking change in the queue; on this shape it is not a breaking
change at all.

## What it leaves standing

- **`spec.placement` is mutable and stays mutable.** Existing files never move, so a template change
  affects only files written afterwards, and a folder can hold documents placed under two templates.
  Match-first identity keeps finding and updating them in place. The immutability-plus-CEL-widening
  machinery an earlier draft proposed was invented to protect a discriminator that no longer exists.
- **`placements_total` keeps its `source` label** — `byType`, `default`, `kustomizeRoot`, `canonical`
  — which now names the rung that answered rather than a resolved layout kind. No label break,
  because the ladder survives.
- **`status.placement` replaces `status.layout`**, and it is the only surviving argument from the
  earlier thesis that no field answers. Its shape and the three decisions it takes are in
  [`implementation-plan.md`](implementation-plan.md)'s PR 3; build it before either field here.
- **`{kindLower}` and the versionless identity fix** are template features and stay queued.

## Open questions

- Does `serializeNamespace: Never` need to **name** its supplier (`KustomizeRoot`,
  `FluxTargetNamespace`, `Asserted`) so the post-scan pass can check the guarantee rather than infer
  which one was meant?
- Is `Require` the right default for a folder that already contains a kustomization when the target
  is created? Adopting a kustomize folder and then silently continuing after its root is deleted is
  the failure `Require` exists to prevent, and the folder itself is evidence of intent.
- Should `placement.default` gain a CRD default now that a defaulted template no longer produces
  unrendered files? The remaining objection is legibility, not correctness.
