# Commit timing: one vocabulary for the target and the save

> **design**: a proposal for the PR after #403, not built. Index: [`../INDEX.md`](../INDEX.md)
>
> The short version: three timers decide when a claimed window closes, and today two fields on two
> kinds carry them under unrelated names. `CommitRequest.spec.closeDelay` does two jobs, and it cannot
> keep a window open past the target's own silence timer. The proposal names every timing knob as a
> **timeout**, named for what runs out, and puts the two timers that close a commit window in one
> block with the same shape on both kinds:
>
> ```yaml
> # GitTarget                      # CommitRequest
> spec:                            spec:
>   commit:                          waitFor:
>     windowTimeouts:                  timeout: 2s     # give up if no window arrives
>       idle: 5s                     windowTimeouts:   # replaces the target's, for the window it claims
>       max: 10m                       idle: 1s
>                                      max: 30s
> ```
>
> Both `closeDelay` and the string `commit.window` go away, each kept for one release as a loud
> rejection. It extends the timer model already decided in
> [`commitrequest-save-wait-options.md`](commitrequest-save-wait-options.md), which it does not replace.

## What decides when a window closes today

After #403:

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
after the claim. The two phases need different sizes. The wait covers the cluster's audit latency
plus however long the client takes to make its first write. The collection covers the span of the
save's own writes.

**A request cannot hold a window open longer than the target allows.** The target's silence timer
still runs on a claimed window (`windowFinalizeReasonTimer` in the event loop). With the default
`window: 5s`, a `closeDelay: 30s` collects for at most 5 seconds of silence. The request asked for a
longer interval and the shorter one wins without saying so.

**Nothing caps a busy window except memory.** The target's `window` is purely rolling: continuous
activity keeps a window open until `buffer-limit` trips. A request gets a fixed cutoff from
`closeDelay`; a target cannot express one.

**The names do not say what the fields do.** A [commit window](../definitions.md) is the interval a
branch worker batches writes over, but `commit.window` holds only its silence timeout. The request's
comparable timer is called `closeDelay`. In the worker's code it is called a "grace" (`collect
grace`, `grace elapsed`), which collides with the [grace window](../definitions.md) the definitions
page reserves for the attribution wait.

**Omission and `2s` look the same.** The CRD fills an omitted `closeDelay` with `"2s"` on write, so
the controller cannot tell a request that chose `2s` from one that left it out.

## Constraints

- **The decided timer model.** [`commitrequest-save-wait-options.md`](commitrequest-save-wait-options.md#the-timer-model)
  settled "give each field one job": a `waitFor.timeout` anchored at receipt as the give-up bound,
  and a collection measured from satisfaction. For a request that names no write, satisfaction is
  the claim.
- **The binding vocabulary.** [`definitions.md`](../definitions.md): one concept keeps one word
  everywhere (rule 1), a rename removes the old spelling instead of aliasing it, and two or more
  facts about one subject may form a block (rule 2). `total` and other count words are taken
  (rule 3). Flux is the precedent for durations (`timeout`, `interval`), and every duration is a Go
  duration string with a CEL bound.
- **No type change under a stored name.** `commit.window` is stored as a string. Redefining that name
  as an object leaves stored GitTargets that no typed client can decode, which breaks GET and LIST
  for the whole kind. That is the failure the duration CEL bounds exist to prevent. The new shape
  needs a new name. [`crd-upgrade-strategies.md`](../facts/crd-upgrade-strategies.md) has the two
  honest options for the old one.
- **An immutable request spec.** A `CommitRequest`'s whole spec is immutable, and a request lives at
  most 48 hours after it finishes.
- **The attach invariant.** A request finalizes its own submitter's work and nobody else's. No timer
  change may widen what a request can claim.

## Naming the timers

### Where the words come from

Every system that batches has the same two timers, and most name them after one of three
traditions:

| Tradition | Rolling timer (resets on activity) | Fixed cap | Waiting for the batch to start |
|---|---|---|---|
| Debounce (lodash `debounce`, RxJS) | `wait` | `maxWait` | n/a |
| Network proxies (Envoy, NGINX, HTTP clients) | `idleTimeout` | `maxDuration`, `maxConnectionDuration` | `connectTimeout` |
| Batch exporters (OpenTelemetry, Kafka) | n/a | `timeout`, `linger.ms` | n/a |
| Alert grouping (Alertmanager) | `group_interval` | n/a | `group_wait` |
| Kubernetes | none rolling | `activeDeadlineSeconds`, `progressDeadlineSeconds` | n/a |
| CI triggers (Jenkins) | `quietPeriod` | n/a | n/a |
| Flux | none rolling | `timeout` | n/a |

The proxy tradition is the only one that covers both timers with words most operators already know,
and "timeout" is also Flux's word. "Idle" is universally understood as "resets on activity".
"Wait" is taken by `waitFor`, so the debounce words would collide. "Deadline" is Kubernetes' word
for a fixed cap, but it reads as an absolute point in time, not a duration.

### Candidates

| # | GitTarget | CommitRequest | For | Against |
|---|---|---|---|---|
| A | `commit.windowTimeouts.{idle, max}` | `windowTimeouts.{idle, max}` | keeps "window" (rule 1); every timing knob is a timeout; one shape on both kinds | `max` alone is terse; a plural block name is unusual |
| B | `commit.window.{idleTimeout, maxDuration}` | `window.{idleTimeout, maxDuration}` | the cleanest YAML; Envoy's exact words | a type change under a stored name: ruled out by the constraints |
| C | `commit.idleTimeout`, `commit.maxWindowDuration` (flat) | `idleTimeout`, `maxWindowDuration` | no block; rule 2 allows two flat fields | "idle" of what? The request's pair loses its subject, and the two kinds stop looking alike |
| D | `commit.batching.{idleTimeout, maxDuration}` | `batching.{…}` | reads naturally | drops "window", the defined word for the concept (rule 1) |
| E | `commit.windowClose.{afterIdle, afterOpen}` | `windowClose.{…}` | says what the timers do | `afterOpen` is wrong for the request, whose cap runs from the claim; verbs make poor field names |
| F | `commit.window.{idle, max}` in a new API version | same | B's shape | needs a conversion webhook for one field; [not a real option](../facts/crd-upgrade-strategies.md) |

**Recommendation: A.** It is the only candidate that keeps the defined word, avoids the stored-type
trap, and gives both kinds the same block. `windowTimeouts.max` reads fine in context: the
block name says it is a timeout, and `max` says which one. The alternatives for `max`: `total`
is a count word (rule 3), `hard` is jargon, and `cap` is less known. `idle` stays as it is; it is
the one word every reader already maps to "resets on activity".

With A, the request's `waitFor.timeout` completes a single family: every timing knob on both kinds
is a timeout, named for what runs out.

## Proposal

### The fields

| Field | Starts from | Resets? | Default | Bound |
|---|---|---|---|---|
| `GitTarget.spec.commit.windowTimeouts.idle` | each event | yes | `5s`; `0s` means a commit per event | `24h` |
| `GitTarget.spec.commit.windowTimeouts.max` | the window opening | no | omitted: no cap, as today | `24h` |
| `CommitRequest.spec.waitFor.timeout` | receipt by the branch worker | no | `2s` | `5m` |
| `CommitRequest.spec.windowTimeouts.idle` | the claim, then each matching event | yes | omitted: no idle close | `5m` |
| `CommitRequest.spec.windowTimeouts.max` | the claim | no | `2s` | `5m` |

### The rules

**The request's block replaces the target's, whole.** For the window a request claims, the target's
`idle` and `max` stop applying and the request's take over. There is no merging field by field. An
omitted request `idle` therefore means "no idle close": a request with only `max` is a pure fixed
cutoff, which is exactly today's `closeDelay`, without the target's silence timer cutting it short.
Author or target changes, `buffer-limit`, drain-before-apply and shutdown remain boundaries; a
request changes only the timers.

**A rolling request needs a cap.** CEL on the request: `max` is required when `idle` is set, and
`idle` must not exceed `max`. Rolling collection groups a save whose writes trickle in, but it has
no predictable end, so it is always bounded. Letting `max` reset on each change would remove the one
bound a caller can rely on.

**The fixed cutoff stays the default.** An omitted `windowTimeouts` on a request is `{max: 2s}`,
today's behaviour. Rolling collection is something a caller asks for.

**The claim checks expiry first.** A request whose wait has run out when it claims keeps its expired
deadline and is finalized in the same pass, as #403 does. The boundary test carries over.

**Waiting and collecting are separate numbers.** `waitFor.timeout` no longer borrows from the
collection value. A save that creates its request before its writes sets `waitFor.timeout` to the
gap before its first write, and `windowTimeouts` to the span of its writes. The worst case to the
finalize is the sum.

### Migration

Both old fields are retained for one release as loud rejections. Removing them outright is the
silent option: a re-applied GitTarget manifest or an integrator's save code would be accepted with
the value dropped, and the behaviour would change on every write.

- `CommitRequest.spec.closeDelay`: CEL `!has(self.closeDelay)`, message "renamed: use
  `windowTimeouts.max` for the cutoff and `waitFor.timeout` for the wait". A request is created
  fresh for every save, so the integrator's code fails on its first save after the upgrade, with a
  message that says what to change.
- `GitTarget.spec.commit.window`: the same CEL rule, pointing at `windowTimeouts.idle`. A stored
  target keeps its value under validation ratcheting until someone edits it; the controller
  **honours** a stored `window` as `windowTimeouts.idle` for that release and reports the rename
  in a condition message, so an unedited target changes nothing on upgrade.
- UPGRADING gets one `jq` query per field listing the objects still carrying it, which the 0.50
  field report singled out as the part of the guide that worked.

### Status, code and metrics

- **Reasons.** `WaitingForCloseDelay` splits into `WaitingForWindow` (before the claim) and
  `CollectingWindow` (after it), both progress reasons under `Reconciling=True`, following
  [`spec/status-conditions-guide.md`](../spec/status-conditions-guide.md).
- **Code.** "Grace" leaves the commit path: `finalizeAt` becomes a wait deadline and a close
  deadline, and comments say wait or collection. The attribution grace window is then the only grace.
- **Metrics.** The `timer` finalize reason splits into `idle-timeout` and `max-timeout`, and a
  request's cutoff reports `max-timeout` instead of `finalize-signal`. That changes label values, so
  [`interpreting-metrics.md`](../interpreting-metrics.md) and UPGRADING both record it.

### What stays out

- **Inherited defaults** (a target-level default for its requests' timeouts). With "replace whole"
  semantics a request never needs to know the target's values. If a default is wanted later, it
  needs omission to be distinguishable, which means moving the `2s` out of the schema. Until then the
  schema default is the more readable contract: `kubectl get -o yaml` shows the value that applied.
- **The named-write wait** (`waitFor` with a resource reference). Phase 2 of the save-wait design;
  `waitFor.timeout` is shaped so it can arrive later without a rename.

## Plan

1. **API.** Add `GitTarget.spec.commit.windowTimeouts` and `CommitRequest.spec.{waitFor, windowTimeouts}`
   as one shared Go type for the block. Retain `commit.window` and `closeDelay` as refusals.
2. **Worker.** Carry a resolved `windowTimeouts` on each open window. Arm the idle timer and the max
   timer from it; on a claim, swap in the request's. Stamp the wait deadline from `waitFor.timeout`.
3. **Controller.** Honour a stored `commit.window` for one release, split the progress reason, and
   size `commitRequestResolveTimeout` from the largest wait plus the largest cutoff.
4. **Docs.** One paragraph per field in [`configuration.md`](../configuration.md), a definition for
   "window timeouts" in [`definitions.md`](../definitions.md), UPGRADING with the queries, and the
   metrics label change.
5. **Tests.** Wait expiry versus claim on the same wake; writes after the claim join one commit; a
   request `idle` longer than the target's keeps the window open; a shorter one closes it sooner; a
   request with only `max` is not closed by the target's idle timer; continuous activity stops at
   `max` on both kinds; a change of author still closes a claimed window; the retained fields are
   refused with their message; a stored `commit.window` keeps working.

## Open questions

- **`max` or something longer?** `windowTimeouts.max` is short and reads well in YAML, but a lone
  `max` is unusual in Kubernetes APIs. `maxDuration` is Envoy's word and removes the ambiguity, at
  the cost of `windowTimeouts.maxDuration` reading twice as long as it needs to.
- **May a request shorten as well as lengthen?** "Replace whole" lets a request close a window
  sooner than the target would. That is the simpler rule to explain; the alternative, lengthen only,
  is harder to misuse but needs a merge rule.
- **Should a target get `max` in the same PR?** It shares the type and the timer code with the
  request, so it is nearly free, but nobody has hit the gap yet. Shipping it together keeps the two
  kinds' blocks identical from day one.
