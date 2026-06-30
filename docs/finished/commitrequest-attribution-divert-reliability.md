# CommitRequest attribution reliability vs. the RV-below-high-water divert

Status: DECISION RECORDED / IMPLEMENTED — 2026-06-20
Author: investigation of CI run 27873681498 (branch `poc/redis-copy`)

Implementation note: this branch adopts Option C. The webhook captures a small
CommitRequest author fact from the accepted, body-merged create audit event before the
demand gate and before the ordered per-type stream write/divert. The finalizer reads
that keyed fact; `commitrequests` is no longer in `AlwaysAllow`. The normal per-type
mirror path is still intact: if a GitTarget claims CommitRequests, their full audit
events continue through the demand-gated stream and can still be materialized into git.
The fact is an idempotent Redis upsert with a TTL chosen to cover leader failover as well
as the attribution window.

> **TL;DR** A green-everywhere-else CI run went red on one e2e spec because a
> CommitRequest's own `create` audit event was never matched on the ordered
> `commitrequests` stream within the 60 s attribution window, so the finalize
> failed closed (`phase=Failed`). The documented cause of an unattributable
> create is the **RV-below-high-water divert**. The tempting bold move — *stop
> diverting* — is the **wrong cut**: the divert protects a load-bearing
> invariant (R8: the per-type log is strictly RV-ordered) that the checkpoint
> splice, the trim cursor, and the signing coverage-watermark all depend on.
> The right cut is to **stop making attribution depend on the ordered stream at
> all** — capture the author as a small keyed fact at ingestion, before any
> ordering/divert decision. That fix is already designed in
> [internal-audit-type-demand.md §5.1](internal-audit-type-demand.md); this run
> is the CI evidence it was waiting for. A separate, latent **commit-ordering**
> problem in the e2e suite is documented here as open work.

---

## 1. Facts first — exactly what happened

Everything in this section is taken from the run logs and the code, not inferred.

### 1.1 The run

| Field | Value |
|---|---|
| Run | [27873681498](https://github.com/ConfigButler/gitops-reverser/actions/runs/27873681498) (PR #167) |
| Branch / sha | `poc/redis-copy` / `a7c4dcd` (*feat(api)!: rename API group version v1alpha1 → v1alpha3*) |
| Result | `Ran 48 of 56 Specs` — **47 Passed | 1 Failed | 0 Pending | 8 Skipped** |
| Jobs | Build, Unit tests, Lint, Helm, devcontainer, **E2E (quickstart)** all green; only **E2E (full)** failed (`exit code 201`) |

### 1.2 The failing spec

- `Commit Request [It] finalizes a CommitRequest created with metadata.generateName` `[commit-request, audit-consumer]`
  — [commit_request_e2e_test.go:228](../../test/e2e/commit_request_e2e_test.go#L228).
- Assertion that failed ([:245](../../test/e2e/commit_request_e2e_test.go#L245), reported at [:251](../../test/e2e/commit_request_e2e_test.go#L251)):

  ```
  [FAILED] Timed out after 120.000s.
  The function passed to Eventually failed at commit_request_e2e_test.go:245 with:
  a CommitRequest created via generateName must reach Committed
  Expected
      <string>: Failed
  to equal
      <string>: Committed
  ```

- The object was `commit-request-gen-1781965173-wxzs5`, created `14:24:10.851`, polled for its
  `.status.phase` until the 120 s `Eventually` expired at `14:26:10.917`.

### 1.3 The confirmed root cause (from the captured manager logs)

The controller manager logs are dumped into the e2e output at suite end (real time in the JSON
`ts` field). The deduped CommitRequest timeline for namespace `1781965173-test-commit-request`:

```
14:24:10Z  Opening commit window            (author=system:admin, deployments/…)
14:24:10Z  Resync request applied           committed=false
14:25:11Z  CommitRequest attribution timed out; failing closed     ← 60s after the 14:24:10 create
14:25:11Z  CommitRequest finalized
```

- There is **exactly one** `attribution timed out; failing closed` line in the entire run, at
  `14:25:11Z` — exactly 60 s after the gen CR's `14:24:10` create. That is
  `commitRequestAttributionTimeout = 60s`
  ([commitrequest_controller.go:160-167](../../internal/controller/commitrequest_controller.go#L160-L167)).
- `phase=Failed` is therefore the **attribution-timeout** path:
  `writeTerminalStatus(..., errors.New(attributionFailedMessage))` →
  `applyFinalizeResultToStatus` `finalizeErr != nil`
  ([commitrequest_finalize.go:57-60](../../internal/controller/commitrequest_finalize.go#L57-L60)).
- It is **not** the resolve-timeout path: `commitRequestResolveTimeout = 60s + 300s + 120s = 480s`
  ([commitrequest_controller.go:98](../../internal/controller/commitrequest_controller.go#L98)), well
  outside the 120 s test window.
- It is **not** a git finalize error: the two sibling specs in the same suite
  (`commit-request-save`, `commit-request-hold-save`) **committed successfully** in the same run.

So: the gen CR's own `create` audit event was **not matched on the ordered `commitrequests` stream
within 60 s**, and the request fails closed by design.

### 1.4 The mechanism (why the create can be unmatched)

Attribution scans **only the main ordered stream**:

```go
// commitrequest_author.go:52-55
// Only the main ordered stream is scanned: a CommitRequest create whose audit event was
// diverted (RV below the high-water) is rejected from that stream and stays unattributable,
// so the finalize fails closed — the documented behaviour (rare, since CommitRequests are low-volume).
```

`LookupCommitRequestAuthor` → `scanForCommitRequestCreate` does one
`XREVRANGE … COUNT 512` over `…commitrequests:audit:stream` and matches by `namespace/name`(+uid)
([commitrequest_author.go:56-90](../../internal/queue/commitrequest_author.go#L56-L90)).
An ordered (numeric-RV) event whose RV is **strictly below the per-type high-water** is rejected
by Valkey's strong key ID ordering (`equal or smaller`), and the ingest path treats that rejection
as the **divert** signal — the event is *not written to any stream*; it becomes a counter + outcome
metric + (opt-in) `diag_all` record + a `lateNotify` checkpoint nudge
([redis_bytype_queue.go:551-560](../../internal/queue/redis_bytype_queue.go#L551-L560),
[markDiverted:602-614](../../internal/queue/redis_bytype_queue.go#L602-L614)).

**Honest confidence note.** A divert emits **no log line** (only a counter / outcome metric, and
`diag_all` was OFF this run), so I could not positively observe a divert record for *this* create.
What is certain: (1) `phase=Failed` via the 60 s attribution timeout (logged); (2) the create was
not matched within 60 s. Divert is the **documented and most-likely** cause; the residual
alternatives are (b) audit delivery delayed > 60 s, or (c) > 512 newer `commitrequests` entries
pushed the create out of the scan window — (c) is implausible at this volume (a handful of CRs).
**All three are eliminated by the same fix** (§4), which is why the exact micro-cause does not
change the recommendation.

### 1.5 What it is NOT

- **Not the last commit (the v1alpha1→v1alpha3 rename).** The rename is mechanically inert for this
  path: `AlwaysAllow` keys on the **group** `configbutler.ai` (unchanged — only the *version* moved)
  ([cmd/main.go:280-281](../../cmd/main.go#L280-L281)); the `auditutil` changes are pure
  import-alias + `OperationType` package renames with unchanged enum *values*; the sibling
  CommitRequest specs passed in the same run; and the commit was validated `test-e2e 48/0` on a
  clean cluster locally.
- **Not the same spec as the two immediately-prior CI reds.** Runs `27723359433` (06-17) and
  `27736294243` (06-18) both failed in `[SynchronizedAfterSuite]`
  ([e2e_suite_test.go:117](../../test/e2e/e2e_suite_test.go#L117)) — the "late lane must be empty"
  invariant (the [late-lane invariant flake investigation](../design/stream/late-lane-e2e-2026-06-16-investigation.md)). Same audit-ordering **root family**, a
  different surface.

---

## 2. Why the divert exists (and why "just stop it" is the wrong cut)

The divert is not incidental — it is how the per-type audit log keeps a property the rest of the
system is built on:

> **R8 / DEC-3 — the per-type `:audit:stream` is strictly RV-ordered; we never knowingly insert an
> out-of-order event.** ([api-source-of-truth-reconcile.md DEC-3](../finished/api-source-of-truth-reconcile.md))

Concrete consumers that **depend** on that invariant today:

1. **The checkpoint splice.** A reconcile replays the log with an *exclusive* `(R +` range from the
   checkpoint's `:objects:rv` and folds forward
   ([api-source-of-truth-reconcile.md](../finished/api-source-of-truth-reconcile.md), the
   `XRANGE :audit:stream (R +` loop). If a strictly-older event were forced into the stream above
   `R`, the splice would fold a **stale** create/update over newer state — the exact ordering
   inversion the divert prevents.
2. **The trim cursor.** `TrimTypeAuditLog` does `XTRIM … MINID <rv>`
   ([redis_bytype_queue.go:334-343](../../internal/queue/redis_bytype_queue.go#L334-L343)). It is
   safe only because stream position tracks RV order.
3. **The signing coverage watermark** is a full stream position `<rv>-<seq>`
   (the [signing tail-replay watermark fix](../design/stream/signing-snapshot-tail-replay-failure-investigation.md)). Out-of-order insertion corrupts the watermark
   arithmetic and re-opens the signing tail-replay failure that work just closed.

So the *naive* bold move — "stop diverting, force every event into the stream in arrival order" —
trades a **rare, localized** attribution flake for **systemic** ordering fragility across the
splice, the trim, and signing. That is the wrong trade.

A *softer* "no divert" (demote an older-numeric-RV event to attach at the high-water with a tag, the
way RV-less events already do — [ingestRVLess:566-600](../../internal/queue/redis_bytype_queue.go#L566-L600))
is **less wrong**, but it pushes a new burden into every fold/replay consumer: each must now skip
entries whose event-RV is older than what it has already applied (a per-object last-RV guard on the
hot path). More places to get right than the targeted fix below.

### 2.1 The actual asymmetry — attribution is the one consumer the backstop can't heal

The divert is declared "safe" because the **checkpoint LIST is the correctness backstop**: a
diverted event's *effect on object state* is recovered when the next checkpoint re-reads the live
API ([redis_bytype_queue.go:566-578](../../internal/queue/redis_bytype_queue.go#L566-L578),
nudged via `Materializer.RequestResync`,
[materializer.go:348-375](../../internal/typeset/materializer.go#L348-L375)).

But **attribution is authorship, and authorship lives only in the audit event — never in object
state.** A checkpoint LIST of `commitrequests` proves the object exists; it cannot tell you *who*
created it (the `userInfo` was in the diverted event). This is stated outright in the existing doc:

> *"git materialization … is fine [under divert], because the checkpoint re-reads object state. But
> attribution is authorship, which lives only in the audit event, not in object state — the
> checkpoint cannot recover it."* — [internal-audit-type-demand.md §7](internal-audit-type-demand.md)

**Principle:** the ordered-log + checkpoint model is correct for consumers whose truth is *current
state*. It is structurally wrong for consumers that need a *per-event fact* (who/when). Attribution
is the first such consumer; any future one (audit trails, event-sourced metadata) has the same shape.

---

## 3. Options considered

| # | Option | Divert kept? | Blast radius | Verdict |
|---|---|---|---|---|
| A | **Stop diverting; force all events in (arrival order)** | No | Splice + trim + signing watermark all break | ✗ Wrong cut (§2) |
| B | **Demote older-numeric-RV to "attach at high-water" + tag** | No (soft) | Every fold/replay consumer needs a per-object last-RV guard | ~ Viable but spreads cost |
| C | **Dedicated divert-immune attribution store** (capture `{ns,name,uid}→{author,ts}` at ingestion, before the ordering decision) | **Yes** (for the consumers that need it) | Attribution only | ✓ **Recommended** |
| D | Widen scan / make webhook deliver in order | Yes | Band-aid; doesn't fix divert | ✗ |

Option C is **already fully designed** as
[internal-audit-type-demand.md §5 / §5.1](internal-audit-type-demand.md) ("a small keyed fact
written early at ingestion, not a full always-on mirror"). Its properties:

- **Divert-immune & scan-miss-immune** — written when the event arrives at the handler, before any
  RV-ordering/`canonicalMu` decision; there is no scan and no dependency on the event landing in the
  ordered stream.
- **Deletes the `AlwaysAllow` special-case** — `commitrequests` no longer needs to be mirrored at
  all; the "who demands this type?" question dissolves.
- **Removes the "audit log as a correctness input" anti-pattern** — the same one whose removal
  justified deleting the late lane.
- **Fails loud** — a failed attribution *write* is a visible functional failure, not a silent 60 s
  fail-closed.

That doc was explicitly waiting for exactly this run: it calls a CI red here
*"a live, CI-visible argument for Option B/§5.1."* This is it.

### 3.1 Does Option C guarantee attribution? And are we masking a real problem?

Two honest objections were raised against Option C; both deserve a straight answer.

**"Is it a 100% guarantee?" — No, and it should not claim to be.** Attribution can never go below
one floor: *the apiserver must actually deliver the CommitRequest's create audit event to our
webhook, captured at a sufficient level.* Option C does not remove that floor; it removes the
**extra** dependencies layered on top of it.

- **Removed (self-inflicted) failure modes:** the divert, the 512-entry scan window, the RV-order
  dependency, and the open run-2 "present-but-missed" scan bug — gone, because there is no scan and
  no ordered-stream dependency.
- **Residual (fundamental) dependencies that remain:** (1) the event must arrive within the deadline
  — the finalize trigger is the controller-runtime *watch*, which fires before the audit event flows
  to us, so the **same retry-to-deadline loop** is still needed, now reading a deterministic store;
  (2) for `generateName`, the name/UID lives only in `responseObject`, so the audit policy must
  capture `commitrequests` at **RequestResponse** level (today's scan needs this too); (3) the store
  **write must succeed** (mirroring is best-effort, IR8) and the record **TTL must outlive the
  worst-case finalize delay**, not just the nominal 60 s.

The qualitative win: today attribution can fail **even when the event arrived** (our own
ordering/scan dropped it); after Option C it fails **only when the event genuinely did not arrive in
time** — a real, observable, far rarer external condition. That also makes a *future* attribution
failure meaningful again ("audit didn't deliver") instead of ambiguous noise.

**"Are we masking things — it should normally have arrived already?" — Split it in two.**

- **The divert: not masking.** The divert is *by design* (a strictly-ordered stream correctly
  rejecting older-RV arrivals), and it stays counted/metric'd, so it is never hidden. Deeper:
  `commitrequests` is **attribution-only** — never materialized, spliced, trimmed, or signed — so it
  *never needed* the strict-RV ordering that justifies the divert. Mirroring an attribution-only type
  into an order-sensitive stream was a category error; Option C corrects the category, it does not
  hide a malfunction.
- **The unexplained scan miss: here the objection is right.** Run-2 showed a create that was
  demonstrably queued, present the whole window, within the bound, name/uid matching — *yet missed by
  ~30 scans*, root cause still open. Deleting the scan deletes the one place we ever noticed that
  bug, and it likely lives in the **shared Redis range-read path** that the checkpoint splice
  (`XRANGE (R +`) and the signing replay also use. So **Option C must not be used as an excuse to
  close the scan-miss investigation** — keep a repro/guard for it independently, or a latent
  *correctness* hole in materialization could persist unseen.
- **Delivery slowness: not masked, and made more visible.** Option C still fails loudly when the
  event is genuinely late/absent; instrument the store write with ingest-lag
  (`now − event.stageTimestamp`) and you get a *direct* "attribution data arriving slowly" signal,
  better than today's indirect divert counter.

### 3.2 The tap is **additive** — the normal ingestion train stays

A clarification on Option C's shape, because it is easy to misread §5.1's *"`commitrequests` is no
longer mirrored at all"* as *"`commitrequests` can never be mirrored."* It cannot be — and must not
be read that way:

- `commitrequests` is a **normal Kubernetes resource**. A user may legitimately point a
  `WatchRule`/`GitTarget` at it and track it in git like anything else; the system "tracks anything
  they claim." So the **normal demand-gated ingestion path must remain fully available** for
  `commitrequests` whenever a GitTarget claims it.
- The attribution store does **not** intercept or bypass that path. It is an **additional, always-on
  tap of the same incoming events**, taken *earlier* (before the ordering/divert decision), that
  records only the small attribution *fact*. The event still flows through the normal train; the tap
  just copies one derived field off it sooner.
- This is exactly consistent with §5.1's actual point: **attribution should not be the thing that
  *demands* the type.** Dropping `commitrequests` from `AlwaysAllow` removes the *attribution-driven*
  demand only. If a GitTarget claims it, demand-gated mirroring activates as usual; if nobody claims
  it, it simply is not mirrored for git — but attribution still works, because the tap is independent
  of demand. So §8's B-3 should read *"confirm the type is no longer mirrored **for attribution's
  sake** (it is still mirrored on demand when a GitTarget claims it),"* not *"never mirrored."*

---

## 4. The commit-ordering claim (separate, latent — open work)

> *The reviewer's claim:* "There is now a bit of timing in the first reconcile commit — when it
> exactly arrives depends on timing — so we should be checking the last *N* commit that does **not**
> start with `reconciled`."

This is a **real and distinct** issue from §1. It did **not** fire in run 27873681498 (that failed
at the *phase* assertion, before any git-log check), but it is a latent flake class, and the e2e
suite is **already hand-rolling ad-hoc workarounds for it** — which is the strongest evidence it is
real.

**Important correction:** there is **no** existing helper that "picks the last commit not starting
with reconciles." That behaviour is **not implemented anywhere**. Reconcile commit subjects render
as `reconciled N <type>` / `reconciled N configmaps (last resourceVersion: …)` /
`reconcile(<target>): N resources` (the default `ReconcileTemplate`), versus explicit
event/CommitRequest subjects (`[CREATE] …`, `save: …`). The one piece of "reconcile-awareness"
today is an *assertion* that a message must **not** contain `reconciled`
([gittarget_isolation_e2e_test.go:139](../../test/e2e/gittarget_isolation_e2e_test.go#L139)) —
the opposite of a skip-filter.

### 4.1 e2e inventory — where "latest commit" is read

**(A) Reads the latest/HEAD commit unscoped — can be fooled by a trailing reconcile commit:**

| File:line | Reads | Asserts |
|---|---|---|
| [commit_request_e2e_test.go:146](../../test/e2e/commit_request_e2e_test.go#L146) | `log -1 --pretty=%B` | message == explicit `spec.message` |
| [commit_request_e2e_test.go:263](../../test/e2e/commit_request_e2e_test.go#L263) | `log -1 --pretty=%B` | message == explicit `spec.message` |
| [commit_request_e2e_test.go:395](../../test/e2e/commit_request_e2e_test.go#L395) | `log -1 --pretty=%B` | message == explicit `spec.message` |
| [commit_request_e2e_test.go:152](../../test/e2e/commit_request_e2e_test.go#L152) / :401 | `rev-parse HEAD` | HEAD == reported `status.sha` |
| [commit_window_batching_e2e_test.go:218](../../test/e2e/commit_window_batching_e2e_test.go#L218) | `show --name-only HEAD` | HEAD touched all burst files |
| [watchrule_configmap_secret_e2e_test.go:568](../../test/e2e/watchrule_configmap_secret_e2e_test.go#L568) / :583 | `log -1 --pretty=%B` / `%an` | subject has `[CREATE]`; author == `jane@acme.com` |
| [crd_lifecycle_e2e_test.go:598](../../test/e2e/crd_lifecycle_e2e_test.go#L598) / :666 | `log --oneline -n 5` | last 5 contain `DELETE` (window can drop it) |
| [repo_assertions_test.go](../../test/e2e/repo_assertions_test.go) `assertLatestCommitTouchesNoNamespaces` | `diff-tree … HEAD` | HEAD touched no forbidden ns |

**(B) Already hand-rolls a reconcile workaround — proof the class is real:**

- [signing_e2e_test.go:375-408](../../test/e2e/signing_e2e_test.go#L375-L408) — scopes to the
  ConfigMap's **own file** and loops over its whole history matching the `e2e:` prefix:
  `continue // tolerate a later heal/reconcile commit on the same file`.
- [gittarget_isolation_e2e_test.go:106-109](../../test/e2e/gittarget_isolation_e2e_test.go#L106-L109)
  — `time.Sleep(5 * time.Second)` to let "baseline reconcile commits … settle" before acting.
- [restart_reconcile_e2e_test.go](../../test/e2e/restart_reconcile_e2e_test.go) — gates on
  Prometheus reconcile/queue-depth metrics instead of reading HEAD, and uses a commit *range*
  (`headBeforeRestart..HEAD`) rather than HEAD.

**(C) Robust patterns already in use (the template for a fix):** scope to a pathspec
(`commit_author_attribution_e2e_test.go:155`, `gittarget_isolation` line 188,
`aggregated_apiserver_e2e_test.go:375`), or resolve a specific hash first
(`latestCommitHashForPath`, `signing_common_test.go`).

### 4.2 Direction for the open work

Two cheap, shared primitives turn the ad-hoc workarounds into one tested helper:

1. `latestNonReconcileCommit(checkoutDir, pathspec, subjectPredicate)` — walk `git log` (optionally
   `-- pathspec`) and return the newest commit whose subject is **not** a reconcile subject (or
   matches the caller's expected prefix). Replaces every `log -1` site in (A) and deletes the sleeps
   in (B).
2. Default every message/author assertion to **pathspec scope** — the file the test actually wrote —
   so an unrelated reconcile/heal commit on another path can never shadow it.

This is **test-robustness**, not a product bug: production never "asserts HEAD's message," and
`status.sha` correctly pins the CR's own commit regardless of what lands after. But it removes a
whole flake class and the copy-pasted workarounds. It is **independent** of §1–§3 and can land
first (it is lower-risk and unblocks noisy reds).

---

## 5. Advice — what I'd actually do

This path has to work reliably, so the bar is *deterministic*, not *less flaky*.

1. **Do not drop the RV-below-high-water divert.** It is load-bearing for the strict-RV invariant
   (R8) that the splice, the trim cursor, and the signing coverage-watermark depend on. "Be bold and
   stop the exclusion" optimizes the wrong variable: it would convert a rare, isolated attribution
   miss into systemic ordering risk. The bold-but-*right* move is to remove attribution's dependence
   on the ordered stream — not to weaken the stream.

2. **Adopt Option C — the divert-immune attribution store —
   [internal-audit-type-demand.md §5.1](internal-audit-type-demand.md).** Write
   `{namespace,name,uid} → {author, auditID, ts}` early in the webhook handler, *before* the
   ordering/divert decision; read it by key in `LookupCommitRequestAuthor`. This makes attribution
   **deterministic** (no 60 s timeout gamble), immune to divert *and* scan-miss *and* delivery delay
   (the three §1.4 candidates collapse to one fixed answer), and it **deletes** the `AlwaysAllow`
   hardcode and the "audit-as-correctness" coupling in one move. For `generateName`, resolve the
   server-assigned name from `responseObject` at write time (same `IdentityFromAuditEvent` logic),
   so the keyed fact is stored under the resolved name. This run is the CI evidence that doc was
   waiting for — promote §5.1 from PROPOSAL to the work item.

3. **Land the commit-ordering helper (§4.2) independently and first.** It is low-risk, retires the
   `signing`/`isolation`/`restart` workarounds, and removes a latent flake class — even though it is
   not what reddened this run.

4. **Re-run the failed job now** to confirm 27873681498 was the transient flake the analysis says it
   is (a single divert/attribution miss), while (2) is built. I expect green on re-run.

**Net:** the divert stays (it earns its keep); attribution stops betting on it. After Option C, a
reordered or diverted CommitRequest create can no longer fail a finalize — the author was already
recorded the moment the event arrived. That is the reliability the path needs.

---

## 6. References

- Confirmed prior design (the fix): [internal-audit-type-demand.md](internal-audit-type-demand.md) §5, §5.1, §7
- Ordering invariant the divert protects: [api-source-of-truth-reconcile.md](../finished/api-source-of-truth-reconcile.md) (R8 / DEC-3)
- Late-lane / divert investigation lineage: [late-lane-e2e-2026-06-16-investigation.md](../design/stream/late-lane-e2e-2026-06-16-investigation.md), [residual-e2e-flakes-2026-06-19.md](../design/stream/residual-e2e-flakes-2026-06-19.md)
- Code: [commitrequest_author.go](../../internal/queue/commitrequest_author.go), [redis_bytype_queue.go](../../internal/queue/redis_bytype_queue.go), [commitrequest_controller.go](../../internal/controller/commitrequest_controller.go), [commitrequest_finalize.go](../../internal/controller/commitrequest_finalize.go)
