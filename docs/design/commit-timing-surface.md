# Commit timing: one vocabulary for the target and the save

> **design**: a proposal for the PR after #403, not built. Index: [`../INDEX.md`](../INDEX.md)
>
> The short version: three timers decide when a claimed window closes, and today two fields spread
> across two kinds carry them under two unrelated names. `CommitRequest.spec.closeDelay` does two
> jobs, waiting for a window and collecting after the claim, and it cannot keep a window open past
> the target's own silence timer. The proposal gives each job one field, reuses `window` for the one
> rolling timer wherever it appears, and keeps today's fixed cutoff as the default. It extends the
> timer model already decided in [`commitrequest-save-wait-options.md`](commitrequest-save-wait-options.md);
> it does not replace it.

## What decides when a window closes today

After #403, these are the timers and boundaries that end a commit window:

| What | Field | Starts from | Resets on a matching change? | Default |
|---|---|---|---|---|
| Target silence | `GitTarget.spec.commit.window` | each event | yes | `5s`, at most `24h` |
| Request wait | `CommitRequest.spec.closeDelay` | receipt by the branch worker | no | `2s`, at most `5m` |
| Request collection | the same `closeDelay` | the claim | no | the same value |

Other boundaries close a window regardless of any timer: a change of author or target, the branch
buffer's memory limit (`buffer-limit`), a resync or atomic apply that needs the window drained
first, `window: 0s`, and shutdown. The `windowFinalizeReason` values in
[`branch_worker.go`](../../internal/git/branch_worker.go) name them all.

## What does not line up

**One field, two jobs.** `closeDelay` bounds the wait for a window and, since #403, the collection
after the claim. Those two phases need different sizes. The wait covers the cluster's audit latency
plus however long the client takes to make its first write. The collection covers the span of the
save's own writes. One number is either too long for one of them or too short for the other.

**A request cannot hold a window open longer than the target allows.** The target's silence timer
still runs on a claimed window (`windowFinalizeReasonTimer` in the event loop). With the default
`window: 5s`, a `closeDelay: 30s` collects for at most 5 seconds of silence, not 30. The request
asked for a longer interval and the shorter one wins without saying so.

**Nothing caps a busy window except memory.** The target's `window` is purely rolling: continuous
activity keeps a window open until `buffer-limit` trips. There is no time bound. A request gets a
fixed cutoff from `closeDelay`; a target cannot express one.

**The names do not say what the fields do.** A [commit window](../definitions.md) is the interval a
branch worker batches writes over. On the target, `window` is that interval's silence timeout. On
the request, the comparable timer is called `closeDelay`. In the worker's code and comments it is
called a "grace" (`collect grace`, `grace elapsed`), which collides with the
[grace window](../definitions.md) the definitions page reserves for the attribution wait.

**Omission and `2s` look the same.** The CRD fills an omitted `closeDelay` with `"2s"` on write, so
the controller cannot tell a request that chose `2s` from one that left it out. Any inherited default,
for example one set on the target, needs that distinction.

## Constraints the next PR inherits

- **The decided timer model.** [`commitrequest-save-wait-options.md`](commitrequest-save-wait-options.md#the-timer-model)
  already settled "give each field one job": a `waitFor.timeout` anchored at receipt as the give-up
  bound, and `closeDelay` measured from satisfaction. #403 moved `closeDelay`'s collection anchor to
  the claim, which is the same idea for a request that names no write. This proposal must land
  inside that model.
- **The binding vocabulary.** [`definitions.md`](../definitions.md) owns field names. Flux is the
  precedent for durations (`timeout`, `interval`), and every duration is a Go duration string with a
  CEL bound ([API durations](../UPGRADING.md#every-duration-in-the-api-is-a-go-duration-string)).
- **An immutable spec.** A `CommitRequest`'s whole spec is immutable, and a request lives at most 48
  hours after it finishes. A field change reaches new requests only; there is no stored object to
  migrate beyond that window.
- **The attach invariant.** A request finalizes its own submitter's work and nobody else's. No
  timer change may widen what a request can claim.

## Proposal

Three concepts, one name each, and the same name wherever the concept appears:

| Concept | Field | Starts from | Resets? | Default |
|---|---|---|---|---|
| Wait timeout | `CommitRequest.spec.waitFor.timeout` | receipt | no | the value of `closeDelay`, which is today's behaviour |
| Rolling collection | `CommitRequest.spec.window` | the claim, then each matching event | yes | omitted: no rolling collection |
| Collection cutoff | `CommitRequest.spec.closeDelay` | the claim | no | `2s` |

**`waitFor.timeout`** is the field the save-wait design already names, shipped first without its
resource reference. A request with only a timeout waits for any matching window, as today. It takes
over the waiting job from `closeDelay`. While it is omitted it defaults to the request's
`closeDelay`, so a request written for 0.50 behaves the same.

**`window` on a request** has exactly the meaning `GitTarget.spec.commit.window` has: the silence
after which the window closes. For a claimed window it **replaces** the target's silence timer, so a
request can ask for a longer interval, or a shorter one, and get it. Author or target changes,
`buffer-limit`, drain-before-apply and shutdown remain boundaries; the request changes only the
timer.

**`closeDelay`** keeps its name and ends up meaning exactly what the name says: close this long
after the claim, whatever arrives. With `window` set it is the cap that keeps continuous activity
from holding the window open. Without `window` it is today's fixed cutoff.

**Keep the fixed cutoff the default.** Rolling collection groups a save whose writes trickle in, but
it has no predictable end. It should be something a caller asks for, bounded by `closeDelay`.
Making `closeDelay` itself reset on each change would lose the one bound a caller can rely on.

### What stays out of this PR

- **A time cap on the target's own window** (`GitTarget.spec.commit.maxWindow` or similar). The
  table shows the gap, but nobody has hit it; `buffer-limit` covers the memory risk. It can reuse the
  request's vocabulary later if it is needed.
- **Inherited defaults** (a target-level or cluster-level default for request timing). That needs
  omission to be distinguishable, which means moving the `2s` default out of the schema and into the
  controller. Do that in the PR that adds the inherited default, not before. Until then the schema
  default is the more readable contract: `kubectl get -o yaml` shows the value that applied.
- **The named-write wait** (`waitFor` with a resource reference). That is phase 2 of the save-wait
  design and depends on its phase 1.

### Status

The single progress reason `WaitingForCloseDelay` covers both phases today. Split it so a stuck
request says which phase it is in: `WaitingForWindow` before the claim, and `Collecting` after it.
Both are progress reasons under `Reconciling=True`, following
[`spec/status-conditions-guide.md`](../spec/status-conditions-guide.md). Rename "grace" to "wait" or
"collection" in the worker's code and comments in the same PR, so the attribution grace window is
the only grace left.

## Plan

1. **API.** Add `spec.window` and `spec.waitFor.timeout` to `CommitRequest`, both optional, both Go
   duration strings with the same pattern and CEL bounds as today (`window` at most `24h`, as on the
   target; `timeout` at most `5m`). Additive, so no new API version.
2. **Worker.** Carry the three values on `AttachCommitRequest`. Stamp the wait deadline from
   `waitFor.timeout`, falling back to `closeDelay`. On the claim, set the cutoff at claim +
   `closeDelay`, and when `window` is set, arm the claimed window's silence timer from it instead of
   the target's. The claim must still check expiry first; #403's boundary test covers that.
3. **Controller.** Resolve the fallback, split the progress reason, and size
   `commitRequestResolveTimeout` from the largest wait plus the largest cutoff.
4. **Docs.** Replace the sizing section in [`configuration.md`](../configuration.md#sizing-closedelay)
   with one paragraph per field, add the upgrade entry, and add a definition for the request's
   collection to [`definitions.md`](../definitions.md).
5. **Tests.** Wait expiry versus claim on the same wake; writes arriving after the claim join one
   commit; a request `window` longer than the target's keeps the window open; a request `window`
   shorter than the target's closes it sooner; continuous activity stops at `closeDelay`; a change of
   author still closes a claimed window.

## Open questions

- Should a request `window` also be allowed to shorten collection below the target's, or only lengthen
  it? Replacing both ways is simpler to explain; lengthening only is harder to misuse.
- Is `closeDelay` still the right name once it is only the cutoff, or should it become `maxWindow`,
  mirroring a future target field? Renaming costs one more migration for every integrator. Keeping it
  keeps a name that now means what it says.
- Does `waitFor.timeout` defaulting to `closeDelay` read as a surprise once the two are separate, or
  should it default to its own `2s`? The fallback keeps 0.50 requests identical; a fixed default is
  easier to document.
