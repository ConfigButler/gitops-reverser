# GitOps API: editing existing GitOps folders as a product surface

> Status: active workstream
> Captured: 2026-07-06
> Related:
> [../../future/idea-application-editing.md](../../future/idea-application-editing.md),
> [../manifest/contextual-namespace-and-kustomize-folder-editing.md](../manifest/contextual-namespace-and-kustomize-folder-editing.md),
> [../manifest/file-agnostic-placement.md](../manifest/file-agnostic-placement.md),
> [../../finished/current-manifest-support-review.md](../../finished/current-manifest-support-review.md)

## Goal

Enable a product layer on top of GitOps Reverser where an end user points the
operator at a Git folder holding **raw YAML or simple Kustomize**, edits those
resources through the Kubernetes API, and the operator writes the changes to a
branch. The product layer then opens the pull request through the Git host's
API.

The division of responsibility is deliberate and already a recorded decision
([file-agnostic-placement.md](../manifest/file-agnostic-placement.md)):

- **GitOps Reverser**: watch live state, edit the folder the way a careful
  human would (in place, comment-preserving, refusing what it cannot own),
  push to a named branch, and expose pollable status (`CommitRequest`
  `Pushed=True` + `status.sha` + `status.branch`).
- **Product layer**: session/branch lifecycle policy, PR creation and merge,
  branch cleanup, and any UI.

The operator never gains Git-host knowledge (no GitHub/GitLab API calls).

## The governing rule

Every feature in this workstream must keep the repository **round-trippable**:
one source document owns one live object, and every edit the operator writes
must be expressible in both directions (live → Git and Git → live via the
GitOps tool's render). Constructs that are lossy or one-way (arbitrary patches,
generators with hash suffixes, name prefixes, Helm inflation) stay refused —
that refusal is the product's defensible support contract, not a gap.

## Feature ladder

Ordered by value-per-risk. Each feature gets its own design doc in this folder
when work starts.

| # | Feature | Design doc | Status |
|---|---------|-----------|--------|
| F1 | Kustomize `images:` / `replicas:` edit-through — a live change produced by an override entry is written back to that entry, never through into the source manifest | [f1-images-replicas-edit-through.md](f1-images-replicas-edit-through.md) | implemented ([#198](https://github.com/ConfigButler/gitops-reverser/pull/198)) |
| F2 | Render-root scoping — a GitTarget declares its render root (e.g. `overlays/env1`); base files reached through `../../base` become read-only context, dissolving overlay fan-out ambiguity | — | not designed |
| F3 | Restricted patch authoring — write/update scalar-field strategic-merge patches in an overlay ("live drift lands in the environment's overlay") | — | not designed |
| F4 | New-file placement rules — sibling inference + `spec.newFilePath` template so new resources land in the folder's convention, not the canonical REST path | designed in [version2/gittarget-new-file-placement-rules.md](../manifest/version2/gittarget-new-file-placement-rules.md) | designed, unbuilt |
| F5 | Branch/session ergonomics — base-branch selection, opt-in remote branch cleanup, a GitTarget-level quiescence condition | — | not designed |

## What already works (baseline, 2026-07-06)

- Raw-YAML folders (explicit namespaces): in-place, comment-preserving edits;
  match-first placement; mark-and-sweep resync.
- Single-context Kustomize folders (`namespace:` + `resources:`/`bases`, local
  files and child-directory bases): graph-aware namespace inference; inherited
  namespaces are kept out of the file bytes on write.
- Branch writing: `GitTarget.spec.branch` (immutable, glob-authorized via
  `GitProvider.spec.allowedBranches`); `CommitRequest` as the per-save control
  surface with terminal `Pushed=True` + SHA.
- Refusals: unsupported kustomize features, duplicate identities, impure or
  foreign content — refuse-first, never mis-edit.

## Known boundary (what stays refused)

Classic base + per-env overlay repositories are refused today on two
independent gates: any overlay feature outside the supported subset
(`patches*`, generators, `components`, `namePrefix`/`nameSuffix`, Helm, remote
bases), and multi-root namespace fan-out over a shared base
(`ambiguous-namespace`). F1 does not change that boundary; F2/F3 are the
features that would move it.
