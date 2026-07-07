# Installing apps is adding a KRM document

GitOps Reverser mirrors and edits **Kubernetes Resource Model (KRM) documents**.
It does not care whether a document is a core `ConfigMap`, an `apps/v1`
`Deployment`, or a higher-level control-plane object such as a Flux
`HelmRelease`, an Argo CD `Application`, or a [KRO](https://kro.run) resource.
They are all just documents with an `apiVersion`, `kind`, and `metadata`, and the
operator treats them identically.

That gives you two everyday workflows without learning the repository's layout:

- **Install an app** → apply the KRM document to the cluster. The operator writes
  it to Git, in the folder your existing files already use.
- **Roll out a new version** → edit one field on the live object (a chart
  version, an image tag, a revision). The operator writes that change back into
  the *same* file, in place, preserving your comments and formatting.

## What counts as "an app"

Anything you would normally `kubectl apply`. In KRM terms, installing an app is
adding one or more documents:

| You want to… | The KRM document you add |
|---|---|
| Run a workload directly | a core `Deployment` / `StatefulSet` (+ `Service`, `ConfigMap`) |
| Install a Helm chart via Flux | a `helm.toolkit.fluxcd.io/v2` `HelmRelease` (+ its `HelmRepository`/`OCIRepository` source) |
| Deploy via Argo CD | an `argoproj.io/v1alpha1` `Application` |
| Use a platform abstraction | a KRO instance (e.g. `kro.run/v1alpha1` `PodInfoApp`) or any CRD your platform provides |

### Example: a Flux HelmRelease

```yaml
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: podinfo
  namespace: apps
spec:
  interval: 30m
  chart:
    spec:
      chart: podinfo
      version: "6.5.0" # bump this to roll out a new version
      sourceRef:
        kind: HelmRepository
        name: podinfo
        namespace: flux-system
```

Apply it and the operator commits it. Later, change `version` to `6.6.0` on the
live object (`kubectl patch`, your platform, or any controller) and the operator
edits that one line in the committed file — your comment on it stays put.

### Example: an Argo CD Application

```yaml
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: guestbook
  namespace: argocd
spec:
  project: default
  source:
    repoURL: https://github.com/argoproj/argocd-example-apps.git
    path: guestbook
    targetRevision: "1.4.0" # bump this to roll out a new revision
  destination:
    server: https://kubernetes.default.svc
    namespace: guestbook
```

## Telling the operator to watch these kinds

A `WatchRule` selects resources by plural name and (optionally) API group. To
mirror HelmReleases in a namespace:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: watch-helmreleases
  namespace: apps
spec:
  targetRef:
    name: my-gittarget
  rules:
  - apiGroups: ["helm.toolkit.fluxcd.io"]
    resources: ["helmreleases"]
```

The kind's CRD must be installed and served in the cluster — the operator
resolves what to watch from the live API surface, so any served kind (core, CRD,
or aggregated) is watchable. See [`configuration.md`](configuration.md) for the
full `GitProvider` / `GitTarget` / `WatchRule` model.

## Where the document lands in Git

A brand-new document follows the layout your folder already uses (sibling
inference), or the `GitTarget`'s declared placement policy. Absent any existing
convention, the cold-start default path is:

```text
{GitTarget path}/{namespace}/{group}/{resource}/{name}.yaml
```

so the HelmRelease above lands at
`.../apps/helm.toolkit.fluxcd.io/helmreleases/podinfo.yaml`. Once a document
exists, it is always edited **in place** wherever it already lives — the operator
never moves or duplicates it. See
[`configuration.md` → Where new resources are written](configuration.md#where-new-resources-are-written-specplacement).

## The boundary: documents, not chart inflation

"No Helm" means the operator never *renders* a chart. It will not run
`helm template`, it will not expand a kustomization `helmCharts:` generator, and
it will not turn a chart into a tree of manifests. What it does is mirror and
edit the `HelmRelease` **document** — the small piece of intent that says "install
this chart at this version." The chart is inflated by Flux, in the cluster, as
always.

The same line holds for the other kinds: the operator edits the fields on the
`Application` or KRO document that a human would edit; it does not understand or
reproduce what Argo CD or KRO then does with them. That boundary is what keeps
every edit round-trippable — exactly one writable destination in Git for each
change — and it is a deliberate part of the support contract, not a gap.
