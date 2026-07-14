# Residual e2e flakes after the first-event-loss fix (2026-06-19)

> **design** — open, not yet built. Index: [`../INDEX.md`](../INDEX.md)

Status: ACTIVE FOR FLAKE B; FLAKE A CLOSED BY THE 2026-06-20 ATTRIBUTION FIX. After the
capture-before-baseline fix
(`first-event-loss-on-reclaim-plan.md`, committed in `5d85e7d`),
the IceCreamOrder / first-event-loss flake is gone (it passed every one of 4 warm validation runs). Two
**other**, pre-existing flaky specs remain and intermittently fail the `E2E (full)` job. This doc
explains both, leading with what was *observed*, then the *interpretation* with an explicit **certainty
number** on each conclusion.

How to read the certainty numbers: they are my honest confidence that the stated conclusion is the true
explanation, given current evidence. **Facts** (observed) are stated separately and are ~100% — they are
measurements, not conclusions.

---

## Flake A — CommitRequest "finalize on demand" times out

### Facts (observed)

- CI run `27830248680` (commit `5d85e7d`, the committed fix) failed **only** the `E2E (full)` job; all
  other jobs (Build, Unit, Lint, Helm, Dev Container, **E2E quickstart**) passed.
- The failing spec: **"Commit Request — finalizes the open commit window on demand and reports the
  resulting SHA"** ([commit_request_e2e_test.go:133](../../test/e2e/commit_request_e2e_test.go#L133)).
- The assertion that timed out: `status.phase == "Committed"` within **120 s** (`Eventually`).
- In the same run, a *different* CommitRequest (`commit-request-bundle-save-…`) reached `Committed`
  fine (age 12.7 s) — so the commit/push path itself was working; one CR did not finalize.
- The run **before** this one on the branch (`27826911318`, 12:54) was **green**; an earlier era's
  warm run also failed a commit_request spec (investigation §2/§5). So this spec fails *intermittently*.

### What the spec does

```
  1. create Deployment ───────────────▶ opens a commit WINDOW (nothing committed; main not created yet)
  2. Consistently(10s): main is empty ─▶ proves the window is holding the edit
  3. create CommitRequest ────────────▶ asks the controller to FINALIZE the window NOW
  4. Eventually(120s): CR.status.phase == "Committed"   ◀── THIS timed out
```

Finalizing a window must **attribute it to an author**. The controller does that by scanning the
`commitrequests` audit stream for the CommitRequest's *own* `create` event
(`LookupCommitRequestAuthor`); it **fails closed** (never finalizes someone else's window) if it cannot
find that event within its 60 s attribution window.

```
  CommitRequest created
        │
        ▼
  controller reconcile ──▶ scan commitrequests:audit:stream for THIS CR's create event
        │                         │
        │                  found? ├── yes ─▶ attribute author ─▶ finalize window ─▶ phase=Committed ✅
        │                         └── no  ─▶ retry (2s) … up to 60s … ─▶ phase=Failed (fail-closed) ✗
        ▼
  spec polls phase==Committed for 120s ──▶ ✗ TIMEOUT  (the observed failure)
```

### Interpretation

- **This is a pre-existing flake, not caused by the first-event-loss fix.** **Certainty: 90%.**
  Reasoning: the fix changed *when the demand gate opens* (claim-time); it did not touch the
  CommitRequest finalize/attribution path, and `commitrequests` is force-mirrored by the static
  `AlwaysAllow` set regardless. A commit_request spec also flaked in a pre-fix era (§2). *Residual 10%:
  I could not pull this run's controller log (a re-run was in progress, logs locked), so I have not
  *directly* confirmed the failure reason for this exact instance.*

- **The likely root is the CommitRequest attribution-scan miss documented in
  `e2e-flakes-2026-06-18-investigation.md §2`.** **Certainty:
  55%.** That investigation proved, for a sibling commit_request spec, that the CR's create event was
  *provably present* in the scanned stream for the whole 60 s window — correct RV, within the 512-entry
  scan bound, matching uid/name, not diverted, not trimmed — yet **missed by ~30 successive scans**.
  Root cause was left **OPEN** (a suspected parse/skip edge or a live-scan race). That same mechanism
  would make this `:133` finalize spec fail-closed and time out. *Why only 55%: (a) §2 itself never
  closed the root cause; (b) the `:133` spec could instead time out for an adjacent reason — e.g. the
  Deployment's window never opened, or the push lagged — and I have not pinned this instance's reason.*

- **An ordering/sequencing cause is plausible and not yet ruled out.** **Certainty that ordering
  contributes: 30%.** The spec checks `phase==Committed` **and** then `status.sha == HEAD`, the message
  verbatim, and exactly one commit. A wrong/older/later HEAD (e.g. HEAD advancing past the
  CommitRequest's commit, or the window finalizing a different commit) would fail the sha/message/count
  checks even though *something* committed. The observed failure was the earlier `phase != Committed`
  timeout — which points more at the attribution scan than at sha ordering — but with the commit/push
  path proven working in the same run (a sibling CR committed), a sequencing mismatch cannot be excluded
  without the diagnostic below. *Why only 30%: the recorded failure was the phase timeout, not a
  sha/message mismatch; ordering is the secondary hypothesis.*

- **It is a correctness fail-closed, not data corruption.** **Certainty: 95%.** By design the finalizer
  refuses to finalize a window it cannot attribute; the cost is a failed/stuck CommitRequest, never a
  mis-attributed commit.

### What would raise certainty
The parallel agent has added a **"last 5 commits" diagnostic** (`recentCommitDiagnostics`,
[repo_assertions_test.go](../../test/e2e/repo_assertions_test.go)) to the phase/sha/message/count
assertions ([commit_request_e2e_test.go:123](../../test/e2e/commit_request_e2e_test.go#L123)): on a
failure it prints the latest 5 `origin/main` commits (sha, time, author, subject), path-scoped — so the
next reproduction makes "wrong / older / later commit, or a gap" obvious at a glance. Beyond that: the
one-line `scanForCommitRequestCreate` diagnostic from §2.3 (entries-scanned + whether a create-for-name
was seen on a miss) to separate attribution-miss from sequencing; or read the controller log on the next
reproduction. The durable fix candidate (deferred) is the early-ingestion author store
([internal-audit-type-demand.md](../architecture.md)), which removes the scan entirely.

---

## Flake B — Commit Signing late-joining target: overlap ConfigMap missing under path B

### Facts (observed)

- Spec: **"Commit Signing — should not replay already-reconciled configmaps as per-event commits to a
  late-joining target"** ([signing_e2e_test.go:507](../../test/e2e/signing_e2e_test.go#L507)).
- The convergence assertion: every `overlap-b-cm-NN` ConfigMap must be **present under path B**. It
  runs under the **90 s** suite default (`SetDefaultEventuallyTimeout(90s)`,
  [signing_e2e_test.go:82](../../test/e2e/signing_e2e_test.go#L82)); it does **not** override it.
  *(Correction: an earlier doc draft said 30 s — that was the explicit timeout in the run-3/f3-era code
  shown by `Timed out after 30.000s`; it has since been raised to the 90 s default.)*
- Observed failures: `overlap overlap-b-cm-17` (f3) and `overlap-b-cm-15` (run-3) not present under B.
- **The decisive new fact (a fresh reproduction by the parallel agent, at the 90 s timeout):** the
  overlap band **split** — `overlap-b-cm-00 … 15` landed in B's **reconcile (backfill) commit**,
  `overlap-b-cm-16` was **missing**, and `overlap-b-cm-17 … 19` arrived as **later per-event (live)
  commits**. So an object was missing **even at 90 s**, in a contiguous gap between the backfilled
  prefix and the live suffix.
- It did **not** fail the no-replay half (`"seed-cm-" must never appear as a per-event commit`); the
  failure is the **convergence** half.
- It is intermittent: passed 3 of 4 warm validation runs and did **not** fail the CI run that Flake A
  failed; a re-run after the missing-16 reproduction passed.

### What the spec does — and what the code actually guarantees at the join

B, joining late on a type A already tails, gets the object set under its own path from **two** sources:
its **initial backfill** (the splice folds the checkpoint `(R, head]` and writes a reconcile commit) and
the **coverage-watermarked tail** (forwards live entries with stream id `> Hc`).

**Key code fact (checked this session):** the splice sets `coverageHead = the last entry it folded`
(redis_type_splice.go), and the watermark is published
as **exactly that value** — `publishTargetTypeWatermark(gitDest, gvr, snapshot.CoverageHead)`
([event_router.go:241](../../internal/watch/event_router.go#L241)) — and the tail forwards id `> Hc`
(audit_tail.go). So `Hc` is **contiguous with the fold by
construction**: everything `≤ coverageHead` is in the backfill, everything `> coverageHead` is forwarded
by the tail. There is no `(S, Hc]` window for an object to fall into — *unless its stream entry is
missing entirely*. **This demotes the "watermark set ahead of the backfill" boundary-gap I hypothesised
in the first two drafts.**

The split that was actually observed (`00–15` backfilled, **`16` missing**, `17–19` live) fits a
**diverted cm-16** far better — the overlap band is one batch `kubectl apply`, so audit events can
arrive **out of RV order**; an event whose RV lands **below the stream high-water** is **diverted**
(rejected from the per-type stream, redis_bytype_queue.go).
A diverted cm-16 is in **neither** the fold (not in the stream) **nor** the tail (not in the stream),
and is healed only when the **divert nudge** re-anchors the type — which races the 90 s deadline:

```
  overlap band applied in ONE batch → audit events arrive out of order
        cm-00..15 ingested in order ─▶ in stream  ─▶ folded by B's backfill (≤ coverageHead) ✅
        cm-16 arrives AFTER cm-17 (rv below high-water) ─▶ DIVERTED, never in stream
                 └─▶ not folded, not tailed ─▶ MISSING under B
                 └─▶ divert nudge → RequestResync → re-anchor re-LIST ─▶ heals cm-16  ⏱ races 90s
        cm-17..19 ingested in order ─▶ in stream, id > coverageHead ─▶ forwarded by tail ✅
```

### Interpretation

- **Distinct, pre-existing race — not the first-event-loss bug, not caused by the fix.**
  **Certainty: 90%.** `configmaps` is a common, already-Synced type; the per-event commits work (no-replay
  half passes); the fix touched neither the splice/coverage code nor the divert path. *Residual 10%: the
  fix mirrors a claimed type slightly earlier — could marginally shift join timing; not demonstrated.*

> **Defer to the authoritative deep-dive:**
> [`signing-overlap-band-coverage-drop-investigation.md`](../finished/signing-snapshot-tail-replay-failure-investigation.md)
> is the rigorous (FACT/DERIVED/HYPOTHESIS-tagged) investigation of this exact flake, and it **measured
> the divert counter**. The bullets below are reconciled to it — and it walks back the "divert" lead I
> had given (the diagram above is now just *one — contradicted — candidate*).

- **What is forced (DERIVED): cm-16 was not a normally-ordered main-stream entry within B's reconcile
  cut.** **Certainty: 85%.** The splice's `Desired` (≤ `coverageHead`) and the tail (> `coverageHead`)
  are one consistent cut, so a normally-ordered stream entry cannot be dropped — therefore the object
  was off the main stream (never ingested, diverted, or an attach anomaly). This is §4.1 of the deep-dive.

- **The specific path off the stream is UNCONFIRMED — and divert is *contradicted*, not confirmed.**
  *(This corrects my earlier "divert leading, 55%".)* The deep-dive measured **`lateCount=0` for
  ConfigMaps** at the failure (§4.4), so **H2 (divert) is argued *against*, not for** — the evidence is
  the opposite of what I assumed. Current ranking (its §4.5): **H1 "never a normally-ordered stream
  entry" is least-contradicted (~40%)**; H3 rv-missing-attach anomaly (`rvMissingCount=66`) open (~20%);
  H4 `Hc`-seam **unlikely** because `Hc` is published as the fold's `coverageHead` (contiguous by
  construction) *unless* a stale-high prior `Hc` is held (~15%); **H2 divert contradicted (~15%)**. No
  mechanism is confirmed — a capture is required.

- **On "just widen the timeout":** it depends on §5 of the deep-dive — *is "every object present under a
  target within one reconcile" a guarantee, or only "by the next checkpoint/heal"?* If the latter, the
  90 s assertion is a **test over-assertion** and widening (or budgeting the heal cadence) is legitimate;
  if the former, the join-time path has a real gap to fix. **Unresolved — certainty either way: ~50%.**

- **Correctness: likely a bounded gap, but "self-healing" is NOT established.** *(Down from my earlier
  confident "converges, 70%".)* The deep-dive's §8 says the object was **permanently absent** in that
  run's repo (the heal did not visibly recover it within the window, and `DeferCleanup` removed B). So
  whether it heals at the next re-anchor is **Q2/Q5 — open**, not a settled fact. The no-replay invariant
  did hold.

### What would raise certainty — and the instrumentation added this session
On the next reproduction, these now-added logs disambiguate divert vs boundary in one read:
- **`late audit event nudged a type resync`** (now **info**, [materialization.go](../../internal/watch/materialization.go)) — fires iff a divert happened; if it names `configmaps` around the join, the divert mechanism is confirmed.
- the scoped **`diag_all`** firehose already captures `configmaps`, so a diverted cm-16 shows as `outcome=older_than_high_water`.
- **`target-type watermark published / held`** (target_type_watermark.go) — shows B's actual `Hc` and whether a stale-high prior was held (the secondary hypothesis).
- **`audit-tail routed batch to target`** (V(1), audit_tail.go) — per-batch `routed / suppressedAtOrBelowHc / firstID / lastID`; if cm-16 appears here and is `suppressedAtOrBelowHc`, it is the boundary case; if it never appears, it was diverted upstream.

If the repro shows divert: the durable answer is faster/again-reliable nudge-heal (or accept + widen the
test deadline). If it shows a held stale-high `Hc`: fix the publish to not hold a boundary ahead of the
fold. **No fix is applied yet — the mechanism is not confirmed (trust gate not met).**

---

## Summary

| | Flake A — CommitRequest finalize | Flake B — Signing late-join |
| --- | --- | --- |
| Failing spec | `commit_request_e2e_test.go:133` | `signing_e2e_test.go:507` |
| Symptom | `phase==Committed` not reached in 120 s | one `overlap-b-cm-NN` missing under B (at **90 s**) |
| Observed in | CI `27830248680`; era run-2 | local f3 + run-3 + a fresh split repro (00–15/✗16/17–19) |
| Pre-existing, not the fix | **90%** | **90%** |
| Likely mechanism | attribution-scan miss (§2, root OPEN) — **55%**; ordering a secondary — **30%** | object was **off the main stream** (DERIVED, 85%); *which* path **unconfirmed** — divert **contradicted** by `lateCount=0`; H1 never-ingested least-contradicted (see deep-dive) |
| Correctness | fail-closed, not mis-attributed — **95%** | bounded gap *likely* but self-heal **NOT established** (object was permanently absent in the repro) |

**Bottom line (certainty 80%):** the first-event-loss fix is sound and landed; `E2E (full)` stays
intermittently red on these two *independent, pre-existing* flakes (the same commit `5d85e7d` failed
E2E(full) on attempt 1 and **passed on re-run** — direct proof they are flaky, not deterministic).
**Neither is evidence against the fix.** Two self-corrections this session: (1) the watermark is
published *as* the splice's `coverageHead`, so the boundary is contiguous by construction (the
`Hc := S` boundary-fix I first proposed is largely moot); (2) the divert lead I then gave is itself
**contradicted** by the deep-dive's `lateCount=0` measurement — so the honest state for Flake B is
"the object was off the main stream, **but by which path is unconfirmed**," tracked rigorously in
[`signing-overlap-band-coverage-drop-investigation.md`](../finished/signing-snapshot-tail-replay-failure-investigation.md).
I deliberately **did not apply a fix** (trust gate not met); instead I landed the instrumentation
(nudge→info, watermark publish/held, per-batch tail-route summary) that, with that doc's §6.1
ingestion-side capture, collapses H1–H4 on the next reproduction. Flake A's leading hypothesis stays
the attribution-scan miss (55%), ordering a tracked secondary (30%).
