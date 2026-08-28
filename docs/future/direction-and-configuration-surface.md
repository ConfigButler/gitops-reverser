# Direction review: what this product is for, and what the configuration surface should say

> Status: proposal — a strategy review with a decided Helm standpoint and worked
> configuration examples. Nothing here binds until scheduled.
> Date: 2026-08-27.
> Companions: [`config-surface-for-a-structured-repository.md`](config-surface-for-a-structured-repository.md)
> (the field-level review this extends),
> [`../design/gittarget-layout-model.md`](../layout/model.md) and
> [`../design/gittarget-api-wave.md`](../layout/api-wave.md) (the API work this
> sequences), and
> [`../design/support-boundary/helm-light-support-boundary.md`](../design/support-boundary/helm-light-support-boundary.md)
> (the Helm option this deliberately parks).

## The two directions on the table

**Direction A — serve practicing GitOps teams.** Mirror and edit their existing
repositories: more kustomize, some Helm, brownfield adoption, audit trails. The users
exist today and the corpus was built for them.

**Direction B — configuration as data.** A team building a cloud-native application does
not store its configuration in database tables. It models configuration as CRDs, edits it
through the Kubernetes API of an intent cluster (real, temporary, or remote via the
config-plane split), and GitOps Reverser manufactures the artifact: a clean, reviewable,
deployable Git folder. The cluster is the editor; Git is the storage; the PR is the
handoff.

## The finding: this is a fork in defaults, not in the codebase

The two directions share nearly all machinery — the watch pipeline, sanitization,
placement, the render oracle, attribution. What actually differs is **which user the
defaults, documentation, and onboarding optimize for, and where compatibility-hardening
stops**. That reframing is the review's main result, because it means the choice is cheap
to make and cheap to hold.

### Why direction B is the thesis

- **The constraints that make direction A hard dissolve in B.** In B we own the repository
  layout: no foreign kustomization to infer, no second writer, no legacy structure. The
  expansion boundary and fan-in rule become internal invariants instead of a
  compatibility frontier.
- **The hard prerequisites already shipped or are designed.** The config-plane split
  (inline `GitTarget.spec.kubeConfig`) points a target at a remote or ephemeral intent
  cluster. `CommitRequest` carries authorship and intent. The branch-per-experiment flow
  is designed in [`idea-application-editing.md`](idea-application-editing.md).
- **The layout model is a direction-B feature wearing direction-A clothes.**
  `kind: Kustomize` with `create: true` bootstraps structure into an empty repository;
  `Flat` and `Tree` are declared shapes; `mode: Observe` is the dry run. Those are exactly
  what "the operator manufactures the artifact" needs, and only incidentally what
  brownfield mirroring needs.
- **Direction A's ceiling is structural.** The poor-fit row in the README — teams who want
  Git to stay the write path — excludes the majority of practicing GitOps teams. Every
  compatibility increment (fancier overlays, generators, chart inflation) buys a thinner
  slice of users at a higher correctness cost, and fan-in = 1 means the ceiling is real
  rather than merely unbuilt.

### What direction A remains

**The funnel and the credibility.** Brownfield audit, onboarding scans, hotfix capture,
and the honest support contract are how the product earns trust and finds users. The
un-fancy kustomize subset stays load-bearing — see the environment axis below — but its
ambitions freeze at the current
[support contract](../design/support-boundary/support-contract.md). Widening it further is
a decision this review recommends against by default; each widening needs its own case.

## The Helm standpoint (decided here)

Three options were on the table:

1. **Declaration editing only** — `HelmRelease`/`Application` knobs, plus the values-file
   projection; the chart stays a black box.
2. **Helm-light inversion** — the provable local-chart subset in
   [`helm-light-support-boundary.md`](../design/support-boundary/helm-light-support-boundary.md):
   probe the chart, map live edits to values edits, prove by re-render.
3. **General Helm reversal** — off the table permanently; the boundary doc records why.

**The decision: option 1, plus the read side of option 2. Helm-light inversion is parked
behind entry criteria, without a schedule.**

The decisive observation is that **values-file projection Move 2 already closes the Helm
reverse loop at the values granularity, with zero inversion machinery.** Once the
free-standing values file is an editable projected document, a user edits it through the
Kubernetes API, the operator commits it, and the deployer re-renders. The chart never
stops being a black box. What inversion *adds* is only the ability to edit the **expanded**
object (the rendered Deployment) instead of the values document — a real convenience, and
in direction B an unnecessary one, because in an intent cluster the object you edit can
simply be the projected values document itself. Inversion is a direction-A luxury bought
with the most speculative machinery in the whole review (the probe, the sensitivity map, a
second renderer with a search step), and its v1 acceptance set — schema-closed,
`tpl`-free, checksum-free, toggle-free charts — is nearly empty among real hand-written
charts until several widenings land. That is too much scaffolding for too thin a first
slice.

What we do take from the helm-light analysis, because it is cheap and sound:

- **Chart-folder detection**: a `Chart.yaml` + `templates/` subtree is skipped as a unit,
  as the contract already plans.
- **Read-side classification**: chart-expanded live objects are expansion layer — never
  mirrored — using the deployer's own render record where one exists (Helm release
  storage under Flux) or a local verification render where it is sound (Argo semantics).
- **Blast-radius reporting**: when someone edits Helm knobs or a projected values file,
  the render diff can say what the change did — which pairs naturally with
  `mode: Observe`.

**Entry criteria for un-parking helm-light inversion** (all of them, together):

1. Move 2 is shipped and used, and users are still asking specifically to edit the
   *expanded* objects rather than the values document.
2. The GitTarget API wave has landed, so the layout/renderer seam exists to declare it on.
3. The render-owned-field carve-out (checksum annotations) is designed, since without it
   the acceptance set stays near-empty.

Until then, the boundary document is the parking spot: the analysis is preserved, the
taxonomy is reusable for the read side, and no probe code exists.

## The source side: the rule objects complete the model

The API wave's principle has a third clause its own text leaves implicit. Stated in full:

> **The connection describes the connection. The folder is described on the GitTarget.
> The subscription — which live objects flow at all — is described on the
> (Cluster)WatchRule.**

The source side already had its breaking rework (the scope-by-kind split: `WatchRule` for
namespaced resources with per-rule `sourceNamespace`, `ClusterWatchRule` for
cluster-scoped resources only), and it landed in the right shape. The authorization model
is **bilateral**: a rule declares capture intent from wherever it lives, and the target
bounds it (`allowedSourceNamespaces`, `allowSourceNamespaceOverride`, aggregated into one
`SourceNamespaceAuthorized` condition). N rules → one target is what lets a team that
does not own a folder request capture into it, and lets the folder's owner say no — which
is why the rules must stay their own objects even for the simple case: a merged object
could not express that delegation.

In direction B this reads even more naturally: **the WatchRule is the artifact's
manifest.** It is how the intent cluster declares what belongs in the artifact, the
namespaced kind is the tenant-ownable self-service surface, the cluster kind is
platform-owned, and #146's `objectSelector` is the inclusion predicate for curated
content. No new source-side API is needed for either direction.

The rules and the layout model meet at `layout.scope`, and the meeting produces two
validations worth declaring:

- **`scope: SingleNamespace` requires `allowedSourceNamespaces` to be an exact one-name
  list — and that name *is* the single namespace.** The question "which namespace is the
  single one?" must not be answered by the rules: N rules do not own the folder, and the
  first-writer-wins alternative is exactly the silent re-deciding the layout model
  exists to forbid. So the identity lives on the GitTarget, as the authorization bound
  collapsing into a structural fact: one exact name, no selector (a label selector cannot
  guarantee singularity), and CEL can check it at admission. Rules then merely subscribe
  within it — an omitted `sourceNamespace`, an explicit match, or `"*"` all resolve to
  that one namespace, and a rule naming any other namespace refuses loudly under the
  existing bilateral check.
- **A `ClusterWatchRule` referencing a `scope: SingleNamespace` target is refused, at the
  rule, by name.** The payoff of `SingleNamespace` plus `writeNamespace: Never` is a
  *portable* folder — deployable into any namespace at apply time. Cluster-scoped content
  breaks that portability, so admitting it would quietly void the structural claim the
  layout declared. The recorded objection is the app folder that carries a `ClusterRole`
  or CRD as a passenger; the honest escapes are `scope: MultiNamespace`, a second
  GitTarget for the cluster-scoped half, or — if the pattern proves common — a future
  third scope value, argued on its own. Silently mixing is the one option the surface
  should not offer.

## Where the configuration surface should go — by example

The one-sentence defect stands as stated in the
[config-surface review](config-surface-for-a-structured-repository.md): `spec.path` is one
string while the folder has structure, and the API offers no way to declare expectations,
see what was understood, or look before writing. The layout model and the API wave are the
fix. The examples below show the surface the two directions actually need — and that it is
the *same* surface with different defaults.

### Example 1 — the direction-B artifact target

A team's configuration CRDs live in an intent cluster; the operator manufactures a
deployable folder in an empty repository.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
metadata:
  name: shop-config
spec:
  providerRef:
    name: artifacts-repo
  branch: main
  path: apps/shop
  mode: Write
  commitWindow: 30s            # moved from GitProvider: batching describes this folder
  allowedSourceNamespaces:
    names: [shop]              # under SingleNamespace: exactly one name — this IS the namespace
  layout:
    kind: Kustomize
    create: true               # an empty repo becomes a buildable folder
    scope: SingleNamespace
    writeNamespace: Never      # the artifact is environment-agnostic; namespace at deploy
```

and the folder is inert until the tenant subscribes content to it:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: WatchRule
metadata:
  name: shop-config-content
  namespace: shop              # rules subscribe from inside the one admitted namespace
spec:
  targetRef:
    name: shop-config
  rules:
    - apiGroups: ["config.shop.example"]
      resources: ["*"]         # the team's configuration CRDs are the artifact's content
```

Everything direction B needs is here and nowhere else: the folder is described on the
GitTarget, the single namespace is *named* on the GitTarget (the one-name
`allowedSourceNamespaces` list is what `scope: SingleNamespace` requires), the WatchRule
is the artifact's manifest, structure can be brought into existence, and the artifact's
shape is a declared fact a reviewer can read. A `ClusterWatchRule` pointed at this target
is refused by name: cluster-scoped content would break the portability that
`writeNamespace: Never` promises.

### Example 2 — brownfield adoption with a dry run (direction A's front door)

Adopting an existing overlay folder should be look-before-write:

```yaml
spec:
  path: clusters/prod
  mode: Observe                # scan, resolve, publish — write nothing
  interval: 10m
  layout:
    kind: Auto                 # declared inference: named, visible, pinned
```

and the operator answers in status before a single commit exists:

```yaml
status:
  layout:
    declaredKind: Auto
    resolvedKind: Kustomize
    renderRoot: clusters/prod
    typesByDeclaration: 14
    typesByFallback: 1         # the one byType rule you forgot, named before it bites
```

Flipping `mode: Observe` to `Write` is then an informed act. This is the interaction that
gives `Observe` a purpose beyond a safety switch, and it is why `status.layout` should be
built with the wave rather than invented twice. The companion rules follow the same
pattern as Example 1 — a `WatchRule` per tenant namespace, or a `ClusterWatchRule` for
the cluster-scoped kinds a prod folder legitimately carries (this target declares no
`SingleNamespace` claim, so both kinds are admissible).

### Example 3 — the app-of-apps Helm folder under the decided standpoint

```yaml
spec:
  path: tenants/team-a
  layout:
    kind: Auto
```

with the folder containing an `Application` per environment, a local chart directory, and
values files. Under the decided standpoint the surface needs **no Helm-specific spec
field at all** — the verdicts do the work, and status makes them legible:

```yaml
status:
  layout:
    resolvedKind: Tree
    skipped:
      - path: tenants/team-a/chart
        reason: HelmChartFolder      # skipped as a unit; never mirrored into
    projected:
      - path: tenants/team-a/values-prod.yaml
        as: ValuesFile               # editable through the projection, committed here
```

with a `WatchRule` in `team-a` subscribing the `Application` documents
(`apiGroups: ["argoproj.io"], resources: ["applications"]`) plus the team's own
namespaced kinds. The `Application` documents are ordinary editable KRM; the chart
subtree is named and skipped; the values files are the write surface. If helm-light inversion is ever
un-parked, it appears here as a declared layout/renderer choice — the shape of the
surface does not change, which is the point of deciding the standpoint before the wave.

### Example 4 — the environment axis, one blessed pattern

Both directions answer environment-specific values the same way, and the surface should
say so instead of leaving it as folklore:

- **Structure**: a base plus one un-fancy overlay per environment — the shipped subset
  (`resources`, `namespace`, `images`, `replicas`, `$patch: delete`, authored
  image/replica entries).
- **Helm values**: one values file per environment, edited through the projection.
- **Never**: a substitution or variable layer. The measured record is unambiguous — the
  render-vs-live fence at the write path is the correct boundary, and a `${...}` token
  gate broke real CRDs when it was tried.

One GitTarget per environment folder keeps fan-in = 1 by construction and gives each
environment its own dry run, status, and commit batching.

## The open-issue map, sequenced by this review

| Issue | Verdict | Sequencing |
|---|---|---|
| #295 placement bugs | fix now; direction-agnostic correctness | before the wave |
| #296 visibility | metric split, `{kindLower}`, template constant now; `status.layout` rides the wave | split |
| #293 layout model | the centerpiece; more valuable under direction B than its own text claims | the wave's core |
| #294 API wave | core four (layout, mode, suspend/interval, commitWindow move) gate nothing else; CommitRequest items ride along without gating | the wave |
| #146 per-rule objectSelector | yes; in direction B it is how an intent cluster marks what belongs in the artifact | any time |
| #148 leader election | yes, scoped to active-passive; decoupled from full HA distribution | any time |
| #210 conventional PR titles | implement the ~15-line workflow check or close; it is a process wish, not a bug | close out |

## What we deliberately do not build

- **General or heuristic Helm inversion** — parked per the standpoint above; permanent
  refusals stay as the boundary doc records them.
- **Chart inflation of remote charts, plugin execution, remote bases** — unchanged.
- **A substitution/variable layer** — the environment axis is structural (overlays and
  values files), never token-based.
- **Kustomize widenings beyond the current contract by default** — each proposed widening
  argues its own case against the support contract; none is implied by direction A's
  continued existence as the funnel.

## The bottom line

Ship #295 now. Land the API wave with the layout model at its center, and let
`mode: Observe` plus `status.layout` turn adoption into a dry run. Hold Helm at
declaration editing plus the values projection, take the cheap read side of the helm-light
analysis, and leave inversion parked behind its entry criteria. Then tell the direction-B
story — intent cluster, declared layout, manufactured artifact, PR as handoff — as the
headline, with brownfield GitOps mirroring as the on-ramp. The fork in the road is real,
but it is a fork in defaults and storytelling, and that is the cheapest kind to take.
