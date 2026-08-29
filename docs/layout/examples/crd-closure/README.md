# CRD closure: what else the folder needs

> **This scenario has no decided answer, and that is deliberate.** It exists to make the choice in
> [`crd-handling.md`](../../../design/crd-handling.md) legible by showing the same event under both
> candidate shapes. Every other scenario in this folder illustrates a decision; this one illustrates
> a question, and the design page is where it gets settled.

An application team's configuration folder holds `Widget` objects. The folder is complete as a
mirror and incomplete as an artifact: applied to a cluster that has never heard of
`widgets.apps.example.com`, every `Widget` in it is rejected. Something has to carry the definition,
and the interesting part is that it does not have to be this folder.

## Starting repository

[`repository/`](repository/) is `apps/shop` in the real repository: one kustomize root, one
ConfigMap, one existing `Widget`.

```text
apps/shop/
  kustomization.yaml
  configmap-storefront.yaml
  widget-checkout.yaml
```

## Proposed configuration

[`config/watchrule.yaml`](config/watchrule.yaml) subscribes to `widgets` and `configmaps`, and
**names no CRD at all**. That is the point: today the only way to get definitions into Git is a rule
for `customresourcedefinitions`, which mirrors every CRD in the cluster, Flux's and cert-manager's
included. The proposal derives the set instead, from the types the rules match, over a join
the operator already performs.

[`config/gittarget.yaml`](config/gittarget.yaml) sets the proposed
`includeTypeDefinitions: Referenced`. The default is `None`.

## The live input, and what it carries

[`input/widget-search.yaml`](input/widget-search.yaml) is an ordinary new object. Its placement is
not the question: the folder's one root takes it as `widget-search.yaml` with a `resources:` entry,
in every option below.

[`input/crd-widgets.yaml`](input/crd-widgets.yaml) is the definition behind it, and it is worth
reading for its metadata rather than its schema:

- `meta.helm.sh/release-name` and `app.kubernetes.io/managed-by: Helm` say a Helm release owns this
  object, and **`sanitize` does not strip them**, so they would arrive in Git intact.
- `kustomize.toolkit.fluxcd.io/name` says a Flux `Kustomization` applied it, and **`sanitize` does
  strip that**, so it can only be read from the live object.

Those two facts are most of the argument.

## Option B: `Referenced`

[`expected-referenced.patch`](expected-referenced.patch). The `Widget` lands as usual, and one extra
generated file appears: a `type-dependencies.yaml` naming the type, its served versions, the CRD, a
digest of its spec, and the Helm release the CRD came from. No schema, about fifteen lines, and it
changes only when the type set or the schema does.

Whoever hydrates the folder reads it: the per-branch editing cluster installs the referenced
definitions before loading objects, and a reviewer can see at a glance which operators a branch
depends on.

## Option A: `Vendored`

[`expected-vendored.patch`](expected-vendored.patch). The definition itself is committed at the
canonical cluster-scoped path and registered in the root. It is self-contained, and it costs three
things the referenced shape does not:

- **A second owner.** The committed copy still says `meta.helm.sh/release-name: widget-operator`,
  so it claims to belong to a release in another cluster, and applying it elsewhere collides with
  Helm's ownership check.
- **Size and churn.** Real schemas are hundreds of lines; Argo CD's `Application` CRD is about a
  megabyte, and every operator upgrade rewrites it.
- **Ordering.** A folder holding a CRD and its custom resources has to apply the definition first.
  Flux and Argo CD arrange that; `kubectl apply -k` over a plain folder does not.

For application configuration it also puts the wrong team in charge: the app developer owns what the
`Widget` CRD looks like, and it belongs beside the controller that serves it.

## Scenario contract

- Starting repository: [`repository/`](repository/), which **is** `apps/shop/`.
- Live inputs: [`input/widget-search.yaml`](input/widget-search.yaml) and, as context rather than as
  a watched object, [`input/crd-widgets.yaml`](input/crd-widgets.yaml).
- Expected Git change: [`expected-referenced.patch`](expected-referenced.patch) under the
  recommendation, [`expected-vendored.patch`](expected-vendored.patch) under the alternative.
- Expected status: `Ready=True`; the type closure is an observation, never a condition.
- Boundary: the operator never installs, upgrades or deletes a CRD. It records or copies one.

## What this scenario cannot show

The definition is necessary and not always sufficient. `Widget` converts with nothing running, which
is why its manifest entry says `conversion: {strategy: None}` and why this scenario stays simple. A
type whose CRD declares `spec.conversion.strategy: Webhook`, or whose real cluster mutates objects
through a mutating admission webhook, needs something to answer, and a workload-less branch cluster
has nothing to answer with. The objects are accepted and are quietly not what production would have
stored.

It does not need a *controller*, though, only an endpoint: `WebhookClientConfig` takes a `url` as
well as a `service`, and `url` is precisely the form for a webhook that does not run in the cluster.
The ladder, the constraints on that URL, and what it would take for ConfigButler to host
transformations rather than every team exposing an endpoint, are in
[`crd-handling.md`](../../../design/crd-handling.md).
