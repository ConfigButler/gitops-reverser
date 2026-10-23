# Applier claim rename and doc corrections: work plan

Status: proposal, 2026-10-22. The kind name was settled on 2026-10-23: **`FluxClaim` /
`ArgoCDClaim`**, referenced as `spec.applierClaimRef`. The original review was checked against
`feat/gitreceiver` at `a4928dcf`. The coordinated-folder terminology is already in definitions.md
and bi-directional.md.

This plan folds in three rounds:

- an external review that proposed renaming `FluxTrigger` / `ArgoCDTrigger`, listed four doc
  corrections, and found an ownership caveat;
- that reviewer's follow-up on the first draft, which moved the name from "Applier" to "Binding" and
  corrected C1, the PR scope and O2. See [What the follow-up changed](#what-the-follow-up-changed);
- a naming discussion that weighed active names against Binding and chose Claim, and the
  reviewer's agreement with three qualifications. See
  [What the naming discussion changed](#what-the-naming-discussion-changed).

There are no users yet, so the rename is a **hard cut**: no migration path, no compatibility
period, and no superseded field kept around to reject.

## What to do

**Decided 2026-10-23: one PR, stacked on #437, in separate commits.** #432 has merged, so nothing
forces the doc corrections to land first, and writing them in trigger terms would only have them
rewritten a commit later. The sections below still say "PR A" and "PR B" for the two halves:

**PR A: doc corrections.** C1–C3 and the ownership limitation (O1), written straight in claim
terms. It changes a Go comment and a godoc that flows into the generated CRDs.

**PR B: the rename.** `FluxTrigger` → `FluxClaim`, `ArgoCDTrigger` → `ArgoCDClaim`,
`spec.applyTriggerRef` → `spec.applierClaimRef`, removed outright. C4 is limited to text swaps
(`ClusterProvider` → `KubeProvider`) and deleting the unused SVG; the structural redraw of
config-basics is done by hand in Excalidraw, here or in a follow-up.

**Not in this PR:** a code fix for the ownership limitation (O2/O3). Decide on that separately.

**Since #437** (Argo CD automated sync held off, as a suspend): an `ArgoCDClaim` holds its
`Application` the way a `FluxClaim` holds its `Kustomization`, so the definition below says so, and
**Held** covers both kinds. The Argo CD finalizer `configbutler.ai/argocd-trigger` becomes
`configbutler.ai/argocd-claim`; `argocdtrigger_hold*.go`, `argocdtrigger_apiserver_test.go` and
`applytrigger_holder_test.go` are renamed with the rest. #437's own names stay:
`status.previousSuspendState`, `SuspendState`, `ArgoCDVersionNotSupported` and the
`ClaimedResourceReconciled` Event, which already reads in claim terms.

## The vocabulary

| Word | Means | Status |
| --- | --- | --- |
| **Applier** | The external Flux `Kustomization` or Argo CD `Application` that applies a folder | unchanged |
| **Applier claim** | Our exclusive, persistent claim on one applier: a `FluxClaim` or an `ArgoCDClaim` | replaces "apply trigger" |
| **Apply request** | One ask to an applier: this commit, for this reason | unchanged |
| **Apply operation** | The execution of a request, followed until the applier's status shows it ended | unchanged |
| **Bound** | An applier claim with at least one eligible `GitTarget` | already in [definitions.md:219](../definitions.md#applying-back); says nothing about whether it holds the applier (that is **Held**) |
| **Coordinated** | A folder, or its `GitTarget`, configured for GitOps Reverser to control apply timing through an applier claim | describes the folder's setup; `Bound` still describes the applier claim |

The sentence that ties them together:

> An applier claim declares that GitOps Reverser should take and retain control of when a
> referenced applier applies. A `GitTarget` names one with `spec.applierClaimRef`.

The sentence says "declares", not "gives". An applier claim can exist without control: no eligible
`GitTarget` references it yet, another applier claim holds the applier, or the applier isn't ready.

Proposed entry for [definitions.md "Applying back"](../definitions.md#applying-back), replacing
**Apply trigger**:

> **Applier claim.** This operator's resource that declares GitOps Reverser should take and retain
> control of when one applier applies: a `FluxClaim` (`spec.kustomizationRef`) or an `ArgoCDClaim`
> (`spec.applicationRef`), each with the `KubeProvider` that reaches it. That control covers apply
> timing and nothing else: the applier still fetches, renders, applies, prunes and reports health.
> The claim does not say when to apply either; the branch worker decides that. Among applier claims
> that reach an applier through the same `KubeProvider`, one holds it and the others report
> `HeldByAnotherClaim`; claims made through two `KubeProvider`s for one cluster are not arbitrated
> (see **In line**). Whether a claim holds its applier is **Held**, not **Bound**. While it holds a
> `Kustomization`, a `FluxClaim` keeps it suspended and resumes it only for an apply GitOps Reverser
> asks for, and deleting the claim releases the `Kustomization` back to Flux. While it holds an
> `Application`, an `ArgoCDClaim` keeps automated sync off and starts each sync itself, and deleting
> the claim switches automated sync back as `status.previousSuspendState` recorded it. A `GitTarget`
> names one with `spec.applierClaimRef` and
> reports it in `ApplierClaimReady`. The applier claim is never called the applier: the applier is
> the object it claims.

**Prose rule:** write "applier claim" or the kind name, never a bare "claim". definitions.md already
reserves a bare "claim" for a rule's demand on the read side (`claimed` in code), and defines
**Branch claim** for the Git side. The audit code also talks about OIDC token claims.

Proposed **Coordinated** entry for definitions.md after the rename:

> **Coordinated.** A folder configured for GitOps Reverser to decide when its applier applies.
> Naming an applier claim makes a `GitTarget`'s folder coordinated. The word also describes the
> `GitTarget` that writes it. It describes the setup; `ApplierClaimReady` reports whether the
> applier claim is ready. A shared folder applied without that coordination is **uncoordinated**:
> the applier keeps its own schedule and can revert live edits before they reach Git.

Use these words in the bi-directional guide's decision tree: **coordinated folder** where GitOps
Reverser controls apply timing, and **uncoordinated shared folder** where the applier keeps its own
schedule. Keep the one-way case separate. `Bound` continues to describe the applier claim.

## Why rename, and why Claim

### Why drop "Trigger"

- **These resources don't declare the triggering conditions.** A trigger can be persistent, so
  that isn't the objection. The objection is that the spec only names the external object and the
  connection to reach it. *When* to apply is decided by the branch worker (after a flush, on a
  quiet branch, for a `ForeignCommit` / `Refusal` / `Repair` / `CatchUp`). A name that suggests
  the object holds those conditions sends readers to the wrong place.
- **"Trigger" already means three other things here:** watch-plan triggers
  (`gitopsreverser_watch_plan_triggers_total`, `TriggerRuleChange`), API-surface triggers
  (`internal/watch/api_surface_triggers*.go`), and Argo Events triggers.

### Why Claim

The naming question is **authority over apply timing**. GitOps Reverser takes that authority,
keeps it, and eventually hands it back, while Flux still does the applying. The resource that
establishes this is **exclusive and taken**: among applier claims reaching an applier through the
same `KubeProvider`, one holds it, the others report `HeldByAnotherClaim`, and deleting the holder
releases the applier. That is what a claim is.

- **It fits the lifecycle.** An applier claim is declared, competes for the applier, holds it, and
  releases it. Every step has a plain sentence.
- **It separates intent from success.** A `FluxClaim` is still an applier claim while another one
  holds the `Kustomization`, so `HeldByAnotherClaim` reads naturally. Binding and Control both
  suggest the relationship already exists.
- **It does not limit the authority to timing by itself.** "This is ours for now" could suggest
  broader ownership. The definition sets the boundary, as it would for any name; Claim's advantage
  is the lifecycle and the resource model, not a narrower meaning.
- **The reference structure is a Kubernetes precedent.** Pod → `PersistentVolumeClaim` →
  `PersistentVolume` is `GitTarget` → `FluxClaim` → `Kustomization`. Several pods can share one
  PVC, just as several `GitTarget`s name one applier claim. The workload points at the claim, which
  also settles Binding's main weakness: in a `RoleBinding` the binding names both sides, while here
  the `GitTarget`s point at it. Naming an applier claim from a `GitTarget` is the consent to hand
  over deployment timing. The analogy stops at the structure: a `Bound` PVC has its volume, while a
  **Bound** applier claim only has an eligible `GitTarget` referencing it. Holding the applier is
  **Held**.
- **It is the same idea as the Git side's branch claim.** A `GitBranch` holds a repository branch
  and another reports `BranchConflict`; an applier claim holds an applier and another reports
  `HeldByAnotherClaim`. One word for one idea on both sides.
- **It keeps the four concepts distinct.** Applier, applier claim, apply request and apply operation
  each get their own word. "Applier" keeps its meaning in about 330 places, and `Applier()`,
  `ApplierKind*`, `git.ApplierRef`, `ApplierNotFound` and `ApplierNamespaceNotAllowed` still name
  the external object. `ApplierClaim.Applier()` reads correctly. holder.go's "holder" is simply the
  applier claim that holds the applier.
- **Against:**
  - Neither Claim nor any other short name says "timing"; the only one that does, Scheduler,
    misleads. The definition and the `HELD` column carry it.
  - A bare "claim" already has other meanings here, hence the prose rule.
  - Crossplane users know a claim as a request that gets provisioned, not a takeover of an existing
    object.

### The example

```yaml
kind: GitTarget
metadata: {name: checkout, namespace: team-a}
spec:
  path: apps/checkout
  applierClaimRef: {kind: FluxClaim, name: checkout}
---
kind: FluxClaim
metadata: {name: checkout, namespace: team-a}
spec:
  kubeProviderRef: {name: management}
  kustomizationRef: {namespace: flux-system, name: checkout}
```

```text
$ kubectl get fluxclaims -n team-a
NAME       READY   REASON      HELD        KUSTOMIZATION   OPERATION   AGE
checkout   True    Succeeded   Suspended   checkout        Succeeded   3d
```

The doc sentences become: "Deleting a `FluxClaim` releases the `Kustomization` back to Flux." "Two
applier claims on one `Kustomization`: one holds it, the other reports `HeldByAnotherClaim`."

### Alternatives considered

| | Claim | Binding | Applier | Gate | ApplyControl |
| --- | --- | --- | --- | --- | --- |
| On a `GitTarget` | `applierClaimRef` | `applyBindingRef` | `applierRef` (reads best) | `applyGateRef` | `applyControlRef` |
| Shows the authority | yes: declared, taken, released; the definition limits it to timing | no (the definition does) | yes | yes | yes, but too broadly |
| Keeps "applier" = `Kustomization` / `Application` | yes | yes | **no**: about 330 uses re-read, plus a new generic word | yes | yes |
| Collides with an existing word | bare "claim" (read-side demand); same sense as branch claim | no | `ApplierNotFound`, `ApplierNamespaceNotAllowed` | **Gate** ([definitions.md:321](../definitions.md), about 1,400 lines) | "controller", "reconcile" |
| Fits Argo CD as well as Flux | yes | yes | yes | weaker: nothing is held closed | yes |

- **Binding** (`FluxBinding`, `spec.applyBindingRef`) was the working name until 2026-10-23. It
  describes the relationship but hides that the relationship is exclusive and takes authority, and
  the `RoleBinding` precedent points the reference the wrong way.
- **Applier** (`FluxApplier`, `spec.applierRef`) reads best on a `GitTarget`, but a clearer public
  name produces less clear internal names. It also suggests GitOps Reverser applies manifests
  itself, the way `kubectl apply` or cli-utils' `apply.Applier` does.
- **Gate** (`FluxGate`, `spec.applyGateRef`) is the most accurate description of what Flux sees, and
  Pod `spec.schedulingGates` is a good precedent. But "Gate" is already a defined term (a condition
  `Ready` aggregates), and the acceptance, render-fidelity and write gates are everywhere.
- **Hold** (`FluxHold`) names the mechanism and matches `Held` and holder.go. But "hold" is used
  loosely all over the code and docs ("Git holds a commit", "hold off resyncs", "a route is held"),
  and a Hold that also starts applies reads as a contradiction.
- **ApplyControl / ApplyController / ApplyReconciler** name the authority, but in an operator
  codebase they read as the reconcile loop, not a resource. "An apply control" is awkward as a
  countable noun, and Control suggests authority over what is applied as well as when.
- **ApplyGovernor** suggests rate limits, quotas or policy. **ApplyScheduler** suggests intervals,
  calendars or queued jobs, and understates keeping and handing back control.
- **ApplyCoordinator** fits publishing, requesting and tracking, but understates unilateral control.
  Coordination still supplies **coordinated** for the folder's setup.
- **Lease** has the right single-holder semantics but implies expiry and renewal, and collides with
  `coordination.k8s.io/Lease`, which leader election already uses.
- **Custody** is accurate (temporary authority, handed back) but too unusual for a kind.
- **Link** is a shorter Binding with the same weakness. **Driver** suggests a plugin or binary.
  **Cooperation** suggests shared control.

Whichever words change, **applier**, **apply request**, and **apply operation** keep their meanings.

## PR B: rename inventory

### Public surface

| Current | New | Where |
| --- | --- | --- |
| kind `FluxTrigger`, plural `fluxtriggers` | `FluxClaim`, `fluxclaims` | [fluxtrigger_types.go](../../api/v1alpha3/fluxtrigger_types.go), CRD `configbutler.ai_fluxtriggers.yaml` |
| kind `ArgoCDTrigger`, plural `argocdtriggers` | `ArgoCDClaim`, `argocdclaims` | [argocdtrigger_types.go](../../api/v1alpha3/argocdtrigger_types.go), CRD file |
| `GitTarget.spec.applyTriggerRef` (`ApplyTriggerReference`) | `spec.applierClaimRef` (`ApplierClaimReference`); old field removed, not rejected | [gittarget_types.go:82-93](../../api/v1alpha3/gittarget_types.go); enum at [applytrigger_types.go:43](../../api/v1alpha3/applytrigger_types.go) |
| `onRefusal: Reapply` CEL rule and message ("needs spec.applyTriggerRef, the trigger asked to reapply") | names `spec.applierClaimRef`, "the applier claim" | [gittarget_types.go:36](../../api/v1alpha3/gittarget_types.go) |
| condition `ApplyTriggerReady` and its printer column | `ApplierClaimReady` | [gittarget_apply_trigger.go:19](../../internal/controller/gittarget_apply_trigger.go), [gittarget_types.go:545](../../api/v1alpha3/gittarget_types.go) |
| reason `ApplyTriggerNotFound` | `ApplierClaimNotFound` | [gittarget_apply_trigger.go:23](../../internal/controller/gittarget_apply_trigger.go) |
| reason `HeldByAnotherTrigger` | `HeldByAnotherClaim` | [holder.go:32](../../internal/applier/holder.go) |
| finalizer `configbutler.ai/flux-trigger` | `configbutler.ai/flux-claim` | [fluxtrigger_types.go:17](../../api/v1alpha3/fluxtrigger_types.go) |
| RBAC markers and `config/rbac/role.yaml` (`fluxtriggers`, `/status`, `/finalizers`, `argocdtriggers`) | `fluxclaims`, `argocdclaims` | [applytrigger_controller.go:139-142](../../internal/controller/applytrigger_controller.go), [gittarget_controller.go:151](../../internal/controller/gittarget_controller.go) |
| chart README (CRD list, uninstall commands), `values.yaml:123-130,373`, `values.schema.json:419`, `templates/applier-rbac.yaml` comments | the new kinds; the keys `applierAccess` and `allowCrossNamespace.appliers` **stay** (they are about appliers) | `charts/gitops-reverser/` |
| samples `fluxtrigger.yaml`, `argocdtrigger.yaml`, samples README | `fluxclaim.yaml`, `argocdclaim.yaml` | `config/samples/` |
| e2e CRD wait list | the new CRD names | [Taskfile-e2e.yml:1332](../../test/e2e/Taskfile-e2e.yml) |

These keep their names: the `Held` condition and its reasons, `status.operation`,
`status.repairPending`, `ApplyReason`, `ApplyOperationPhase`, every `apply_*` metric, and the
`applier_kind` label with its values `kustomization` / `application`.

### Internal Go

Rename the files:

- `api/v1alpha3/{applytrigger,fluxtrigger,argocdtrigger}_types.go` → `{applierclaim,fluxclaim,argocdclaim}_types.go`
- `internal/controller/applytrigger_*.go` → `applierclaim_*.go`
- `fluxtrigger_hold*.go`, `fluxtrigger_suspend_apiserver_test.go` → `fluxclaim_*`
- `gittarget_apply_trigger*.go` → `gittarget_applier_claim*.go`
- `internal/git/apply_trigger*.go` → `applier_claim*.go`
- `test/e2e/apply_trigger.go` → `applier_claim.go`

Rename the identifiers:

| Current | New |
| --- | --- |
| `ApplyTrigger` (interface) | `ApplierClaim` |
| `ApplyTriggerReconciler` | `ApplierClaimReconciler` |
| `TriggerKind()` | `ClaimKind()` |
| `ApplyTriggerKindFlux` / `ApplyTriggerKindArgoCD` | `ApplierClaimKindFlux` / `ApplierClaimKindArgoCD` |
| `ApplyTriggerConditionHeld` / `…Configured` | `ApplierClaimConditionHeld` / `…Configured` |
| `ApplyTriggerStatus`, `ApplyTriggerStatusWriter`, `PublishApplyTriggerStatus`, `SetApplyTriggerStatusWriter` | `ApplierClaim…` |
| `ResolveApplyTrigger`, `nameApplyTrigger`, `observeApplyTrigger`, `NewApplyTrigger` | `…ApplierClaim` |
| `FluxTrigger*` / `ArgoCDTrigger*` (`Spec`, `Status`, `List`, `Source`, `Finalizer`) | `FluxClaim*` / `ArgoCDClaim*` |
| `TriggersOfApplier`, `holderTrigger`, `errTriggerReleased`, `applyTriggerLoop`, `applyTriggerVerdict`, `applyTriggerSuspendingInterval`, `GitTargetConditionApplyTriggerReady`, `ReasonApplyTriggerNotFound` | `ClaimsOfApplier`, `holderClaim`, `errClaimReleased`, `applierClaimLoop`, `applierClaimVerdict`, `applierClaimSuspendingInterval`, `GitTargetConditionApplierClaimReady`, `ReasonApplierClaimNotFound` |
| test helpers (`testFluxTrigger`, `createFluxTrigger`, `reconcileTrigger`, `fluxTriggerResource`, …) | `…Claim` |

**Keep:** every `Applier*` identifier, since the applier is unchanged.

**Do not rename** these; they are different concepts: `TriggerRuleChange`,
`TriggerAllRuleChange`, `watchPlaneTriggers`, `WatchPlanTriggersTotal`, `crdTriggerGVR`,
`apiServiceTriggerGVR`, `ensureAPISurfaceTriggerInformers`, `selectAPISurfaceTriggers`,
`internal/watch/api_surface_triggers*.go`.

**Do not collide with** the existing claim identifiers: the branch claim (`BranchClaims`,
`BranchClaimant`, `NewBranchClaims`, `Claims()`), a `CommitRequest` claiming a window, the
read-side `claimed` demand, and OIDC token claims in the audit code. Every new identifier says
`ApplierClaim`, `FluxClaim` or `ArgoCDClaim`, never a bare `Claim`. The one exception is
`ClaimKind()`, a method on `ApplierClaim`, where the receiver already says which claim it is.

### Docs

- **[definitions.md "Applying back"](../definitions.md#applying-back):**
  - Replace **Apply trigger** with the entry above.
  - Keep **Applier**, and change "An apply trigger names it" to "An applier claim names it".
  - Update **Eligible**, **Bound**, **In line** and **Held** ("trigger" → "applier claim").
  - Update **Coordinated** to the entry above, using the renamed field and condition vocabulary.
  - In **Branch claim**, extend "never a bare 'claim'" to mention that the applier claim is the
    cluster side of the same idea.
- **definitions.md "Words we do not use":**
  - Add "trigger, apply trigger (for an applier claim) → applier claim".
  - Update the "fence" row, which says "what a trigger does to its applier".
- **The configuration heading:** rename [Who applies this folder (`spec.applyTriggerRef`)](../configuration.md#who-applies-this-folder-specapplytriggerref)
  to "Who applies this folder (`spec.applierClaimRef`)". That changes the anchor, so fix the 12
  links: 8 in configuration.md, and one each in UPGRADING, api-first-publication,
  interpreting-metrics and design/apply-trigger.
- **Guide pages:** about 1,550 lines mention the trigger names. Most are mechanical. Read these by
  hand, because they mix "trigger" and "applier" in the same sentences, and a bare "claim" must not
  slip in:
  - `bi-directional*.md` (about 180 trigger mentions)
  - `architecture.md`
  - `configuration.md`
  - `rbac.md`
  - `security-model.md`
  - `interpreting-metrics.md` (the `apply_requests_total` row says "names the trigger")
- **`docs/design/*` and `docs/reviews/*`:** leave as the historical record. Add one line at the top
  of [apply-trigger.md](../design/apply-trigger.md) saying the kinds are now applier claims.
- **[UPGRADING.md](../UPGRADING.md):** add a short entry, nothing more:

  > **`FluxTrigger` and `ArgoCDTrigger` are now `FluxClaim` and `ArgoCDClaim`.** Breaking, with no
  > migration path. Before upgrading, delete every `FluxTrigger` and `ArgoCDTrigger` (a
  > `FluxTrigger` hands its `Kustomization` back to Flux), then delete their CRDs. After upgrading,
  > recreate them under the new kinds with the same `spec`, and on each `GitTarget` replace
  > `spec.applyTriggerRef` with `spec.applierClaimRef`.

  Local e2e clusters: run `task clean-cluster` once after pulling PR B.

### C4. Redraw the diagrams

- [config-basics.excalidraw.svg](../images/config-basics.excalidraw.svg) is shown in
  [configuration.md:29](../configuration.md) and [quickstart.md:11](../quickstart.md).
  - It shows `ClusterProvider` and has no `GitBranch` or `KubeProvider`.
  - The SVG embeds its Excalidraw scene, so edit it in place.
  - Draw: `WatchRule` → `GitTarget` → `GitBranch` → `GitProvider` → `Secret`; `GitTarget` →
    `KubeProvider` (`local`, greyed out as "usually exists"); a dashed optional row `GitTarget` →
    `FluxClaim` → `Kustomization`; and `GitReceiver` ⇢ `GitProvider` ("wakes by spec.url").
  - Update the alt text in both docs.
- [overview.excalidraw.svg](../images/overview.excalidraw.svg) (README.md:42) still says
  `ClusterProvider`. Change it to `KubeProvider`.
- `config-cluster.excalidraw.svg` isn't referenced anywhere. Delete it.

## Ownership: one holder per KubeProvider, not per cluster

[holder.go:36-39](../../internal/applier/holder.go) decides that two triggers name the same applier
by `TriggerKind()`, `KubeProviderKey()`, and the object's namespace and name. Two `KubeProvider`s
that reach **one** cluster (for example the chart's `local`, plus a team's own provider with a
kubeconfig for the same API server) therefore give two holders for one `Kustomization`. Both
suspend it under the same field manager. Each reads the other's resume as a `flux resume` and
suspends it mid-apply. Deleting one hands back a hold the other still needs. These are exactly the
failures the comment at the top of holder.go lists. For an `Application`, both start syncs.

This is a misconfiguration, not an attack, so it falls outside the "don't design against spoofed
identity" decision. Renaming doesn't change it, though "claim" makes the gap easier to state: an
applier claim is exclusive only among claims made through the same `KubeProvider`.

- **O1 (PR A): document it.** Add a paragraph to
  [Who applies this folder](../configuration.md#who-applies-this-folder-specapplytriggerref) and to
  the **In line** entry in definitions.md: *"One holder per `Kustomization` or `Application` is
  enforced among triggers that reach it through the same `KubeProvider`. GitOps Reverser can't tell
  that two `KubeProvider`s reach the same cluster, so name one `KubeProvider` for every trigger that
  reaches a given cluster."*
- **O2 (later): compare cluster identity. This is partial protection.**
  - The `KubeProvider` records the cluster's identity in status (the `kube-system` Namespace UID,
    read once at validation), and `sameApplier` compares that instead of the provider key.
  - Where the credential can't read that Namespace, it falls back to the provider key, and the O1
    limitation still applies.
  - Cost: `get` on `namespaces/kube-system`, plus a note at
    [architecture.md:389](../architecture.md) ("reads no `Namespace` objects in a source cluster")
    that this read is about identity, not authorization.
- **O3 (later, complete): an object-local stamp.**
  - Write the holder's identity into the `Kustomization` in the same server-side apply as the
    suspend, and refuse to adopt over a live stamp that isn't ours. This also catches two operator
    installs on one cluster.
  - Cost: two first adopters can race, so arbitration has to work across operators, and an
    `Application` needs a write of its own.

## PR A: doc corrections

PR A ships before the rename, so its text uses the current names. PR B renames them with
everything else.

### C1. Qualify "the Kubernetes API is the source of truth"

[architecture.md:53](../architecture.md). The rule is still right for GitOps Reverser's own
pipeline. What's missing is how Git edits reach the cluster, and that the applier's echoes are kept
out of Git (see **Echo**, [definitions.md:249](../definitions.md#applying-back)). Replace the first
two sentences with:

> **The Kubernetes API is the source of truth for what GitOps Reverser writes.** Git is a
> materialized mirror of desired state from the API, ingested by **watch**; GitOps Reverser never
> reads Git as object state. An edit made in Git reaches the cluster only through Flux or Argo CD.
> When a `GitTarget` names an apply trigger, those applies are coordinated: GitOps Reverser first
> publishes the API edits it has decided, then asks the applier to apply. The watch observes the
> result, and the applier's own writes during that apply (its echoes) are not mirrored, because
> Git already holds them.

Keep the existing sentence about push conflicts after it.

### C2. Describe KubeProvider as a connection and an identity

Today the docs call it the source cluster, but a trigger uses it to **write** (suspend, resume,
sync) in a cluster that may be a management cluster.

- [architecture.md:569](../architecture.md): replace "the read-side peer of `GitProvider`" with "the
  Kubernetes-side peer of `GitProvider`: a connection to one cluster and the identity used there.
  What it's used for depends on who names it: a `GitTarget` reads its source cluster through it,
  and an apply trigger reaches its `Kustomization` or `Application` through it, often in a
  management cluster."
- [kubeprovider_types.go:196](../../api/v1alpha3/kubeprovider_types.go): change "KubeProvider names
  a source cluster: the cluster a GitTarget mirrors from, or an apply trigger reaches its applier
  in" to "KubeProvider is a connection to a Kubernetes cluster and the identity used there. A
  GitTarget mirrors from it; an apply trigger reaches its applier through it." Then run
  `task manifests`, since this godoc flows into the CRD description.
- [definitions.md:46-49](../definitions.md#the-two-clusters-and-the-two-directions), **Source
  cluster**: change "A `KubeProvider` names one" to "A `GitTarget` names it through its
  `KubeProvider`". Add a **KubeProvider** entry: "A connection and identity, not a role. The same
  provider can be a `GitTarget`'s source cluster and an apply trigger's management cluster."
  Change "`spec.inCluster` means the control cluster, acting as a source" to "… the control
  cluster, reached with the operator's own identity".

### C3. Remove the obsolete multi-worker text

Since #428 (`c49803f3`) a branch worker is keyed by repository and branch, and
[branch_owner.go](../../internal/git/branch_owner.go) lets exactly one `GitBranch` write a
repository branch. `apply_sibling_flush.go` is gone.

- [configuration.md:1470-1471](../configuration.md): delete "The branch workers of other
  `GitProvider`s writing the same repository and branch flush too, and the request waits until
  they have."
- [bi-directional.md:434](../bi-directional.md): delete the bullet "An edit through another
  `GitProvider` on the same repository and branch …". That exposure no longer exists.
- [internal/git/apply_trigger.go:382](../../internal/git/apply_trigger.go): delete ", and so does a
  sibling's flush" from the comment.

## Validation and acceptance

Both PRs run the full sequence: `task fmt` → `task generate` → `task manifests` → `task vet` →
`task lint` → `task test` → `task test-e2e`. Push once lint and unit pass. A PR is done when
e2e passes, in CI or locally (sequentially).

**PR A is done when:**

- the full sequence passes;
- `task lint-docs` passes (anchors and links);
- the regenerated KubeProvider CRD description shows the C2 wording.

**PR B is done when:**

- the full sequence passes, **plus** `task test-e2e-bi-directional`, because the Flux and Argo CD
  specs only run in that corner;
- `git grep -niE 'fluxtrigger|argocdtrigger|apply.?trigger'` returns nothing outside `docs/design`,
  `docs/reviews`, `docs/UPGRADING.md`, `CHANGELOG.md` and `external-sources/`;
- `git grep -niw 'trigger'` over `docs/*.md` returns only the unrelated senses (watch plan, API
  surface, Argo Events) and the "Words we do not use" row;
- definitions.md contains no entry that calls the applier claim a trigger or an applier, and no
  bare "claim" for it.

## What the follow-up changed

- **The name moved from Applier to Binding**, to keep applier / apply binding / apply request /
  apply operation distinct. The first draft's D1 ("held object" as the generic word) is gone with
  it; it also mixed identity with lifecycle state.
- **The argument against "Trigger" is now** that these resources don't declare triggering
  conditions, not that a trigger sounds fire-and-forget.
- **C1 no longer says the applied result is mirrored "like any other change".** Echoes are not
  mirrored.
- **C4 moved to PR B only** (the first draft had it in both). PR A now lists e2e in its validation.
- **O2 is marked as partial protection.**
- **The migration section is gone.** There are no users yet. The follow-up's points about
  sequencing a handoff and about allowing a temporary compatibility mechanism would apply if that
  changes.

## What the naming discussion changed

2026-10-23.

- **The name moved from Binding to Claim:** `FluxClaim` / `ArgoCDClaim`, generically "applier
  claim". Binding kept the four concepts distinct but hid the authority. Claim keeps them distinct
  and shows that the resource exclusively takes an applier and releases it on delete.
- **The field is `spec.applierClaimRef`, not `applyClaimRef`.** "Apply claim" can read as a claim
  that something was applied; "applier claim" is a claim *on* the applier, and keeps "applier" for
  the external object.
- **The active-name families were rejected.** ApplyControl, ApplyController, ApplyReconciler,
  ApplyGovernor, ApplyCoordinator and ApplyScheduler each either collide with operator vocabulary
  or describe the wrong mechanism; see [Alternatives considered](#alternatives-considered).
- **The prose rule now guards a bare "claim"** instead of a bare "binding", because definitions.md
  already gives a bare "claim" to the read side's demand and defines the branch claim.
- **The reviewer agreed with Claim and qualified three arguments**, all taken:
  - Claim does not limit authority to timing by itself; the definition does. Its advantage is the
    lifecycle (declare, compete, hold, release) and that it separates intent from success.
  - The definition states the scope of exclusivity (applier claims through the same
    `KubeProvider`) instead of "one applier claim holds an applier at a time", which overstated O1.
  - **Bound** stays distinct from **Held**. The PVC analogy explains the reference structure only.
- **The defining sentence says "declares … should take and retain control"**, so an unreferenced,
  conflicting or not-yet-ready applier claim is still described correctly.
