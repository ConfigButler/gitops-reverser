# Higher-level KRM objects as first-class documents

> Status: shipped ([#203](https://github.com/ConfigButler/gitops-reverser/pull/203)) —
> corpus + e2e pins landed with this doc; **no operator code changed** (the guarantee
> already held; this work makes it load-bearing). Filed under `finished/`.
> Captured: 2026-07-07
> Related:
> [../README.md](../README.md),
> [../kustomize-support-boundary.md](../kustomize-support-boundary.md),
> [images-and-replicas-edit-through.md](images-and-replicas-edit-through.md),
> [../../../future/idea-application-editing.md](../../../future/idea-application-editing.md),
> [../../../architecture.md](../../../architecture.md)

## Premise: the pipeline is already kind-agnostic

The promise is *install an app = add a KRM document; roll out a version = edit a
field*, and it must hold for the objects platform teams actually deploy:
Flux `HelmRelease`, Flux `Kustomization`, Argo CD `Application`, KRO resources,
and plain core objects. That already works, because **nothing in the write path
carries a per-kind allowlist**:

- **Watch selection** is by `(group, version, resource)` resolved against the
  *served* API surface — any served GVR (core, CRD, or aggregated) is watchable.
  A `WatchRule`/`ClusterWatchRule` names plural resources and optional
  `apiGroups`; an omitted group is resolved from discovery
  ([../../../../api/v1alpha3/watchrule_types.go](../../../../api/v1alpha3/watchrule_types.go),
  `ResourceRule` — "resolves the resource name across all served API groups").
  There is no set of "supported kinds."
- **The in-document editor** (`internal/git/manifestedit`) edits generic
  `yaml.Node` trees keyed only by object identity
  (apiVersion/kind/namespace/name). It has no notion of any specific kind
  ([../../../../internal/git/manifestedit/DECISION.md](../../../../internal/git/manifestedit/DECISION.md)).
- **Placement** is GVR-driven: the cold-start default is
  `{namespace}/{group}/{resource}/{name}.yaml`, and sibling inference /
  `spec.placement` are equally generic
  ([../../../architecture.md](../../../architecture.md), *What It Writes to Git*).

The one kind-keyed *filter* in the pipeline is a small **exclusion** list of
noisy built-ins (`pods`, `events`, `endpoints`, `endpointslices`, `leases`,
`controllerrevisions`, `flowschemas`, `jobs`, …) — a deny list, never an allow
list ([../../../../internal/watch/resource_policy.go](../../../../internal/watch/resource_policy.go)).
`HelmRelease`, `Application`, and KRO kinds are not on it, so they are
default-allowed. The remaining kind-specific branches are **write-safety** and
**override** rules, and higher-level KRM falls through all of them:

- Sensitive types (core `Secret` plus configured types) are encrypted before
  they touch the worktree. A `HelmRelease`/`Application`/KRO instance is not
  sensitive by default, so it writes plaintext like any document.
- The `/scale` subresource route and the kustomize
  [`images:`/`replicas:` override route](images-and-replicas-edit-through.md)
  apply to specific shapes. A control-plane CR hits neither and takes the
  ordinary in-place patch path.
- `status` is stripped from every mirrored object and status-only churn is
  deduped, so a controller reconciling the CR does not produce spurious commits
  (`internal/sanitize/marshal.go`, `internal/watch/target_watch.go`).

This kind-agnosticism is **already e2e-proven for a generic CRD**: the
icecream-order lifecycle spec installs an arbitrary CRD and pins
create→file / update→file / delete→file-removed with `status` never committed
([../../../../test/e2e/crd_lifecycle_e2e_test.go](../../../../test/e2e/crd_lifecycle_e2e_test.go)).
What this work adds is the same proof for a **named, real higher-level
control-plane type** (Flux `HelmRelease`) plus the un-installed kinds at the
unit level.

So a Flux `HelmRelease`, a Flux `Kustomization`, an Argo CD `Application`, a KRO
`ResourceGraphDefinition` or its generated instance, and a plain `Deployment`
all mirror and edit **identically** — they are KRM documents, and editing KRM
documents is the whole surface. Higher-level tools come into scope as KRM, not
as special cases.

## Why prove something that already works

This is not a code feature; it is the **proof and the promise**. It matters for
two reasons:

1. **Regression fence.** "Kind-agnostic" is an *emergent* property of several
   independent components (watch resolution, the node editor, placement,
   sensitivity classification, override routing). A future special-case — a new
   override heuristic, a placement tweak, a sensitivity default — could quietly
   break a control-plane CR with no test noticing. A corpus + e2e make the
   guarantee load-bearing so a regression announces itself.
2. **Stating the boundary correctly.** The support contract is "we edit KRM
   documents; we do not inflate Helm charts." Users conflate "Flux
   `HelmRelease`" with "Helm." The user docs draw the line: the `HelmRelease`
   *document* is first-class; `helmCharts:` *inflation* stays permanently
   refused — see
   [../kustomize-support-boundary.md](../kustomize-support-boundary.md).

## What this pins

### Coverage split

| Kind | live e2e (mirror + edit) | unit corpus (round-trip + convergence) |
|---|---|---|
| Flux `HelmRelease` (`helm.toolkit.fluxcd.io/v2`) | ✅ CRD in base e2e cluster | ✅ |
| Argo CD `Application` (`argoproj.io/v1alpha1`) | — (CRD not in cluster) | ✅ |
| KRO instance, e.g. `PodInfoApp` (`kro.run` group) | — (KRO is demo-only) | ✅ |
| core (`ConfigMap`/`Deployment`) | already pinned (e2e labels `inplace-edit`, `f4-placement`, `scale`) | already pinned |

Flux CRDs (`helmreleases.helm.toolkit.fluxcd.io`,
`kustomizations.kustomize.toolkit.fluxcd.io`) are established during standard
e2e setup ([../../../../test/e2e/Taskfile.yml](../../../../test/e2e/Taskfile.yml),
`_flux-installed`), so the live pin uses a `HelmRelease`. The Argo and KRO
controllers are not in the base cluster; their kinds are pinned at the unit
level, where the analyzer/editor need only the document bytes — no controller,
no CRD. **That is exactly the point:** the pipeline never consults the
controller, only the document, so a static document is a faithful proof of the
mirror/edit behavior.

### Live e2e pin — `test/e2e/helmrelease_mirror_edit_e2e_test.go`

Pins the *roll out a new version* case — a chart version on a Flux
`HelmRelease`:

1. A `WatchRule` selecting `helmreleases` → `GitTarget`; create a `HelmRelease`
   live with `spec.chart.spec.version: X` (its source need not resolve — the
   operator mirrors the *object*, not Flux's reconcile result).
2. Assert the operator commits the canonical file
   `{path}/{ns}/helm.toolkit.fluxcd.io/helmreleases/{name}.yaml`.
3. Seed a hand-authored comment into the committed file (semantically inert, so
   the operator leaves it alone).
4. Patch live `spec.chart.spec.version` → `Y`.
5. Assert the file shows `Y`, the comment survives, and `X` is gone — an
   in-place, comment-preserving edit of a control-plane CR, identical to the
   `ConfigMap` case ([inplace_edit_e2e_test.go](../../../../test/e2e/inplace_edit_e2e_test.go)).

### Unit corpus — `internal/git/manifestedit/testdata/corpus`

Add `helmrelease.yaml`, `argocd-application.yaml`, `kro-podinfoapp.yaml`. The
two existing globbed gates then automatically cover them:

- `TestCorpusRoundTrip_ByteIdentical` — every document re-encodes byte-for-byte
  (comment and formatting fidelity).
- `TestConvergence_Corpus` — perturb the projection with a label, prove the next
  reconcile settles to a byte-stable no-op (a field edit converges).

Each document carries a hand-authored comment and a realistic version/revision
field so the gates exercise the properties that matter for these kinds.

## What this deliberately does not do

- **No per-kind semantics.** A `HelmRelease`'s `chartRef`, an `Application`'s
  `targetRevision`, a KRO field are ordinary scalars; the operator edits the
  field a human pointed it at, it does not understand chart/app semantics.
- **No chart inflation.** `helmCharts:` in a kustomization stays refused; the
  operator mirrors the `HelmRelease` *document*, it never renders a chart.
- **No new controllers in e2e.** Argo and KRO stay out of the base cluster;
  adding their CRDs for a live pin is future work, wanted only if the unit-level
  proof ever proves insufficient.
- **No placement changes.** New-file placement for these kinds takes the generic
  placement path; no kind-specific placement was added.

## Test plan

- **Unit:** three corpus documents flow through the two globbed gates
  (round-trip byte-identical, convergence perturb-then-settle) with no new test
  code — the gates already glob `testdata/corpus/*.yaml`.
- **e2e:** `HelmRelease` mirror + chart-version-bump edit, comment-preserving,
  under the `manager` + `f7-higher-level-krm` labels.
- **User docs:** [../../../installing-apps-as-krm.md](../../../installing-apps-as-krm.md)
  — "adding an app is adding a KRM document," with `HelmRelease`/`Application`
  examples and the chart-inflation boundary.
