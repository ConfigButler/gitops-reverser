# Building the layout model: the order of the work

> **plan**: the implementation order for [`model.md`](model.md)
> and [`api-wave.md`](api-wave.md), under the direction recorded in
> [`../future/direction-and-configuration-surface.md`](../future/direction-and-configuration-surface.md).
> Date: 2026-08-28. Index: [`../INDEX.md`](../INDEX.md).
>
> The designs it sequences remain proposals; this page is the sequencing decision and the
> definition of done for each step. It also folds in six design changes that came out of
> reviewing the worked examples in [`examples/README.md`](examples/README.md), each
> recorded below with the finding that produced it.

## Why the order carries most of the risk

The layout model reads like a rewrite and is not one. The four-rung placement ladder is a
single function, [`LocateNew`](../../internal/manifestanalyzer/placement.go), with a single
caller in [`plan_flush.go`](../../internal/git/plan_flush.go). Everything downstream of the
path decision — registering a new file with the kustomization that governs it, the render
fidelity gate, refusal accounting, the metrics — already exists and stays where it is.
`spec.layout` is a dispatch on `kind` in place of a fallthrough, plus a spec field, plus
validation, plus status.

So the risk is concentrated in two places that have nothing to do with the size of the diff:

- **Review surface.** The wave document names this as its largest cost, and it is right.
- **One coordinated consumer bump.** The consumer pins the image, the Go module and the
  `require` line, so each breaking release is a scheduling event.

The order below is built around one rule: **exactly one release is breaking, and every step
before it ships on its own.** Steps 2 and 3 deliver user-visible value under today's API and
retire most of the wave's uncertainty before the breaking change is written.

## The order

| PR | Content | Breaking | Depends on |
|---|---|---|---|
| 1 | [#295](https://github.com/ConfigButler/gitops-reverser/issues/295) correctness fixes, plus the worked examples as an executable corpus | no | — |
| 2 | `spec.suspend`, `spec.mode`, `spec.interval` | no | 1 |
| 3 | `status.layout` over today's placement model | no | 2 |
| 4 | `spec.layout`, the `LocateNew` rewrite, `spec.placement` as a loud rejection | **yes** | 1, 3 |
| 5 | `commitWindow` and `commit.message` move off `GitProvider` | **yes** | — |
| 6 | The riders: the asserted CommitRequest author, its lifecycle, `meta.LocalObjectReference`, `TooManyStreams`, the ClusterProvider default message | **yes** | — |

PRs 4, 5 and 6 land in one release. They are reviewed as three changes and paid for as one
bump, which is the wave's argument applied to the review rather than only to the consumer.
PR 6 is the trim handle: nothing depends on it.

`F9`'s envtest against the minimum supported Kubernetes version stays outside this order, as
the wave document says. Its answer constrains the enum work, so run it before PR 4 is planned.

## PR 1 — correctness, and an executable corpus (this PR)

Two halves that belong together: the corpus is what proves the fixes, and the fixes are the
first entries the corpus asserts.

### Half one: the #295 fixes

- **The ancestor walk.** A declared path into a subdirectory of a kustomize folder is
  registered with the nearest governing kustomization, whether that kustomization is a
  sibling or an ancestor. This is rule 1 of the layout model, built one release early under
  the current API, where it is a bug fix rather than a new invariant.
- **The versionless identity gate.** `IdentityCompletePlacementTemplate` demands `{version}`,
  which contradicts the deliberate versionless canonical path. The gate accepts a versionless
  identity-complete template.

Both are direction-agnostic correctness and neither touches the API.

### Half two: the corpus harness

[`examples/README.md`](examples/README.md) already has the shape of a golden-file suite —
`repository/`, `config/`, `input/`, `expected-*.patch` — and is read by nobody but a human.
Wiring it up is what stops the design and the implementation drifting, and it converts the
PR 4 review from "does this prose hold together" into "does the diff match the patch".

The harness has a seam waiting for it: `newWorktreeForTest` and `flushEventsToWorktree` in
[`internal/git`](../../internal/git/placement_test.go) already do exactly this at a smaller
scale.

Per scenario folder:

1. Seed a worktree from `repository/` and commit it.
2. Build the write event from `input/`.
3. Derive the flush policy from `config/gittarget.yaml`.
4. Flush, diff the worktree against the seed commit, and compare with `expected-*.patch`.

Two details decide whether this stays maintainable:

- **Normalize the diff.** Blob hashes and index lines are noise; compare paths, modes and
  hunks. A helper that renders a canonical patch from the worktree diff keeps the committed
  `.patch` files reviewable as documents.
- **Regenerate rather than hand-edit.** A `-update` flag that rewrites the `.patch` files
  makes the corpus cheap to extend, and the review of a regenerated patch is the review of
  the behaviour change.

### The skip protocol

Most scenarios describe behaviour PR 4 introduces. Each such case is written now and skipped
with the PR that will unskip it named in the skip message. **PR 4 is finished when the last
skip is gone.** That is the definition of done this plan is built to produce, and it is worth
more than any prose acceptance criteria.

The scenarios that should assert real behaviour immediately are the ones the #295 fixes cover:
`brownfield-kustomize` (the ancestor walk) and any `byType` case pointing into a subdirectory.

### Config parsing without the API

`config/gittarget.yaml` uses fields that do not exist yet. Parse the scenario config into a
small harness-local struct rather than into `v1alpha3.GitTarget`, and map the fields that do
exist onto today's `PlacementPolicy`. When PR 4 lands, that mapping is deleted and the
fixtures unmarshal into the real type — which is itself a useful check that the API shape the
examples describe is the API shape that got built.

### Done when

- `task lint` and `task test` pass; e2e is unaffected.
- Every scenario folder is loaded by the harness; none is silently unreferenced.
- Non-skipped assertions cover both #295 fixes.
- Every skipped case names the PR that unskips it.
- The corpus gaps below are filled.

## PR 2 — `spec.suspend`, `spec.mode`, `spec.interval`

Additive, and the wave's order is right that `suspend` comes before anything that creates
files. Ship the three together because they answer one question — whether and when this folder
is written — and because `interval` is what keeps a scan-derived status fresh.

- `suspend` stops resource writes **and** bootstrap creation. State that before either exists.
- `mode: Observe` scans, resolves and publishes without writing.
- `interval` drives an observation pass on the target.

`suspend` and `mode` stay two fields, per the wave's open question, on the Flux convention that
a suspend switch is its own thing. Each field's documentation must say what the other is for.

## PR 3 — `status.layout` under today's model

The wave puts `status.layout` after the layout field. Inverting that is the highest-value
change in this plan: `status.layout` is useful now, because `renderRoot`, `renderRootReason`
and `observedRevision` are facts about the folder that today's placement model already
computes and discards. Publishing them turns `Observe` from a mode that does nothing into an
adoption dry run, one release early.

`declaredKind` and the resolved `kind` join the same field in PR 4. The two-halves rule holds
from the start: a current half stamped with the revision it came from, and a historical half
accumulated since, with the current half never depending on a placement having happened.

Ship the `Ambiguous` render-root condition and the Event over the existing `EventRecorder`
here as well. Both are about the folder rather than about the layout field.

## PR 4 — `spec.layout` (the breaking one)

- `spec.layout` with `kind`, `scope`, `namespace`, `writeNamespace`, `kustomize`, `byType`.
- `LocateNew` becomes a dispatch on the resolved kind.
- `spec.placement` becomes a loud rejection naming the replacement, following the
  `ClusterWatchRule.spec.rules[].scope` precedent.
- CEL: immutability with the widening exception, and the namespace agreement rule as amended
  below.
- `Auto` resolves once and is pinned.

The one genuinely new coupling is the pin, and it needs deciding before the code is written.
The resolution is computed on the branch worker from a repository scan, while the pin is
reported in status. **Do not put a status read into the write path.** The worker holds the
resolution keyed by `observedRevision` and the roll-up seam projects it into status; a later
folder state that would resolve differently raises a condition instead of re-laying-out the
folder. Status reports the pin; it does not store it for the writer to read back.

## PR 5 — `commitWindow` and `commit.message` move to `GitTarget`

The last of the principle items, and the one that makes the object coherent: batching and
phrasing describe a folder, and `GitProvider` describes a connection. Mechanical, and
independent of everything above, which is why it can be written in parallel and merged into
the same release.

## PR 6 — the riders

`CommitRequest.spec.author` with its SAR guard, the CommitRequest lifecycle hole, F12's
reference types, the `TooManyStreams` cap, and the ClusterProvider default message. In the
release because they are breaking and the consumer should pay once. Trim from here first.

## Design changes this plan folds in

Six changes to the designs above, each from reviewing the worked examples.

### 1. The namespace agreement is checked when present, not required

`model.md` argues that the namespace must be declared **because**
`allowedSourceNamespaces` may be absent, and then requires an exact one-name list under
`scope: SingleNamespace`. The field that may be absent becomes mandatory in the most common
case, and the four-field quickstart becomes six.

**Amendment:** `layout.namespace` is the structural identity and stands alone. The CEL rule is
"if `allowedSourceNamespaces` is set, it must be exactly `[layout.namespace]` with no
selector". A document arriving from a second namespace is refused at the write boundary either
way, which is what enforces singularity. A target that declares both still reads to a reviewer
as the design intends.

### 2. `Auto` needs post-scan validation, because per-kind rules are not admission-checkable

`Flat` may not use `writeNamespace: FromContext` or `Never`: a flat directory has no build step
and nothing supplies the namespace. Under `kind: Auto` that violation depends on the folder,
so no CEL rule can catch it — and the brownfield example declares exactly that combination.

**Amendment:** per-kind field validity is a **post-scan** validation class. When `Auto` resolves
to a kind whose rules the declared fields violate, the target goes `Validated=False` naming the
resolved kind and the field, in the same shape as the ambiguous-root refusal. Add the case to
the corpus with an expected status rather than an expected patch.

### 3. `kind: Template` leaves the wave

`Template` exists to preserve `placement.default`, and a blanket default is the rung that
produced the unrendered-file class of bug. With one consumer and an established loud-rejection
pattern, the migration for `placement.default` can be "declare a structural kind" rather than a
new kind that carries the old hazard forward.

Honest accounting: this removes one kind, rule 2 (a structural kind excludes a blanket
`default`), and the obligation to make registration correct against arbitrary blanket paths. It
does **not** remove the template engine, which `byType` still needs. Add `Template` when a user
asks for something the structural kinds cannot express, and let that request name the case.

### 4. `Auto` stays the CRD default

The wave asks whether a dry-run mode makes an explicit `kind` affordable. It does, and it is
still not worth it: pinning plus `declaredKind` beside `kind` already removes the harm, and
defaulting is what keeps the quickstart at four fields.

### 5. `interval` keeps two names rather than one field

`GitProvider` needs an `ls-remote` cadence and `GitTarget` needs an observation cadence. The
smell is one name on two objects, so name the provider's field for what it polls. Two fields,
two names, no merge.

### 6. Namespace-local `GitProvider` is a recorded gap, not a blocker

The homelab examples put a `GitTarget` in `argocd`, `flux-system` and `homelab-config`, each
needing its own `GitProvider` — three copies of one credential in a single-owner cluster.
`ClusterProvider` already has the `allowedNamespaces` and fail-closed SAR machinery a
cluster-scoped Git provider would need. Out of scope for this order; file it, and say so in the
examples' prerequisites so a reader does not read the duplication as intended design.

## Corpus gaps to fill in PR 1

- **A refusal scenario.** Every example is a happy path, while the model's headline claim is
  what it makes unstatable. One folder covering two roots under a declared `Kustomize` kind, a
  second source namespace under `SingleNamespace`, and a `ClusterWatchRule` against a
  `SingleNamespace` target, with `expected-status.yaml` in place of a patch. This is the
  scenario that proves the design.
- **The missing `ClusterProvider`.** `krm-app-configuration` references
  `clusterProviderRef: app-intent` and no such object appears anywhere. It is the intent
  cluster, which is the whole direction-B thesis, so it deserves a concrete specimen beside the
  cluster-tree one.
- **The prerequisites note** from design change 6.

## What this plan does not decide

- The helm standpoint, which is decided in the direction review and parked with entry criteria.
- Whether a layout is ever its own CRD. The trigger is written down in the layout model: revisit
  for the `byType` map first.
- Whether `scope` is derived and materialized at creation. It stays declared here.
- Whether `writeNamespace: Never` names its supplier.
- The `F9` enum question, which is measured before PR 4 is planned.
