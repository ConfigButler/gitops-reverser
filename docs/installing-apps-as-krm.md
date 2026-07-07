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

## The design decision: capture intent, not the rendered output

This is the part worth slowing down for. Before you point GitOps Reverser at a
cluster, decide **where and how your intent is expressed**, and configure the
operator to mirror *that* — not the objects your controllers render from it.

Every kind in the table above is a piece of **intent** that a controller expands
into many **derived** objects. One `HelmRelease` becomes Deployments, Services,
ConfigMaps, ReplicaSets, and Pods. One Argo CD `Application` becomes whatever it
syncs. One KRO instance becomes its entire resource graph. You want Git to hold
the small authored document, not the large derived tree — the derived tree is a
*projection* the controller recomputes on every reconcile.

**Why mirroring the output is wrong**, not just noisy:

- It breaks the [governing rule](design/gitops-api/README.md) — round-trippability.
  A rendered object is *owned by a controller*: it carries generated names, hash
  suffixes, injected defaults, and controller-managed metadata. There is no single
  writable human destination for it, so an edit can't round-trip. Capturing
  intent keeps exactly one writable destination per change.
- It churns. Reconcilers rewrite their outputs constantly; a repo full of
  rendered Deployments and ReplicaSets commits on every loop.
- It duplicates state that already exists as intent — the `HelmRelease` *is* the
  Deployment, expressed once, at the level a human actually edits.

This is the same principle as the [chart-inflation boundary](#the-boundary-documents-not-chart-inflation)
below, applied at runtime: don't put *derived* state in Git, at any level.

**The catch: the operator cannot tell intent from output for you.** A
`Deployment` is a `Deployment` whether you hand-authored it or Flux rendered it.
GitOps Reverser ignores a few purely-runtime kinds by default — Pods, Events,
Endpoints/EndpointSlices, Leases, ControllerRevisions, Jobs, CronJobs — but it
does **not** ignore Deployments, ReplicaSets, Services, or ConfigMaps, because
those are often exactly the intent you *do* want to capture. Drawing the line is
your job, and it is worth doing deliberately.

**How to draw it:**

1. **Select intent kinds, not workload kinds.** In a namespace where a controller
   renders output, watch `helmreleases` / `applications` / your KRO CRD — not the
   `deployments` and `services` they produce. Avoid a catch-all `resources: ["*"]`
   there.
2. **Separate intent from runtime, ideally by namespace.** The cleanest, most
   robust setup keeps authored intent in its own namespace (or cluster area) that
   your `GitTarget`/`WatchRule` points at, and lets the rendered workloads live
   elsewhere, unwatched. Then "what to capture" is answered by *where*, not by a
   fragile per-kind allow/deny list.
3. **Mind overlaps.** If a controller renders into the same namespace as other
   authored intent, keep the rule kind-scoped (watch only the intent kinds), so
   you never recapture the rendered output alongside it.

Getting this balance right up front is most of the work of adopting the tool. If
you skip it, the symptom is unmistakable: the repository fills with
controller-owned objects that change on every reconcile and that you cannot
meaningfully edit — noise, not intent.

## Telling the operator to watch these kinds

A `WatchRule` selects resources by plural name and (optionally) API group. Keep
it **scoped to intent kinds** (see the section above). To mirror HelmReleases in
a namespace — and nothing they render:

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
