# Signing overlap-band coverage drop: a single object never reaches a late-joining target

Status: **open investigation.** This records exactly what was observed, what it proves, and what is
still an unproven assumption. It is deliberately conservative: every claim is tagged **FACT**
(directly observed), **DERIVED** (forced by a FACT plus code we can read), or **HYPOTHESIS** (a
candidate mechanism not yet confirmed). The goal is a precise, falsifiable starting point — not a
fix.

It is the sequel to
[`signing-snapshot-tail-replay-failure-investigation.md`](signing-snapshot-tail-replay-failure-investigation.md)
(the per-target coverage watermark `Hc`, §5–§8). That doc closed the *replay* direction (historical
entries wrongly re-committed). This one is the *opposite* failure: an object that should be present
under a late-joining target is **silently absent**, and §8.1's claim that content-convergence is
"deterministic regardless of which path delivers it" did not hold.

---

## 1. Executive summary

A full `task test-e2e` run failed on one spec: `Commit Signing > should not replay already-reconciled
configmaps as per-event commits to a late-joining target`
([`test/e2e/signing_e2e_test.go:507`](../../../test/e2e/signing_e2e_test.go#L507)). The convergence
assertion at [line 609](../../../test/e2e/signing_e2e_test.go#L609) timed out: under target **B**,
`overlap-b-cm-16` was missing. All 40 `seed` ConfigMaps and the other **19 of 20** `overlap-b`
ConfigMaps were present.

The object was **dropped, not delayed**: it is still absent in the Gitea repo for that run long after
the test gave up. The failure is **not** caused by re-running the suite ("second run") — per-run
state is clean, the spec passes in isolation, later full-suite runs against the same reused cluster
passed the overlap spec again, and a full-suite run after `task clean-cluster` also passed. Later
warm-cluster failures were in unrelated specs. It is an **intermittent coverage gap** exposed under
the concurrency of the full suite.

What is *not* yet known: the exact path by which a single CREATE'd ConfigMap reached neither target
B's reconcile fold nor its live tail. The most intuitive cause (late-lane diversion) is **weakened**
by a counter measurement (§4.4) and must not be assumed.

---

## 2. Verified facts (with evidence)

All from the failing run on **2026-06-15**, Ginkgo report written **14:06:10**, against the reused
k3d cluster `k3d-gitops-reverser-test-e2e`.

**F1 — One spec failed, and the assertion was content-convergence.**
The JSON report (`.stamps/cluster/k3d-gitops-reverser-test-e2e/gitops-reverser/ginkgo-report-full.json`)
shows `passed:55, skipped:8, failed:1`. The failure message:
`Timed out after 30.001s … overlap overlap-b-cm-16 must be present under path B`
(`signing_e2e_test.go:609`, inside the `Eventually` at lines 601–619). The listing in the failure
shows `overlap-b-cm-00..15` and `overlap-b-cm-17..19` present — exactly one missing: **cm-16**.

**F2 — The missing object is a genuine drop, not slowness.**
Querying Gitea (`testorg/e2e-signing-1781531991`, branch `main`, path
`e2e/signing-overlap/b/v1/configmaps/1781531991-test-signing/`) well after the run:

| Band | Present under B |
|---|---|
| `seed-cm-*` | 40 / 40 |
| `overlap-b-cm-*` | **19 / 20** (cm-16 absent) |
| `live-b-*` | 0 (the spec failed before that step ran) |

The repo is per-run (timestamped name), so this is the same data the test saw — cm-16 never landed.

**F3 — Per-run state is isolated.**
Each run uses a fresh timestamped namespace (`1781531991-test-signing`), a fresh Gitea repo, and the
coverage watermark is in-memory on the controller, keyed by the GitTarget's namespaced name, cleared
on delete (`clearTargetTypeWatermarks`) and lost on controller restart (the pod is redeployed each
run). There is no documented channel for run N−1 to corrupt run N.

**F4 — Redis streams are bounded, not accumulating.**
At inspection the largest per-type stream was `core:configmaps:audit:stream` at 433–454 entries; most
others sit near a ~50 trim cap. Valkey is **not** flushed between runs, but `XADD MAXLEN` keeps each
stream bounded, so "the second run drowns in run-1 backlog" is not the mechanism.

**F5 — The spec passes in isolation.**
A focused re-run (`ginkgo --focus="late-joining target" --procs=1`) against the same warm cluster
**passed** (1/1, 35.0s; bootstrap was a 2.1s no-op). The failure is therefore **intermittent**, and
correlates with full-suite concurrency (`--procs=4`), not with run ordinality.

**F6 — Two requested full-suite follow-ups did not reproduce the failure.**
On **2026-06-15**, while the cluster from the failed investigation was still available, a full
`task test-e2e` run against the reused cluster **passed**: random seed `1781535194`, warm
`SynchronizedBeforeSuite` `1.423s`, `48 Passed / 0 Failed / 8 Skipped`, suite runtime `335.793s`
(Ginkgo total `5m36.963847831s`). The overlap-band spec itself passed in `34.220s`.

Then `task clean-cluster` **passed**, deleting `gitops-reverser-test-e2e` and
`.stamps/cluster/k3d-gitops-reverser-test-e2e`. Docker was rechecked and available. A fresh-cluster
`task test-e2e` run also **passed**: random seed `1781535554`, cold `SynchronizedBeforeSuite`
`206.329s`, `48 Passed / 0 Failed / 8 Skipped`, suite runtime `522.089s` (Ginkgo total
`8m43.437847699s`). The overlap-band spec itself passed in `39.479s`.

These two passes are negative evidence only: they show the failure does not currently reproduce on
demand, with or without cluster reuse. They do **not** explain where the original missing
`overlap-b-cm-16` event went.

**F7 — A later warm-cluster rerun failed elsewhere, while the overlap-band spec passed again.**
On **2026-06-15**, Docker was available, but `make test-e2e` could not run because the repository has
no `test-e2e` make target (`make: *** No rule to make target 'test-e2e'.  Stop.`). The equivalent
project task, `task test-e2e`, was run against the warm cluster with random seed `1781536217`.

That run **failed** overall: `47 Passed / 1 Failed / 8 Skipped`, suite runtime `346.814s` (Ginkgo
total `5m48.038004632s`). The failure was **not** the signing overlap-band spec. The overlap-band
spec passed in `39.591s`.

The failing spec was `Commit Window Batching > collapses a burst of events into one grouped commit
and one push` ([`test/e2e/commit_window_batching_e2e_test.go:164`](../../../test/e2e/commit_window_batching_e2e_test.go#L164)):
`expected 1 grouped commit for the burst, got 3`. The local checkout for
`e2e-commit-window-1781536217` showed the expected grouped 4-resource commit, plus two extra commits
touching `commit-window-burst-1781536217-3.yaml` (`D` then `A`). This is evidence of a separate
commit-window behavior, not a reproduction of the signing coverage drop.

**F8 — Another warm-cluster rerun passed overlap and commit-window, then failed in CRD lifecycle.**
On **2026-06-15**, Docker was available and `task test-e2e` was run again against the same warm
cluster without `task clean-cluster`. Random seed: `1781536806`. Warm `SynchronizedBeforeSuite`:
`1.900s`.

That run **failed** overall: `44 Passed / 1 Failed / 11 Skipped`, suite runtime `425.066s` (Ginkgo
total `7m6.313383409s`). The overlap-band spec passed in `34.665s`; the previously failing
commit-window batching spec also passed in `31.315s`.

The failing spec was `Manager CRD Lifecycle > should create Git commit when IceCreamOrder is added
via WatchRule` ([`test/e2e/crd_lifecycle_e2e_test.go:340`](../../../test/e2e/crd_lifecycle_e2e_test.go#L340)).
It timed out after `120.000s` waiting for
`.stamps/repos/e2e-manager-crd-1781536806/e2e/icecream-test/crd-lifecycle.e2e.example.com/v1/icecreamorders/1781536806-test-manager-crd/alices-order.yaml`.
During the quiet period, the test process was blocked deleting namespace
`1781536806-test-manager-crd`; the namespace was `Terminating` for more than two minutes and then
completed deletion. The local checkout for `e2e-manager-crd-1781536806` contained the CRD-install
commits, including `icecreamorders.crd-lifecycle.e2e.example.com.yaml`, but no
`e2e/icecream-test/.../alices-order.yaml`.

---

## 3. What this rules out

- **R1 — Not a "second run / reuse" determinism bug.** F3 + F4 + F5 + F6. The user's framing ("it
  should run a second time") is correct *as a property*; the observed failure simply is not caused by
  the re-run. The first run passing and the second failing is consistent with an intermittent race
  sampled twice, and the later reused-cluster pass plus clean-cluster pass make a reuse-only
  explanation even less likely.
- **R2 — Not stale coverage-watermark contamination.** The watermark is in-memory, namespaced-name
  keyed, and reset on delete/restart (F3). A run-1 `signing-overlap-b` boundary cannot gate run-2.
- **R3 — Not the docs/upgrade-notes change.** That change set is pure markdown; it does not touch the
  watch/queue/git path.

---

## 4. The mechanism: what is forced, and what is still open

### 4.1 The invariant that narrows the cause (DERIVED)

Target B becomes present-correct for a type by exactly two paths, both fed from one
`SpliceSnapshotForType` call ([`event_router.go:210–241`](../../../internal/watch/event_router.go#L210-L241)):

1. **the reconcile fold** — `snapshot.Desired`, the materialized checkpoint+log folded into a desired
   set, written by a scoped resync;
2. **the live tail** — `applyAuditChangesForType`
   ([`audit_tail.go:211–220`](../../../internal/watch/audit_tail.go#L211-L220)) routes a main-stream
   entry to B iff its stream id is strictly after `Hc = snapshot.CoverageHead`.

`Desired` and `CoverageHead` come from the **same** splice, so they are intended to be a consistent
cut: every object folded has id ≤ `Hc`; every object with id > `Hc` is routed live. **Therefore an
object that is a normally-ordered entry on the main RV stream cannot be dropped** — it is either ≤
`Hc` (folded) or > `Hc` (routed).

**DERIVED conclusion:** cm-16 was *not present as a normally-ordered main-stream entry within the
splice's cut at the moment B reconciled.* The drop must come from one of: (a) it was diverted off the
main stream, (b) it was never ingested onto the main stream, or (c) the checkpoint the splice folded
did not contain it *and* `Hc` was nonetheless advanced past its position.

### 4.2 The late-lane path (the obvious candidate)

The mirror keeps the main stream strictly RV-ordered; an event whose RV is **strictly below the
stream high-water** is rejected by `XADD` and diverted to the diagnostic late lane `:audit:late`
([`redis_bytype_queue.go:53–68, 119–145`](../../../internal/queue/redis_bytype_queue.go#L53-L68)).
The ordered log **never replays** a late-lane entry, so the fold never folds it and the tail never
routes it — a silent loss by construction. The `lateNotify` callback is the only recovery: it asks
the materialization layer to *pull the next checkpoint forward* so a fresh LIST re-captures the
object. This is the "late-event → resync nudge."

If cm-16's CREATE arrived out of RV order under batch+join load, this path explains the drop exactly,
and explains why a backstop *should* have recovered it.

### 4.3 Why a single object can race here (HYPOTHESIS)

The spec deliberately creates the 20-object `overlap-b` band and target B **at the same instant**
([`signing_e2e_test.go:588–598`](../../../test/e2e/signing_e2e_test.go#L588-L598)), precisely to keep
the band straddling B's join (§8.1 of the sibling doc). Under `--procs=4`, audit delivery for a batch
is not RV-ordered, so one object landing just below the high-water — or just after B's checkpoint cut
— is plausible for *some* object on *some* run. That it was cm-16 specifically is noise.

### 4.4 The fact that disciplines the late-lane hypothesis (FACT → caution)

The per-type `:audit:idstate` hash for ConfigMaps reads:

```
mainCount=454  rvMissingCount=66  lastRV=12442  lastStreamID=12442-0
# lateCount field absent ⇒ 0
```

**`lateCount=0` for ConfigMaps.** Across this cluster's stream life, *no* ordered ConfigMap event was
diverted to the late lane (other types show `lateCount` 1–54). So **§4.2 is not supported by the
counters for this type** and must not be assumed to be the cause. Two honest caveats keep this from
being a clean refutation: the idstate hash persists across runs and may have been reset when the
stream was last (re-)created, and the late lane was **empty** at inspection (trimmed), so the original
cm-16 entry — wherever it went — was not captured.

What `lateCount=0` does *not* rule out is the **`rvMissingCount=66`** path: ConfigMap events here are
frequently RV-less and "attach to the high-water" (`placementAttachedToLastRV`). Whether that attach
can ever place an object where neither the fold nor the tail picks it up is **unverified** and worth a
unit test.

### 4.5 Candidate mechanisms, ranked by current evidence

| # | Hypothesis | Supported by | Argued against by | Decisive check |
|---|---|---|---|---|
| H1 | **Never ingested** onto the main stream (audit-delivery gap or best-effort mirror-write loss, IR8/IR9) | consistent with `lateCount=0`; the splice/tail invariant (§4.1) forces "not on the stream" | no direct evidence of a dropped webhook delivery | Dump the ConfigMap stream for the run's namespace at failure time; check audit-webhook receive logs for cm-16's UID |
| H2 | **Late-lane diversion** (RV below high-water) | exact silent-loss-by-construction; nudge-should-recover story | **`lateCount=0` for ConfigMaps** (§4.4) | Snapshot `:audit:late` + `lateCount` *at failure time*, before trim |
| H3 | **RV-missing attach anomaly** (`rvMissingCount=66`) places the entry outside the fold/tail cut | rv-missing is common for this type | attached entries are still on the main stream (should be folded or routed) | Unit test: an RV-less CREATE around a splice boundary |
| H4 | **Checkpoint/`Hc` seam**: the folded checkpoint lacked cm-16 yet `Hc` advanced past its position | the only main-stream-consistent drop | the splice is one consistent read (§4.1) | Assert in `SpliceSnapshotForType` that every id ≤ `CoverageHead` is in `Desired` |

H1 is currently the **least-contradicted** by measurement; H2 is the most intuitive but is
**contradicted by `lateCount=0`**. No hypothesis is confirmed.

---

## 5. The assumption to challenge directly

The spec — and §8.1 of the sibling doc — assume content-convergence under B is **fast and
self-completing**: "Present eventually … regardless of which path delivered it," asserted inside a
**30s** `Eventually`. But the only *guaranteed* recovery for an object that missed both the
join-time fold and the live tail is the **next checkpoint / heal re-anchor**, which re-LISTs the live
API. If that backstop is not bounded within 30s — and `DeferCleanup` deletes target B at spec end,
removing it before any later heal can run — then the test asserts a timeliness guarantee the system
may not actually make.

So a precise question, ahead of any fix:

> Is "every object that exists when a target joins appears under that target within *one* reconcile"
> a guarantee we make, or only "…by the next checkpoint/heal"? The test currently assumes the former.

The answer decides whether this is a **product bug** (the join-time path must be gap-free) or a
**test over-assertion** (convergence is eventually-consistent and the assertion needs the heal cadence
in its budget) — or both (a real gap *and* a too-tight assertion masking it).

---

## 6. Getting a deterministic reproduction

Chasing this through the full e2e is the wrong instrument (§8.1: an e2e cannot classify the overlap
band without an observable `Hc` or a tail-pause hook). Proposed order of attack:

1. **Instrument the e2e failure path first (cheap, high value).** On this spec's failure, before
   `DeferCleanup`, dump for `core:configmaps`: the main-stream `XRANGE` filtered to the run namespace,
   the `:audit:late` `XRANGE`, and the `:audit:idstate` hash; plus the controller's audit-ingest log
   lines for the missing object's name. One real capture collapses H1–H4 to one.
2. **Unit/integration repro in `internal/watch` / `internal/queue`** (`TestAuditTailFanout_*`,
   `redis_bytype_queue_test.go`): drive a splice while feeding the mirror an object event that is
   (a) RV below high-water, (b) RV-less, and (c) arriving just after the checkpoint cut — and assert
   the object is present under the target after the splice settles (and after a simulated late
   notify). This is the red-first proof the sibling doc already recommended for the replay direction.
3. **Only then** decide the fix surface: a gap-free join-time path, a reliable late→resync nudge for
   the affected target (not just the type), or an explicit "converges by next checkpoint" contract
   with the test budget adjusted to match.

---

## 7. Open questions (explicit unverified assumptions)

- **Q1.** Where did cm-16's event actually go — main stream, late lane, or nowhere? *Unverified;* the
  evidence was trimmed before capture. Instrumentation (§6.1) is required.
- **Q2.** Does the `lateNotify` resync nudge re-drive a reconcile for an **already-Synced** target B,
  or only refresh the type's checkpoint? If only the latter, B's `Desired` can stay stale until a
  separate re-anchor fires.
- **Q3.** Is `SpliceSnapshotForType` guaranteed to return a `CoverageHead` that is ≤ the highest id it
  folded into `Desired`? (The H4 invariant.) Untested as an assertion today.
- **Q4.** Can an RV-less event attached to the high-water (`rvMissingCount` path) ever sit at an id the
  fold excludes but the tail also excludes? (The H3 invariant.)
- **Q5.** What is the heal / re-anchor cadence relative to the spec's 30s window, and does
  `DeferCleanup` delete B before it can run? (Decides §5.)

---

## 8. Bottom line

A single CREATE'd ConfigMap was permanently absent under a late-joining target. It is a real,
intermittent coverage gap, not a re-run artifact; subsequent full-suite checks on both the reused
cluster and a fresh cluster passed, and later suite failures were in commit-window batching and CRD
lifecycle while the overlap spec passed. The splice/tail invariant proves the object was not a
normally-ordered main-stream entry within B's reconcile cut, but the **specific** path off the main
stream is **unconfirmed**, and the most intuitive explanation (late lane) is contradicted by
`lateCount=0` for this type. The next concrete step is not a fix but a **capture**: instrument the
spec to dump the stream, late lane, idstate, and ingest log at failure time, so H1–H4 collapse to one.
