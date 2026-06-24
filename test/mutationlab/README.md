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
