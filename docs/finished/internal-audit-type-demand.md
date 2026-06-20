# Internal-consumer demand for audit types (and CommitRequest attribution)

Status: IMPLEMENTED — 2026-06-20

Implementation note: Option B landed. CommitRequest attribution is now a small Redis-backed
keyed fact captured by the audit webhook from the accepted, body-merged create event before
demand-gating and before the ordered per-type stream write/divert. `LookupCommitRequestAuthor`
reads that fact instead of scanning `commitrequests:audit:stream`, and `commitrequests` has
been removed from `AlwaysAllow`. The normal demand-gated mirror path remains available: if a
GitTarget claims CommitRequests, their full events still flow through the per-type stream and
can be captured in git. The attribution record TTL is intentionally longer than the 60 s
attribution window so it also covers leader failover and post-failover reconcile backlog.

## 1. Scope and the question

Demand-gated ingestion only mirrors an audit type when **something demands it**. Today there are
two demand sources, and they are lopsided:

- **GitTarget claims** → the materialization layer calls `gate.Require(gvr)` synchronously the moment
  a type is **claimed** (`DeclareForGitTarget`), and `Unrequire` on the `Unclaimed` event (the sweep's
  GC of the last claim) — capture-before-baseline, see
  [first-event-loss-on-reclaim-plan.md](first-event-loss-on-reclaim-plan.md). This is dynamic,
  Redis-backed, multi-pod, and releasable ([gate.go `Require`/`Unrequire`](../../internal/gate/gate.go),
  [materialization.go](../../internal/watch/materialization.go)).
- **Internal consumers** → a **static `AlwaysAllow` list hardcoded in `cmd`**
  ([cmd/main.go:279-281](../../cmd/main.go)) that force-allows `commitrequests`, because the
  CommitRequest finalizer's author attribution scans that type's audit stream
  ([commitrequest_author.go](../../internal/queue/commitrequest_author.go)) but **no GitTarget
  claims it**.

The user's question: *who is asking for `commitrequests`?* Right now the answer is "a literal in
`cmd`," which is a smell — the component that actually needs the type (the CommitRequest finalizer)
does **not** express that need; `cmd` does, on its behalf, in a place divorced from the consumer.
**The controller should ask for its own types.** This doc designs that, and — taking the late-lane
investigation with it — questions whether scanning the audit stream for attribution is the right
shape at all.

## 2. Current mechanics (verified)

- The gate keeps two sets: a read-only **`static`** set (from `AlwaysAllow`, unioned into every
  `Allow` check, never released) and a dynamic **`members`** set (the Redis `__required__` set,
  refreshed by the subscriber loop) — [gate.go:99-207](../../internal/gate/gate.go).
- `commitrequests` is **attribution-only**. It is never written to git, never materialized (no
  checkpoint, no per-type tail). The only consumer is `LookupCommitRequestAuthor`, which
  `XREVRANGE`-scans `…commitrequests:audit:stream` for the request's own `create` event to resolve
  the author ([commitrequest_author.go:56-61](../../internal/queue/commitrequest_author.go)).
- The CommitRequest **finalize trigger is _not_ the audit stream** — it is the controller-runtime
  watch on CommitRequest objects ([commitrequest_controller.go](../../internal/controller/commitrequest_controller.go)).
  So the *only* reason `commitrequests` is ingested at all is attribution.
- The finalizer **gates on attribution and fails closed**: if the create event has not been observed
  within `commitRequestAttributionTimeout`, it writes a terminal "attribution failed" status rather
  than finalize on a guess ([commitrequest_controller.go:158-168](../../internal/controller/commitrequest_controller.go)).
- **Investigation tie-in (the risk is broader than divert).** Two failure modes for audit-scan
  attribution are now evidenced:
  1. **Divert.** Since the late lane was removed
     ([audit-diagnostic-streams-plan.md](../design/stream/audit-diagnostic-streams-plan.md)),
     `LookupCommitRequestAuthor` scans **only the main ordered stream**, so a `create` whose audit
     event is *diverted* (RV below the high-water) is rejected from the main stream and never
     attributable → fail closed.
  2. **An unexplained scan miss (worse).** In a real e2e run, a CommitRequest `create` that was
     demonstrably **`queued` to the correct main stream and present for the whole 60s window** was
     still missed by ~30 successive scans → fail closed. Ingestion lag, the 512 scan bound, the UID
     guard, MaxLen trimming, and a restart were all ruled out; the root cause is still open. See
     [e2e-flakes-2026-06-18-investigation.md §2](e2e-flakes-2026-06-18-investigation.md).
  The upshot: **scanning the shared, ordered audit stream for attribution is fragile in ways we
  cannot even fully diagnose** — which is the strongest possible argument for not doing it.
  - **Update (2026-06-19): this bit CI.** Run `27830248680` on `5d85e7d` failed `E2E (full)` on
    *"Commit Request — finalizes the open commit window on demand and reports the resulting SHA"*
    (`commit_request_e2e_test.go:133`): the CommitRequest never reached `Committed` within 120 s while a
    *sibling* CR committed fine. It is intermittent (the re-run passed). This is **Flake A** in
    [`residual-e2e-flakes-2026-06-19.md`](../design/stream/residual-e2e-flakes-2026-06-19.md) — the leading hypothesis is
    this very scan miss (~55%), with a wrong/older/later-commit *ordering* mismatch a tracked secondary
    (~30%); a "last 5 commits" diagnostic (`153a0f2`) now prints on failure to tell them apart. It is a
    live, CI-visible argument for Option B/§5.1.

## 3. What an internal consumer actually needs

Generalising from `commitrequests`, an internal consumer of the audit pipeline needs one of two
things — and they have very different demand shapes:

1. **The full per-type stream** (to scan/replay it). This is what attribution does today. It
   requires the type to be **ingested** (gate-allowed), and it inherits the stream's ordering
   semantics — including the divert gap. It does **not** require materialization.
2. **A specific, derived fact about each event** (here: "who authored this create"). This needs the
   event **seen at ingestion**, not the whole stream mirrored, and it can be captured in a shape
   that is immune to ordering.

Today we serve (2) by doing (1) — mirror the whole `commitrequests` type just so one scan can pull
one field. That over-demands (a whole always-on per-type stream) **and** under-delivers (divert
gap). Both options below fix the "who asks" question; they differ on which of (1)/(2) we build.

## 4. Option A — first-class self-demand (keep audit-scan attribution)

Make the internal demand **expressed by the consumer**, not hardcoded in `cmd`. The gate's `static`
set is already the right home (always-allowed, never-released, identical on every pod); only its
*population* moves.

- Each internal consumer declares the audit GVRs it needs — e.g. a package-level
  `commitrequest.RequiredAuditTypes() []schema.GroupVersionResource`, owned by the CommitRequest
  controller package. `cmd` aggregates every consumer's declaration into `gate.Config.AlwaysAllow`
  (or a renamed `InternalDemand`) instead of a literal list. The demand now lives **with** the
  consumer; `cmd` only wires.
- Optionally surface it: log/metric the internal-demand set at startup so "why is `commitrequests`
  ingested?" is answerable from the running system, not from reading `cmd`.

**What this fixes:** the "who asks" smell — the consumer owns its demand, new internal consumers add
a line in their own package, and the set is auditable. **What it does not fix:** the divert
fail-closed gap (§2) — a reordered create is still unattributable, because attribution still depends
on the create landing in the ordered main stream. Cheap (a few lines), but it is a *relocation* of
the current design, not a robustness improvement.

## 5. Option B — a dedicated author-attribution store (remove the demand and the anti-pattern)

Stop scanning the audit stream for attribution. Capture the fact we need **at ingestion**, in a
purpose-built store, and let the finalizer read that.

- When the webhook handler accepts a `commitrequests` **create** event, write a small bounded record
  keyed by `{namespace, name, uid}` → `{author, auditID, timestamp, rv}` (a Redis hash/stream with
  `MAXLEN`/TTL). This is the same `resolveUserInfo` author the scan resolves today, recorded once.
- `LookupCommitRequestAuthor` reads that store by key instead of scanning a per-type stream.
- **`commitrequests` no longer needs to be ingested/mirrored at all** — the attribution store
  replaces the only reason. `AlwaysAllow` for `commitrequests` goes away; the demand question
  *dissolves* rather than being relocated.

**Why this is the principled fix (and the natural completion of the late-lane work):**

- **It is divert-immune.** The store is written when the event *arrives* at the handler, before any
  RV-ordering/divert decision. A reordered create still records its author → no fail-closed gap.
- **It removes the "audit stream as a correctness input" anti-pattern.** The same anti-pattern that
  justified deleting the late lane (a "diagnostic" surface had quietly become correctness) applies
  here: the per-type *audit log* is a reconcile/replay substrate, and attribution had piggy-backed
  on it. A dedicated store makes attribution an explicit, owned, functional surface — *"events for
  attribution, state for correctness"* applied honestly.
- **A failed attribution write is a real, visible functional failure** (not silent), exactly as we
  want for a fail-closed path — and it can be retried/alerted on its own.

**Costs:** a new ingestion-side hook (recognise CommitRequest creates, write the record), a store
with a retention/cleanup story (TTL keyed to the attribution window + a bound), and the finalizer
read path swap. More code than Option A, but it deletes the `AlwaysAllow` special-case and the
divert gap in one move.

### 5.1 "Store all `commitrequests` upfront" — the always-on variant (recommended shape of B)

The natural question — *should we just store everything for `commitrequests` from the start?* — is
exactly Option B made **always-on and demand-free**, and it is the right shape:

- **Write the record at ingestion, before the ordering/divert decision _and_ before the
  `canonicalMu` serialization** (i.e. early in the webhook handler, alongside the raw decode), so it
  is immune to *both* evidenced failure modes (divert **and** the unexplained scan miss) — there is
  no scan and no dependency on the event landing in the ordered stream. This also avoids re-creating
  the throughput cost of an in-lock second write
  ([e2e-flakes-2026-06-18-investigation.md §6](e2e-flakes-2026-06-18-investigation.md)).
- **Store the attribution _fact_, not the full event.** We only need
  `{ns,name,uid}→{author,auditID,ts}` (a tiny keyed record), not a mirror of every CommitRequest
  body. "Full store of all events" would re-introduce the per-type stream we are trying to delete;
  the keyed fact is bounded and cheap (TTL ≥ the 60 s attribution window).
- **Always-on, not demand-gated.** Because it is the controller's own hard requirement and trivially
  cheap, it needs no gate entry — `commitrequests` drops out of `AlwaysAllow` entirely and is no
  longer mirrored at all. (If we ever *also* want the full event stream for another reason, that is a
  separate, demand-gated decision — but attribution should not be the thing that demands it.)

So: **yes, capture `commitrequests` author attribution upfront — but as a small keyed fact written
early at ingestion, not as a full always-on mirror.** That is divert-immune, scan-miss-immune,
load-cheap, and dissolves the demand question.

## 6. Recommendation

**Adopt Option B; treat Option A as an optional stepping-stone only if B can't land immediately.**
B is the design the late-lane investigation points to: it answers "who asks for this type?" by
making the answer *"no one needs the type mirrored — the consumer captures its own fact at
ingestion,"* and it closes the divert fail-closed gap that A leaves open. A is a clean relocation of
the demand but preserves the fragile audit-scan attribution, so it is worth doing **only** as a
quick correctness/clarity win while B is built (and it is then deleted by B).

If we keep audit-scan attribution for any reason, Option A is mandatory (the `cmd` hardcode should
not stay) — but we should still document the divert fail-closed as an accepted, monitored edge.

## 7. The divert/ordering angle (why this matters beyond cleanliness)

The whole audit-outcome taxonomy now classifies a diverted event as `older_than_high_water` (dropped,
checkpoint-recovered) — fine for **git materialization**, because the checkpoint re-reads object
*state*. But attribution is **authorship**, which lives *only* in the audit event, not in object
state — the checkpoint cannot recover it. So an internal consumer that needs authorship cannot rely
on the ordered log surviving a divert. Option B is the only one that makes such a consumer correct:
capture the authorship at ingestion, before ordering can drop it. This is the general principle for
*any* future internal consumer that needs a per-event fact (not just attribution): **derive and
store the fact at ingestion; do not demand the ordered stream and scan it.**

## 8. Migration / slices

1. **B-1 — attribution store at ingestion (implemented).** Add the store + the webhook hook that
   writes a `{ns,name,uid}→{author,auditID,ts}` record on a `commitrequests` create. Write it **early
   in the handler — after body joining, before the demand gate and the ordering/divert decision** —
   so it is divert-immune and scan-miss-immune. Unit-test the write + the divert case (a create that
   would have been diverted still records its author).
2. **B-2 — finalizer reads the store (implemented).** Swap `LookupCommitRequestAuthor` to read the store by key;
   keep the fail-closed-on-timeout semantics (now only triggered by a genuinely-absent record, not a
   divert). e2e: the existing CommitRequest finalize spec stays green; add a divert-injection unit
   test that proves attribution survives reorder.
3. **B-3 — drop the demand (implemented).** Remove `commitrequests` from `AlwaysAllow`; confirm the type is no
   longer mirrored **for attribution's sake** — it is still mirrored on demand whenever a GitTarget
   claims it (it is a normal trackable resource; the attribution tap is *additive*, not a bypass of
   the normal ingestion train — see
   [commitrequest-attribution-divert-reliability.md §3.2](commitrequest-attribution-divert-reliability.md)) —
   and confirm attribution still works. Delete `commitrequest_author.go`'s stream scan.
4. *(Only if B slips)* **A-0 — self-demand interim:** move the `AlwaysAllow` population from the
   `cmd` literal to a `commitrequest`-package declaration that `cmd` aggregates; log the
   internal-demand set at startup.

### 8.1 HA considerations

Option B is the HA-friendly shape because it follows the existing HA design's
capture-decoupled-from-consume seam
([ha-improvements.md §1](../design/stream/ha-improvements.md)): audit intake runs on whichever
ready pod receives the Service traffic, writes the attribution fact to shared Redis, and the
leader-only CommitRequest finalizer reads that fact later. A new leader after failover needs no
handoff; it reads the same Redis keyspace.

The implementation carries four HA constraints:

- **TTL covers failover, not just attribution.** Retention must outlive the 60 s attribution window
  plus leader-failover detection and any post-failover reconcile backlog. The current TTL is 10
  minutes for that reason.
- **Duplicate delivery is an idempotent upsert.** Multiple apiservers or webhook retries can deliver
  the same create more than once, possibly to different pods. The store is keyed by
  `{namespace,name,uid}` and rewritten with the same deterministic author, so at-least-once delivery
  converges.
- **No single-writer ownership is needed.** Attribution is a keyed fact, not a checkpoint LIST or
  trim cursor. It does not need the single-writer machinery deferred in
  [ha-improvements.md §5](../design/stream/ha-improvements.md).
- **Redis HA can cause transient read misses.** If a future Redis topology reads from a lagging
  replica immediately after a write, the existing retry-to-deadline loop absorbs it. If that ever
  becomes visible, pin attribution reads to the primary; do not reintroduce stream scanning.

This is orthogonal to HA Improvement A (claim leases as Redis ZSET+TTL). Attribution is not
demand-gated after Option B, so the claim-lease migration neither blocks nor is blocked by this
store.

## 9. Non-goals / open questions

- **Not** changing the finalize *trigger* (the controller-runtime watch on CommitRequest objects is
  correct and untouched).
- **Not** re-introducing any late lane or treating the audit stream as a correctness input.
- **Open:** store shape and retention — a per-key hash with a TTL ≥ the attribution window, vs a
  bounded stream scanned by key. The window is short (a CommitRequest attributes within
  `commitRequestAttributionTimeout`), so a modest TTL bounds it.
- **Open:** whether other internal consumers exist or are foreseen. If attribution is the only one,
  B is narrowly scoped; if more are coming, the "derive-and-store at ingestion" principle (§7)
  should be stated as the house pattern and the generic self-demand registry (§4) may still be worth
  it for the genuinely "needs the whole stream" cases.
