# Commit windows and saves: one configuration surface

> **design**: a proposal for the PR after #403, not built. Index: [`../INDEX.md`](../INDEX.md)
>
> The short version: a commit window is opened only by writes and closes on two timers. A save (a
> `CommitRequest`) attaches to one window and replaces both timers for it, longer or shorter. Both
> kinds describe the window in one `window` block with the same words, and a save adds which window
> it attaches to and whether it records its message when nothing changed. It ships as one coherent
> `v1alpha4`:
>
> ```yaml
> apiVersion: configbutler.ai/v1alpha4    apiVersion: configbutler.ai/v1alpha4
> kind: GitTarget                         kind: CommitRequest
> spec:                                   spec:
>   commit:                                 message: "fix: raise the checkout memory limit"
>     window:                               window:
>       idleTimeout: 5s                       attach: CurrentOrNext   # or Next
>       maxDuration: 1m                       attachTimeout: 2s
>                                             idleTimeout: 1s         # omitted: no idle close
>                                             maxDuration: 10s
>                                           emptyCommit: Skip         # or Create
> ```
>
> It replaces `CommitRequest.spec.closeDelay` and the string `GitTarget.spec.commit.window`, and it
> supersedes the timer model in [`commitrequest-save-wait-options.md`](commitrequest-save-wait-options.md#the-timer-model).

## Why

Today one field, `closeDelay`, both bounds a save's wait for a window and, since #403, its
collection after the attach. The two need different sizes: the wait covers audit latency and the
client's time to its first write, and the collection covers the span of the save's writes. The
target's silence timer still runs on an attached window, so a save asking for 30 seconds gets at
most the target's 5 seconds of silence, silently. Nothing but the memory limit caps a busy window. A
save cannot choose to skip the author's earlier edits. And the words do not match: the target's
timer is called `window`, the save's `closeDelay`, and the worker's code calls it a "grace" and
says both "attach" and "claim".

## Principles

These are decided.

1. **Only writes open a window.** A commit window opens on an author's first write to a target and
   collects that author's writes until it closes. Nothing else opens one: not a save, not a timer.
   A save that opens its own window, so the user has one to respond into, was weighed and rejected:
   it would cut off the window already open, create windows without writes, and give a save two ways
   to start collecting. `attach: Next`, `attachTimeout` and `emptyCommit: Create` cover what it was
   for.
2. **A save attaches; it never opens.** It attaches to at most one window and gives that window its
   message and its timers. "Attach" is the one word for the act, in the API, the code and the docs.
3. **The save's timers replace the target's, whole.** For the window a save attaches to, the save's
   `idleTimeout` and `maxDuration` apply and the target's do not. A save may lengthen or shorten
   either. The target's settings are the automatic policy, not a ceiling on explicit saves.
4. **Every timer is named for what runs out:** `attachTimeout`, `idleTimeout`, `maxDuration`. The two
   collection timers mean the same thing on both kinds.
5. **Choices are enums.** A boolean that later needs a third state is a breaking change, and an enum
   value names the behavior it selects (Kubernetes API conventions).
6. **A save never widens what it may take.** It finalizes its own submitter's work and nobody else's.
   No setting here changes which windows match.
7. **Settings are snapshotted.** A window takes the target's timers when it opens; a save takes its
   own when the worker first registers it. Editing a `GitTarget` never moves a deadline already
   running. A `CommitRequest`'s whole spec stays immutable, the new fields included.

## The surface

### `GitTarget.spec.commit.window`

| Field | Meaning | Default | Bound |
|---|---|---|---|
| `idleTimeout` | close after this much silence; each write restarts it | `5s` | `24h` |
| `maxDuration` | close this long after the window opened, whatever arrives | `1m` | `24h` |

The finite `maxDuration` default is a product choice: continuous activity should not postpone a
commit until memory pressure forces one. One minute is a starting value to validate, not a
measurement.

### `CommitRequest.spec.window`

| Field | Meaning | Default | Bound |
|---|---|---|---|
| `attach` | which window the save covers: `CurrentOrNext` or `Next` | `CurrentOrNext` | enum |
| `attachTimeout` | stop waiting for a window this long after the worker registers the save | `2s` | `5m` |
| `idleTimeout` | close the attached window after this much silence | omitted: no idle close | `5m` |
| `maxDuration` | close the attached window this long after the attach | `2s` | `5m` |

The default is a fixed two-second cutoff, so an explicit save finishes promptly. Rolling collection
is one setting away: `idleTimeout: 1s, maxDuration: 10s` collects a save whose writes trickle in and
still ends. CEL: `idleTimeout` must not exceed `maxDuration`.

### `CommitRequest.spec.emptyCommit`

`Skip` (default) or `Create`. It sits outside `window` because it decides the commit, not the
window. CEL: `Create` requires `spec.message`, because a message is the only thing an empty commit
records.

## Clock rules

- A window's `maxDuration` starts when a write opens it. A save's starts when it attaches.
- A save's attach starts its first idle interval. Each later matching write restarts the idle
  interval only; nothing restarts `maxDuration`.
- A repeated delivery of the same save (the controller re-sends until it reads an outcome) restarts
  nothing and is recognized by the save's identity.
- The first deadline reached closes the window. An author or target change, the memory limit, a
  drain before a resync or atomic apply, and shutdown can close it earlier; a save does not change
  those.
- A deadline bounds when the window is finalized. The push follows, with its cooldown and retries,
  and no timer here promises when it lands.

### Zero and omission

Each value means one thing, and none borrows from another:

| Setting | Meaning |
|---|---|
| `attachTimeout: 0s` | attach to an eligible window already open, or give up at once; do not wait |
| `maxDuration: 0s` | finalize right after the attach |
| `idleTimeout` omitted (save) | no idle close; `maxDuration` alone ends collection |
| `idleTimeout: 0s` | close on the first silence, which is right after the write that opened or extended it |

A zero `attachTimeout` never forces a zero collection: a save that attaches immediately still
collects for its `maxDuration`.

Two rules keep the edges honest:

- **An expired save cannot take a later window.** Once `attachTimeout` has run out, the save
  resolves; a window that opens afterwards is not its window. When the deadline and a matching write
  are ready on the same loop wake, the expiry is decided first. This is #403's boundary test.
- **A save gets its attach before a zero timer fires.** With the target's `idleTimeout: 0s` the
  worker finalizes a window in the same step the write opens it, so a waiting save never sees it.
  The worker must offer a new window to waiting saves before it applies the target's timers.
  Otherwise the promised override vanishes in exactly that configuration.

## `attach: CurrentOrNext` or `Next`

- **`CurrentOrNext`** attaches to the author's window if one is open at registration, and otherwise
  to the next one a write opens. It is what a save made *after* its writes wants: the README example
  applies the resources, then creates the request.
- **`Next`** starts clean. If the author has an eligible window open at registration, the worker
  closes it first, and the save waits for a write to open another. It is for a save created
  *before* its writes, which should not sweep up the author's earlier, unrelated edits.

`Next` has three rules:

- **The closed window keeps its message.** If another save already attached to it, that save's
  message goes with it, and that save resolves exactly as if its own deadline had fired.
- **The close happens once per save.** It is keyed by the save's identity, so a controller re-send
  never closes a second window.
- **Competing saves are served first come, first served,** in the order the worker first registered
  them. A window carries at most one save; the next waits for the next window.

What `Next` guarantees is narrower than its name suggests. It separates work the worker already
collected from work that reaches the worker afterwards. It cannot prove a write was made after the
request: a write still held for its audit fact arrives later and lands in the next window, and
waiting for the previous save to resolve does not exclude a delayed write from that save either.
That is acceptable for this feature, and the docs say so plainly. A save that must cover exactly
one write is what the named-write wait in the save-wait design is for.

## `emptyCommit: Create`

With `Create`, a save records its message even when its writes changed nothing:

| Outcome | `Skip` (default) | `Create` |
|---|---|---|
| Eligible writes changed files | commit | commit |
| Eligible writes already matched Git | no commit, `AlreadyPresent` | empty commit, `AlreadyPresent` |
| No eligible window before `attachTimeout` | no commit, `NoWindow` | empty commit, `NoWindow` |
| Only another author's window was open | no commit, `WindowMismatch` | no commit, `WindowMismatch` |
| Commit or push failed, or the target is suspended | the failure | the failure |

The cause stays in the `Ready` reason, and `status.commit` with `Pushed=True` says a commit was
recorded. A caller reads both: a hash proves the message is in Git, and `NoWindow` says the save saw
no writes, so a write that was delayed past `attachTimeout` is not in that commit. A mismatch never
falls back to an empty commit, because recording a save while another author's work is in flight
would make the history claim more than happened.

The empty commit is not a window, and principle 1 holds. Its author follows the same rule as any
commit: the request's submitter when admission captured one, otherwise the committer. Empty commits
already exist in this codebase: #383 reverts refused edits with one.

**Duplicates across a restart are possible, and documented.** The worker suppresses a repeated save
while it runs. A crash after the push and before the status write loses that memory, and the
re-sent save can record a second empty commit. Preventing that needs durable recovery state, which
this feature does not justify.

## Status

Conditions stay the interface. The progress reasons under `Reconciling=True` become
`WaitingForWindow` (before the attach), `CollectingWindow` (after it) and `WaitingForPush` (committed
locally, not yet on the remote). The terminal `Ready` reasons tell apart an ordinary commit
(`Committed`), the two no-change causes (`AlreadyPresent`, `NoWindow`, which replaces
`NoWindowInGrace`), `WindowMismatch`, and failures. `Pushed=True` and `status.commit` appear only
after the remote confirmed the push.

Every one of these comes from the worker. Sending an attach does not prove an attach, so the
controller reports a phase only when the worker says the save reached it.

The controller's safety timeout covers the largest `attachTimeout`, the largest `maxDuration`, and
the push cooldown and retries.

## Naming

Every system that batches has the same timers, and a few also wait for the batch to start:

| Tradition | Resets on activity | Fixed cap | Waiting to start |
|---|---|---|---|
| Network proxies (Envoy, Gateway API, NGINX) | `idleTimeout` | `maxDuration` | `connectTimeout` |
| Debounce (lodash, RxJS) | `wait` | `maxWait` | n/a |
| Batch exporters (OpenTelemetry, Kafka) | n/a | `timeout`, `linger.ms` | n/a |
| Alert grouping (Alertmanager) | `group_interval` | n/a | `group_wait` |
| Kubernetes, Flux | n/a | `activeDeadlineSeconds`, `timeout` | n/a |

The proxy words are the ones operators already know, and "timeout" is Flux's. "Idle" means "resets
on activity" to almost every reader; "deadline" reads as a point in time; the debounce words collide
with "wait" as a verb.

- **The block is `window`,** the word [`definitions.md`](../definitions.md) already defines, on both
  kinds. The timers are properties of the window.
- **Full names:** `idleTimeout` and `maxDuration`. Inside `window` nothing stutters, and a lone `max`
  is unusual in Kubernetes APIs.
- **"Attach", not "claim".** "Claimed" already names a rule's demand on the read side.
- **`CurrentOrNext`, not `Current`.** The longer value states the fallback instead of hiding it.

Rejected: `windowTimeouts` and `windowPolicy` (both existed only to avoid changing a stored field's
type), flat fields on `commit` (they lose their subject), `batching` (drops the defined word), and
`closeDelay` as the cutoff (names an action, not a duration).

## Plan

One `v1alpha4` change, built in three steps on one branch:

1. **Timers.** The `window` blocks, the clock rules, the zero table, attach before a zero timer, the
   snapshot, the target's `maxDuration`. `closeDelay` and the string `window` are gone.
2. **Attach selection.** `CurrentOrNext` and `Next`, the once-per-identity close, first-come
   ordering, and the split progress reasons with worker-sourced observations.
3. **Empty commits.** `emptyCommit`, the message requirement, the outcome table, and the documented
   restart limitation.

Alongside: "grace" and "claim" leave the commit path's code and comments; the finalize-reason
metric label splits into `idle-timeout` and `max-duration`, and `Next`'s close gets its own reason,
recorded in [`interpreting-metrics.md`](../interpreting-metrics.md); [`definitions.md`](../definitions.md)
gains "only writes open a window" and "attach"; the save-wait design's timer model points here.

Tests:

- Omitted blocks, `{}`, partial blocks, every zero value, and duration round-trips.
- The whole request spec is immutable, including `attach` and `emptyCommit`.
- A save's `idleTimeout` longer and shorter than the target's; a save with only `maxDuration` is not
  closed by the target's idle timer; continuous activity stops at `maxDuration` on both kinds.
- Expiry versus a matching write on the same wake; a waiting save attaches under the target's
  `idleTimeout: 0s`; `attachTimeout: 0s` with an open window still collects.
- Overlapping saves in first-come order; a re-send restarts nothing and closes nothing; `Next`
  keeps another save's message on the window it closes.
- A forced close (author change, memory limit, drain) of an attached window; push replay; each row
  of the empty-commit table, and a restart between push and status.
- The upgrade, once, against stored `v1alpha3` objects.

## Open questions

- **Is one minute the right target `maxDuration`?** It needs a look at real write bursts; the
  decision that matters is that it is finite.
- **What else rides the `v1alpha4` bump?** Any break the vocabulary cleanup still owes costs less in
  the same version change than in the next one.

## Migration

No production users outside our control, so this stays small. All CRDs move to `v1alpha4` together,
with no conversion webhook and no aliases. The version label alone does not rewrite stored fields:
with `None` conversion a stored string `commit.window` still fails to decode, so the upgrade rewrites
stored objects into the new shape before the new CRDs serve them, and deletes finished
`CommitRequest`s rather than converting them. That procedure is tested once against real stored
`v1alpha3` objects and written up in UPGRADING.
