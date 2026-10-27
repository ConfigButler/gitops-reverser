# Corpus cluster provenance

A captured shape is only meaningful against a known apiserver version. This file
records the cluster the committed corpus under `test/mutationlab/corpus/` was
captured from. When the cluster version bumps, regenerating the corpus and
reviewing the diff *is* the changelog of "what changed in Kubernetes between
these versions" (see
[the design](../../../docs/spec/mutation-capture-lab-design.md#validating-new-kubernetes-versions)).

The lab runs on its own cluster, created on the k3s image below unless
`LAB_K3S_IMAGE` names another. `task lab-corpus-update` writes the three rows of
the table; `task lab-capture` fails when the cluster serves another version than
the one recorded here.

| Field | Value |
|---|---|
| k3d image | `rancher/k3s:v1.37.1-k3s1` |
| Server version | `v1.37.1+k3s1` (linux/amd64) |
| Captured at | 2026-10-27 |

Moving from `v1.36.1+k3s1` (captured 2026-06-25) to `v1.36.4+k3s1` changed one
committed shape: the garbage collector's `userAgent` in
`configmap/owner-ref-cascade/`. Every other scenario reproduced unchanged.

Recapturing on the lab's own cluster (2026-10-27, same version) changed one
field: each audit event of an impersonated actor in rows 8a and 19
(`configmap/deletion-intent-actor/`, `configmap/selector-membership/`) gained
`authenticationMetadata.impersonationConstraint: impersonate:user-info`. A plain
k3s v1.36.1 and v1.36.4 cluster, with an audit log backend instead of the lab,
records it for the same request. Every other scenario reproduced unchanged.

Moving from `v1.36.4+k3s1` to `v1.37.1+k3s1` (2026-10-27, the e2e cluster's
version) changed three shapes, each reproduced by a second capture:

- `configmap/deletecollection/`: the `deletecollection` response's list
  resourceVersion is now the last deleted object's own resourceVersion, where
  1.36 returned a newer one, so the watch DELETED placeholders shift by one;
- `pod/graceful-delete/`: the deleted pod's `PodReadyToStartContainers`
  condition now carries `reason: PodSandboxNotReady` and a message;
- `configmap/owner-ref-cascade/`: the garbage collector's `userAgent` names the
  new version.

Every other scenario reproduced unchanged.
