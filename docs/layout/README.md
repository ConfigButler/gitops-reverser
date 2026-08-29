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

[`examples/`](examples/README.md) makes the proposal tangible: six scenarios, each with a repository
folder, the matching `GitTarget` and rule configuration, one live input, and the exact patch the
operator proposes. They are design material rather than install manifests, and `model.md` turns them
into an executable corpus in its first PR.

## What is deliberately not here

- [`../design/gittarget-api-wave.md`](../design/gittarget-api-wave.md) sequences the breaking work on
  `GitTarget`. The placement work is additive and is not part of it.
- [`../design/placement-visibility-and-declared-defaults.md`](../design/placement-visibility-and-declared-defaults.md)
  holds the three questions the sibling-inference deletion left. Decided, mostly unbuilt, and its
  Question 2 is superseded by [`model.md`](model.md).
- [`../design/support-boundary/`](../design/support-boundary/README.md) owns what the operator may
  edit and what it refuses. Layout decides **where** a document goes; the support boundary decides
  **whether** it may be written at all.
