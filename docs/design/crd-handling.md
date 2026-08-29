# CRD handling: what a folder needs to be applied, and where the definitions come from

> Status: design, unbuilt. Nothing here binds until it is scheduled.
> Date: 2026-08-29. Index: [`../INDEX.md`](../INDEX.md).
>
> Worked example: [`../layout/examples/crd-closure/`](../layout/examples/crd-closure/README.md),
> which shows the same live objects under both candidate shapes so the choice can be read rather
> than argued.

A mirrored folder of custom resources is not applicable on its own. A `Widget` cannot exist in a
cluster that has never heard of `widgets.apps.example.com`, so somewhere between the folder and the
cluster the definition has to arrive. This page decides where from, and deliberately does **not**
decide it by mirroring every CRD in the source cluster, which is the only thing a `WatchRule` can
express today.

## Two consumers, and they want different things

**1. Applicability.** Can this folder be applied to a fresh cluster? Answering yes by inspection
means the definitions are in the repository.

**2. The per-branch editing cluster.** The direction this project is heading is that a branch under
edit gets its own small, workload-less Kubernetes cluster, and the branch's documents are hydrated
into it as real API objects so they can be edited through the API rather than as text. That cluster
has to be able to **hold** the objects, which means every custom type in the folder must be
installed before hydration. Here the definition does not have to be in the repository at all. It has
to be *installable at spin-up*.

The second consumer is the demanding one, and it is better served by a reference than by a copy: a
reference can be resolved to the version the type's owner publishes, while a copy freezes whatever
happened to be installed in the cluster we were mirroring.

## What we already know, at no cost

`typeset` classifies every served type's origin as `builtin`, `crd` or `aggregated`, with the CRD
itself as evidence and a confidence that says whether an object was observed
([`model.go`](../../internal/typeset/model.go), [`type-followability.md`](../spec/type-followability.md)).

So "which CRDs back the types this GitTarget writes" is a lookup over a join the operator already
performs. Whatever this page decides, the **selection** is close to free. The cost is in what we do
with the answer.

This also settles one thing early: the selection cannot be expressed by a per-rule `objectSelector`
([#146](https://github.com/ConfigButler/gitops-reverser/issues/146)). The relevant set is *derived*
from the rules and changes when they do; it is not a label match anyone can write down.

## Four options

| | What lands in Git | Who owns the CRD | Where the branch cluster gets it |
|---|---|---|---|
| **A. Vendor** | the CRD objects | now two: the installer and this folder | the folder |
| **B. Reference** | a small manifest of names, versions, sources and digests | its installer, unchanged | resolving the manifest |
| **C. Nothing** | nothing | its installer | copied from the source cluster at spin-up |
| **D. Synthesize** | a generated minimal schema | nobody real | the generated schema |

**A costs more than it looks.** A CRD installed by Helm carries `meta.helm.sh/release-name`,
`meta.helm.sh/release-namespace` and `app.kubernetes.io/managed-by: Helm`, and **none of those are
stripped on the way to Git**: [`sanitize`](../../internal/sanitize/types.go) removes Flux, kro and
applyset bookkeeping plus two exact Argo keys, and Helm's ownership metadata is not in either list.
So a vendored CRD arrives in the repository still claiming to belong to a release in another
cluster, and applying it somewhere else walks into Helm's own ownership check. Add the size (our
smallest CRD is 25 KB; Argo CD's `Application` CRD is about a megabyte) and the churn (every
operator upgrade rewrites the file), and a configuration repository becomes mostly vendored schema
diffs.

**B is the one that matches who owns what.** For application configuration in particular, the app
team owns what its CRD looks like, and the CRD belongs in the app's own repository next to the
controller that serves it. A configuration folder that vendors it forks it.

**C is right for the case it covers and silent about the rest.** Copying from the source cluster at
spin-up needs nothing committed and always matches production exactly. It fails precisely when the
editing cluster is most useful: a branch from a contributor without source-cluster access, an
environment that no longer exists, an edit made while the cluster is unreachable.

**D is rejected.** A synthesized schema accepts objects the real API server would reject, and the
entire value of hydrating a branch into a real cluster is that it validates like the real one.

## Recommendation

- **Default `None`.** The folder holds instances. The installer owns the definitions. This is the
  posture that does not create a second owner, and it is what a mirror of an existing cluster wants.
- **Opt-in `Referenced`**: commit a small manifest of references and digests, never schemas.
- **The branch cluster resolves in order**: the reference manifest first, the source cluster second
  (which is option C kept as a fallback rather than a design), and otherwise **fail loudly, naming
  the types it could not install**. Hydrating a branch into a cluster that silently lacks a type
  produces an editing session that looks fine and drops objects.
- **`Vendored` stays available** for a type whose definition has no installable source. It is an
  escape hatch with the ownership cost stated at the point of use, not a default.

## Where a reference comes from, and the trap in reading it

Provenance has to be read from the **live object**, in the operator, and can never be recovered from
the repository. `sanitize` strips `kustomize.toolkit.fluxcd.io/*` from labels and annotations, which
is exactly the evidence that would say a CRD came from a Flux `Kustomization`. Capture it at
observation time or lose it.

The ladder, in the order it should be tried:

1. **Helm**: `meta.helm.sh/release-name` and `release-namespace`, with
   `app.kubernetes.io/managed-by: Helm`. Naming the chart and version means reading the release
   object, which is a Secret in the source cluster. That is a new read, and an authorization
   question, not a free one.
2. **Flux**: `kustomize.toolkit.fluxcd.io/name` and `namespace` on the live CRD, resolved through
   the `Kustomization` to its source. Live-only, per the trap above.
3. **Argo CD**: no tracking annotation is stamped on CRDs, since its repo-server stamps every
   *non-CRD* object it applies. Provenance for an Argo-installed CRD comes from the `Application`
   that lists it, or not at all.
4. **Unattributed**: record the CRD name, its served versions and a digest, and say plainly that the
   source is unknown. A reference that admits it cannot be resolved is more useful than a guess.

## The finding that matters most for the branch cluster

**A workload-less cluster runs no webhook of its own.** A CRD with
`spec.conversion.strategy: Webhook` needs something to convert between stored versions, and a type
whose real cluster mutates objects through a mutating admission webhook will hold *unmutated*
objects in the branch cluster. Installing the CRD is necessary and not always sufficient, and the
gap is invisible: the object is accepted, it is simply not what production would have stored.

Three things narrow that, in increasing order of ambition. Take them in this order.

**1. Most defaulting is not a webhook at all.** Structural-schema defaults are applied by the API
server itself
([`pkg/apiserver/schema/defaulting`](https://github.com/kubernetes/apiextensions-apiserver)), so a
CRD's `default:` values work in a cluster with nothing running. Only *mutating admission* is
missing, which is a much smaller set than "defaulting".

**2. Conversion only fires when versions differ.** If the branch cluster installs the CRD with the
versions the folder actually uses, and serves what it stores, no conversion runs. Narrowing the
installed version list is a subset of the real CRD rather than a synthesized schema, so it does not
fall into option D, and it is the cheapest honest answer for the common case of a folder written at
one version.

**3. The webhook can live outside the cluster, and this is the interesting one.**
`WebhookClientConfig` takes **either** `service` **or** `url`
([apiextensions v1 types](https://github.com/kubernetes/apiextensions-apiserver), verified at
v0.36.4), and the upstream field documentation is explicit that `url` is for a webhook that is *not*
running in the cluster: the API server cannot resolve in-cluster DNS, so `service` is the in-cluster
form and `url` is the external one. The same shape exists on admission webhook configurations.

So a branch cluster can install the CRD pointed at a conversion endpoint that runs somewhere else,
and the type converts correctly with nothing deployed. The constraints, all from the field's own
contract:

- **`https` only.** No query string, no fragment, no `user:password@`. A path *is* allowed and may
  carry an arbitrary string, which upstream itself suggests using as a cluster identifier.
- **`caBundle` is optional**: omitted, the API server uses its system trust roots, so a publicly
  trusted certificate needs no bundle at all. A private CA needs one, and needs it rotated.
- **The API server sends no bearer token.** Client credentials for webhooks come from API
  server-level configuration, not from anything a per-CRD `url` can carry. An external endpoint
  therefore authenticates its caller by mTLS or by the path identifier above, and must assume the
  path is the only secret it has.
- **Reachability runs the other way from usual.** The branch cluster's control plane makes the
  outbound call, so the endpoint has to be reachable from wherever we run those clusters, and every
  object of that type is sent to it. That is an egress policy question and a data exposure question
  before it is a design question.
- **It is in the edit path.** Latency and availability of that endpoint become latency and
  availability of editing a branch.

### Hosted transformations

The natural extension: rather than every team exposing an endpoint, ConfigButler hosts one, with
common transformations composed by the user (clicked together, or scripted, or generated) and
addressed per branch through the URL path.

It is a real product direction and the mechanism is already proven, since it is the same webhook
contract with us as the operator. Two things have to be true before it is safe, and they are worth
writing down now:

- **A transformation is code, and it must be pinned.** A branch cluster whose behavior depends on
  the current state of a hosted rule set is not reproducible: the same branch hydrates differently
  next week. The reference manifest is the place to pin the transformation version, next to the
  CRD digest it belongs to.
- **Divergence from the app's real webhook must be loud.** If our transformation and the
  application's own conversion disagree, the branch cluster stores objects production would not, and
  we have recreated the invisible gap this section opened with, except now we caused it. A type
  served by a hosted transformation rather than by the app's own endpoint should be marked
  non-authoritative in the session, and the marking should survive into whatever the branch produces.

The honest ladder is therefore: **narrow the versions** so nothing converts; else **point at the
application's own endpoint**, which is where that code already lives and where the app team already
owns it; and only then **host a transformation**, for types whose owner has no endpoint to point at.

## Placement consequences

- **CRDs are cluster-scoped.** Canonically that is
  `_cluster/apiextensions.k8s.io/customresourcedefinitions/{name}.yaml`; in a kustomize folder it is
  a flat file beside the root, registered in `resources:` like any other placement.
- **Apply order.** A folder holding both a CRD and its custom resources has to apply the definition
  first. Flux and Argo CD both handle this; `kubectl apply -k` over a plain folder is where it bites.
  If we vendor, the folder has to say who guarantees the ordering.
- **Scope.** A namespaced `GitTarget` writing cluster-scoped documents widens what that tenant puts
  in Git. That is an authorization question and it belongs with
  [`source-scope-simplification.md`](source-scope-simplification.md), not with placement.

## The API shape

`GitTarget.spec.includeTypeDefinitions: None | Referenced | Vendored`, defaulting to `None`.
Additive, so it needs no coordinated bump and does not belong to the breaking wave. The derived set
is recomputed when rules change, over the isolation seam that already exists for exactly that
([`gittarget-isolation-on-rule-change.md`](../spec/gittarget-isolation-on-rule-change.md)).

## Open questions

- Does the reference manifest belong **in the folder** as a committed artifact, or only in
  `status`? In the folder it is reviewable and travels with the branch; in status it never conflicts
  and never goes stale in a PR.
- **A digest of what?** The CRD's `spec`, so a resource-version bump is not a change, or the whole
  object, so any drift shows.
- One manifest **per target folder** or one **per repository**? Two targets in one repository will
  reference overlapping types.
- Does the branch cluster install **only CRDs**, or the controllers too? With `url` webhooks the
  question narrows: what has to run is an *endpoint*, not a controller, and it does not have to run
  in the branch cluster at all.
- Does the reference manifest record a type's **conversion strategy** and, where it is `Webhook`,
  the endpoint the branch cluster should point at? The manifest is the only place that knows both
  the type and its origin, so it is the natural home, and it makes "this folder needs a reachable
  endpoint to be editable" a reviewable fact rather than a runtime surprise.
- Who **owns the caBundle** for an app-hosted endpoint, and what happens to a branch when it
  rotates?
- Should `Vendored` **strip Helm and Flux ownership metadata** on the way out? It would make the
  vendored copy applicable elsewhere, at the cost of `sanitize` acquiring a rule that exists for one
  option of one field.
