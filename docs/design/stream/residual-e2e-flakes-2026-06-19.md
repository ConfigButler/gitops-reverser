# Residual e2e flakes after the first-event-loss fix (2026-06-19)

Status: FACTS + diagnosis. After the capture-before-baseline fix
([first-event-loss-on-reclaim-plan.md](first-event-loss-on-reclaim-plan.md), committed in `5d85e7d`),
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
  resulting SHA"** ([commit_request_e2e_test.go:133](../../../test/e2e/commit_request_e2e_test.go#L133)).
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
  [e2e-flakes-2026-06-18-investigation.md §2](e2e-flakes-2026-06-18-investigation.md).** **Certainty:
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
[repo_assertions_test.go](../../../test/e2e/repo_assertions_test.go)) to the phase/sha/message/count
assertions ([commit_request_e2e_test.go:123](../../../test/e2e/commit_request_e2e_test.go#L123)): on a
failure it prints the latest 5 `origin/main` commits (sha, time, author, subject), path-scoped — so the
next reproduction makes "wrong / older / later commit, or a gap" obvious at a glance. Beyond that: the
one-line `scanForCommitRequestCreate` diagnostic from §2.3 (entries-scanned + whether a create-for-name
was seen on a miss) to separate attribution-miss from sequencing; or read the controller log on the next
reproduction. The durable fix candidate (deferred) is the early-ingestion author store
([internal-audit-type-demand.md](internal-audit-type-demand.md)), which removes the scan entirely.

---

## Flake B — Commit Signing late-joining target: overlap ConfigMap missing under path B

### Facts (observed)

- Spec: **"Commit Signing — should not replay already-reconciled configmaps as per-event commits to a
  late-joining target"** ([signing_e2e_test.go:507](../../../test/e2e/signing_e2e_test.go#L507)).
- The convergence assertion: every `overlap-b-cm-NN` ConfigMap must be **present under path B**. It
  runs under the **90 s** suite default (`SetDefaultEventuallyTimeout(90s)`,
  [signing_e2e_test.go:82](../../../test/e2e/signing_e2e_test.go#L82)); it does **not** override it.
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

### What the spec does

B, joining late on a type A already tails, gets the object set under its own path from **two** sources
that must meet exactly at the join: its **initial backfill** (a snapshot of the type at revision `S`,
written as a reconcile commit) and the **coverage-watermarked tail** (live events with revision `> Hc`,
the per-(GitTarget,GVR) watermark). For no gap and no double-count, the backfill snapshot `S` and the
watermark `Hc` must be **contiguous**. The observed split is the signature of them **not** being:

```
   overlap band created at revisions:   …00 01 … 15 │ 16 │ 17 18 19…
                                         └──────────┘   ▲   └────────┘
   B's backfill snapshot (≤ S) ─────────▶ 00 … 15       │   (S = rv of cm-15)
   B's tail (rev > Hc) ─────────────────▶            17 … 19   (Hc = rv of cm-16)
                                                      ▲
                              cm-16 ∈ (S, Hc]  →  in NEITHER source  →  MISSING under B
```

When `Hc` is set **ahead of** `S` (the watermark covers more than the backfill captured), any object
created in the window `(S, Hc]` is **backfilled-too-late and tail-suppressed-too-early** — covered by
neither. That is an **ordering / coverage-watermark boundary gap**, not "the pipeline was slow": at
90 s the object is still absent, and it stays absent under B until the next periodic re-anchor re-LISTs
the type (~1 h). This is the per-(GitTarget,GVR) coverage-watermark machinery the spec's own comment
points at ([signing-snapshot-tail-replay-failure-investigation.md]).

### Interpretation

- **This is a distinct, pre-existing race — not the first-event-loss bug and not caused by the fix.**
  **Certainty: 90%.** `configmaps` is a common, already-Synced type (never the freshly-(re)claimed case
  the fix addresses); the failing path is the late-join backfill/coverage machinery; the per-event
  ConfigMap commits themselves work (the no-replay half passes); the fix did not touch the
  snapshot/coverage code. *Residual 10%: the fix makes a claimed type mirror slightly earlier, which
  could marginally shift the join timing — not demonstrated, not excluded.*

- **It is a coverage-watermark / backfill-boundary GAP (an ordering bug), not mere deadline latency.**
  **Certainty: 65%** *(up from a wrong "latency, 80%" in the first draft).* The split pattern
  (`00–15` backfilled, `16` missing, `17–19` live) is the textbook signature of `Hc` set ahead of the
  backfill snapshot `S`, leaving `(S, Hc]` uncovered — and it reproduced at **90 s**, which rules out
  "just slow." *Why only 65%: I have not yet instrumented the actual `S` and `Hc` values for the missing
  object, so the exact boundary mechanism (off-by-one in watermark publish vs backfill pin) is inferred
  from the commit-split shape, not measured. The agent has added the live "last 5 commits" diagnostic
  (below) to capture more of this on the next reproduction.*

- **It is NOT simple deadline pressure.** **Certainty: 70%** *(this is a deliberate down-weighting of
  the earlier claim).* 90 s is ample and the object was still absent; widening the timeout would only
  hide a real gap. *Residual 30%: a single split observation; conceivably a different per-run timing
  produced it and pure latency still contributes on other runs.*

- **Correctness impact: a bounded, self-healing completeness gap — not permanent corruption.**
  **Certainty: 65%** *(down from "converges, 80%").* A gap object is missing under B until the next
  periodic re-anchor re-LISTs the type (~1 h), so within the test window (and for up to a sweep
  interval) B's git is genuinely **incomplete**, not merely late. The re-run passing shows it heals /
  does not always occur, but "converges within the live window" is *not* established — only "eventually,
  at re-anchor." The no-replay invariant did hold (no over-replay).

### What would raise certainty
Capture `S` (backfill snapshot rv) and `Hc` (published coverage watermark) for the missing object on one
live reproduction — the §"Where to add logs" instrumentation the parallel agent proposed
(`EmitTypeReconcileForGitDest` / `SpliceSnapshotForType` / `publishTargetTypeWatermark` /
`routeAuditChangesToTarget`). If `Hc > S` at the gap, the boundary-gap hypothesis is confirmed and the
fix is to pin `Hc := S` (contiguous), **not** to widen the timeout.

---

## Summary

| | Flake A — CommitRequest finalize | Flake B — Signing late-join |
| --- | --- | --- |
| Failing spec | `commit_request_e2e_test.go:133` | `signing_e2e_test.go:507` |
| Symptom | `phase==Committed` not reached in 120 s | one `overlap-b-cm-NN` missing under B (at **90 s**) |
| Observed in | CI `27830248680`; era run-2 | local f3 + run-3 + a fresh split repro (00–15/✗16/17–19) |
| Pre-existing, not the fix | **90%** | **90%** |
| Likely mechanism | attribution-scan miss (§2, root OPEN) — **55%**; ordering a secondary — **30%** | coverage-watermark / backfill **boundary gap** (ordering) — **65%**; *not* mere latency — **70%** |
| Correctness | fail-closed, not mis-attributed — **95%** | bounded gap, heals only at re-anchor (~1 h) — **65%** |

**Bottom line (certainty 80%):** the first-event-loss fix is sound and landed; `E2E (full)` will keep
intermittently red until these two *independent, pre-existing* flakes are addressed. **Neither is
evidence against the fix.** But the earlier "just widen the timeout for B" suggestion was wrong: B is now
best explained as an **ordering / coverage-boundary gap** (an object in `(S, Hc]` covered by neither the
backfill nor the watermarked tail), which reproduced at the real 90 s timeout — so it is a real
completeness gap to *fix at the boundary* (`Hc := S`), not to hide. **Instrument one reproduction
first** (the "last 5 commits" diagnostic is now in; add the `S`/`Hc` logs next) before choosing a durable
fix. Flake A's leading hypothesis stays the attribution-scan miss (55%), with ordering a tracked
secondary (30%). A CI re-run still usually goes green — useful as a stabiliser, not as the diagnosis.
