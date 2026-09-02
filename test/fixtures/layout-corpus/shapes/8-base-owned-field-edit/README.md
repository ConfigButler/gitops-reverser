# 8 — Editing a field the base owns

> **This scenario exercises the write path, not placement.** No file is placed and the placement
> ladder never runs. It is here because it is the property that makes
> [shape 6](../6-kustomize-base-and-overlays/README.md) and
> [shape 7](../7-kustomize-layered/README.md) safe to point at a real repository, and because it is
> the first question anyone asks after "which folder does a new file go in": *what happens when the
> thing I changed is not mine to change?*

**The same object, two fields, two opposite outcomes.** That is the whole scenario. One live
`Deployment`, owned by a base outside the target's write scope, is edited twice: once in a field
kustomize has a declaration for, and once in a field it does not.

## Starting repository

```text
apps/checkout/
  base/
    kustomization.yaml
    deployment.yaml          # image 1.4.0, env LOG_LEVEL=info
  overlays/prod/
    kustomization.yaml       # namespace: shop-prod, resources: [../../base] — no images: block
```

The target is [`config/gittarget-prod.yaml`](config/gittarget-prod.yaml) at
`apps/checkout/overlays/prod`. The base is one directory up: **read** to render the overlay, never
written. Read scope is wider than write scope, always.

## Part one: the image tag, which the overlay can express

[`input/deployment-image-bumped.yaml`](input/deployment-image-bumped.yaml) is the live object with
`1.4.0` bumped to `1.5.0`. The document that carries that value is `base/deployment.yaml`, out of
reach — so the writer does not edit it, and does not refuse either. It **authors a new `images:`
entry in the overlay**:

[`expected-image-bump.patch`](expected-image-bump.patch) adds a block that did not exist:

```yaml
images:
  - name: ghcr.io/example/checkout
    newTag: 1.5.0
```

Two details make this more than a convenience, and both are visible in the code rather than inferred:

- **`overlayAuthorKustomization` fires only in this exact situation** — the matched document is out
  of the write jail *and* the overlay has a supported render root of its own
  ([`plan_flush.go`](../../../../../internal/git/plan_flush.go)). For a self-contained folder or an
  in-jail document it returns `""`, and the source file is edited directly, because there is nothing
  to route around.
- **The authored entry is put to the re-render oracle before it can commit.** The proposal is not
  trusted because it looks reasonable: the folder is rebuilt with the entry applied, and the result
  must render to the live object. A proposal that over-reaches is refused there rather than written
  ([`OverrideEdit.Create`](../../../../../internal/manifestanalyzer/overrides_projection.go)).

So the change lands as an **environment-specific declaration**. `test` and `acceptance` still render
`1.4.0`, which is the property the base/overlay split exists for, and which a write into the base
would have destroyed.

## Part two: the env var, which it cannot

[`input/deployment-env-changed.yaml`](input/deployment-env-changed.yaml) is the same Deployment with
`LOG_LEVEL` changed from `info` to `debug`. Kustomize has no declaration for an env var — that is
what a strategic-merge patch is for — so there is nothing to author into the overlay.

The result is [`expected-env-change-status.yaml`](expected-env-change-status.yaml): **no patch,
because no file is written.**

### How the refusal happens

Not by a filter that inspects the edit up front. The edit is computed in full, and *then* the write
plan is refused as a whole.

1. **Match-first finds the base document.** The live object's identity resolves to
   `base/deployment.yaml` — in the render scope, out of the write jail.
2. **The projection tries to place the change.** `SplitDesiredForOverrides` computes the *source
   form* the document would need and peels off what an override entry can supply. In part one that
   was everything; here it is nothing.
3. **What is left goes into the base file's buffer**, which is now a planned write to
   `base/deployment.yaml`.
4. **`pathScopePrecondition` refuses before a byte moves.** L1 runs in `writeBatch.flush` at the one
   moment every planned path is known: the path is outside `writeSubdir`, so it raises one
   `write-escapes-scope` issue and returns. Nothing is staged, nothing is committed.

A second route reaches the same place: a projection that cannot express the desired state in source
form at all refuses earlier with `unplaceable-edit` rather than guessing. Different mechanism, same
reason code.

### What that costs

- **The blast radius is the whole flush, not the offending edit.** A commit window that also held
  three placeable writes commits none of them. Refusing a batch whose intent cannot be honoured is
  better than committing half of it — but one base-owned field change stalls everything the target
  was about to write.
- **It is reported once, on the GitTarget, and not per edit.**
  [`gitPathRefusalReason`](../../../../../internal/watch/event_router.go) maps a refusal made purely of
  write-boundary kinds to `WriteBoundaryRefused` — distinct from the umbrella `UnsupportedContent`,
  because the folder is not malformed; the edit had nowhere honest to land. There is **no per-edit
  record**: `FullyReflected` and the unreflected set are designed and unbuilt in
  [`unreflectable-edits-and-write-gating.md`](../../../../../docs/design/support-boundary/unreflectable-edits-and-write-gating.md).
- **Telling anyone is its own mechanism.** The resync path returns the refusal on its result channel;
  the live-event path has none, because a commit window is finalized on a timer, so the branch worker
  reports it through a `GitPathRefusalReporter` hook the watch manager installs. Without that hook
  the write would still be correctly prevented and nobody would be told — the worse failure.
- **Recovery is sticky on purpose.** Only a successful per-type resync clears `GitPathAccepted`; a
  live write never does, because a write that happens to avoid the offending file proves nothing
  about the rest of the subtree. The refused edit is not queued anywhere.

**From outside the operator:** the `kubectl apply` succeeded, the cluster runs `debug`, Git has not
moved, and the GitTarget is `Stalled` naming `base/deployment.yaml`.

## Why this scenario is worth keeping

It pins the boundary between the two halves of the support contract in one folder. The operator
**inverts what kustomize declares** and refuses everything else, and part one is what stops that
sounding like a limitation: within the declared surface it will do real work, including writing a
declaration that was not there before, and it proves the result by re-rendering.

The route that would extend part two — authoring a narrow strategic-merge patch into the overlay and
proving the rebuild changes that field and nothing else — is designed and unshipped in
[`patch-authoring.md`](../../../../../docs/design/support-boundary/patch-authoring.md). Until it lands, this
scenario is the line: **`images:` and `replicas:` become overlay declarations, everything else
inherited is refused.**

A closely related example from the use-case side, where an `images:` entry already exists and is
updated rather than authored, is
[shape 6](../6-kustomize-base-and-overlays/README.md).
