# The apply trigger

> **design**: decided 2026-10-16, extended 2026-10-17; built: the Flux applier (its
> `Kustomization` held suspended), the Argo CD applier (automated sync off), the trigger kinds that
> hold them, the push receiver, `onRefusal: Reapply` and the apply window. The webhook applier is
> the aim. Index: [`../INDEX.md`](../INDEX.md).
> How to use it: [`../bi-directional.md`](../bi-directional.md),
> [`../bi-directional-flux.md`](../bi-directional-flux.md) and
> [`../bi-directional-argocd.md`](../bi-directional-argocd.md).
> Fields: [`../configuration.md`](../configuration.md#who-applies-this-folder-specapplytriggerref).

**GitOps Reverser decides when Git is applied to the cluster, and the applier never applies on its
own.** Flux and Argo CD keep fetching, rendering, applying, pruning and reporting health. A Flux
`Kustomization` is held suspended while it is idle and resumed for each apply; an Argo CD
`Application` runs with automated sync off. Every apply is one GitOps Reverser started, at a moment
it checked.

This page is why, what we learned building it, and what is still open. "Applier" keeps its meaning
from [`../definitions.md`](../definitions.md): the `Kustomization` or `Application` itself. The
**trigger** (`FluxTrigger`, `ArgoCDTrigger`) is GitOps Reverser's resource that holds one, and a
`GitTarget` names it with `spec.applyTriggerRef`.

## The problem

Two API edits to one field, a few seconds apart, are enough. GitOps Reverser pushes the first
commit. Flux fetches it, the second edit lands, and Flux applies the first commit over it. GitOps
Reverser faithfully mirrors that revert as Flux's commit, Flux applies the second commit, and the
branch ping-pongs until a fetch happens to land on the tip. There was no conflict to decide: both
edits came from the API side. gitops-api hit exactly this in production (two saves, then four
reconciler-authored commits).

The cause is not a bug in either tool. Each apply was correct; it ran at the wrong moment.

## The principle

**An apply is safe only when nothing live is missing from the commit being applied.** Between an
edit and its push, the edit is live and absent from Git: in an open commit window, or in the
writes waiting to be pushed. Only the branch worker can see those. So only it can say when an apply
is safe, and every trigger the applier runs by itself fires without that knowledge.

An apply is started only on a **quiet branch**: no open commit window and no pending writes. Four
things owe one:

| Reason | When | Applied by |
| --- | --- | --- |
| `ForeignCommit` | The branch gained a commit GitOps Reverser did not push. Open windows are closed early and pending writes replayed onto it and pushed first, so the applied commit carries the API side's answer | Every applier on the branch |
| `Refusal` | An edit had no legal destination in Git and the target opted in with `onRefusal: Reapply` | The refusing target's applier |
| `Repair` | The apply window withheld a late apply over a newer API edit, so the cluster runs an older value than Git | The applier that wrote it |
| `CatchUp` | The branch has been quiet for `quietPeriod` after GitOps Reverser's own pushes, or since it started | Every Flux applier on the branch |

## What we learned

Each of these changed the design, and each is the reason for a rule the code now follows.

**Filtering webhooks is not enough; every timer is a trigger.** The first ask was to forward only
foreign pushes to the applier. But Argo CD's timed refresh syncs every new tip two to three minutes
after each push, with `selfHeal: false` too, and Flux applies on every interval. Neither can be
filtered from outside, so the applier's own triggers go, and GitOps Reverser makes the requests.

**A long interval is not exclusive control.** The first Flux applier kept the `Kustomization`
unsuspended on a 24h interval and asked it to reconcile. Flux still applied when that interval
fired, when kustomize-controller restarted (from whatever artifact it held, which is why `CatchUp`
was once a safety rule), when a failed apply retried, when the `Kustomization`'s own spec changed,
and on every new artifact of its `GitRepository`, including one another consumer's request fetched.
None of those waits for a quiet branch. The third review called it best-effort coordination, and it
was. A suspended `Kustomization` does none of them, so it is held suspended, and resumed for one
apply at a time ([below](#the-flux-applier-suspended)).

**Flux has no fetch-only mode, unless its consumers are suspended.** A new `GitRepository` artifact
makes every unsuspended `Kustomization` reading it apply at once. A suspended one ignores it
(measured: 0 of 16 runs attempted the new revision). So a source whose every consumer is held can
fetch on every push, and push webhooks can reach it again, through GitOps Reverser's receiver.

**Not every commit GitOps Reverser authors is a no-op to apply.** A replayed `F + W` carries
somebody else's `F`, and a refusal needs an apply to be reverted. So the decision is state on the
worker ("this applier is owed an apply"), set by observations and cleared by an operation that
succeeded. A push notification decides nothing by itself; it is a hint: the worker reads the remote
and judges from that, which gets late, duplicate and rolled-back deliveries right.

**A decision has to hold when the write is sent as well as when it is made.** The first review found
that a request decided once was attempted for minutes against whatever the cluster had become: a
target its `ClusterProvider` no longer admitted still drove the applier, and a source that followed
another branch was still stamped. Every attempt now reads each target again, and the client checks
that the source fetches this repository and branch, and reads the artifact again, just before the
write that starts the apply.

**A retry is a decision too, so only the worker may make one.** The second review found every
remaining problem in the hand-off between the worker's decisions and the deliverer's retries: a
retry skipped the quiet check, outlived a refusal that had recovered, ran for a recreated target, or
waited on another applier's backoff. The fix was structural rather than four patches. The worker
owns every obligation until an operation that succeeded completes it. The dispatcher carries out one
attempt and reports back; it never retries, and never asks for something weaker than it was told. A
retry is decided like a first attempt: on a quiet branch, at the tip as it is then.

**An apply is an operation, and its end is observed, never guessed.** The third review found the
apply window closing early in three ways: kustomize-controller writes `lastAttemptedRevision`
before it builds anything (measured: about 0.8 s early per 200 objects); an artifact that held the
commit was taken as applied; and a 5-minute timer reported an apply delivered while it could still
run. Each apply is now one operation with phases, followed until the applier's own status proves it
ended. One past its budget is `Unresolved` and still followed, because a timer that closes the
window does not stop the apply. The operation is recorded on the trigger before the apply can start,
so a restart finds it.

**Four relations, four units.** One branch worker serves many `GitTarget`s, many `GitTarget`s can
name one trigger, and one `GitRepository` can feed many `Kustomization`s. Keeping each concern on
the right unit is what makes the 1:n cases fall out:

| Unit | Owns | Why |
| --- | --- | --- |
| The **branch** (repository and branch) | Quietness | A `Kustomization` applies the tip whichever folder an edit is in. Two `GitProvider`s naming one repository give one branch two workers, so a worker is quiet only when its siblings are too |
| The **applier**, held by its trigger | Obligations, retries, the operation | One operation at a time, its own backoff, one for every `GitTarget` naming the trigger. The trigger's status records it, and its `quietPeriod` is declared once |
| The **source** (cluster and `GitRepository`) | One operation at a time across its appliers | The artifact is shared: a sibling's fetch landing between an operation's recheck and its reconcile would apply another commit |
| The **`GitTarget`**, by uid | Configuration and consent | Consent to a `Refusal` is per target, and a target deleted and recreated under the same name inherits nothing owed to the old one |

## The design, as built

```mermaid
flowchart LR
    subgraph WORKER["Branch worker (one per GitProvider and branch)"]
        OBS["Observations<br/>push · fetch · notification"]
        OWE["Owed, per applier<br/>ForeignCommit · Refusal · Repair · CatchUp"]
        QUIET{"Quiet branch?<br/>this worker and its siblings"}
    end
    EXEC["Dispatcher<br/>one attempt, no retries"]
    TRIG["Trigger status<br/>operation · repairOwed"]
    FLUX["Flux<br/>GitRepository · Kustomization"]
    ARGO["Argo CD<br/>Application"]

    OBS --> OWE --> QUIET
    QUIET -- "yes, and due" --> EXEC
    EXEC -- "record before the write" --> TRIG
    EXEC -- "recheck, fetch, resume with a token,<br/>follow, suspend again" --> FLUX
    EXEC -- "recheck, set a pinned sync,<br/>follow its token" --> ARGO
    EXEC -. "report: Requested · Running · Settling ·<br/>Unresolved · Succeeded · Failed · Current ·<br/>RequestFailed · Rejected · Dropped · Stale" .-> OWE
```

- **Observing.** Every observation of the remote passes one point. The first after a start is a
  baseline (owes `CatchUp`, never `ForeignCommit`, because an apply on startup would revert edits
  made while the operator was down). A tip the worker pushed owes `CatchUp`; any other owes
  `ForeignCommit`. When in doubt it is foreign, which costs one apply on a quiet branch.
- **Deciding.** At the end of every wake, if the branch is quiet, each applier that is owed
  something, has no operation out, shares no source with one that has, and is past its backoff
  (and, for a `CatchUp`, its quiet period) gets one attempt. The attempt carries away what it
  covers; each target in it carries its uid and its own reason. The worker takes the operation's
  token; nothing is on the trigger yet.
- **Attempting.** The dispatcher re-reads each target. It asks only for those that are still the
  same incarnation, still name a trigger that holds this applier on this branch, are still admitted
  by their `ClusterProvider`s (their own, and their trigger's), and for a `Refusal` still consent.
  If no target still holds the request's reason, it asks nothing (`Dropped`). Then the applier's
  kind decides, and both kinds write the operation through to the trigger as `Requested`
  (`StatusWriter.RecordApplyOperation`) just before the write that starts the apply. The record is
  refused, and nothing is started, while the trigger is being deleted or its status holds another
  operation in flight (an external resume the reconciler recorded, or one an earlier process is
  still owed). A recorded attempt that then asks for nothing withdraws its record, which puts back
  what the status held: the trigger never shows an operation nothing could have applied. The
  writer matches operations by their token, never by time. A `FluxTrigger` that is not
  `Held=True` (still adopting, or being deleted) is not resumed (`RequestFailed`). A source
  on another repository or branch, a `Kustomization` whose `spec.suspend` another manager applies,
  or an `Application` with automated sync on, is `Rejected`. A decision that no longer holds when
  the write is due (the commit is no longer the tip, or something is waiting to be published) asks
  nothing (`Stale`, no failure).
- **Following.** The dispatcher follows the operation and reports each move, so the attempt is out
  for exactly the apply window. Flux: `Running`, `Settling` once suspended again, then `Succeeded`
  or `Failed` ([below](#the-flux-applier-suspended)). Argo CD: the operation state carrying the
  request's token ([below](#the-argo-cd-applier)). One not completed after 5 minutes is
  `Unresolved`: a Flux `Kustomization` is suspended at once, and the operation is followed on, at an
  interval doubling to a minute. The applier object being deleted, or its trigger being deleted,
  completes it as `Failed`; no `GitTarget` naming the trigger any more abandons it, and a Flux
  `Kustomization` is suspended, and that suspend handled, before it is let go.
- **Settling.** An operation that succeeded is done. A stale attempt gives back what it carried and
  is decided again at the next quiet moment. Any other outcome, a failed operation included, gives
  back what the attempt carried, a refusal only if it still stands, and starts that applier's
  backoff (2s doubling to 5m, at least 15s after a rejection). `ApplyRequestFailed` or
  `ApplyFailed` is recorded for the first failure of a run, and `ApplyUnresolved` when an operation
  becomes `Unresolved`; `apply_operations_unresolved` counts those still out.
- **Learning about pushes.** A push webhook into GitOps Reverser's receiver (seconds), a rejected
  compare-and-swap push (at the next publication), or the periodic refresh (minutes). All three
  feed the same observation point.

## The Flux applier, suspended

### Source facts

Each was read in kustomize-controller **v1.9.6** (`internal/controller/`) and flux2 **@1ed703a3**,
both under `external-sources/flux/`, and each is applied by the fake the tests run against
(`internal/applier/fluxsim`, tested on its own). None should be re-derived.

| Fact | Where | What it gives |
| --- | --- | --- |
| `finalizeStatus` is deferred and copies `reconcile.fluxcd.io/requestedAt` to `status.lastHandledReconcileAt` on **every** return: suspended, failed, dependency not ready, succeeded | `kustomization_controller.go:143`, `:1335–1340` | A token is handled exactly when the reconcile that read it ends, whatever the outcome |
| A suspended object returns right after the finalizer check, before resolving the source | `:189–192` | Suspended means nothing is fetched, built or applied |
| `spec.suspend` is read only when a reconcile starts | `:189` | A suspend does not stop an apply already running (measured too) |
| `lastAttemptedRevision` is written before the build | `:390` | It says nothing about an apply having finished |
| `observedGeneration` is set whenever `Ready` is True at the end, a suspended return with a `Ready` left from the last apply included | `:1345–1348` | No completion signal can be read from it |
| A failed apply requeues after `retryInterval`; a dependency not ready requeues with `Ready=False`, `DependencyNotReady` | `:311`, `:264–268` | Both are outcomes to read; a requeue into a suspended object does nothing |
| A reconcile is enqueued by a new generation **or** a new `requestedAt`; a new source revision enqueues it too | `kustomization_manager.go:139–173` | One write that sets both `suspend` and `requestedAt` enqueues a reconcile that reads both |
| A dependent needs its dependency's `observedGeneration == generation` and `Ready` | `:572`, `:618–624` | A dependency suspended after a failed apply holds its dependents back until it succeeds |
| controller-runtime runs at most one reconcile per object at a time | workqueue contract | A token handled by a later reconcile proves every earlier reconcile of that object has ended |
| `flux resume ks` is a merge patch of `spec.suspend: false`, then a wait on readiness | flux2 `cmd/flux/resume.go:183–245` | A resume is `flux resume` with a token added |

### One operation, every reason

Every reason runs the same operation, because a resume alone applies (measured: it reverted a
drifted object with no new commit):

1. **Make the artifact hold the commit.** When it does not, stamp the `GitRepository` and wait for
   it to handle that token. An artifact that ends on another commit is `Stale` when the decision no
   longer holds, and a failure otherwise: the source cannot fetch what the worker checked.
2. **Recheck just before resuming:** read the `Kustomization` again (the same uid, the same
   `sourceRef`, no other manager applying `spec.suspend`: one replaced or re-pointed during the
   fetch is not resumed), read the artifact again, and ask the decision (`ApplyRequest.Current`).
   This closes the race of measurement point 6 as far as it can be closed.
3. **Record the operation** on the trigger, written through, as `Requested` with its token, and
   ask the decision once more: the record can wait on the API server, and a decision that went
   stale meanwhile withdraws the record and resumes nothing.
4. **Resume with a token.** One server-side apply under the field manager `gitops-reverser` sets
   `spec.suspend: false` and `requestedAt: <T1>`, carrying the uid and `resourceVersion` of that
   last read as preconditions, so anything that changed the object since is a conflict (read again,
   up to five times) and a deleted one is not re-created. A resume whose answer is lost (a timeout,
   an internal error) may have landed, so it is followed and suspended again as if it had.
5. **Wait for T1 handled.** A token somebody stamped over T1 (`flux reconcile`) stands in for it:
   the reconcile that handles it started after the resume. A handled token does not prove a
   reconcile ran resumed, though: a suspend landing between the resume and the reconcile (a
   `flux suspend`) has the suspended return handle it, leaving the last apply's `Ready`. So the
   outcome also needs proof that a reconcile ran since the resume: `Ready`'s `lastTransitionTime`
   (a real reconcile marks it `Unknown` first) or the top `status.history` entry moved past what the
   resume's apply returned. Then read the outcome:

   | Read after T1 is handled | Outcome |
   | --- | --- |
   | No reconcile ran since the resume | `Failed`, so the worker asks again |
   | `Ready=True`, `lastAppliedRevision` holds the commit | `Succeeded` |
   | `Ready=True`, the revision the `GitRepository` holds now, which is not the commit | `Succeeded`, logged and counted in `apply_revision_mismatches_total` (the residual race: the source moved on) |
   | `Ready=True`, any other revision (kustomize-controller's cached source lagged the artifact checked) | `Failed`, so the worker asks again |
   | `Ready=False`, `DependencyNotReady` | `Failed`, with a reason of its own |
   | `Ready=False`, anything else, or no `Ready` | `Failed` |

6. **Suspend again:** another server-side apply sets `spec.suspend: true` and
   `requestedAt: <T2>` (`Settling`, the token recorded as `suspendToken`). T2 handled proves that any
   reconcile that started before the suspend has ended and the object is suspended now: a suspended
   reconcile handles a token without applying, and reconciles of one object never overlap. Found
   resumed again before T2 is handled, the `Kustomization` is suspended again.
7. **Complete** once T2 is handled, and keep the window open 10 s more.

A `ForeignCommit` or `CatchUp` asks nothing (`Current`) when the `Kustomization` is suspended and
both its artifact and `lastAppliedRevision` hold the commit: nothing applied since, and nothing will
until it is resumed. A `Refusal` and a `Repair` always run the operation. A failed apply is suspended
like any other, which is what stops Flux's own `retryInterval` retry; the worker owns the retry.

An `Unresolved` operation is suspended at once (the suspend does not stop the running apply, but it
stops whatever would follow), and that suspend's token then decides when it completes. There is no automatic
cancellation.

`CatchUp` stays, as convergence rather than safety: a suspended `Kustomization` never applies a
stale artifact, but Flux's inventory learns of an object only from an apply of a commit that holds
it, and an object created through the API and never applied by Flux would not be pruned when Git
later deletes it.

### Who owns `spec.suspend`

- **A trigger acts only while an eligible `GitTarget` references it.** Eligible: in the trigger's
  namespace, not being deleted, naming the trigger, admitted by its `ClusterProvider`s
  (`ApplierAdmitted`), and writing the repository and branch the applier's source fetches. It is
  judged from those inputs alone, never from the `GitTarget`'s `Ready`, which reads the trigger's
  own `Ready` back. A trigger no `GitTarget` names is `Configured=Unknown` and `Ready=True`, both
  `NotReferenced`: it reads and writes nothing in the applier's cluster and is not in line. With
  several naming it, one eligible binds it; none eligible while one cannot be judged yet (its
  `GitProvider` missing, a read failed) is `Configured=Unknown`, `NotBound`, progressing; each
  refused is `Configured=False` with the reason they share, or `NotBound`, stalled. The trigger
  reconciler watches what eligibility reads: `GitTarget`s (before and after an update),
  `GitProvider`s, `ClusterProvider`s and namespace labels. A `GitTarget` reads `ApplyTriggerReady`
  `Unknown` (`NotReferenced`) until the trigger has observed it, since naming a trigger does not
  move its generation.
- **One trigger per applier.** Every trigger suspends under the same field manager, so two holding
  one `Kustomization` would take each other's resume for an external one and release each other's
  hold. Of the triggers of one kind in line for one applier in one cluster, one holds it; the others
  are `Configured=False`, `HeldByAnotherTrigger`, do nothing, and release nothing, and the dispatcher
  drops a request through them. In line: an eligible `GitTarget` references the trigger, or, for a
  `FluxTrigger`, it carries its finalizer (it holds the `Kustomization`, or is handing it back), so
  an older trigger with the wrong repository, or not admitted, keeps no younger one out. First comes
  a `FluxTrigger` with an operation in flight, then one that carries its finalizer, then the oldest
  by `creationTimestamp` (then namespace and name). So a `Kustomization` changes hands only once its
  holder lets go of it, and never mid-apply or mid-release. The dispatcher's check before a resume,
  and the reconciler's suspend of a resumed `Kustomization`, read the triggers uncached. A holder
  that lets go with a trigger in line leaves the `Kustomization` suspended, its suspend handled, for
  it, and the change re-runs the others. The same rule holds for `ArgoCDTrigger`s, which would
  otherwise both start syncs, without the in-flight and finalizer rules: a sync somebody else
  started is never replaced.
- **Adoption.** A `Configured` `FluxTrigger`, so one an eligible `GitTarget` references, puts its
  finalizer in place and then suspends its `Kustomization` with a server-side apply under
  `gitops-reverser`, and a token. A trigger that never adopted has no finalizer, and deletes at
  once. It is `Held=True` (`Suspended`), and `Ready`, once that
  token is handled; `Held=True` (`Applying`) while an operation of a branch worker is in flight and
  followed. A `Normal` `Adopted` Event records it.
- **The manifest omits `suspend`.** Whatever applies the `Kustomization` object (a parent
  `Kustomization`, Helm, flux-operator) must not set the field, or it takes it back on its next
  apply. A `managedFields` entry of another manager with operation `Apply` owning
  `f:spec.f:suspend` is `Configured=False`, `SuspendOwnedElsewhere`, naming the manager, and
  nothing is held. An `Update` entry (`flux resume`, `flux suspend`, `kubectl edit`) wrote the field
  once, and the next forced apply takes it back.
- **Resumed by someone else.** The reconciler finds the `Kustomization` resumed with no operation of
  a branch worker in flight (read uncached, since the worker records its operation a moment before
  it resumes). It suspends it with a token, records a `Normal` `ResumedElsewhere` Event, and records
  the apply as an `Unresolved` operation with that suspend token and no request token, which it
  completes once the suspend is handled. It does this every time, with an Event each time: there is no
  step-back state. A `FluxTrigger` that is `Ready` reads its `Kustomization` every minute, which
  bounds how long such a resume runs unsuspended. The reconciler reads the operation before and
  after the `Kustomization`, so an operation that completes between the two reads is no external
  resume.
- **An operation nobody follows.** The reconciler leaves a branch worker's operation in flight
  alone only while this process's dispatcher follows it (`Following`), or for a minute after it
  first saw it unfollowed, so restart recovery gets its chance. After that it suspends the
  `Kustomization` with a token, sends a `Warning` `OperationUnfollowed` Event, and completes the
  operation `Failed` once the suspend is handled. A resumed `Kustomization` is never left with
  nothing watching it.
- **Hand-back.** The trigger carries the finalizer `configbutler.ai/flux-trigger` from before its
  first adoption write until it has handed the `Kustomization` back or over, also while its
  configuration is invalid (then `Held=Unknown`, `NotConfigured`, and the message says the
  `Kustomization` is left suspended under `gitops-reverser`). It hands it back when it is deleted,
  and when no eligible `GitTarget` references it any more, conclusively: a reference that cannot be
  judged yet keeps the hold, since a release is not undone. Until the hand-back is done the trigger
  is `Held=False`, `Releasing`, and stays the holder in line. It suspends the `Kustomization` with a
  token if an apply has it resumed, waits until that is handled, then releases `spec.suspend` with
  an empty apply, so Flux owns it again (`Released`). An operation still recorded in flight that
  this process does not follow then completes as `Failed`. A reference that returns before the
  release is done holds the `Kustomization` again; one that returns after adopts it anew. Deleting
  or re-creating a `GitTarget` therefore hands it back, and Flux applies the tip at once: what Flux
  does without GitOps Reverser, accepted because with no `GitTarget` nobody carries live edits to
  Git anyway. With another
  trigger in line that can take it over (`Configured=True`, or `False` only for
  `HeldByAnotherTrigger`), it is not released but left suspended for that one: released, Flux would
  apply it at once, unchecked against the branch, before the next trigger suspended it. On a real
  API server our forced apply over a `flux suspend` *shares* `spec.suspend` with that `Update`
  entry, so an empty apply alone would leave it `true`: the release first takes the field with a
  forced `suspend: false`. A manager that *applies* the field keeps it, and `Released` names it. An
  operation whose trigger is being deleted completes as `Failed` and writes nothing more to the
  `Kustomization`; the dispatcher reads the trigger uncached before every step that may suspend. A
  `Kustomization` that is gone lets the trigger go. A `ClusterProvider` that is gone does not end the
  hand-back: it goes on through whatever client still reaches the cluster, which for a provider
  without `kubeConfig` is the operator's own. A cluster no client reaches any more, a suspend or
  release the identity may not make, or a hand-back still unfinished 10 minutes after `Held` turned
  `Releasing` (each a `Warning` `ReleaseFailed` Event, which says where the `Kustomization` was left)
  lets the trigger go.

### Restart recovery

The worker reads its triggers' `status.operation` before the loop handles its first item, so no
live event is mirrored before the windows of what it finds are open:

- **An operation not completed** is followed to its end by its recorded tokens, asking the applier
  for nothing (`ApplyRequest.Recovered`), with its apply window open from the recovery. For Flux,
  the follow suspends the `Kustomization` again. A recorded token that was stamped over (by our own
  suspend before the crash, or a `flux reconcile`) is judged on whether a reconcile ran since the
  operation's `requestedAt`. A recorded request arriving before the dispatcher starts waits for it.
  A recorded token that is nowhere on the
  `Kustomization` (neither its `requestedAt` nor the last handled) means the process stopped
  between the record and the resume, or a suspend it never reported followed: the `Kustomization`
  is suspended, and the operation ends `Failed`.
- **`repairOwed`** is restored.
- A worker's own record is read back once, when this process first lists the trigger; later reads
  can lag its own writes. A record with no request token (an external resume the reconciler
  suspended) is followed whenever it is listed, once per suspend token, so its window opens while the
  process runs too.

The first observation is still a baseline that owes a `CatchUp`, so a recovered operation decides
nothing about what is owed.

### Shared sources and prefetch

- The trigger lists the consumers of its `GitRepository` in the applier's cluster:
  `Kustomization`s with that `sourceRef`, and `HelmRelease`s whose chart comes from it, directly or
  through a `HelmChart` their `chartRef` names (at whichever `HelmRelease` version the cluster
  serves; consumers that cannot be determined are an error, so no prefetch). Those no `FluxTrigger`
  on the same `ClusterProvider` holds, that is reports `Held=True`, are
  `status.source.otherConsumers`, information
  only: it never affects `Ready`, and no `Warning` is sent, because one `GitRepository` for `apps`
  and `infra` is a normal setup.
- `status.source.prefetch` is true only when that list was read and is empty, and the trigger holds
  its own `Kustomization`. The dispatcher checks once more, on the cluster, that every consumer is
  held just before it stamps the source. Then a push somebody
  else made, observed while the branch is busy, has the source fetch at once, so the artifact is
  ready when the apply is due. On a quiet branch the attempt sent at once fetches for itself. No
  prefetch while any `FluxTrigger` on that source has an operation in flight, because a resumed
  consumer applies whatever the artifact becomes. A listing that is forbidden counts as not holding.
- Appliers that share a source have one operation at a time, and take turns: the one whose last
  turn is oldest goes first, so a busy sibling cannot starve another. This holds across the
  workers of GitProviders that name the same repository and branch: a worker claims the source's
  group before it sends, under the siblings' lock, and a sibling's operation of that source holds
  a prefetch back too.

### What the measurement found

Measured on the e2e cluster (Flux v2.9.6, kustomize-controller and source-controller v1.9.6) with a
throwaway Gitea repository, 1 and 200 ConfigMaps, polling through `kubectl proxy` (±30 ms):

| Median, ms from the first Flux write | 1 object, request | 1 object, suspended | 200 objects, request | 200 objects, suspended |
| --- | ---: | ---: | ---: | ---: |
| Source artifact holds the commit | 60 | 58 | 122 | 111 |
| Applied (`lastAppliedRevision`, `Ready`) | 149 | 58 + 115 | 928 | 111 + 852 |
| Suspend write | – | 10 | – | 12 |
| **Total** | **149** | **193** | **928** | **980** |

The suspended lifecycle costs a fixed ~50 ms per apply, one serialized round trip, at any payload
size; build and apply dominate either way. The same runs found the behaviors the lifecycle is
built around: a suspended consumer ignores new artifacts; suspend does not stop a running apply; a
`requestedAt` on a suspended object is handled without an apply (which made the request-based
client report a `Refusal` done with nothing reverted); a resume alone applies; and a resume applies
whatever the artifact holds then.

### Decisions

Taken 2026-10-17, with the work built as a stack on #419 and released with it, so `spec.applier`
was never released:

1. **Suspension is the only Flux mode,** as automated sync off is the only Argo CD mode. A second,
   request-based mode would keep every hole of the 24h setup alive, and two completion protocols.
2. **A shared source is information only:** `status.source.otherConsumers`, prefetch off while it is
   not empty, no `SharedSource` condition.
3. **An external resume is always suspended again,** with a `Normal` Event each time.
4. **`CatchUp` stays for Flux,** as convergence. The same inventory question exists for Argo CD,
   which was decided without `CatchUp`.
5. **The resources are triggers,** with long kind names: `FluxTrigger` (`spec.kustomizationRef`)
   and `ArgoCDTrigger` (`spec.applicationRef`), named by `GitTarget.spec.applyTriggerRef` in the
   same namespace. Each is its own kind because suspension needs one owner object with a finalizer,
   the operation and `repairOwed` need a durable status, and typed kinds replace the union of
   references and its CEL rules. The references are immutable: a re-pointed trigger would leave the
   old applier held by nothing.

## The apply window

**An apply is asynchronous, and that leaves a window.** An API edit that lands while an apply runs,
to a field the applier sets, is reverted by the apply, because the commit it applies does not hold
the edit yet. Making the request synchronous would not help: waiting for the apply does not stop a
user from editing during it, and refusing edits would halt the API for everyone to protect a few
seconds.

The quiet-branch rule confines the window to three moments: right after somebody else's push, right
after a refusal the target asked to have reverted, and after `quietPeriod` of silence on a branch
GitOps Reverser pushed to. Inside it, **the API wins**, as it does for any object both sides
changed:

- **The window is known.** It opens when the attempt is sent and stays open until its operation
  completed: for Flux, the resume's token and the suspend after it handled; for Argo CD, the operation
  state carrying the request's token completed. An `Unresolved` operation keeps it open until it
  completes. Then the worker keeps it open 10 seconds more, because the applier's writes reach it
  through the watch. An attempt sent within those 10 seconds carries the edits seen in the window
  before it, and that window's end, so a late write of the earlier apply is still withheld, and an
  attempt that asked for nothing does not end the earlier grace. A write of the new apply to an
  object edited during the earlier one is then withheld too, since the managers cannot tell the two
  applies apart: that costs one redundant `Repair`, never an edit.
- **The late apply is withheld.** A write whose latest `managedFields` entries are the applier's
  own managers alone (`kustomize-controller`; `argocd-controller` or
  `argocd-application-controller`), inside the window, to an object in the applier's folders that
  the API side edited after the attempt was sent, is not mirrored. Git keeps the edit, and the
  withheld write owes its applier a `Repair`: an apply of the tip at the next quiet moment, with no
  quiet period, for Flux and Argo CD alike. The cluster runs the reverted value until then, and the
  trigger shows `repairOwed`.
- **Only proof withholds.** `managedFields` times resolve to the second. When the applier ties
  with another manager, the write could be the edit itself, and withholding an edit loses it
  outright; it is mirrored as before. It is also remembered as an edit, even as the first write in
  the window, so the applier's write alone a second later is still withheld. A DELETE carries
  nothing about its deleter and is never withheld. Both are logged.

Every collision is logged with the object, the applier and the requested commit, and counted in
`apply_window_collisions_total` (`withheld` or `ambiguous`). What remains: an edit reverted on the
cluster until the repair, an ambiguous tie mirrored as the applier's commit, and an applier's write
that reaches the watch more than 10 seconds after its operation completed.

## The Argo CD applier

**GitOps Reverser takes full control of when Argo CD syncs.** The `Application` an `ArgoCDTrigger`
holds has automated sync off (`syncPolicy.automated.enabled: false`, or no
`automated` block), so its timed refresh and its webhook only compare, and every sync it runs is
one GitOps Reverser asked for, pinned to a commit the branch worker checked. This is the supported
setup for bi-directional editing with Argo CD, not an option next to automated sync: an
`Application` with automated sync on makes its trigger `Configured=False` (`AutomatedSyncEnabled`)
and is never synced. The user guide is [`../bi-directional-argocd.md`](../bi-directional-argocd.md).

Automated sync off is Argo CD's equivalent of a suspended `Kustomization`, and it needs no suspend after an apply: an
Argo CD operation that has completed is not followed by another one nobody started. **It gets no
`CatchUp`**: an `ArgoCDTrigger` has no `quietPeriod`, and the worker never owes an `Application`
one.

Every rule the client relies on is Argo CD's own, read from its source (3.5.0):

| Fact | Consequence |
| --- | --- |
| Automated sync is on when `syncPolicy.automated` is present and its `enabled` is unset or true (`IsAutomatedSyncEnabled`) | `AutomatedSyncEnabled` is exactly this rule; `automated: {enabled: false, prune: …}` is the off switch that keeps prune |
| Automated sync builds `Operation{Sync{Revision, Prune: automated.prune, SyncOptions, Source}, InitiatedBy{Automated: true}, Retry: syncPolicy.retry or {Limit: 5}}` (`autoSync`) | A request builds the same, with `Revision` pinned to the checked commit |
| A sync starts by setting `operation`, never while one is set, in an update that also clears `status.operationState` (`SetAppOperation`; the CRD has no status subresource) | A request updates the whole `Application`, guarded by the `resourceVersion` it read, and waits out a running operation instead of replacing it |
| `NewOperationState` copies the operation, its `info` included, into `status.operationState`, and `operation` is cleared only once the operation completed | An update whose answer was lost, or one recovered after a restart, is followed by its token: in `operation` or `status.operationState` it landed. Neither means it was not made yet. The update carries the `resourceVersion` it was made from, so GitOps Reverser then writes the `Application` at the version it read, asking for a refresh (`argocd.argoproj.io/refresh`, which only compares). Once that write is made the update can never land, and the operation is `Failed` and asked again; a conflict means read again. No API-server deadline is assumed |
| The controller clears `operation` when the operation completes and leaves the result in `status.operationState`; a failed operation that will be retried is `Running` again, with a `finishedAt` | An operation completes when the operation state carrying its token has completed (`Succeeded`, `Failed`, `Error`); a pending retry is still running |
| A retry with `retry.refresh: true` drops the operation's revision and syncs the newest one | That one field is dropped from the copied retry: a retry stays pinned |
| Every apply writes as field manager `argocd-controller`, client-side and server-side alike; a `Replace=true` sync creates and replaces with no field manager, so the API server takes it from the user agent, `argocd-application-controller` | The apply window knows both as Argo CD's managers |
| Sync windows are checked for every operation, by `InitiatedBy.Automated`, and a blocked operation stays `Running` until the window opens (`syncWindowPreventsSync`, `CanSync`) | A sync GitOps Reverser starts obeys the project's windows for automated syncs, and a blocked one runs later, still pinned: by then it can be older than the tip. The guide forbids sync windows on a held `Application`; `Configured` does not check them yet |
| Hooks default to `BeforeHookCreation`: each sync deletes and recreates the hook's object (`DeletePolicies`) | A `WatchRule` covering a hook's kind mirrors that delete and create on every sync; the guide keeps hook kinds out of them |

Three choices were decided (2026-10-17):

1. **Prune** comes from `syncPolicy.automated.prune` when `automated` is present with
   `enabled: false`. Without the block, a sync does not prune, and the guide says so. There is no
   `prune` field on the trigger.
2. **The initiator** is `InitiatedBy{Username: "gitops-reverser", Automated: true}`. GitOps Reverser
   replaces automated sync, so a sync window that blocks automated syncs blocks its syncs too.
3. **Hooks run on every sync**, so on every `ForeignCommit`, which asks every applier on the
   branch. Narrowing it by changed paths waits for a measured complaint.

A hub Argo CD keeps `Application`s in a management cluster: the trigger's `spec.clusterProviderRef`
reaches it as it reaches a Flux in another cluster. Out of scope: multi-source `Application`s (and
`sourceHydrator`), which are `UnsupportedSource`; `ApplicationSet`s, which own the spec of the
`Application`s they generate; and Argo CD's own sharding and HA.

## The aim beyond Flux and Argo CD

**A webhook applier** for any other tool: a third trigger kind that POSTs `{"repository", "branch",
"commit", "reason"}`, signed `X-Signature: sha256=<hex HMAC>` the way Flux's `generic-hmac`
`Receiver` checks. It gets every reason, `CatchUp` included, since nothing says the tool does not
apply on its own. No in-process plugin: it would run with every credential the operator holds.

## What it costs

- **While GitOps Reverser is down, nothing applies.** A Git-side push waits until it is back, or for
  a manual `flux resume` or sync (after which GitOps Reverser suspends the `Kustomization` again on
  start). This is the price of exclusivity, and the same trade Argo CD with automated sync off makes.
  Bi-directional editing already stops in that state.
- **Flux tooling shows `Suspended`.** `flux get ks` reports it, and `flux reconcile ks` does nothing.
- **A dependency that failed stays behind.** A `Kustomization` suspended after a failed apply keeps
  its generation unapplied, and its dependents wait until GitOps Reverser's retry succeeds.
- **The `Kustomization`'s manifest must omit `suspend`.** This is checked, not just documented.
- **The residual race.** Between the artifact recheck and the reconcile reading the artifact,
  somebody can stamp the `GitRepository`. With prefetch held off during operations, that is a person
  or the source's own interval. The outcome table catches it, and the apply window still withholds
  a late write over a newer edit.
- **One replica.** The receiver, the branch workers and the dispatcher run in one pod, and the chart
  refuses more than one replica. Leader election is not switched on; more than one replica needs
  ownership coordination first, and deliveries routed to the pod that owns the branch.
- **An apply holds its applier while it runs.** `wait: true`, health checks and a timeout lengthen
  it, which is the point: nothing is stacked on an apply still running. One that never ends holds
  its applier, which `apply_operations_unresolved` and an `ApplyUnresolved` Event show.
- **Every applier on the branch applies on a `ForeignCommit`**, whether or not the push touched its
  folder. Harmless for Flux; for Argo CD it runs hooks. Narrowing it needs the applier's own source
  paths, so it waits for a measured complaint.
- **A sync somebody else started holds an Argo CD request**, for up to 5 minutes, and then fails
  it; the worker asks again after its backoff.

## Where this goes next

The Flux applier PR (#419) and the stack on it (#420) carry what gitops-api, the first adopter,
needs before it can drop its own gate: the three reviews' findings and the customer review's three
blockers, the hardened receiver, the apply window, the Argo CD applier with automated sync off, and
the Flux applier held suspended. After them, in order, each one pull request:

1. **The webhook applier**, as a third trigger kind.
2. **More than one replica.** Leader election is not switched on, and the chart runs one replica.
   Standbys need ownership coordination first (the HA plan in `docs/future/`), and deliveries then
   need routing to the pod that owns the branch.

Test gaps to close along the way: a remote source cluster and an applier in another cluster (a hub
Argo CD among them) in e2e, `Forbidden` against real RBAC, the receiver and applier roles installed
by the Helm chart, two `GitTarget`s sharing one `Kustomization` in e2e, an apply-window collision in
e2e, a prefetch observed end to end, a `Replace=true` Argo CD sync's field manager measured rather
than read from the source, and `-race` in CI.

**What adopting it changes for gitops-api.** Its gate shrinks to "one save, one window", which
protects commit messages and has nothing to do with Flux. Its push handler relays each push to the
`GitProvider`'s receiver instead of stamping Flux. And publications trail an editing session by at
least `quietPeriod`, since Flux applies GitOps Reverser's own commits only after the branch has
been quiet; `quietPeriod` is the knob for that trade.

## What proves it

| Claim | Test |
| --- | --- |
| The decision: quiet branch, reasons, quiet period, baseline | [`apply_trigger_test.go`](../../internal/git/apply_trigger_test.go) |
| Retries are decided by the worker; recovered refusals, withdrawn consent and recreated targets are not retried; each applier keeps its own clock; one attempt for a shared applier; a busy sibling worker holds the branch | [`apply_attempt_test.go`](../../internal/git/apply_attempt_test.go) |
| The operation is recorded before the write that starts it, moves through its phases on the trigger's status, an attempt that asked nothing restores the record before it, and a withheld rollback owes a `Repair` | [`apply_operation_test.go`](../../internal/git/apply_operation_test.go), [`status_writer_test.go`](../../internal/applier/status_writer_test.go) |
| Every attempt rechecks targets, asks for nothing weaker than it was told, and resumes no `Kustomization` on another branch | [`dispatcher_test.go`](../../internal/applier/dispatcher_test.go), [`flux_test.go`](../../internal/applier/flux_test.go) |
| The suspended lifecycle against the source facts: an apply is not finished when attempted; a token handled while suspended is no apply; a resume alone reverts a refusal; a suspend does not stop a running apply; the artifact is rechecked before the resume; a failed apply is suspended and Flux's requeue applies nothing; a shared source applies only the consumer asked | [`flux_lifecycle_test.go`](../../internal/applier/flux_lifecycle_test.go), [`flux_consumers_test.go`](../../internal/applier/flux_consumers_test.go), [`fluxsim`](../../internal/applier/fluxsim/fluxsim_test.go) |
| Adoption, `SuspendOwnedElsewhere`, an external resume suspended again with an Event every time, the hand-back on deletion, and the source's other consumers | [`fluxtrigger_hold_test.go`](../../internal/controller/fluxtrigger_hold_test.go) |
| A restarted worker follows an operation left in flight with its window open, restores `repairOwed`, and follows an apply started elsewhere; one recorded but never resumed ends `Failed` | [`apply_recovery_test.go`](../../internal/git/apply_recovery_test.go), [`dispatcher_test.go`](../../internal/applier/dispatcher_test.go) |
| Appliers sharing a source have one operation at a time; a push is prefetched only while the branch is busy and no operation of the source is in flight | [`apply_groups_test.go`](../../internal/git/apply_groups_test.go), [`flux_consumers_test.go`](../../internal/applier/flux_consumers_test.go) |
| An applier in another cluster is reached through its own `ClusterProvider`, behind that provider's `accessFrom` | [`dispatcher_test.go`](../../internal/applier/dispatcher_test.go), [`clusterprovider_admission_test.go`](../../internal/authz/clusterprovider_admission_test.go), [`applytrigger_controller_test.go`](../../internal/controller/applytrigger_controller_test.go) |
| The applier's late write over a newer edit is withheld, and nothing that could be the edit is | [`apply_window_test.go`](../../internal/git/apply_window_test.go), [`last_field_managers_test.go`](../../internal/watch/last_field_managers_test.go) |
| The receiver's path names no provider, and every miss answers the same | [`receiver_test.go`](../../internal/webhook/receiver_test.go) |
| The original race: two API edits end at the later value with no extra commit | [bi-directional Flux corner](../../test/e2e/flux_bi_directional_e2e_test.go) |
| A `Kustomization` is adopted and stays suspended; `SourceMismatch` never resumes it and is applied once fixed; a Gitea webhook and the refresh alone each get a push applied; a failed apply is suspended and retried by the worker; a `flux resume` is suspended again; deleting the trigger hands the `Kustomization` back | [Manager Flux Applier](../../test/e2e/flux_applier_e2e_test.go) |
| A manager restarted during an apply completes the same operation and leaves the `Kustomization` suspended | [Flux Restart Recovery](../../test/e2e/flux_restart_recovery_e2e_test.go) |
| A refused edit is reverted by a resume of its `Kustomization` | [Refusal reapply](../../test/e2e/refusal_reapply_e2e_test.go) |
| The Flux guide's example is accepted by the code | [`flux_guide_test.go`](../../internal/applier/flux_guide_test.go) |
| An Argo CD sync is the one automated sync builds, pinned to the commit, with a pinned retry; a running operation is waited out and never replaced; an operation set between the read and the update makes it fail; every `Configured` reason, `AutomatedSyncEnabled` among them; when a sync counts as finished | [`argocd_test.go`](../../internal/applier/argocd_test.go), [`dispatcher_test.go`](../../internal/applier/dispatcher_test.go) |
| An `Application` is never owed a `CatchUp`; Argo CD's late write over a newer edit is withheld by its own managers | [`apply_trigger_test.go`](../../internal/git/apply_trigger_test.go), [`apply_window_test.go`](../../internal/git/apply_window_test.go) |
| Each trigger kind holds one applier in one cluster, immutably; `quietPeriod` is bounded and defaulted on a `FluxTrigger` alone; a new `FluxTrigger` is reconciled in the pass that adds its finalizer | [`applytrigger_schema_test.go`](../../internal/controller/applytrigger_schema_test.go) |
| With Argo CD: two API edits keep the later value with no extra commit; a Git-side push is synced by a pinned `ForeignCommit`; a refused edit is reverted by a sync; an `Application` with automated sync on is `AutomatedSyncEnabled` and never synced | [bi-directional Argo CD corner](../../test/e2e/argocd_bi_directional_e2e_test.go) |
| The Argo CD guide's example is accepted by the code | [`argocd_guide_test.go`](../../internal/applier/argocd_guide_test.go) |

The history of how the first trigger was decided, with the options weighed and the Git-side
reference the code cites, is in
[`../finished/push-notification-and-reconcile-trigger.md`](../finished/push-notification-and-reconcile-trigger.md).
