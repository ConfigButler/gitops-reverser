# Commit windows and saves: one configuration surface

> **design**: a proposal for the PR after #403, not built. Index: [`../INDEX.md`](../INDEX.md)
>
> The short version: a commit window is opened only by writes, and it closes on two timers. A save
> (`CommitRequest`) attaches to one window and may replace both timers for it. Both kinds describe
> the window in one block with the same words, and the save adds how it attaches and whether it
> commits when nothing came in:
>
> ```yaml
> # GitTarget                    # CommitRequest
> spec:                          spec:
>   commit:                        window:
>     window:                        attach: Current     # or Next
>       idleTimeout: 5s              attachTimeout: 2s
>       maxDuration: 10m             idleTimeout: 1s
>                                    maxDuration: 30s
>                                  emptyCommit: Skip     # or Create
> ```
>
> It replaces `CommitRequest.spec.closeDelay` and the string `GitTarget.spec.commit.window`, and it
> builds on the timer model decided in [`commitrequest-save-wait-options.md`](commitrequest-save-wait-options.md).

## What decides when a window closes today

After #403:

| What | Field | Starts from | Resets on a matching write? | Default |
|---|---|---|---|---|
| Target silence | `GitTarget.spec.commit.window` | each write | yes | `5s`, at most `24h` |
| Request wait | `CommitRequest.spec.closeDelay` | receipt by the branch worker | no | `2s`, at most `5m` |
| Request collection | the same `closeDelay` | the attach | no | the same value |

Other boundaries close a window regardless of any timer: a change of author or target, the branch
buffer's memory limit (`buffer-limit`), a resync or atomic apply that needs the window drained
first, and shutdown. The `windowFinalizeReason` values in
[`branch_worker.go`](../../internal/git/branch_worker.go) name them all.

## What does not line up

**One field, two jobs.** `closeDelay` bounds both the wait for a window and the collection after
the attach. The two need different sizes. The wait covers the cluster's audit latency plus the
client's time to its first write; the collection covers the span of the save's own writes.

**A save cannot hold a window open longer than the target allows.** The target's silence timer
still runs on an attached window. With the default `5s`, a `closeDelay: 30s` collects for at most 5
seconds of silence, and nothing says so.

**Nothing caps a busy window except memory.** The target's timer only resets. Continuous activity
keeps a window open until `buffer-limit` trips.

**A save cannot choose which window it covers.** It always takes the window open at receipt, which
may hold the author's earlier, unrelated edits.

**A save can end with no commit, three ways.** `NoWindowInGrace`, `WindowMismatch` and
`AlreadyPresent` all resolve `Ready=True` with nothing in Git. A UI that shows "saved as `abc123`"
must handle each one.

**The words do not match.** The target's timer is called `window`, though it holds only the
window's silence timeout. The save's is `closeDelay`. The worker's code calls it a "grace", which
[`definitions.md`](../definitions.md) reserves for the attribution wait. The code says both
"attach" and "claim" for the same act, and "claimed" already names something on the read side.

## Principles

1. **Only writes open a window. Decided.** A commit window opens on the first write of an author to
   a target and collects that author's writes until it closes. Nothing else opens one: not a save,
   not a timer. This is today's behaviour, stated as a rule so the save's settings can lean on it.

   The alternative was weighed and rejected: a save that opens its own window, so the user gets a
   window to respond into. It would have to cut off whatever window is open at that moment, it would
   create windows that contain no writes, and it would give a save two ways to start collecting
   instead of one. What it was for is covered without it: `attach: Next` gives a save a fresh start,
   `attachTimeout` gives the user time to make the first write, and `emptyCommit: Create` records
   the message when nothing comes.
2. **A save attaches; it never opens.** It waits for a window it may attach to, attaches to at most
   one, and gives that window its message and its timers. "Attach" is the one word for this, in the
   API, the code and the docs.
3. **The save's timers replace the target's, whole.** For the window a save attaches to, the save's
   `idleTimeout` and `maxDuration` apply and the target's do not. A save may lengthen or shorten
   either. Author or target changes, `buffer-limit`, drain-before-apply and shutdown remain
   boundaries; a save changes only the timers.
4. **Every timer is named for what runs out.** `attachTimeout`, `idleTimeout`, `maxDuration`.
5. **Choices are enums, not booleans.** Per the Kubernetes API conventions: a boolean that later
   needs a third state is a breaking change, and an enum value names the behaviour it selects.
6. **A save never widens what it may take.** It finalizes its own submitter's work and nobody
   else's. No setting here changes which windows match.

## The surface

### `GitTarget.spec.commit.window`

| Field | Meaning | Default | Bound |
|---|---|---|---|
| `idleTimeout` | close after this much silence; resets on each write | `5s`; `0s` commits every write on its own | `24h` |
| `maxDuration` | close this long after the window opened, whatever arrives | none | `24h` |

### `CommitRequest.spec.window`

| Field | Meaning | Default | Bound |
|---|---|---|---|
| `attach` | which window the save covers: `Current` or `Next` (below) | `Current` | enum |
| `attachTimeout` | stop waiting for a window this long after receipt | `2s` | `5m` |
| `idleTimeout` | close the attached window after this much silence | none: no idle close | `5m` |
| `maxDuration` | close the attached window this long after the attach | `2s` | `5m` |

CEL: `idleTimeout` must not exceed `maxDuration`. The defaults give today's fixed cutoff: no idle
close, close 2 seconds after the attach. A save that wants rolling collection sets `idleTimeout`,
and `maxDuration` still ends it.

### `CommitRequest.spec.emptyCommit`

`Skip` (default) or `Create`. It sits outside `window` because it decides the commit, not the
window.

### `attach: Current` or `Next`

- **`Current`** attaches to the author's window if one is open at receipt, and otherwise to the next
  one that opens. This is today's behaviour, and what a save made *after* its writes wants: the
  README example applies the resources, then creates the request.
- **`Next`** starts clean. If the author has a window open at receipt, the worker closes it first,
  under the target's ordinary message, and the save attaches to the next window. This is for a save
  created *before* its writes, which should not sweep up the author's earlier, unrelated edits.

`Next` means "reaches the branch worker after receipt", not "was made after the request". A write
still held for its audit fact when the request arrives lands in the next window. The docs must say
so, and the sizing advice covers it: a save that needs a hard line should create its request only
after the previous save resolved.

### `emptyCommit: Skip` or `Create`

With `Create`, a save always produces one commit carrying its message:

| Situation | `Skip` (today) | `Create` |
|---|---|---|
| Writes arrived and changed files | commit | commit |
| Nothing arrived before `attachTimeout` | `NoWindowInGrace`, no commit | empty commit |
| Writes arrived but the files already matched | `AlreadyPresent`, no commit | empty commit |
| Only another author's window was open | `WindowMismatch`, no commit | empty commit |

The empty commit is not a window: principle 1 still holds, and the commit holds nothing but the
message. Its author is the request's submitter when admission captured one, otherwise the
committer, the same rule as any other commit. The reason stays specific (`CommittedEmpty`), so a
caller can still tell "saved your changes" from "recorded your message". Empty commits already
exist in this codebase: #383 reverts refused edits with one.

This is what makes a save a reliable primitive for an integrator: every successful save has a
commit hash to show. The cost is noise in history for saves that change nothing, which is why it is
opt-in.

## Naming, and why these words

### Precedents

Every system that batches has the same two timers, and a few also have a wait for the batch to
start:

| Tradition | Rolling timer (resets on activity) | Fixed cap | Waiting to start |
|---|---|---|---|
| Network proxies (Envoy, Gateway API, NGINX) | `idleTimeout` | `maxDuration`, `maxConnectionDuration` | `connectTimeout` |
| Debounce (lodash, RxJS) | `wait` | `maxWait` | n/a |
| Batch exporters (OpenTelemetry, Kafka) | n/a | `timeout`, `linger.ms` | n/a |
| Alert grouping (Alertmanager) | `group_interval` | n/a | `group_wait` |
| Kubernetes | n/a | `activeDeadlineSeconds` | n/a |
| Flux | n/a | `timeout` | n/a |

The proxy words are the ones operators already know, and "timeout" is Flux's word as well. "Idle"
means "resets on activity" to almost every reader. "Deadline" reads as a point in time, not a
duration. The debounce words collide with "wait" as a verb.

### Decisions

- **The block is `window`,** the word [`definitions.md`](../definitions.md) already defines. The
  timers are properties of the window, so they live in it (rule 1: one concept, one word).
- **Full field names:** `idleTimeout` and `maxDuration` rather than `idle` and `max`. Inside a
  `window` block nothing stutters, and a lone `max` is unusual in Kubernetes APIs.
- **`attach` for the act, on both the enum and its timer.** The code's "claim" goes, because
  "claimed" already means a rule's demand on the read side.
- **The same block shape on both kinds.** The save's block is a superset: `attach` and
  `attachTimeout` have no meaning on a target, which has nothing to wait for.

Rejected along the way: `windowTimeouts.{idle, max}`, which existed only to dodge the stored
string; flat `idleTimeout` on `commit`, which loses its subject; `batching`, which drops the defined
word; `closeDelay` kept as the cutoff, which describes an action rather than a duration.

## Status, code and metrics

- **Reasons.** `WaitingForCloseDelay` splits into `WaitingForWindow` (before the attach) and
  `CollectingWindow` (after it). `CommittedEmpty` joins the terminal reasons. All follow
  [`spec/status-conditions-guide.md`](../spec/status-conditions-guide.md).
- **Code.** "Grace" and "claim" leave the commit path. `finalizeAt` becomes an attach deadline and a
  close deadline.
- **Metrics.** The `timer` finalize reason splits into `idle-timeout` and `max-duration`, a save's
  cutoff reports `max-duration` instead of `finalize-signal`, and `attach: Next` closing a window
  gets its own reason. [`interpreting-metrics.md`](../interpreting-metrics.md) records the labels.
- **Definitions.** [`definitions.md`](../definitions.md) gains the rule that only writes open a
  window, and "attach".

## Out of scope

- **Inherited defaults** (a target-level default for its saves' timers). With "replace whole", a save
  never needs the target's values.
- **The named-write wait** (`waitFor` with a resource reference): phase 2 of the save-wait design.
  With this surface, `waitFor` names *which* write, and `window.attachTimeout` stays the one bound on
  waiting for it.

## Plan

1. **API.** `GitTarget.spec.commit.window` becomes the block; `CommitRequest.spec` gains `window`
   and `emptyCommit` and drops `closeDelay`.
2. **Worker.** Carry resolved timers on each open window and arm an idle and a max timer from them;
   on attach, swap in the save's. Stamp the attach deadline from `attachTimeout`. `Next` closes the
   author's open window at receipt. `Create` commits empty with the message on each no-commit
   outcome.
3. **Controller.** Split the progress reason, add `CommittedEmpty`, and size
   `commitRequestResolveTimeout` from the largest `attachTimeout` plus the largest `maxDuration`.
4. **Docs.** One paragraph per field in [`configuration.md`](../configuration.md), the definitions,
   the metric labels, and the migration note.
5. **Tests.** Attach expiry versus a write on the same wake; writes after the attach join one commit;
   a save's `idleTimeout` longer and shorter than the target's; a save with only `maxDuration` is not
   closed by the target's idle timer; continuous activity stops at `maxDuration` on both kinds; an
   author change still closes an attached window; `Next` commits the earlier window separately under
   its ordinary message; `Create` produces one empty commit for each no-commit situation above.

## Open questions

- **`Current` and `Next`, or other values?** `Current` hides "or the next one if none is open".
  `OpenOrNext` is exact but clumsy.
- **Should `emptyCommit: Create` apply to `WindowMismatch`?** It records a message while another
  author's work is still in flight. That is honest, since the commit carries none of their work, but
  it may confuse a reader of the history.
- **Does a target need `maxDuration` in the first PR?** It shares the timer code with the save, so
  it is nearly free, but nobody has asked for it.

## Migration (small)

There are no production users outside our control, so this is a note, not a strategy. Both removed
fields are **deleted outright**. A stored `GitTarget` whose `commit.window` is still a string cannot
be decoded after the CRD changes, so it is patched to the block before the upgrade (one `jq`
query in UPGRADING lists them). `closeDelay: X` becomes `window.maxDuration: X`, plus
`attachTimeout: X` for a save that relied on the wait.
