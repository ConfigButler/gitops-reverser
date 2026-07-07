# Secret handling roadmap

> Status: roadmap.
> Date: 2026-07-07
> Related issue: [#205](https://github.com/ConfigButler/gitops-reverser/issues/205).
> Detailed plans:
> [Secret value retention](secret-value-retention-plan.md) and
> [Scoped RBAC and least-privilege install](scoped-rbac-least-privilege-plan.md).

## Why this exists

The issue asks for `--secret-namespaces=ns1,ns2` so the operator stops watching every
Secret in the cluster. That request is directionally right, but it mixes three different
problems:

| Axis | Question | Main fix |
|---|---|---|
| Process exposure | What Secret values does the controller hold in memory? | Direct Secret reads only |
| Rotation reaction | How fast do changed credentials or age keys take effect? | 5-minute reconcile fallback |
| RBAC blast radius | What is the ServiceAccount allowed to read? | Drop the wildcard and generate narrow RBAC |

Those fixes should not be shipped as one large feature. The first two are small and give
the biggest immediate security win. RBAC narrowing is a separate install mode with a real
trade-off: no wildcard means no automatic access to newly selected resource types.

The important update: control-plane Secret watches are not required. Mirroring still needs
data-plane watches for selected resources, and controllers should still watch our own CRDs.
But Git credentials and age-key inputs can be read directly and refreshed by a 5-minute
periodic reconcile instead of a Secret informer.

## Current state

The operator has three Secret paths today:

- `GitTarget` registers a full-object Secret watch to react to generated age Secret
  changes. With the default controller-runtime cache, that means every Secret value in
  every namespace can be held in memory.
- `GitProvider`, branch workers, encryption setup, and signing-key helpers read specific
  Secrets through the cached client.
- Mirrored Secrets selected by a `WatchRule` or `ClusterWatchRule` use the dynamic watch
  path. That path is about user-selected resources, not the controller's own credential
  inputs.

The default chart also grants `*/* get,list,watch` for dynamic watching. Because
Kubernetes RBAC is additive, a namespaced Secret Role does not reduce the ServiceAccount's
Secret read permission while that wildcard remains.

## Decision

Treat this as two tracks.

**Track A: stop retaining Secret values.** This is the first implementation track. It
keeps the default broad install behavior, but changes the process footprint:

- delete control-plane Secret watches instead of converting them to metadata;
- bypass the cache for typed Secret reads;
- read referenced Secret values directly by name;
- standardize control-plane periodic reconciles at 5 minutes;
- remove private age identities from the SOPS encrypt path.

This solves the immediate "the process caches every Secret value forever" problem and
removes the need for Secret list/watch on controller-owned inputs in the default mode.

**Track B: support narrow installs.** This is the install/RBAC track:

- document bring-your-own RBAC;
- add a "never mirror Secrets" policy;
- make followability permission-aware so missing grants become status refusals rather than
  runtime watch churn;
- add a generator for minimal Roles and ClusterRoles.

This is what reduces what the ServiceAccount is allowed to read.

## Recommended order

1. **Stop value caching and drop Secret dependency watches.**
   Implement the small controller/client changes in
   [secret-value-retention-plan.md](secret-value-retention-plan.md). This makes Secret
   `list/watch` unnecessary for controller-owned inputs; packaging cleanup follows in the
   RBAC track.
2. **Remove private age identity handling from the encrypt path.**
   Keep SOPS encryption public-recipient only. This is a small deletion with a strong
   security payoff.
3. **Set control-plane reconcile fallback to 5 minutes.**
   Use 5 minutes as the common periodic reconcile cadence for control-plane controllers
   after removing Secret watches.
4. **Add `--exclude-resources=secrets` or equivalent.**
   This lets an install say: Git credentials are controller input; Kubernetes Secrets are
   not mirrored output.
5. **Document BYO-RBAC and add permission-aware followability.**
   This makes hand-narrowed RBAC predictable.
6. **Add an RBAC generator.**
   Generate the narrow role set from `GitProvider`, `GitTarget`, `WatchRule`, and
   `ClusterWatchRule` manifests.
7. **Optional: add Secret metadata watches for faster rotation.**
   Do this only if the 5-minute fallback is not responsive enough. Prefer a namespace set
   derived from manifests or generated Helm values before adding a runtime flag.

## Non-goals

- Do not replace the default wildcard install for everyone. The default remains convenient.
- Do not reintroduce a full Secret value informer just to support a namespace flag.
- Do not add a Secret metadata watch by default; direct reads plus a 5-minute reconcile are
  the simpler baseline.
- Do not add a namespace flag before proving generated or inferred configuration is not
  enough.
- Do not add a long-lived in-process Secret value TTL cache unless an install measures API
  pressure and explicitly opts in.
