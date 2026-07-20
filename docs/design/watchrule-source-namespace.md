# Addressing a source namespace from a WatchRule

> **design** — nothing here is built. Companion to upstream wishlist #14 and
> [config-plane-split.md](../finished/config-plane-split.md).
> Written 2026-07-19, revised 2026-07-20. Index: [INDEX.md](../INDEX.md).

## Expected use cases

1. **Nothing changes for today's rules.** A WatchRule with no sourceNamespace continues to watch
   the namespace containing the rule.
2. **A tenant's configuration namespace and source namespace differ.** A WatchRule in tenant-acme
   selects repo-config in its workspace. This removes the collision caused by a shared config
   plane.
3. **One source cluster serves several tenants.** Each GitTarget declares the source namespaces
   that its WatchRules may select. tenant-acme's target can select its workspace namespace without
   giving tenant-zen's target any new scope.
4. **An in-cluster source is deliberately cross-namespace.** This is an explicit platform-admin
   delegation through the operator's cluster RBAC. The mechanism is the same for remote and
   in-cluster providers, but the in-cluster sign-off is especially sensitive.

## The model

The authorization follows the existing object references:

~~~text
WatchRule  ──uses──>  GitTarget  ──uses──>  ClusterProvider
    │                     │                       │
    │                     │                       └ permits this target namespace
    │                     └ permits source namespaces for its WatchRules
    └ requests one source namespace
~~~

- **WatchRule.spec.sourceNamespace** is the requested source namespace. When omitted, its effective
  value is the WatchRule's namespace.
- **GitTarget.spec.allowedSourceNamespaces** is the target owner's allow-list for the source
  namespaces that WatchRules using this target may request.
- **ClusterProvider.spec.allowedNamespaces** remains the existing platform-admin policy: it permits
  a control-cluster namespace to create GitTargets that use the provider.
- **ClusterProvider.spec.allowWatchRuleSourceNamespaceOverride** is a new, false-by-default
  platform-admin delegation. When false, a WatchRule may use only its own namespace; when true, a
  GitTarget using this provider may grant a different source namespace through
  allowedSourceNamespaces.

The three objects therefore have distinct responsibilities: the rule asks, the target scopes that
target's work, and the provider delegates use of its source-cluster credential.

## Example

~~~yaml
apiVersion: configbutler.ai/v1alpha3
kind: ClusterProvider
metadata:
  name: workspaces
spec:
  kubeConfig:
    secretRef:
      name: workspaces-kubeconfig

  # Existing gate: who may create a GitTarget using this provider.
  allowedNamespaces:
    selector:
      matchLabels:
        gitops.configbutler.ai/workspace-tenant: "true"

  # New gate: an admitted GitTarget may authorize source-namespace overrides.
  allowWatchRuleSourceNamespaceOverride: true
---
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: acme
  namespace: tenant-acme
spec:
  providerRef:
    name: acme-git
  branch: main
  path: tenants/acme
  clusterProviderRef:
    name: workspaces

  # Source-cluster namespaces that WatchRules using this target may select.
  allowedSourceNamespaces:
    names: [repo-config]
    selector:
      matchLabels:
        gitops.configbutler.ai/mirrorable: "true"
---
apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: repo-config
  namespace: tenant-acme
spec:
  targetRef:
    name: acme
  sourceNamespace: repo-config
  rules:
    - resources: [configmaps]
~~~

The WatchRule is accepted because tenant-acme may use the provider, the provider delegates source
namespace overrides, and the GitTarget allows repo-config. A GitTarget in tenant-zen has its own
allowedSourceNamespaces policy, so it does not inherit the acme target's selection scope.

## Why this belongs on GitTarget

GitTarget already binds one source cluster to one Git destination, branch, and path. The selected
source namespaces determine what arrives at that destination, so the target is the natural place
to declare them. A reader can follow the same chain the controller follows:

~~~text
WatchRule.sourceNamespace
  → WatchRule.targetRef
  → GitTarget.allowedSourceNamespaces
  → GitTarget.clusterProviderRef
  → ClusterProvider.allowedNamespaces and delegation flag
~~~

Putting both allowed properties on ClusterProvider instead is attractive, but they become two
unrelated unions. A provider that admits tenant-acme and tenant-zen, then lists acme-config and
zen-config, cannot express which tenant owns which source namespace. A provider-side pair list can
represent that relationship, but duplicates the per-target source scope and makes the ordinary
reference path harder to read.

The GitTarget model works because a target is already namespaced: all current WatchRules that use
it are in that namespace. No cross-namespace GitTarget reference is added or implied.

## What the provider delegation means

The boolean is no longer redundant once the source allow-list lives on GitTarget. It does not grant
access by itself: the relevant GitTarget must still list or select the requested namespace. Instead
it says that the platform admin delegates source-namespace selection to admitted GitTargets.

That is a meaningful authority grant. A target owner can configure a broad selector, including one
that matches every source namespace, so the source cluster's kubeconfig RBAC remains the hard
maximum. Set the boolean only when the owner of an admitted GitTarget is trusted to choose a subset
of what that credential may read.

For a remote source this is normally a clear delegation: the remote credential already controls
what can be read. For an in-cluster provider it can turn the operator's broad cluster RBAC into a
cross-namespace read path. Keeping the flag false by default makes that power explicit.

If a platform instead needs a platform-admin-owned, per-tenant maximum source namespace policy, this
model alone is intentionally insufficient. Use one ClusterProvider and source credential per tenant,
or add a later provider-side pair policy. Do not silently treat GitTarget's policy as a substitute
for that stronger boundary.

### Locality is not the switch

A ClusterProvider is in-cluster when kubeConfig is omitted and remote when it is set. The name
default is only the value of an omitted GitTarget clusterProviderRef. Neither decides authorization;
only the explicit delegation flag and policies do.

## Names and labels behave the same way

Both namespace policies use one generic NamespaceMatcher shape:

| Field | Labels come from | Meaning |
|---|---|---|
| ClusterProvider.allowedNamespaces | control cluster | Which namespaces may create a GitTarget using the provider. |
| GitTarget.allowedSourceNamespaces | source cluster | Which namespaces WatchRules using this target may select. |

For both fields:

- names and selector are ORed;
- an omitted or empty matcher admits nothing;
- a name match needs no Namespace label read;
- a selector matches labels on the Namespace object in the field's respective cluster.

The source policy is scoped to one GitTarget, not a shared provider. It does not need an SSA map
list; the existing names set remains a set, and the selector remains one object.

## Reactivity and source-cluster RBAC

Policy changes must grant and revoke access promptly, not merely when a WatchRule happens to be
edited:

| Input changes | Required reaction |
|---|---|
| GitTarget allowedSourceNamespaces | Reconcile its WatchRules. |
| ClusterProvider allowedNamespaces or delegation flag | Reconcile affected GitTargets and their WatchRules. |
| Control-cluster Namespace labels | Reconcile affected GitTargets. |
| Source-cluster Namespace labels | Reconcile WatchRules whose GitTarget uses a source selector. |

The watch manager already owns source-cluster watch lifecycles. This design adds one
label-filtered Namespace informer per active source cluster, not one per WatchRule. It emits only
meaningful label changes and maps them to WatchRules whose GitTarget resolves through that source
cluster.

The in-cluster manager role already grants:

~~~yaml
apiGroups: [""]
resources: [namespaces]
verbs: [get, list, watch]
~~~

A remote provider used with a GitTarget source selector needs the same permissions for the identity
in its kubeconfig. GET supplies current labels during reconciliation; LIST and WATCH keep selector
grants and revocations current. If the permissions are absent or the watcher cannot be established,
selector-based overrides fail closed with SourceNamespacePolicyUnavailable. Exact-name entries
remain usable without source Namespace access.

On any refusal or revocation, the controller removes the compiled rule from RuleStore, replans the
watch manager to stop its stream, and then reports Ready=False and Stalled=True with reason
SourceNamespaceNotAllowed or SourceNamespacePolicyUnavailable. WatchRule does not currently
publish a Validated condition, so this design uses its existing Ready/Stalled contract.

## Reconciliation and validation

The effective source namespace is controller logic; an API-server default cannot refer to
metadata.namespace:

~~~go
effectiveSourceNamespace := rule.Spec.SourceNamespace
if effectiveSourceNamespace == "" {
    effectiveSourceNamespace = rule.Namespace
}
~~~

The legacy case needs no new authorization: if the effective source namespace equals the WatchRule
namespace, the rule continues to work with no delegation flag and no GitTarget policy. An explicit
different namespace requires all three checks:

1. the GitTarget's namespace is admitted by the ClusterProvider;
2. the ClusterProvider delegation flag is true; and
3. the GitTarget policy admits the effective source namespace.

This is cross-object authorization, and source selectors also require remote state. It is therefore
a reconcile-time check, not CEL or an admission webhook. The check runs before
RuleStore.AddOrUpdateWatchRule.

sourceNamespace is optional with MinLength=1 when present. GitTarget.allowedSourceNamespaces is an
optional deny-by-default NamespaceMatcher. The generated CRD descriptions must update the current
statement that WatchRule watches only its own namespace.

## Routing remains source-native

No write-path change is required. Events already carry the namespace of the source object, which
feeds its resource identity and Git placement. sourceNamespace changes the watched namespace; it
must not substitute the WatchRule's control-cluster namespace into the Git path.

## Alternatives, briefly

- **No delegation flag:** rejected. A GitTarget policy would otherwise silently turn provider access
  into cross-namespace source reachability.
- **Provider-wide allowedSourceNamespaces:** useful only for a one-tenant provider. With several
  tenants it becomes a Cartesian product and cannot express ownership.
- **Provider-side pair policies:** can enforce platform-owned per-tenant maxima, but are more
  complex and less followable than the GitTarget model. Reserve this for a later stronger-boundary
  requirement.
- **Remote-only override:** rejected. kubeConfig is connectivity, not permission.
- **ClusterWatchRule selector:** remains a separate, cluster-admin-owned feature.

## Implementation sketch

1. Generalize the current names-or-selector helper into a reusable NamespaceMatcher.
2. Add WatchRule.spec.sourceNamespace, GitTarget.spec.allowedSourceNamespaces, and
   ClusterProvider.spec.allowWatchRuleSourceNamespaceOverride. Generate CRDs and deepcopy code.
3. Carry effectiveSourceNamespace through CompiledRule, watched-type selections, and their
   fingerprints.
4. Add the three-part reconciler gate before RuleStore.AddOrUpdateWatchRule. On failure, remove any
   existing compiled rule and replan the watch manager.
5. Map GitTarget and ClusterProvider changes to affected WatchRules, and add the per-source-cluster
   Namespace label watcher.
6. Test legacy same-namespace behavior; flag disabled; target exact-name allow and deny; source and
   control label grant/revocation; target isolation; missing remote Namespace RBAC; compiled-rule
   removal; and fingerprint changes.

## Compatibility and rollout

This is additive for v1alpha3. Existing WatchRules omit sourceNamespace and continue to select their
own namespace. Existing GitTargets require no policy until a WatchRule requests an override.

Roll out the controller before permitting manifests that use the new fields. An older controller
would decode sourceNamespace as absent and continue watching the rule's own namespace, which is safe
but surprising. No conversion webhook or storage migration is needed.
