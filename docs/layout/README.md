# `docs/layout/` — where a document goes in Git, collected in one place

This folder is organized by **topic**, which is a deliberate exception to the rule in
[`../INDEX.md`](../INDEX.md) that the other folders are picked by lifecycle. The layout question
(given a live object, which file in which folder receives it, and what else has to change so that
file is reachable) had grown to five documents spread across `spec/` and `design/`, and following
the argument meant knowing which of the two a given piece lived in.

Lifecycle is still the thing that decides whether a page binds, so each entry below is labelled
with the class it would have had in the old layout. Read the label before you read the page.

## Current behavior, and the code depends on it

These are `spec/`-class. Go source cites them by path, and `task lint-docs` checks those citations.
If you change one of these behaviors, change the document in the same commit.

| Document | What it pins |
|---|---|
| [`new-file-placement-rules.md`](new-file-placement-rules.md) | where a brand-new resource's file goes: declared, the folder's one kustomize root, canonical. Sibling inference is removed, and kept as history |
| [`contextual-namespace.md`](contextual-namespace.md) | kustomize graph-aware namespace inference, and the supported subset |

## Being decided

These are `design/`-class: intent, not shipped behavior.

| Document | What it proposes |
|---|---|
| [`model.md`](model.md) | the proposal: a path template is the wrong primitive, so declare what the folder **is**. `spec.layout` as a discriminated union, with registration as an invariant rather than a rung |
| [`api-wave.md`](api-wave.md) | how the layout break sequences with the other `feat(api)!` work on `GitTarget`, so the consumer pays one bump |
| [`placement-visibility-and-declared-defaults.md`](placement-visibility-and-declared-defaults.md) | the three questions the sibling-inference deletion left. Decided, mostly unbuilt |

## What is deliberately not here

- [`../design/support-boundary/`](../design/support-boundary/README.md) owns what the operator may
  edit and what it refuses. Layout decides **where** a document goes; the support boundary decides
  **whether** it may be written at all.
- [`../design/open-asks-priority.md`](../design/open-asks-priority.md) is the cross-cutting queue.
  It references the layout work rather than containing it.
- [`../future/config-surface-for-a-structured-repository.md`](../future/config-surface-for-a-structured-repository.md)
  is the broader configuration-surface review that the layout thesis came out of. It covers
  `spec.path`, modes and `commitWindow` as well, so it stayed with its companion review.
