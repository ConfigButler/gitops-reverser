# `docs/layout/` — where a document goes in Git, collected in one place

This folder is organized by **topic**, which is a deliberate exception to the rule in
[`../INDEX.md`](../INDEX.md) that the other folders are picked by lifecycle. The layout question
(given a live object, which file in which folder receives it, and what else has to change so that
file is reachable) had grown to eight documents spread across `spec/`, `design/` and `future/`, and
following the argument meant knowing which of the three a given piece lived in.

Lifecycle is still the thing that decides whether a page binds, so each entry below is labelled
with the class it would have had in the old layout. Read the label before you read the page.

## Current behavior, and the code depends on it

These are `spec/`-class. Go source cites them by path, and `task lint-docs` checks those citations.
If you change one of these behaviors, change the document in the same commit.

| Document | What it pins |
|---|---|
| [`new-file-placement-rules.md`](new-file-placement-rules.md) | where a brand-new resource's file goes: declared, the folder's one kustomize root, canonical. Sibling inference is removed, and kept as history |
| [`contextual-namespace.md`](contextual-namespace.md) | kustomize graph-aware namespace inference, and the supported subset. This is the inference `writeNamespace` is proposed to replace |

## Being decided

These are `design/`-class: intent, not shipped behavior.

| Document | What it proposes |
|---|---|
| [`model.md`](model.md) | the proposal: a path template is the wrong primitive, so declare what the folder **is**. `spec.layout` as a discriminated union, with registration as an invariant rather than a rung |
| [`implementation-plan.md`](implementation-plan.md) | the order the work is built in: six PRs of which exactly one release is breaking, the corpus harness that turns the examples into the definition of done, and six design changes the examples produced |
| [`api-wave.md`](api-wave.md) | how the layout break sequences with the other `feat(api)!` work on `GitTarget`, so the consumer pays one bump |
| [`placement-visibility-and-declared-defaults.md`](placement-visibility-and-declared-defaults.md) | the three questions the sibling-inference deletion left. Decided, mostly unbuilt, and its Question 2 is superseded by [`model.md`](model.md) |

## Reviewed

| Document | What it found |
|---|---|
| [`flux-maintainer-review.md`](flux-maintainer-review.md) | `L1`-`L27` over the model, the plan and the examples, read as a Flux maintainer. The earlier review covering status and the configuration model has been retired into the documents that own its findings |

## Worked examples

[`examples/`](examples/README.md) makes the proposal tangible: six scenarios, each with a
repository folder, the matching `GitTarget` and rule configuration, one live input, and the exact
patch the operator proposes. They are design material rather than install manifests, and
[`implementation-plan.md`](implementation-plan.md) turns them into an executable corpus in its
first PR.

## What is deliberately not here

- [`../design/support-boundary/`](../design/support-boundary/README.md) owns what the operator may
  edit and what it refuses. Layout decides **where** a document goes; the support boundary decides
  **whether** it may be written at all.
- [`../design/open-asks-priority.md`](../design/open-asks-priority.md) is the cross-cutting queue.
  It references the layout work rather than containing it.
- [`../future/config-surface-for-a-structured-repository.md`](../future/config-surface-for-a-structured-repository.md)
  is the broader configuration-surface review that the layout thesis came out of. It covers
  `spec.path`, modes and `commitWindow` as well, so it stayed with its companion review.
