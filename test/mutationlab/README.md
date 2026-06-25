# Mutation-capture lab

A small, separate application that records the exact structures Kubernetes
exposes through native watches, audit webhooks, and validating admission
webhooks, and commits them as a versioned corpus. It is **not** a second
GitOps Reverser — see [the design](../../docs/design/mutation-capture-lab-design.md).

## Layout

| Path | What |
|---|---|
| `cmd/mutation-capture-lab/` | the lab binary (recorders + records API) |
| `internal/mutationlab/` | record model, normalizer, store, corpus, recorders, API |
| `test/mutationlab/Dockerfile` | the lab image (built only by `task lab-build-image`) |
| `test/mutationlab/swap-image.sh` | swaps the lab image onto the controller Deployment |
| `test/mutationlab/e2e/` | the live-cluster driver (build tag `mutationlab_e2e`) |
| `test/mutationlab/corpus/` | the committed golden corpus + `CLUSTER.md` provenance |

## Scenario index

Each captured row of the [Difficult Cases Catalog](../../docs/design/mutation-capture-lab-design.md#difficult-cases-catalog)
maps to one driver test and one corpus directory. The "Sources" column is what the
corpus commits — a **silence** (no audit / no admission) is itself the finding, not a
gap (see [Capturing Intent, Not State](../../docs/design/mutation-capture-lab-design.md#capturing-intent-not-state)).

| Row | Scenario | Test (`e2e/…`) | Corpus dir (`corpus/…`) | Sources committed |
|---|---|---|---|---|
| 1 | Create succeeds | `configmap_scenarios_test.go` · `TestCreateSucceeds` | `configmap/create-succeeds/` | watch, audit, admission |
| 2 | Update (PUT) | `configmap_scenarios_test.go` · `TestUpdate` | `configmap/update/` | watch, audit, admission |
| 3 | Server-side apply | `configmap_scenarios_test.go` · `TestServerSideApply` | `configmap/server-side-apply/` | watch MODIFIED, audit (`patch`, apply field manager), admission UPDATE (apply options) |
| 4 | No-op apply | `configmap_scenarios_test.go` · `TestNoOpApply` | `configmap/no-op-apply/` | audit, admission — **no** watch (resourceVersion unchanged) |
| 5 | Status subresource | `workload_scenarios_test.go` · `TestStatusSubresource` | `deployment/status-update/` | watch ×2 — **no** audit, **no** admission |
| 6 | Scale subresource | `workload_scenarios_test.go` · `TestScaleSubresource` | `deployment/scale-patch/` | watch, audit — **no** admission |
| 7 | Graceful delete | `workload_scenarios_test.go` · `TestGracefulDelete` | `pod/graceful-delete/` | watch (MODIFIED + DELETED), admission — **no** audit |
| 8 | Finalizer delete | `configmap_scenarios_test.go` · `TestFinalizerDelete` | `configmap/finalizer-delete/` | watch (MODIFIED + DELETED), audit (delete + patch — **no** second delete), admission (DELETE + UPDATE) |
| 9 | Deletecollection | `configmap_scenarios_test.go` · `TestDeletecollection` | `configmap/deletecollection/` | watch ×N, audit ×1 (name-less), admission ×N (per object) |
| 10 | Owner-ref cascade | `configmap_scenarios_test.go` · `TestOwnerRefCascade` | `configmap/owner-ref-cascade/` | watch DELETED ×2 (parent + cascaded child), audit ×2 (parent = human, child = `generic-garbage-collector`) |
| 11 | Dry-run create | `configmap_scenarios_test.go` · `TestDryRunCreate` | `configmap/dry-run-create/` | audit, admission — **no** watch / **no** etcd object |
| 12 | Record-and-reject | `configmap_scenarios_test.go` · `TestRecordAndReject` | `configmap/record-and-reject/` | audit, admission — **no** watch / **no** etcd object |
| 13 | Optimistic-concurrency conflict | `configmap_scenarios_test.go` · `TestOptimisticConcurrencyConflict` | `configmap/conflict-update/` | audit ×1 (`update`, code 409) — **no** watch / **no** admission (rejected at storage, before admission) |
| 14 | Multi-version CRD conversion | `crd_conversion_test.go` · `TestCRDConversion` | `widget/crd-conversion/` | watch (v2), audit (v1), admission (v1), conversion ×2 (both directions) |
| 15 | Aggregated API write | `aggregated_api_test.go` · `TestAggregatedAPIWrite` | `flunder/aggregated-api-write/` | watch (full object), audit (empty body), audit-additional (proxy-enriched full body); admission is observed but not committed |
| 16 | Watch resync (`410 Gone`) | `watch_transport_test.go` · `TestWatchExpiredResourceVersion` | `configmap/watch-resync/` | watch ERROR (`Status` 410); driver verifies relist recovery |
| 17 | Bookmark | `watch_transport_test.go` · `TestWatchBookmark` | `configmap/watch-bookmark/` | watch BOOKMARK with resourceVersion |

All seventeen catalogued scenarios are now captured. Rows 16 and 17 test the watch
transport itself; the driver uses the lab's targeted `/watch-probe` endpoint so
transport-only events can be scenario-attributed — see the
[watch-only ingestion architecture](../../docs/design/watch-only-ingestion-architecture.md#phase-0-finish-the-evidence)
Phase 0 notes.

## How it integrates: swap the image, reuse the wiring

The lab serves the **same** webhook URLs as the product —
`/validate-admission-webhook`, `/audit-webhook`, and the proxy-enrichment
`/audit-webhook-additional` — on the same ports and TLS cert mounts. So making a
cluster capture with the lab is just swapping the controller image: no new audit
policy, webhook config, or certificates. `task lab-e2e` does this on the
already-prepared e2e cluster, then drives the scenarios serially.

The `/audit-webhook-additional` endpoint is the integration point the
[`apiservice-audit-proxy`](https://github.com/ConfigButler/apiservice-audit-proxy)
posts enriched bodies to. The lab records it as its own source so the corpus can
show, side by side, what the official audit event carried versus what the proxy
added — and whether a live watch already carries the same object, which would
make that proxy unnecessary for object content.

## Running it (opt-in, serial)

These targets are **not** part of `task test-e2e` or the default CI lane.

```bash
# Prepare the e2e cluster + product wiring, swap in the lab image, capture, diff:
task lab-e2e

# Accept a new/changed capture as the golden corpus:
task lab-corpus-update
```

`task lab-e2e` leaves the cluster running the lab image. To restore the product:

```bash
task clean-cluster && task test-e2e
```

The unit tests for the lab packages run in the normal lane (`task test`); the
`test/mutationlab/e2e/` driver is behind the `mutationlab_e2e` build tag and only
runs under `task lab-e2e`.

## Validating a new Kubernetes version

Bump the k3d image, run `task lab-corpus-update`, and review the `corpus/` diff:
it is the behavioral changelog for the upgrade, scoped to exactly the fine-grained
behaviors GitOps Reverser depends on. An empty diff is a positive result.
