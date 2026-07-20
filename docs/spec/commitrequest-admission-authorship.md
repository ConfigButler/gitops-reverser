# CommitRequest authorship from admission

> **spec** — current behaviour. The code depends on this document; change one, change the other.
> Index: [`../INDEX.md`](../INDEX.md)

A `CommitRequest` is a command to close an existing GitTarget commit window. Its submitter is captured
by the `/validate-operator-types` validating admission webhook, while mirrored-resource attribution is
separately derived from kube-apiserver audit facts. The two paths answer different questions and must not
be conflated.

## Contract

| Concern | CommitRequest submitter | Mirrored-resource author |
|---|---|---|
| Source | validating admission request `userInfo` | post-persist audit fact joined to watch event |
| Key | CommitRequest UID | source provider, GVR, UID, and resourceVersion |
| Timing | written before the object persists | waits up to `--author-attribution-grace` after the watch event |
| Miss | request claims no actor | watch commit has the explicit unresolved author |

The admission handler records the authenticated submitter in the Redis command-author store and always
allows the request. It is deliberately best-effort: a Redis failure or an unavailable webhook must not
reject a user's command. The controller reads the record when the persisted `CommitRequest` is first
reconciled. The result is **present-or-never**; waiting cannot create a record that admission did not write.

## Attach and final Git author

`AuthorAttributed=True` means admission captured the command submitter. The request can attach only to an
open window with that same named actor and GitTarget.

`AuthorAttributed=False` (`CommitterFallback`) means no admission record was available. It is not a
failure and does not mean the eventual Git author is necessarily the configured committer. The request
claims no actor and can attach only to an unnamed window. That window determines the actual Git author:

- configured-author mode or a replay/resync write: the configured committer;
- live attribution that ran but found no usable audit fact:
  `unknown (attribution unresolved) <attribution-unresolved@gitops-reverser.invalid>`.

The worker never closes another actor's window. If no matching window appears before the close delay
expires, the request ends successfully with `Ready=True`, `Pushed=False`, and `NoWindowInGrace` or
`WindowMismatch`.

## Conditions

`AuthorAttributed` is binary and settled on the first reconcile; it has no audit-wait or `Unknown` state.

| Condition | True | False |
|---|---|---|
| `AuthorAttributed` | `AttributedFromAdmission`: the command submitter was captured | `CommitterFallback`: no command-author record; request claims no actor |
| `Pushed` | the attached window was committed and pushed | a benign no-commit or finalize failure |
| `Ready` | a pushed commit or benign no-commit | progress or a finalize failure |

`Reconciling=True` with `WaitingForCloseDelay` is the only normal in-progress state. It covers the
optional collect delay followed by worker finalization and push.

## Wiring

The command-author store is wired when the admission webhook is enabled and Redis is configured. It is
independent of `--author-attribution`, which controls audit-backed authorship for live watched resources.
An installation may therefore have either, both, or neither source of author information.

Related live contracts:

- [architecture: CommitRequest Finalize](../architecture.md#commitrequest-finalize)
- [configuration: CommitRequest conditions](../configuration.md#commitrequest-finalize)
- [status conditions guide](status-conditions-guide.md)
