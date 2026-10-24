# One holder per applier, across `KubeProvider`s

Status: **idea**, nothing built. Two ways to close a known gap, and a third model for consent,
recorded from the applier claim rename plan (#438) and review 7 of #420.

## The gap

[`holder.go`](../../internal/applier/holder.go) (`sameApplier`) decides that two applier claims
name the same applier by `ClaimKind()`, `KubeProviderKey()`, and the applier's namespace and name.
Two `KubeProvider`s that reach **one** cluster, for example the chart's `local` and a team's own
provider with a kubeconfig for the same API server, therefore give two holders for one
`Kustomization`. Both suspend it under the same field manager. Each reads the other's resume as a
`flux resume` and suspends it mid-apply. Deleting one hands back a hold the other still needs. For
an `Application`, both start syncs.

This is a misconfiguration, not an attack, so the decision not to design against spoofed Flux or
Argo CD identity does not cover it. It is documented today (O1): an applier claim is exclusive only
among claims made through the same `KubeProvider`
([definitions.md](../definitions.md#applying-back),
[configuration.md](../configuration.md#who-applies-this-folder-specapplierclaimref)).

## O2: compare cluster identity (partial)

- The `KubeProvider` records the cluster's identity in status, the `kube-system` Namespace UID read
  once at validation, and `sameApplier` compares that instead of the provider key.
- Where the credential cannot read that Namespace, it falls back to the provider key, and the O1
  limitation still applies.
- Cost: `get` on `namespaces/kube-system`, plus a note in
  [architecture.md](../architecture.md) (which says the operator "reads no `Namespace` objects in a
  source cluster") that this read is about identity, not authorization.

## O3: an object-local stamp (complete)

- Write the holder's identity into the `Kustomization` in the same server-side apply as the
  suspend, and refuse to adopt over a live stamp that is not ours. This also catches two operator
  installs on one cluster.
- Cost: two first adopters can race, so arbitration has to work across operators, and an
  `Application` needs a write of its own.

## Consent on the applier (`ReferenceGrant` style)

Who may claim an applier is decided today by the `KubeProvider`'s `allowedApplierClaims` grants,
which name the applier namespaces a consumer namespace may reach. Review 7 of #420 weighed a
stronger model: consent on the `Kustomization` or `Application` itself, in the shape of Gateway
API's `ReferenceGrant`, so an applier's owner says which namespaces may hold it. It is the best
long-term model, and it is worth building only if delegation per provider proves too coarse.
