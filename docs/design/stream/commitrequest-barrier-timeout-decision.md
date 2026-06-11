# Decision memo: what should a CommitRequest do when the barrier can't be satisfied?

> Status: **DECIDED 2026-06-11 — Option A** (15 s bound; on expiry finalize anyway with a
> visible status reason + one counter metric; constant, not a knob). This resolves the
> "barrier timeout shape" open question of
> [canonical-stream-retirement.md](canonical-stream-retirement.md) §9. The same session
> also settled the other §9 questions: body merge KEPT (re-decide once the Joiner is
> merge-only after C-C), duplicate-absorption test goes in with C-C.
> Captured: 2026-06-11 · Decided by: Simon

## 1. The promise a CommitRequest makes

A user who creates a CommitRequest is saying: *"finalize the open commit window for this
GitTarget **now** — and the commit must contain everything I changed before I asked."*

The second half is the hard part. The user's earlier edits travel as audit events through
the per-type streams and tails before they reach the GitTarget's BranchWorker. If we
finalize the moment the CommitRequest appears, an edit the user made two seconds earlier
might still be in flight — and would land *after* the finalized commit, in a later one.
That breaks the natural reading of "commit what I just did".

## 2. How the barrier delivers the promise (settled — §6 of the retirement doc)

Kubernetes gives us a global clock for free: `resourceVersion`. Every edit the user made
*before* creating the CommitRequest has a smaller RV than the CommitRequest itself
(`rv_C`). The per-type streams are RV-keyed. So the controller can simply **wait** until,
for each type this GitTarget claims, the tail has applied every stream entry older than
`rv_C`. Then it enqueues the finalize — the BranchWorker is a single FIFO, so everything
queued before the finalize is inside the commit. Done; promise kept.

For a quiet type (no entries near `rv_C`) the check passes immediately. In the normal
case the whole barrier resolves in well under a second.

## 3. The question: the wait might not finish — then what?

The barrier can stall when:

- a tail is **far behind** on a very busy stream (catch-up takes seconds);
- a claimed type's tail is **not running** (the type fell out of followability for
  longer than the grace — rare now that typeset-grace landed, but possible);
- **Redis is briefly unreachable** (cursor cannot advance at all).

We must pick a bounded behaviour. The honest context for judging the options:

1. **Today's behaviour has the same gap.** The current consumer finalizes when the
   CommitRequest's own audit event arrives, *assuming* earlier events were processed
   first — a delivery-order hope, not a guarantee. So "finalize without the barrier" is
   never *worse* than what ships today.
2. **The checkpoint backstop catches stragglers.** A mutation that misses the finalized
   commit is not lost — the next per-type checkpoint reconcile commits it (with bulk
   attribution instead of the user's, the known R11 limitation). The damage of a missed
   barrier is *"my edit landed one commit later than I asked"*, not data loss.

## 4. The options

| | Behaviour on stall | What the user sees | Cost / risk |
|---|---|---|---|
| **A — short bound, visible degrade** *(recommended)* | Wait up to **~15 s** (≈3 tail read windows). On expiry: finalize anyway, set the terminal status with an explicit reason (e.g. `Committed` + `barrierTimedOut=true` / message "finalized without full ordering guarantee"), bump a metric. | Almost always: a normal fast finalize. On a stall: still gets the commit within ~15 s, and the status *tells them* an in-flight edit may follow in the next commit. | Honest and bounded. Needs one extra status field/message. |
| **B — long bound (60 s+), silent degrade** | Same, but wait much longer and don't surface the degrade in status (metric only). | A stuck tail makes every finalize feel hung for a minute. Degrade is invisible — user can't tell their commit might be missing an edit. | "Feels like a hang"; hides exactly the fact the user would care about. |
| **C — non-blocking requeue loop** | Don't block the reconcile; re-check cursors every ~2 s via requeue until satisfied or deadline. | Same outcomes as A, slightly later. | More moving parts (deadline persisted in status, requeue churn) for the same result; only worth it if blocking a reconcile worker ever measured as a problem. CommitRequests are rare — it won't. |
| **D — refuse: fail instead of degrading** | On expiry, do **not** finalize; mark the CommitRequest `Failed` ("ordering could not be guaranteed"). The window stays open. | Strictest reading of the promise. But the user asked for a commit and gets nothing; they must retry; and today's behaviour never refused. | Turns a freshness hiccup into a user-visible failure. Worst UX for the same underlying data-safety (the backstop exists either way). |

The timeout *value* in A is not sacred: it just needs to cover a tail's normal catch-up
(reads happen in ≤5 s blocking windows, so ~3 windows ≈ 15 s) without feeling like a hang.

## 5. Recommendation

**Option A.** It keeps the promise whenever the system is healthy, is never slower than
~15 s, is never worse than today's behaviour when the system is not, and — unlike B/D —
tells the user the one thing they'd want to know in the degraded case. C is a refactor
of A we can do later if blocking ever shows up in a profile; D punishes the user for an
internal freshness hiccup the checkpoint already heals.

What "choosing A" pins concretely:

- `FinalizeAtWatermark(gitTarget, rv, timeout)` blocks ≤ timeout (default 15 s, a
  constant — not a knob — unless operations later prove otherwise);
- terminal status carries the degrade fact (field or message) + one counter metric
  (`commitrequest_barrier_timeouts_total`);
- no behaviour change at all for the no-stall path.
