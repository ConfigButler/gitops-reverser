# `docs/layout/` — where a document goes in Git, collected in one place

This folder is organized by **topic**, which is a deliberate exception to the rule in
[`../INDEX.md`](../INDEX.md) that the other folders are picked by lifecycle. The layout question
(given a live object, which file in which folder receives it, and what else has to change so that
file is reachable) was spread across `spec/` and `design/`, and following the argument meant knowing
which of the two a given piece lived in.

Lifecycle still decides whether a page binds, so each entry is labelled with the class it would have
had in the old layout. Read the label before you read the page.

| Document | Class | What it holds |
|---|---|---|
| [`new-file-placement-rules.md`](new-file-placement-rules.md) | **spec** | where a brand-new resource's file goes: declared, the folder's one kustomize root, canonical. Go source cites it by path, and `task lint-docs` checks those citations |
| [`contextual-namespace.md`](contextual-namespace.md) | **spec** | kustomize graph-aware namespace inference, and the supported subset. This is the inference `serializeNamespace` overrides |
| [`model.md`](model.md) | **design** | the proposal, reversed and much smaller: the path template **stays**, and gains two optional booleans — `spec.placement.useKustomize`, and `spec.serializeNamespace` one level up because it governs every write rather than only new files. Carries the status stanza, the post-scan pass, and the order the work is built in |

## The worked examples are a test, and they live in the test tree

The shapes and the ecosystem examples used to sit here as two subfolders. They now live at
[`test/fixtures/layout-corpus/`](../../test/fixtures/layout-corpus/README.md), because they stopped
being illustrations: every one of them is seeded, written through the real plan-then-flush path and
diffed against a committed patch by `TestLayoutCorpus`. A folder that a test executes belongs beside
the other fixtures a test executes, and the move also puts it next to
[`test/fixtures/gitops-layouts/`](../../test/fixtures/gitops-layouts/README.md), the corpus it is
most often confused with. The README there says which is which.

Their prose travelled with them, so the corpus is still where a layout question is answered:

| Folder | Answers |
|---|---|
| [`shapes/`](../../test/fixtures/layout-corpus/shapes/README.md) | the cross-product. Flat and tree, each with and without `metadata.namespace` in the document, plus one kustomize folder, base-and-overlays, and layered, with the same live object written into all of them. Carries the decision flow as a diagram, an empty-folder column for every shape, and the measured behavior of the deployers that consume them |
| [`specific-examples/`](../../test/fixtures/layout-corpus/specific-examples/README.md) | the remainder: an Argo CD app-of-apps and a Flux two-layer repository, which are ecosystem scenarios rather than folder shapes, plus the shared `GitProvider` prerequisites |

What stays in this folder is the argument: the two contracts the code cites by path, and the model
that produced them. What moved is the evidence.

## What is deliberately not here

- [`../design/gittarget-api-wave.md`](../design/gittarget-api-wave.md) sequences the breaking work on
  `GitTarget`. The placement work is additive and is not part of it.
- [`../design/placement-visibility-and-declared-defaults.md`](../design/placement-visibility-and-declared-defaults.md)
  holds the three questions the sibling-inference deletion left. Decided, mostly unbuilt. Its
  Question 2 (a CRD default for `placement.default`) is **reopened** by [`model.md`](model.md)'s
  reversal, not superseded by it.
- [`../design/support-boundary/`](../design/support-boundary/README.md) owns what the operator may
  edit and what it refuses. Layout decides **where** a document goes; the support boundary decides
  **whether** it may be written at all.
