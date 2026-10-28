# CommitRequest window finalization

> **spec**: current behavior, reviewed at `670032d2` on 2026-10-27.
> The code depends on this document; change one, change the other.
> Index: [`../INDEX.md`](../INDEX.md)

A `CommitRequest` is a one-shot “save now” command for one `GitTarget`. It does not mirror the
`CommitRequest` object. Instead, it can attach a message to a matching open commit window and asks the
worker to close that window after the requested collect delay.

## Two ways in, one core

A `CommitRequest` is the one operator kind that can also live on a source cluster. It is picked up
in one of two ways, and both hand it to one core
([`commitrequest_core.go`](../../internal/controller/commitrequest_core.go)), which decides everything
else once:

- **On the control cluster**, the `CommitRequestReconciler` serves it with the operator's own client,
  as any operator kind.
- **On a source cluster**, the `CommitRequestServer` watches the requests of every `KubeProvider`
  whose `spec.commitRequests` is `Reconcile` or `ReconcileAndDeleteFinished`, through that provider's
  credential, in the namespaces it lends its `GitTarget`s in `spec.allowedGitTargets`. It writes
  status, and with `ReconcileAndDeleteFinished` deletes finished requests, through the same
  credential. A rotated kubeconfig restarts the watch on a client built from the new credential, so
  watches and status writes never keep the old one. It never writes the CRD, and reports its state in
  the `KubeProvider`'s `CommitRequestsServed` condition.

The core takes one step at a time, each decided again from the object as it is stored. Its
responsibilities, in order: admission by `spec.allowedCommitRequests`; the request's author; the
attach and its outcome; the apply `catchUp: AfterPush` asked for; and retention.

### Which path owns a request

The named `GitTarget`'s `KubeProvider` alone picks the path that serves a request, its owner:

| The request is on | The `GitTarget`'s `KubeProvider` | What happens |
| --- | --- | --- |
| the control cluster | `inCluster` | the control-cluster path serves it |
| the control cluster | a kubeconfig, with a `Reconcile` value | left alone: the kubeconfig may point back at the control cluster, and then its watch serves it |
| the control cluster | a kubeconfig, with `commitRequests: Ignore` | `NotAllowed`, saying to create the request on that cluster |
| a source cluster that provider P watches | P | P's watch serves it |
| a source cluster that provider P watches | `inCluster`, and the request is not the control cluster's own object (by UID) | `NotAllowed`, naming both clusters |
| a source cluster that provider P watches | `inCluster`, and the request is the control cluster's own object | left alone: P's kubeconfig points back at the control cluster, whose path serves it |
| a source cluster that provider P watches | another provider, with `commitRequests: Ignore` | `NotAllowed`, naming both clusters |
| a source cluster that provider P watches | another provider, with a `Reconcile` value | left alone: it may watch the same cluster |
| a source cluster | `gitTargetRef` names no namespace | not picked up, and no status |
| either | the `GitTarget` does not exist | `GitTargetNotFound` once `attachTimeout` has passed since creation |

Three rules keep several observers of one request safe:

1. **A path writes only what it knows to be true.** Only the owner serves a request and writes its
   progress. A path that is not the owner writes only `GitTargetNotFound`, or `NotAllowed` when it
   can show no path serves the request, and only into an empty status, because nothing else is true
   for every observer.
2. **Every status write is conditional on the resourceVersion its decision was made from.** On a
   conflict the writer reads the request again and decides again, so a `GitTargetNotFound` decided
   from an older version never overwrites an owner that has started.
3. **The owner claims a request by writing its first status, and attaches only after that.** The
   first write records the author and `Ready=Unknown`, reason `Saving`. If it conflicts and the fresh
   object already holds an outcome, the owner stops without saving. After the claim only the owner
   writes, so a later conflict comes from its own writes, and an outcome the worker produced is
   written onto the fresh object as it is; the save is never restarted.

Ownership is structural, not "the process that wrote the status": after a restart, the owner is
still the path the `KubeProvider` picks, finds its own claim, and resumes with the author it
recorded.

A control-cluster request for a `GitTarget` that mirrors another cluster is refused rather than
served, because its author would come from the control cluster's audit and its edits' from the
other cluster's. A request on a source cluster must name the `GitTarget`'s namespace, because that
namespace belongs to the control cluster; the path cannot refuse a request without one, since a
kubeconfig pointing back at the control cluster sees the control cluster's requests, where an
omitted namespace is valid. A source-cluster request for a `GitTarget` that mirrors the control
cluster is refused only once an uncached read of the control cluster finds no request of that UID:
the same UID means the watch's kubeconfig points back at the control cluster, whose path serves it.
Known gap: a control-cluster request whose `GitTarget`'s provider has a
`Reconcile` value and points somewhere else gets no status from anyone.

### Admission

A grant in the `GitTarget`'s `spec.allowedCommitRequests` sets exactly one of two fields, enforced in
CEL. `consumerNamespaces` admits the request's namespace on the control cluster, by its labels there,
and only on the control-cluster path. `sourceNamespaces` admits it on a source cluster, by name or
`"*"`, only on the source-cluster path, and only when the `KubeProvider` lends that namespace to the
`GitTarget` in `spec.allowedGitTargets`. Omitted, the list is `[{consumerNamespaces: {from: Same}}]`.
A request no grant admits is `NotAllowed`, with a message naming the field that applies. The
`GitTarget` that admitted a request is recorded in `status.gitTargetUID`: a `GitTarget` of that name
with another UID replaced it and must admit the request again, and its refusal is written only once
no worker holds the request.

## Request and window contract

The request identifies the target in `spec.gitTargetRef`, may provide `spec.message`, and sets
`spec.window` (which window to attach to, how long to wait for one, and the timers that close it),
`spec.whenNothingToCommit` and `spec.catchUp`. It is handled by the target’s single branch worker,
so resource events and the attach request share one FIFO.

The worker attaches a request only when all of these match an open window:

1. GitTarget name and namespace;
2. the named actor, if either side has one; and
3. whether each side names an actor (`AttributionOutcome.NamesActor`).

A request with no named author can attach to either a configured-author or an unresolved live
window, but never to a named actor’s window. A request with a named author can attach only to that
actor’s named window. Therefore one user’s request never finalizes another user’s work.

A window is only ever opened by a write; a request attaches to one and never opens one. On first
registration, the worker sets the attach deadline to registration plus `window.attachTimeout`, and
repeated reconciles are idempotent: they restart nothing and, for `attach: Next`, close nothing again.
With `CurrentOrNext` the request attaches to the author's window already open at registration, before
any deadline is consulted, so `attachTimeout: 0s` can attach at all. Otherwise it waits. A request
whose deadline has passed resolves and never takes a later window; when its deadline and a matching
write are ready on the same wake, expiry wins.

On attach, the request's `idleTimeout` and `maxDuration` replace the window's timers: `maxDuration`
runs from the attach, and so does the first idle interval. A waiting request gets a new window before
the target's own timers are applied, so a target with `idleTimeout: 0s` does not close the window
before the request can attach.

`attachTimeout` defaults to `"5s"` rather than `"0s"`, and counts from the worker's registration,
which is after the request's own author was found. The write a request exists to publish reaches
the worker through the same audit join, and on a busy cluster after it, so a zero wait is shorter
than the window-open latency the pipeline can produce.
The two durations are separate settings with separate clocks: `attachTimeout` runs from
registration, `maxDuration` from the attach. Each is a pointer, so an omitted value and an explicit
`"0s"` stay distinguishable. A zero `attachTimeout` attaches to a window already open, or gives up at
once, and never shortens the collection that follows; a zero `maxDuration` finalizes right after the
attach.
Other window closers can close an attached window early, carrying its message: an author or target
change, the byte cap, a resync, or shutdown. An apply coming due does not. While a request is
registered, from its registration until it resolves, the branch worker makes no
[flush](../design/apply-trigger.md#flush-then-apply) and sends no apply request, because a save
defines its own commit boundary: an apply landing between the request and its writes would revert
its first edits before they reach Git. A Git host's push notice leaves an attached window open too.
Each request attaches
to at most one window and cannot rename a finalized commit, including one waiting for push. A bundle
provides no ordering guarantee. The wait does not reserve a transaction. Competing requests are
not queued: a later request of the same author on the same `GitTarget` is refused with
`SaveInProgress` while the earlier one waits or collects, except that a `Next` request closes a
collecting window under the earlier message and then waits for its own.

`spec.message` is literal, including template-like text and surrounding spaces. It is never parsed
as a template, so a request author cannot execute one. Omission uses
`GitTarget.spec.commit.message.liveTemplate`. A target may set
`GitTarget.spec.commit.message.requestTemplate` to frame the message with the window's resources;
the message still arrives unaltered, as `.RequestMessage`, and a template that does not render it
is rejected at admission, so the request's bytes always reach the commit. A `requestTemplate` that
fails to render commits the message verbatim rather than losing the window. A present value accepts 1–1024 Unicode characters;
newline is allowed, other ASCII controls and whitespace-only text are rejected. Validation never
truncates accepted text. With the default `whenNothingToCommit: Resolve`, a no-op creates no commit;
`CommitEmpty` records the message in an empty commit instead. The message does not change Git
identities.

Automation stops on `Ready=True` or `Stalled=True`. Require `Pushed=True` and `status.commit` when a
pushed commit is required; `Ready=True` also includes successful no-commit outcomes.

## Authorship

A request's author comes from the audit event that created it, on the cluster it was created on, the
same way a watched write's does: the core looks it up in the same fact index, on the audit route of the
`GitTarget`'s `KubeProvider`, by the request's UID, and waits up to `--author-attribution-grace`. Only a
fact marked as the creating write answers: a `create`, or a `patch` answered `201 Created`
(server-side apply of a new object). A later write to the request, such as a controller patching its
labels before the request is claimed, names who made that write, so it never names the author and
never moves the request to that actor's window. It acquires the fact stream of `commitrequests` on that
route, so the fact follower reads those audit facts at all.

| Outcome | The request |
|---|---|
| attribution is off | names no author, and nothing waits for one |
| a fact names the creator | names them, in `status.author` |
| no fact, on a provider whose `AuditFactsReceived` is not `True` | names no author: the cluster sends no audit |
| no fact, on a provider that has received facts | `Ready=True`, reason `AuthorUnresolved`: its fact went missing, so it saves nothing rather than take the window that should stay unattributed |

The wait runs whenever attribution is on; whether the provider has received facts is checked only
after it, so a request is not refused while its provider's first audit event is still in transit.
The author is written in the claim, and read from there afterwards.

The author decides which window the request attaches to. The matched window decides the Git author:

- configured-author, replay, and resync windows use the configured committer;
- a resolved live attribution fact uses the authenticated actor; and
- a live attribution miss uses `unknown (attribution unresolved) <attribution-unresolved@gitops-reverser.invalid>`.

See [architecture: author identity](../architecture.md#author-and-committer-identity-in-git) for the three Git
author states.

## Lifecycle and outcomes

The owner claims the request, attaches it at once, and polls the worker.

| Outcome | Conditions |
|---|---|
| Commit pushed | `Ready=True`, `Pushed=True`, reason `Committed`; `status.commit` and `status.branch` are set |
| No same window before deadline | `Ready=True`, `Pushed=False`, reason `NoWindow` or `WindowMismatch` |
| Nothing to commit, with `whenNothingToCommit: CommitEmpty` | `Ready=True`, `Pushed=True`, reason `NoWindow` or `AlreadyPresent`; `status.commit` is the empty commit |
| Window produced no diff, and the remote agreed | `Ready=True`, `Pushed=False`, reason `AlreadyPresent` |
| Terminal write failure, withdrawal before the worker holds it, shutdown with an unresolved held write, or a target that may not be written | `Ready=False`, `Pushed=False`, `Stalled=True`, reason `FinalizeFailed` |
| Refused by the grants, or made on the wrong cluster | `Ready=False`, `Stalled=True`, reason `NotAllowed` |
| The `GitTarget` did not exist within `attachTimeout` | `Ready=False`, `Stalled=True`, reason `GitTargetNotFound` |
| An earlier save of the same author on the `GitTarget` still in progress | `Ready=False`, `Stalled=True`, reason `SaveInProgress` |
| No audit fact names the creator, on a cluster that sends them | `Ready=True`, `Pushed=False`, reason `AuthorUnresolved` |

A retryable materialization or push failure keeps a held request in `WaitingForPush`; it is not a
terminal outcome. Publication has to settle the request or the worker must establish that it will
no longer execute it before the controller can report failure.

`Ready=Unknown` with `Reconciling=True` is the normal in-progress state, with the phase the worker
reports as its reason: `Saving` (claimed, not yet registered), `WaitingForWorker`, `WaitingForWindow`,
`CollectingWindow`, `WaitingForPush`. Once the
request is `CollectingWindow` or `WaitingForPush` the worker holds it, and only the worker ends it. Before
that, a request still unresolved `attachTimeout + maxDuration + 120s` after creation is withdrawn from the
worker, which cancels it for good, and fails with `FinalizeFailed`; a GitTarget that never started a worker
is named in the message. The withdraw rides the worker's queue behind every attach, and is a no-op for a
request the worker holds, so the reported failure and the worker's answer cannot disagree.

The withdraw goes to the worker that accepted the request's attach, not to whichever worker the GitTarget
names now: a worker shared by several GitTargets outlives a deleted one, and a retired worker finishes its
last push after the manager stopped listing it. Only a request no worker holds falls back to the GitTarget,
and only then does "no GitTarget" or "no worker" mean that nothing can commit it. Once the controller starts
withdrawing it sends no further attach, and the worker keeps a request's outcome while any attach for it is
still queued, so a late attach can never register a withdrawn request again. A worker whose loop has exited,
including one that never started because its GitProvider could not be read, gives back every request it
never acted on and answers a withdraw at once.

**`Committed` and `AlreadyPresent` are decided by the push, including the no-commit one.**
(`WindowMismatch` is decided locally, at the deadline: no window was attached, so there is nothing
for a push to say. So is `NoWindow` under `Resolve`. Under `CommitEmpty` a `NoWindow` request records
its message in an empty commit, and it resolves when that commit reaches the remote.) A window that
produced no diff used to resolve `AlreadyPresent` at finalize, on the strength of the local plan.
That was only sound while every cycle fetched before it planned. It no longer does (see
[how the branch worker reads the remote](../design/push-notification-and-reconcile-trigger.md#12-the-push-is-the-read)),
so the plan may have run against a tree the remote has moved past, and the replay that follows a
rejected push can turn the same captured object into a commit. "Already present" is a claim about
the remote, so the remote settles it: a no-diff request resolves `AlreadyPresent` when the push confirms there was
nothing to add, and `Committed` when the replay produced a commit after all.

**A request whose write is decided stays identifiable until publication.** It remains attached to
that write, and the controller keeps re-sending its attach until it reads an outcome: at once on
every change the worker reports about the request (`CommitRequestChanges`), and at the latest every
ten seconds, the fallback for a change that was dropped. Before the worker registers the request,
the controller polls every two seconds, since nothing would report that the worker exists yet. The
worker keeps that attachment even when local materialization is deferred, so the re-send is
recognized as the same request: it cannot register afresh and expire into
`NoWindow` while its commit waits out the push cooldown, and it cannot claim the next same-author
window and stamp its message on somebody else's commit. Its attach deadline is spent and no longer
arms anything.

**A worker that stops while still holding the write fails the request**, because the controller
does not time out a request the worker holds, and "the worker stopped before the commit reached the
remote" says what happened.

### When the target may not be written

Two gates stop a GitTarget being written at all: `spec.suspend`, and a render-fidelity check that
has not passed. One branch worker serves every GitTarget on its branch, and each gate applies per
GitTarget, so a sibling target's save is unaffected. A third condition, an unreachable remote, is
not a gate but a retry:

| Path | Suspended | Render fidelity not established | Remote unreachable (commit or push) |
|---|---|---|---|
| Live window, no request | The window still scans, writes nothing | Writes dropped on arrival, and the window dropped at close; the next resync re-derives them | The decided write is kept and published by a later push |
| Window a request is attached to | The request fails; the window still scans | The request fails | The request stays `WaitingForPush` |
| Request no window reached, `Resolve` or `CommitEmpty` | The request fails; no empty commit | The request fails; no empty commit | `CommitEmpty`: the record is kept, `WaitingForPush` |
| A gate closes after the request's window closed | The write is pushed and the request resolves with it | Same | Same, once a push lands |

A request fails with `FinalizeFailed` and the gate as its message, under either
`whenNothingToCommit`: `NoWindow` would say the save saw no writes, and a target that drops or
suppresses its writes cannot say that. `WindowMismatch` is the exception, because another author's
window refused that request and its reason says so.

The gates are read when the window closes, which is when the write is decided and normally when it
is committed. A write decided before a gate closed is pushed rather than kept back, because a write
that never left the worker would surface later, out of order.

A decided write is kept when the remote cannot be reached, whether that happens at its commit (a
rebuild of the checkout that needs a fetch) or at its push. The worker retries on its own schedule
(10s, doubling to 5m), with parent recovery owning attempts while it remains open. A
request riding the write stays `WaitingForPush` until publication resolves it or worker shutdown
fails it. An unavailable remote can leave it pending indefinitely, but retries no longer depend on
another commit arriving. Only a failure of the write itself fails the request: a refused plan, or a
write that cannot be made. A missing parent branch is a remote that cannot take the write yet, and
holds the request the same way, and its `WaitingForPush` message says why the push has not landed,
in the words its `GitTarget` uses. While an outage has paused the branch's intake, a new request is
refused at admission and the controller sends it again; past its safety window, it fails with
`FinalizeFailed` only after withdrawal confirms that the worker does not hold it.

### Retention

A finished request (`Ready=True` or `Stalled=True`) is stamped `configbutler.ai/delete-after`, now
plus `--commit-request-ttl`, and deleted once that time has passed; the delete is conditional on the
version that was judged. The annotation is the contract: removing it keeps the request, and a value
already there is never replaced. A request that asked to catch up is deleted only once `Applied` is
settled. On the control cluster this always applies. On a source cluster only
`commitRequests: ReconcileAndDeleteFinished` stamps and deletes; with `Reconcile` nothing is stamped,
and an annotation stamped before a switch back to `Reconcile` stays without effect.

Retention belongs to the path that owns the request, or that refused it because no path serves its
`GitTarget`. Another path that deletes finished requests, such as a second provider watching the same
namespace, or the control cluster under a kubeconfig that points back at it, leaves the request
alone, whatever `delete-after` it carries. Once the `GitTarget` is gone no path owns the request,
and any path that deletes finished requests may.

### Capacity, replay, and restart limits

The budget charges every kept item its payload bytes, its message, and a fixed overhead for its
bookkeeping, so a stream of empty saves fills it too. It counts queued work, retained writes, and
requests still waiting for a window. A refusal pauses the branch's intake until a push lands; see
the [branch-worker pending-writes plan](../design/gittarget-branch-worker-pending-writes.md#recovery-contract). A refused
request is sent again by the controller.

For a request the worker already holds, saturation does not change its window membership or
replace its decided write with a newer snapshot. A refused replay entry fails its own request;
unrelated retained writes can still publish. Requests accepted but still waiting for a window
keep their existing attach deadline and no-window behavior.

An unaccepted request is retried within the controller's safety bound. Neither an API object
created successfully nor a later source snapshot proves that the worker accepted that save.
Resource watches and save commands arrive independently: a save is not a barrier covering every
Kubernetes mutation before its creation.

Pausing the live worker preserves accepted work in memory. A graceful stop settles what it holds
before the process exits: every worker closes its windows, pushes, and settles the saves it carries,
and once every worker has stopped the controller writes each settled outcome to its request's status
through the path that attached it, as one more conditional step (`CommitRequestsInFlight.Settle`).
Both are bounded (10s for the workers, 5s for the statuses) to fit the chart's 20s grace period; what
runs over is left as a crash leaves it. A request still waiting for a window, or whose attach was
still queued, is not committed; the next process attaches it again from its status. Restarting the
process does not otherwise preserve its attachments or outcome receipts: nothing is persisted.
Startup replays current watched state, subject to pruning, but cannot recreate the same save history.
A push accepted immediately before a crash can therefore be followed by another empty commit when
the request is resent. A lost push response also needs remote
evidence; equal tree content alone cannot prove that an empty save's message was published. Durable
save recovery belongs to the [future HA plan](../future/ha-gittarget-distribution-plan.md).

The complete status vocabulary is in the [status conditions guide](status-conditions-guide.md).
