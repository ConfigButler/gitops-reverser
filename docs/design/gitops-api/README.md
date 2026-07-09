# GitOps API: editing existing GitOps folders as a product surface

> Status: active workstream
> Captured: 2026-07-06
> Related:
> [kustomize-support-boundary-and-product-model.md](kustomize-support-boundary-and-product-model.md),
> [../../future/idea-application-editing.md](../../future/idea-application-editing.md),
> [../manifest/contextual-namespace-and-kustomize-folder-editing.md](../manifest/contextual-namespace-and-kustomize-folder-editing.md),
> [../manifest/file-agnostic-placement.md](../manifest/file-agnostic-placement.md),
> [../../finished/current-manifest-support-review.md](../../finished/current-manifest-support-review.md)

## Goal

Enable a product layer on top of GitOps Reverser where an end user points the
operator at a Git folder holding **Kubernetes Resource Model (KRM) documents**
— core resources, custom resources, simple Kustomize folders, and higher-level
control-plane objects such as Flux `HelmRelease`, Argo CD `Application`, or
KRO resources — edits those resources through the Kubernetes API, and the
operator writes the changes to a branch. The product layer then opens the pull
request through the Git host's API.

The division of responsibility is deliberate and already a recorded decision
([file-agnostic-placement.md](../manifest/file-agnostic-placement.md)):

- **GitOps Reverser**: watch live state, edit the folder the way a careful
  human would (in place, comment-preserving, refusing what it cannot own),
  push to a named branch, and expose pollable status (`CommitRequest`
  `Pushed=True` + `status.sha` + `status.branch`).
- **Product layer**: session/branch lifecycle policy, PR creation and merge,
  branch cleanup, and any UI.
- **Onboarding (CLI / library)**: repo-wide discovery — enumerate candidate
  `GitTarget`s, classify each folder's layout, report supported-vs-refused with
  reasons, and propose the CRs — built on the shared analyzer engine, writing
  nothing. See
  [f8-repo-discovery-and-onboarding-scan.md](f8-repo-discovery-and-onboarding-scan.md).

The operator never gains Git-host knowledge (no GitHub/GitLab API calls).

## The governing rule

Every feature in this workstream must keep the repository **round-trippable**:
every edit the operator writes has exactly one writable destination in Git,
and the result must be expressible in both directions (live → Git and Git →
live via the GitOps tool's render). Source documents that are shared by more
than one render root are read-only context; lossy or one-way constructs
(arbitrary patches, generators with hash suffixes, name prefixes, Helm
inflation) stay refused. That refusal is the product's defensible support
contract, not a gap.

## Launch use cases and the practical path

The product launches on the things non-Git-experts and busy platform engineers
do every day, not on the hardest repository shapes:

1. **"Add something to the test environment"** — a test deployment, a test
   database, an extra tool, or a product-specific abstraction. In KRM terms
   this is adding ordinary documents: a `Deployment`, a Flux `HelmRelease`
   plus its source, an Argo CD `Application`, or a KRO resource. In an
   explicit Kustomize environment folder, the operator must also add the file
   to that overlay's `resources:`.
2. **"Roll out a new version of an installation"** — bump an image tag
   (F1's `images:` entry, or a plain in-place edit), a chart version on a
   Flux `HelmRelease`, a revision on an Argo CD `Application`, or any other
   version field on an ordinary KRM document.
3. **"Change the boring knobs without learning the repo"** — replicas, values
   held in a CR spec, simple ConfigMap data, or other fields on documents the
   environment owns directly. Fields owned by a shared base are explicitly
   deferred to F3 patch authoring or a normal Git PR.

Two consequences shape the plan:

**Higher-level products come into scope as KRM, not as special cases.**
"No Helm" stays true for *inflation*: `helmCharts` remains refused and we
never render a chart. But a Flux `HelmRelease`, Argo CD `Application`, KRO
resource, or plain `Deployment` is a KRM document. Editing KRM documents is
the product surface; Flux, Argo, KRO, and core Kubernetes are examples of
controllers whose intent can be expressed that way.

**Both standard layouts are on the launch path — scoped to these use
cases.** The day-to-day repertoire is whole-document adds plus governed
version bumps, and that slice is expressible in *both* shapes people
actually have in their GitOps repos:

- **One plain folder per environment** (no shared base; no explicit
  `kustomization.yaml` required — many GitOps tools can apply a directory of
  KRM documents directly). Adding a file to test is just
  adding a file; promotion is a product-level copy/diff between sibling
  folders. Fully supported today.

  ```text
  apps/podinfo/
  ├── test/                            # one GitTarget per environment;
  │   ├── deployment.yaml              # core KRM is intent too
  │   ├── helmrelease-postgres.yaml    # Flux example
  │   └── application-observability.yaml # Argo CD example
  ├── acceptance/
  └── production/
  ```

- **Base + environment overlays, kept un-fancy** — day-one support, and the
  reference layout in
  [kustomize-support-boundary-and-product-model.md §6](kustomize-support-boundary-and-product-model.md):
  each overlay sets `namespace:` and uses only
  `resources`/`images`/`replicas`. This needs **F2** (render-root scoping):
  use case 1 lands as an overlay-local file + `resources:` entry, use case 2
  as the overlay's `images:` entry or an overlay-local KRM document edit.
  The base is read-only, always.

The scoping move that keeps this launchable: **F2 + F4 are day-one
Kustomize support; F3 is the deferred hard part.** Adding overlay-local KRM
and bumping governed versions do not need patch authoring. A per-environment
edit of a base-owned *field* (the `kubectl set env` case) has no destination
until F3. Today such an edit is *prevented* — the write-boundary preconditions
refuse it and fail the GitTarget (`WriteBoundaryRefused`), so it is never written
into the base. Turning that target-level refusal into a per-edit report,
reverted by hydration and never silently lost, is the **designed but unbuilt**
unreflected-set accounting
([unreflectable-edits-and-write-gating.md](unreflectable-edits-and-write-gating.md)),
a launch prerequisite. Tier-2 metrics on how often users hit that wall are
exactly what prices F3.

## Feature ladder

Ordered by delivery priority for the launch path above (F-numbers are stable
identifiers, not an ordering). Each feature gets its own design doc in this
folder when work starts; a shipped feature's doc moves to
[finished/](finished/). The cross-cutting scope decisions behind the ladder — the
kustomization field taxonomy, the supported-layout allowlist, who runs
kustomize, the multi-environment product model (promotion, "factor into
base"), and the mirror-mode vs. intent-cluster topology — live in
[kustomize-support-boundary-and-product-model.md](kustomize-support-boundary-and-product-model.md).

| # | Feature | Design doc | Status |
|---|---------|-----------|--------|
| F1 | Kustomize `images:` / `replicas:` edit-through — a live change produced by an override entry is written back to that entry, never through into the source manifest | [finished/f1-images-replicas-edit-through.md](finished/f1-images-replicas-edit-through.md) | implemented ([#198](https://github.com/ConfigButler/gitops-reverser/pull/198)) |
| F7 | Higher-level KRM objects as first-class documents — corpus + e2e pinning that Flux `HelmRelease`/`Kustomization`, Argo CD `Application`, KRO resources, and core resources mirror and edit like any KRM document (they should already; F7 *proves* it), plus "install an app = add KRM" user docs | [finished/f7-higher-level-krm-documents.md](finished/f7-higher-level-krm-documents.md) | implemented (manifestedit corpus for HelmRelease/Application/KRO; HelmRelease mirror+edit e2e; [installing-apps-as-krm.md](../../installing-apps-as-krm.md) user docs) |
| F2 | Render-root scoping — a GitTarget declares its render root (e.g. `overlays/env1`); base files reached through `../../base` become read-only context, dissolving overlay fan-out ambiguity. Launch scope: overlay `images:`/`replicas:` entries + overlay-local documents, shipped **with** the per-edit `FullyReflected` accounting | — | not designed — **launch-critical** |
| F4 | New-file placement rules — sibling inference + `spec.placement` template so new resources land in the folder's convention, not the canonical REST path; includes creating the `resources:` entry when the target folder carries a kustomization | designed in [version2/gittarget-new-file-placement-rules.md](../manifest/version2/gittarget-new-file-placement-rules.md) | implemented (v1: declared policy + sibling inference steps 1/2/4 + kustomize `resources:` entry; step 3 and ordered-rule Option A deferred) |
| F5 | Branch/session ergonomics — base-branch selection, opt-in remote branch cleanup, a GitTarget-level quiescence condition | — | not designed — **launch-critical** |
| F3 | Restricted patch authoring — write/update scalar-field strategic-merge patches in an overlay ("live drift lands in the environment's overlay"); turns the launch-time unreflected class (per-env edits of base-owned fields) into reflected edits | — | post-launch; priced by tier-2 metrics |
| F6 | Admission preflight gate — opt-in intent-cluster webhook rejecting edits that cannot be reflected into the folder (fail-open; the underlying per-edit `FullyReflected` accounting ships with the F2/F4 launch unit) | [unreflectable-edits-and-write-gating.md](unreflectable-edits-and-write-gating.md) | designed, unbuilt — value highest while F3 is absent |
| F8 | Repo discovery & onboarding scan (**CLI / product-layer**) — walk a whole repo, enumerate candidate `GitTarget`s, classify each folder's layout (plain / kustomize-single / kustomize-overlay / refused), report supported-vs-refused with reasons, and propose the CRs; the product layer (GitHub App) consumes the report to generate `GitTarget`s and open PRs. Built on the shared analyzer engine — **ships no operator code** | [f8-repo-discovery-and-onboarding-scan.md](f8-repo-discovery-and-onboarding-scan.md) | designed, unbuilt |

## What already works (baseline, 2026-07-09)

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
- The two-layer **write boundary**, enforced as write-plan preconditions before
  any byte is written: **L1** — no write leaves `spec.path` (reads may, writes
  never); **L2** — no in-place edit of a source file that more than one kustomize
  render path reaches with override entries at stake (write-fan-in = 1). A
  violation aborts the whole flush, commits nothing, and fails the GitTarget with
  `GitPathAccepted=False` / reason `WriteBoundaryRefused` — on the live-event path
  as well as on resync. Specified in
  [gittarget-granularity-and-cross-environment-edits.md §1](gittarget-granularity-and-cross-environment-edits.md).
  The refusal is target-level; the per-edit `FullyReflected` accounting that would
  name each dropped edit is designed and unbuilt.
- Higher-level KRM documents (Flux `HelmRelease`, Argo CD `Application`, KRO
  resources) mirror and edit exactly like core resources — the pipeline is
  kind-agnostic, now pinned by F7's corpus + HelmRelease e2e
  ([finished/f7-higher-level-krm-documents.md](finished/f7-higher-level-krm-documents.md)).

## Known boundary (what stays refused)

Classic base + per-env overlay repositories are refused today on two
independent gates: any overlay feature outside the supported subset
(`patches*`, generators, `components`, `namePrefix`/`nameSuffix`, Helm, remote
bases), and multi-root namespace fan-out over a shared base
(`ambiguous-namespace`). F1 does not change that boundary; F2/F4/F3 are the
features that would move it.

F2+F4 move the *layout* half of this boundary **at launch**: un-fancy
base+overlay folders (per-overlay `namespace` + `resources`/`images`/
`replicas`) become accepted, new overlay-local KRM can be added to
`resources:`, and the base stays read-only. The *feature* half stays:
`patches*` (until F3 authors its own), generators, `components`, name
prefixes, Helm rendering, and remote bases keep refusing the folder — that
refusal remains the support contract.
