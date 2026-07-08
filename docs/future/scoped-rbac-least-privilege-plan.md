# Scoped RBAC and least-privilege install

> Status: design plan — **not started** (this is Track B). Its runtime prerequisites
> (stop retaining Secret values, drop control-plane Secret watches, 5-minute reconcile
> fallback) shipped in PR
> [#208](https://github.com/ConfigButler/gitops-reverser/pull/208), so the runtime no longer
> *needs* Secret `list/watch` for controller-owned inputs. What remains is the packaging/RBAC
> work below: the default install still *grants* the broad Secret permission and the
> `*/* get,list,watch` wildcard.
> Date: 2026-07-07 (updated 2026-07-08).
> Roadmap: [Secret handling roadmap](secret-handling-roadmap.md).
> Related issue: [#205](https://github.com/ConfigButler/gitops-reverser/issues/205).

## Summary

This plan is about what the operator's ServiceAccount is allowed to read. It is separate
from the Secret cache cleanup in
[secret-value-retention-plan.md](secret-value-retention-plan.md), which reduces what the
process keeps in memory.

The key constraint is Kubernetes RBAC: permissions are additive. There is no deny rule and
no "all resources except Secrets" rule. The default install currently grants:

- core `secrets get,list,watch,create,update,patch`;
- core `namespaces get,list,watch`;
- `*/* get,list,watch` for dynamic watching.

While the wildcard exists, a namespaced Secret Role does not reduce the ServiceAccount's
cluster-wide Secret read permission. Real least privilege means running without the
wildcard and granting only the resources the install needs.

## What the reporter wants

The reported use case is narrower than the default product model:

- use one Git credential Secret in one namespace;
- do not mirror Kubernetes Secrets into Git at all;
- avoid cluster-wide Secret reads;
- keep SOPS encryption one-way and avoid private material on the write path.

The right answer is not only `--secret-namespaces`. That flag name also implies a scoped
value cache, which is not the target after the retention cleanup. The better plan is:

1. ✅ stop retaining Secret values in the controller process (PR #208);
2. ✅ drop control-plane Secret watches and use a 5-minute reconcile fallback (PR #208);
3. ⬜ add a mode that refuses to mirror Secrets;
4. ⬜ support BYO-RBAC without the wildcard;
5. ⬜ generate the narrow RBAC needed for a specific install.

## Current RBAC sources

The generated ClusterRole is in
[config/rbac/role.yaml](../../config/rbac/role.yaml) and mirrored into the chart at
[charts/gitops-reverser/config/role.yaml](../../charts/gitops-reverser/config/role.yaml).

Important grants:

```yaml
- apiGroups: [""]
  resources: ["namespaces"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["secrets"]
  verbs: ["create", "get", "list", "patch", "update", "watch"]
- apiGroups: ["*"]
  resources: ["*"]
  verbs: ["get", "list", "watch"]
```

Why they exist:

- The wildcard comes from the dynamic watch manager:
  [internal/watch/manager.go](../../internal/watch/manager.go#L33). It lets
  `WatchRule` and `ClusterWatchRule` select many current and future resource types without
  an RBAC redeploy.
- Secret reads support Git credentials, age encryption configuration, and signing keys.
  Secret list/watch is only needed for the current Secret reactivity watch and should go
  away in the retention plan.
- Secret writes support generated signing and age-key Secrets.
- namespace reads are needed for namespaced rule resolution.

## Supported install modes

### Default mode

Keep the generated ClusterRole and wildcard.

Good for convenience and broad discovery. Not least privilege.

### Hardened static mode

Operators set:

```yaml
rbac:
  create: false
serviceAccount:
  create: false
  name: existing-service-account
```

They bind that ServiceAccount to a hand-written or generated role set. This already works
at the packaging layer. What is missing is documentation and better runtime behavior when
a rule selects something not granted.

### Future dynamic mode

The operator reconciles its own Roles and RoleBindings from active rules. This is not a
near-term target. It requires RBAC write permission, privilege-escalation safeguards, and
an explicit opt-in.

## Plan

### 1. Document BYO-RBAC

Write an operator-facing recipe for hardened static installs:

- disable chart-created RBAC and ServiceAccount;
- create a ServiceAccount owned by the platform team;
- grant fixed controller permissions;
- grant variable watch permissions based on the install's rules;
- document the trade-off: no wildcard means no automatic access to new resource types.

Fixed permissions include:

- GitOps Reverser CRDs, `/status`, and `/finalizers`;
- leader-election leases only if leader election is enabled. It is not enabled today, and
  the generated role does not currently grant `coordination.k8s.io/leases`;
- events if emitted;
- namespace reads for namespaced rule resolution;
- scoped Secret `get` for referenced Git credentials and age-key inputs;
- scoped Secret writes for generated signing and age-key material;
- Secret `list/watch` only if optional fast-rotation metadata watches are enabled.

Variable permissions include:

- one `get,list,watch` rule per selected GVR;
- Role when every selection is namespace-scoped;
- ClusterRole when the selected resource or selection scope is cluster-wide.

### 2. Add "never mirror Secrets"

Add an exclusion policy, for example:

```text
--exclude-resources=secrets
```

or a structured counterpart to `SensitiveResourcePolicy`.

Behavior:

- Secrets are refused by the followability funnel;
- `WatchRule` or `ClusterWatchRule` status explains the refusal;
- the dynamic target watch path never opens a Secret watch;
- generated RBAC can omit mirrored-Secret read grants.

This serves the reporter's strongest requirement: Git credentials remain controller input,
but cluster Secrets are not mirrored output.

### 3. Make followability permission-aware

The current funnel checks discovery-advertised verbs, not the ServiceAccount's actual
authorization:
[internal/typeset/funnel.go](../../internal/typeset/funnel.go#L16).

With narrow RBAC, a type can look followable in discovery and then fail with `403
Forbidden` when the dynamic watch opens. That should become a clean refusal.

Implementation shape:

- collect effective permissions for relevant namespaces and cluster scope;
- prefer `SelfSubjectRulesReview` for bulk rules;
- use `SelfSubjectAccessReview` as a fallback for ambiguous wildcard or aggregated cases;
- add permitted verbs to the type observation;
- fail the verbs requirement with a new reason such as `ReasonNotPermitted`;
- surface the reason on `WatchRule` and `ClusterWatchRule` status.

This is not required to remove Secret values from the cache, but it is required to make
least-privilege installs pleasant and diagnosable.

### 4. Add an RBAC generator

Add a command or task that reads manifests or live cluster state and emits the narrow role
set for:

- `GitProvider`;
- `GitTarget`;
- `WatchRule`;
- `ClusterWatchRule`.

Output:

- fixed controller grants;
- Secret input grants for referenced Git credentials and age-key Secrets;
- generated Secret write grants where `generateWhenMissing` or signing-key generation can
  create/update Secrets;
- one watch grant per selected GVR;
- no `*/*` wildcard when `--no-wildcard` is selected.

Start with manifest input. It is reviewable in CI and does not require cluster access.
Live-cluster input can come later.

### 5. Use a 5-minute control-plane reconcile fallback

The hardened default should not need Secret watches for controller-owned inputs. Instead,
control-plane controllers should use a common 5-minute periodic reconcile:

- credential or age-key rotation is picked up within 5 minutes if no related work happens
  sooner;
- branch workers still read credentials directly when work is happening;
- generated-key recovery and recipient annotation refresh can wait for the next GitTarget
  reconcile;
- RBAC for controller-owned Secret inputs can drop `list/watch` and keep scoped `get`.

This does not affect data-plane mirroring watches. Those still need `get,list,watch` for
the resources the user chooses to mirror.

### 6. Optional: namespace-scope Secret metadata watches

After [secret-value-retention-plan.md](secret-value-retention-plan.md) switches to direct
reads and a 5-minute reconcile fallback, metadata watches can be added back only for
installs that need faster Secret rotation. Avoid adding a public flag first if a generated
static namespace set is enough.

```text
--secret-watch-namespaces=ns1,ns2
```

If a flag is needed, prefer `--secret-watch-namespaces` over the issue's
`--secret-namespaces`: it says exactly what is scoped. This should not create a full
Secret value cache. It only scopes rotation reactivity. The matching RBAC still needs
normal Secret `get,list,watch` permissions in those namespaces.

## What not to do

- Do not claim metadata-only watch lowers RBAC. It does not.
- Do not reintroduce a namespace-scoped full Secret cache as the primary fix.
- Do not require a Secret metadata watch by default when a 5-minute reconcile is enough.
- Do not claim a namespaced Secret Role helps while `*/* get,list,watch` is still bound.
- Do not replace the default wildcard install for everyone.
- Do not use `resourceNames` as the main model for Secret watches; list/watch and rotation
  behavior make that brittle.

## Done

This plan is complete when:

- hardened static install docs exist;
- users can opt out of mirroring Secrets;
- missing watch permissions surface as rule status, not unexplained watch churn;
- an RBAC generator can produce a no-wildcard role set for a concrete install;
- public docs clearly distinguish RBAC blast radius from in-process Secret exposure.
