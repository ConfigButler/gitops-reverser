# Corpus cluster provenance

A captured shape is only meaningful against a known apiserver version. This file
records the cluster the committed corpus under `test/mutationlab/corpus/` was
captured from. When the cluster version bumps, regenerating the corpus and
reviewing the diff *is* the changelog of "what changed in Kubernetes between
these versions" (see
[the design](../../../docs/design/mutation-capture-lab-design.md#validating-new-kubernetes-versions)).

The lab reuses the main e2e cluster (`task lab-e2e` swaps the controller image),
so this provenance tracks the k3s image used for the last committed capture.
After the e2e cluster image changes, run `task lab-corpus-update` before changing
the values below.

| Field | Value |
|---|---|
| k3d image | `rancher/k3s:v1.35.2-k3s1` |
| Server version | `v1.35.2+k3s1` (linux/amd64) |
| Captured at | 2026-06-24 |
