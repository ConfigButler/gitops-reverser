# First-event loss on a freshly (re)claimed type — root cause & fix plan

Status: PROPOSAL (2026-06-19). Companion to the evidence in
[e2e-flakes-2026-06-18-investigation.md §10](e2e-flakes-2026-06-18-investigation.md). That doc proves
*what* happened from a live run; this doc explains *why* in code, draws the current shape and the
candidate fixes, and proposes a concrete, test-first plan.

The invariant we are defending — the user's words — is **"events can't get lost."** Demand-gated
ingestion is allowed to *gate out* an event for a type nobody wants, but a type a GitTarget **claims**
must have its events captured. Today there is a window where a claimed type's first events are dropped
*and never healed*. That is the bug.

---

## 1. What breaks, in one paragraph

A GitTarget claims a type by way of its WatchRule. The audit webhook only mirrors a type that the
**demand gate** marks `Required`. The gate is told to `Require` a type **only after** the
materialization checkpoint sync is *requested* (`SyncRequested`), which itself only fires when the
GitTarget's Declare actually **claims the type** *and* the type is `Followable`. If, at the one Declare
that mattered, the type is **not in the claimed set** — for any of the reasons in §1.1 — then nothing is
`Require`d, no checkpoint sync runs, the first `create` is gated `not_needed` and dropped before it ever
reaches the per-type stream, and **nothing heals it** until a much later reconcile or the ~1 h sweep.
The commit never happens; an e2e spec with a 120 s deadline fails.

This was confirmed end-to-end on run 3 of the scoped-`diag_all` experiment: `crd-lifecycle…icecreamorders`
was never `Required`/`Synced` (durable `:objects:state` frozen at the *prior run's* `phase=removed`,
absent from `__index__`, zero materialization activity), the `alices-order` create was absent from the
firehose (`not_needed`), and there was no commit. Runs 1–2 passed for the opposite reason: the create
*was* ingested live and committed.

## 1.1 Status of the diagnosis — confirmed *where*, likely *why*

Be honest about what the run proves and what it doesn't:

**Confirmed (hard evidence).** The event was lost **at the demand gate** (`not_needed`, absent from the
firehose), and the type was **never `Required` and never checkpoint-synced** in run 3 (durable state
frozen at the prior run, not in `__index__`, zero materialization activity). That much is not a
hypothesis.

**Not yet uniquely proven.** *Why* the claim/Require never happened. The run-3 timeline even cuts against
the simplest story: `TypeActivated`(followable) at 07:50:31 came **before** the GitTarget at 07:50:35
and the object create at 07:50:35.814 — so the type *looked* followable when the GitTarget Declared. The
missing fact is **what GVR set `DeclareForGitTarget` actually produced** (and whether it ran after the
activation). We do not have it, because that path logs nothing today. The candidate triggers, all
consistent with the evidence:

| | Candidate trigger | Note |
|---|---|---|
| **W1** ✅ **CONFIRMED in the wild** | Resolve **failed closed** on a watched type held `retained` (within the removal grace, a wobble) → declared **nothing**. | `resolveSnapshotGVRs` aborts the whole set on any `retained` type. The S0 diagnostic caught this live (see below). |
| **W2** | Resolve returned **empty-observable** (type briefly out of `Followable()` between 07:50:31 and the Declare) → claim withdrawn, **long** requeue. | An empty observable resolve is authoritative today. |
| **W3** | Declared the **wrong same-Kind GVR** (the 07:51:31 heal on this target was scoped to `wildcard-watchrule`, not `crd-lifecycle`). | Same-Kind/multi-group resolution confusion. |
| **W4** | `DeclareForGitTarget` did not run again after activation within the window (controller timing). | Long requeue + no settle signal. |

**Proof run (2026-06-19, cold + 3 warm, S0 diagnostic on the unfixed tree).** All four runs passed — the
manager-crd spec did **not** re-fail this round (the flake is intermittent, ~1-in-3 to 1-in-6, and the
wobble cleared before the spec's deadline here). **But the S0 diagnostic confirmed W1 happens for real:**
run p3 logged `materialization declare resolved nothing (surface not observable / wobbling)` six times for
a target, with `err: … watched type restart-reconcile.e2e.example.com/v1, Kind=IceCreamOrder within the
removal grace (currently unserved); refusing to sweep a reduced cluster view`. So a `retained` (wobbling)
watched type does abort the whole Declare and claim nothing — the loss mechanism is no longer
hypothetical. (The captured instance was a *wildcard* target, for which only the broader §6.1 work helps;
the originally-failing manager-crd spec uses a *fully-specified* rule, which §6.1 fixes directly.)

**Why the fix is still the right one.** Option B (§5) claims the rule's **fully-specified** GVR from the
**rule spec**, unconditionally, and opens the gate on the claim — which closes **W1, W2, W3, and W4
alike** (the claim no longer depends on discovery state, resolve success, or same-Kind disambiguation).
So the fix is robust across every candidate trigger. But per the reviewer, we will still **prove the
exact why** with a one-line diagnostic + a red test before/while landing it (slice **S0**, §8) so the
plan rests on a proven cause, not a plausible one.

---

## 2. The pipeline today, and where the event dies

```
                          ┌───────────────────────────── control plane ─────────────────────────────┐
  WatchRule/GitTarget ─▶  GitTarget reconcile
   (declares group+ver        │
    +resource)                │ DeclareForGitTarget(gitDest)
                              ▼
                       resolveSnapshotGVRs ──────────────┐
                              │                           │  builds the table from
                              │                           │  registry.Followable() ONLY
                              ▼                           ▼
                       matchFollowableRecords      ❶ type not Followable *right now*
                              │                        → table EMPTY for this type
                              ▼                        → Declare([]) WITHDRAWS the claim
                    Materializer.Declare(ref, gvrs)     → controller requeues LONG
                              │
              followable ∧ claimed ∧ Dormant?  ──no──▶  stays Dormant, NO SyncRequested  ❷
                              │yes
                              ▼
                        SyncRequested ──▶ driver ──▶ gate.Require(gvr)  ❸ (only here!)
                              │                           │
                              ▼                           ▼
                        LIST (mirrorTypeObjects)    :__required__ set
                              │
                              ▼
                        SyncSucceeded ─▶ checkpoint :objects:state, tail starts


  ┌───────────────────────────── data plane (audit webhook) ────────────────────────────┐
  kube-apiserver audit ─▶ webhook ─▶ mirrorByType ─▶ gate.Allow(gvr)? ──no──▶ ✗ not_needed
                                                          │                     (DROPPED — no stream,
                                                          │yes                   no diag, no heal)
                                                          ▼
                                                   per-type :audit:stream ─▶ tail ─▶ commit
```

**The kill point is the `gate.Allow` check (❸ wired only at ❶/❷).** For a claimed type, `Allow` is
true only once `Require` has run — and `Require` is gated behind a chain (`Followable` at Declare time →
claim kept → `SyncRequested` → driver → `Require`) that has two failure modes:

- **❶ / ❷ the claim never forms / never reaches `Requested`** because the type was not `Followable` at
  the one Declare that mattered, so the gate is never told to want it; *and* no LIST runs, so no
  checkpoint heals the dropped event.
- **a narrow async gap** even on the happy path: `SyncRequested` → buffer → driver → `Require` is a few
  hops; an event in that gap is gated out, but there the checkpoint LIST *does* run and folds it, so it
  is harmless. (Not the bug — noted for completeness.)

### Why "warm" matters (run 3 vs runs 1–2)

`crd-lifecycle…icecreamorders` is one of several **same-Kind, different-group** IceCreamOrder CRDs the
suite installs and tears down across specs. On a warm cluster the type had just been removed by an
earlier spec (durable `phase=removed` — note this is only *evidence the sync never re-ran*, not itself a
blocker: a re-claim is supposed to drive a fresh sync regardless) and was being re-installed under load.
Whatever the precise trigger (§1.1, W1–W4), the claim/Require chain did not engage within the window.
Cold (run 1) starts clean; warm runs where it engages in time (run 2) pass; the warm run where it does
not (run 3) loses the first event. It is a **race in the demand handshake under warm/load**, which is
exactly why it is intermittent.

---

## 3. The three design gaps (what the fix must close)

| # | Gap | Consequence |
|---|-----|-------------|
| **G1** | `DeclareForGitTarget` declares only the *currently-`Followable`* subset and treats an empty observable resolve as authoritative → **withdraws** the claim. | A transient followability gap **drops the claim** for a type the rule fully specifies. The Materializer's DEC-L9 design ("claim a not-yet-followable type, converge when it activates") is **never exercised** — the watch layer pre-filters it away. |
| **G2** | The gate is `Require`d only at `SyncRequested`, i.e. *after* a claim has reached `Requested`. | A **claimed** type that is not yet `Requested` (because of G1, or just the async gap) has its live events gated `not_needed` and **lost**. |
| **G3** | The only backstop for "claimed + followable but never checkpointed" is the next GitTarget reconcile (long) or the ~1 h sweep. | A stall is unbounded in time — far past any test deadline, and in principle a real mirror could sit empty for an hour. |

The Materializer leaf itself is **correct and elegant** (it already accepts claims for not-yet-followable
types and converges via `maybeRequestLocked` from *both* `Declare` and `OnLifecycleEvent`). The bug is
entirely in the **watch-layer wiring around it** (G1/G2) plus the absence of a bounded safety net (G3).
The fix should make the system behave the way the leaf already promises, not add a new mechanism.

---

## 4. The invariant to restore

> **A type that a live GitTarget claims is `Require`d in the gate for as long as the claim stands —
> independent of whether its checkpoint has synced yet — so its audit events are captured from the
> first one. The checkpoint sync establishes the baseline and folds the captured events; it is never a
> precondition for *capturing* them.**

Capture (gate) is cheap and lossless to start early; baseline (checkpoint) can follow. Today we have it
backwards: we gate-open only after we have a baseline.

---

## 5. Solution options

### Option A — Require-on-claim only (gate opens when claimed)

Move/extend the gate `Require` so a type is `Required` the moment it is **claimed** (≥1 claimant),
regardless of followability or sync phase; `Unrequire` when the last claim is withdrawn.

```
   Declare(ref, gvrs) ──▶ for each gvr: gate.Require(gvr)   ← NEW: at claim, not at SyncRequested
                          (Materializer still drives the LIST/checkpoint as before)
```

- ✅ Closes **G2** directly: a claimed type's events flow into the stream from the first one.
- ✅ Small, local, obviously correct; trusts the gate abstraction.
- ❌ Does **not** fix **G1**: if the *claim itself* is never made (empty resolve withdrew it), there is
  nothing to Require. On its own it would not have saved run 3.

### Option B — Claim the intended GVRs unconditionally + Require-on-claim  ★ recommended

Two changes, each honoring an existing abstraction:

1. **Declare the rule's *fully-specified* GVRs unconditionally** (group + version + resource all named →
   the GVR is known without discovery), independent of current followability. The claim becomes a
   function of the **rule spec**, which is stable, instead of of **transient discovery**, which wobbles.
   The Materializer then holds the claim and **converges to a sync when the type becomes `Followable`**
   — precisely DEC-L9, which today never runs. Wildcard / version-less rule entries still resolve through
   discovery (they are inherently discovery-driven), so this is a *superset*, never a withdrawal.
2. **Require-on-claim** (Option A), so the held claim opens the gate immediately.

```
   rule {group:G, version:v1, resource:R}  ─▶  claim GVR{G,v1,R}  (unconditional, stable)
                                                   │
                                                   ├─▶ gate.Require(GVR)         ← events captured now
                                                   │
                          TypeActivated(GVR) ──────┴─▶ maybeRequest → SyncRequested → LIST → checkpoint
                          (whenever discovery settles)            (folds the captured events)
```

- ✅ Closes **G1 and G2** together; would have saved run 3.
- ✅ Makes the Materializer's DEC-L9 convergence real instead of dead code.
- ✅ No new mechanism — uses `Declare` (claim-without-followability is already allowed) and the gate.
- ⚠️ Claim set for fully-specified rules no longer shrinks on a wobble (by design — that is the fix);
  the sweep still ages out genuinely-removed rules because the *rule* stops declaring them.
- ⚠️ Subtlety: `Unrequire` must track **claim withdrawal** (sweep GC of the last claimant), **not** the
  `Released` event — `Released` also fires on followability loss while the claim survives (force-release),
  and we must keep mirroring a still-claimed type through a wobble. Handled in §6.

### Option C — Bounded heal ticker (defense in depth)

A fast periodic pass (seconds, not ~1 h) that force-requests a sync for any **claimed ∧ followable ∧
no-checkpoint** type. Catches *any* stall regardless of cause.

```
   every N s:  for gvr in claimed ∧ followable ∧ checkpointRV=="":  RequestFirstSync(gvr)
```

- ✅ A blunt safety net that closes **G3** for all causes (including ones we haven't foreseen).
- ❌ A band-aid if used alone (it papers over G1/G2 rather than fixing them); best as a *backstop* under
  B, not a substitute.

### Recommendation

**Option B as the fix; land it first.** Add Option C as a backstop **only after** B's core invariant is
in and a test can demonstrate a real stalled `claimed ∧ followable ∧ no-checkpoint` state — and keep C
**scoped and observable** (a metric/log on every forced sync) so it never quietly becomes a second
reconciliation engine masking a regression in B. B removes the actual fragility and activates an
abstraction we already paid for; C only bounds the worst case for a residual or future stall. Reject the
early-ingestion attribution store (§9.2 of the facts doc) — it is a workaround for one symptom and leaves
the pipeline lossy.

---

## 6. Detailed design (Option B + C backstop)

### 6.1 Claim from the rule spec, not from discovery (G1)

In the watch layer's Declare path, split the claimed set into two sources:

- **Specified GVRs** — rule entries naming group **and** version **and** a concrete resource (not `*`).
  Their GVR is fully determined by the rule; emit them as claims **without** consulting followability.
- **Discovered GVRs** — wildcard/version-less entries, resolved through `Followable()` as today.

Union the two; `Declare` the union. An *observable* resolve that yields a non-empty **specified** set is
never treated as an empty withdrawal. Keep the existing fail-closed for the *discovered* part (a wobble
there must still not withdraw discovered claims) — but a specified claim no longer depends on it.

> Trusting the abstraction: `Materializer.Declare` already documents "a claim is recorded regardless of
> followability … and drives a sync the moment it becomes followable." We simply stop hiding
> not-yet-followable specified types from it.

### 6.2 Require-on-claim, Unrequire-on-withdrawal (G2)

Make the gate's `Required` set track **claimed-ness** precisely:

- On `Declare`, `Require` every GVR in the freshly-claimed set (idempotent; a no-op for already-Required).
- On **claim withdrawal** — the last claimant of a GVR ages out in `Sweep`'s lease GC — `Unrequire`.
  This is distinct from the `Released` checkpoint-drop: a force-release from a followability wobble keeps
  the claim, so it must **keep** the type `Required` (we still want to capture events through the wobble).

**The two edges are asymmetric, and that asymmetry decides the design.** Opening the gate
(`Require`) is **loss-critical** — an event before it is `not_needed` and dropped — so it must be
**prompt**. Closing it (`Unrequire`) is **harmless if late** — over-capturing a no-longer-wanted type
just writes a few entries that trim away (the gate's own §3/§6 posture) — so it can be eventually
consistent. So we make `Require` synchronous and `Unrequire` event-driven, rather than routing both
through the async work buffer (the reviewer's caveat: a `Claimed → Require` over the buffer leaves a
small "claimed but gate not open yet" window).

**Final shape (S2):**

- **`Require`: synchronous in `DeclareForGitTarget`, for *every* claimed GVR, every reconcile.** It runs
  on the control-plane reconcile goroutine (not the audit hot path); `gate.Require` is an idempotent
  `SADD` that only pings on a real change, so re-asserting the full claimed set each reconcile is cheap
  **and self-healing** — a transient gate-write failure is simply retried next reconcile. This opens the
  gate the instant a claim exists, with **no async window**, and needs no `Claimed` event at all.
- **`Unrequire`: a new `Unclaimed` materialization event** emitted by `Sweep`'s lease GC when a GVR's
  **last** claimant ages out (≥1→0). The driver maps `Unclaimed → Unrequire`. This is the *only* claim
  edge that needs an event, because it originates in the sweep (no `Declare` to hang it off).
- **Remove `Unrequire` from the `Released` handler.** `Released` fires on a checkpoint drop, which
  includes a followability **force-release where the claim survives** (a wobble) — we must keep mirroring
  a still-claimed type through a wobble. `Released` keeps only its checkpoint/audit-key cleanup
  (`clearTypeObjects` + `deleteTypeAuditKeys` + `stopTypeAuditTail`); the gate flag now moves strictly
  with the **claim**, not the checkpoint.
- **Remove `Require` from the `SyncRequested` handler** — it is now owned by the synchronous Declare path
  (and `DeclareForGitTarget` is the sole production caller of `Materializer.Declare`).

This is simpler than emitting both `Claimed` and `Unclaimed` (no redundant `Claimed→Require` next to the
synchronous one) and keeps the gate honest: **Required ⟺ claimed**. A test asserts the gate is `Required`
**by the time `DeclareForGitTarget` returns**, not "eventually."

### 6.3 Bounded first-sync backstop (G3)

Add a fast backstop (seconds-scale, injectable like `materializationSweepIntervalOverride`) that calls a
new `Materializer.RequestFirstSyncIfStalled(gvr)` for every **claimed ∧ followable ∧ `checkpointRV==""` ∧
phase∈{Dormant,Requested}** type — i.e. a claimed type that *should* have a checkpoint but doesn't yet.
It re-emits `SyncRequested` (idempotent; a no-op once Syncing/Synced). This is the only timing knob; keep
it boring and well-tested.

### 6.4 What we do NOT change

- The Materializer phase machine (`materializer.go`) — correct as is; B/C only *call* it more honestly.
- The fail-closed discipline for **discovered** (wildcard/version-less) claims and for the snapshot
  *sweep* scope (deleting KRM from git on a reduced view must still fail closed — that is a different,
  correct guard).
- The checkpoint/tail/splice machinery downstream of `SyncSucceeded`.

---

## 7. Red-first tests (reproduce the fights we kept losing)

Each test is written to **fail on `main`** (today's code) and pass after the fix. They target the exact
shapes that have bitten us repeatedly: first-event-of-a-fresh-type, re-claim-from-removed, and the
gate-out window.

### 7.1 typeset leaf — `internal/typeset/materializer_test.go`

- **`TestDeclare_ClaimSurvivesNotYetFollowable_ThenConvergesOnActivation`**
  Declare a GVR while it is **not** followable → assert a claim exists and phase stays `Dormant` (no
  event). Then `OnLifecycleEvent(TypeActivated)` → assert exactly one `SyncRequested` and phase
  `Requested`. *(Guards DEC-L9; passes today at the leaf — it proves the leaf is not the bug and pins the
  contract the watch layer must honor.)*
- **`TestReclaimAfterRelease_ReRequestsSync`**
  Drive Dormant→Requested→Syncing→Synced→(sweep release, claim withdrawn)→Dormant, then re-`Declare` the
  same GVR while followable → assert a fresh `SyncRequested`. *(The "removed→re-claim" transition.)*
- **`TestClaimEdges_EmitClaimedAndUnclaimed`** *(if (b1) chosen)*
  Declare a new GVR → expect a `Claimed` event; let its last claimant age out in `Sweep` → expect
  `Unclaimed`; a force-release via `TypeRemoved` with a surviving claim → **no** `Unclaimed`.

### 7.2 watch wiring — `internal/watch/materialization_test.go` (new)

- **`TestDeclareForGitTarget_SpecifiedGVRClaimedWhenNotYetFollowable`** ★ the run-3 reproducer
  Registry where the rule's fully-specified type is **not** in `Followable()`. Call `DeclareForGitTarget`
  → assert the GVR **is** claimed (Materializer.Claimants non-empty) and the gate saw `Require(gvr)`.
  *On `main` this fails:* the empty resolve withdraws the claim and never Requires. After the fix it
  passes.
- **`TestDeclareForGitTarget_RequiresClaimedTypeBeforeSync`**
  Assert `gate.Require` is called for a claimed type **before/without** any `SyncSucceeded`. *Fails on
  main* (Require is wired only at `SyncRequested`).
- **`TestFirstSyncBackstop_RequestsCheckpointForStalledClaim`**
  A claimed+followable type left at `Dormant`/`Requested` with no checkpoint → one backstop tick →
  assert a `SyncRequested` / a LIST is driven. *(Guards G3.)*

### 7.3 gate — `internal/gate/gate_test.go`

- **`TestAllow_TrueForClaimedNotYetSyncedType`**
  After Require-on-claim, `Allow(gvr)` is true for a claimed type whose checkpoint has not synced. *Fails
  on main* (Allow is false until SyncRequested-time Require).

### 7.4 e2e — `test/e2e/crd_lifecycle_e2e_test.go`

- **`should commit a CR created immediately after its WatchRule on a freshly (re)installed CRD`**
  Add a variant of the existing IceCreamOrder spec that (a) **re-installs** the CRD right
  before the rule (mimicking warm `phase=removed`), and (b) creates the CR **within ~100 ms** of the
  WatchRule — then asserts the commit lands within a *short* window. This is the spec that flaked; after
  the fix it should be deterministic. Pair it with the existing teardown assertion
  `audit_events_total{outcome="not_needed", resource="icecreamorders"}` not swallowing a claimed type's
  first create. *(Keep it `Serial`/labeled like its sibling.)*

> Note on the *second* run-3 failure (signing late-join, `overlap-b-cm-15` missing within 30 s): it is a
> **different** race (snapshot/coverage completeness for a late-joining target on a common type, not the
> `removed`-phase claim stall). It is out of scope here and tracked separately in the facts doc §10.4.

---

## 8. Slices (land incrementally, each green before the next)

0. **S0 — prove the *why* (reviewer's ask).** Add a single structured log in `DeclareForGitTarget` that
   records the resolved/claimed GVR set for each GitTarget (count + names) at V(0)/info, and the §7.2
   red unit test `…SpecifiedGVRClaimedWhenNotYetFollowable` (which fails on `main` by asserting the claim
   set is empty/wrong for a fully-specified rule whose type isn't in `Followable()`). Then re-run the
   warm 3× experiment (scoped `diag_all` still on): the log pins which of W1–W4 actually fired in a real
   failure, turning the diagnosis from *highly plausible* into *proven*. The log is cheap, independently
   useful operationally, and stays in. *(This is a diagnosis/observability slice — it ships before the
   behavioural fix so the fix lands on a proven cause.)*
1. **S1 — leaf contract tests (red→green where applicable).** Add §7.1 tests. They mostly pass today
   (the leaf is correct) and lock the contract the watch layer must honor. *No production change.*
2. **S2 — Require-on-claim (Option A / G2).** Implement §6.2 — claim-edge `Claimed`/`Unclaimed` events
   for the symmetric seam, but the **`Require` edge synchronous inside `DeclareForGitTarget`** (close the
   window outright) and **`Unrequire` on last-claim withdrawal** (sweep GC), *not* on `Released`. Tests:
   §7.2 `RequiresClaimedTypeBeforeSync` (asserts Required *by the time Declare returns*), §7.3. Smallest
   change with immediate value: a claimed type's events stop being dropped.
3. **S3 — claim the specified GVRs unconditionally (G1).** Implement §6.1 + the §7.2 run-3 reproducer.
   This is the heart of the fix.
4. **S4 — bounded first-sync backstop (G3).** Implement §6.3 + its test. Defense in depth.
5. **S5 — e2e (§7.4) + full gate.** `task lint/test/test-e2e`; re-run the warm 3× experiment with scoped
   `diag_all` still on to confirm the firehose now shows the first create `queued` (not `not_needed`),
   then turn the scoped firehose back off.

Each slice is independently revertable and independently valuable; **S0 proves the cause**, S2+S3
together close the bug, S1/S4 harden it, S5 proves it on the cluster that produced the failure.

---

## 9. Decisions and remaining open questions

**Decided (this review).**
- **Require/Unrequire seam (§6.2):** `Require` is **synchronous in `DeclareForGitTarget` for every
  claimed GVR each reconcile** (idempotent, self-healing, no async window — so **no `Claimed` event**);
  `Unrequire` is driven by a **new `Unclaimed` event** from the sweep's last-claim GC, **never** from
  `Released` (a wobble force-release keeps the claim). This refines the reviewer's b1 down to its
  loss-critical core: prompt synchronous open, eventually-consistent close.
- **Sequencing:** land **B first** (S0→S3), add **C** only after, scoped + observable.
- **Diagnosis:** prove the *why* (S0) before the behavioural fix lands — log the declared claim set + the
  red test, re-run the warm experiment.

**Still open.**
- **Specified-GVR claim scope (§6.1):** start with **group+version+resource-named** entries only
  (simplest, covers the e2e), or also handle **group+resource** (version-less → collapse to the preferred
  served version) at the cost of a `Followable()`/catalog round-trip? Recommend starting narrow and
  widening only if a real rule needs it.
- **Backstop interval (§6.3):** ~10 s prod, sub-second test override. It only ever *re-requests*
  idempotently, so it is cheap — but confirm the metric/log shape so it stays observable.
- Should `Unrequire`-on-withdrawal also delete the per-type audit keys immediately, or keep the existing
  grace-protected `Released` deletion? (Lean: keep deletion on `Released`; only the gate flag moves with
  the claim.)
