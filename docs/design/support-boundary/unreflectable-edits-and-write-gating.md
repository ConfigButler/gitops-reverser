# Unreflectable edits: honest accounting and the admission preflight gate

> Status: direction-setting — designed, not built (no code change; extends
> [kustomize-support-boundary.md](kustomize-support-boundary.md) §5)
> Captured: 2026-07-06
> Related:
> [README.md](README.md),
> [finished/images-and-replicas-edit-through.md](finished/images-and-replicas-edit-through.md),
> [../unsupported-folder-refusal-plan.md](../../spec/unsupported-folder-refusal-plan.md)

## Problem

The write-fan-in invariant (never write a file consumed by more than one
render root) makes base files and all other shared context **read-only**.
That is the right call — it is what keeps an edit in test from changing what
production renders. But it has an unavoidable consequence: **some fields of a
hydrated object can no longer be changed through the Kubernetes API** — or
more precisely, the change persists in the cluster but has no legal
destination in Git.

With the reference layout (base + test/acceptance/production overlays) — where
`images:`/`replicas:` edit-through is supported today and overlay **patch
authoring** is not — the unreflectable classes are concrete and enumerable:

1. **Out-of-scope field edits on a base-owned object.** Overlay patch authoring
   (writing a scalar-field strategic-merge patch into the overlay) is designed
   but **not supported today**, so *every* per-environment field edit on a
   base-owned object currently lands in this class — it is the largest one. Even
   with patch authoring, anything beyond scalar fields (structural list surgery
   on unkeyed lists, atomic-list semantics, map-key removal) still has no
   destination.
2. **Per-environment delete of a base-owned object.** Expressible only as a
   `$patch: delete` patch — whether patch authoring ever covers it is a scope
   decision; until it does, unreflectable. (A rename is a delete plus a create:
   the create half is always fine, the delete half is this class.)
3. **Divergent consumers of a shared override entry** (a known limitation of
   `images:`/`replicas:` edit-through): one `images:` entry cannot hold two tags.
4. **Transformer-supplied metadata**: a label or annotation owned by a
   tolerated-but-unmodeled metadata transformer (`commonLabels`, `labels`,
   `commonAnnotations`, `buildMetadata`) cannot be edited per environment.
5. **Read-only context beyond the sibling base.** In-repo shared bases
   anywhere in the repository (e.g. `platform/common-base`) are reachable
   read scope, never write scope. Remote URL bases stay refused at
   onboarding — we cannot even *read* them deterministically.

The question this document answers: **what happens when a user makes one of
these edits anyway?**

## Principles

1. **Never write through.** The invariant is absolute; a fallback that edits
   shared context is worse than any amount of refusal.
2. **Never drop silently.** An edit the operator cannot reflect must become
   a visible fact, not a quiet divergence.
3. **Never punish the folder for one edit.** Structural facts about the
   *folder* may refuse the whole GitTarget (that gate exists today); a
   single runtime *edit* must never stall every subsequent write to the target.
4. **Feedback belongs near the actor.** The best place to learn "this cannot
   be saved" is at `kubectl apply` time; the second best is on the
   CommitRequest a caller already polls; a GitTarget condition is the
   backstop, not the primary surface.
5. **Correctness must not depend on optional layers.** Any admission-time
   gate can be down, stale, or disabled; the system underneath it must
   already be honest.

## Three tiers of "no"

The two ideas on the table — an admission webhook that refuses unsavable
writes, and a GitTarget state that reports misuse — are not competitors.
They are different tiers of one escalation, each scoped to what it is good
at, plus the structural gate that already exists:

| Tier | Scope | Mechanism | Status |
|---|---|---|---|
| **1. Onboarding refusal** | whole folder, structural facts | acceptance gate: `GitPathAccepted=False`, `Stalled=True` on unsupported constructs | shipped |
| **2. Per-edit accounting** | one object/field, runtime edits | the **unreflected set** + `FullyReflected` conditions | designed, not built — the load-bearing answer, and a prerequisite for overlay support |
| **3. Admission preflight** | one API request, pre-persistence | opt-in validating webhook; rejects writes that would leave residue; **fail-open** | designed, not built |

```mermaid
flowchart TD
    E[Sanitized watch event in env X] --> D{Legal destination<br/>in overlay X?}
    D -->|images / replicas entry| K[Edit overlay kustomization]
    D -->|overlay-local document| P[In-place file edit]
    D -->|patch-expressible field<br/>on base-owned object| S[Author / update overlay patch]
    D -->|new object| N[New overlay file + resources entry]
    D -->|none| U[Record residue in unreflected set<br/>write nothing]
    K --> C[Commit on the target's branch]
    P --> C
    S --> C
    N --> C
    U --> R[FullyReflected=False on<br/>GitTarget + CommitRequest]
    R --> H[Standing, honest drift report<br/>cleared if the render is re-applied]
```

Note the fifth branch: a single API write can contain both reflectable and
unreflectable fields. The writer reflects what it can (override-entry
edit-through already routes mixed changes field-by-field) and records the
remainder — see the atomicity caveat under tier 3.

## Tier 2: the unreflected set (load-bearing)

**Definition.** The unreflected set is the standing sanitized diff between
live state and the folder's render, per GitTarget, annotated with the
supplier/reason that made each residue unwritable. It should be recomputable:
the mark-and-sweep resync rebuilds it from scratch, steady-state events add
and remove entries incrementally, and no durable store is needed (consistent
with "watch supplies state"). But this is real implementation work, not a
status-only rename: the projection must know which fields came from shared
context, overlay-local files, override entries, or transformer-owned metadata.

**Surfacing.**

- **GitTarget** gains a `FullyReflected` condition (`True` when the set is
  empty) plus a bounded status summary (count + a capped sample of
  `object, field, reason`), in the style of `status.streams`. `Ready` is
  unaffected — the target still works; this is a report, not a failure.
- **CommitRequest** gains the same `FullyReflected` condition scoped to the
  saved window: "everything you asked to save was expressed in the commit."
  This is the primary surface for a caller — a tool built on the operator
  already polls the CommitRequest for `Pushed` + `status.sha`, so "the commit
  landed, but these 2 edits could not be expressed" is one more condition on
  an object it already reads.

**Convergence.** What happens to the residue depends on whether anything
re-applies the folder's render to the cluster, and in both cases the honest
report is our whole job:

- **Nothing re-applies the render** (the operator mirrors a cluster that is
  itself the source of truth): the residue simply persists. The condition is
  then a standing, honest statement — "live state exists that this folder
  cannot express." That is a drift report, not an error, and it must never
  block the cluster's own operations.
- **Something re-applies the render** (a drift-correcting GitOps controller,
  or a layer above the operator that hydrates the cluster from Git): the
  residue is reverted — the unsavable part of the buffer is discarded, like
  an editor refusing to keep an invalid character. `FullyReflected` returns to
  `True` on its own, gate or no gate.

**Clearing.** An entry clears when live equals render again — whether by a
re-applied render, by drift correction, or by the user undoing the edit. No
timer, no manual ack.

## Tier 3: the admission preflight gate

An opt-in validating admission webhook, registered on CREATE/UPDATE/DELETE in
the namespaces claimed by gated GitTargets. It evaluates the same projection
the writer uses — "would this write leave residue?" — and rejects with an
actionable message naming the supplier:

> `spec.template.spec.containers[0].env` on `Deployment/podinfo` is supplied
> by `apps/podinfo/base/deployment.yaml`, which is shared by all
> environments. This edit cannot be saved for `podinfo-test` alone. (Bump
> images/replicas via the overlay entry; other per-env fields need patch
> support / a base change via a normal Git PR.)

Design choices, each answering a concern raised against the webhook idea:

- **Fail-open (`failurePolicy: Ignore`), because tier 2 backstops it.** The
  gate is a UX accelerator, not a correctness layer. If it is down, edits
  land, tier 2 reports them, and a re-applied render reverts them. This is
  what makes "slows things down / could be annoying" acceptable: nothing
  depends on it.
- **Scoped, so the latency worry stays small.** It gates only the claimed
  namespaces on a cluster that exists to be edited *through* the operator; no
  unrelated workload traffic crosses it. Evaluation is in-memory against the
  manifest store — no Git round-trip.
- **Staleness is tolerated, not solved.** The decision uses a snapshot of
  the analyzer store that can race a concurrent push. A wrong *allow* is
  caught by tier 2; a wrong *deny* is a retry. Neither corrupts anything.
- **Free preflight via dry-run.** With `sideEffects: None` the gate runs on
  `kubectl apply --dry-run=server` — "can I save this?" becomes a native
  Kubernetes question any caller can ask before the user commits.
- **The real argument for building it is atomicity, not speed.** Tier 2
  reflects field-by-field: a single apply that bumps a tag *and* edits an
  unreflectable field saves the tag and loses the edit — a mixed outcome
  the user never expressed. Admission is the only point where intent can be
  rejected *whole*, before persistence. (This is the same pre-persistence
  property that makes admission wrong for attribution — see
  [architecture.md](../../architecture.md) — used here for exactly what it
  is good at: prevention. Admission prevents, audit attributes, watch
  supplies state.)
- **Never on a cluster the operator merely mirrors.** We do not get to reject
  a cluster's own operations because our mirror of it is lossy. The gate is
  off by default and enabled per GitTarget (e.g. `spec.writeGate: Enforce |
  Off`) — appropriate only where the cluster is an *editing surface* whose
  changes are meant to land in Git, never where the cluster is the source of
  truth.

## Why not a GitTarget-wide "unsupported mode"

The alternative considered — the GitTarget drops into a degraded state when
an unreflectable edit occurs — mixes two different kinds of fact:

- **Structural facts** are stable, folder-level, and human-fixable
  ("this folder uses Helm"). Whole-target refusal is right, exists today
  (tier 1), and stays.
- **Runtime edits** are transient, object-level, and often self-healing (a
  re-applied render reverts them minutes later). Escalating them to a
  target-wide state punishes every subsequent write for one edit, moves the
  feedback far from the actor, and flaps: `Stalled` would toggle as the
  residue is reverted.

So the target-wide idea survives as the *reporting* half of tier 2 — a
`FullyReflected=False` condition with a bounded sample — while never
degrading `Ready` and never blocking other writes. Clear *and*
non-invasive, rather than a trade between the two.

## Reference-layout walk-through

Every row assumes the reference layout (`base/` + `overlays/{test,acceptance,
production}`) and the shipped narrow render-root scope acting in `podinfo-test`.
Existing overlay-local documents and declared image/replica entries are editable;
new-object `resources:` entry creation and overlay patch authoring are not.

| Action in `podinfo-test` | Outcome |
|---|---|
| `kubectl set image deploy/podinfo podinfo=…:6.6.1` | reflected → `images:` entry in `overlays/test/kustomization.yaml` |
| `kubectl scale deploy/podinfo --replicas=5` | reflected → `replicas:` entry |
| set an env var / resource limit on the base-owned Deployment | with **overlay patch authoring**: reflected → `overlays/test/podinfo-deployment.patch.yaml`; without it: **unreflected** — reported, and reverted wherever the render is re-applied |
| `kubectl apply -f new-cronjob.yaml` | **unreflected today** — overlay-local file placement plus a `resources:` entry needs the planned write-path correction |
| edit the test-only debug-toolbox object | reflected → in-place edit of `overlays/test/debug-toolbox.yaml` |
| delete the base-owned Service in test only | **unreflected** until patch authoring covers `$patch: delete` → reported, reverted where the render is re-applied (the preflight gate rejects it up front) |
| change a label supplied by `commonLabels` | **unreflected** (transformer-owned metadata) |
| hot-bump one of two Deployments sharing one `images:` entry | **unreflected** (divergent consumers — a known limitation of override-entry edit-through) |

These rows are the intended residual surface for that layout. The definition of
done for overlay support must prove that surface with a corpus and e2e cases;
any new residual class either gets a legal destination or joins this table with
a clear reason. That is the support statement the operator can make — each
unsupported row has a designed answer (report + revert, optionally reject up
front) rather than undefined behavior.

## Consequences

- **Tier 2 is a prerequisite for overlay support**, not a separate feature:
  overlay support without the unreflected set would reintroduce silent
  divergence, which principle 2 forbids. Patch authoring is not supported, so
  per-environment edits of base-owned fields are the largest unreflected class
  exactly where overlays are used.
- **Tier 3 is independent of patch authoring** — opt-in, fail-open, and only
  for a cluster that is an editing surface. Its value is *highest while patch
  authoring is absent*: per-env edits of base-owned fields are then the largest
  unreflected class, and the gate is what tells the user so at apply time
  instead of after the save. It needs the same projection tier 2 needs, so its
  marginal cost is the webhook plumbing, not new analysis.
- **Patch-authoring scope decisions move rows between tables.** Each capability
  added to patch authoring (`$patch: delete`, map-key removal) deletes a row
  from the unreflected classes. The classes list above doubles as its backlog,
  priced by how often each row is hit in practice — a metric tier 2's
  accounting can emit.
