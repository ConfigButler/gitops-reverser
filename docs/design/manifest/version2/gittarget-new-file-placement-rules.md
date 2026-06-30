# GitTarget new-file placement rules

> Status: proposed
> Captured: 2026-06-05
> Related:
> [file-agnostic-placement.md](../file-agnostic-placement.md) — **the vision Option C serves**,
> [contextual-namespace-and-kustomize-folder-editing.md](../contextual-namespace-and-kustomize-folder-editing.md),
> [gittarget-repository-validity-and-placement.md](gittarget-repository-validity-and-placement.md),
> [current-manifest-support-review.md](../../../finished/current-manifest-support-review.md),
> [manifestedit-new-file-placement-spike.md](../../../finished/manifestedit-new-file-placement-spike.md),
> [reconcile-via-watchlist-mark-and-sweep.md](../reconcile-via-watchlist-mark-and-sweep.md),
> [per-type-reconcile-and-streaming-tail.md](per-type-reconcile-and-streaming-tail.md),
> [gitpath-foreign-content-stringency.md](../../gitpath-foreign-content-stringency.md)

## Summary

New-resource placement should become an explicit GitTarget-level policy. There
are three viable shapes:

- **Option A: ordered rule lists** (`sensitiveRules` / `normalRules`), evaluated
  top to bottom.
- **Option B: type maps plus defaults** (`sensitive.byType` / `normal.byType`),
  using exact GVR keys such as `v1/secrets` and `apps/v1/deployments`.
- **Option C: follow the existing layout (sibling inference)** — no policy at all;
  place a new resource where resources like it already live in the repo, and only
  fall back to canonical placement when there is no sibling to learn from.

A and B both make placement a *declared CRD policy*. C makes it a *continuation
of the layout already in the repo* — zero new API surface. They are not rivals;
they layer:

- **Option B is the chosen declared API.** When a user wants to *prescribe* a
  layout, the nested type-map (`placement.sensitive.byType` / `.normal.byType` +
  defaults) is the surface to reach for — small, exact, easy to validate. Ordered
  rules (A) stay a later escape hatch only if the type map proves too limiting.
- **Option C is the default underneath it.** With no policy, placement *follows the
  layout already in the repo*; on an empty repo it falls through to today's
  canonical path, so default behaviour is byte-identical to now. C is what makes
  "point me at an existing folder and it just works" real — the vision in
  [file-agnostic-placement.md](../file-agnostic-placement.md) — and it removes the
  need for B to carry a catch-all default, because the gaps B leaves are filled by
  inference rather than by a hand-written fallback template.

So the recommended shape is **B over A for the declared surface, with C as the
zero-config default that B overrides where the user has an opinion.** The rest of
this document develops B and C together; C's sharp edges get a dedicated
[problems-and-risks](#problems-and-risks-with-option-c) section because inferring
policy from mutable repo state is where the real subtlety lives.

Existing manifests are still match-first: once a resource already has a document
in Git, updates and deletes use that document's current location instead of
re-running placement.

This keeps the useful part of the older `newFilePath` proposal, but makes the
per-type policy explicit:

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
spec:
  providerRef:
    name: platform
  branch: main
  path: clusters/prod
  placement:
    sensitive:
      byType:
        v1/secrets: "{namespace}/secret-{name}.sops.yaml"
      default: "{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.sops.yaml"
    normal:
      byType:
        v1/configmaps: "{namespace}/configmaps.yaml"
      default: "all-else.yaml"
```

In this example:

- sensitive resources land in one identity-complete SOPS file per resource;
- ConfigMaps are grouped into `clusters/prod/configmaps.yaml`;
- every other new resource is appended to `clusters/prod/all-else.yaml`.

That is powerful enough to express layouts such as `namespace-{namespace}.yaml`,
per-kind bundles, Secret-only SOPS paths, and the current canonical
`group/version/resource/namespace/name.yaml` layout. Splitting sensitive and
normal placement is what keeps this from becoming too sharp: a broad normal
default cannot catch a Secret, and sensitive placement can have stricter
uniqueness rules.

The pushback: fully ordered rules may be more API than we need first. A type map
is smaller, easier to validate, and still supports Secret-specific paths,
ConfigMap bundles, and a catch-all default. Ordered rules remain the flexible
escape hatch if users later need scope-wide or metadata-aware placement.

## Current implementation, as reviewed

The current `GitTargetSpec` has `providerRef`, `branch`, `path`, and optional
`encryption`; it has no placement policy yet
([api/v1alpha3/gittarget_types.go](../../../../api/v1alpha3/gittarget_types.go)).

The writer already uses the materialized-model direction described in
[current-manifest-support-review.md](../../../finished/current-manifest-support-review.md):

- steady-state writes scan the GitTarget subtree into a content-derived store,
  then apply a commit-scoped plan
  ([internal/git/plan_flush.go](../../../../internal/git/plan_flush.go));
- resync uses the same content-derived upsert path plus mark-and-sweep for
  managed orphans ([internal/git/resync_flush.go](../../../../internal/git/resync_flush.go));
- existing resources are found by manifest identity, so moved files are updated
  in place;
- new resources still fall back to `ResourceIdentifier.ToGitPath()`, with
  `.sops.yaml` added for sensitive resources
  ([internal/types/identifier.go](../../../../internal/types/identifier.go),
  [internal/git/git.go](../../../../internal/git/git.go)).

So placement policy should replace only the final "resource has no document in
Git, pick a path" step. It should not change content identity, acceptance,
mark-and-sweep, or the rule that existing documents stay where they are.

## Why GitTarget-level, not WatchRule-level

Placement belongs on `GitTarget`.

`WatchRule` and `ClusterWatchRule` select resources. They are allowed to overlap:
two rules may select the same ConfigMap through different resource expressions,
or a future rule may select by label while another selects by type. If placement
lives on the selecting rule, a single resource can have two valid placements. The
controller would then need a second conflict-resolution system whose only purpose
is to decide which rule's placement won.

A GitTarget owns exactly one repository folder. The folder layout is part of that
ownership policy. The watched rules decide what enters the target; the target
decides where a new entry goes.

If per-rule placement is ever needed, it should be expressed as data available to
the GitTarget placement matcher, not as the placement owner itself. For example,
a future matched resource could carry `watchRuleNames` or `watchSource`, and a
GitTarget rule could match on that. That keeps one ordered placement list.

## Option A: ordered rule lists

Prefer a new structured field instead of changing `spec.newFilePath` into a list.
A list named `newFilePath` is no longer a file path; it is a policy. The clearer
shape is:

```yaml
spec:
  placement:
    sensitiveRules:
      - path: "{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.sops.yaml"
    normalRules:
      - match:
          apiGroups: [""]
          resources: ["configmaps"]
        path: "{namespace}/configmaps.yaml"
      - path: "{groupPath}/{version}/{resource}/{namespace}/{name}.yaml"
```

Compatibility option:

- no `spec.placement` means the current canonical placement;
- a future single-string `spec.newFilePath` can be treated as one fallback rule if
  it already exists by the time this lands;
- do not expose both long-term. Pick one canonical surface in the CRD.

Suggested Go shape:

```go
type GitTargetPlacementSpec struct {
    SensitiveRules []GitTargetPlacementRule `json:"sensitiveRules,omitempty"`
    NormalRules    []GitTargetPlacementRule `json:"normalRules,omitempty"`
}

type GitTargetPlacementRule struct {
    Match *GitTargetPlacementMatch `json:"match,omitempty"`
    Path  string                   `json:"path"`
}

type GitTargetPlacementMatch struct {
    APIGroups   []string `json:"apiGroups,omitempty"`
    APIVersions []string `json:"apiVersions,omitempty"`
    Resources   []string `json:"resources,omitempty"`
    Kinds       []string `json:"kinds,omitempty"`
    Namespaces  []string `json:"namespaces,omitempty"`
    Scope       string   `json:"scope,omitempty"`     // Namespaced | Cluster
}
```

Rules are deliberately simple:

- the controller chooses `sensitiveRules` for sensitive resources and
  `normalRules` for everything else;
- rules are evaluated in list order;
- fields inside one `match` are ANDed;
- lists inside one field are ORed;
- an omitted `match` matches everything;
- each non-empty rule list must include a catch-all fallback rule;
- omitted `placement` uses the built-in canonical fallback for both lists;
- omitted `sensitiveRules` uses the built-in secure canonical SOPS fallback;
- omitted `normalRules` uses the built-in canonical plaintext fallback;
- an explicitly empty rule list is invalid.

That gives the user top-to-bottom control without needing CEL, Go-template
conditionals, or per-rule priorities.

## Option B: type map plus defaults

There is a smaller API shape worth considering before committing to ordered
rules. Most placement needs are not "run a matcher"; they are "this type goes
here, and everything else goes there." That can be expressed as exact type
lookups plus defaults:

```yaml
spec:
  placement:
    sensitiveTypes:
      v1/secrets: "{namespace}/secret-{name}.sops.yaml"
    sensitiveDefault: "{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.sops.yaml"
    normalTypes:
      v1/configmaps: "{namespace}/configmaps.yaml"
    normalDefault: "all.yaml"
```

Names can improve, but the model is:

- classify the resource as sensitive or normal;
- build its resolved type key from GVR;
- look for an exact entry in the matching type map;
- otherwise use that class's default.

The type key should be based on **API resource identity**, not manifest kind:

| Resource | Type key |
|---|---|
| core Secret | `v1/secrets` |
| core ConfigMap | `v1/configmaps` |
| Deployment | `apps/v1/deployments` |
| cert-manager Certificate | `cert-manager.io/v1/certificates` |

That means plural resource names, not singular kind names. This matches the
writer's `ResourceIdentifier` and the watch-rule resource model. It also avoids
the question of whether `v1/Secret`, `v1/secret`, or `v1/secrets` is the "right"
spelling.

The type-map shape is less flexible than ordered rules, but that may be the
point:

- no rule ordering to understand;
- no `match` object to validate;
- no "does this broad rule accidentally catch too much?" concern;
- exact type overrides are naturally unique;
- defaults make the policy short.

It also gives sensitive resources the same hard split as the rule-list API. A
normal type map cannot catch a Secret. A sensitive type map cannot route plaintext
resources. If ConfigMaps are intentionally added to the configured sensitive
resource policy, they use `sensitiveTypes` / `sensitiveDefault`; otherwise they
use `normalTypes` / `normalDefault`. The placement policy does not decide
sensitivity.

Suggested Go shape:

```go
type GitTargetPlacementSpec struct {
    SensitiveTypes   map[string]string `json:"sensitiveTypes,omitempty"`
    SensitiveDefault string            `json:"sensitiveDefault,omitempty"`
    NormalTypes      map[string]string `json:"normalTypes,omitempty"`
    NormalDefault    string            `json:"normalDefault,omitempty"`
}
```

An object wrapper may be better for future metadata:

```yaml
placement:
  sensitive:
    byType:
      v1/secrets: "{namespace}/secret-{name}.sops.yaml"
    default: "{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.sops.yaml"
  normal:
    byType:
      v1/configmaps: "{namespace}/configmaps.yaml"
    default: "all.yaml"
```

```go
type GitTargetPlacementSpec struct {
    Sensitive GitTargetPlacementClass `json:"sensitive,omitempty"`
    Normal    GitTargetPlacementClass `json:"normal,omitempty"`
}

type GitTargetPlacementClass struct {
    ByType  map[string]string `json:"byType,omitempty"`
    Default string            `json:"default,omitempty"`
}
```

This nested version is probably the cleaner type-map API. It keeps the
sensitive/normal split obvious and leaves room for class-level fields later, such as
`allowMultiDocument`, without inventing new top-level names.

The validation rules are almost the same as for ordered rules:

- omitted `placement.sensitive.default` uses the built-in secure canonical SOPS
  fallback;
- omitted `placement.normal.default` uses the built-in canonical plaintext
  fallback;
- every `byType` key must parse as a valid resolved type key;
- every referenced type should be served and watched by the GitTarget, or at
  least reported as unused policy;
- sensitive paths must end in `.sops.yaml` or `.sops.yml`;
- sensitive paths must be identity-complete, unless the type key itself narrows
  to one namespaced or cluster-scoped type and the path contains the scope
  identity;
- normal paths may intentionally collide and append to plaintext multi-document
  files.

The main loss is expressiveness. A type map cannot say "all namespaced resources
go to `namespace-{namespace}.yaml`, but cluster-scoped resources go to
`cluster.yaml`" unless every type is listed. It also cannot match future
metadata such as labels. If we do not need those patterns yet, this may be a
better first API than ordered rules.

My current preference is:

1. ship the nested type-map (B) as **the** declared API — it is the smallest
   surface that covers the real "this type here, everything else there" need;
2. ship Option C (sibling inference, below) as the **default** that runs when B is
   absent or silent for a resource, so an unconfigured target follows the repo's
   own layout instead of forcing canonical;
3. keep ordered rules (A) as a future extension only if users hit the type-map
   limit;
4. keep the same template renderer, SOPS validation, and append rules across all
   three, so B's templates and C's inferred locations flow through one writer.

## Option C: follow the existing layout (sibling inference)

A and B both ask the user to *declare* the layout in the CRD up front, in a
template language, split into sensitive and normal. Option C asks for nothing. It
places a new resource **where resources like it already live in the repo.** The
folder is the policy.

This is not a new principle — it is the one the writer already uses, generalized
one notch. Today an existing resource is found by manifest identity and edited *in
place*: the operator already follows a file's current location instead of imposing
canonical placement ("existing manifests are still match-first", above). Option C
extends "follow the existing location" from *the same resource* to *its siblings*:
a resource the operator has never written before is placed next to the resources
it most resembles. It is the literal implementation of
[file-agnostic-placement.md](../file-agnostic-placement.md)'s goal — "point at a
real folder and it just works", "the location doesn't matter" — because the layout
already in Git, having passed acceptance, is by definition a layout the operator
will accept.

### How it resolves a path

Placement only ever runs for a **new** resource — one with no document in the
store yet (match-first handles everything else). The content-derived store already
knows every accepted document, its file path, and its effective identity. C reads
two independent facts straight off that store; it never reverse-engineers a
template.

1. **Which directory** — find the nearest *cohort* of existing documents that
   shares attributes with the new resource, most specific first:

   | Step | Cohort | Place in |
   |---|---|---|
   | 1 | same (resource type, namespace) | that cohort's directory |
   | 2 | same resource type, any namespace | that cohort's directory |
   | 3 | same namespace, any type | that cohort's directory |
   | 4 | nothing matches | canonical `ToGitPath()` directory (today's default) |

2. **One-per-file vs bundle** — look at how that cohort is stored:
   - cohort is **one resource per file** → create a new single-document file in
     that directory, named `{name}.yaml` (the only filename pattern worth
     inferring; anything fancier falls back to `{name}.yaml`);
   - cohort **shares a file** (a bundle such as `configmaps.yaml` or `all.yaml`) →
     append the new resource as a document to that same bundle, via the existing
     plaintext multi-document append path.

Both decisions are *observed*, not guessed, so there is no fuzzy template
inference and the result round-trips: the store's file↔identity map (the same one
match-first relies on) is the single source of truth.

### Sensitive stays hard-split — with no config

The sensitive/normal split that A and B get from two config blocks, C gets for
free from the encryption classification already in the store:

- a sensitive resource **never** infers from plaintext siblings and is **never**
  appended to a plaintext bundle;
- it infers only from other sensitive single-document `.sops.yaml` siblings in the
  same directory, otherwise it uses the built-in **secure canonical SOPS fallback**
  (identity-complete, `.sops.yaml`).

So the encryption guarantee never depends on the user having configured the split
correctly — there is no split to configure.

### Namespace style comes along for free

Because C mirrors a sibling, it also mirrors that sibling's `NamespaceSource`
(from [contextual-namespace-and-kustomize-folder-editing.md](../contextual-namespace-and-kustomize-folder-editing.md)).
Drop a new file beside namespace-less documents that sit under a supported
kustomize context and it inherits that style — no `metadata.namespace` is written;
beside explicit-namespace documents the namespace is written. This is the one
option that answers file-agnostic-placement.md's "I'd like to *not* write the
namespace" without adding a `manifestStyle` knob: placement and output style
converge under "match your neighbours". (Bounded by the contextual-namespace
rules: an ambiguous context already refuses the GitTarget before placement runs.)

### Empty folder → canonical, then self-propagating

A freshly bootstrapped repo has no siblings, so the first resource of each kind
lands on canonical `ToGitPath()` — **byte-identical to today.** From then on the
layout propagates itself. A brand-new target behaves exactly as it does now; the
power only appears once a human (or a prior import) has established any layout.

### Determinism and ambiguity

A type can legitimately live in two layouts at once (some ConfigMaps bundled, some
canonical). The lookup must be deterministic:

- pick the cohort with the **most members**; tie-break on lexically-smallest
  directory, then smallest file — stable and independent of walk order.
- Unlike contextual-namespace ambiguity (a correctness hazard, so it *refuses*), a
  "wrong but valid" new-file location is cosmetic: the document is match-first the
  instant it exists, so a deterministic tie-break is better than failing the
  target. Sensitive identity-completeness is still enforced, so determinism never
  weakens the encryption guarantee.

### What it does not do, and how it composes

C cannot express a layout you do not yet *have* — a greenfield "I want all
ConfigMaps bundled even though none exist yet" intent. That is exactly what A and B
are for, and the three compose cleanly:

- **C is the default** (no config), best for brownfield / "point me at an existing
  repo";
- **B (or A) is the override:** an explicit type-map entry takes precedence, and C
  fills every gap — so B no longer needs a catch-all default, because the canonical
  fallback is just C's step 4;
- if a per-repo override is ever wanted *without* CRD surface, the repo-native
  form — consistent with the just-landed `.gittargetignore` (an in-repo, versioned,
  zero-API policy file, see
  [gitpath-foreign-content-stringency.md](../../gitpath-foreign-content-stringency.md))
  — is the natural shape. Noted, not required: C's whole point is that the base
  case needs nothing.

### Problems and risks with Option C

Sibling inference is powerful precisely because it reads its policy from mutable
repo state — which is also exactly where every one of its sharp edges comes from.
None of these is fatal, but each needs a decided answer before C ships, and several
are the concrete reason **B exists as the override.**

**P1 — Placement is path-dependent on history, and the "most members" tie-break can
flip.** The cohort lookup is computed against the repo *as it is now*. If a type
lives in two layouts at once (some ConfigMaps canonical, some bundled into
`all.yaml`), the winner is "most members" — but that count moves as the repo grows.
A repo that is 6-canonical / 5-bundled routes a new ConfigMap to canonical; after a
human bundles four more it is 6-canonical / 9-bundled and the *next* new ConfigMap
goes to the bundle. Same kind, different destination, purely because of *when* it
arrived. This instability is inherent to "infer from the repo" and cannot be fully
removed; it can be tamed: (a) surface the chosen cohort, its size, and the
tie-break in the scan/dry-run output so a flip is never silent, and (b) let a
cohort the user declared in **B outrank any inferred cohort**, so the unstable case
only exists where the user expressed no preference at all.

**P2 — Cold start and batch resync collapse to canonical.** On a fresh import or a
full resync the desired set is planned against a store that is empty (or mid-fill).
With no siblings, every resource falls to canonical — including a large initial
sync the user might have wanted bundled. Worse, if placement consulted a store that
mutates *within* a single plan, the first ConfigMap placed would become the sibling
for the rest, making a whole batch's layout depend on intra-batch ordering.
Decision: **resolve every cohort against the pre-plan store snapshot** and place a
batch together (reusing step 8's "group new creates by path"), so a batch is
order-independent and a cold start is deterministically canonical. The consequence
is blunt and must be stated: **C cannot bootstrap a layout that does not yet
exist.** A greenfield bundled layout happens only if the user declares it (B) or
seeds one file by hand first.

**P3 — The self-fulfilling canonical bias.** Because empty → canonical and siblings
then propagate, a repo the operator bootstrapped itself stays canonical *forever*
unless a human reorganizes it. C's benefit is therefore concentrated almost
entirely on the brownfield / human-authored / imported repo; for the dominant
"operator created the repo" path, C ≡ today's behaviour. That is a safe default,
not a defect — but nobody should expect bundling to *emerge* on its own, and the
user docs must say so.

**P4 — Step 2 cannot extend a custom per-namespace layout to an unseen namespace.**
Inference deliberately refuses to reverse-engineer a filename/segment template, so
when a brand-new namespace appears for a known type, step 1 (same type + namespace)
misses and step 2 (same type, any namespace) finds a cohort spread across
`…/<ns>/…` directories but cannot know the `<ns>` segment without inferring the very
template it swore off. For the **canonical** layout this is harmless — step 4's
fallback *is* the canonical per-namespace path, so the result is identical. For a
**custom** per-namespace layout (e.g. `{namespace}/configmaps.yaml`) the new
namespace cannot be inferred and lands canonical, breaking the user's pattern. This
is the single clearest reason **B is the override, not a nicety:** a custom
namespace-segmented layout must be *declared*; C can continue it for namespaces it
has already seen but cannot invent the segment for new ones. Document the boundary
so it is a known limit, not a surprise.

**P5 — Step 3 (same namespace, any type) can over-capture into a growing bundle.**
This is the most dangerous rung. A single heterogeneous bundle in a namespace can
become a sink that swallows every *new* type in that namespace just because they
share a namespace — a layout the user never asked for, growing without bound.
Mitigation: fire step 3 **only into a cohort that is already a single heterogeneous
bundle file** (the user demonstrably chose "one file for this namespace"), never to
scatter a new type into per-type files that merely happen to share a namespace. If
step 3 still feels too clever, **drop it entirely** — steps 1, 2, and 4 already
cover per-type bundles, per-type files, and canonical, and the namespace-bundle
layout can simply be required via B. (See the open question on whether step 3 ships
at all.)

**P6 — Delete-then-recreate can move a resource.** Existing documents are
match-first, so a live resource never moves while it exists. But a resource that is
deleted (its document swept) and later recreated is "new" again and re-inferred
against whatever the repo looks like *then*, which may differ from where it lived
before. Placement is create-time and non-retroactive — the same contract A and B
carry — but C makes "create-time" depend on mutable repo state, so this churn is
more visible. Acceptable, but call it out.

**P7 — An inferred path is still subject to the write-time ignore invariant.** A
resolved path — inferred or canonical — can collide with a `.gittargetignore`
pattern and trip the §4.3 `IgnoreShadowsManagedPath` precondition
([gitpath-foreign-content-stringency.md](../../gitpath-foreign-content-stringency.md)),
aborting the flush. Ignored and foreign files are never in the store, so they can
never *be* siblings (good) — but a new canonical-fallback path can still be
shadowed. C inherits this failure mode rather than creating it; the existing
precondition already handles it, and the diagnostic must name the inferred path and
the matching pattern.

**P8 — Explainability becomes a hard requirement, not a nicety.** With A/B a user
reads the CRD to know where a resource will go; with C the answer lives in the repo
plus a precedence ladder. The scan/dry-run output **must** state, per new resource,
the chosen path *and* the cohort + ladder step that produced it (e.g. "matched 9
ConfigMaps in `all.yaml` via step 1"). Without that, "why did it land there?" is
unanswerable. This is the one part of C that is genuinely *more* work than B, and it
should be treated as in-scope, not optional polish.

**P9 — Cohort lookup cost at scale.** Naively, each new resource scans the store
(O(store size)); a large repo × a big resync batch is O(N·M). Build the per-plan
indexes (by resource type, by namespace) once from the snapshot and resolve against
them. Minor, but real at cluster scale.

**P10 — It sharpens the stakes on exact indexing.** C trusts the store's
file↔identity map to decide *where new things go*, not only how existing ones are
edited. It is the same map match-first already depends on, so this is a sharpening
of an existing requirement rather than a new one — but a misindexed existing
document now also mis-routes future siblings, so the cost of an indexing bug is
higher under C.

### Validation and acceptance (reuses the existing gate)

- C adds **no policy to validate** — there is no template to parse in the base
  case. The only new runtime check is the sensitive backstop: a resolved sensitive
  path must be `.sops.yaml` and identity-complete (the same invariant A/B enforce),
  else fall back to canonical SOPS.
- The resolved path still passes the existing path validation (under `spec.path`,
  no `..`, correct suffix, inside discovery scope) and the existing
  plaintext-append acceptance (never partially manage a file).

### Keeping it small (C-specific limits)

- infer only **directory** + **single-file-vs-bundle**; never reverse-engineer a
  filename template beyond `{name}.yaml`;
- one fixed precedence ladder; no configurable matching;
- deterministic, documented tie-break; no per-resource status spam;
- sensitive never infers across the plaintext boundary;
- no retroactive moves when the repo layout changes — same rule as A and B.

## Sensitive placement and uniqueness

Sensitive placement should be stricter than normal placement. A normal template
may intentionally map many resources to one file because plaintext
multi-document append is supported. A sensitive template must not do that in the
first version.

The guarantee should be structural:

> Every accepted sensitive template must render an identity-complete path.

Identity-complete means the rendered path cannot collide for two distinct
sensitive resources in the GitTarget. There are two ways a template can prove
that:

1. The path contains the full API identity variables:
   `{groupPath}`, `{version}`, `{resource}`, `{namespaceOrCluster}`, and `{name}`.
2. The placement entry narrows to exactly one served resource type, and the path
   contains the scope identity for that type:
   `{namespace}` plus `{name}` for namespaced resources, or `{name}` for
   cluster-scoped resources.

For the type-map API, the `byType` key itself narrows to one type. For ordered
rules, "narrows to exactly one served resource type" means the rule names one API
group, one API version, and one resource, with no wildcard or omitted type field.

This rule is intentionally conservative. A user might know that
`{namespace}/secret-{name}.sops.yaml` is unique because they only watch core
Secrets, but the controller can only rely on that if the match proves it:

```yaml
placement:
  sensitiveRules:
    - match:
        apiGroups: [""]
        apiVersions: ["v1"]
        resources: ["secrets"]
      path: "{namespace}/secret-{name}.sops.yaml"
```

If the match does not narrow to one type, use the full identity path:

```yaml
placement:
  sensitiveRules:
    - path: "{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.sops.yaml"
```

Variable expansion must also be non-lossy for identity variables. Do not use a
sanitizer that turns two legal Kubernetes names into the same path segment.
Percent-encoding or another reversible path encoding is safer than lossy
replacement for `{groupPath}`, `{version}`, `{resource}`, `{namespace}`,
`{namespaceOrCluster}`, and `{name}`.

## Template variables

Templates should be small path templates, not a general programming language.
Branching belongs in `match`; path rendering belongs in `path`.

Use brace variables such as `{namespace}` rather than full Go templates. The
current commit-message templates already use Go templates, but file paths need
stricter validation and less expressive power. A dedicated path-template renderer
can validate every variable and every rendered segment before the write happens.

Recommended variables:

| Variable | Meaning |
|---|---|
| `{group}` | API group, empty for core resources |
| `{groupPath}` | API group as a path segment, omitted for core resources |
| `{version}` | API version |
| `{apiVersion}` | Kubernetes manifest `apiVersion` |
| `{resource}` | plural resource name, for example `configmaps` |
| `{kind}` | manifest kind, for example `ConfigMap` |
| `{scope}` | `namespaced` or `cluster` |
| `{namespace}` | metadata namespace, empty for cluster-scoped resources |
| `{namespaceOrCluster}` | namespace, or `cluster` for cluster-scoped resources |
| `{name}` | metadata name |
| `{sensitiveSuffix}` | `.sops.yaml` in sensitive rules, `.yaml` in normal rules |

With those variables, the built-in canonical normal layout is:

```text
{groupPath}/{version}/{resource}/{namespace}/{name}{sensitiveSuffix}
```

For a core `v1` ConfigMap named `app` in namespace `default`, empty path segments
are removed, so the canonical result is:

```text
v1/configmaps/default/app.yaml
```

For an `apps/v1` Deployment:

```text
apps/v1/deployments/default/app.yaml
```

For a Secret:

```text
v1/secrets/default/app.sops.yaml
```

Optional future variables can expose selected object metadata:

| Variable | Meaning |
|---|---|
| `{label:key}` | sanitized value of a metadata label |
| `{annotation:key}` | sanitized value of a metadata annotation |

Those are useful, but they should not be day-one unless there is a strong need.
Labels and annotations can change. Placement is create-time and non-retroactive,
so changing a label later would not move the file, but it can still surprise
users who expected the path to track metadata.

Do not expose arbitrary object fields such as `{spec.foo}` in the first version.
That makes path policy depend on mutable, schema-specific content and pulls the
placement layer into every CRD's structure.

## Path validation

The rendered path is always relative to `spec.path`. A rendered path must:

- be non-empty;
- be a clean relative path;
- stay under the GitTarget path after cleaning;
- not contain `..`, an absolute path, Windows drive prefixes, or empty final file
  names;
- end in `.yaml`, `.yml`, `.sops.yaml`, or `.sops.yml`;
- use sanitized path segments for every variable expansion;
- land inside the configured discovery scope.

The discovery-scope rule matters if `discovery.recurse: false` also lands. A
non-recursive GitTarget cannot create `namespaces/default/app.yaml`, because the
next scan would intentionally ignore that child folder. Either the placement rule
must render an immediate child file such as `default-app.yaml`, or the GitTarget
must enable recursive discovery.

Sensitive resources need one more invariant: if the selected resource is sensitive,
the final path must be a SOPS path. Because sensitive resources use
the sensitive placement class, the policy can validate this without guessing:

- every sensitive template must render `.sops.yaml` or `.sops.yml`;
- every sensitive template must be identity-complete;
- if sensitive placement is omitted, the controller uses the built-in secure
  canonical SOPS rule.

A Secret rule that renders `secrets/{name}.yaml` should fail validation or
reconciliation before any cleartext write is attempted.

## Collision and append behavior

Normal placement rules intentionally allow many resources to render to the same
file:

```yaml
placement:
  normalRules:
    - match:
        resources: ["configmaps"]
      path: "configmaps.yaml"
    - path: "all-else.yaml"
```

That means collision is not automatically an error. It is a request to create or
append a multi-document YAML file.

Plaintext rules:

- if the rendered file does not exist, create it;
- if several new plaintext resources in one plan render to the same path, write a
  multi-document file in deterministic resource-identity order;
- if the file already exists and is accepted managed KRM, append the new document
  when doing so does not create a duplicate identity;
- if the existing file is non-KRM YAML, invalid YAML, allowlisted auxiliary KRM,
  outside scope, or otherwise refused by acceptance, do not append;
- never partially manage a file. After append, every document in the file must be
  managed by the GitTarget.

Sensitive rules:

- sensitive resources remain single-document files for the first version;
- a sensitive rule that is not identity-complete is invalid;
- a sensitive rule that still maps two resources to the same path is a placement
  error;
- a sensitive resource must not be appended to a plaintext multi-document file;
- a plaintext resource must not be appended to a SOPS file.

That is stricter than SOPS can theoretically support, but it keeps the current
writer's invariant: encrypted documents are not patched in place and are handled
through the re-encrypt path. Multi-document encrypted append can be a later
explicit feature.

## Repository acceptance and validity

Placement policy feeds into the same acceptance model as the current manifest
design. A GitTarget must not reconcile when its repository folder cannot be
accepted as a fully managed projection.

The content acceptance gate remains responsible for:

- duplicate identities;
- non-KRM YAML in managed files;
- unwatched API-backed KRM;
- watched KRM outside target scope;
- mixed files that combine managed resources with retained allowlisted KRM.

Placement adds policy acceptance:

- the policy must be syntactically valid;
- custom placement classes must have defaults, or use the built-in defaults;
- every path template must reference only known variables;
- rendered paths for the current desired snapshot must pass path validation;
- sensitive resources must render to SOPS paths;
- sensitive templates must be identity-complete;
- sensitive collisions are refused;
- plaintext collisions are allowed only when they produce an accepted managed
  multi-document file.

A useful status split:

```text
Validated
PlacementPolicyValid
RepositoryValid
SnapshotSynced
EventStreamLive
Ready
```

`PlacementPolicyValid` catches invalid placement policy before any repository scan.
`RepositoryValid` catches content and rendered-placement problems discovered
against the checked-out tree and the desired snapshot.

If we want fewer conditions, `PlacementPolicyValid` can be folded into
`Validated`. The important behavior is the same: invalid placement policy blocks
snapshot sync and live event processing.

## Examples

### Type map with default

This is the likely first API shape:

```yaml
placement:
  sensitive:
    byType:
      v1/secrets: "{namespace}/secret-{name}.sops.yaml"
    default: "{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.sops.yaml"
  normal:
    byType:
      v1/configmaps: "{namespace}/configmaps.yaml"
    default: "all.yaml"
```

The keys are plural resource keys, so use `v1/secrets` and `v1/configmaps`, not
`v1/secret` or `v1/configmap`. A ConfigMap goes through `normal.byType` unless
the cluster/operator configuration classifies ConfigMaps as sensitive; in that
case it goes through `sensitive.byType` or `sensitive.default`.

### Namespace bundle with ordered rules

Group every namespaced resource into one file per namespace. Cluster-scoped
resources get their own bundle.

```yaml
placement:
  sensitiveRules:
    - path: "{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.sops.yaml"
  normalRules:
    - match:
        scope: Namespaced
      path: "namespace-{namespace}.yaml"
    - path: "cluster.yaml"
```

This is compact and friendly to humans, but it creates large multi-document files.
It is a good fit for small namespaces and a poor fit for clusters with hundreds
of resources per namespace.

### Secret isolation with ordered rules

```yaml
placement:
  sensitiveRules:
    - match:
        apiGroups: [""]
        apiVersions: ["v1"]
        resources: ["secrets"]
      path: "{namespace}/secrets/{name}.sops.yaml"
  normalRules:
    - path: "{groupPath}/{version}/{resource}/{namespace}/{name}.yaml"
```

This keeps sensitive resources one-per-file and leaves everything else in the
current canonical layout.

### ConfigMaps grouped with ordered rules

```yaml
placement:
  sensitiveRules:
    - path: "{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.sops.yaml"
  normalRules:
    - match:
        apiGroups: [""]
        resources: ["configmaps"]
      path: "{namespace}/configmaps.yaml"
    - path: "{groupPath}/{version}/{resource}/{namespace}/{name}.yaml"
```

This is a reasonable middle ground: only the low-risk, plaintext resource type is
bundled.

### Broad normal default

```yaml
placement:
  normalRules:
    - path: "all.yaml"
```

This affects only normal resources. It is valid for plaintext resources but
operationally heavy. Every edit touches one file, and a very large file becomes
harder to review, merge, and re-render. Sensitive resources still use the
built-in secure canonical fallback unless `sensitiveRules` is explicitly set.

### Brownfield import with no policy (Option C)

A user points a GitTarget at an existing folder and sets **no** `placement`. The
folder already looks like:

```text
clusters/prod/
  all.yaml                       # 9 ConfigMaps, multi-document
  v1/secrets/app/db.sops.yaml    # one Secret, encrypted, one-per-file
```

A new ConfigMap `cache` in namespace `app` arrives:

- step 1 finds the type-cohort "ConfigMaps", which lives entirely in `all.yaml` (a
  bundle) → the new document is **appended to `all.yaml`** — no `configmaps.yaml`
  guessed, no canonical tree created.

A new Secret `api-token` in namespace `app` arrives:

- it is sensitive, so plaintext siblings are ignored; the only sensitive cohort is
  `v1/secrets/app/` one-per-file → a new **`v1/secrets/app/api-token.sops.yaml`**.

A new ConfigMap in a *new* namespace `billing` arrives:

- step 1 misses (no `billing` ConfigMaps yet); the type-cohort is the `all.yaml`
  bundle, so it is **appended to `all.yaml`** too — the bundle is namespace-agnostic,
  so the new namespace needs no new segment.

Nothing was configured; the layout the user already had simply continued. The same
target with `placement.normal.byType: { "v1/configmaps": "{namespace}/configmaps.yaml" }`
(Option B) would instead route ConfigMaps into per-namespace files — B overriding
C where the user has an opinion.

## Keeping it small

The placement model can get too clever quickly. The first version should stay
inside these limits:

- GitTarget-level only;
- default to inference (C); declare a layout (B) only when inference cannot reach
  it (a layout that does not exist yet, or a custom per-namespace pattern that must
  extend to unseen namespaces — P4);
- C infers **directory + bundle-vs-file only** — never a filename or path-segment
  template (that is B's job);
- separate sensitive and normal rule lists;
- prefer exact type-map overrides plus defaults unless ordered matching proves
  necessary;
- when ordered matching exists, keep it first-match-wins only;
- no CEL expressions;
- no Go-template conditionals;
- no arbitrary object-field variables;
- no regex matching;
- no template functions except safe path-segment sanitization;
- no retroactive moves when rules change;
- no sensitive multi-document files;
- no per-resource status spam. Status should show bounded examples.

This still gives enough flexibility for the use cases that motivated the idea:
Secret-specific SOPS paths, ConfigMap bundles, namespace files, and a catch-all
layout.

## Implementation sketch

1. Settle the surface: **B is the declared API, C is the default.**
   - B (chosen): nested type map (`placement.sensitive.byType`,
     `placement.sensitive.default`, `placement.normal.byType`,
     `placement.normal.default`);
   - C (default): no API surface — it resolves against the content-derived store;
   - A: ordered `sensitiveRules` / `normalRules`, a later escape hatch only.
2. Add the CRD field:
   - `GitTargetSpec.Placement *GitTargetPlacementSpec`
   - the chosen nested type-map shape, or the ordered-rule shape
   - policy/path validation that can be done statically.
3. Introduce a placement policy interface in the writer/manifestreport layer:

   ```go
   type PlacementPolicy interface {
       LocateNew(resource types.ResourceIdentifier, objectMeta PlacementObjectMeta) (ManifestLocation, error)
   }
   ```

4. The default implementation is **Option C, not bare canonical.** Provide it as a
   `PlacementPolicy` that resolves against the store; with no `spec.placement` and
   an empty repo it falls through to `ResourceIdentifier.ToGitPath()`, so output is
   byte-identical to today, and once siblings exist it follows them. When B is
   present, B is consulted first and C fills every gap B leaves (canonical becomes
   C's step-4 fallback, not the whole policy). The C resolver must:
   - read cohorts from the **pre-plan store snapshot** only (P2), via per-plan
     by-type and by-namespace indexes (P9);
   - resolve a whole batch of new creates together so placement is
     order-independent (P2), reusing step 8's grouping;
   - never let a sensitive resource infer across the plaintext boundary (it uses
     other `.sops.yaml` siblings or the secure canonical SOPS fallback);
   - emit, for every new create, the chosen path plus the cohort and ladder step
     that produced it, into the scan/dry-run output (P8).
5. Parse and validate path templates once per GitTarget reconcile. Sensitive
   templates must be SOPS-suffixed and identity-complete. Type-map keys must
   parse to exact GVR keys. Store compiled templates in the resolved target
   metadata passed to the BranchWorker.
6. Replace calls to `filePathForIdentifier` / `generateFilePath` for new
   resources with `placement.LocateNew`.
7. Leave existing-document paths unchanged. `applyUpsert` still checks the store
   first and only calls placement when no document exists.
8. Add plaintext append support for same-path creates:
   - group new create actions by rendered path;
   - sort documents by resource identity for deterministic output;
   - write or append multi-document YAML only for accepted plaintext files.
9. Add sensitive collision checks before rendering encrypted bytes; this is a
   runtime backstop behind the static identity-completeness validation.
10. Feed placement errors into GitTarget status and the scan/dry-run output.
11. Update chart docs and examples after the API shape is settled.

## Tests

Unit tests:

- default placement reproduces `ResourceIdentifier.ToGitPath()` exactly;
- type-map keys parse as exact GVR keys, including core `v1/secrets` and grouped
  `apps/v1/deployments`;
- type-map default applies when no exact type entry exists;
- ordered-rule option: first matching rule wins;
- ordered-rule option: fallback rule catches resources not matched earlier;
- path validation rejects absolute paths, `..`, empty names, bad suffixes, and
  paths outside non-recursive discovery scope;
- core group removes the empty `{groupPath}` segment;
- sensitive resources require `.sops.yaml`;
- omitted sensitive placement still uses the built-in secure canonical fallback;
- sensitive templates that are not identity-complete are rejected;
- plaintext same-path creates produce deterministic multi-document YAML;
- sensitive same-path creates fail;
- existing moved manifests are updated in place and do not re-run placement;
- policy changes do not move existing files.

Option C (sibling inference) unit tests:

- empty repo reproduces `ResourceIdentifier.ToGitPath()` exactly (C ≡ canonical at
  cold start);
- a new resource whose type-cohort is a bundle is appended to that bundle file;
- a new resource whose type-cohort is one-per-file gets a new `{name}.yaml` beside
  the siblings;
- a sensitive resource never joins a plaintext bundle and uses the secure canonical
  SOPS path when only plaintext siblings exist;
- cohort tie-break is deterministic: most members, then lexically-smallest
  directory, then file (P1);
- a batch of new creates against an empty snapshot is order-independent — all
  canonical, regardless of input order (P2);
- a declared B entry outranks any inferred cohort (P1);
- a new namespace under a custom per-namespace layout falls back to canonical, while
  a new namespace under the canonical layout is unchanged (P4);
- the new file inherits its sibling's `NamespaceSource` (namespace omitted beside
  namespace-less kustomize-context siblings, written beside explicit-namespace
  ones);
- an inferred path that collides with a `.gittargetignore` pattern fails via the
  write-time precondition, naming the inferred path (P7).

Integration/e2e tests:

- a GitTarget with ConfigMaps grouped into `configmaps.yaml` creates and updates
  multiple ConfigMaps without duplicate files;
- Secret placement writes `.sops.yaml` and never creates cleartext Secret YAML;
- a namespace-bundle policy removes one document when the API resource is deleted
  and deletes the file only after the last managed document is gone;
- an invalid policy blocks `Ready` before live events are accepted;
- an external push that adds a duplicate identity still makes
  `RepositoryValid=False` and the controller does not guess which path to keep.

## Open questions

- For the ordered-rule option, should a custom rule list be required to end with
  an explicit catch-all, or should the controller append the canonical fallback
  implicitly? This document recommends explicit catch-all rules because they make
  the user's layout complete on the page.
- Should `{label:key}` and `{annotation:key}` ship in v1, or wait until somebody
  has a concrete use case?
- Should `discovery.recurse: false` survive the newer "whole folder ownership"
  model, or should flat discovery be dropped before placement rules land?
- Should placement rule matches include `watchRuleNames` later for users who want
  rule-origin-aware placement without moving policy onto WatchRule?
- For Option C, should step 3 (same namespace, any type) ship at all, or only fire
  into an already-heterogeneous bundle file (P5)? Dropping it keeps inference to
  per-type cohorts and canonical, and pushes the namespace-bundle layout onto B.
- Should C ever offer a one-time, opt-in "adopt/normalize" pass that *does* move
  existing files to a declared B layout, or is non-retroactive placement absolute?
  (Today both A/B and C never move existing files; this would be a deliberate,
  separate, destructive feature.)
- How much of C's cohort/ladder reasoning belongs in GitTarget *status* versus the
  scan/dry-run output (P8)? Status must stay bounded; the per-resource "why here"
  trace likely belongs only in the dry-run.
- When B and C disagree for a resource (B names a path, C would infer another),
  confirm B always wins and C only fills B's gaps — and that this precedence is
  visible in the dry-run.
