# Implementation prompt: materialization healing + GitTarget status

Hand this to a fresh agent (or use it yourself). It implements the two agreed designs:

- [materialization-tail-and-live-readiness-review.md](../../finished/materialization-tail-and-live-readiness-review.md)
  (Gaps 1–6, Recs 1–6)
- [../status-design-git-target.md](../status-design-git-target.md) (the two-axis status
  contract: `Ready` vs `Synced`, removing `EventStreamLive` and `status.snapshot`)

---

## Your mission and mindset

You are implementing a correctness fix and a status redesign in a Kubernetes controller that
mirrors live cluster state into git. **Own the outcome — do not cargo-cult these docs.**

- **The docs are the rationale, the code is the truth.** Every file/symbol named below is a
  pointer that may have drifted. Before you touch anything, `grep`/read it and confirm it still
  does what the doc claims. If reality differs, trust the code and say so.
- **Push back.** If a design point is wrong, unnecessary, or already handled when you reach the
  code, stop and flag it with the evidence rather than implementing it blindly. Simplify; don't
  just add. Prove "this is already broken / already fine" with a controlled check, not a hunch.
- **Tests are the deliverable, not an afterthought.** For each fix, write the test that would
  have caught the original bug *first or alongside* — especially the `8f2ad84` regression
  (a re-anchor stealing an open commit window) and the shared-worker cross-steal.
- **Ship in vertical slices.** Each slice below is independently shippable and must end green
  (see Validation). Do not start the next slice until the current one is committed and green.
- **Match the surrounding code.** Comment density, naming, error-handling idiom, table-driven
  tests. Read a neighbouring file before writing a new one.

## Ground rules (from AGENTS.md — non-negotiable)

- Before a slice is "done": `task fmt` → `task generate` → `task manifests` (if API changed) →
  `task vet` → `task lint` → `task test` → `task test-e2e`. All must pass.
- e2e needs Docker (`docker info` first) and the commands run **sequentially, not in parallel**.
  `task test-e2e` has no build dep — run `task prepare-e2e` first to rebuild + reinstall CRDs.
- Keep `>90%` coverage on new code. Commit per slice; end commit messages with the
  `Co-Authored-By: Claude ...` trailer. Branch is `poc/redis-copy`.

---

## Slice A — GitTarget status: serviceability roll-up + `Synced` phase, drop `EventStreamLive` + `status.snapshot`

Self-contained, no behavioural risk to the data path, and it unblocks the e2e gating. Do this
first. Source: status-design doc §1–§6 + review-doc Gap 3/Gap 6/Rec 4.

1. **Fix the roll-up to use serviceability, not `phase == Synced`** (review-doc Gap 6). A type
   is serviceable when it has a usable checkpoint (`checkpointRV != ""`): true for `Synced`,
   `Resyncing`, and `Failing`-with-prior. Today `MaterializationSummaryForGitTarget` /
   `GitTargetMaterializationSummary` (`internal/watch/materialization.go`) count `Resyncing` as
   `Pending` and only `PhaseSynced` as `Synced`, so a periodic re-anchor flaps the signal.
   `typeset.TypeMaterialization` already exposes `CheckpointRV` and `Phase` — use it (consider a
   small `Serviceable()` helper on the leaf so the rule lives in one place).
2. **API** (`api/v1alpha2/gittarget_types.go`): add `status.phase string`; remove
   `status.snapshot` (`GitTargetSnapshotStatus` + `GitTargetSnapshotStats`) — it is vestigial,
   nothing writes it. Then `task generate` + `task manifests`.
3. **Controller** (`internal/controller/gittarget_controller.go`):
   - Remove the `EventStreamLive` condition. **Keep** the ensure-worker + register-stream step
     (the `Declare` path needs the worker), but on its rare failure set `Ready=False` with a new
     reason `WorkerUnavailable`. `Ready = Validated ∧ EncryptionConfigured ∧ worker-wired`.
   - Add a `Synced` condition (name it `Synced` or `Materialized`, **not** `Live`) and derive
     `status.phase ∈ {Pending, Initializing, Synced, Degraded}` purely from conditions +
     the serviceability roll-up (status-design §3.3 has the derivation flowchart).
   - Drop the now-unused `GitTargetReadyReasonInitialSyncInProgress`. Add a `phase` printcolumn.
4. **Tests** (status-design §7): table-driven reconcile tests for each phase, **including**:
   a periodic re-anchor (`Synced→Resyncing→Synced`) does NOT flap `Synced`; a
   claimed-but-not-followable type yields `Ready=True, Synced=False, phase=Degraded`.
   Replace the bespoke e2e helper `waitForGitTargetMaterializationSettled`
   (`test/e2e/e2e_test.go`) with `kubectl wait --for=condition=Synced`; update any
   `EventStreamLive`/`Ready`-gating in `test/e2e/cluster/start-cluster.sh` and `demo_e2e_test.go`.
5. **(Optional, cheap) metrics** (review-doc §6 gaps): a `…_materialization_sync_duration_seconds`
   histogram (`Requested→Synced` latency) and a per-GitTarget `synced_types` gauge; and add
   `WithDescription`/`WithUnit` to instruments in `internal/telemetry/exporter.go`.

## Slice C — Restore periodic healing via a deferred-until-idle heal resync (review-doc Rec 1)

The headline correctness fix. Source: review-doc Gap 1 + Rec 1 + §1.1 (shared worker).

1. **Add a `Heal` kind to the resync request** (grep for `ResyncRequest` — likely
   `internal/git/types.go`). It must NOT force-finalize the open window.
2. **Worker** (`internal/git/branch_worker.go` + `resync_flush.go`): today `handleResyncRequest`
   calls `finalizeOpenWindowWithReason(windowFinalizeReasonResyncBeforeApply)`. For a `Heal`
   item, instead **defer while `openWindow != nil`** — stash one pending heal and apply it at the
   next window-finalize boundary (which recurs on every silence timeout and every identity
   switch, so it never starves). The check is **internal** to the worker loop; do NOT add an
   external `HasOpenWindow()` poll (TOCTOU race against the FIFO). Remember one worker serves N
   GitTargets, so a heal for A must never steal B's open CommitRequest window.
3. **Re-enable the re-anchor re-splice** (`internal/watch/materialization.go`,
   `handleMaterializationEvent`): drop the `!isAuditTailRunning` skip on `TypeSynced` and route
   the re-anchor reconcile through the heal kind, so the periodic sweep and the late-event nudge
   reach git again.
4. **Tests**: the `8f2ad84` regression — a re-anchor while the tail is running heals drift
   (orphan deleted / late event folded) **and** leaves an open CommitRequest window intact;
   a heal scoped to GitTarget A does not steal GitTarget B's window on a shared worker; a heal
   waits while a window is open and applies once it finalizes.

## Slice B — Make the tail-waits-for-backfill boundary explicit (review-doc Rec 2)

Hardening; largely subsumed by Slice C but cheap and removes fragility. Gate the per-type tail
behind a successful first backfill for every current watcher, and retry a failed Declare-time
backfill instead of recording the type as "declared" in `newlyDeclaredSyncedGVRs`. Files:
`internal/watch/materialization.go`, `audit_tail.go`. Test: a failed first backfill is retried
and the tail does not deliver to an un-backfilled target. (Optional Rec 3: have the backfill
return its fold tip `H` and anchor the tail at `H` to drop the `(R, head]` double-commit —
treat as a follow-up; `R`-anchoring is the safe default.)

## Slice D — WATCH-first checkpoint with LIST fallback (review-doc Rec 6)

Independent; do it whenever LIST cost matters. `mirrorTypeObjects`
(`internal/watch/type_objects_mirror.go`) does an unconditional `List` today; there is no
`sendInitialEvents` watch anywhere (the string survives only in a stale comment in
`internal/typeset/materializer.go`). Open a watch-list (`sendInitialEvents=true`,
`resourceVersionMatch=NotOlderThan`, `allowWatchBookmarks=true`), fold the initial `ADDED`
events, pin the `initial-events-end` bookmark rv as the checkpoint, and fall back to the
consistent LIST only on a `sendInitialEvents` rejection (aggregated apiservers). Fix the stale
comment. Test both paths.

---

## Suggested order and definition of done

Order: **A → C → B → D** (A unblocks tests; C is the correctness headline; B/D harden). Each
slice is done when: code matches surrounding style; the named regression test exists and fails
without the fix; `task lint`, `task test`, and `task test-e2e` are green; and the relevant
design doc's checklist items are ticked. If you discover the design is wrong somewhere, update
the design doc in the same slice and explain why.
