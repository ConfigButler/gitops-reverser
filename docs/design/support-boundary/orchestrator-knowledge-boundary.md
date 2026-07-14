# Orchestrator knowledge boundary: renderability, ownership, and claims

> Status: direction-setting; ships no code. Nothing it describes is supported today.
> Captured: 2026-07-10
> Related:
> [README.md](README.md),
> [kustomize-support-boundary.md](kustomize-support-boundary.md),
> [repo-discovery-and-onboarding-scan.md](repo-discovery-and-onboarding-scan.md),
> [unreflectable-edits-and-write-gating.md](unreflectable-edits-and-write-gating.md),
> [gittarget-granularity-and-cross-environment-edits.md](gittarget-granularity-and-cross-environment-edits.md),
> the layout corpus at [`test/fixtures/gitops-layouts/`](../../../test/fixtures/gitops-layouts/)

## Purpose

This doc answers two questions that turned out to be the same question:

1. **What makes a folder safe to write?** The support boundary today answers
   *"can an edit be mapped back to one source document?"* That is necessary and
   it is not sufficient.
2. **Where does Argo CD / Flux knowledge belong?** It is real knowledge, the
   operator needs it, and it must not be smeared through the acceptance gate.

The answer to (2) falls out of (1): the missing safety property is **ownership**,
ownership is asserted by orchestrator objects that already live in the repository
as ordinary KRM, and so orchestrator knowledge enters the system as a bounded set
of *claims about paths* rather than as a dependency on Argo CD or Flux.

## The evidence

Running `manifest-analyzer --mode scan-repo` across the layout corpus produced a
snapshot (`test/fixtures/gitops-layouts/support-today.md`). Its refusals are
healthy: specific, actionable messages for non-KRM YAML, foreign files, impure
multi-document files, and unsupported kustomize features.

The **accepts** are the finding. Three fixtures written specifically to say *"a
tool must not touch this"* came back green:

| Fixture | Reported | Why that is wrong |
|---|---|---|
| `13-sops-encrypted` | `apps/frontend` accepted, `rendered/editable/nonKrm = 2/2/0` | One of those two "editable" documents is a SOPS Secret. Rewriting it invalidates the `mac`; decryption then fails in-cluster. |
| `16-flux-image-automation` | all three candidates accepted | `apps/frontend` is rewritten and committed by `ImageUpdateAutomation` on an interval, and its image field is pinned by a `$imagepolicy` setter comment. |
| `14-rendered-manifests` | `rendered/production` accepted | The first line of the file is `DO NOT EDIT`. Any edit is destroyed by the next CI render. |

A false refusal costs a support ticket. **A false accept corrupts a user's
repository**, and in the SOPS case does so in a way the user cannot see in the
diff.

## The two axes

All three folders above are perfectly renderable: plain KRM, one document, one
home file, no generator, no patch. They pass every question the gate asks.

They fail a question it never asks.

| Axis | Question | Failure mode | Modelled today |
|---|---|---|---|
| **Renderability** | Can an edit be written back to exactly one source document, such that live → Git → live round-trips? | We cannot express the change. | ✅ Yes — this *is* the [support boundary](kustomize-support-boundary.md). Generators, `namePrefix`, patches, remote bases, Helm inflation are refused because they are non-invertible. |
| **Ownership** | Is this file already owned by something else that writes it? | We *can* express the change, write it, and destroy someone else's invariant. | ❌ No. |

The two are independent. A folder can be renderable and unowned (write it),
renderable and owned (refuse, or write elsewhere), unrenderable and unowned
(refuse), or both (refuse twice).

Renderability is a property of the **folder's own contents**. Ownership is a
property of **something outside the folder** that points at it. That asymmetry is
why ownership cannot be answered by the structure-only walk that answers
renderability, and it is exactly why orchestrator knowledge is needed at all.

### The governing rule, restated

[README §The governing rule](README.md) says every edit must have exactly one
writable destination in Git. This doc adds the second half:

> **…and that destination must not already have a writer.**

## Where orchestrator knowledge lives today

The module has **no Argo CD or Flux Go dependency** — `go.mod` is clean, and the
upstream checkouts under `external-sources/` are gitignored reference material,
not vendored code. That is the right posture and this doc does not change it.

But orchestrator knowledge has still leaked in, through two unrelated side doors
with no seam between them:

| Site | Shape of the knowledge |
|---|---|
| [`internal/sanitize/types.go`](../../../internal/sanitize/types.go) | Hardcoded label/annotation prefixes (`kustomize.toolkit.fluxcd.io/`, `kro.run/`, `applyset.*`) stripped before a document reaches Git. |
| [`internal/manifestanalyzer/scan_repo.go`](../../../internal/manifestanalyzer/scan_repo.go) — `isFleetRoot` | Three directory names: `clusters`, `apps`, `infra`. |

Neither is wrong to exist. Both are in the wrong place, and the second one is
wrong on its own terms.

## The specimen: `fleetRoot`

`fleetRoot` is worth dwelling on, because it shows precisely how this knowledge
leaks when it has nowhere principled to go.

- Its **name** is Flux vocabulary. `fleet-infra` is the repository name in the
  `flux bootstrap` documentation.
- Its **implementation** contains no Flux knowledge whatsoever: it requires three
  top-level directories named `clusters`, `apps`, and `infra`. Any tool can
  produce that tree, and many repositories that are not fleet roots do.
- The **thing it reaches for** — *"this is a cluster entry point, never an editing
  target"* — is real, and the genuine evidence is somewhere else entirely:
  `clusters/<cluster>/flux-system/` containing `gotk-components.yaml` and
  `gotk-sync.yaml`, which `flux bootstrap` writes and nothing else does.

So it carries Flux's vocabulary, none of Flux's evidence, and a name-based guess
that:

- **false-negatives** on the canonical Flux layout in `09-flux-monorepo`, which
  uses `infrastructure/` — the spelling the Flux documentation itself prescribes
  — and therefore reports `fleetRoot: false` on the most Flux-shaped repository
  in the corpus;
- **false-positives** on any repository that happens to use those three names.

Argo CD's equivalent concept is not a directory at all. It is a *document*: a
root `Application` (app-of-apps) or an `ApplicationSet`. No directory-name
heuristic can ever see it.

## The tiering

Four tiers of knowledge. Only the last is the one to isolate.

### Tier 0 — KRM facts

Is this document a Kubernetes object? Is it encrypted? Is this file YAML but not
KRM? Do two documents claim the same identity? Zero tool knowledge. This is the
core, and it is the only tier permitted in the acceptance gate's hot path.

### Tier 0b — Resource semantics

*What may I do to this document?* A value may be unreadable, may live in another
system, or may be owned by a controller. Every such fact is asserted by **the
document itself** — a `sops:` stanza, a `SealedSecret`'s `encryptedData`, an
`ExternalSecret`'s `remoteRef` — which is what separates this from Tier 2, where
the fact is asserted by *another* object.

Four knobs and a lookup table, designed in
[resource-capability-model.md](resource-capability-model.md). The discriminator
turns out not to be encryption but **schema conformance**: a real API server
rejects a SOPS `Secret` and accepts the identical ciphertext inside a CRD field
typed `string`.

### Tier 1 — Renderer facts

Kustomize build semantics: `resources` graphs, render roots, generators,
`namePrefix`/`nameSuffix`, `images`, `replicas`, remote bases.

**This is not Argo CD or Flux knowledge.** Kustomize is a real, versioned engine
whose semantics directly determine round-trippability, both tools drive it, and
the operator's own writer must understand it. It belongs in core, where it
already is.

### Tier 2 — Orchestrator facts

- *This folder is the source of Argo CD `Application` X.*
- *This folder's namespace is injected by Flux `Kustomization` Z's
  `targetNamespace`, and its `${var}` tokens by `postBuild.substitute`.*
- *This folder is written by `ImageUpdateAutomation` Y.*
- *This `.argocd-source-<app>.yaml` outranks the `kustomization.yaml` beside it.*

Tier 2 is what to isolate. And isolating it is cheap, because of one fact:

> **Tier 2 knowledge arrives as ordinary KRM documents in the repository that
> Tier 0 has already parsed.**

An Argo CD `Application` is an object with `spec.source.path`. A Flux
`Kustomization` is an object with `spec.path` and `spec.postBuild`. Reading them
needs their group and kind strings and a handful of field paths. It never needs
Argo CD's or Flux's code.

## The claim vocabulary

Core produces a document store and a set of folder candidates. **Interpreters**
— one per orchestrator — read that store and emit **claims about paths**, in a
closed vocabulary that never names a tool:

| Claim | Meaning | Emitted from |
|---|---|---|
| `RenderRootFor{path, by}` | Something deploys this folder. | Argo `Application.spec.source.path`; Flux `Kustomization.spec.path` |
| `TransformedOutOfBand{path, by, what}` | Namespace or substitution injected from outside the folder. | Flux `postBuild.substitute`, `targetNamespace`; Argo Application-level Helm parameters |
| `WrittenBy{path, by}` | A machine commits to this path. | Flux `ImageUpdateAutomation.spec.update.path`; Argo CD Image Updater write-back |
| `OverriddenBy{path, file}` | A file outranks the visible configuration. | `.argocd-source.yaml`, `.argocd-source-<app>.yaml` |
| `Generated{path}` | Committed render output, not source. | `DO NOT EDIT` headers, `config.kubernetes.io/origin` annotations |
| `NotAnEditingTarget{path, why}` | A cluster entry point. | `flux-system/gotk-sync.yaml`; a root `Application`/`ApplicationSet` |

Core consumes claims. It never learns what "Flux" is. Adding kro, Crossplane, or
Renovate later means adding an interpreter, not touching the acceptance gate.

The vocabulary is not invented — it is read straight off the corpus. `WrittenBy`
is fixtures 16 and 05. `Generated` is 14. `TransformedOutOfBand` is 10.
`OverriddenBy` is 05 and 07. `NotAnEditingTarget` is the honest `fleetRoot`.

Note that **encryption is Tier 0b, not Tier 2.** A SOPS document is
self-describing: the `sops:` stanza is in the file. No orchestrator asserts it.
The analyzer already knows — `Encrypted` and `CauseEncrypted` exist in
[`internal/manifestanalyzer/analyzer.go`](../../../internal/manifestanalyzer/analyzer.go).
The bug is that `scan-repo` never surfaces it (see below). Ownership has two
sources: the document itself, and a claim about it.

Ownership is also not always exclusive, and SOPS is the case that proves it: the
key holder owns *reading* while we can still own *writing*, because encrypting to
an `age` recipient needs only the public key that `.sops.yaml` publishes. That
turns a refusal into a capability, and it is designed in
[write-only-encrypted-secrets.md](write-only-encrypted-secrets.md).

## Architecture rules

**Never depend on `argoproj/argo-cd` or `fluxcd/*` Go modules.** They pull very
large dependency trees and version-lock this module to their Kubernetes
libraries. Match on `apiVersion`/`kind` strings over `unstructured`, with typed
accessors for the few field paths that matter. Keeping upstream source in a
gitignored `external-sources/` is exactly right: consult it as documentation,
never as a dependency.

**Match on group + kind, tolerating version.** You will meet
`helm.toolkit.fluxcd.io/v2beta1` and `v2`, `image.toolkit.fluxcd.io/v1beta2` and
`v1`, in the same repository. An interpreter that pins a version is a bug.

**Do not extract a separate repository or Go module yet.** Make the *package*
boundary real now — `internal/gitops/{argocd,flux}` over a shared claim
vocabulary and registry — and extract only when a second consumer exists. A
premature module split buys release overhead and nothing else. The claim
vocabulary is the contract; the package layout is an implementation detail that
can move.

**Keep ownership *policy* in core, not in the interpreters.** The interpreter
says "`ImageUpdateAutomation` writes this path." The rule "refuse to write a
machine-owned file" is core policy, applied uniformly to every claim from every
interpreter. Invert this and the operator ships Flux-shaped safety with no
Argo-shaped equivalent.

**Interpreters are read-only and cluster-free**, exactly like `ScanRepo`. They
read the parsed store, never the network, never a kubeconfig.

## What this changes in the code

Two of these are small, confirmed defects that stand on their own, independent of
whether the tiering above is adopted.

### 1. `editable` promises ownership and delivers location

[`scan_repo.go`](../../../internal/manifestanalyzer/scan_repo.go) documents
`Editable` as *"the source the operator would own and write in place"* — an
ownership claim — and computes it as `pathWithin(filePath, dir)`, which is pure
location. That is why fixture 13 reports a SOPS Secret as `editable`.

The fix is small and already half-built: surface the existing `Encrypted` count
through `ResourceCounts`, and stop counting non-editable documents as editable.
This is a **contract** fix, not only a code fix: the field is exported through
[`pkg/manifestanalyzer`](../../../pkg/manifestanalyzer).

### 2. `isFleetRoot` uses the wrong evidence

Replace the `clusters` + `apps` + `infra` directory-name test with the real
fingerprint: a `flux-system/` directory containing `gotk-sync.yaml`. Until
Tier 2 exists, that is a one-function change that both fixes the false negative
on `09-flux-monorepo` and stops the false positives.

### 3. A public field worth renaming early

`RepoSummary.FleetRoot` is exported from
[`pkg/manifestanalyzer`](../../../pkg/manifestanalyzer). It should become something
honest — `clusterEntryPoints []string`, or a `NotAnEditingTarget` claim carrying a
reason.

That package used to promise that fields are added and never repurposed or removed.
It no longer does: the project is pre-1.0, and
[`doc.go`](../../../pkg/manifestanalyzer/doc.go) now says so plainly, so a rename
costs nothing on paper.

The real clock is **adoption, not the doc comment**. Every consumer that reads
`fleetRoot` before the rename is a consumer that has to change. Nothing does yet.
The decision should be made before the write path hardens around the name.

### 4. Onboarding, not refusal, for foreign files

`01-argocd-plain` is refused because `ci-metadata.yaml` is non-KRM — a file the
Argo CD `Application` beside it already excludes via `directory.exclude`. Every
simulated repo root in `11-repo-per-environment` is refused for carrying a
`.gitignore`. Real repository roots always carry `.gitignore`, `README.md`, and
`LICENSE`.

The refusal message already names the remedy (`.gittargetignore`). Onboarding
should **propose** that file rather than report a refusal, and an Argo CD
interpreter can seed it from `directory.exclude`. This is a report-quality bug,
not a boundary question.

## Not supported today

None of the following is built, and none is designed here beyond the shape above.

| Capability | Scope |
|---|---|
| **Ownership axis** | `Encrypted`/non-editable in `ResourceCounts`; a core write-gate that refuses a machine-owned or generated destination; `Generated` detection from headers and `config.kubernetes.io/origin`. Fixes the three false accepts. Its first API surface is [`EncryptedSecret`](write-only-encrypted-secrets.md). |
| **Orchestrator interpreters** | `internal/gitops` claim vocabulary + registry; `argocd` and `flux` interpreters; `fleetRoot` re-expressed as `NotAnEditingTarget`; role classification (workload / control-plane / generated / machine-written) in the repo scan report. |
| **Resource capability registry** | Tier 0b: four knobs, three classifiers ([design](resource-capability-model.md)). Carries the derived-object gate below. |
| **Derived-object gate** | Never mirror an object carrying a controller `ownerReference` — it is a controller's output, not desired state. Live-state, Tier 0, no orchestrator knowledge. Today [`sanitize`](../../../internal/sanitize/sanitize.go) *deletes* that field without ever gating on it, so an ESO- or sealed-secrets-derived `Secret` would be committed as a second source of truth. See [sealed-secrets-and-external-secrets.md](sealed-secrets-and-external-secrets.md). |

The derived-object gate stands alone: it is a few lines, it needs nothing else in
this doc, and it protects far more than secrets. The ownership axis is the one
with a correctness argument behind it — the three false accepts above are its
evidence. Interpreters are what make the ownership axis complete, and what turn
the repo scan report into an onboarding answer rather than a folder census.

One further report field comes from neither: `RepoReport` should carry
`requiredCRDs`, the group/kind pairs a repository needs installed before any
cluster can hold its documents. It is computable from `apiVersion`/`kind` alone,
during the scan the analyzer already performs.

## Why the repo scan report is not yet an onboarding answer

Related, and worth recording while the evidence is fresh. `scan-repo` counts
candidates, and every candidate counts the same:

- `02-argocd-app-of-apps` reports four accepted candidates. Two are `bootstrap/`
  and `applications/` — directories of Argo CD `Application` CRs, not workloads.
- `06-helm-chart` reports one accepted candidate: `charts/frontend/crds`, a
  directory *inside* a Helm chart that Helm owns.
- `07-helm-environment-values` reports one accepted candidate: `argocd/`.

Each is structurally true. None is a useful thing to tell a user during
onboarding, and a report that leads with "1 folder supported" on a Helm
repository is misleading rather than honest. Candidates need a **role** beside
their layout, and a Helm chart should surface as an explicit layout rather than
as silence plus an incidental `crds/`.

## Corpus and baseline

Two follow-ups on the corpus itself, which is the test surface for all of the
above:

- **No fixture currently produces `kustomize-overlay` / `overlay-fan-out-unsupported`.**
  Every overlay in the corpus also uses `patches`, so `refused-structural` — the
  permanent wall — fires first and hides the other verdict entirely. The two are
  not interchangeable: `refused-structural` is a permanent refusal, while
  `overlay-fan-out-unsupported` names a folder that is simply not written today.
  [repo-discovery-and-onboarding-scan.md](repo-discovery-and-onboarding-scan.md)
  calls that distinction load-bearing and says it must never collapse; today
  nothing pins it. A minimal overlay that only sets `namespace` and `images` over
  `../../base` closes the gap.
- **`support-today.md` should be generated, not pasted.** A task target that runs
  `scan-repo` over every fixture and rewrites the file turns the corpus from
  documentation into a **behavioural baseline**: not an assertion, a diff. Any
  change to the acceptance gate then shows, in the pull request, exactly which
  fixtures moved and in which direction. That is the cheapest way to keep this
  map honest, and it is the step that makes every fix above reviewable.

## Non-goals

- **No orchestrator emulation.** Interpreters read declarations. They do not run
  kustomize on a remote base, resolve a Helm chart, evaluate an ApplicationSet
  generator, decrypt SOPS, or contact a registry.
- **No new operator dependency.** Interpreters serve the analyzer and the repo
  scan report. Whether the operator ever consumes a claim is a separate decision; the
  ownership *gate* it needs can be fed by Tier 0 (`Encrypted`) alone for the
  first cut.
- **The write boundary does not widen.** Everything refused for renderability
  stays refused. Ownership only ever refuses more.

## Open questions

1. **Where does a claim about a path outside every candidate go?** A Flux
   `Kustomization` in `clusters/production/` claims `./apps/frontend`. Both are
   candidates. Does the claim attach to the target, the source, or the edge?
2. **Is `Generated` detectable without a convention?** `DO NOT EDIT` headers and
   `config.kubernetes.io/origin` are conventions, not guarantees. Is a
   false-negative here acceptable, given the cost is an overwritten edit?
3. **Should `WrittenBy` refuse the folder, or only the fields the machine owns?**
   In `16-flux-image-automation` the automation owns exactly one field — the image
   on the line carrying the `$imagepolicy` comment. Refusing the whole folder is
   safe and coarse; refusing the field is precise and needs setter-comment
   awareness. The same question, in a different costume, as restricted patch
   authoring — write the one field, or refuse the document that holds it.
4. **Does a `RenderRootFor` claim from an Argo CD `Application` change which
   folders are candidates at all**, or only annotate the ones the structural walk
   already found? The structural walk found `manifests/frontend` without help;
   the `Application` merely confirms it. Where do the two disagree?
5. **How is a claim from a *different repository* represented?** An `Application`
   in a bootstrap repo points at a path in a workload repo. The single-repo scan
   cannot see it. Is that simply out of scope, or does a tool built on top of the
   operator stitch claims across repositories?
