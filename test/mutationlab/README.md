# Mutation-capture lab

A small, separate application that records the exact structures Kubernetes
exposes through native watches, audit webhooks, and validating admission
webhooks, and commits them as a versioned corpus. It is **not** a second
GitOps Reverser — see [the design](../../docs/spec/mutation-capture-lab-design.md).

## Layout

| Path | What |
|---|---|
| `cmd/mutation-capture-lab/` | the lab binary (recorders + records API) |
| `internal/mutationlab/` | record model, normalizer, store, corpus, recorders, API |
| `test/mutationlab/Dockerfile` | the lab image (built only by `task lab-build-image`) |
| `test/mutationlab/Taskfile-lab.yml` | the lab's tasks: its own cluster, deployment, and capture |
| `test/mutationlab/deploy/` | the lab's namespace, RBAC, Deployment, Services, and webhook registration |
| `test/mutationlab/tools/lab-certs/` | issues the lab's CA, serving certificate, and the API server's audit client certificate |
| `test/mutationlab/deploy.sh`, `port-forward.sh` | deploy onto the lab cluster; forward its records API |
| `test/mutationlab/e2e/` | the live-cluster driver (build tag `mutationlab_e2e`) |
| `test/mutationlab/corpus/` | the committed golden corpus + `CLUSTER.md` provenance |

## Scenario index

Each captured row of the [Difficult Cases Catalog](../../docs/spec/mutation-capture-lab-design.md#difficult-cases-catalog)
maps to one driver test and one corpus directory. The "Sources" column is what the
corpus commits — a **silence** (no audit / no admission) is itself the finding, not a
gap (see [Capturing Intent, Not State](../../docs/spec/mutation-capture-lab-design.md#capturing-intent-not-state)).

| Row | Scenario | Test (`e2e/…`) | Corpus dir (`corpus/…`) | Sources committed |
|---|---|---|---|---|
| 1 | Create succeeds | `configmap_scenarios_test.go` · `TestCreateSucceeds` | `configmap/create-succeeds/` | watch, audit, admission |
| 2 | Update (PUT) | `configmap_scenarios_test.go` · `TestUpdate` | `configmap/update/` | watch, audit, admission |
| 3 | Server-side apply | `configmap_scenarios_test.go` · `TestServerSideApply` | `configmap/server-side-apply/` | watch MODIFIED, audit (`patch`, apply field manager), admission UPDATE (apply options) |
| 4 | No-op apply | `configmap_scenarios_test.go` · `TestNoOpApply` | `configmap/no-op-apply/` | audit, admission — **no** watch (resourceVersion unchanged) |
| 5 | Status subresource | `workload_scenarios_test.go` · `TestStatusSubresource` | `deployment/status-update/` | watch ×2 — **no** audit, **no** admission |
| 6 | Scale subresource | `workload_scenarios_test.go` · `TestScaleSubresource` | `deployment/scale-patch/` | watch, audit — **no** admission |
| 7 | Graceful delete (audit-EXCLUDED type) | `workload_scenarios_test.go` · `TestGracefulDelete` | `pod/graceful-delete/` | watch (MODIFIED + DELETED), admission — **no** audit ⚠️ |
| 8 | Finalizer delete | `configmap_scenarios_test.go` · `TestFinalizerDelete` | `configmap/finalizer-delete/` | watch (MODIFIED + DELETED), audit (delete + patch — **no** second delete), admission (DELETE + UPDATE) |
| 9 | Deletecollection | `configmap_scenarios_test.go` · `TestDeletecollection` | `configmap/deletecollection/` | watch ×N, audit ×1 (name-less), admission ×N (per object) |
| 10 | Owner-ref cascade | `configmap_scenarios_test.go` · `TestOwnerRefCascade` | `configmap/owner-ref-cascade/` | watch DELETED ×2 (parent + cascaded child), audit ×2 (parent = human, child = `generic-garbage-collector`) |
| 11 | Dry-run create | `configmap_scenarios_test.go` · `TestDryRunCreate` | `configmap/dry-run-create/` | audit, admission — **no** watch / **no** etcd object |
| 12 | Record-and-reject | `configmap_scenarios_test.go` · `TestRecordAndReject` | `configmap/record-and-reject/` | audit, admission — **no** watch / **no** etcd object |
| 13 | Optimistic-concurrency conflict | `configmap_scenarios_test.go` · `TestOptimisticConcurrencyConflict` | `configmap/conflict-update/` | audit ×1 (`update`, code 409) — **no** watch / **no** admission (rejected at storage, before admission) |
| 14 | Multi-version CRD conversion | `crd_conversion_test.go` · `TestCRDConversion` | `widget/crd-conversion/` | watch (v2), audit (v1), admission (v1), conversion ×2 (both directions) |
| 15 | Aggregated API write | `aggregated_api_test.go` · `TestAggregatedAPIWrite` | `flunder/aggregated-api-write/` | watch (full object), audit (empty body); admission is observed but not committed |
| 15a | Aggregated API delete | `aggregated_api_test.go` · `TestAggregatedAPIDelete` | `flunder/aggregated-api-delete/` | watch DELETED (full object), audit (`delete`, `objectRef` has the NAME from the URL but **no uid**) |
| 15b | Aggregated API deletecollection | `aggregated_api_test.go` · `TestAggregatedAPIDeletecollection` | `flunder/aggregated-api-deletecollection/` | watch ×N, audit ×1 (name-less, selector in `requestURI`, **no response body**), admission ×N (per object) |
| 16 | Watch resync (`410 Gone`) | `watch_transport_test.go` · `TestWatchExpiredResourceVersion` | `configmap/watch-resync/` | watch ERROR (`Status` 410); driver verifies relist recovery |
| 8a | Finalizer delete, two actors + tunable hold | `deletion_intent_actor_test.go` · `TestDeletionIntentActorRace` | `configmap/deletion-intent-actor/` | watch (MODIFIED + DELETED), audit (`delete` by the human + `patch` by a ServiceAccount) — both audit response bodies carry the **same** resourceVersion |
| 18 | `generateName` create | `configmap_scenarios_test.go` · `TestGenerateNameCreate` | `configmap/generate-name-create/` | watch, audit (`objectRef` has **no name**, response body **does**), admission — the control for rows 15a/15b |
| 17 | Bookmark | `watch_transport_test.go` · `TestWatchBookmark` | `configmap/watch-bookmark/` | watch BOOKMARK with resourceVersion |
| 19 | Label-selector membership | `selector_membership_test.go` · `TestSelectorMembership` | `configmap/selector-membership/` | selected watch (`watch.selected-*`, `team=a`) beside the unfiltered watch, audit and admission, three actors: a relabel out is selected DELETED carrying the **previous** labels at the write's resourceVersion (unfiltered MODIFIED); re-entry is ADDED with the same uid; an immediate delete's audit response is a `Status` naming the uid with **no** resourceVersion; a finalizer-held DELETED comes one resourceVersion **after** the delete and the finalizer patch; a relabel out followed by another actor's delete of the same uid leaves **one** selected DELETED (the exit) and a uid-only `Status` fact that did not cause it |

Row 8a is not a catalog row: it extends row 8 to the case the catalog could not express, because
row 8 drives both phases with one identity. A finalized deletion in production has TWO actors — a
human asks, a controller cleans up — and the row exists to measure what that does to attribution.
The finding is in the last column: the human's `delete` and the controller's finalizer `patch` both
return a body, and both bodies carry the resourceVersion the DELETION stamped, so the two facts they
produce collide on one key. `LAB_FINALIZER_HOLD` tunes the gap between the phases, which is what
decides whether both land in one audit batch. See
[the attribution spec](../../docs/spec/attribution.md).

All seventeen catalogued scenarios are now captured. Rows 15a and 15b are not catalog
rows: they extend row 15 to the removal verbs, because the create alone could not say
whether a proxied delete carries a uid (it does not) or whether a proxied
`deletecollection` returns a response body (it does not). Row 18 is their control: a
`generateName` create has an equally name-less `objectRef` and joins perfectly well,
because it carries a response body to recover the name from, which is what makes the
BODY rather than the name the thing the aggregated rows are actually missing. Rows 16
and 17 test the watch transport itself; the driver uses the lab's targeted `/watch-probe` endpoint so
transport-only events can be scenario-attributed — see the
[watch-first ingestion architecture](../../docs/architecture.md)
design notes.

> ⚠️ **The "no audit" rows record this cluster's audit POLICY, not the API server.** The lab cluster
> uses the e2e audit policy
> ([`test/e2e/cluster/audit/policy.yaml`](../e2e/cluster/audit/policy.yaml)), which drops `pods` and
> every `*/status` at `level: None` as runtime noise. So rows 5 and 7 show what a cluster configured
> like this one does not tell us — a `DELETE` on a pod is an audited request like any other when the
> policy asks for it. Read them as "excluded here", not as "unknowable". Re-capturing either row
> against a policy that includes those types would settle it by measurement.

## How it integrates: a cluster of its own

The lab runs on its own k3d cluster, `gitops-reverser-capture-lab`, which runs the lab, the
`wardle` sample aggregated API server, and nothing of the product
([design](../../docs/design/mutation-capture-lab-own-cluster.md)). Every source arrives the way it
does on any cluster:

- the API server's **audit webhook backend** posts to `https://127.0.0.1:30444/audit-webhook`, a
  NodePort on its own node, over mutual TLS: it presents a client certificate from the lab CA, and
  the lab refuses a connection without one;
- the `ValidatingWebhookConfiguration` `mutation-capture-lab` sends **admission** reviews to the
  lab's Service;
- the scenario Widget CRD's **conversion** webhook calls the same Service at `/convert`;
- the lab **watches** through its own ServiceAccount, with read-only RBAC on what it watches.

`lab-certs` issues the certificates before the cluster exists, so the audit kubeconfig the API
server reads at startup is already the final one and the cluster never restarts. The cluster is
created by the same [`start-cluster.sh`](../e2e/cluster/start-cluster.sh) as the e2e cluster, with
the same audit flags and policy, on one node, and without switching the current kube context.

Nothing in the lab's wiring names a product resource or depends on a product task;
[`selfcontained_test.go`](selfcontained_test.go) enforces that in `task test`. The lab used to swap
its image into the product's Deployment and borrow its webhook registration and certificates, and
lost its admission source the day the product removed them, with every check still green.

Row 15 (aggregated API write) is what settled the body-enrichment question: the
official audit event for an aggregated-API write carries an empty body, yet the
live watch carries the full object. Because the watch supplies the object content
natively, the supplementary `/audit-webhook-additional` body-enrichment proxy was
retired — so the lab no longer serves or records that endpoint.

## Running it (opt-in, serial)

These tasks are not part of `task test-e2e` or the per-PR CI lanes; the
[mutation lab workflow](../../.github/workflows/mutation-lab.yml) runs `lab-capture` nightly.

```bash
task lab-bootstrap      # certificates, cluster, image, deployment, and a live check of all five sources
task lab-capture        # run the scenarios and compare the capture to the corpus
task lab-corpus-update  # accept a new or changed capture as the corpus, CLUSTER.md included
task lab-status         # the cluster, its k3s version, the lab pod, and how many records it holds
task lab-cleanup        # delete the cluster and its stamps
```

The lab cluster can run beside the e2e cluster: its own name, stamps
(`.stamps/cluster/k3d-gitops-reverser-capture-lab/`), run lock, and records-API port (18081). It
needs no restore afterwards; the e2e cluster is never touched.

The unit tests for the lab packages run in the normal lane (`task test`); the
`test/mutationlab/e2e/` driver is behind the `mutationlab_e2e` build tag and runs only through the
lab's tasks.

## Validating a new Kubernetes version

```bash
task lab-cleanup
task lab-bootstrap LAB_K3S_IMAGE=rancher/k3s:v1.38.0-k3s1
task lab-corpus-update
git diff test/mutationlab/corpus/
```

The `corpus/` diff is the behavioral changelog for the upgrade, scoped to exactly the fine-grained
behaviors GitOps Reverser depends on. An empty diff is a positive result. Without `LAB_K3S_IMAGE`, a
new lab cluster runs the k3s image `CLUSTER.md` records, and `lab-capture` fails when the cluster
serves another version than the corpus was captured on.
