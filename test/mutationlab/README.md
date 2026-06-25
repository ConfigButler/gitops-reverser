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
| 1 | Create succeeds | `create_succeeds_test.go` · `TestCreateSucceeds` | `configmap/create-succeeds/` | watch, audit, admission |
| 2 | Update (PUT) | `m1_scenarios_test.go` · `TestUpdate` | `configmap/update/` | watch, audit, admission |
| 5 | Status subresource | `m2_scenarios_test.go` · `TestStatusSubresource` | `deployment/status-update/` | watch ×2 — **no** audit, **no** admission |
| 6 | Scale subresource | `m2_scenarios_test.go` · `TestScaleSubresource` | `deployment/scale-patch/` | watch, audit — **no** admission |
| 7 | Graceful delete | `m2_scenarios_test.go` · `TestGracefulDelete` | `pod/graceful-delete/` | watch (MODIFIED + DELETED), admission — **no** audit |
| 9 | Deletecollection | `m1_scenarios_test.go` · `TestDeletecollection` | `configmap/deletecollection/` | watch ×N, audit ×1 (name-less), admission ×N (per object) |
| 11 | Dry-run create | `m1_scenarios_test.go` · `TestDryRunCreate` | `configmap/dry-run-create/` | audit, admission — **no** watch / **no** etcd object |
| 12 | Record-and-reject | `m1_scenarios_test.go` · `TestRecordAndReject` | `configmap/record-and-reject/` | audit, admission — **no** watch / **no** etcd object |
| 14 | Multi-version CRD conversion | `m3_scenarios_test.go` · `TestCRDConversion` | `widget/crd-conversion/` | watch (v2), audit (v1), admission (v1), conversion ×2 (both directions) |
| 15 | Aggregated API write | `m4_scenarios_test.go` · `TestAggregatedAPIWrite` | `flunder/aggregated-api-write/` | watch (full object), audit (empty body), audit-additional (proxy-enriched full body); admission is observed but not committed |

Rows **3, 4, 8, 10, 13, 16, 17** (server-side apply, no-op apply, finalizer delete,
owner-ref cascade, optimistic-concurrency conflict, watch resync, bookmark) are not yet
captured.

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
