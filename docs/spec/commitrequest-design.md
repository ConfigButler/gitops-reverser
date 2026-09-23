# CommitRequest window finalization

> **spec** — current behaviour. The code depends on this document; change one, change the other.
> Index: [`../INDEX.md`](../INDEX.md)

A `CommitRequest` is a one-shot “save now” command for one `GitTarget`. It does not mirror the
`CommitRequest` object. Instead, it can attach a message to a matching open commit window and asks the
worker to close that window after the requested collect delay.

## Request and window contract

The request identifies the target in `spec.gitTargetRef.name`, may provide `spec.message`, and sets
`spec.closeDelay` (a Go duration string, at most `5m`, default `"2s"`). It is handled by the target’s single branch worker,
so resource events and the attach request share one FIFO.

The worker attaches a request only when all of these match an open window:

1. GitTarget name and namespace;
2. the named actor, if either side has one; and
3. whether each side names an actor (`AttributionOutcome.NamesActor`).

The third rule keeps independently configured command admission and live-resource attribution from being
coupled. A request with no named submitter can attach to either a configured-author or an unresolved live
window, but never to a named actor’s window. A request with a named submitter can attach only to that
actor’s named window. Therefore one user’s request never finalizes another user’s work.

On its first receipt, the worker sets the deadline to receipt plus `closeDelay`. Repeated reconciles
are idempotent and keep that first deadline. Time spent waiting for a matching window consumes the delay.

The default is `2` rather than `0` because the write a request exists to publish reaches the worker
strictly after the request does: a watch event is held until its audit fact arrives, so a zero
deadline is shorter than the smallest window-open latency the pipeline can produce. The field is a
pointer, so an omitted value and an explicit `0` stay distinguishable — `0` still means finalize on
the next pass of the event loop.
Normal flush triggers can close an attached window early, carrying its message. Each request claims
at most one window and cannot rename a finalized commit, including one waiting for push. A bundle
provides no ordering guarantee; use a non-zero window for custom save messages. The delay does not
reserve a transaction. Competing requests keep the earliest-finalize-deadline selection policy.

`spec.message` is literal, including template-like text and surrounding spaces. It is never parsed
as a template, so a request author cannot execute one. Omission uses
`GitTarget.spec.commit.message.liveTemplate`. A target may set
`GitTarget.spec.commit.message.requestTemplate` to frame the message with the window's resources;
the message still arrives unaltered, as `.RequestMessage`, and a template that does not render it
is rejected at admission, so the request's bytes always reach the commit. A `requestTemplate` that
fails to render commits the message verbatim rather than losing the window. A present value accepts 1–1024 Unicode characters;
newline is allowed, other ASCII controls and whitespace-only text are rejected. Validation never
truncates accepted text. A no-op still creates no commit. The message does not change Git identities.

Automation stops on `Ready=True` or `Stalled=True`. Require `Pushed=True` and `status.sha` when a
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
| Commit pushed | `Ready=True`, `Pushed=True`, reason `Committed`; `status.sha` and `status.branch` are set |
| No same window before deadline | `Ready=True`, `Pushed=False`, reason `NoWindowInGrace` or `WindowMismatch` |
| Window produced no diff, and the remote agreed | `Ready=True`, `Pushed=False`, reason `AlreadyPresent` |
| Finalize or push error | `Ready=False`, `Pushed=False`, `Stalled=True`, reason `FinalizeFailed` |

`Reconciling=True` with reason `WaitingForCloseDelay` is the normal in-progress state. The controller fails
with `FinalizeFailed` only if the worker does not resolve the request within its bounded safety window; it
never polls indefinitely.

**`Committed` and `AlreadyPresent` are decided by the push, including the no-commit one.**
(`NoWindowInGrace` and `WindowMismatch` are decided locally, at the deadline: no window was claimed,
so there is nothing for a push to say.) A window that produced no diff used to resolve
`AlreadyPresent` at finalize, on the strength of the local plan. That was only sound while every
cycle fetched before it planned. It no longer does (see
[inbound push notification](../design/push-notification-and-reconcile-trigger.md) §3), so the plan may have run
against a tree the remote has moved past, and the replay that follows a rejected push can turn the
same captured object into a real commit. "Already present" is a claim about the remote, so the
remote settles it: a no-diff request resolves `AlreadyPresent` when the push confirms there was
nothing to add, and `Committed` when the replay produced a commit after all.

**A request whose window has committed stays identifiable until then.** It is neither pending nor
resolved in that interval, and the controller keeps re-sending its attach every couple of seconds
until it reads an outcome. The worker marks such a request committed rather than forgetting it, so
the re-send is recognized as the same request: it cannot register afresh and expire into
`NoOpenWindow` while its commit waits out the push cooldown, and it cannot claim the next
same-author window and stamp its message on somebody else's commit. Its close deadline is spent and
no longer arms anything.

**A worker that stops while still holding the write fails the request**, rather than leaving the
controller to poll until its own safety window expires: a timeout says nothing about what happened,
and "the worker stopped before the commit reached the remote" does.

The complete status vocabulary is in the [status conditions guide](status-conditions-guide.md).
