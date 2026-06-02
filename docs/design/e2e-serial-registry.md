# E2E Serial Registry

This is a **living document**. It is the authoritative list of e2e test
containers that must run `Serial` (i.e. never concurrently with any other spec)
under bounded Ginkgo parallelism (Phase 2.5 of
[e2e-speedup-plan.md](e2e-speedup-plan.md)).

> De-serializing any row below is an isolation refactor, not a label flip. The
> two realistic candidates (`crd_lifecycle` and the four audit pipeline specs) are
> planned in [e2e-serial-deserialization-plan.md](e2e-serial-deserialization-plan.md).

## Rule

A test container is `Serial` only when it mutates shared, **process-level or
cluster-wide** state that cannot be isolated by name. Prefer name isolation
(e.g. per-file CRD groups — see [test/e2e/icecream.go](../../test/e2e/icecream.go))
over `Serial`; reserve `Serial` for conflicts name isolation cannot fix.

**When you add a spec that performs a cluster-scoped write (rolls/scales the
controller, installs an `APIService`/CRD/cluster RBAC, or otherwise affects
shared apiserver state), update this registry in the same PR** — either by
isolating the state by name or by marking the container `Serial` and adding a
row below.

## Serial containers

| File | Container | Shared state it touches | Why name isolation can't fix it |
|---|---|---|---|
| [test/e2e/restart_snapshot_e2e_test.go](../../test/e2e/restart_snapshot_e2e_test.go) | `Restart Snapshot Safety` | Rollout-restarts the controller deployment. | The controller is a singleton; restarting it disrupts in-flight reconciles/commits for every other spec. |
| [test/e2e/image_refresh_test.go](../../test/e2e/image_refresh_test.go) | `image refresh dependency chain` | Changes the controller image / redeploys the controller. | Same singleton controller; an image swap perturbs all concurrent specs. |
| [test/e2e/aggregated_apiserver_e2e_test.go](../../test/e2e/aggregated_apiserver_e2e_test.go) | `Aggregated API server` | Installs/removes a cluster-scoped `APIService` (aggregation layer). | Registering/removing an aggregated API briefly disrupts apiserver discovery for **every** client, including unrelated kubectl calls in other specs. |
| [test/e2e/commit_window_batching_e2e_test.go](../../test/e2e/commit_window_batching_e2e_test.go) | `Commit Window Batching` | Same global audit pipeline (labelled `audit-consumer`). | Bursts of ConfigMap events flow through the shared audit consumer and leak into other audit specs' commits. |
| [test/e2e/commit_request_e2e_test.go](../../test/e2e/commit_request_e2e_test.go) | `Commit Request` | Same global audit pipeline (labelled `audit-consumer`). | Shares the audit consumer; commit-window/queue semantics interleave under concurrency. |
| [test/e2e/bi_directional_e2e_test.go](../../test/e2e/bi_directional_e2e_test.go) | `Bi Directional` | Whole-cluster Flux↔gitops-reverser round-trip; asserts on **exact** remote commit counts to prove no commit loop. | Any concurrent controller activity adds/reorders commits and breaks the exact-count loop assertions. Passes sequentially; +2 commits only under parallelism. |

### De-serialized

`Manager CRD Lifecycle` ([crd_lifecycle_e2e_test.go](../../test/e2e/crd_lifecycle_e2e_test.go))
**was** `Serial` because installing/deleting its CRD changes cluster-wide
discovery and historically forced unrelated GitTargets to resnapshot (leaking
`reconcile: sync …` commits into other specs' exact `[CREATE]`/`[DELETE]`
assertions). It now runs parallel (`Ordered`, not `Serial`) because:

- Finding #2's fix ([manager.go:1191](../../internal/watch/manager.go#L1191))
  only resnapshots a target when its *resolved* plan hash changes, so a catalog
  refresh no longer perturbs targets that do not match the new GVR.
- Per-file CRD groups ([icecream.go:30](../../test/e2e/icecream.go#L30)) keep the
  `IceCreamOrder` CRD name-isolated, and the only other wildcard-ish
  `ClusterWatchRule` (`restart_snapshot`) is itself `Serial`, so no concurrent
  spec has a rule matching the icecream group.

See [e2e-serial-deserialization-plan.md](e2e-serial-deserialization-plan.md) for
the full rationale.

The former `Audit Redis Queue` / `Audit Redis Consumer` containers were retired:
the producer-path and basic consumer-commit assertions duplicated coverage in
the WatchRule suite, so they were dropped. The one genuinely unique assertion —
that OIDC name/email claims in `user.extra` end up as the Git commit author —
moved to `Commit Author Attribution`
([commit_author_attribution_e2e_test.go](../../test/e2e/commit_author_attribution_e2e_test.go)),
which runs **parallel** (not `Serial`): its dedicated GitProvider uses a 0s
commit window so each event is its own commit, and it reads the author scoped to
each ConfigMap's unique path, so concurrent audit traffic cannot change the
commit it asserts on.

The remaining `audit-consumer`-labelled containers (`Commit Window Batching`,
`Commit Request`) stay `Serial`: they assert on commit-window/batching semantics
over the shared audit consumer, which any concurrent audit traffic violates.
Because Ginkgo runs `Serial` specs alone (after all parallel specs), the audit
pipeline only ever has one client at a time for those.

## Triage list (watch during stability runs)

`controller_basics_e2e_test.go` spec #5 ("should receive audit webhook events")
reads a **global Prometheus audit counter** with a baseline/delta pattern. It
remained parallel and passed, because the delta only needs the counter to
*increase*. If a future change makes that read exact, concurrent audit traffic
from other parallel specs could break it — harden the read (filter by
namespace/resource) or move the container into the Serial table above.
