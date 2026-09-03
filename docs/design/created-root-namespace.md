# What namespace does a created `kustomization.yaml` carry?

> **design, decided**: being built. Index: [`../INDEX.md`](../INDEX.md)
> Date: 2026-09-01.
>
> One question, five answers, and the reason the obvious one is wrong. It came out of review of
> [#328](https://github.com/ConfigButler/gitops-reverser/pull/328), which shipped
> `spec.placement.useKustomize` writing `namespace:` into every root it created. This page records
> the choice so the next reader does not have to re-derive it from a diff.

## The question

`spec.placement.useKustomize: true` creates a `kustomization.yaml` when a folder has none.
`spec.serializeNamespace: false` says the documents in that folder carry no `metadata.namespace`.
Set both and something has to decide: does the root the operator writes carry `namespace: <the
target's source namespace>`, or nothing at all?

It is not a formatting question. It decides whether the folder is an artifact anyone can install
anywhere, or a mirror pinned to the namespace it was captured from.

## What is not in question

Three facts, because the first draft of this argument got them wrong:

- **A `kustomization.yaml` with no `namespace:` is ordinary.** It is what a kustomize *base* is.
  Three already ship in our own corpus, and
  [shape 6's](../../test/fixtures/layout-corpus/shapes/6-kustomize-base-and-overlays/repository/apps/checkout/base/kustomization.yaml)
  carries the comment "No namespace: the base is written to be deployable into any of them".
- **Both installers supply one downstream.** Flux's `Kustomization.spec.targetNamespace` and Argo
  CD's `Application.spec.destination.namespace` each apply over the built output, so a
  namespace-less folder lands wherever the installer says.
- **Nothing in kustomize, Flux or Argo refused the namespace-less root.** What refused it was *our*
  render-fidelity gate, comparing a rendered object with no namespace against a live object in
  `shop`. That is discussed under [the fidelity rule](#the-fidelity-rule-that-goes-with-it).

## The options

Likelihood is how often we expect a real user to want that behavior, given the two things this
operator is used for: mirroring a live cluster so it can be reviewed and re-applied, and publishing
a folder other clusters install.

| | What the created root carries | Who it serves | Likelihood useful | What it costs |
|---|---|---|---|---|
| **A** | always `namespace: <source namespace>` | someone mirroring one cluster into a folder only that cluster installs | **Medium.** Real, but it is the case that needs no flag: leave `serializeNamespace` unset and the documents carry their own namespaces | Contradicts the field the user set. They asked us not to serialize the namespace and we serialized it one file up, where it is harder to see |
| **B** | nothing, ever *(decided)* | anyone publishing a folder an installer places: one base, several environments, or a cluster whose namespace is chosen at install time | **High.** It is the standard kustomize base convention, and the only shape that survives being installed twice into two namespaces | The folder stops recording which namespace the objects were mirrored from. Applied with no installer, they land in `default` |
| **C** | it, but only when `serializeNamespace` is unset or `true` | nobody, on inspection | **Low.** When the documents carry their own namespaces a root-level `namespace:` is redundant at best; where it disagrees it silently relocates every document | The worst of both: a transformer nobody asked for, over documents that already answered the question |
| **D** | a namespace from a new explicit field | someone who wants an operator-owned root pinned to a namespace that is not the source namespace | **Low today.** No one has asked, and it is additive later if they do | A third field on a two-field model, and a second way to say something `serializeNamespace` already implies |
| **E** | refuse `serializeNamespace: false` + `useKustomize: true` outright | nobody | **Very low.** It is the one pairing where each half answers the other's question | Deletes the shape the empty-folder bootstrap exists for |

**The decision is B: a created root never writes `namespace:`.** The rule that falls out of it is
one sentence, and it is the field's own meaning: *the namespace comes from the documents when
`serializeNamespace` is unset or `true`, and from the installer when it is `false`.* Nothing the
operator writes pins it anywhere else.

A was shipped first and is wrong for one reason worth stating plainly: `serializeNamespace: false`
means *this artifact does not encode its deployment namespace*, and adding a root must not quietly
change that contract. Where the namespace is written is exactly what the user was configuring.

## The fidelity rule that goes with it

The render gate compares what the folder renders with the live object it mirrors. Under B the
rendered object has no namespace and the live one does, so the gate has to be told when that
difference is the point rather than a fault.

**It is scoped by the governing root, not by the target's field alone.** A pre-existing root that
declares `namespace: shop` has a concrete render contract, and a live `billing` object written into
it really is being relocated: relaxing that would hide the exact failure the gate was built for.

| `spec.serializeNamespace` | The root governing the path | `metadata.namespace` in the comparison |
|---|---|---|
| unset or `true` | any | **checked** |
| `false` | sets `namespace:` | **checked** — the folder makes a concrete claim and must keep it |
| `false` | sets none, or there is no root | **ignored**, and every other field still compared |

The third row is not a weakening; it is the removal of an inconsistency. A namespace-free flat
folder ([shape 2](../../test/fixtures/layout-corpus/shapes/2-flat-namespace-free/README.md)) is never namespace-checked
today, because the gate only arms when a flush touched a kustomization. The same declaration got a
different answer in a kustomize folder purely because a root file existed. Now both answer the same
way.

The [one-source-namespace rule](../layout/model.md#the-second-guard-one-source-namespace-and-this-one-refuses)
is untouched by this and keeps its job. It protects **source** identity: two source namespaces
collapsing onto one namespace-less document that two live objects take turns overwriting. It never
tried to constrain the installer's **destination**, which is what this page is about.

## The other half: a file nobody renders is refused

`useKustomize: true` declares that the folder is a kustomize folder the operator maintains. Under
that declaration, committing a document that no `resources:` list names is committing a file that
looks mirrored and is applied by nothing.

It arises in one case, and creating a second root is not the answer: a folder that already has a
render root, with a `byType` or `default` template placing the document outside it. A second root
would make the folder cover two render roots, which is
[`Ambiguous`](../layout/model.md#statusplacement-and-the-post-scan-pass), and an ambiguous folder
stops placing new documents at all. So the existing root is left alone and **the placement is
refused** rather than written unrendered. The fix is the user's: point the template inside the root,
or point the target at the folder the root governs.

Without `useKustomize` nothing changes. A target that made no claim about kustomize keeps today's
behavior, and this is not a new refusal for folders that never asked for one.

## Where this is written down

[`../layout/model.md`](../layout/model.md) is the layout model and stays the specification; this
page is only the argument behind one of its sentences. The behavior a user reads is in
[`../configuration.md`](../configuration.md), and
[shape 5](../../test/fixtures/layout-corpus/shapes/5-kustomize-single-folder/README.md) is the worked example the corpus
executes.
