# CommitRequest window finalization

> **spec**: current behavior, reviewed at `373bf8d7` on 2026-10-03.
> The code depends on this document; change one, change the other.
> Index: [`../INDEX.md`](../INDEX.md)

A `CommitRequest` is a one-shot “save now” command for one `GitTarget`. It does not mirror the
`CommitRequest` object. Instead, it can attach a message to a matching open commit window and asks the
worker to close that window after the requested collect delay.

## Request and window contract

The request identifies the target in `spec.gitTargetRef.name`, may provide `spec.message`, and sets
`spec.window` (which window to attach to, how long to wait for one, and the timers that close it) and
`spec.whenNothingToCommit`. It is handled by the target’s single branch worker,
so resource events and the attach request share one FIFO.

The worker attaches a request only when all of these match an open window:

1. GitTarget name and namespace;
2. the named actor, if either side has one; and
3. whether each side names an actor (`AttributionOutcome.NamesActor`).

The third rule keeps independently configured command admission and live-resource attribution from being
coupled. A request with no named submitter can attach to either a configured-author or an unresolved live
window, but never to a named actor’s window. A request with a named submitter can attach only to that
actor’s named window. Therefore one user’s request never finalizes another user’s work.

A window is only ever opened by a write; a request attaches to one and never opens one. On first
registration, the worker sets the attach deadline to registration plus `window.attachTimeout`, and
repeated reconciles are idempotent: they restart nothing and, for `attach: Next`, close nothing again.
With `CurrentOrNext` the request attaches to the author's window already open at registration, before
any deadline is consulted, so `attachTimeout: 0s` can attach at all. Otherwise it waits, and waiting
requests are served first come, first served. A request whose deadline has passed resolves and never
takes a later window; when its deadline and a matching write are ready on the same wake, expiry wins.

On attach, the request's `idleTimeout` and `maxDuration` replace the window's timers: `maxDuration`
runs from the attach, and so does the first idle interval. A waiting request gets a new window before
the target's own timers are applied, so a target with `idleTimeout: 0s` does not close the window
before the request can attach.

`attachTimeout` defaults to `"2s"` rather than `"0s"` because the write a request exists to publish
reaches the worker strictly after the request does: a watch event is held until its audit fact
arrives, so a zero wait is shorter than the smallest window-open latency the pipeline can produce.
The two durations are separate settings with separate clocks: `attachTimeout` runs from
registration, `maxDuration` from the attach. Each is a pointer, so an omitted value and an explicit
`"0s"` stay distinguishable. A zero `attachTimeout` attaches to a window already open, or gives up at
once, and never shortens the collection that follows; a zero `maxDuration` finalizes right after the
attach.
Other flush triggers can close an attached window early, carrying its message. Each request attaches
to at most one window and cannot rename a finalized commit, including one waiting for push. A bundle
provides no ordering guarantee. The wait does not reserve a transaction. Competing requests are
served in registration order, first come, first served.

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

The validating admission webhook captures a command submitter before the `CommitRequest` persists. The
controller performs one best-effort, present-or-never lookup:

| Admission result | `AuthorAttributed` | Request claim |
|---|---|---|
| submitter record found | `True` / `AttributedFromAdmission` | that named actor |
| capture ran but no record | `False` / `CommitterFallback` | no actor |
| webhook disabled or Redis unavailable | `False` / `AuthorCaptureDisabled` | no actor |

`AuthorAttributed=False` is not a statement about the eventual Git author. The matched live window decides
that:

- configured-author, replay, and resync windows use the configured committer;
- a resolved live attribution fact uses the authenticated actor; and
- a live attribution miss uses `unknown (attribution unresolved) <attribution-unresolved@gitops-reverser.invalid>`.

See [CommitRequest admission authorship](commitrequest-admission-authorship.md) for capture provenance and
[architecture: author identity](../architecture.md#author-and-committer-identity-in-git) for the three Git
author states.

## Lifecycle and outcomes

The controller stamps its conditions on first reconcile, immediately attaches the request, and polls the
worker. There is no audit wait for a `CommitRequest`; a delayed admission record cannot arrive after the
object is visible.

| Outcome | Conditions |
|---|---|
| Commit pushed | `Ready=True`, `Pushed=True`, reason `Committed`; `status.commit` and `status.branch` are set |
| No same window before deadline | `Ready=True`, `Pushed=False`, reason `NoWindow` or `WindowMismatch` |
| Nothing to commit, with `whenNothingToCommit: CommitEmpty` | `Ready=True`, `Pushed=True`, reason `NoWindow` or `AlreadyPresent`; `status.commit` is the empty commit |
| Window produced no diff, and the remote agreed | `Ready=True`, `Pushed=False`, reason `AlreadyPresent` |
| Terminal write failure, withdrawal before the worker holds it, shutdown with an unresolved held write, or a target that may not be written | `Ready=False`, `Pushed=False`, `Stalled=True`, reason `FinalizeFailed` |

A retryable materialization or push failure keeps a held request in `WaitingForPush`; it is not a
terminal outcome. Publication has to settle the request or the worker must establish that it will
no longer execute it before the controller can report failure.

`Reconciling=True` is the normal in-progress state, with the phase the worker reports as its reason:
`Progressing`, `WaitingForWorker`, `WaitingForWindow`, `CollectingWindow`, `WaitingForPush`. Once the
request is `CollectingWindow` or `WaitingForPush` the worker holds it, and only the worker ends it. Before
that, a request still unresolved `attachTimeout + maxDuration + 120s` after creation is withdrawn from the
worker, which cancels it for good, and fails with `FinalizeFailed`; a GitTarget that never started a worker
is named in the message. The withdraw rides the worker's queue behind every attach, and is a no-op for a
request the worker holds, so the controller's failure and the worker's answer cannot disagree.

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
[inbound push notification](../design/push-notification-and-reconcile-trigger.md) §3), so the plan may have run
against a tree the remote has moved past, and the replay that follows a rejected push can turn the
same captured object into a commit. "Already present" is a claim about the remote, so the
remote settles it: a no-diff request resolves `AlreadyPresent` when the push confirms there was
nothing to add, and `Committed` when the replay produced a commit after all.

**A request whose write is decided stays identifiable until publication.** It remains attached to
that write, and the controller keeps re-sending its attach every couple of seconds until it reads
an outcome. The worker keeps that attachment even when local materialization is deferred, so
the re-send is recognized as the same request: it cannot register afresh and expire into
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
(10s, doubling to 5m), with parent recovery owning attempts while its obligation remains active. A
request riding the write stays `WaitingForPush` until publication resolves it or worker shutdown
fails it. An unavailable remote can leave it pending indefinitely, but retries no longer depend on
another commit arriving. Only a failure of the write itself fails the request: a refused plan, or a
write that cannot be made. A missing parent branch is a remote that cannot take the write yet, and
holds the request the same way. While an outage has filled the branch's retained-byte budget, a new
request is refused at admission and the controller sends it again; past its safety window, it fails
with `FinalizeFailed` only after withdrawal confirms that the worker does not hold it.

### Capacity, replay, and restart limits

The current budget uses retained payload bytes. Empty save records contribute zero bytes, so this
threshold alone cannot bound a stream of empty saves. The
[branch-worker log plan](../design/gittarget-branch-worker-log.md#recovery-contract) specifies the
remaining byte/count accounting and explicit producer pause. That behavior is planned; current
enqueue refusal and controller retry remain the runtime contract.

For a request the worker already holds, saturation does not change its window membership or
replace its decided write with a newer snapshot. A refused replay entry fails its own request;
unrelated retained writes can still publish. Requests accepted but still waiting for a window
keep their existing attach deadline and no-window behavior.

An unaccepted request is retried within the controller's safety bound. Neither an API object
created successfully nor a later source snapshot proves that the worker accepted that save.
Resource watches and save commands arrive independently: a save is not a barrier covering every
Kubernetes mutation before its creation.

Pausing the live worker preserves accepted work in memory. Restarting the process does not preserve
its attachments or outcome receipts. Startup replays current watched state, subject to pruning,
but cannot recreate the same save history. A push accepted immediately before a crash can therefore be
followed by another empty commit when the request is resent. A lost push response also needs remote
evidence; equal tree content alone cannot prove that an empty save's message was published. Durable
save recovery belongs to the [future HA plan](../future/ha-gittarget-distribution-plan.md).

The complete status vocabulary is in the [status conditions guide](status-conditions-guide.md).
