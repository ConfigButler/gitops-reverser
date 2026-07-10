# Least privilege: what is left

> Status: three open items. Everything else from the old Secret-handling/RBAC plans has shipped
> and now lives in [`../rbac.md`](../rbac.md) and [`../security-model.md`](../security-model.md).
> Origin: issue [#205](https://github.com/ConfigButler/gitops-reverser/issues/205).

## Already shipped (do not re-plan)

The controller holds no Secret values (cache `DisableFor`), runs no control-plane Secret watch,
refreshes on a 5-minute steady reconcile, and passes only public age recipients to SOPS. The
wildcard cluster read moved out of the manager ClusterRole into `rbac.watchTypes`, so an install
can grant read on named types only, and the manager role's Secret verbs are down to
`get,create,update`.

**The one constraint that shapes everything below:** Kubernetes RBAC is additive. There is no deny
rule and no "everything except Secrets". While `mode: any` is bound, no namespaced Secret `Role`
reduces anything.

## 1. Never mirror Secrets

An install should be able to say: *git credentials are controller input; cluster Secrets are not
mirrored output*. Today a `WatchRule` naming `secrets` is honoured.

Shape: an exclusion policy (`--exclude-resources=secrets`, or a structured counterpart to
`SensitiveResourcePolicy`) that makes the followability funnel refuse the type, explains the refusal
on `WatchRule`/`ClusterWatchRule` status, and never opens the watch. Generated RBAC can then omit
the grant entirely.

## 2. Permission-aware followability

The funnel checks the verbs **discovery advertises**, not the verbs this ServiceAccount **holds**
([`internal/typeset/funnel.go`](../../internal/typeset/funnel.go)). Under `mode: selected` a type
can look followable and then `403` when the watch opens.

Shape: collect effective permissions (`SelfSubjectRulesReview` in bulk, `SelfSubjectAccessReview`
for ambiguous wildcard/aggregated cases), add permitted verbs to the type observation, and fail the
verbs requirement with a `ReasonNotPermitted` surfaced on rule status.

This is what makes a narrowed install diagnosable instead of mysterious. The API-surface trigger
informers already do the honest thing on `403` — they stop and log once, and re-arm when the
permission is granted — but watched types still churn.

## 3. Scope the Secret grant, and an RBAC generator

`secrets: get,create,update` is cluster-scoped because a `GitProvider` may reference a Secret in any
namespace. A `selected` install therefore cannot *enumerate* Secrets but can still read one it can
name. Closing that needs a namespaced `Role` per referenced namespace, which the chart cannot render
without knowing the manifests.

Which is the same input an **RBAC generator** needs: read `GitProvider`, `GitTarget`, `WatchRule`
and `ClusterWatchRule` manifests and emit the narrow role set — fixed controller grants, Secret
`get` scoped to referenced namespaces, Secret write grants only where `generateWhenMissing` or
signing-key generation applies, and one `get,list,watch` per selected GVR. Manifest input first: it
is reviewable in CI and needs no cluster access.

Until it exists, the escape hatch is `rbac.create: false` with a hand-written role set.

## What not to do

- Do not claim a metadata-only Secret watch lowers RBAC. Kubernetes has no metadata-only Secret
  permission; it still needs `get,list,watch`.
- Do not reintroduce a namespace-scoped Secret **value** cache to satisfy a namespace flag.
- Do not add a Secret watch by default. Direct reads plus the 5-minute reconcile are the baseline;
  add `--secret-watch-namespaces` only if measured rotation latency demands it, and never let it
  become a value cache.
- Do not use `resourceNames` as the model for Secret watches: `list`/`watch` and rotation make it
  brittle.
- Do not remove the `any` default. It is the convenient path, and it is honest about its cost.
