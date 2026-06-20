# Two e2e failures during the late-lane removal — facts & root-cause (2026-06-18)

Status: FINISHED / HISTORICAL INVESTIGATION. Records what actually happened in the e2e runs that validated
the late-lane removal ([audit-diagnostic-streams-plan.md](../design/stream/audit-diagnostic-streams-plan.md)), so the
two failures are diagnosed, not waved away as "flakes." One is a known timing flake; **the other is
a genuine, unexplained attribution anomaly that pre-dates and is independent of the late-lane
removal, and deserves its own fix.**

## 0. The three runs (context)

| Run | Result | Failed spec | `category="error"` (the removal's own gate) |
| --- | --- | --- | --- |
| 1 (`bdl3jrcqb`) | FAIL 44/1/11 | Aggregated API "should never drop … as shallow" (`shallow_dropped=1`) | 0 ✅ |
| 2 (`e2e-rerun`) | FAIL 46/1/9 | Commit Request "holds a new edit in the open window…" (attribution fail-closed) | 0 ✅ |
| 3 (clean) | **SUCCESS 48/0/8** | — | 0 ✅ |

Two runs failed **different** specs; the third passed clean. The late-lane removal's own invariant
(`audit_events_total{category="error"} == 0`) was green in all three. That pattern — different
single-spec failures, a clean pass — is the signature of pre-existing flakiness, not a deterministic
regression. But "flaky" is a conclusion to *earn*; below is the evidence for each.

## 1. Failure 1 — aggregated `shallow_dropped=1` (run 1)

**Facts.**
- The metric breakdown showed `shallow_dropped=1` in run 1; **`shallow_dropped=0` in runs 2 and 3.**
- The failing spec asserts `audit_events_total{outcome="shallow_dropped"} == 0` *Consistently*.
- A `shallow_dropped` is produced by the joiner's `dropShallowOfficial`: an identity-shallow official
  event (no body) waited up to `--audit-event-body-wait` (500ms) for the additional-body proxy's
  contribution, and none arrived in time.

**Mechanism (known flake class).** The aggregated-API path depends on the apiservice-audit-proxy
delivering the request/response body that the official audit event lacks; under load the proxy can
lose the 500ms body-wait race for a single event → one shallow drop. This is the documented
aggregated body-join race (cf. the 2026-06-13 shallow-drop fix, which addressed the *deletecollection*
variant; a non-deletecollection event can still lose the race).

**Independence from the late-lane removal (FACT).** The shallow-drop *decision* lives entirely in the
joiner's body-join path, which is **upstream of and untouched by** the queue change. Slice 1 only
moved *where the metric is recorded* (joiner → handler), not whether a drop happens; the nil-joiner
fix only affects `Joiner == nil` (not the e2e, which wires a joiner). So this change cannot raise the
shallow-drop count.

**Verdict.** Pre-existing timing flake. The test asserting `== 0 Consistently` is itself fragile to
the body-wait race. Evidence-limited only in that run-1's cluster was reclaimed before we could dump
the exact event — but the metric trend (1 → 0 → 0) and the mechanism are conclusive.

## 2. Failure 2 — CommitRequest attribution fail-closed (run 2)

The failing spec ("holds a new edit in the open window without advancing an already-established
branch", `commit_request_e2e_test.go:202`) waited for the CommitRequest to reach `phase=Committed`.
The controller log is explicit:

```
CommitRequest finalized  name=commit-request-hold-save-1781816302  ns=…-test-commit-request
  phase="Failed"
  message="could not attribute the CommitRequest to an author:
           its create audit event was not observed within the attribution window"
  age="1m0.141s"
```

So the finalize **failed closed on attribution**: `LookupCommitRequestAuthor` did not find the
request's own `create` event in the commitrequests audit stream within the window.

### 2.1 What we proved about the create event (from run-2's `diag_all` dump)

| Fact | Value | Source |
| --- | --- | --- |
| The create **was ingested** | `outcome="queued"`, `category="stored"`, `placement="resource-version"` | `diag_all` entry, auditID `b6eb9b76` |
| It has a numeric RV | `3208` | the entry's `resource_version` |
| It was **not diverted** | `commitrequests older_than_high_water = 0` (the run's one divert was a *deployment*) | metric breakdown |
| It was ingested **promptly** | `diag_all` entry id `1781816561204-…` = **21:02:41.204Z**, 185 ms after the apiserver create (`stageTimestamp` 21:02:41.019Z) | entry id |
| It was **well within the scan bound** | only **73** commitrequests events in the whole run, vs `commitRequestAuthorScanCount = 512` | `grep -c` on the dump |
| The scan retried **~30×** | `commitRequestAttributionTimeout = 60s`, `commitRequestAttributionRetryDelay = 2s` | [commitrequest_controller.go:83-87](../../../internal/controller/commitrequest_controller.go) |
| name/namespace/uid match | objectRef `…-test-commit-request/commit-request-hold-save-1781816302`, uid `b3d9e2da…` | the entry; the controller passes the same `CommitRequest.UID` |

### 2.2 Ruling out the easy explanations (FACTS)

- **Not ingestion lag.** The create was in the main stream from 21:02:41.204; the window ran until
  21:03:41, scanning every 2s. ~29 of ~30 scans happened *after* the event was present.
- **Not the 512 scan bound.** 73 commitrequests events total — the create is always in the newest 512.
- **Not the UID guard.** The scan treats an empty objectRef UID as a match, and the body-backfilled
  UID equals the object's UID; either way it matches.
- **Not the late-lane removal (the change under test).** `LookupCommitRequestAuthor` now scans only
  the main stream, but `scanForCommitRequestCreate` is byte-identical to before, and the create was
  `queued` to the **main** stream — it was never a late-lane entry. The removed late-lane fallback
  only ever helped *diverted* creates; this create was not diverted. So pre-change behaviour on this
  exact event would have been identical (the main-stream scan, run first, with no late entry to fall
  back to). **The change cannot explain this miss.**

### 2.3 What remains (OPEN — a suspected pre-existing attribution race)

A `create` event that is provably in the scanned stream for the entire 60s window, within the scan
bound, with matching identity, was **not found by ~30 successive scans**. None of lag / bound / UID /
the late-lane change explains it, and run 3 passed (so it is **intermittent**). That is a genuine
anomaly, not a clean flake. Candidate causes, none confirmable from the post-hoc dump:

1. **A parse/skip edge on the stored entry** — if `parseAuditEvent`/`VerbToOperation`/identity
   extraction skips this specific commitrequests entry, every scan would silently miss it (this is
   exactly the kind of consume-side skip the outcome taxonomy calls out; for a *scan* it is invisible).
2. **A reconcile/scan race or requeue stall** the logs don't show (the reconcile did run for the full
   minute, so a total requeue stop is unlikely, but a per-attempt read anomaly is not excluded).
3. An environment effect (e.g. the restart-reconcile spec) — but its activity is timestamped *after*
   the failure (21:04:57 vs the 21:03:41 timeout), so it is not concurrent.

**This warrants its own investigation** with a reproduction that dumps the live stream contents at the
moment attribution fails (compare what the scan reads vs what `diag_all` shows). It is **not** a
blocker for the late-lane removal (§2.2), but it is a real fragility in CommitRequest attribution.

## 3. The unifying point, and why it argues for the attribution redesign

Both failures are **load/timing fragilities in the audit ingestion pipeline under the heavy concurrent
e2e suite**, and neither is caused by the late-lane removal. Failure 2 in particular is a sharp
example of the fragility flagged in
[internal-audit-type-demand.md](internal-audit-type-demand.md): **CommitRequest attribution is
coupled to scanning a shared, ordered audit stream**, and that scan can fail to deliver a present
fact for reasons that are hard to even diagnose. A dedicated **author-attribution store written at
ingestion**, read by a direct `{ns,name,uid}` key lookup (Option B of that doc), removes the scan
(and thus this whole failure class): no 512 bound, no ordering/divert dependency, no "scan missed a
present event," and — if written *before* the ordered-ingestion serialization — no coupling to mirror
throughput. That is the recommended fix for the *attribution* fragility, independent of the late-lane
work.

## 4. Recommendations

- **Late-lane removal:** validated — green on a clean run (run 3), its own invariant green in all
  three runs, and both failures shown independent of it. No action needed for the removal itself.
- **Failure 1 (shallow-drop assert):** consider softening the aggregated test's `== 0 Consistently`
  to tolerate the inherent body-wait race (e.g. assert the *body was recovered for the test's own
  Flunder* — a convergence assert — rather than a global zero), or treat it as a known flaky guard.
- **Failure 2 (attribution):** open a focused investigation (§2.3) with a live-stream dump on
  attribution failure; pursue the attribution-store redesign (Option B) as the durable fix.

---

## 5. Follow-up: a 3-run experiment (2026-06-19) — it's a flaky *suite*, and `diag_all` was adding load

To test the hypothesis that the attribution miss is a cold-start / first-event effect, three more
full runs were done: one after `clean-cluster` (cold), two reusing the cluster (warm). Result:

| Run | Cold/warm | Result | Failing spec |
| --- | --- | --- | --- |
| A | cold | **PASS** | — |
| B | warm | FAIL | Manager CRD — "create Git commit when IceCreamOrder is added" (120s timeout) |
| C | warm | FAIL | Manager WatchRule — "backfill pre-existing ConfigMap when WatchRule is added" (120s timeout) |

Combined with the earlier runs, the era's tally is **four different failing specs** (aggregated
shallow-drop, commit_request attribution, Manager CRD commit, Manager WatchRule backfill) with **no
single spec recurring**, and:

- **Cold runs:** 2 pass / 2 fail (run-3, A pass; run-2, run-1 fail).
- **Warm runs:** **0 pass / 2 fail** (B, C) — warm is where the cluster has accumulated the most
  state/load (`audit_events_total{queued}` grew 1033 → 2448 → 3859 across A→B→C, cumulative).

**Conclusion: this is a flaky *suite* of timing-sensitive specs under audit-pipeline load, not a
regression.** The materialization specs (B, C) time out waiting for an object's audit event to be
ingested → materialized → committed; the aggregated spec loses the 500 ms body-join race; the
attribution spec misses a present create. All four are *latency-vs-deadline* races in the audit
ingestion/materialization pipeline, and warm/accumulated runs are the worst — classic load
sensitivity. None touch the late-lane code path.

### 5.1 The cold-start hypothesis for the attribution miss — not supported

The failing `commit-request-hold-save` (run 2) was **not** the first `commitrequests` event:
earlier CommitRequests on the same shared stream (`commit-request-save`, `commit-request-bundle-save`)
attributed fine seconds before (`save` in 4.67 s). And A (cold) did not reproduce the miss. So
"first event on a cold stream" is not the mechanism — the miss is intermittent and, per §2.2, the
create was demonstrably **queued to the correct stream** yet missed by ~30 live scans. **Root cause
remains unexplained by any post-hoc data and requires a scan-path diagnostic to catch live** (a
present, untrimmed, correctly-keyed create missed by the scan is a true heisenbug from the outside).

## 6. A real contributor we *can* fix now: `diag_all` was enabled in e2e and adds serialized load

Slice 3 enabled the opt-in `diag_all` firehose in the e2e deployment
(`config/deployment.yaml --audit-bytype-diag`). That is now **reverted**, because:

- **It adds a second XADD per ingested event _inside the `canonicalMu` lock_.** For official-source
  events, `processEvent` holds `canonicalMu` (the in-pod ordering lock) through
  `mirrorByType → Enqueue`, and `writeDiagAll` runs there too. So with `diag_all` on, every official
  event does **two serialized Redis round-trips instead of one**. Under the heavy concurrent suite
  (thousands of events) this ~halves in-lock ingestion throughput → backlog → exactly the
  latency-vs-deadline races that produced all four flakes.
- **The teardown doesn't use it.** The `SynchronizedAfterSuite` dump reads Prometheus
  (`audit_events_total{outcome}`), not `diag_all` — so enabling the firehose in routine runs bought
  nothing automated; its only use was manual post-failure inspection (as in §2.1).

`diag_all` remains the opt-in investigation tool (enable `--audit-bytype-diag` when chasing a
specific failure); it is just no longer on by default in e2e. **(Prod note:** the in-`canonicalMu`
diag XADD is a throughput cost for anyone enabling `diag_all` always-on; a future improvement is to
move the diag write out of the ordering lock / make it fire-and-forget.)

## 7. Recommendations (updated)

- **Late-lane removal:** validated (run-3 and run-A clean passes; its `category="error"` invariant
  green in every run; none of the four flakes touch its code path). Done.
- **e2e flakiness:** `diag_all` disabled in e2e (§6) — re-measure the flake rate with it off; if the
  materialization specs (B, C) still time out, raise their deadlines or add audit-pipeline
  backpressure/throughput headroom, since they are latency-vs-deadline races, not logic bugs.
- **Commit_request attribution (§2, §5.1):** add a one-line diagnostic to `scanForCommitRequestCreate`
  (log entriesScanned + whether a create-for-name was seen, on the miss) and run cold until it
  reproduces, to finally see what the live scan reads. Independently, pursue the **dedicated
  author-attribution store written at ingestion** ([internal-audit-type-demand.md](internal-audit-type-demand.md)
  Option B / "store `commitrequests` upfront") — it removes the scan entirely, so this whole failure
  class disappears regardless of the unexplained scan miss.

## 8. Correction: disabling `diag_all` did NOT fix the flakiness

The clean validation run with `diag_all` confirmed OFF (no `:diag_all` key) **still failed two specs**
— a **fifth** distinct one ("Commit Signing — should not replay already-reconciled configmaps to a
late-joining target") plus the Manager WatchRule backfill again. So:

- **The `diag_all` in-lock XADD was not the (primary) cause.** The load hypothesis (§6) was plausible
  and the mechanism is real, but the data refutes it as *the* cause: removing it left the suite just
  as flaky. `diag_all` stays disabled in e2e on the honest grounds that it is **unused by the teardown
  and adds load** — not as a flakiness fix.
- **The flakiness is deeper and pre-existing.** Across the era we have now seen **five** different
  failing specs — aggregated shallow-drop, commit_request attribution, Manager CRD commit, Manager
  WatchRule backfill (×2), Commit Signing replay — almost all *"X should be committed/backfilled/not-
  replayed within 120 s"* timeouts. These are **materialization/commit/coverage timing races** in the
  audit→checkpoint→tail→commit pipeline under the concurrent suite, spread across unrelated
  subsystems. That spread is itself the signal: it is a general *pipeline-under-load latency* problem,
  not one feature's bug.
- **Relation to the late-lane work (FACT, restated):** none of the five failing specs touch the
  late-lane code path; the late-lane removal's own invariant (`category="error" == 0`) was green in
  every run; and the change is, if anything, marginally *faster* (one fewer XADD on divert). The
  removal stays validated. Whether the broader suite flakiness predates the whole audit-outcome series
  or was nudged by it is **not established here** — it would need a `main`-baseline flake-rate
  comparison (run `main` e2e N times) — but mechanistically these changes do not touch the failing
  specs' paths.

**Net recommendation:** treat the e2e suite's materialization-timing flakiness as its **own
investigation** (separate from the audit-outcome work): either give the worst specs more headroom
(deadlines / backpressure) or chase why a freshly-claimed type's first object can take >120 s to
materialize+commit under load. The commit_request attribution miss (§2, §5.1) is the one with a clear
durable fix in hand (the early-ingestion attribution store).

## 9. Reframe and plan (2026-06-19) — find the situation; don't paper over it

Three decisions from review of the above:

1. **The branch is the new source of truth.** It has diverged from `main` enough that a
   `main`-baseline flake-rate comparison is not a useful fallback. We investigate *forward on this
   branch*, not by reverting. (So the §8 "compare to `main`" idea is dropped.)

2. **"Events can't get lost" is the invariant — fix the pipeline, don't work around it.** The
   early-ingestion **attribution store is deferred**: it would make the *CommitRequest* symptom
   disappear, but it is a workaround. Attribution scanning the normal per-type stream *should* just
   work, because every accepted event *should* reach the stream and stay there. The fact that a
   `create` we proved was `queued` was then not found (§2) is therefore **a real pipeline defect to
   find**, not a reason to stop relying on the pipeline. (This supersedes the store recommendation in
   §3 and §7 — keep it on the shelf, not the workplan.)

3. **The misses are probably ONE situation, not five unrelated flakes.** Restated symptoms:
   - commit_request: a `queued` create not found by the scan (§2).
   - Manager CRD / WatchRule backfill / Commit Signing: an expected object not materialized /
     committed / and-or *replayed-when-it-shouldn't-be* within 120 s.
   These all reduce to **"an event that was (or should have been) in the per-type stream did not get
   delivered/applied when expected."** That is a single hypothesis: *intermittently, an event is lost
   from — or stalls before reaching — the per-type ordered stream / its consumer*. The variety of
   failing specs is just the variety of consumers that then notice. Finding that one situation is the
   goal.

### 9.1 The instrumented experiment (next)

Goal: locate **where** an event is lost or stalled — at ingestion (never written), in the stream
(written then gone), or downstream (written but not delivered/applied in time).

- **Scope `diag_all` to the types we have seen fail** (`commitrequests`, `configmaps`, `secrets`,
  `deployments`, the e2e `icecreamorders` CRDs) instead of every claimed type, to keep the firehose
  (and its load) bounded while still capturing the suspects' full event flow. (Implementation: a
  resource allowlist on the `diag_all` writer.)
- **Run the full suite three more times** under this scoped capture.
- **On any failure, capture the live cluster immediately** and answer, for the failing object:
  *is its audit event in `diag_all` (ingested)? in the per-type `:audit:stream` (queued)? did it
  materialize / commit?* — which pins the loss/stall to a stage. (`diag_all` already proved the
  attribution create was ingested+queued; the open question is whether it later *left* the stream, or
  whether the consumer simply never read it.)

Expected outcome: either we catch an event **missing from the stream** (→ an ingestion/trim/eviction
loss to fix at the source) or **present-but-not-consumed** (→ a tail/scan/materialization delivery
bug). Either way it names the situation, which is the prerequisite to fixing the pipeline rather than
masking a symptom.

## 10. FOUND: the situation — a re-claimed type stuck in materialization `phase=removed` (2026-06-19)

The scoped `diag_all` experiment (§9.1) ran cold→warm→warm. Runs 1–2 passed; **run 3 (warm) failed two
specs**, and the firehose + a live Valkey keyspace dump pinned the cause of the headline one.

### 10.1 The failing object, traced end to end (Manager CRD / IceCreamOrder)

Spec: *"should create Git commit when IceCreamOrder is added via WatchRule"*. It installs the
`crd-lifecycle.e2e.example.com` CRD, creates a WatchRule (correctly declaring **group
`crd-lifecycle.e2e.example.com`**) + GitTarget, then creates `alices-order` ~145 ms later and waits
120 s for the commit. Timeline (run 3):

| Time | Event |
| --- | --- |
| 07:50:31 | `TypeActivated` crd-lifecycle icecreamorders → **followable** (discovery layer; CRD installed) |
| 07:50:35.669 | WatchRule + GitTarget `watchrule-icecream-orders-dest` created (declares group crd-lifecycle) |
| 07:50:35.814 | `alices-order` created |
| 07:52:35 | spec times out — no commit |
| 07:52:36 | `TypeWobbling` → retained (CRD deleted at cleanup) |

Decisive evidence, all consistent:

- **The create was never ingested.** Scoped `diag_all` captured icecreamorders, yet run-3's
  `alices-order` (ns `1781855213-test-manager-crd`) is **absent** from the firehose; only run-1
  (`1781854235…`) and run-2 (`1781854844…`) `alices-order` entries exist. `audit outcome not_needed =
  1576` in the teardown — the create was gated **before the queue**.
- **The per-type stream never existed.** `__index__` lists only
  `restart-reconcile…icecreamorders` among the icecreamorder groups — crd-lifecycle has **no
  `:audit:stream`** at all.
- **Materialization never ran for it in run 3.** The durable checkpoint
  `crd-lifecycle…icecreamorders:objects:state` reads
  `{"phase":"removed", … "updated_at":"2026-06-19T07:46:01Z"}` — **that timestamp is run 2's**. It was
  only "restored materialization checkpoints from durable state" at boot, then never updated. There
  are **zero** Declare/Requested/Syncing/Synced/Require/materialize log lines for crd-lifecycle in
  run 3 (count = 0). Contrast a healthy type, `restart-reconcile…icecreamorders:objects:state` =
  `{"phase":"synced","count":3,"resource_version":"8450","updated_at":"07:53:01Z"}` (live).
- **Runs 1–2 passed for the opposite reason:** their `alices-order` create *was* in the firehose
  (ingested) → committed via the live audit/tail path; no resync was needed.

### 10.2 Root cause — confirmed *where*, likely *why*

**Confirmed (hard evidence above).** The event was lost **at the demand gate**, and the type was **never
`Required` and never checkpoint-synced** in run 3. Both delivery paths were dead: the gate never marked
the type wanted (first `create` gated `not_needed`, absent from the firehose), and no checkpoint LIST
ever ran to heal it. → no commit → 120 s timeout.

**Not yet uniquely proven — the *why*.** What we have NOT shown is *why* the demand chain never engaged.
The timeline even cuts against the simplest story: `TypeActivated`(followable) at 07:50:31 preceded the
GitTarget (07:50:35) and the create (07:50:35.814), so the type *looked* followable when the GitTarget
Declared. The missing fact is **what GVR set `DeclareForGitTarget` actually produced** — which logs
nothing today. Candidate triggers, all consistent with the evidence: **W1** resolve failed closed on a
sibling `retained` type; **W2** the type was briefly out of `Followable()` so an empty-observable resolve
withdrew the claim (→ long requeue); **W3** it resolved to the wrong same-Kind GVR (the 07:51:31 heal on
this target was scoped to `wildcard-watchrule`, not `crd-lifecycle`); **W4** the Declare did not re-run
post-activation in the window. The `phase=removed` durable value is *evidence the sync never re-ran*, not
itself the blocker.

It is **intermittent**: cold (run 1) works fresh; a warm run where the chain engages in time (run 2)
works; the warm run where it does not (run 3) fails — a **race in the demand handshake under warm/load**.

The fix and the plan to **prove the exact why** (a one-line diagnostic + a red test, then a re-run) live
in [first-event-loss-on-reclaim-plan.md](first-event-loss-on-reclaim-plan.md): claim the rule's
fully-specified GVR unconditionally + open the gate on claim (robust across W1–W4), with a bounded
backstop. See that doc §1.1 and slice S0.

### 10.3 Why this is exactly the "events can't get lost" violation

The demand-gating design tolerates a gated-out event by promising it is **checkpoint-healed**
([[demand-gated-audit-ingestion-proposal]]). That guarantee is **void when materialization itself is
stuck**: here the type never reached `Synced`, so the gate dropped the live event *and* no checkpoint
LIST backstopped it. The event was genuinely lost. The fix is not a workaround store (§9.2) — it is to
make the **re-claim of a `removed` type promptly and reliably re-Declare → Sync** (and/or make discovery
`followable` and demand `Require` converge before the first event), so neither path can silently drop
the first object of a freshly re-claimed type.

### 10.4 The second run-3 failure is a *different* race (not this bug)

*"Commit Signing … late-joining target"* failed because `overlap-b-cm-15` was missing under path B
(`signing_e2e_test.go:609`). Configmaps is a common core type (never `removed`), so this is **not** the
`removed`-phase stall; it is a distinct late-join coverage gap.

> **Reframed since (2026-06-19/20) — do not trust the "snapshot/coverage-completeness latency" wording
> above.** A dedicated deep-dive,
> [`signing-overlap-band-coverage-drop-investigation.md`](../design/stream/signing-overlap-band-coverage-drop-investigation.md),
> proved the missing object was **not a normally-ordered main-stream entry within B's reconcile cut** and
> was **permanently absent** (not merely late) — so it is a real coverage *gap*, and the assertion's
> timeout (90 s by the suite default, not 30 s) is not the cause. Which path off the stream is
> **unconfirmed**; the intuitive "divert" explanation is **contradicted** by `lateCount=0` for the type.
> It is tracked as **Flake B** in
> [`residual-e2e-flakes-2026-06-19.md`](../design/stream/residual-e2e-flakes-2026-06-19.md); instrumentation to capture
> it on the next reproduction has landed (`2bd5303`).

### 10.5 Recommendation — UPDATED (fix landed; residual flakes split out)

- **The demand-handshake fix LANDED and is validated** — designed and implemented in
  [first-event-loss-on-reclaim-plan.md](first-event-loss-on-reclaim-plan.md) (S0–S3, committed `5d85e7d`):
  claim the rule's fully-specified GVR unconditionally + open the gate on the claim (capture before
  baseline). The formerly-flaky IceCreamOrder/backfill specs now pass 7/7 across two validation rounds.
- **The two remaining `E2E (full)` flakes are unrelated and pre-existing**, tracked in
  [`residual-e2e-flakes-2026-06-19.md`](../design/stream/residual-e2e-flakes-2026-06-19.md): **Flake A** = the
  CommitRequest finalize/attribution-scan miss (this §2's anomaly, still root-OPEN); **Flake B** = the
  late-join overlap drop (§10.4, deep-dive above). Neither is caused by the fix; both are instrumented,
  awaiting a reproduction to confirm the mechanism before any fix.
- The late-lane removal and the scoped-firehose change are untouched by all of this.
