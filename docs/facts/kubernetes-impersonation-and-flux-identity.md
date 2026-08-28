# Impersonation and service-account identity, as Kubernetes and Flux implement them

> **facts** — durable reference. Index: [`../INDEX.md`](../INDEX.md)
>
> Read for [`../design/source-scope-simplification.md`](../design/source-scope-simplification.md),
> which decided against adopting impersonation. This page holds the evidence so the case does not
> have to be re-derived if the decision is re-opened.
>
> Verified against `k8s.io/apiserver@v0.34.1` in the module cache and the gitignored
> `external-sources/flux/` checkout. Both are named by path rather than linked, because neither is
> tracked by this repository.

## What the API server does

All five are from `pkg/endpoints/filters/impersonation.go`.

**The subject does not have to exist.** An `Impersonate-User` header of
`system:serviceaccount:homelab-config:mirror` is split by `serviceaccount.SplitUsername` into a
`ServiceAccount` object reference, and nothing ever reads that object. The identity is a string,
and the namespace half need not name a namespace that exists.

**The authorization check is namespaced and name-scoped.** The filter builds an attributes record
with verb `impersonate`, resource `serviceaccounts`, and both the namespace and the name taken from
the header. So an RBAC rule can bound impersonation with `resourceNames`. A `ClusterRole` doing so
bounds it by name only, across every namespace; bounding by namespace needs a `Role`.

**The service-account groups are added without a check.** When no groups are requested the filter
sets `groups = serviceaccount.MakeGroupNames(namespace)`, which is `system:serviceaccounts` and
`system:serviceaccounts:<namespace>`. Those are appended after the per-request authorization loop,
so no `impersonate` check runs against them. Any privilege the cluster binds to every service
account in that namespace comes along with the identity.

**The impersonator's own permissions are not an upper bound.** The filter constructs a fresh
`user.DefaultInfo` and replaces the request user with it; the original survives only in the audit
log. There is no intersection between what the impersonator may do and what the impersonated
subject may do.

> This last one is worth stating loudly, because the natural assumption is the opposite, and an
> earlier design review recorded that assumption as fact — that the operator's own `ClusterRole`
> remains the outer bound, so the effective permission is the intersection of what the operator may
> read and what the impersonated subject may read. It is not. The effective permission is
> exactly what the impersonated subject may do, which is why `resourceNames` scoping would be
> load-bearing rather than tidy: without it, `impersonate` on `serviceaccounts` is
> admin-equivalent in the target cluster.

**Impersonation is per request, so it applies to watches.** The header travels on a watch request
like any other, and when the binding behind it is removed the API server ends the stream. Whoever
reads under an identity the target cluster issued gets revocation without implementing it.

## What Flux does

From `pkg/runtime/client/impersonator.go` and
`flux-operator/internal/controller/resourceset_controller.go`:

- `setImpersonationConfig` builds `system:serviceaccount:%s:%s` from `i.serviceAccountNamespace`,
  and every caller passes the reconciled object's own namespace
  (`WithServiceAccount(r.DefaultServiceAccount, obj.Spec.ServiceAccountName, obj.GetNamespace())`).
  No field anywhere lets a user write the namespace half. That is the whole security argument: a
  tenant can claim only an identity bounded by the RBAC their own namespace already grants.
- `clientForKubeConfig` calls `setImpersonationConfig` too, so impersonation **composes** with a
  remote kubeconfig rather than replacing it.
- `CanImpersonate` does a `Get` for the `ServiceAccount` through the local client. On the
  kubeconfig path that checks the wrong cluster, and it is an existence check where an
  authorization check is wanted. RFC 0010 flags it as a known bug.

RFC 0001 states the opposite of the second point ("All accesses that would use impersonation use
the remote client instead"). The RFC predates the code. Cite the code.

## Impersonating into another cluster

RFC 0010 states the rule directly: with `spec.serviceAccountName`, the authenticated identity "must
have the necessary permissions to impersonate this `ServiceAccount` in the remote cluster".

Combined with the first fact above, this means the impersonated `ServiceAccount` is resolved in the
remote cluster and nowhere else, and the namespace in the identity string comes from the local
object. The pair is a **name convention two clusters agree on**, not an object reference. The
remote cluster's admin honors it with an ordinary `RoleBinding` naming a subject whose namespace
may not exist locally to them.

The consequence for any design that adopts this: whoever can create a namespace in the local
cluster can mint an identity claim against every remote cluster that ever granted that name.

## Where Flux puts multi-tenancy

Not in its API. `--no-cross-namespace-refs` and `--default-service-account` are controller flags,
and the workload-identity profile splits the latter into three by concern
(`--default-decryption-service-account`, `--default-kubeconfig-service-account`). The multi-tenant
lockdown is a documented Kyverno profile plus `flux-operator`'s
`internal/builder/profiles.go`. The one API-level exception, `acl.AccessFrom` with its
`namespaceSelectors`, is on sources and answers "which namespaces may reference this object".

## What adopting impersonation here would require

Recorded so the cost is not re-estimated:

- The tenant must pick the name half and never the namespace half, or they can name
  `kube-system:default`.
- `ClusterWatchRule` is cluster-scoped and has no namespace to derive from, so it needs an explicit
  `serviceAccountRef`. Acceptable only because creating a cluster-scoped object is already an admin
  act.
- The precheck is a `SubjectAccessReview` against the target cluster, not a `Get`, and it must
  carry `system:serviceaccounts` and `system:serviceaccounts:<namespace>` as groups or it answers
  narrower than reality. That needs `create subjectaccessreviews` on a credential users have
  already issued.
- Identity joins cluster, GVR, and namespace in the informer key, so it multiplies the resource the
  `TooManyStreams` cap exists to bound.
