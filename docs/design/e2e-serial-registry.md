# E2E Serial Registry

This is a **living document**. It is the authoritative list of e2e test
containers that must run `Serial` (i.e. never concurrently with any other spec)
under bounded Ginkgo parallelism (Phase 2.5 of
[e2e-speedup-plan.md](e2e-speedup-plan.md)).

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
| [test/e2e/audit_redis_e2e_test.go](../../test/e2e/audit_redis_e2e_test.go) | `Audit Redis Queue`, `Audit Redis Consumer` | The single global audit pipeline: audit webhook → shared Redis stream → consumer. | The consumer is a cluster-wide singleton processing one shared stream; these specs assert on commit *exclusivity*, which any concurrent audit traffic violates. |
| [test/e2e/commit_window_batching_e2e_test.go](../../test/e2e/commit_window_batching_e2e_test.go) | `Commit Window Batching` | Same global audit pipeline (labelled `audit-redis`). | Bursts of ConfigMap events flow through the shared stream/consumer and leak into other audit specs' commits. |
| [test/e2e/commit_request_e2e_test.go](../../test/e2e/commit_request_e2e_test.go) | `Commit Request` | Same global audit pipeline (labelled `audit-redis`). | Shares the stream/consumer; commit-window/queue semantics interleave under concurrency. |

## Watched, but intentionally NOT Serial

- **`crd_lifecycle_e2e_test.go`** installs a `ClusterWatchRule` that watches all
  CRDs cluster-wide (`ClusterResourceRule` has no per-object-name filter, see
  [api/v1alpha1/clusterwatchrule_types.go](../../api/v1alpha1/clusterwatchrule_types.go)).
  While that rule is live it mirrors *other* files' CRD definitions into its own
  repo. This is harmless: the rule is only active during the two CRD-lifecycle
  specs (install/delete), it is torn down before the "no new commit" assertion,
  and every assertion checks a *specific*, group-scoped file path. Extra mirrored
  files are cosmetic. Kept parallel; revisit if the stability window shows flakes.

All four `audit-redis`-labelled containers above were moved to `Serial` after the
first procs=2 smoke run: `Audit Redis Consumer` asserted its commit touched only
its own file but picked up `Commit Window Batching`'s files from the shared
consumer. Because Ginkgo runs `Serial` specs alone (after all parallel specs),
the audit pipeline now only ever has one client at a time.

## Triage list (watch during stability runs)

`controller_basics_e2e_test.go` spec #5 ("should receive audit webhook events")
reads a **global Prometheus audit counter** with a baseline/delta pattern. It
remained parallel and passed, because the delta only needs the counter to
*increase*. If a future change makes that read exact, concurrent audit traffic
from other parallel specs could break it — harden the read (filter by
namespace/resource) or move the container into the Serial table above.
