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
| `<release>-watch-any` **or** `<release>-watch-selected` | The types a `WatchRule` may read, `get`/`list`/`watch` only | The chart's `rbac.watchTypes`; for kustomize, [`config/rbac/watch-any-role.yaml`](../config/rbac/watch-any-role.yaml). |

They are separate because **RBAC is additive**. A wildcard read folded into the manager role
would grant cluster-wide Secret `list`/`watch` no matter how narrow the Secret rule beside
it is, and no chart value could take it back. Split out, the wildcard can be replaced.

## Choosing what the reverser may read

```yaml
rbac:
  create: true
  watchTypes:
    mode: any # any | selected
    selected: []
```text

**`mode: any`** (default) grants cluster-wide read on every resource. A `WatchRule` can name
any type, including one installed after the operator, and it will just work. The price is
that the reverser **can read every Secret in the cluster** — every git credential, every
token — even though it never asks for one it was not pointed at.

**`mode: selected`** grants read on exactly the types you list, and nothing else:

```yaml
rbac:
  watchTypes:
    mode: selected
    selected:
      - apiGroups: [""] # core group
        resources: ["configmaps"]
      - apiGroups: ["apps"]
        resources: ["deployments"]
```

Verbs are always `get`, `list`, `watch` — the reverser never writes to a watched type, so a
`verbs:` key is rejected rather than letting you grant one by accident. So is `mode:
selected` with an empty list, which would leave the operator able to watch nothing, and an
unknown key under `rbac`, which is how you think you narrowed access when you did not.

Those rules live in the chart's
[`values.schema.json`](../charts/gitops-reverser/values.schema.json), which Helm enforces on
`template`, `lint`, `install` and `upgrade`, naming the offending path:

```text
Error: values don't meet the specifications of the schema(s) in the following chart(s):
- at '/rbac/watchTypes/selected/0': additional properties 'verbs' not allowed
```

Do **not** restate `namespaces`, `customresourcedefinitions` or `apiservices`: the manager
role already carries them. A `WatchRule` naming a type you did not grant will fail to watch
it, so the list must cover every type your rules name.

### `namespaces` on a REMOTE source cluster

The manager role's `namespaces` `get`/`list`/`watch` covers the operator's **own** cluster. A remote
`ClusterProvider` whose `GitTarget`s declare a **selector-based** `allowedSourceNamespaces` — or whose
`WatchRule`s use `sourceNamespace: "*"` against one — needs the same for the identity in that
provider's kubeconfig, because the selector matches labels on `Namespace`s in the *source* cluster.

Exact `names` entries stay usable without it: a name-based policy, including a `"*"` item resolved
against a names-only policy, is answered from the API objects and never reads a `Namespace`. That is
a deliberate degradation path, not an oversight. When the source credential is forbidden from listing
Namespaces, a selector policy reports `SourceNamespaceAuthorized=False` with reason
`SourceNamespacePolicyUnavailable` (or `Unknown` while retaining an already-resolved scope) rather
than silently narrowing to nothing.

## The operator does not read Secrets wholesale

It holds **no Secret informer**: Secrets are excluded from the manager's cache
([`cmd/main.go`](../cmd/main.go), `Cache.DisableFor`), so every read is a direct `get` of a
Secret a `GitProvider` or `GitTarget` names. Nothing in the operator calls `list` or `watch`
on Secrets. It creates and updates only the Secrets it owns (the generated signing key).

That is why the manager role asks for `get`, `create`, `update` and nothing more.

## Running least-privilege

Set `mode: selected` and list your types. The chart renders the ClusterRole and its binding
for you — there is no hand-written role to maintain, and no way to drift from the
ServiceAccount the operator actually runs as.

Result: `GitProvider` Ready, `ClusterWatchRule` Ready with its streams, commits flowing, and
the reverser **cannot enumerate Secrets** — no `list`, no `watch`, on any namespace.

### What `selected` does not remove

Be precise about what is left, because RBAC is additive and the manager role is still bound:

| Verb on `secrets` | Still granted? | Why |
|---|---|---|
| `list`, `watch` | **No** | The operator never enumerates Secrets. Removed from the manager role, and `selected` adds no wildcard to put them back. |
| `get` | Yes, cluster-scoped | It reads the git-creds and age-key Secrets a `GitProvider`/`GitTarget` names — and those may live in any namespace. |
| `create`, `update` | Yes, cluster-scoped | It generates the signing key and, with `generateWhenMissing`, the age key. |

So a `selected` install can still **read a Secret whose name and namespace it already
knows**. It cannot discover one. That is a real reduction — enumeration is what turns a
mirroring operator into a credential-harvesting one — but it is not zero Secret access, and
this page will not pretend otherwise.

Scoping `get`/`create`/`update` down to the namespaces a `GitProvider` actually references
needs a namespaced `Role` the chart does not yet render. Until then the escape hatch is
`rbac.create: false` plus your own role set. See
[`future/least-privilege-remaining-work.md`](future/least-privilege-remaining-work.md).

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
