# E2E Serial Registry

This is a **living document**. It is the authoritative list of e2e test
containers that must run `Serial` (i.e. never concurrently with any other spec)
under bounded Ginkgo parallelism (Phase 2.5 of
[e2e-speedup-plan.md](../finished/e2e-speedup-plan.md)).

> De-serializing any row below is an isolation refactor, not a label flip.
> The two audit-consumer specs, `bi_directional`, and `aggregated_apiserver` have
> all been de-serialized (see the "De-serialized" section). `crd_lifecycle` was
> de-serialized and then **re-serialized** (commit `3d249e3`) when a residual
> cluster-wide CRD-discovery churn re-flaked it. The current Serial set is the two
> singleton-controller specs, `crd_lifecycle`, and `playground`.

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
| [test/e2e/restart_reconcile_e2e_test.go](../../test/e2e/restart_reconcile_e2e_test.go) | `Restart Reconcile Safety` | Rollout-restarts the controller deployment. | The controller is a singleton; restarting it disrupts in-flight reconciles/commits for every other spec. |
| [test/e2e/image_refresh_test.go](../../test/e2e/image_refresh_test.go) | `image refresh dependency chain` | Changes the controller image / redeploys the controller. | Same singleton controller; an image swap perturbs all concurrent specs. |
| [test/e2e/crd_lifecycle_e2e_test.go](../../test/e2e/crd_lifecycle_e2e_test.go) | `Manager CRD Lifecycle` | Installs/deletes a cluster-scoped CRD and watches all `customresourcedefinitions` cluster-wide, asserting exact Git file presence/absence. | A concurrent CRD install/delete bumps the global discovery-catalog generation and re-resolves every GitTarget's watched-type tables, delaying this spec's reconcile (CRD file appears late / post-delete sweep lags — the 649↔673 flake). A cluster-wide catalog bump cannot be scoped by name. |
| [test/e2e/tilt_playground_e2e_test.go](../../test/e2e/tilt_playground_e2e_test.go) | `playground` | Reusable manual-playground fixture: fixed (non-randomized) `tilt-playground` namespace and `playground` repo/provider names, preserved across runs. | A singleton by construction — its resource names are intentionally stable so `task playground-*` can re-attach, so per-run name isolation does not apply. |

### De-serialized

`Manager CRD Lifecycle` ([crd_lifecycle_e2e_test.go](../../test/e2e/crd_lifecycle_e2e_test.go))
**was de-serialized and then re-serialized** — it is in the Serial table above, not
here. Finding #2's fix ([manager.go:1191](../../internal/watch/manager.go#L1191)) —
resnapshot a target only when its *resolved* plan hash changes — removed the worst
symptom: a catalog refresh no longer drags unrelated targets into `reconcile: sync …`
resnapshots, and per-file CRD groups ([icecream.go:30](../../test/e2e/icecream.go#L30))
keep the `IceCreamOrder` CRD name-isolated. But a residual effect remained — a
concurrent CRD install/delete still bumps the cluster-wide discovery-catalog
generation and re-resolves every GitTarget's watched-type tables, delaying this
spec's *own* reconcile enough to flake its exact file presence/absence checks (the
649↔673 flake). It was re-serialized in commit `3d249e3`; de-serializing it again
needs that catalog-churn delay fixed in the controller, not another label flip.

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

The two `audit-consumer`-labelled containers (`Commit Window Batching`
([commit_window_batching_e2e_test.go](../../test/e2e/commit_window_batching_e2e_test.go)),
`Commit Request` ([commit_request_e2e_test.go](../../test/e2e/commit_request_e2e_test.go)))
were also de-serialized. Their real serial cause was **not** the shared audit
*pipeline* — namespace-scoped WatchRules already route each spec's events only to
its own GitTarget — but a shared *repo*: both borrowed one Gitea repo from a
`sync.Once` helper and asserted on whole-branch state (`Commit Request` reads
`HEAD`/`status.sha`; `Commit Window Batching` reads `git rev-list --count main`),
so the *other* audit spec's commits to the same `main` corrupted those reads.
Each now provisions its **own** repo via `SetupRepo`, so the only writer to its
`main` is its own GitTarget, fed exclusively by audit events from its own
namespace. The batching property is a per-GitTarget branch-worker behaviour (each
target runs its own commit window), so events for other GitTargets cannot enter
this spec's grouped commit. The retired `sync.Once` helper (`audit_helpers_test.go`)
was deleted.

`Bi Directional (Flux)` ([flux_bi_directional_e2e_test.go](../../test/e2e/flux_bi_directional_e2e_test.go))
was de-serialized too. It already owned a dedicated repo and per-file CRD group;
its historical "+2 commits only under parallelism" came from cluster-wide GVR
catalog churn (another spec installing/deleting a CRD) dragging unrelated targets
into a resnapshot whose `reconcile: sync …` commits inflated its exact count.
Finding #2's fix ([manager.go:1191](../../internal/watch/manager.go#L1191)) —
a target only resnapshots when its *resolved* plan hash changes — removed that
mechanism, the same fix that unblocked `crd_lifecycle`.

`Aggregated API server` ([aggregated_apiserver_e2e_test.go](../../test/e2e/aggregated_apiserver_e2e_test.go))
was de-serialized after its `Serial` justification proved **stale**. The note
claimed it "installs/removes a cluster-scoped `APIService` while it is in flight",
but the wardle APIService (`v1alpha1.wardle.example.com`) is installed once at
cluster setup by the apiservice-audit-proxy HelmRelease
(setup/flux/releases/aggregated-api.yaml)
and stays Available for the whole suite. The spec only *reads* it — it never
registers or removes it — so there is no in-flight discovery perturbation to
serialize against. Everything the spec mutates is name-isolated (own namespace,
own repo, namespace-scoped flunder WatchRule, namespaced Flunders), and its one
cluster-wide assertion (`gitopsreverser_audit_shallow_dropped_total == 0`) is a
global zero-invariant that is parallel-safe by design.

## Triage list (watch during stability runs)

`controller_basics_e2e_test.go` spec #5 ("should receive audit webhook events")
reads a **global Prometheus audit counter** with a baseline/delta pattern. It
remained parallel and passed, because the delta only needs the counter to
*increase*. If a future change makes that read exact, concurrent audit traffic
from other parallel specs could break it — harden the read (filter by
namespace/resource) or move the container into the Serial table above.
