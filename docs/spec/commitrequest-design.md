# CommitRequest window finalization

> **spec** — current behaviour. The code depends on this document; change one, change the other.
> Index: [`../INDEX.md`](../INDEX.md)

A `CommitRequest` is a one-shot “save now” command for one `GitTarget`. It does not mirror the
`CommitRequest` object. Instead, it can attach a message to a matching open commit window and asks the
worker to close that window after the requested collect delay.

## Request and window contract

The request identifies the target in `spec.targetRef.name`, may provide `spec.message`, and sets
`spec.closeDelaySeconds` (0–300 seconds). It is handled by the target’s single branch worker, so resource
events and the attach request share one FIFO.

The worker attaches a request only when all of these match an open window:

1. GitTarget name and namespace;
2. the named actor, if either side has one; and
3. whether each side names an actor (`AttributionOutcome.NamesActor`).

The third rule keeps independently configured command admission and live-resource attribution from being
coupled. A request with no named submitter can attach to either a configured-author or an unresolved live
window, but never to a named actor’s window. A request with a named submitter can attach only to that
actor’s named window. Therefore one user’s request never finalizes another user’s work.

On its first attach, the worker sets the deadline to receipt plus `closeDelaySeconds`. Repeated reconciles
are idempotent and keep that first deadline. The delay lets a `kubectl apply` bundle whose `CommitRequest`
arrives before its resource mutations collect into the same window.

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
| Window produced no diff | `Ready=True`, `Pushed=False`, reason `AlreadyPresent` |
| Finalize or push error | `Ready=False`, `Pushed=False`, `Stalled=True`, reason `FinalizeFailed` |

`Reconciling=True` with reason `WaitingForCloseDelay` is the normal in-progress state. The controller fails
with `FinalizeFailed` only if the worker does not resolve the request within its bounded safety window; it
never polls indefinitely.

The complete status vocabulary is in the [status conditions guide](status-conditions-guide.md).
