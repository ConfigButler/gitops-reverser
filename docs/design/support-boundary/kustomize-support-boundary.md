# The Kustomize support boundary

> Status: the field taxonomy and layout allowlist behind the support contract.
> The §4 fan-in invariant is no
> longer emergent — it ships as a write-plan refusal (see §1 and
> [gittarget-granularity-and-cross-environment-edits.md §1](gittarget-granularity-and-cross-environment-edits.md)).
> Captured: 2026-07-06
> Related:
> [README.md](README.md),
> [gittarget-granularity-and-cross-environment-edits.md](gittarget-granularity-and-cross-environment-edits.md),
> [finished/images-and-replicas-edit-through.md](finished/images-and-replicas-edit-through.md),
> [../manifest/contextual-namespace-and-kustomize-folder-editing.md](../../spec/contextual-namespace-and-kustomize-folder-editing.md),
> [../unsupported-folder-refusal-plan.md](../../spec/unsupported-folder-refusal-plan.md),
> [../manifest/version2/gittarget-new-file-placement-rules.md](../../spec/gittarget-new-file-placement-rules.md)

## Purpose

The other docs in this folder answer "what do we do about X." This one takes a
step back and answers the questions that decide what X should be:

1. Which part of the kustomization surface can the operator **ever** support,
   and which part is permanently out?
2. Which **folder layouts** do we accept?
3. Who **runs kustomize** — us, or the GitOps tool?

The governing rule from [README.md](README.md) is the measuring stick: the
repository must stay **round-trippable** — every edit the operator writes has
exactly one writable Git destination, and the result must be expressible in both
directions (live → Git, and Git → live via the GitOps tool's render). Shared
source documents are read-only context.

## 1. The kustomization surface: an invertibility taxonomy

Measured against round-trippability, the ~29 documented `kustomization.yaml`
fields ([reference](https://kubectl.docs.kubernetes.io/references/kustomize/kustomization/))
fall into four buckets. The boundary is not a temporary implementation gap —
most of the "never" bucket is *structurally* non-invertible, and saying so is
the support contract.

| Bucket | Fields | Why |
|---|---|---|
| **Invertible — supported** | `resources`/`bases` (local), `namespace`, `images`, `replicas` | Today's subset: namespace inference + `images`/`replicas` edit-through |
| **Invertible — planned** | `patches` (scalar strategic-merge, **operator-authored only**) | The one crossing worth building; environment drift needs a per-environment destination |
| **Invertible — possible, not planned** | `labels`, `commonLabels`, `commonAnnotations`, `buildMetadata` (subtractable in projection); `namePrefix`/`nameSuffix` (identity arithmetic cascades into every name reference); `configMapGenerator` limited to literals + `disableNameSuffixHash` | Cost/value is poor; prefixes especially buy little and touch everything |
| **Never — structurally non-invertible** | `helmCharts`/`helmGlobals` (live state does not determine chart+values inputs); generators with hash suffixes (the hash couples content to rollout — inverting means authoring rename cascades); `replacements`/`vars` (the value's source of truth is another field; a change to the target is semantically ambiguous); `generators`/`transformers`/`validators` plugins (arbitrary code = unknowable render); `components` (patch bundles composed across variants); remote bases (cannot edit what we do not own) |
| **Never — policy** | `secretGenerator` | Plaintext secrets in Git contradict the SOPS stance regardless of invertibility |
| **Refuse rather than tolerate** | `configurations`, `openapi`, `crds` | They silently change merge-key semantics, so the editor's assumptions become wrong with no visible signal |
| **Tolerable ignorables** | `sortOptions` | Does not change object state |

Two nuances found while auditing the current gate
(`hasUnsupportedKustomizeFeature`, `internal/manifestanalyzer/store.go`):

- **Tolerated metadata transformers leak.** `commonLabels`, `labels`,
  `commonAnnotations`, and `buildMetadata` pass the gate silently, but a live
  object can carry transformer-supplied metadata its source file lacks — the
  writer patches it into the file as "drift." The render stays correct
  (idempotent), so this is pollution rather than corruption, but it is the
  same "dead text shadowed by a transformer" pathology the `images:`
  edit-through already fixed. A future projection-side subtraction is the same
  kind of fix; until then the support statement should name the limitation.
- **The fan-out fallback is now an explicit refusal.** Ambiguous override chains
  (`ambiguous-images`, `diamond-images` corpora) still emit a warning at store
  build time, but a *planned write* into such a file is refused before any byte
  is written: write-through into a file consumed by two render roots is the one
  edit that must never happen, and it no longer depends on the coincidental
  namespace ambiguity (`NamespaceNone`) that used to block the live-object match.
  The refusal fails the GitTarget with reason `WriteBoundaryRefused`. See
  [§4](#4-the-invariant) and
  [gittarget-granularity-and-cross-environment-edits.md §1](gittarget-granularity-and-cross-environment-edits.md).

## 2. Supported layouts: an allowlist, not field caveats

The public support statement should enumerate **layouts we accept**, not
fields we reject. Three layouts:

1. **Plain manifest folder** — raw YAML, explicit namespaces. *Shipped.*
2. **Single-context kustomize folder** — one render root; `namespace`,
   `resources` (local files / child dirs), `images`, `replicas`. *Shipped.*
3. **Base + environment overlays** — one shared base, N overlay roots, one
   Kubernetes namespace per overlay. *Designed, not shipped; see §5.* This is
   the canonical layout in the kustomize documentation
   and common GitOps examples — a kustomize story without it reads as toy
   support.

**Scope.** Layouts 1–2 are shipped, and apply *per environment* — one plain
folder per environment, each its own GitTarget. Layout 3 is designed but not
shipped, and its designed scope is deliberately narrow: per-overlay `namespace` +
`resources`/`images`/`replicas`, overlay-local documents, base read-only, and new
overlay-local KRM added to that overlay's `resources:`. The everyday operations —
add something to an environment, bump a version, edit an overlay-local object —
need exactly that slice. Per-environment edits of base-owned *fields* are the hard
part and are deliberately left out; today they are refused rather than written
into the base, and reporting each one as unreflected is the unbuilt per-edit
accounting (§4).

Explicitly **out of scope**, and worth saying in user docs:

- **Fleet / cluster-root repositories** (`clusters/` + `apps/` + `infra/`
  layouts). A GitTarget points at an *app subtree*, never a cluster root.
  This composes fine with fleet repos — a Flux `Kustomization`, Argo CD
  `Application`, or another product object can point at the same app folder
  we do — and keeps us out of infrastructure folders full of Helm rendering
  and components.
- **Helm rendering in any form** (`helmCharts`, hybrid repos): permanently
  refused. Flux `HelmRelease`, Argo CD `Application`, KRO resources, and other
  control-plane CRs are ordinary KRM documents and fully in scope — the
  boundary is chart inflation, not the CRs that request it.
- **Folders transformed outside the folder.** Flux `postBuild` substitutions
  and `targetNamespace`, Argo CD Application-level kustomize overrides, or any
  controller-side transform that is not written in the folder: the folder
  alone no longer determines the render, so the round-trip promise cannot
  hold. Our contract is "we mirror what the folder alone renders." (Reading
  controller CRs in-cluster to learn those transforms is a possible
  much-later feature, not part of this boundary.)

This allowlist is a **write** contract, not a discovery one. The read-only
scanner ([repo scan](repo-discovery-and-onboarding-scan.md)) walks a whole repo
and classifies each folder against *exactly* this boundary — `plain` /
`kustomize-single` (accepted), `kustomize-overlay` (recognised, not yet
accepted), `refused-structural` (the permanent wall above) — without widening
what the operator writes. Discovering broadly and writing narrowly is what lets
onboarding meet real GitOps repos without eroding round-trippability.

## 3. Why overlays are a different animal

Fan-out is exactly why the governing rule is "one writable destination per
edit," not "one source document owns one live object": a base
`deployment.yaml` can produce N live objects, one per environment. Two
consequences:

- **Read side** — which overlay produced the object we are watching? We need
  a variant → namespace mapping.
- **Write side** — an edit observed in the test environment must land in the
  test overlay, *never* in the base. Writing to base changes production
  because someone touched test.

So overlay support is not an incremental extension of the shipped
edit-through; it changes the write model from "edit the document that produced
the object" to "synthesize the minimal expression of the change **in the right
variant**."

## 4. The invariant

> **The operator never writes to a file consumed by more than one render
> root.** (Write fan-in = 1. Base files and shared components are read-only
> context, always.)

> **This rule has one home:**
> [gittarget-granularity-and-cross-environment-edits.md §1](gittarget-granularity-and-cross-environment-edits.md)
> specifies it as the two-layer write boundary (L1 filesystem jail, L2 fan-in = 1)
> and names its enforcement — a write-plan precondition that aborts the whole flush
> and fails the GitTarget with `GitPathAccepted=False` / `WriteBoundaryRefused`.
> The paragraph above states the rule; it does not redefine it. If the two ever
> disagree, §1 wins.

This single sentence:

- makes "operator edits base" structurally impossible — nothing observed in
  one environment can ever change what another environment renders;
- explains the `diamond-images` refusal (two paths from one root);
- decides the multi-environment question for good — the operator cannot
  "add to all environments" *by design*, and nothing above it should expect that;
- **has been promoted from emergent behavior to an explicit, tested rule** — a
  write-plan precondition that refuses the flush (`WriteBoundaryRefused`) rather
  than writing through. It is paired with the filesystem jail (writes never leave
  `spec.path`); the two layers are specified in
  [gittarget-granularity-and-cross-environment-edits.md §1](gittarget-granularity-and-cross-environment-edits.md).
  Generalizing it from "a file two override chains reach" to "any file two render
  roots reach" is render-root scoping.

## 5. The overlay model

With the invariant in place, every observed change in environment X has
exactly one legal destination:

| Observed change in env X | Lands in |
|---|---|
| image tag / replica count | overlay X's `images:`/`replicas:` entry (the shipped edit-through machinery, scoped per render root) |
| new object | new overlay-local file in overlay X **+ a `resources:` entry** (placement plus entry creation) |
| any other spec field (env var, resource limits, args…) | an operator-authored strategic-merge patch in overlay X — **not supported, so nowhere** |
| delete of a base-owned object in one env | unrepresentable without a `$patch: delete` patch — refuse the write |

The third row is the honesty condition on accepting overlays at all: an
arbitrary field edit on a base-owned object has no legal destination without
patch authoring (in-place would be the base). The designed Kustomize support
therefore covers the common slice — overlay entries, overlay-local documents, and
adding overlay-local KRM to `resources:`.

Two mechanisms cover what falls outside that slice, and they must not be
conflated:

| | Status | Granularity | Surface |
|---|---|---|---|
| **Write-boundary refusal** (L1/L2) | **shipped** | aborts the whole flush; commits nothing | `GitPathAccepted=False`, `Stalled=True`, reason `WriteBoundaryRefused` |
| **Per-edit `FullyReflected` accounting** | **designed, unbuilt** | records the individual dropped edit; reverted by hydration | planned `FullyReflected` condition + unreflected set |

Today an out-of-scope edit is *prevented* — refused before any byte is written —
but the target is told at the target level, not per edit. Making the third row
"reported and reverted, never silently lost" is the unbuilt accounting, and it is
a **prerequisite for accepting overlay layouts at all**. That remains a scoped
promise rather than a gap: the everyday operations (add to an environment, bump a
version) never hit the third row, and how often real users *do* hit it is what
would price patch authoring.

If patch authoring is ever built, its scope stays narrow and safe: the operator
would only create and update patches **it authored** (scalar fields, one patch
file per object per overlay), and pre-existing hand-written patches would still
refuse the folder. The gate keeps its shape — we accept exactly the structure we
fully model.

What happens when an edit has **no** legal destination — the "or nowhere"
rows above — and how the user finds out, is designed separately in
[unreflectable-edits-and-write-gating.md](unreflectable-edits-and-write-gating.md)
(per-edit `FullyReflected` accounting, self-healing by re-applying the folder's
render, and the optional admission preflight).

### Variant identification: self-describing folders over configuration

Prefer reading the mapping from the folder to declaring it on the GitTarget.
The analyzer already computes render roots (kustomizations no other
kustomization references); if each overlay sets `namespace:`, the
variant → namespace mapping is free and the repository documents itself.

- **Rule:** every overlay root must set `namespace:`, distinct per root.
  Refusal message: "add `namespace:` to each overlay."
- Namespace injected out-of-band (Flux `targetNamespace`, Argo destination)
  stays refused (§2). An explicit GitTarget-side mapping
  (`spec.variants: [{root, namespace}]`) is a fallback we add only if real
  folders cannot be made self-describing.

### Scoping: one GitTarget per overlay root

The natural shape: a GitTarget's `spec.path` names the overlay
(`…/overlays/test`), so
environment = GitTarget = watch scope = write scope, and multiple GitTargets
share the branch through the existing BranchWorker serialization.

Structural consequence the design must solve: the base sits *outside*
`spec.path`, so the analyzer needs a **read scope** (the repo subtree
reachable via `../../base`) wider than the **write scope** (the overlay
directory). The GitTarget path-overlap rejection must learn that shared
*read-only* context between targets is legal while overlapping *write*
scopes stay forbidden.

## 6. The reference layout

The blessed shape for docs, examples, and the test corpus:

```text
apps/podinfo/
├── base/                                 # read-only to the operator, always
│   ├── kustomization.yaml                #   resources: [deployment.yaml, service.yaml]
│   ├── deployment.yaml
│   └── service.yaml
└── overlays/
    ├── test/                             # GitTarget A → namespace podinfo-test
    │   ├── kustomization.yaml            #   namespace: podinfo-test
    │   │                                 #   resources: [../../base, debug-toolbox.yaml]
    │   │                                 #   images:   [{name: podinfo, newTag: 6.6.0-rc1}]
    │   │                                 #   replicas: [{name: podinfo, count: 1}]
    │   └── debug-toolbox.yaml            # an extra installed only in test
    ├── acceptance/
    │   └── kustomization.yaml            #   namespace: podinfo-acc, resources: [../../base], images/replicas
    └── production/
        └── kustomization.yaml            #   namespace: podinfo-prod, resources: [../../base], images/replicas
```

"Install extras in test" and "add files in test" are the same mechanism: an
overlay-local resource file plus its `resources:` entry, created by the
operator when a new object appears in that environment's namespace.

## 7. Who runs kustomize

Deployment stays with the user's GitOps controller — Flux, Argo CD, or
anything else that consumes KRM — because that keeps the operator out of the
deployment business and out of drift-ownership fights. But "running
kustomize" means two different things:

- **Deploying** (Git → cluster): theirs, always.
- **Understanding** (folder → expected render): ours, necessarily — the
  writer must know which file supplied each live value.

**Decision (2026-07-14): we embed kustomize (`sigs.k8s.io/kustomize/api`) as the
renderer. The re-implementation is being removed.** This reverses the earlier
position — kept here because the reasoning was wrong in a specific, instructive way.

> **Landed so far.** The typed `kustomization.yaml` parse (#229), and
> [`kustomize_render.go`](../../../internal/manifestanalyzer/kustomize_render.go) — a
> sandboxed `krusty` build that returns each object with kustomize's own provenance
> (`config.kubernetes.io/origin` says which file produced it;
> `alpha.config.kubernetes.io/transformations` says which kustomization's transformers
> touched it, in order). The renderer is not yet on the write path: it is currently the
> differential oracle the re-implementation is checked against, which is what makes
> removing the re-implementation safe rather than brave.
>
> **What the oracle found immediately:** the re-implemented image transformer treated
> tag and digest as independent, where kustomize replaces both. A folder using `digest:`
> had the tag written out of its source manifest on every reconcile. Two silent-corruption
> bugs, in shipped code, found by the first differential run — which is the whole argument
> for this decision, made concrete.

The old position was that re-implementing the narrow transformer subset "keeps the
refusal boundary honest: we refuse exactly what we do not model," and that `krusty`
was at best a *verification oracle* comparing against our own projection.

Both halves turned out to be false:

- **The re-implementation does not keep the boundary honest — it makes it dishonest
  in two places.** `vars` was never added to the deny-list, so `$(VAR)` in a source
  file is silently overwritten with its substituted value. `commonLabels`/`labels`/
  `annotations` were classed as benign and leak into source documents as drift.
  "We refuse exactly what we do not model" was the *intent*; what shipped is "we
  refuse most of what we do not model, and corrupt source files with the rest."
- **The dependency was never the cost we thought.** `sigs.k8s.io/kustomize/api` and
  `kyaml` are **already in this module's requirement graph**. Taking them as a direct
  dependency at the version Flux ships (v0.21.1) adds **zero new modules** — one line
  in `go.mod`, and code we already carry starts getting linked instead of re-typed.

Against that: ~1,050 lines across
[`overrides.go`](../../../internal/manifestanalyzer/overrides.go),
[`overrides_projection.go`](../../../internal/manifestanalyzer/overrides_projection.go)
and [`kustomization.go`](../../../internal/git/manifestedit/kustomization.go)
re-derive image-reference parsing, the image transformer, the replica transformer's
fieldspec, render-root discovery and the resource DAG walk — and every future feature
demands more of the same (strategic-merge semantics, name-reference cascades, the
generator content-hash algorithm). We were re-writing kustomize by instalments.

**Understanding is still ours; deploying is still theirs.** Embedding the library does
not put us in the deployment business. It replaces our *guess* at what Flux will render
with the *library Flux actually renders with*.

### The sandbox is part of the contract

```go
krusty.Options{
    LoadRestrictions: kustypes.LoadRestrictionsRootOnly,
    PluginConfig:     kustypes.DisabledPluginConfig(),  // no exec, no Go plugins
}
```

**`LoadRestrictions` does not stop the network, and this was measured, not assumed.**
Given a remote base, kustomize shells out to `/usr/bin/git fetch` — under
`LoadRestrictionsRootOnly` *and* under an in-memory filesystem. Both were tried; both
fetched.

So the remote-base detection we already have
([`hasRemoteResource`/`isRemoteResource`](../../../internal/manifestanalyzer/store.go))
is **not** made redundant by the renderer. It is promoted to a **security precondition
that runs before krusty is ever called**. It is the one piece of the re-implementation
that must survive, and *"we do not run kustomize on a remote base"* stays literally
true — now enforced rather than merely implied by not having a renderer at all.
