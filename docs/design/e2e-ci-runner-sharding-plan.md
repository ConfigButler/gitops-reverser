# E2E CI Runner Sharding Plan

## Question

We are now allowed **up to 20 concurrent GitHub-hosted runners** (previously this
repo was throttled at ~3). Can we split the e2e suite across more runners to cut
wall-clock CI time? An earlier attempt "didn't fully work" — this doc explains
why, what changed, and a concrete plan.

## TL;DR

- **Yes, and the one thing that blocked it before is now gone.** The only reason
  matrix sharding was rejected in [e2e-speedup-plan.md](e2e-speedup-plan.md)
  ("Considered and skipped", item 2) was the concurrent-runner throttle: extra
  shards *queued* instead of running in parallel, so they burned CPU minutes for
  no wall-clock win. With 20 runners that constraint is lifted.
- **Sharding across runners is strictly *safer* than raising `--procs` on one
  cluster.** Each shard gets its own k3d cluster, so the full-suite shared-state
  races documented in
  [e2e-full-suite-shared-state-investigation.md](e2e-full-suite-shared-state-investigation.md)
  **cannot cross a shard boundary**, and `Serial` specs stop contending with
  everything else in the run.
- **The economics work now**: in both current e2e lanes, *test execution*
  dominates *fixed cluster-bringup overhead* (see the measured breakdown below),
  so splitting execution across runners actually moves the needle.
- **The real critical path today is the `quickstart` lane (~18 min), not `full`
  (~15 min)** — so a good plan splits *both*, not just `full`.

## Measured baseline (run 28649519807, PR e2e, 2026-07-03)

Both e2e lanes run in parallel today on 2 `ubuntu-latest` runners. Step-level
timings:

| Lane | Fixed overhead¹ | Test execution | Total |
|---|---|---|---|
| `full` | ~4.6 min | **636 s (10.6 min)** | ~15.2 min |
| `quickstart` | ~4.2 min | **823 s (13.7 min)** | ~18.0 min |

¹ Fixed overhead = free-disk + artifact download + docker load + "Bring up
cluster + Flux services" (140 s / 95 s) + coverage collect + report. It is paid
**once per runner/cluster** and is the tax every new shard adds.

Two facts drive the whole plan:

1. **Execution ≫ overhead** (10.6 vs 4.6; 13.7 vs 4.2). Amdahl is on our side:
   splitting the execution slice across `S` shards gives, per shard,
   `wall ≈ overhead + execution / S`.
2. **`quickstart` is the longer lane.** Sharding only `full` below 15 min buys
   nothing until `quickstart` is also shortened — the whole-CI e2e gate is
   `max(all lanes)`.

### What each lane actually runs

- **`full`** = `go run ginkgo --procs=4 --label-filter='!image-refresh'` — the
  ~46-spec functional suite on one cluster. `Serial` specs
  (`restart-reconcile`, `crd_lifecycle`, `playground`) are interleaved by Ginkgo;
  everything else runs 4-wide. This is the only lane that collects e2e coverage
  (config-dir install carries the `GOCOVERDIR` overlay).
- **`quickstart`** = **four heavy operations, strictly sequential on one cluster**:
  1. `test-e2e-quickstart-helm` — Helm install validation
  2. `cleanup-installs.sh`
  3. `test-image-refresh` (`Serial`) — local **rebuild** + k3d reload + rollout
  4. `test-e2e-quickstart-manifest` — plain-manifest install validation

  Steps 1/3/4 each install/reinstall the controller, so they can't overlap *on
  one cluster* — but on **separate clusters** they are independent.

## Why "it didn't fully work" before

Historical context, not a blocker anymore:

- **Runner throttle** (the real one): shards queued, so 3 shards × 15 min ran
  serially ≈ 45 min. Recorded in [e2e-speedup-plan.md](e2e-speedup-plan.md).
- **Shared-state flakes** within one cluster (stale `GitTarget`/`WatchRule`
  writing to a shared repo/branch — see the
  [shared-state investigation](e2e-full-suite-shared-state-investigation.md)).
  Sharding *removes* this class between shards (separate clusters) and the
  per-target isolation fix
  ([gittarget-isolation-on-rule-change.md](../finished/gittarget-isolation-on-rule-change.md))
  already raised the safe in-cluster parallelism from `procs=1` → `procs=4`.

## Why sharding beats "just raise `--procs`"

The controller is a **singleton** per cluster. On a stock `ubuntu-latest` runner
it is CPU-starved past `procs=4` (a different spec timed out each run — see
Phase 2.5 of the speedup plan). More Ginkgo procs on one cluster all fight for
one controller's CPU. **A new shard gets its own controller, its own cluster, and
a whole runner's CPU** — it sidesteps the singleton ceiling entirely instead of
pushing against it.

## Proposal

Reshape the `e2e` matrix in [ci.yml](../../.github/workflows/ci.yml) from **2
lanes → ~6 balanced legs**, each its own runner + cluster. Partition functional
specs with Ginkgo `--label-filter` (labels already exist — see the `Label(...)`
on each `Describe`), and break the `quickstart` chain into independent legs.

### Target matrix

| Leg | Selector | Source (today) | Notes |
|---|---|---|---|
| `manager` | `--label-filter='manager'` | part of `full` | Big bucket of many small specs; includes `Serial` `crd_lifecycle`. Collects coverage. |
| `signing-agg-bidi` | `'signing \|\| aggregated-api \|\| bi-directional'` | part of `full` | Three slow-ish independent suites. Collects coverage. |
| `audit-restart` | `'audit-consumer \|\| restart-reconcile'` | part of `full` | Slow audit specs + the slow `Serial` restart spec, alone so it can't contend. Collects coverage. |
| `quickstart-helm` | `'quickstart-framework'`, `E2E_QUICKSTART_MODE=helm` | `quickstart` step 1 | Needs release bundle artifact. |
| `quickstart-manifest` | `'quickstart-framework'`, `E2E_QUICKSTART_MODE=plain-manifests-file` | `quickstart` step 4 | Needs release bundle artifact. |
| `image-refresh` | `'image-refresh'`, local build (no `PROJECT_IMAGE`) | `quickstart` step 3 | `Serial` rebuild/reload/rollout; its own runner. |

Six legs uses **6 of 20 runners** — comfortable headroom.

### Projected wall-clock

Per leg `≈ overhead + execution/slice`. Splitting `full`'s 636 s three ways and
`quickstart`'s 823 s three ways, with ~4.5 min overhead each:

- functional legs: ~4.5 + ~3.5 ≈ **~8 min** each
- quickstart legs: ~4.5 + (helm/manifest/refresh slice) ≈ **~7–10 min** each

**New e2e gate ≈ 8–10 min, down from ~18 min — roughly halved.** The new critical
leg is whichever single slice is largest (likely `image-refresh`'s local docker
rebuild or the `manager` bucket); rebalance from data (next section).

### Cost

~6 legs × ~8 min ≈ **~48 runner-minutes** vs today's 2 × ~16.5 ≈ ~33 — about
**1.5× the CPU minutes for ~2× the speed**. Acceptable given the granted capacity;
tune the leg count to trade minutes for latency.

## Details that must be handled

1. **Balance from real data, don't guess.** Each run already uploads
   `e2e-ginkgo-reports-*` artifacts, and `test/e2e/tools/spec-timings` ranks
   specs by duration. Pull a green run's per-spec CI timings and pack the label
   groups to equalize slices. Start with the table above; adjust once measured.
2. **Coverage merge.** Today only `full` collects e2e coverage. Each functional
   shard must run the **config-dir** install (so it carries the `GOCOVERDIR`
   overlay), run `task e2e-coverage-collect`, and upload with `flags: e2e`.
   Codecov unions same-flag uploads, so the merged `e2e` coverage is the union of
   what each shard exercised — non-regression ratchet still holds. (The
   quickstart/manifest/image-refresh legs don't carry the overlay and collect no
   coverage, same as today.)
3. **Required status checks / branch protection.** Job name is
   `E2E (${{ matrix.name }})`, so adding legs changes the required-check set. Add
   a tiny aggregator job `e2e-complete` (`needs: [e2e]`, succeeds iff all legs
   pass) and make **that** the single required check — then the matrix can grow or
   shrink without editing branch protection each time.
4. **Keep `fail-fast: false`** (already set) so one leg's flake doesn't cancel the
   others mid-run and lose their reports.
5. **Serial registry stays authoritative.** [e2e-serial-registry.md](e2e-serial-registry.md)
   still governs *within* a shard. Sharding doesn't let us drop any `Serial`
   marker — it just means a `Serial` spec only blocks its own (smaller) shard.
6. **Disk pressure is per-shard.** Each fresh runner has its own disk, so the
   DiskPressure eviction mitigations (PR #187) and the `Free runner disk` step
   apply per leg — no worse per shard.
7. **Artifact fan-out.** Each shard re-downloads the CI container + project image
   tarballs (~35 s + ~44 s load). That's part of the per-shard overhead already
   counted; watch total artifact egress if leg count grows large.

## Phased rollout

- **Phase 1 — split `full` into 3 label legs** (lowest risk, no chain surgery).
  Wire per-shard coverage upload. Land the `e2e-complete` aggregator + update
  branch protection. Verify green over ~5–10 runs (watch for any residual
  cross-spec flake *inside* a shard — should be none, separate clusters).
- **Phase 2 — split the `quickstart` chain into 3 legs**
  (`quickstart-helm`, `quickstart-manifest`, `image-refresh`). This is where the
  critical-path win actually lands, since quickstart is the long pole.
- **Phase 3 — rebalance** leg membership from measured per-spec CI timings; merge
  or split legs to flatten the slowest leg. Consider whether a 4th functional leg
  is worth the extra ~4.5 min overhead.

## Rollback

Every change here lives in the `e2e` matrix of `ci.yml` (plus one aggregator job
and a branch-protection edit). Reverting to the 2-lane matrix is a one-file revert
— no product code, no test code, no cluster/state changes.

## Open questions / to validate during Phase 1

- Do `playground` / `demo` labels currently execute in the `full` lane or
  self-skip without their env gates? Confirm before assigning them to a leg (they
  should stay out of the functional shards).
- Exact per-slice split of `quickstart`'s 823 s across helm / cleanup / refresh /
  manifest — measure to decide whether `image-refresh` (local rebuild) needs to
  be the sole occupant of the slowest leg or can share.
- Whether one designated leg should run the *whole* suite for a single clean
  coverage number instead of per-shard union (simpler Codecov story vs. one extra
  full-length leg). Recommendation: start with per-shard union; revisit only if
  the merged number looks lossy.

## References

- [e2e-speedup-plan.md](e2e-speedup-plan.md) — Phases 0–3 already shipped; this
  plan is its "Revisit if we get real concurrent runner capacity" item.
- [e2e-serial-registry.md](e2e-serial-registry.md) — the authoritative `Serial` set.
- [e2e-full-suite-shared-state-investigation.md](e2e-full-suite-shared-state-investigation.md)
  — the cross-spec races sharding isolates away.
- [gittarget-isolation-on-rule-change.md](../finished/gittarget-isolation-on-rule-change.md)
  — the fix that raised safe in-cluster parallelism to `procs=4`.
- [ci-overview.md](../ci-overview.md) — trust-zone CI design the matrix lives in.
</content>
</invoke>
