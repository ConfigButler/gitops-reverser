# CommitRequest: design around two use cases

> Status: living design note, 2026-06-12.
> Supersedes the speculative redesign in
> [github-e2e-per-type-tail-failure-investigation.md](./github-e2e-per-type-tail-failure-investigation.md) §11–§15.
> Related: [commitrequest-barrier-timeout-decision.md](../../finished/commitrequest-barrier-timeout-decision.md)
> (Option A), [commitrequest-multi-finalize-design.md](../../finished/commitrequest-multi-finalize-design.md)
> (single-worker serialization), [canonical-stream-retirement.md](../../finished/canonical-stream-retirement.md)
> (the per-type-stream refactor that motivates this note).

## 1. What a CommitRequest is for

A CommitRequest is a one-shot **"save my changes now"** signal. Instead of waiting
for a GitTarget's silence timer (the commit window) to expire, a user creates a
CommitRequest naming the GitTarget; the operator finalizes that user's open commit
window into a real Git commit and reports the branch and SHA back in status.

The promise we want to keep is small and precise:

> The edits **this author** made **before** asking to save are finalized into a
> commit **attributed to that author**, carrying **the author's message**, and the
> commit SHA is reported back.

This note designs the feature around the **two use cases that must work really
well**, states the simplifying assumptions that make them tractable, and is
explicit about what we cannot promise.

## 2. The two use cases

### UC1 — "Save" button

A user makes a handful of changes, then enters a commit message and presses
**Save**. The UI emits a CommitRequest with `spec.message` set. There is human time
(seconds) between the edits and the save.

> **Expected:** the edits made before Save are committed as one commit with the
> typed message; the SHA comes back. Until Save, nothing is pushed (the commit
> window is long).

### UC2 — `kubectl apply` of a bundle that includes a CommitRequest

A user runs `kubectl apply -f bundle.yaml`, where the bundle contains several
resources **and** a CommitRequest carrying the intended message. The API server
processes the documents close together but in some order, and they fan out across
**different per-type audit streams**.

> **Expected:** all the bundle's resources and the CommitRequest land in **one
> commit** with the CommitRequest's message.

UC2 is the hard one, for one reason: **the CommitRequest may be processed first, in
the middle, or last.** If it is processed first, its "save" intent arrives before
the resources it is meant to save even exist in the pipeline. So the CommitRequest
must not shut the window the instant it is attributed — it needs a **grace period**
during which the rest of the same-author bundle can arrive and join the window.

## 3. Simplifying assumptions

This design is written for the two use cases above under two explicit assumptions.
Both are realistic for the use cases and keep the problem tractable.

- **A1 — The commit window is large.** The GitProvider's silence timer is long
  (minutes), so it never rolls the window out from under us during a save or an
  apply. Once the author's window opens, it stays open until *we* finalize it.
- **A2 — No concurrent authors.** Only one author is writing to this GitTarget
  during the action. We already decided we cannot merge independent authors into
  one commit (§4), so we assume that situation away here rather than pretend to
  handle it.

Relaxing either assumption lands you in the "cannot promise" territory of §4 — that
is exactly where it belongs.

## 4. What we honestly cannot promise

Stating the limits is half the design. These are consequences of having **one
commit to make** and of giving up the single total order in the per-type refactor.
The design's job is to **bound and report** them, not to pretend they don't exist.

- **One window, one commit — authors cannot be merged.** A branch worker holds at
  most one open commit window. If ten different authors each make a change in one
  second, we audit each one and produce **ten commits**, one per author, in arrival
  order. We will not, and should not, fold two authors' edits into a single commit:
  that would require conflict resolution between independent writers, which is the
  complexity a single-window model exists to avoid. A CommitRequest from author X
  only ever finalizes author X's window; another author's window is left untouched
  (status `Rejected`/`WindowMismatch`, §6.7). **That strictness is a feature.**
- **Cross-type order is already approximate.** Because each type has its own audit
  stream and the streams drain independently, even those ten one-event-each commits
  may not be in the exact order the API server saw them. For a
  **single author writing one bundle (UC2) this does not matter** — all the edits
  land in the *same* commit regardless of intra-bundle order. It only surfaces
  across commit boundaries, which we accept.
- **An internal writer can still steal the window.** Even with A1+A2, a per-type
  **resync** finalizes the open live window before it applies (to preserve arrival
  order). If one fires between an edit and the CommitRequest's finalize, the
  author's window is gone. We cannot stop this — but §6.4 makes it *graceful*
  instead of lossy: once the message is attached to the window, the resync's commit
  carries the user's message and the CommitRequest reports `Committed`. The only
  irreducible loss is an interrupt that lands *before* attribution, when there is no
  message to attach yet. The worker must also ensure the stolen window's commit is
  still **pushed** so the data is never lost. (This is the failure analyzed in the
  investigation note; the push fix is worker-side, and §6.4 is the semantics that
  preserve intent.)
- **"Before the CommitRequest" is bounded by grace, not guaranteed.** An edit made
  moments before the save, on a type whose stream lags, may arrive after the grace
  window and land in the **next** commit. We make the grace configurable and say so
  in status; we do not wait unboundedly.

## 5. How it works today

### 5.1 The CRD

`CommitRequestSpec` ([api/v1alpha1/commitrequest_types.go](../../../api/v1alpha1/commitrequest_types.go)):

- `gitTargetRef.name` — the GitTarget whose window to finalize (same namespace).
- `message` — optional verbatim commit message (1–1024 chars, no control chars).
- `delaySeconds` — the collect-grace window, `0`–`300`, default `0`.
- The spec is **immutable** after creation (CEL `self == oldSelf`).

`CommitRequestStatus` reports `phase` (`WaitingForAuditEvent`, `Committed`,
`NoOpenWindow`, `Failed`), an optional `message`, and `branch` / `sha` on success.
*(The design renames the `NoOpenWindow` phase to `Rejected` and adds a structured
`reason` — see §6.7.)*

### 5.2 The controller state machine

[`CommitRequestReconciler.Reconcile`](../../../internal/controller/commitrequest_controller.go)
runs with `MaxConcurrentReconciles=1` — the single worker *is* the
multi-CommitRequest ordering design, so no two finalizes interleave. One object is
advanced through:

1. **Stamp** `WaitingForAuditEvent` (uncached re-reads guard against a stale cache
   echo re-running a finalize that already terminated).
2. **Attribute** — `LookupCommitRequestAuthor` polls the `commitrequests` per-type
   audit stream for *this object's own create event* (matched by namespace, name,
   UID) and takes the author from it. Not seen yet → requeue every 2s. Past the 60s
   attribution timeout → **fail closed** (`Failed`). The author is never guessed.
3. **Delay** — if `delaySeconds > 0`, requeue until `creationTimestamp +
   delaySeconds`. Anchored at the object's creation timestamp. *(The design re-anchors
   this at the attribution moment — see §6.4.4 — because creation-anchoring is fragile
   under a delayed ingestion pipeline.)*
4. **Finalize** — enqueue the author-bound finalize signal and write terminal
   status. (Today this step is preceded by the watermark barrier of §5.4, which
   §6 argues to remove.)

### 5.3 Attribution is the keystone

Step 2 does two jobs at once, and this is the most important property of the design:

- **Identity** — the author comes from a real audit event, never a guess.
- **Ordering anchor** — every mutation, including this CommitRequest, enters through
  the same audit path. The author's earlier edits entered it *before* their
  CommitRequest, so by the time we observe the CommitRequest's own create event,
  the author's earlier work is — under comparable per-type ingestion delay — already
  ingested and enqueued. Attribution buys most of the ordering we want, for free.
  This is the basis for both use cases working with very little extra machinery.

### 5.4 The finalize signal and the open window

`FinalizeGitTargetWindow` enqueues a `FinalizeSignal`
([finalize_signal.go](../../../internal/git/finalize_signal.go)) onto the branch
worker's **FIFO event queue** — the same queue resource events ride — so by arrival
order it is processed after every earlier write for that worker.
`handleFinalizeSignal`:

- no open window → `NoOpenWindow`;
- an open window whose **author + GitTarget + namespace** do not match → leave it
  open, report `WindowMismatch` (cannot happen under A2 for the author's own work);
- a matching window → finalize into a commit; record branch + SHA.

### 5.5 The watermark barrier (the part we are removing)

Today, before finalizing, the controller takes a per-type watermark snapshot and
drains the tails to it ([commit_barrier.go](../../../internal/watch/commit_barrier.go)),
bounded by `FinalizeBarrierTimeout = 15s`, degrading visibly on timeout (Option A).
Three facts decide its fate:

- It is **already best-effort** — the 15s timeout means it never was an invariant.
- It is taken **audit-anchored**, in step 4 *after* attribution and delay
  ([commitrequest_controller.go:175](../../../internal/controller/commitrequest_controller.go#L175)),
  despite the stale "creation time" comment in the code.
- It has a **target-local gap**: the tail advances one cursor *per type* even when a
  particular GitTarget was skipped
  ([audit_tail.go:177](../../../internal/watch/audit_tail.go#L177) vs the skip at
  [audit_tail.go:200](../../../internal/watch/audit_tail.go#L200)), so it can report
  `barrierReached=true` for a target that did not actually receive every protected
  entry. A best-effort mechanism that can return a **false positive** launders "we
  don't know" into a green status.

## 6. The design that makes UC1 and UC2 work

The core is small and, except for removing the barrier, already built:

1. **Attribute** from the CommitRequest's own create audit event, or fail closed.
2. **Wait** `delaySeconds` from creation — the collect-grace window.
3. **Finalize** the open window bound to the attributed author + GitTarget.

No snapshot, no drain loop. Here is why that is enough for each use case.

### 6.1 UC1 walks straight through (delaySeconds = 0)

The user edits, then — seconds later — presses Save. By the time the CommitRequest's
audit event is observed, the edits (made earlier, on the human timescale) are long
since mirrored into the open window (A1 keeps it open; A2 means it is the author's).
Attribution completes, `delaySeconds: 0` means finalize immediately, the window
matches → **one commit, the typed message, the SHA returned.** The barrier adds
nothing here: the human gap already guarantees the edits are present.

### 6.2 UC2 needs the collect-grace, and the collect-grace is enough (delaySeconds > 0)

Lay out the three orderings, all under A1+A2:

- **CommitRequest last.** Resources ingest first and open/extend the window; the
  CommitRequest's event arrives last; finalize after the grace → all in one commit.
- **CommitRequest in the middle.** Some resources are already in the window at
  attribution; the rest arrive during the grace and join the same (still-open)
  window; finalize at the end of the grace → all in one commit.
- **CommitRequest first.** This is the case that breaks a naive "finalize on
  attribution." At attribution there may be **no window yet**. But the finalize is
  deferred by the grace (`attribution + delaySeconds`, §6.4.4); during that grace the
  bundle's resources arrive, open a window, and fill it; the deferred finalize then
  finds the now-open window → all in one commit.

So the **single mechanism — defer the finalize by `delaySeconds` and then finalize
whatever same-author window is open — covers all three orderings.** "A little bit of
grace before the CommitRequest is really shut down" *is* `delaySeconds`. UC2's
contract is therefore: set `delaySeconds` to comfortably exceed the bundle's
ingestion spread (a few seconds), and all of it lands in one commit.

The honest boundary (§4): if a resource's stream lags past the grace, that resource
opens a *fresh* window after the finalize and waits for the next close. We do not
wait unboundedly, and we say so.

### 6.3 Why the barrier goes

The barrier helped neither use case: UC1 is covered by the human gap, UC2 by the
grace. It defended only a narrow, low-harm, already-degradable case (a same-author
edit on a lagging stream), while doing nothing for the cases that actually bite
(§4), and it could report false-positive success. **Remove it** — and retire the
Option-A timeout wording with it. We do not replace it with a blanket caveat on every
commit: §6.5 makes `Committed` mean "pushed to the remote", which is the honest claim
that matters. The one residual — an edit on a lagging type stream may land in the next
commit — is a documented property of the model (§4), not a per-request status note,
because once the barrier is gone there is nothing left that detects it.

Keep the *idea* of an audit-anchored picture documented as the first thing to reach
for **if** real workloads ever prove harmful per-type skew — but reintroduce it then
as a **target-local applied watermark**, never the global cursor.

### 6.4 Window-attach with eager message binding (the recommended target)

§6.2 makes the **happy paths** of UC1 and UC2 work, but it has one weakness on the
**unhappy path**: between attribution and the finalize deadline, the CommitRequest's
message lives only on the controller side. If anything finalizes the window during
that interval — a resync (§4), a buffer-limit flush, anything — the resulting commit
carries the *generated* message and the CommitRequest later finds no window and
is `Rejected` (`NoWindowInGrace`, §6.7). The work is committed; the user's **intent**
(their message, and a `Committed` result) is lost.

Window-attach fixes exactly that, and it is the model this feature should grow into:

> **Bind the CommitRequest's message to the author's open window as early as
> possible. Once bound, whichever path finalizes the window first uses that
> message and resolves the CommitRequest as `Committed`.**

The grace period stops being a "hold before we act" and becomes a "hold during which
the intent is already safe." That is the durability the feature is really about.

#### 6.4.1 Worker model: the window carries the message

There are **two state machines**, at two layers, and they hand off at attribution:

- **Controller-side lifecycle** (the CRD phase): `Created → WaitingForAuditEvent
  (attribution) → [attributed] → attach → poll → terminal`. The attribution wait is a
  first-class state — it is where a **delayed ingestion pipeline is absorbed**, and it
  is the point the grace is anchored *after* (§6.4.4). Nothing below happens until the
  controller has observed the CommitRequest's own create audit event and bound the
  author. (So this section's states are *not* missing the "waiting for the audit
  event" moment — that moment lives one layer up, in §5.2 step 2 and the §7 table.)
- **Worker-side states** (below), which begin only *after* attribution, when the
  controller sends the attach.

Today the message travels on the one-shot `FinalizeSignal` and is applied only at the
finalize call. Window-attach moves it onto the window itself:

```
openWindow:
    ... existing fields (author, target, events) ...
    pendingMessage  string                 // the CommitRequest's message, once attached
    pendingCR       *commitRequestRef      // namespace/name/uid + result bookkeeping
```

A small per-worker table tracks CommitRequests not yet attached and the outcomes of
those already resolved:

```
pendingByAuthorTarget   map[authorTargetKey][]commitRequestRef   // waiting for a window
commitRequestOutcomes   map[commitRequestID]Outcome              // resolved: phase, sha, branch
```

A pending CommitRequest moves through four worker-local states:

- **WaitingForWindow** — no matching (author + GitTarget + namespace) window is open
  yet; the request is parked, with a finalize deadline.
- **Attached** — a matching window is open and now carries this request's
  `pendingMessage`/`pendingCR`; a finalize is armed for the deadline.
- **AwaitingPush** — the window was finalized by *some* path; the `pendingCR` moved
  onto the resulting `PendingWrite`, which is retained until its push lands (§6.5).
- **Resolved** — recorded in `commitRequestOutcomes`: `Committed` with the **pushed**
  SHA once the PendingWrite reaches the remote, or `Rejected` with a reason — the
  deadline passed with no window, the window was finalized but produced no diff, or it
  belonged to someone else (§6.7).

Transitions, all on the single worker loop goroutine (so no locking beyond the table):

- attach request dequeued, matching window open and unclaimed → **Attached**
  (set `pendingMessage`, arm finalize timer at the deadline);
- attach request dequeued, no matching window → **WaitingForWindow** (park, arm a
  deadline timer);
- a same-author window opens while WaitingForWindow → **Attached**;
- finalize timer fires while Attached → finalize the window with the attached
  message → **AwaitingPush**;
- **any other finalize path** runs on a window that is Attached (resync-before-apply,
  buffer-limit, author switch, silence timer, shutdown) → it uses the attached
  message → **AwaitingPush**;
- the `PendingWrite` carrying the `pendingCR` pushes successfully → **Resolved**
  (`Committed` + pushed SHA, §6.5);
- a finalize produces no diff (the change is already present) → **Resolved**
  (`Rejected`/`AlreadyPresent`, §6.7) — it does *not* enter AwaitingPush;
- deadline passes while still WaitingForWindow → **Resolved** (`Rejected`/`NoWindowInGrace`).

#### 6.4.2 One chokepoint makes every cut-off honor the message

All finalize paths already funnel through a single function
([`finalizeOpenWindowWithMessage`](../../../internal/git/branch_worker.go)). That is
the leverage point. Change it to:

1. use the explicit override message if the caller passed one, **else the window's
   `pendingMessage`,** else the generated grouped message; and
2. if the window carries a `pendingCR`, **carry it onto the resulting `PendingWrite`**
   alongside the message. Resolution does not happen here — it happens when that
   `PendingWrite` pushes (§6.5).

Because resync-before-apply, the commit-window timer, the buffer-limit flush, the
author-switch close, and shutdown **all** call this one function, every one of them
automatically carries the attached message *and* the `pendingCR` onto the
`PendingWrite` — with no per-path code. This is what makes "if something cuts it off,
the message is still there" true by construction rather than by enumerating closers.

The companion worker fix from the investigation note still applies and is now a
hard prerequisite: a finalize that closes a window into a pending write **must
schedule its push** (`maybeSchedulePush`), so an attached-then-cut commit actually
reaches the remote. Attach makes the *result* correct; the push fix makes the *data*
land.

#### 6.4.3 Controller ↔ worker protocol (attach, then poll)

The synchronous request/reply of today does not fit, because the finalize may now
happen seconds later or be triggered by an unrelated path. Make it asynchronous, in
the same poll-via-requeue shape the attribution step already uses:

1. **Controller**, the instant it attributes, sends one **AttachCommitRequest**`{crID,
   author, target, message, delaySeconds}` onto the worker FIFO — no controller-side
   delay. The worker stamps `finalizeAt = receipt + delaySeconds` on first registration
   (§6.4.4). Re-sends across requeues are idempotent: the worker keys pending requests
   by `crID` and keeps the first deadline.
2. **Worker** attaches (or parks) per §6.4.1; on the push that carries the
   request's `PendingWrite`, it records the outcome (§6.5).
3. **Controller** requeues and polls `LookupCommitRequestOutcome(crID)`:
   - not resolved and within a safety bound (covers the push cooldown + retries) →
     requeue, surfacing any push-retry error in `status.message`;
   - resolved → write the terminal status from the recorded outcome;
   - no worker exists for the target → `Rejected` (`NoWindowInGrace`), as today.

Polling (not a held result channel) keeps the controller non-blocking, survives a
reconcile running on a different goroutine, and is restart-tolerant. The worker GCs
`commitRequestOutcomes` entries by age. The controller's existing terminal-status
guards (uncached re-read, UID check) still make the status write at-most-once.

#### 6.4.4 Timing: anchor the grace at attribution, not at object creation

`delaySeconds` is a **collect window**, so it must be measured from the moment the
save is *observed*, not from when the object was created. Anchor it at attribution:

> `finalizeAt = (attribution observed) + delaySeconds`, with `delaySeconds` bounded to
> ≤ 300s.

This is a deliberate change from today, which anchors at the object's
`creationTimestamp` (§5.2). Object-creation anchoring is fragile under a delayed
ingestion pipeline: if ingestion takes longer than `delaySeconds`, then
`creation + delaySeconds` is already in the past by the time we attribute — the grace
has been entirely eaten by ingestion latency, the window may still be empty, and we
collect nothing (worst case `Rejected`/`NoWindowInGrace`). Creation-anchoring forces
`delaySeconds` to cover the *absolute* pipeline latency; attribution-anchoring lets it
cover only the inter-stream *spread*, which is all UC2 actually needs. Under a slow
pipeline it degrades gracefully — wait for ingestion, *then* collect — instead of
failing to collect.

Concretely, no new status field is needed: the controller sends the attach the
instant it attributes, and the **worker** stamps `finalizeAt = receipt + delaySeconds`
when it first registers the request (receipt ≈ the attribution moment). Idempotent
re-sends keep the first deadline.

- **UC1, `delaySeconds: 0`** — window already open at attribution; attach and
  finalize immediately → `Committed`, the typed message.
- **UC2, `delaySeconds: N`** — attach the moment the first same-author edit opens the
  window (before *or* after attribution, depending on bundle ordering); keep
  collecting same-author edits; finalize at `finalizeAt`.

An optional idle variant — finalize a few seconds after the *last* same-author edit,
capped at `finalizeAt` — would batch a burst more tightly; it adds a knob and the risk
of a chatty author deferring the commit, so treat it as a later tweak, not the
baseline.

#### 6.4.5 What this buys, and the one residual

Eager attach shrinks the window of vulnerability from the whole grace period down to
**just the attribution latency**:

- Before attribution, there is no message to attach; if a resync cuts the window in
  that gap, the commit uses the generated message and the CommitRequest finds no
  window → `Rejected` (`NoWindowInGrace`). This is irreducible — we cannot attach an
  intent we have not yet attributed.
- After attribution, the message is on the window. Any cut-off — resync or
  otherwise — produces a commit **with the user's message**, and the CommitRequest
  resolves as `Committed` once that commit is pushed (§6.5). The §4 "internal writer
  steals the window" case stops being a lost-intent failure and becomes a graceful,
  slightly-early commit.

So we still cannot promise *one* commit in the face of a mid-flight resync (late
same-author edits that arrive after the cut open a fresh window and land later), but
we can promise the thing the user cares about most: **the message and the
work-so-far are committed under the author's name, and the request succeeds.** That
is the honest, defensible contract.

#### 6.4.6 Edge cases

- **Two CommitRequests for the same author/target.** A window carries at most one
  `pendingCR`. A second request arriving while the window is Attached is parked
  WaitingForWindow and binds to the *next* window (the edits after the first
  finalize). Each CommitRequest still maps to exactly one commit with its own message.
- **No window ever opens.** Deadline passes WaitingForWindow → `Rejected`
  (`NoWindowInGrace`) — pressing save with nothing pending, or a bundle with no
  watched resources.
- **Restart.** Explicitly out of scope; see §6.6.

## 6.5 Resolving the request: on push, from the `PendingWrite`

The `PendingWrite` is already the system's nicest construct here: it carries the
commit message verbatim ([commit_executor.go:73](../../../internal/git/commit_executor.go#L73)),
it is retained across the cooldown wait, and it survives the push-conflict
rebase-replay ([branch_worker.go:1091](../../../internal/git/branch_worker.go#L1091))
because the replay re-commits straight from it. So make it carry the CommitRequest's
result handle too, and resolve the request **when, and only when, its `PendingWrite`
is pushed**:

```
PendingWrite:
    ... existing fields (events, message, signer, target) ...
    CommitRequest  *commitRequestRef   // who to resolve once this write is pushed
    CommitSHA      plumbing.Hash        // the hash of the commit this write created
```

- `executePendingWrite` already calls `worktree.Commit`, which returns the hash;
  thread it back onto `CommitSHA`. On a rebase-replay the write is re-executed, so
  `CommitSHA` is naturally **refreshed to the post-rebase hash** — no stale SHA.
- On a successful push ([`pushPending`](../../../internal/git/branch_worker.go#L910)),
  walk the pushed writes and, for each that carries a `CommitRequest`, record
  `commitRequestOutcomes[cr] = {Committed, CommitSHA, branch}`.

This is cheap — a couple of fields and a loop on the success path — and it finishes
the contract:

- **`Committed` becomes honest.** The request is not resolved at local-commit time;
  it is resolved when the commit is genuinely on the remote. A "save" therefore
  confirms the push, which is exactly what a save should mean. The cost is latency
  bounded by the push cooldown plus retries — seconds, and the right kind of wait.
- **The SHA is the real one.** It is read from the pushed write's own commit
  (per-write, not branch HEAD, since a batched push may stack a later commit on top),
  *after* any rebase-replay. The SHA-churn problem from the push-honesty discussion
  disappears.
- **Push failure is honest too.** A failed push retains the `PendingWrite`
  ([branch_worker.go:916-922](../../../internal/git/branch_worker.go#L916-L922)) and
  does not advance `lastPushAt`, so the request simply stays non-terminal while the
  worker retries, with the push error surfaced in `status.message`. It flips to
  `Committed` when a retry lands. It genuinely is not saved until then, and the status
  says so — no new phase required (the CRD already documents `WaitingForAuditEvent` as
  "the finalize it gates has not completed").

## 6.6 Restart and replay: explicitly out of scope

We do **not** engineer durable recovery of an in-flight CommitRequest across an
operator restart. The message itself is never lost — it lives in `spec.message`, a
durable Kubernetes object — and on restart the controller re-reconciles any
non-terminal request, re-attributes from the durable audit event, and re-attaches.
That heals the common cases for free.

The one case we knowingly accept is: a request whose commit was already **pushed**
but whose terminal status was not yet written when the operator restarted. The
in-memory `commitRequestOutcomes` entry is gone, the re-driven attach finds the work
already mirrored (so the re-finalize is a no-op), and the request resolves
`Rejected` (`AlreadyPresent`, §6.7) even though its commit — with its message — is on
the remote. The data and the message are safe; only the request's own status is
slightly off — and `AlreadyPresent` is at least an honest hint that the change is
already there. We judge this rare and low-harm and choose **not** to add a durable
record (e.g. a commit trailer) to
close it. If that ever changes, the durable system of record would be the commit
itself, stamped with the request identity; until then, restart recovery is best-effort.

## 6.7 When nothing is committed: the `Rejected` outcome

Not every CommitRequest produces a commit, and "no commit" is not one thing. Today
these collapse onto a single `NoOpenWindow` phase (with a mismatch *message* for the
foreign case). Rename that terminal phase to **`Rejected`** — one honest "the request
was handled correctly but produced no commit" state — and distinguish the reasons with
a structured `status.reason` plus a human `status.message`:

| `reason` | When | Note |
|---|---|---|
| `NoWindowInGrace` | The deadline passed with no matching same-author window. | Benign: nothing was pending to save. |
| `WindowMismatch` | A window was open but belonged to a different author/GitTarget. | Benign and strict (§4): the foreign window is left untouched. |
| `AlreadyPresent` | A matching window was finalized, but its events produced **no diff** — the change already matches the remote, so the commit was dropped (loop prevention). | The theoretical edge this section exists for. |

`Rejected` is deliberately **not** `Failed`. In all three cases the system behaved
correctly; there was simply no commit to make. `Failed` stays reserved for genuine
faults — attribution failed, the finalize/commit errored, or a push that can never
land — so a dashboard can treat `Failed` as "look at this" and `Rejected` as
informational. (At the worker level the existing `FinalizeNoOpenWindow` /
`FinalizeWindowMismatch` outcomes, plus a new no-change signal, simply roll up to
`Rejected` + the matching reason.)

Concretely, the CRD gains a typed, validated `reason` enum and the field on status:

```go
// CommitRequestRejectReason explains a Rejected CommitRequest: the request was
// handled correctly but produced no commit. It is set only when phase is Rejected.
// +kubebuilder:validation:Enum=NoWindowInGrace;WindowMismatch;AlreadyPresent
type CommitRequestRejectReason string

const (
    // RejectNoWindowInGrace: the grace period elapsed with no matching same-author
    // window — nothing was pending to save.
    RejectNoWindowInGrace CommitRequestRejectReason = "NoWindowInGrace"
    // RejectWindowMismatch: an open window existed but belonged to a different author
    // or GitTarget, so it was deliberately left untouched.
    RejectWindowMismatch CommitRequestRejectReason = "WindowMismatch"
    // RejectAlreadyPresent: a matching window was finalized but produced no diff — the
    // change already matches the remote, so the commit was dropped (loop prevention).
    RejectAlreadyPresent CommitRequestRejectReason = "AlreadyPresent"
)

// in CommitRequestStatus:
//   // Reason explains a Rejected phase. Empty for non-Rejected phases.
//   // +optional
//   Reason CommitRequestRejectReason `json:"reason,omitempty"`
```

The human `message` still carries the prose; `reason` is the stable, machine-readable
discriminator that status consumers and tests assert on.

### `AlreadyPresent` must not hang in `AwaitingPush`

This is the case that interacts with §6.5, and it is why it needs explicit handling. A
finalized window normally creates a commit whose `PendingWrite` resolves the request
on push. But [`executePendingWrite`](../../../internal/git/commit_executor.go#L132-L138)
returns `(0, nil)` when the events produce no change — **no commit, nothing to push.**
A request waiting for a push that never comes would sit in `AwaitingPush` until the
controller's safety bound and then mis-resolve.

So the no-op is detected at finalize time and resolved immediately:

- when a `PendingWrite` carrying a `pendingCR` executes and reports no change
  (`anyChanges == false`), the worker records `commitRequestOutcomes[cr] = {Rejected,
  AlreadyPresent}` right there — it never enters `AwaitingPush`;
- only a `PendingWrite` that actually created a commit (a real `CommitSHA`) takes the
  `AwaitingPush` → `Committed`-on-push path of §6.5.

In the §6.4.1 state machine this is one extra terminal edge off the finalize step:
*Attached → finalize produces no diff → Resolved(`Rejected`/`AlreadyPresent`).*

### Worth a unit test

Cheap, deterministic, and it guards a path that "never happens": finalize a window
whose event re-asserts state already present in the worktree (so
`applyPendingWriteEvents` returns `false`), then assert the carried request resolves to
`Rejected` with `reason == AlreadyPresent` — **and** that it resolves promptly rather
than blocking on a push. Assert on the structured `reason`, never on message text.

## 7. State model

The CRD keeps a small phase set; the richer "moments" are internal.

| Internal moment | Meaning | User-visible phase |
|---|---|---|
| Created, waiting for audit event | Its own create audit event has not reached the `commitrequests` stream yet — this is where a delayed ingestion pipeline is absorbed. | `WaitingForAuditEvent` |
| Attributed, in grace | Author known; in the `delaySeconds` collect window anchored at attribution (§6.4.4). | `WaitingForAuditEvent` |
| Window matched, collecting | A matching same-author window exists and stays collectable until grace ends. | `WaitingForAuditEvent` |
| Finalized, awaiting push | The window was committed locally and carries the message; its `PendingWrite` is retained until it pushes (push retries surface in `status.message`). | `WaitingForAuditEvent` |
| Pushed | The carrying commit reached the remote; branch + pushed SHA recorded (§6.5). | `Committed` |
| No commit produced | No window in grace, a foreign window, or the change already matched the remote — distinguished by `reason` (§6.7). | `Rejected` (+ `reason`) |
| Attribution failed | The own audit event never arrived within 60s. | `Failed` |

## 8. End-to-end tests (both use cases must be covered)

Both use cases get a dedicated e2e spec. They share the existing harness shape
([test/e2e/commit_request_e2e_test.go](../../../test/e2e/commit_request_e2e_test.go)):
a dedicated Gitea repo, GitProvider with a **large `commitWindow` (300s)** so the
silence timer can never be what produces the commit, a GitTarget, and a WatchRule.
A large window plus the dedicated repo realize A1; the dedicated namespace/repo
realizes A2.

### 8.1 E2E-1 — UC1 "Save" (delaySeconds = 0)

This is essentially the **existing** first spec ("finalizes the open commit window
on demand and reports the resulting SHA") and is the regression anchor for UC1:

1. Apply a Deployment (opens the window).
2. `Consistently` assert for 10s that `main` does not yet exist — the edit is held.
3. Apply a CommitRequest (`delaySeconds: 0`) with an explicit `message`.
4. `Eventually` assert `status.phase == Committed`, `status.sha` non-empty,
   `status.branch == main`.
5. Assert the commit subject equals `spec.message` verbatim, the Deployment file is
   present, and `HEAD` equals the reported SHA.

Strengthen it with one assertion: **exactly one new commit** was produced (HEAD
advanced by one), proving the save did not also trigger a stray second commit.

### 8.2 E2E-2 — UC2 `kubectl apply` bundle (delaySeconds > 0, CommitRequest first)

A new spec proving the bundle lands in one commit even when the CommitRequest is
applied **first** (the hard ordering):

1. Build a single multi-document manifest whose **first** document is a
   CommitRequest (`delaySeconds: 8`, explicit `message`) followed by **two or three
   Deployments** in the watched namespace.
2. Apply the whole bundle in one `kubectl apply -f -`.
3. `Eventually` assert `status.phase == Committed` and `status.sha` non-empty.
4. Assert **exactly one commit** was produced for the bundle (HEAD advanced by
   exactly one), its subject equals the CommitRequest's `message`, and **every**
   Deployment in the bundle is present in that single commit.
5. (Stronger variant, optional) Watch a second type so the bundle spans two per-type
   streams (e.g. add a ConfigMap to the bundle and a ConfigMap WatchRule), proving
   the one-commit guarantee holds across independent streams — mind the pre-existing
   `kube-root-ca.crt` ConfigMap noted in the existing suite when asserting commit
   counts.

The `delaySeconds: 8` value must comfortably exceed the bundle's ingestion spread so
the test is deterministic; the assertion is on the **outcome** (one commit, all
files, the message), not on internal ordering. Putting the CommitRequest first in
the file is the deliberately-hard arrangement: it exercises "save intent arrives
before the work" and proves the collect-grace (§6.2) closes that gap.

### 8.3 Intent durability — a cut-off still carries the message (§6.4)

The property §6.4 exists for is best pinned by a **deterministic worker/integration
test**, because forcing a resync to interleave is racy in full e2e:

1. Open an author window (one event), then deliver an `AttachCommitRequest` with a
   distinctive `message` and a non-zero `finalizeAt` so the window is **Attached**
   but not yet finalized.
2. Before the deadline, invoke a window-closing path directly — the
   `resync-before-apply` finalize is the canonical one.
3. Assert the resulting commit's message equals the CommitRequest's `message` (not
   the generated grouped message), and that **after the carrying `PendingWrite`
   pushes** (§6.5) `LookupCommitRequestOutcome` reports `Committed` with the **pushed**
   commit's SHA — and that the reported SHA matches what is actually on the remote.

Two complementary assertions: (a) `finalizeOpenWindowWithMessage` with no override on
a window carrying a `pendingMessage`/`pendingCR` uses the pending message and moves
the `pendingCR` onto the `PendingWrite`; (b) a push that hits a conflict and
rebase-replays still resolves the request to the *post-replay* SHA (no stale/orphaned
SHA). Add a full e2e only if a reliable resync trigger exists; the integration test is
the dependable pin.

### 8.4 Already-present — a finalize that produces no diff (§6.7)

The theoretical edge, pinned by a deterministic unit test (see §6.7): finalize a
window whose event re-asserts already-present state so `applyPendingWriteEvents`
returns `false`, and assert the carried request resolves to `Rejected` with
`reason == AlreadyPresent` **promptly** — it must not block waiting for a push that
never comes. Assert on the structured `reason`, not on message text.

## 9. Migration / order of work

1. Add **E2E-2** (UC2), strengthen **E2E-1** (UC1), and add the **§8.3 intent-
   durability** integration test — they pin the behavior before any refactor.
2. Remove the snapshot + drain step from `Reconcile`; keep attribution, delay, and
   the author-bound finalize unchanged.
3. Delete (or park behind a clearly-labelled "future, target-local" stub)
   `TakeTypeSnapshot` / `DrainTailsToSnapshot` and the per-type tail **cursor**
   bookkeeping used only by the barrier. The tail itself, its checkpoint anchor, and
   the late-event nudge stay — they are the ingestion path, not the barrier.
4. Retire the Option-A barrier-timeout status wording along with the barrier. Leave
   the happy-path status clean: §6.5 makes `Committed` mean "pushed", which is the
   honest claim. The lagging-stream caveat (§4) lives in the docs, not on every
   request.
5. Worker: ensure **any** finalize that closes a window schedules its push (the
   stranded-write fix) — prerequisite for §6.4 to be safe under a cut-off.
6. Build **§6.4 eager attach**: move the message onto `openWindow`, make
   `finalizeOpenWindowWithMessage` honor the attached message and carry the
   `pendingCR` onto the resulting `PendingWrite`, evolve the `FinalizeSignal` into an
   idempotent `AttachCommitRequest`, switch the controller to attach-then-poll, and
   **re-anchor the grace at attribution** — the controller drops its own delay and the
   worker stamps `finalizeAt = receipt + delaySeconds` (§6.4.4), replacing today's
   creation-anchored delay.
7. Build **§6.5 resolve-on-push**: add `CommitRequest` + `CommitSHA` to `PendingWrite`,
   capture the commit hash from `executePendingWrite` (refresh it on rebase-replay),
   and resolve the request from the push success path with the pushed SHA. This makes
   `Committed` mean "on the remote" and removes the stale-SHA case.
8. Adopt **§6.7 `Rejected`**: rename the `NoOpenWindow` phase to `Rejected`, add the
   structured `status.reason` (`NoWindowInGrace` / `WindowMismatch` / `AlreadyPresent`),
   and resolve a no-diff finalize (`anyChanges == false` carrying a `pendingCR`)
   immediately as `Rejected`/`AlreadyPresent` rather than letting it wait on a push.
   Update the existing e2e assertions that expect `NoOpenWindow`. (CRD enum change is
   acceptable in `v1alpha1`.)
9. Document restart recovery as out of scope (§6.6) — no durable record built for now.
10. Future, only if proven needed: the idle-reset grace variant (§6.4.4), and — if
    cross-type skew is ever shown harmful — a **target-local** audit-anchored
    watermark, never the global cursor.

## 10. Bottom line

The refactor's one true cost is the loss of a single total order, and we tried to
buy it back with a per-type watermark barrier that is best-effort, partly incorrect,
and aimed at a case neither use case needs. Strip it. The promise users care about —
**save this author's recent work, attributed correctly, with their message, and tell
me the SHA** — is kept by two things we already have: **attribution from the
CommitRequest's own audit event** and a **configurable collect-grace**. UC1 needs
only the first; UC2 needs both, with `delaySeconds` sized to the bundle. Be honest in
status about what "before the CommitRequest" can mean across independent streams, pin
both use cases with e2e tests, and resist rebuilding the total order until something
real proves we need it.

And grow toward **eager message attachment** (§6.4): bind the author's message to
their open window the instant we can, so the grace period protects intent rather than
merely deferring action. Then the worst realistic interruption — a resync cutting the
window — yields a commit *with the user's message*, and the only thing we cannot save
is an interrupt that beats attribution itself.

Finally, let the `PendingWrite` carry the request to its end (§6.5): it already holds
the message verbatim and survives every replay, so it is the natural place to also
hold the result handle and the commit SHA. Resolve the request **on push**, with the
pushed SHA — and `Committed` stops meaning "written locally" and starts meaning "on
the remote," with no stale SHA and at near-zero cost. Restart recovery we knowingly
leave best-effort (§6.6). That is a contract worth being proud of: simple at the core,
honest at the edges, and durable exactly where it counts.
