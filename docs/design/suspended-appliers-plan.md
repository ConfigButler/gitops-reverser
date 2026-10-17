# Suspended appliers: the plan

> **design, being built.** Steps 1 to 4 are built. Written 2026-10-17 against `6efbf4d0`, the head of #419, and built
> as a stack on top of it (branch `feat/suspended-appliers`); the two release together. Inputs: the
> third review of #419 (five findings, summarized in the table below) and
> [`flux-suspend-lifecycle.md`](flux-suspend-lifecycle.md) (the measurement). It settles the
> direction, the API shape and the order of work, and ends with the decisions taken on it. Once
> built, it is folded into [`apply-trigger.md`](apply-trigger.md), as the Argo CD plan was.
> Index: [`../INDEX.md`](../INDEX.md).

## The direction in one paragraph

**The applier never applies on its own. GitOps Reverser starts every apply.** Argo CD already works
this way: automated sync is off, and every sync is one we asked for. Flux gets the same contract by
keeping its `Kustomization` **suspended while idle**. To apply, GitOps Reverser resumes it, watches
that one reconcile finish, and suspends it again. Today's setup only asks Flux to apply sooner than
its 24h interval would. The new one hands Flux's apply moment to GitOps Reverser completely. In the
API, each kind of applier gets its own trigger resource (`FluxTrigger`, `ArgoCDTrigger`, later
`WebhookTrigger`), referenced by the `GitTarget`. The trigger's in-flight operation, the repairs it
owes and its suspension are durable state, and that state needs a status of its own. "Applier" keeps
its meaning from [`../definitions.md`](../definitions.md): the `Kustomization` or `Application`
itself. The trigger is our resource that holds it.

## Why the direction changes

### What the 24h setup leaves open

A 24h interval makes Flux apply less often. It does not make Flux wait for us. The current guide
([`../flux.md`](../flux.md)) still leaves all of these applying at moments nobody checked:

| Autonomous apply | Today, with 24h | Suspended |
| --- | --- | --- |
| The interval itself fires | Once a day, on a branch that may be mid-edit | Never: a suspended reconcile returns before it applies |
| kustomize-controller restarts | Re-applies whatever artifact it holds. The only reason `CatchUp` is a safety requirement | Nothing applies |
| A failed apply retries after `retryInterval` | Not gated on a quiet branch; [`../flux.md`](../flux.md) documents the hole | GitOps Reverser re-suspends after a failure; the worker retries on a quiet branch |
| The `Kustomization` spec changes (a parent re-applies it, someone edits it) | Applies immediately | Nothing applies |
| The `GitRepository` gets a new artifact (its own interval, a stamp for a sibling consumer) | Every consumer applies (finding 3) | Every suspended consumer ignores it (measured: 0 of 16 runs) |
| Someone runs `flux reconcile ks` | Applies | Marked handled with no apply (measured) |

That table is the "24h bug". It is not one defect. It is that the setup was never exclusive, and
the docs describe it as if it were.

### What the measurement settled

[`flux-suspend-lifecycle.md`](flux-suspend-lifecycle.md) measured the lifecycle on the e2e
cluster. The objections it answers are the ones that would have stopped this direction:

- **Cost:** a fixed ~50 ms per apply at 1 and at 200 objects. Noise next to the apply itself, and
  invisible behind a 30s `quietPeriod`.
- **Isolation:** a suspended consumer ignores new artifacts. That removes finding 3 for every
  consumer we suspend.
- **Completion:** resuming changes the generation, and that reconcile can be identified exactly. That
  removes finding 1 without the `lastAttemptedRevision` guess, which ends about 0.8 s early per 200
  objects.
- **Fetch-only mode:** with every consumer suspended, the source can fetch on every push. The design
  page's lesson "Flux has no fetch-only mode" stops being true, so push webhooks can reach the source
  again.
- **A bug in today's code (point 3):** `Handled()` treats a `requestedAt` as handled on a suspended
  `Kustomization` that applied nothing. In the lifecycle below that same behavior becomes the fence
  that proves quiescence.

### Source facts the lifecycle relies on

Each was read in kustomize-controller **v1.9.6** (`internal/controller/`) and flux2 **@1ed703a3**,
both checked out under `external-sources/flux/`. These replace guesses. The protocol below is built
on them, and none should be re-derived.

| Fact | Where | What it gives us |
| --- | --- | --- |
| `finalizeStatus` is deferred and copies `reconcile.fluxcd.io/requestedAt` to `status.lastHandledReconcileAt` on **every** return: suspended, failed, dependency-not-ready, succeeded | `kustomization_controller.go:143`, `:1335–1340` | A token is handled exactly when the reconcile that read it ends, whatever the outcome |
| A suspended object returns right after the finalizer check, before resolving the source | `:189–192` | Suspended means nothing is fetched, built or applied |
| `spec.suspend` is read only when a reconcile starts | `:189` | Suspending does not stop an apply that is already running (measured too) |
| `lastAttemptedRevision` is written before the build | `:390` | Confirms finding 1a |
| `observedGeneration` advances only when `Ready` is True (or on a terminal error) | `:1345–1348`, `:201`, `:258` | Not usable alone as a completion signal: a failed apply leaves it behind |
| A failed apply requeues after `retryInterval`; a dependency not ready requeues with `Ready=False, DependencyNotReady` | `:311`, `:264–268` | Both are distinguishable outcomes. A requeue into a suspended object does nothing |
| The `Kustomization` is reconciled on a generation change **or** a new `requestedAt`; source revision changes enqueue it too | `kustomization_manager.go:139–173` | One merge patch that sets both `suspend` and `requestedAt` is guaranteed to enqueue a reconcile that reads both |
| A dependent needs its dependency's `observedGeneration == generation` and `Ready` | `:572`, `:618–624` | A dependency we suspend after a failed apply blocks its dependents until it succeeds (cost, below) |
| controller-runtime runs at most one reconcile per object at a time | workqueue contract | A token handled by a later reconcile proves every earlier reconcile of that object has ended |
| `flux resume ks` is a merge patch of `spec.suspend: false`, then a wait on readiness | flux2 `cmd/flux/resume.go:183–245` | Our resume is `flux resume` with a token added |

## The Flux operation, suspended

### One operation, every reason

Today the Flux client has three paths: stamp the source, stamp the `Kustomization`, or `Current`.
Each has its own completion rule. Under suspension every reason is the same operation, because a
resume alone applies (measured: it reverted a drifted object with no new commit):

1. **Decide** on a quiet branch, as today. Nothing about quietness changes.
2. **Make the artifact hold the commit.** If it does not, stamp the `GitRepository` and wait until
   it has handled that token. With prefetch (below) the artifact usually holds the commit already.
   If the artifact ends on another commit, the result is `Stale`, and the worker decides again.
3. **Recheck just before resuming:** the decision still holds (`ApplyRequest.Current`, as today),
   *and* the artifact still holds the commit. That closes the race in measurement point 6 as far as
   it can be closed (see residual risk).
4. **Record the operation** durably in the trigger's status: `phase: Requested`, the commit, the
   reason and the token.
5. **Resume with a token.** One merge patch sets `spec.suspend: false` and
   `reconcile.fluxcd.io/requestedAt: <T1>`. The patch enqueues a reconcile that reads both, so the
   reconcile that handles T1 is an unsuspended one, which applies.
6. **Wait for `lastHandledReconcileAt == T1`.** This is the completion signal. It fires when that
   reconcile ends, whether the apply succeeded or failed (every return runs `finalizeStatus`). Then
   read the outcome:

   | Read after T1 is handled | Outcome |
   | --- | --- |
   | `Ready=True`, `lastAppliedRevision` holds the commit | `Succeeded` |
   | `Ready=True`, another revision | `Succeeded`, but not the commit we checked; logged and counted (residual risk) |
   | `Ready=False`, reason `DependencyNotReady` | `Failed`, a retryable failure with a distinct reason |
   | `Ready=False`, anything else | `Failed` |

7. **Suspend with a fence.** One merge patch sets `spec.suspend: true` and `requestedAt: <T2>`.
   Wait for `lastHandledReconcileAt == T2`. Reconciles of one object never overlap, so T2 being
   handled proves two things: any reconcile that started before the suspend (a requeue, a source
   event) has ended, and the object is suspended now. This is measurement point 3 used on purpose. A
   suspended reconcile handles a token without applying, and that is exactly the proof needed.
8. **Close the window** 10 s after T2 is handled (the watch grace, unchanged), and report to the
   worker.

A failed apply is suspended again in step 7 like any other, which is what stops Flux's own
`retryInterval` retry. The worker owns the retry and decides it on a quiet branch.

`Current` remains only for `ForeignCommit` and `CatchUp`: the artifact holds the commit **and**
`lastAppliedRevision` is that commit. While suspended, nothing can have applied since, so the check
is sound. `Refusal` and `Repair` always run the operation.

### Operation states

These are shared by every applier kind, in worker memory and in the trigger's `status.operation`:

| Phase | Meaning | Window | Next attempt to this applier |
| --- | --- | --- | --- |
| `Requested` | Recorded; the resume (Flux) or the operation (Argo CD) has been sent | Open | Waits |
| `Running` | The applier is seen working on it (Flux: not suspended, T1 not handled; Argo CD: `Running`) | Open | Waits |
| `Settling` | Flux only: applied, the suspend fence (T2) not handled yet | Open | Waits |
| `Unresolved` | The poll budget ran out, or a restart found work with no completion | Open; `Ready=False` with reason `OperationUnresolved`, logged, counted | Waits; polling continues with backoff |
| `Succeeded` / `Failed` | Completed, and for Flux suspended again | Grace (10 s), then closed | Allowed (a `Failed` one after backoff) |

Separately, a trigger can owe a **`Repair`**: a late apply was withheld (finding 2), so the cluster
runs an older value than Git. It is decided like a `Refusal`: on a quiet branch, with no quiet
period, and always running the operation.

`Unresolved` is never closed by a timer. That was finding 4: the timer closed the window but did not
stop the apply. It resolves when its token is handled, or when the trigger is deleted, or when no
`GitTarget` names it any more. For Flux, an `Unresolved` operation is suspended at once (the suspend
does not stop the running apply, but it does stop whatever would follow), and the fence then decides
when the window closes. There is no automatic cancellation. The condition message says how to clear
it.

### Who owns `spec.suspend`

- **Adoption.** Once a `FluxTrigger` is configured, GitOps Reverser suspends the `Kustomization`. It
  applies `spec.suspend` server-side under its own field manager, so `managedFields` record who owns
  it. The trigger is not `Ready` until the first fence is handled.
- **The manifest omits `suspend`.** Whatever applies the `Kustomization` object (a parent
  `Kustomization`, Helm, flux-operator) must not set the field, or it takes ownership back on its
  next apply. Validation reads `managedFields`. If another manager owns `f:spec.f:suspend`, the
  trigger is `Configured=False` with reason `SuspendOwnedElsewhere`, naming the manager.
- **Resumed by someone else.** `flux resume` is the documented manual override. The trigger
  reconciler finds the `Kustomization` unsuspended with no operation recorded. It treats that as
  `Unresolved` (the window opens, because an unchecked apply is running), suspends it with a fence,
  and records a `Normal` Event. It does this every time, with an Event each time: there is no
  step-back state (decision 3).
- **Hand-back.** The trigger carries a finalizer. Deleting it waits out any operation, then resumes
  the `Kustomization` (`suspend: false`, the field released), so Flux owns it again. A `GitTarget`
  that stops referencing the trigger releases nothing. The trigger object is what holds the
  `Kustomization`.

### Shared sources and prefetch

With suspension, finding 3 becomes a **precondition** instead of a race:

- The trigger reconciler lists the consumers of its `GitRepository` in the applier's cluster:
  `Kustomization`s with that `sourceRef`, and `HelmRelease`s whose chart comes from it.
- **Every consumer is suspended by one of our triggers:** the source is free. A stamp wakes nobody,
  and **prefetch** is on. When the worker observes a push (the receiver, a rejected compare-and-swap,
  the refresh), it stamps the `GitRepository`. The rule: no prefetch while any consumer of that source
  has an operation that is not completed, because a resumed consumer applies whatever the artifact
  becomes.
- **Some consumer is not ours:** `status.source.otherConsumers` lists it. Prefetch is off, and a
  source stamp still applies that consumer, as today. It is information only (decision 2): it never
  affects `Ready`, and no `Warning` Event is sent, because one `GitRepository` for `apps` and `infra`
  is a normal setup. A consumer that no trigger names applies a folder that has no promise of
  protection anyway.
- If listing is forbidden, the precondition cannot be checked, so it counts as not holding.

Flux appliers on one branch that read one `GitRepository` form a group. A group has one operation at
a time, because the artifact is shared.

### Restart recovery

Before live events for a covered `GitTarget` are mirrored, the worker reads each trigger's
`status.operation`:

- **Not completed:** reopen the window and resume polling the recorded token (T1 or T2). For Flux, if
  the object is unsuspended, finish with the fence.
- **No record, but the `Kustomization` is unsuspended.** Somebody resumed it while we were down;
  a crash cannot cause this, because the record is written before the resume. `Unresolved`, then
  the fence.
- **`repairOwed` recorded:** restore it.

The first observation is still a baseline, as today. That rule stays.

### What goes away

- `OutcomeStampedKustomization`, `OutcomeStampedSource` as an apply, the `lastAttemptedRevision`
  heuristic in `Handled()`, and the 5-minute "delivered anyway". The point-3 bug goes with them,
  because a token is now read only as part of a fence.
- `CatchUp` stops being a **safety** requirement, because a suspended `Kustomization` never applies
  a stale artifact. It stays as **convergence** (decision 4 below). Flux's inventory learns about an
  object only when it applies a commit that holds it. An object created through the API and never
  applied by Flux is not in the inventory, so a later Git-side deletion of it would not be pruned.
  `quietPeriod` stays with it.
- The guide's "keep the `Kustomization` unsuspended" and "a long interval as backstop". With the
  `Kustomization` suspended, the intervals no longer decide anything about applies. The
  `GitRepository` interval can be short again when every consumer is ours.

## The Argo CD side

Automated sync off is already Argo CD's equivalent of suspended, and that does not change. Findings
2, 4 and 5 apply to it unchanged: the `Repair` obligation, `Unresolved` instead of a timeout, and
restart recovery from `status.operation`. Its completion signal (the operation state that carries
our token, `Succeeded`/`Failed`/`Error`) is already exact. It has no fence step: an Argo CD
operation that has completed is not followed by another we did not start.

## The API: typed trigger kinds

### Shape

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: FluxTrigger
metadata: { name: apps, namespace: team-a }
spec:
  clusterProviderRef: { name: management }        # where the Kustomization lives; default "default"
  kustomizationRef: { name: apps, namespace: flux-system }
  quietPeriod: 30s                                # CatchUp only
status:
  conditions:                                     # Ready, Configured, Suspended
  source:
    gitRepository: flux-system/apps-repo
    artifactRevision: main@sha1:…
    otherConsumers: [flux-system/infra]           # consumers no trigger suspends
  operation:
    id: 42
    reason: Refusal                               # ForeignCommit | Refusal | CatchUp | Repair
    commit: 3f2a…
    phase: Running                                # Requested | Running | Settling | Unresolved | Succeeded | Failed
    resumeToken: "2026-10-17T12:00:00.123Z"       # T1
    suspendToken: ""                              # T2
    resumedGeneration: 17
    requestedAt: …
    finishedAt: …
    appliedRevision: main@sha1:3f2a…
  repairOwed: false
---
kind: ArgoCDTrigger
spec:
  clusterProviderRef: { name: management }
  applicationRef: { name: apps, namespace: argocd }
status:
  conditions: […]                                 # Ready, Configured (AutomatedSyncEnabled, …)
  operation: { …, phase: Running, token: …, argoPhase: Running }
  repairOwed: false
---
kind: GitTarget
spec:
  applyTriggerRef: { kind: FluxTrigger, name: apps }  # same namespace, like gitProviderRef
  onRefusal: Reapply
```

### Why the trigger is its own resource

1. **It names the unit the code already uses.** One `applierState` per applier: one attempt in
   flight, its own backoff, its own window. Today that state has no object. It lives in worker memory,
   and each `GitTarget` shows only its last request.
2. **Suspension needs an owner object.** Who holds the `Kustomization` suspended, and who resumes it
   on hand-back, has to be one object with a finalizer. A `GitTarget` cannot be that object, because
   several can name one trigger.
3. **The fixes need a durable status.** The operation record (restart recovery), `repairOwed`, the
   source group and the consumer check all need a home that `kubectl get fluxtriggers` can show.
4. **Declared once.** `quietPeriod` is today "the longest among the targets naming it", and
   `clusterProviderRef` is per target even though it describes the applier. Both move to the trigger.
5. **Typed kinds remove the union.** The CEL rules "exactly one of" and "no `quietPeriod` with an
   `applicationRef`" disappear. Each kind gets its own schema, status and RBAC. The webhook trigger
   becomes a third kind, not a third branch. This follows Flux's own convention:
   `sourceRef: {kind: GitRepository|OCIRepository|…}`.
6. **It fixes the e2e gotcha.** `status.applier` refreshes today only when the `GitTarget`
   reconciles. A status written on every operation transition is current by construction.
7. **It is free now.** `spec.applier` has never been released.

### Roles do not move

- **The branch worker decides when.** Quietness belongs to the branch. The worker sends operations and
  writes `status.operation` and `repairOwed` on each transition. That replaces the dispatcher's
  in-memory ledger.
- **The dispatcher executes.** It runs one operation and reports transitions back. It never retries.
- **The trigger reconciler holds and validates.** It adopts (suspends), validates (source, consumers,
  automated sync, field ownership, RBAC), handles external resumes and hand-back, and publishes
  `status.source`. It never starts an apply.
- **The `GitTarget` keeps consent** (`onRefusal`, by uid) and an `ApplyTriggerReady` condition
  that mirrors its trigger's, so `kubectl get gittarget` still says whether it is wired.

### What it costs

- One more object to create, one more reconciler, and RBAC for two kinds now and three later.
- `clusterProviderRef` is on the trigger, since "the target's own" stops meaning anything once
  several targets reference one trigger. Omitted, it is the `default` provider, exactly as a
  `GitTarget`'s is (decided 2026-10-17, after step 3 first made it required). The trigger's namespace is gated by that
  `ClusterProvider`'s `accessFrom`, and a `GitTarget` may reference only a trigger in its own
  namespace. That is simpler than today's per-target `authz.ApplierAdmitted`.
- New RBAC in the applier cluster: list `kustomizations` and `helmreleases` cluster-wide, for the
  consumer check. Without it, prefetch is off.
- `GitTarget.status.applier` goes away. Per-target reasons stay in Events.

## Costs of the direction, stated plainly

- **While GitOps Reverser is down, Flux applies nothing.** Today a 24h backstop runs. Suspended, a
  Git-side push waits until GitOps Reverser is back, or for a manual `flux resume` (after which
  GitOps Reverser fences and re-suspends on start). This is the same trade Argo CD with automated sync
  off already makes. It is the price of exclusivity, and the docs say so.
- **Flux tooling shows `Suspended`.** `flux get ks` reports it, and `flux reconcile ks` does nothing.
  The guide explains both and names `flux resume` as the override.
- **A dependency that failed stays behind.** A `Kustomization` we suspend after a failed apply keeps
  `observedGeneration` behind its generation. Its dependents (`dependsOn`) wait until it succeeds.
  This is arguably correct, but it is new, and the guide says so.
- **The manifest of the `Kustomization` must omit `suspend`.** This is checked, not just documented.
- **Residual race.** Between the artifact recheck (step 3) and the reconcile reading the artifact,
  someone else can stamp the `GitRepository`. With prefetch held off during operations, that
  "someone" is only a person or the source's own interval tick. The outcome table catches it (another
  revision applied), and the apply window still withholds a late applier write over a newer edit.
- **A slow apply holds the applier.** `wait: true`, health checks and `timeout` all lengthen
  `Running`. That is correct (the apply can still revert edits), and it is visible in the status.

## Work plan

Every step is a commit on `feat/suspended-appliers`, stacked on #419, and the two release together
(decided 2026-10-17). Each step starts with the
reviewer's regression probe, or a measurement point, written as a test that fails first. `task lint`
and `task test` pass at every step, and each step is pushed when green.

**Step 1. Record a first-event tie (finding 5).** In `withholdLateApply`
([`apply_window.go`](../../internal/git/apply_window.go)), a write by the applier and another manager
in the same second records the object as edited whether or not it was before. It counts as
`ambiguous` only when it was. Test: a tie first, then an applier-only write a second later, is
withheld. Independent of everything else.

**Step 2. The operation model in the worker (findings 2 and 4).** Split today's single
`ApplyDelivered` into transitions: accepted, running, settling, completed. Add the `Unresolved`
phase, with a short timeout per poll and `apply_operations_unresolved`. Add the `Repair` obligation,
set by a `withheld` collision, and the `Repair` reason (an enum addition, free while unreleased).
Argo CD benefits at once. Tests: the reviewer's Argo CD sequence (sync 2, edit 3, rollback withheld)
ends with a second, pinned sync; a timeout keeps the window open; an `Unresolved` operation resolves
on its token.

**Step 3. The trigger kinds.** `FluxTrigger` and `ArgoCDTrigger` types, CRDs, RBAC and deepcopy.
`GitTarget.spec.applyTriggerRef`; drop `spec.applier` and `status.applier`; add `ApplyTriggerReady`.
Move the validation in `gittarget_applier.go` to the trigger reconciler. Have the worker resolve
appliers through their triggers and write `status.operation`.
Update [`flux_guide_test.go`](../../internal/applier/flux_guide_test.go),
[`argocd_guide_test.go`](../../internal/applier/argocd_guide_test.go), the schema tests and the e2e
helpers. Review the generated CRDs with the `k8s-crd-design-review` skill.

As built, step 3 settled four things the plan left open:

- **The references are immutable** (`clusterProviderRef`, `kustomizationRef`, `applicationRef`):
  the trigger holds what it names, so a re-pointed trigger would leave the old applier held by
  nothing. Delete and recreate it; step 4's finalizer hands the old one back.
- **The binding check is split.** The trigger checks its applier alone (exists, a source GitOps
  Reverser can drive, writable, automated sync off) and publishes `status.source`; reasons for the
  source alone are `UnsupportedSource`. Each `GitTarget` compares that source with its own
  repository and branch, so `SourceMismatch` is the `GitTarget`'s `ApplyTriggerReady` reason. The
  dispatcher still checks the binding against the applier itself before every write.
- **The worker publishes through a writer, never from its loop.** `applier.StatusWriter` coalesces
  per trigger and updates the status from a fresh read, so the reconciler's conditions are never
  overwritten. The record is published once the request is reported `Requested`; step 4 must write
  it before the resume instead, for restart recovery to rely on it.
- **`fenceToken` and `appliedRevision`** are in `status.operation` already, for step 4 to fill.

**Step 4. The suspend lifecycle.** A new Flux client that does adoption, the resume token, the
outcome table, the suspend fence and failure re-suspension, with the recheck before resume. Delete
the request paths listed under "What goes away". The trigger reconciler checks `suspend` ownership,
handles external resumes and hand-back through the finalizer. Unit tests against a fake that applies
the source facts above: a token is handled on every return, a suspended object ignores artifacts,
reconciles never overlap. e2e: the Flux corner and the manager specs run with suspended
`Kustomization`s; a refusal is reverted by a resume alone; a `retryInterval` retry does not fire
after a failure.

**Step 5. Restart recovery.** On start, read `status.operation` and reopen windows, as described
above. e2e: restart the manager in the middle of a 200-object apply; the late write over a newer edit
is withheld, and the `Kustomization` ends up suspended.

**Step 6. Consumers, groups and prefetch.** The consumer list (`status.source`), operation groups per
`GitRepository`, and prefetch from the worker's observations when the precondition holds. Tests: the
reviewer's A/B sequence (an edit to B during A's fetch keeps its value), and a push to the receiver
that leaves an artifact ready while nothing applies.

**Step 7. Docs.**

- [`../flux.md`](../flux.md): rewritten around suspension: what the manifest must omit, intervals,
  `flux resume` as the override, the costs above.
- [`apply-trigger.md`](apply-trigger.md): the Flux lessons are corrected.
  "Flux has no fetch-only mode" now holds only for an unsuspended consumer, and `CatchUp` becomes
  convergence rather than safety.
- [`../bi-directional.md`](../bi-directional.md) and
  [`../configuration.md`](../configuration.md): the new kinds.
- [`flux-suspend-lifecycle.md`](flux-suspend-lifecycle.md): marked built, and
  folded into `apply-trigger.md` as the Argo CD plan was.
- [`../INDEX.md`](../INDEX.md).
- `UPGRADING.md`: only the `PushEmptyCommit` removal, which is still the breaking change users see,
  since `spec.applier` was never released.

As built, step 4 settled these, where the plan above was silent or measured otherwise:

- **Every write of `spec.suspend` is a forced server-side apply** under the field manager
  `gitops-reverser`, with the token in the same apply: the resume, the fence, the adoption and the
  hand-back (an empty apply, which releases both fields). `SuspendOwnedElsewhere` is an entry of
  another manager with operation `Apply` owning `f:spec.f:suspend`: a manifest applied again and
  again. An `Update` entry (`flux resume`, `flux suspend`, `kubectl edit`) wrote the field once, and
  the next apply takes it back.
- **The worker records before it sends.** It takes the token (`ApplyRequest.Token`) and publishes
  the `Requested` operation as it sends the attempt; the requester writes it through
  (`ApplyRequest.Record`, `StatusWriter.PersistApplyTriggerStatus`) after the recheck and before the
  resume, and resumes nothing when that write fails. An attempt that ends without asking for an
  apply puts the operation before it back. The writer never replaces an operation with one requested
  earlier.
- **`observedGeneration` is not used, so there is no `resumedGeneration`.** `finalizeStatus` copies
  the generation whenever `Ready` is True at the end, which includes a suspended return with `Ready`
  left over from the last apply. The outcome is read from `Ready` and `lastAppliedRevision` once the
  resume's token is handled. A token somebody stamped over ours stands in for it.
- **`Current` also needs `spec.suspend: true`.** A Kustomization the trigger has not adopted yet
  can still apply on its own, so the operation runs, and its fence suspends it.
- **A fetch that ends on another commit is a failure,** retried with backoff, unless the decision no
  longer holds, which is `Stale`. The source cannot fetch the commit the worker checked.
- **An external resume is recorded by the reconciler** as an `Unresolved` operation with a fence
  token and no request token, completed once the fence is handled. The worker opening its apply
  window for it reads `status.operation`, which is step 5's restart recovery. A Kustomization found
  resumed after the fence of a worker's own operation is fenced again by that operation. A
  `FluxTrigger` that is Ready reads its Kustomization every minute, which bounds how long a
  `flux resume` runs before it is suspended again.
- **Hand-back:** an operation whose trigger is being deleted completes as `Failed` and writes
  nothing more; the finalizer suspends with a token if the Kustomization is resumed, waits until it
  is handled, then releases. A Kustomization or `ClusterProvider` that is gone, or a release the
  identity may not make (a `Warning` Event), lets the trigger go.
- **"Logged and counted"** is `apply_revision_mismatches_total{applier_kind}`.
- The fake is `internal/applier/fluxsim`, tested against the source facts on its own.

Steps 1 and 2 do not depend on the API. Steps 4 to 6 need step 3's status. Step 4 is the large one,
and the step to run a local e2e on before pushing (AGENTS.md, "When to push").

## Decisions

Decided 2026-10-17: the work is a stack on #419 (`feat/suspended-appliers`), released together
with it, so `spec.applier` is never released and gitops-api sees one API, not two.

The open decisions were settled the same day:

1. **Suspension is the only Flux mode,** the way "automated sync off" is the only Argo CD mode. A
   second, request-based mode would keep every hole in the 24h table alive, along with two
   completion protocols.
2. **A shared source is information only.** `status.source.otherConsumers` lists the source's
   other consumers, and prefetch is off while it is not empty. It never affects `Ready`, and no
   `Warning` Event is sent: one `GitRepository` for `apps` and `infra` is a normal setup. There is no
   `SharedSource` condition.
3. **An external resume is always fenced and re-suspended,** with a `Normal` Event each time. There
   is no step-back state: the person who runs `flux resume` on purpose gets one checked apply, and a
   tool that keeps resuming gets an Event per resume.
4. **`CatchUp` stays for Flux,** as convergence: inventory adoption, so that a later Git-side
   deletion is pruned. The same inventory question exists for Argo CD, which was decided without
   `CatchUp`. Noted here, not reopened.
5. **The resources are triggers.** "Applier" keeps its meaning in
   [`../definitions.md`](../definitions.md): the Flux `Kustomization` or Argo CD `Application`
   itself. Our kinds are `FluxTrigger` (`spec.kustomizationRef`) and `ArgoCDTrigger`
   (`spec.applicationRef`); "Argo" alone would collide with the triggers of Argo Events. A
   `GitTarget` names one with `spec.applyTriggerRef: {kind: FluxTrigger | ArgoCDTrigger, name}` in its
   own namespace, and carries an `ApplyTriggerReady` condition. `spec.applier` was never released, so
   it is replaced outright, with no migration.
