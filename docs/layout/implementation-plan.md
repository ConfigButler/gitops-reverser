# Building the placement work: the order

> **plan**: the implementation order for [`model.md`](model.md) and [`api-wave.md`](api-wave.md),
> under the direction recorded in
> [`../future/direction-and-configuration-surface.md`](../future/direction-and-configuration-surface.md).
> Date: 2026-08-28. Index: [`../INDEX.md`](../INDEX.md).
>
> The designs it sequences remain proposals; this page is the sequencing decision and the definition
> of done for each step.

## The order

| PR | Content | Breaking | Depends on |
|---|---|---|---|
| 1 | The worked examples as an executable corpus. The [#295](https://github.com/ConfigButler/gitops-reverser/issues/295) correctness fixes it was to carry **shipped** in 0.42.1 | no | — |
| 2 | `spec.suspend`, and the reconcile-request annotation | no | 1 |
| 3 | `status.placement`, and the post-scan validation pass | no | 2 |
| 4 | `placement.serializeNamespace` and `placement.kustomizeRoot` ([#322](https://github.com/ConfigButler/gitops-reverser/issues/322)) | no | 1, 3 |
| 5 | `commitWindow` and `commit.message` move off `GitProvider`, as `GitTarget.spec.commit` | **yes** | — |
| 6 | The riders: the asserted CommitRequest author, its lifecycle, `meta.LocalObjectReference`, `TooManyStreams`, the ClusterProvider default message | **yes** | — |

**PRs 1 to 4 are not breaking**, which is the largest change to this plan since it was written. The
model no longer replaces `spec.placement` with a discriminated union; it keeps the template and adds
two members whose defaults equal today's behavior. So there is no loud rejection, no `LocateNew`
rewrite, no migration, and the whole placement story ships without a coordinated consumer bump.

What remains breaking is PRs 5 and 6, neither of which is about placement. They land in one release,
reviewed as two changes and paid for as one bump. PR 6 is the trim handle: nothing depends on it.

**`F9`'s envtest stays outside this order and gates it.** Its answer constrains the enum work, so run
it before PR 4 is planned; the question and the fallback are in [`api-wave.md`](api-wave.md).

## Why the order carries less risk than it did

The placement work reads like a rewrite and never was one. The four-rung ladder is a single function,
[`LocateNew`](../../internal/manifestanalyzer/placement.go), with a single caller in
[`plan_flush.go`](../../internal/git/plan_flush.go). Everything downstream of the path decision —
registering a new file with the kustomization that governs it, the render fidelity gate, refusal
accounting, the metrics — already exists and stays where it is. `LocateNew` is not touched at all.

That leaves the risk in one place: **review surface**, which is why the corpus in PR 1 matters more
than any prose acceptance criteria.

## PR 1 — an executable corpus

**The correctness half has shipped.** Both #295 fixes are on `main` and released in 0.42.1
([#319](https://github.com/ConfigButler/gitops-reverser/pull/319)): a declared path into a
subdirectory is registered with its nearest ancestor kustomization inside the write jail, and
`IdentityCompletePlacementTemplate` no longer demands `{version}`, so the versionless canonical shape
passes our own gate. That is why this plan reversed — the ancestor walk made registration an
invariant. So PR 1 is the corpus alone, and the shipped fixes become the first thing it asserts.

### The corpus harness

[`examples/README.md`](examples/README.md) already has the shape of a golden-file suite —
`repository/`, `config/`, `input/`, `expected-*.patch` — and is read by nobody but a human. Wiring it
up is what stops the design and the implementation drifting, and it converts the PR 4 review from
"does this prose hold together" into "does the diff match the patch". The seam exists:
`newWorktreeForTest` and `flushEventsToWorktree` in
[`internal/git`](../../internal/git/placement_test.go) already do this at a smaller scale.

Per scenario folder: seed a worktree from `repository/` and commit it; build the write event from
`input/`; derive the flush policy from `config/gittarget.yaml`; flush, diff the worktree against the
seed commit, and compare with `expected-*.patch`. For a scenario carrying `expected-status.yaml`
instead of a patch, compare the observation the post-scan pass produced with that file — two scenario
shapes need this and neither can assert a patch: a `Never` declaration with no supplier, and an
ambiguous folder covering two roots.

Three details decide whether this stays maintainable:

- **Normalize the diff.** Blob hashes and index lines are noise; compare paths, modes and hunks. A
  helper that renders a canonical patch from the worktree diff keeps the committed `.patch` files
  reviewable as documents.
- **Normalize the status the same way.** Drop the fields that cannot be stable in a fixture —
  `observedRevision`, `observedGeneration`, timestamps — and compare the remainder as parsed YAML, so
  field order in the fixture is free. Until PR 3 exists there is nothing to capture, so these
  scenarios are skipped for it by name, exactly like the PR 4 cases.
- **Regenerate rather than hand-edit.** A `-update` flag that rewrites the `.patch` files makes the
  corpus cheap to extend, and the review of a regenerated patch is the review of the behavior change.

### The skip protocol

Most scenarios describe behavior PR 4 introduces. Each such case is written now and skipped with the
PR that will unskip it named in the skip message. **PR 4 is finished when the last skip is gone.**
That is the definition of done this plan is built to produce.

The scenarios that assert real behavior immediately are the ones the shipped fixes cover:
`brownfield-kustomize` (the ancestor walk) and any `byType` case pointing into a subdirectory. They
are regression cover for 0.42.1 as much as scaffolding for what follows.

### Config parsing without the API

`config/gittarget.yaml` uses fields that do not exist yet. Parse the scenario config into a small
harness-local struct rather than into `v1alpha3.GitTarget`, and map the fields that do exist onto
today's `PlacementPolicy`. When PR 4 lands, that mapping is deleted and the fixtures unmarshal into
the real type — which is itself a useful check that the API shape the examples describe is the API
shape that got built.

### Done when

- `task lint` and `task test` pass; e2e is unaffected.
- Every scenario folder is loaded by the harness; none is silently unreferenced.
- Non-skipped assertions cover both shipped #295 fixes, so the corpus is regression cover from its
  first commit.
- Every skipped case names the PR that unskips it.
- The corpus gaps below are filled.

## PR 2 — `spec.suspend`, and the reconcile-request annotation

One field and one annotation, which is the smallest this part of the wave has been:
`spec.mode: Observe|Write` and `spec.interval` were both fields here and are both dropped, with the
reasoning in [`api-wave.md`](api-wave.md).

- `suspend` stops resource writes **and** bootstrap creation. State that before either exists.
- `suspend` does **not** stop scanning: a suspended target still resolves its render root and
  publishes `status.placement`, which is what makes it a dry run rather than an off switch. This
  deviates from Flux, so the field's documentation says so in one sentence.
- `reconcile.configbutler.ai/requestedAt` plus `status.lastHandledReconcileAt` refresh that
  observation on demand.

## PR 3 — `status.placement`, and the post-scan pass

The wave put status after the layout field. Inverting that is the highest-value change in this plan:
`renderRoot` and the resolution are facts about the folder that today's placement already computes and
discards. This is the specification of the stanza; the other two documents point here.

```yaml
status:
  observedGeneration: 4
  conditions:
    - type: LayoutResolved
      status: "True"
      reason: SingleKustomization        # SingleKustomization | Ambiguous | None
      message: "render root '.' governs new files"
      observedGeneration: 4
  placement:
    renderRoot: .
    serializeNamespace: Auto             # what it resolved to for this folder
    byTypeEntries: 1
    observedRevision: 9f3c1ab
    observedTime: "2026-07-30T09:14:22Z"
    examples: []                         # capped at three, illustrative, not a tally
```

Three decisions are taken here rather than deferred, because each is cheaper to take before the field
exists than after:

- **The resolution reason is a condition reason, not a field.** `renderRootReason` would have been a
  reason enum in a bespoke field, and shipping it one release before the model that defines it would
  have meant breaking a field sold as stable. Every consumer in this ecosystem already reads reasons
  from `conditions`.
- **No accumulating counters.** `placedResources`, `overriddenTypes` and `refusedResources` are
  metrics; `placements_total` carries them with better labels. A monotonic counter in status is a
  status write per event — a hundred etcd writes a minute on a busy target for something nobody polls
  at that resolution — and it re-creates the self-triggering reconcile edge the status work already
  fixed once. `examples` stays, capped and fixed-size, because "show me where a Secret would land" is
  not a metric.
- **`conditions` and `observedGeneration` are in the stanza**, because every scenario README already
  asserts `Ready=True`.

The current half must never depend on a placement having happened: `renderRoot` is a fact about the
folder from the last scan, available before anything is ever written.

**The post-scan validation pass ships here too**, because it is the same scan. Three rules whose
precondition is a property of the observed folder rather than of the spec, so no CEL rule can reach
them:

| Rule | Precondition |
|---|---|
| `serializeNamespace: Never` requires a namespace supplier | a kustomization with `namespace:` governs the path |
| `kustomizeRoot: Require` needs a root | one governs the path |
| a declared single-root assertion | the folder has exactly one root |

One pass, one condition shape, `Validated=False` naming the offending field and what the folder
actually contains. A corpus scenario per row, with an `expected-status.yaml` instead of a patch.

The pass is written in PR 3 against the two rules that exist today; PR 4 adds the third row when it
adds the field. That ordering is deliberate: the machinery is proven before a new field depends on it.

## PR 4 — `serializeNamespace` and `kustomizeRoot`

Both nest inside `spec.placement` and both default to current behavior, so an object that says
nothing behaves exactly as it does today.

- `serializeNamespace: Auto | Always | Never`. `Auto` is what `namespaceIsInheritedFromContext`
  already does, so the work is the two explicit values plus the `Never` row of the post-scan pass.
- `kustomizeRoot: Adopt | Create | Require`. `Adopt` is the ancestor walk that shipped in
  [#319](https://github.com/ConfigButler/gitops-reverser/pull/319). `Require` is a refusal on the
  existing scan. **`Create` is the only genuinely new machinery in the whole placement story**:
  writing a `kustomization.yaml` that does not exist, with `namespace:` set when the folder is
  single-namespace, and registering into it in the same commit.

Build `Create` last and on its own. It is the one value that writes a file nobody asked for by name,
it is what makes `Never` provable on an empty folder, and it is the only part of this PR that cannot
be verified by pointing the corpus at an existing fixture.

## PR 5 — `commitWindow` and `commit.message` move to `GitTarget`

The last of the principle items, and the one that makes the object coherent: batching and phrasing
describe a folder, and `GitProvider` describes a connection. They land as `spec.commit.window` and
`spec.commit.message.template` rather than two top-level fields — the move is breaking either way, so
the grouping is free ([`api-wave.md`](api-wave.md), "Where the fields live"). Mechanical, and
independent of everything above, which is why it can be written in parallel and merged into the same
release.

## PR 6 — the riders

`CommitRequest.spec.author` with its SAR guard, the CommitRequest lifecycle hole, F12's reference
types, the `TooManyStreams` cap, and the ClusterProvider default message. In the release because they
are breaking and the consumer should pay once. Trim from here first.

**Size `TooManyStreams` against what actually produces the fan-out**, which is the wildcard
source-namespace expansion, not the number of rules. It is the trim handle inside the trim handle.

## Design changes this plan folds in

Each came from reviewing the worked examples. The reversal dissolved four earlier entries — the
namespace agreement rule, `kind: Template` leaving the wave, `Auto` as the CRD default, and
`interval` on two objects — because every field they amended is gone. They are not restated here.

### 1. The post-scan validation class, generalized

Still the right shape, and now it carries three rules instead of one. It lives in PR 3, above, where
the scan that feeds it lives.

The brownfield example is not evidence of the violation, and an earlier draft of this plan said it
was. It declares a namespace inheritance that is valid for its folder; the violation appears only if
the `kustomization.yaml` is later deleted. That is a better illustration anyway, because it is the
argument for observing per scan: the declaration never changed, only the folder did.

### 2. Namespace-local `GitProvider` is a recorded gap, not a blocker

The homelab examples put a `GitTarget` in `argocd`, `flux-system` and `homelab-config`, each needing
its own `GitProvider` — three copies of one credential in a single-owner cluster. `ClusterProvider`
already has the `allowedNamespaces` and fail-closed SAR machinery a cluster-scoped Git provider would need.
Out of scope for this order; file it, and say so in the examples' prerequisites so a reader does not
read the duplication as intended design.

## Corpus gaps to fill in PR 1

- **A refusal scenario.** Every example is a happy path, and the post-scan pass has the least
  coverage. One folder per row of the table in PR 3: a `serializeNamespace: Never` whose governing
  kustomization has no `namespace:`, a `kustomizeRoot: Require` with no root, and a folder covering
  two roots. `expected-status.yaml` in place of a patch.
- **The missing `ClusterProvider`.** `empty-repo-bootstrap` references `clusterProviderRef: app-intent`
  and no such object appears anywhere. It is the intent cluster, which is the whole direction-B
  thesis, so it deserves a concrete specimen beside the cluster-tree one.
- **The prerequisites note** from design change 2.

## What this plan does not decide

- The helm standpoint, decided in the direction review and parked with entry criteria.
- Whether the `byType` map is ever shared across targets. The trigger is in the model: that map is the
  only part with real reuse pressure.
- Whether `kustomizeRoot: CreatePerDirectory` is built. Deferred in the model with its trigger.
- Whether `serializeNamespace: Never` names its supplier.
- The `F9` enum question, which is measured before PR 4 is planned.
