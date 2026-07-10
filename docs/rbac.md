# RBAC: running the reverser without trusting it with the cluster

The reverser ships able to read every object in the cluster, because a `WatchRule` may name
any type — including a type installed after the operator. That default is honest, not
required. This page shows how to run it with the read access it actually uses, which on a
management cluster is the difference between "mirrors two CRDs" and "can read every
credential we own".

## The two ClusterRoles

| ClusterRole | Contents | Where it comes from |
|---|---|---|
| `<release>-manager-role` | Exactly what the binary calls: its own CRs, `namespaces`, `customresourcedefinitions`, `apiservices`, and `secrets` with **`get`, `create`, `update`** | Generated from the kubebuilder markers into [`config/rbac/role.yaml`](../config/rbac/role.yaml). Never edit by hand. |
| `<release>-watch-any-resource` | `apiGroups: ["*"], resources: ["*"], verbs: ["get","list","watch"]` | Hand-maintained: [`config/rbac/watch-any-resource-role.yaml`](../config/rbac/watch-any-resource-role.yaml), or the chart's `rbac.watchAnyResource` (default `true`). |

They are separate because **RBAC is additive**. A wildcard read folded into the manager role
would grant cluster-wide Secret `list`/`watch` no matter how narrow the Secret rule beside
it is, and no chart value could take it back. Split out, the wildcard can be dropped whole.

## The operator does not read Secrets wholesale

It holds **no Secret informer**: Secrets are excluded from the manager's cache
([`cmd/main.go`](../cmd/main.go), `Cache.DisableFor`), so every read is a direct `get` of a
Secret a `GitProvider` or `GitTarget` names. Nothing in the operator calls `list` or `watch`
on Secrets. It creates and updates only the Secrets it owns (the generated signing key).

That is why the manager role asks for `get`, `create`, `update` and nothing more.

## Running least-privilege

Set `rbac.watchAnyResource: false` and grant read on the types you actually mirror:

```yaml
# helm values
rbac:
  create: true
  watchAnyResource: false
```

```yaml
# your own ClusterRole, bound to the reverser's ServiceAccount
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: gitops-reverser-watched-types
rules:
  # exactly the types your WatchRules name
  - apiGroups: ["gitops-api.configbutler.ai"]
    resources: ["tenants", "reposelections"]
    verbs: ["get", "list", "watch"]
```

The manager role already carries `namespaces`, `customresourcedefinitions` and `apiservices`,
so you do not restate them. Bind your role to the same ServiceAccount, alongside the manager
role's binding.

Result: `GitProvider` Ready, `ClusterWatchRule` Ready with its streams, commits flowing, and
**zero cluster-wide Secret access**.

If the reverser needs a Git credentials Secret in its own namespace, that is a namespaced
`Role`, not a `ClusterRole` — the manager role's `get` on `secrets` is cluster-scoped only
because a `GitProvider` may point at any namespace. A namespaced binding is enough when it
does not.

## What happens if a read is denied

Discovery answers what the API **server serves**, not what this ServiceAccount may **read**.
An ordinary apiserver serves `apiregistration.k8s.io` whether or not your ClusterRole names
`apiservices`, so a narrowed role sails past the "is it served?" check and then gets a `403`
on the informer's first `LIST`.

For the two **API-surface trigger** informers (`customresourcedefinitions`, `apiservices`)
the operator treats a `403` as authoritative: it stops that informer, logs once, and falls
back to refreshing its API-resource catalog on the periodic tick. It does **not** retry the
denial forever, which would bury real errors under benign noise. Grant the permission later
and the next catalog refresh re-arms the informer — no restart.

These two are conveniences: they only make the catalog notice a new CRD or aggregated API
sooner than the periodic refresh would. Everything else keeps working without them, which is
why failing closed and quiet is the right answer to "you may not read this".

A denied **watched type** is different: that is the job you asked for, not a convenience, so
the operator keeps retrying it rather than quietly dropping it. Its watch stream errors and
the manager retries with backoff. If you narrow the role, make sure every type your
`WatchRule`s name is granted, and check the operator logs after the change.
