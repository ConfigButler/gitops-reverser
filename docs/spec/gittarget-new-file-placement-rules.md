# GitTarget new-file placement rules

> **spec** — current behaviour. The code depends on this document; change one, change the other. Index: [`../INDEX.md`](../INDEX.md)
>
> Status: implemented (v1 — Option B2: one `byType`/`default` placement map,
> with sensitivity treated as an internal write-safety classification rather than a
> separate user-facing placement namespace) + the kustomize-root fallback + the
> canonical path. **Option C (sibling inference) was implemented and then removed**;
> its sections below are kept as history because the argument is worth having on the
> page, and because the risks it enumerated are what the removal answers. The earlier
> B1 surface (a nested `sensitive:` override block) shipped first and was superseded
> by B2 on the same branch before release; Option A remains deferred. See
> "Sensitivity as a write-safety classifier (B2 implementation notes)" below for how
> the encryption guarantee is preserved without the API-level split.
> Captured: 2026-06-05. Option C removed: 2026-07-29.
> Related:
> [open-asks-priority.md](../design/open-asks-priority.md) — **the argument for deleting Option C**,
> [contextual-namespace-and-kustomize-folder-editing.md](contextual-namespace-and-kustomize-folder-editing.md),
> [gittarget-repository-validity-and-placement.md](gittarget-new-file-placement-rules.md),
> [current-manifest-support-review.md](current-manifest-support-review.md),
> [manifestedit-new-file-placement-spike.md](gittarget-new-file-placement-rules.md),
> [reconcile-via-watchlist-mark-and-sweep.md](reconcile-via-watchlist-mark-and-sweep.md),
> [gitpath-foreign-content-stringency.md](gitpath-foreign-content-stringency.md)

## Summary

> **What is live, in one paragraph.** A new resource — one with no document in Git yet —
> gets its path from the GitTarget's declared `placement.byType`/`placement.default`
> (Option B2); failing that, from the folder's one supported kustomization root, if it has
> exactly one; failing that, from the built-in canonical
> `{namespaceOrCluster}/{group}/{resource}/{name}.yaml` path. Nothing reads the layout of
> the other documents of the same type. Option C did exactly that and was deleted — the
> argument is in [`open-asks-priority.md`](../design/open-asks-priority.md) and summarised
> in [its own section below](#option-c-follow-the-existing-layout-sibling-inference--removed).
> Every resolution is counted by `placements_total{source,…}`, so a repository whose
> layout needs a declaration says so in a metric rather than in a folder.

New-resource placement should become an explicit GitTarget-level policy. There
are three viable shapes:

- **Option A: ordered rule lists** (`sensitiveRules` / `normalRules`), evaluated
  top to bottom.
- **Option B: type maps plus defaults**, using exact GVR keys such as
  `v1/secrets` and `apps/v1/deployments`. B has three surface variants below:
  the fully nested split, B1's top-level normal plus `sensitive` override, and
  B2's single map.
- **Option C: follow the existing layout (sibling inference)** — no policy at all;
  place a new resource where resources like it already live in the repo, and only
  fall back to canonical placement when there is no sibling to learn from.

A and B both make placement a *declared CRD policy*. C makes it a *continuation
of the layout already in the repo* — zero new API surface. They are not rivals;
they layer:

- **Option B is the declared API family, shipped in its B2 shape.** When a user
  wants to *prescribe* a layout, an exact type-map plus default is the surface to
  reach for — small, exact, easy to validate. The shipped shape is **one map**
  (`placement.byType`, `placement.default`) where type-specific entries express
  Secret placement just like any other resource placement. Sensitivity still
  exists, but as a write-safety classifier: it decides encryption,
  identity-completeness, and append/collision rules, not which config block to
  read. Ordered rules (A) stay a later escape hatch only if the type map proves
  too limiting.
- **Option C was the default underneath it, and is no longer.** With no policy,
  placement used to *follow the layout already in the repo*. That is what made "point me
  at an existing folder and it just works" a demo, and it is what made a human's edit to
  the repository silently change where the operator writes. It was removed; what fills
  B's gaps now is one structural fact (the folder's single kustomize root, if it has one)
  and then the canonical path. See
  [Option C … removed](#option-c-follow-the-existing-layout-sibling-inference--removed).

So the shipped shape is **B for the declared surface, the kustomize root where the
folder's own structure decides, and canonical otherwise.** The rest of this document
develops B; C's sections are retained as history, and its
[problems-and-risks](#problems-and-risks-with-option-c-as-written-before-the-removal)
list is the record of why inferring policy from mutable repo state was the wrong
foundation rather than a set of edges to keep filing down.

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
    byType:
      v1/secrets: "{namespace}/secret-{name}.yaml"
      v1/configmaps: "{namespace}/configmaps.yaml"
    default: "{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.yaml"
```

In this example:

- Secrets land in one identity-complete encrypted file per resource;
- ConfigMaps are grouped into `clusters/prod/configmaps.yaml`;
- every other new resource uses the identity-complete canonical-style fallback.

That is powerful enough to express layouts such as `namespace-{namespace}.yaml`,
per-kind bundles, Secret-specific paths, and the current canonical
`group/version/resource/namespace/name.yaml` layout. Sensitivity is still what
keeps this from becoming too sharp, but it is enforced after placement: a
sensitive write must be encrypted, identity-complete, and single-document in v1.
It does not need a separate placement namespace to say where the file belongs.

The pushback: fully ordered rules may be more API than we need first. A type map
is smaller, easier to validate, and still supports Secret-specific paths,
ConfigMap bundles, and a catch-all default. Ordered rules remain the flexible
escape hatch if users later need scope-wide or metadata-aware placement.

## Current implementation, as reviewed

The current `GitTargetSpec` has `providerRef`, `branch`, `path`, and optional
`encryption`; it has no placement policy yet
([api/v1alpha3/gittarget_types.go](../../api/v1alpha3/gittarget_types.go)).

The writer already uses the materialized-model direction described in
[current-manifest-support-review.md](current-manifest-support-review.md):

- steady-state writes scan the GitTarget subtree into a content-derived store,
  then apply a commit-scoped plan
  ([internal/git/plan_flush.go](../../internal/git/plan_flush.go));
- resync uses the same content-derived upsert path plus mark-and-sweep for
  managed orphans ([internal/git/resync_flush.go](../../internal/git/resync_flush.go));
- existing resources are found by manifest identity, so moved files are updated
  in place;
- new resources still fall back to `ResourceIdentifier.ToGitPath()`; sensitive
  resources are currently routed through the encrypted writer and commonly use
  the built-in `.sops.yaml` naming convention
  ([internal/types/identifier.go](../../internal/types/identifier.go),
  [internal/git/git.go](../../internal/git/git.go)).

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

### Option B1: one normal surface plus a sensitive override

There is a smaller variant of the nested type-map API that may be the better
first surface: make the common, plaintext placement policy the top-level shape,
and keep `sensitive` only as the guarded override.

```yaml
placement:
  byType:
    v1/configmaps: "{namespace}/configmaps.yaml"
  default: "all.yaml"
  sensitive:
    byType:
      v1/secrets: "{namespace}/secret-{name}.sops.yaml"
```

```go
type GitTargetPlacementSpec struct {
    ByType    map[string]string       `json:"byType,omitempty"`
    Default   string                  `json:"default,omitempty"`
    Sensitive GitTargetPlacementClass `json:"sensitive,omitempty"`
}

type GitTargetPlacementClass struct {
    ByType  map[string]string `json:"byType,omitempty"`
    Default string            `json:"default,omitempty"`
}
```

The semantics are:

- classify the resource first;
- sensitive resources consult `placement.sensitive.byType`, then
  `placement.sensitive.default`, then sibling inference, then the built-in secure
  canonical SOPS fallback;
- normal resources consult `placement.byType`, then `placement.default`, then
  sibling inference, then the built-in canonical plaintext fallback;
- a top-level `default` never applies to sensitive resources;
- any supplied sensitive template is still strictly validated as SOPS and
  identity-complete.

This makes the ordinary case read like what users mean: "put ConfigMaps here,
and put everything else normal in `all.yaml`." They do not have to learn a
`normal` wrapper before they can express the common case, and they do not feel
invited to provide two defaults. The sensitive block remains visible only where
the user wants to override the secure default.

Pros:

- fewer concepts in the common path: `placement.byType` and `placement.default`
  are enough for normal resources;
- the broad default is safer to explain, because it is explicitly a normal-only
  default rather than a default that needs a hidden exception for Secrets;
- sensitive placement remains hard-split and cannot be caught by a plaintext
  bundle such as `all.yaml`;
- it still leaves room for sensitive-specific fields later without putting them
  on every placement class.

Cons:

- the shape is slightly asymmetric: normal is implicit at the top level, while
  sensitive has an explicit block;
- future class-level fields for normal resources need top-level names, while the
  sensitive class has its own namespace;
- users who expect parallel classes may ask why there is `sensitive` but no
  `normal`;
- migration from the fully nested shape would require either accepting both
  shapes for a while or picking this before the field ships.

This is a useful intermediate shape: it keeps the security property that
motivated the split, but removes the awkward "two defaults" feel from the day-one
UX. Its weakness is that it still makes sensitivity part of the placement API.
That is not quite the model we want. Placement answers "where does this resource
go?"; sensitivity answers "what write rules apply once it gets there?"

### Option B2: one type map, sensitivity as write policy

B2 goes one step smaller: there is only one declared placement map. Users express
Secret placement by naming `v1/secrets` in `byType`, exactly like they express
ConfigMap or Deployment placement.

```yaml
placement:
  byType:
    v1/secrets: "{namespace}/secrets/{name}.yaml"
    v1/configmaps: "{namespace}/configmaps.yaml"
  default: "{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.yaml"
```

```go
type GitTargetPlacementSpec struct {
    ByType  map[string]string `json:"byType,omitempty"`
    Default string            `json:"default,omitempty"`
}
```

The semantics are:

- resolve placement from the single map: exact `byType` first, then `default`,
  then the folder's single kustomize root, then canonical fallback;
- independently classify the resource as sensitive or not;
- for a sensitive resource, require the selected path to be identity-complete;
- for a sensitive resource, write encrypted content and refuse multi-document
  append/collision in v1;
- for a non-sensitive resource, allow plaintext multi-document append only into
  files that are not classified encrypted;
- `.sops.yaml` is not required. It is a useful convention and may still be what
  the built-in canonical SOPS fallback chooses, but real GitOps repositories can
  contain SOPS-encrypted files named `secret.yaml`, and the controller should
  infer encryption from content/classification, not from filename alone.

The important safety rule moves from the API shape into validation:

- a `byType` entry for a sensitive type can be shorter because the type key
  supplies type identity; it still needs scope identity and name, for example
  `{namespace}/{name}.yaml`;
- a `default` that can catch sensitive resources must be identity-complete across
  type, scope, and name, for example
  `{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.yaml`;
- a broad bundling default such as `all.yaml` is valid only if it cannot catch
  sensitive resources, for example because every sensitive type watched by the
  target has an exact `byType` entry or because the target has no sensitive
  writes in scope. Otherwise the policy is unsafe and should fail validation or
  be reported as unused/ambiguous policy before writes start.

Pros:

- smallest user-facing API: one `byType`, one `default`;
- no duplicated "normal vs sensitive" mental model;
- matches how users already think about resource layouts: "Secrets go here,
  ConfigMaps go there";
- supports repositories that encrypt SOPS files without a `.sops.yaml` suffix;
- keeps safety attached to the write class, where it belongs: encrypted content,
  identity-complete paths, no sensitive append, no plaintext append into
  encrypted files.

Cons:

- validation needs to know, or conservatively approximate, which selected types
  are sensitive for this GitTarget;
- a broad `default: all.yaml` becomes invalid or limited when sensitive resources
  can reach it, so the error message must explain how to fix it with explicit
  `byType` entries;
- without the visual `sensitive:` block, documentation must be very clear that
  sensitivity still exists and still changes write rules;
- a future per-class option such as `allowMultiDocument` would need either a new
  field or a later rule-list surface.

**Decided and implemented: B2 is the declared API.** It keeps the good part of
B1 — one obvious common path — and removes the remaining API-level split.
Sensitivity stays load-bearing, but not as a second placement namespace. It is a
controller-owned write-safety contract: encrypt sensitive content, require
identity-complete placement, and refuse unsafe appends/collisions. The
`GitTargetPlacementSpec` CRD field is exactly `{byType, default}`; the earlier B1
`sensitive:` block was removed. How the encryption guarantee survives the removal
of the API-level split is written up in "Sensitivity as a write-safety classifier
(B2 implementation notes)" below.

The validation rules are almost the same as for ordered rules:

- omitted `placement.default` uses the kustomize-root fallback and then the built-in
  canonical fallback;
- every `byType` key must parse as a valid resolved type key;
- every referenced type should be served and watched by the GitTarget, or at
  least reported as unused policy;
- paths selected for sensitive resources must be identity-complete, unless the
  exact `byType` key itself narrows to one namespaced or cluster-scoped type and
  the path contains the scope identity plus name;
- paths selected for sensitive resources do **not** need to end in `.sops.yaml`
  or `.sops.yml`; suffix is convention, content classification is the truth;
- paths selected for plaintext resources may intentionally collide and append to
  plaintext multi-document files, but must not append to encrypted files.

The main loss is expressiveness. A type map cannot say "all namespaced resources
go to `namespace-{namespace}.yaml`, but cluster-scoped resources go to
`cluster.yaml`" unless every type is listed. It also cannot match future
metadata such as labels. If we do not need those patterns yet, this may be a
better first API than ordered rules.

Implemented shape:

1. the B2 type-map is **the** declared API — the smallest surface that covers the
   real "this type here, everything else there" need while keeping sensitivity as a
   write-safety rule;
2. when B is absent or silent for a resource, the fallbacks run: the folder's single
   supported kustomize root (a structural fact, so a new file is reachable from a render
   root), and otherwise canonical. Option C's sibling inference used to sit here and was
   removed;
3. ordered rules (A) remain a future extension only if users hit the type-map
   limit;
4. one template renderer, identity validation, encryption enforcement, and append
   rules serve all of it, so a declared template and a fallback path flow through one
   writer — and through one refusal gate, counted by one metric.

### Sensitivity as a write-safety classifier (B2 implementation notes)

Dropping B1's `sensitive:` block only removed the ability to give sensitive
resources a *different declared path*; it removed **no** part of the encryption
guarantee, because every piece of that guarantee already lived outside the
placement API and stays there under B2:

- **Encrypt by classification, not by path.** Whether a resource is written
  through the encrypted (SOPS) writer is decided by
  `types.SensitiveResourcePolicy` (core Secrets always; plus operator-configured
  types), independent of which path placement chose. A Secret routed to `all.yaml`
  is still encrypted.
- **The encrypted boundary is enforced at the write, not at resolution.** A sensitive
  resource whose path already holds any document is refused, and a plaintext resource
  routed at a file holding an encrypted document is refused — both read the document's own
  `CauseEncrypted` classification, and both are counted with a bounded reason. This is
  where the guarantee always actually lived: sibling inference had its own version of the
  rule, and deleting it removed a second implementation rather than the guarantee.
- **Sensitive never appends.** A sensitive resource whose resolved path already
  holds a document is refused (`finishPlacement`), never appended.
- **Canonical stays SOPS.** The built-in fallback keeps the `.sops.yaml` suffix for
  a sensitive resource.

What B1's API split *did* additionally provide — the guarantee that a Secret could
never reach a shared/plaintext file — is preserved by moving it from "structural
(two maps)" to "one static check plus two write-time guards", so it now holds for
**every** sensitive type (core and operator-configured), not just those the user
remembered to list in a `sensitive:` block:

- **Static (the Validated gate).** Core Secrets are always sensitive, so the
  spec-only validation can name them: an explicit `byType["v1/secrets"]` route must
  be identity-complete, and a bundling `default` (one that is not itself
  identity-complete, e.g. `all.yaml`) is rejected unless such a route exists — a
  Secret can never fall through a bundle. Additional sensitive types are operator
  configuration, invisible to a spec-only gate, so they rely on the write-time
  guards instead.
- **Write-time guard 1 — no cold-bundle mixing.** When several new resources in one
  batch collide on a brand-new path, the writer refuses to place a sensitive and a
  plaintext resource in the same file regardless of arrival order (the first wins,
  the other is skipped and retried).
- **Write-time guard 2 — no append into an encrypted file.** A plaintext resource
  is refused (not appended, and not overwritten) when its resolved path already
  holds an encrypted document.

The residual, deliberately accepted for v1: if an operator configures an
*additional* sensitive type and a GitTarget uses a bundling `default` without an
explicit `byType` entry for that type, resources of it are **skipped fail-safe**
rather than co-mingled — not written until the policy is fixed. As implemented, that
skip is **logged per-resource at the skip site and counted in the resync summary as
`placementSkipped`** (`ResyncStats.PlacementSkipped`); it is deliberately **not** a
dedicated GitTarget status condition in v1. So the observability claim is bounded:
the operator will not silently mirror the resource, and the skip is visible in logs
and the resync roll-up, but a reader watching only GitTarget conditions will not see
it. Core Secrets never hit this because the static gate rejects the policy up front.
Whether to teach the Validated gate about operator-configured sensitive types (so
this becomes a fast, up-front rejection there too), and whether to add a bounded
status surface for placement skips, are open questions below.

## The resolution ladder, as implemented

Placement only ever runs for a **new** resource — one with no document in the store yet
(match-first handles everything else, and never moves it afterwards). Three steps, in order,
and each one either answers or declines:

| Step | Mechanism | `placements_total{source}` | Decides because |
|---|---|---|---|
| 1 | declared `placement.byType`, then `placement.default` | `declared` | the GitTarget said so |
| 2 | the folder's single supported kustomization root | `kustomize_root` | a file that root cannot reach never renders |
| 3 | canonical `{namespaceOrCluster}/{group}/{resource}/{name}.yaml` | `canonical` | nothing else did |

Every resolved path — whichever step produced it — then passes one gate before a byte is
written: [path validation](#path-validation), the append-safety rules, and the
[write-safety refusals](#sensitive-placement-and-uniqueness). A refusal is a resource the
mirror does not hold, and it is counted as `placement_refusals_total{reason}` rather than
being merely logged.

### The kustomize-root fallback

The canonical path is a `{namespaceOrCluster}/{group}/{resource}/{name}.yaml` tree a
kustomization's `resources:` graph can never reach. So in a folder that kustomize builds,
a new document at the canonical path is not merely oddly placed — it is **never rendered**,
and nothing applies it. That is the failure new-file placement exists to prevent, and it is
why this step survived the Option C deletion.

When the whole *writable* subtree is governed by **exactly one** supported kustomization,
the new document is written beside that kustomization's other files and added to its
`resources:` list in the same commit (the product-level "add to the right kustomize file"
framing lives in
[`unreflectable-edits-and-write-gating.md`](../design/support-boundary/unreflectable-edits-and-write-gating.md)).

This is a **structural fact, not an inference**: the destination follows from there being
one root, not from picking the largest matching cohort of similar documents. Consequently:

- more than one supported kustomization under the scanned root is **ambiguous** and
  declines to canonical rather than guessing which root the resource belongs to;
- an *unsupported* kustomization is never a root (the writer must not edit one), and never
  a destination;
- under render-root scoping — where the scan reaches past `spec.path` into a base the
  overlay reads — "one supported kustomization" means one **writable** one, so an overlay
  resolves to its own root instead of declining because the read-only base counts as a
  second;
- if the `resources:` entry cannot be added, the file is committed outside every render.
  That is invisible in the folder, so it is counted:
  `placement_kustomization_entries_total{outcome="failed"}`.

### Namespace style follows the governing kustomization

A document in a directory whose kustomization sets a `namespace:` transformer does not carry
`metadata.namespace` — the build context supplies it (see
[contextual-namespace-and-kustomize-folder-editing.md](contextual-namespace-and-kustomize-folder-editing.md)).
A new document placed there must follow that convention, or it breaks the folder's own
style, and this applies to **every** resolved path: a declared template pointing into a
governed directory is under the same obligation as the kustomize-root fallback.

It is conditional on the two namespaces **matching**. Omitting `metadata.namespace` hands
the namespace to kustomize, so a transformer naming a *different* namespace would render the
document as a different object — the mirror would claim to hold a resource it does not. When
they differ the namespace is written explicitly, and the render oracle reports a folder that
cannot express the object rather than the write quietly diverging.

## Option C: follow the existing layout (sibling inference) — REMOVED

> **History.** Option C shipped (steps 1/2/4) and was removed on 2026-07-29. The full
> argument is in [`open-asks-priority.md`](../design/open-asks-priority.md), "The one real
> design call". The short form:
>
> - **It made a human's edit to the repository change the operator's behaviour**, with no
>   Kubernetes object changing and nothing in status recording the move. Delete enough of
>   one namespace's ConfigMaps from a bundle and the bundle stops being namespace-agnostic,
>   so the next new ConfigMap takes a different path. Nobody approved that.
> - **Its central guard failed by cascading.** The namespace-agnosticism check was, for a
>   period, vacuous on the singleton branch: one directory holding one namespace satisfied
>   "all the same directory" trivially, so a new namespace's object was appended into the
>   first namespace's file, which then genuinely spanned two namespaces, which legitimized
>   the bundle for every later object, which collapsed a whole type into one file. The fix
>   was right; the lesson is that a rule inferred from mutable state has failure modes that
>   feed themselves.
> - **The explainability it required (P8, below) was declared mandatory and never built.**
> - **What it bought was smaller than the demo suggested.** "Point me at an existing repo
>   and it just works" is carried by *match-first*, not by inference: every resource that
>   already has a document is edited exactly where it lives, forever. Inference only fired
>   for a type or namespace the target had never written — a rare event, and precisely the
>   one where a wrong guess is invisible because there is no prior file to compare against.
> - **And the layout most likely to be hand-authored — per-namespace segmentation — was the
>   one it could not extend anyway (P4).** The user had to declare it regardless.
>
> **What replaced it:** nothing, deliberately. A layout this operator cannot derive from one
> root is declared in `placement.byType`/`placement.default`, or it is canonical — and
> `placements_total{source="canonical"}` names the GitTarget and the type that needs the
> line, so "you need a declaration here" is a query rather than an archaeology exercise. No
> `spec.placement.mode` enum was added: an off-switch for a removed feature is a permanent
> API field bought to solve a temporary problem.
>
> The rest of this section is what the design said while it was live.

A and B both ask the user to *declare* the layout in the CRD up front, in a
template language, split into sensitive and normal. Option C asks for nothing. It
places a new resource **where resources like it already live in the repo.** The
folder is the policy.

### How it resolved a path

Placement only ever runs for a **new** resource — one with no document in the
store yet (match-first handles everything else). The content-derived store already
knows every accepted document, its file path, and its effective identity. C read
two independent facts straight off that store; it never reverse-engineered a
template.

1. **Which directory** — find the nearest *cohort* of existing documents that
   shares attributes with the new resource, most specific first:

   | Step | Cohort | Place in |
   |---|---|---|
   | 1 | same (resource type, namespace) | that cohort's directory |
   | 2 | same resource type, any namespace | that cohort's directory |
   | 3 | same namespace, any type | never implemented (P5) |
   | 4 | nothing matches | canonical `ToGitPath()` directory |

2. **One-per-file vs bundle** — look at how that cohort is stored:
   - cohort is **one resource per file** → create a new single-document file in
     that directory, named `{name}.yaml`;
   - cohort **shares a file** (a bundle such as `configmaps.yaml` or `all.yaml`) →
     append the new resource as a document to that same bundle.

Step 2 additionally required a candidate to **prove** it was namespace-agnostic before it
could be used for a namespace it had never seen — a bundle by already spanning two
namespaces, a singleton style by having every member in one shared directory that spans
two. That proof requirement was the fix for the cascade described above, and it is the part
worth remembering: it was correct, and it was not enough, because the feature's problem was
never the quality of its guard.

### Sensitive stayed hard-split — with no config

The sensitive/normal split that A and B get from two config blocks, C got for
free from the encryption classification already in the store: a sensitive resource never
inferred from plaintext siblings and was never appended to a plaintext bundle; it inferred
only from other encrypted siblings, and otherwise used the built-in secure canonical
fallback. That guarantee is unchanged by the removal — it is now enforced entirely by the
[write-safety refusals](#sensitive-placement-and-uniqueness), which is where it always
actually lived.

### Empty folder → canonical, then self-propagating

A freshly bootstrapped repo has no siblings, so the first resource of each kind
landed on canonical `ToGitPath()` — byte-identical to today. From then on the layout
propagated itself. This is why **cold-start repositories are unaffected by the removal**:
inference already fell to canonical there. The behaviour change is confined to a brownfield
folder that had a layout to continue.

### Determinism and ambiguity

A type can legitimately live in two layouts at once (some ConfigMaps bundled, some
canonical), so the lookup picked the cohort with the **most members**, tie-breaking on
lexically-smallest directory then file — stable and independent of walk order. The
reasoning at the time was that a "wrong but valid" location is cosmetic, since the document
is match-first the instant it exists. That is true of one document and false of a
convention: the wrong file is where every later resource of that type then goes.

### What it could not do, and how it composed

C could not express a layout you did not yet *have* — a greenfield "I want all ConfigMaps
bundled even though none exist yet" intent. That is what B is for, and B always outranked
C. It is also, in hindsight, the whole trade: B could express everything C could and say so
on the page.

### Problems and risks with Option C, as written before the removal

Sibling inference is powerful precisely because it reads its policy from mutable
repo state — which is also exactly where every one of its sharp edges comes from.
This list was written as "each needs a decided answer before C ships". Read in retrospect,
it is the case for the deletion: **P1, P2, P3, P4, P6 and P8 are all one property**, stated
six times, and only P7, P9 and P10 survive as facts about the code that remains.

**P1 — Placement is path-dependent on history, and the "most members" tie-break can
flip.** The cohort lookup is computed against the repo *as it is now*. A repo that is
6-canonical / 5-bundled routes a new ConfigMap to canonical; after a human bundles four
more it is 6-canonical / 9-bundled and the *next* new ConfigMap goes to the bundle. Same
kind, different destination, purely because of *when* it arrived. *Retired by the
deletion: the destination no longer depends on repo state at all.*

**P2 — Cold start and batch resync collapse to canonical.** With no siblings, every
resource falls to canonical — and a store mutating *within* one plan would have made a
whole batch's layout depend on intra-batch ordering. Decision at the time: resolve every
cohort against the pre-plan store snapshot. *The snapshot rule is still in force* for the
store reads that remain (does this path already hold an append-safe file; does its
directory carry a kustomization), so a batch is still order-independent.

**P3 — The self-fulfilling canonical bias.** A repo the operator bootstrapped itself stayed
canonical forever unless a human reorganized it, so C's benefit was concentrated on the
brownfield repo and was a no-op for the dominant path. *Retired: canonical is now simply
the documented answer, and the metric says when a declaration would be better.*

**P4 — Step 2 cannot extend a custom per-namespace layout to an unseen namespace.**
Inference refused to reverse-engineer a path segment, so a custom `{namespace}/…` layout
could not be continued into a namespace it had never seen and fell to canonical, breaking
the user's pattern. *This was the strongest argument for the deletion: the layout most
likely to be hand-authored was the one inference could not reach, so the user had to
declare it anyway.*

**P5 — Step 3 (same namespace, any type) can over-capture into a growing bundle.** A single
heterogeneous bundle could become a sink that swallowed every new type in a namespace.
*Never implemented, and now unreachable.*

**P6 — Delete-then-recreate can move a resource.** A resource whose document was swept and
which was later recreated was "new" again, and re-inferred against whatever the repo looked
like then. *Retired: a recreated resource resolves the same way it did the first time,
because the answer does not depend on the folder's history.*

**P7 — A resolved path is still subject to the write-time ignore invariant.** Any resolved
path — declared, kustomize-root, or canonical — can collide with a `.gittargetignore`
pattern and trip the §4.3 `IgnoreShadowsManagedPath` precondition
([gitpath-foreign-content-stringency.md](gitpath-foreign-content-stringency.md)), aborting
the flush. *Still live, and unrelated to inference: placement inherits this failure mode
rather than creating it.*

**P8 — Explainability becomes a hard requirement, not a nicety.** With A/B a user reads the
CRD to know where a resource will go; with C the answer lived in the repo plus a precedence
ladder, so the scan/dry-run output **must** state, per new resource, the chosen path and the
cohort and ladder step that produced it. *It was never built, which is a fact worth keeping
on this page: the feature shipped without the thing that made it defensible.* What exists
now is the smaller obligation the smaller ladder deserves — `placements_total{source}` per
(GitTarget, type), plus the per-resource log line naming the resolved path.

**P9 — Cohort lookup cost at scale.** Each new resource scanned the store, so a large repo
× a big resync batch was O(N·M). *Retired: there are no cohorts to index.*

**P10 — It sharpens the stakes on exact indexing.** C trusted the store's file↔identity map
to decide where *new* things go, not only how existing ones are edited, so a misindexed
document also mis-routed future siblings. *Partly retired: the map still decides
match-first and append safety, but a bad index can no longer propagate into a layout.*

### Validation and acceptance

- There is no inferred policy left to validate. A declared template is validated
  statically (see [Path validation](#path-validation)); the resolved path is validated at
  runtime on every resolution path, and the sensitive backstop (identity-complete,
  encrypted writer) is enforced at the write.
- The resolved path still passes the existing path validation (under `spec.path`, no `..`,
  correct suffix) and the existing plaintext-append acceptance (never partially manage a
  file).

## Sensitive placement and uniqueness

Sensitive writes should be stricter than normal writes. A normal template may
intentionally map many resources to one file because plaintext multi-document
append is supported. A sensitive resource must not do that in the first version,
regardless of whether the filename contains `.sops`.

The guarantee should be structural:

> Every placement selected for a sensitive resource must render an
> identity-complete path, and the content writer must produce encrypted content.

Identity-complete means the rendered path cannot collide for two distinct
sensitive resources in the GitTarget. There are two ways a template can prove
that:

1. The path contains the full API identity variables:
   `{groupPath}`, `{version}`, `{resource}`, `{namespaceOrCluster}`, and `{name}`.
2. The placement entry narrows to exactly one served resource type, and the path
   contains the scope identity for that type:
   `{namespace}` plus `{name}` for namespaced resources, or `{name}` for
   cluster-scoped resources.

For the type-map API, the `byType` key itself narrows to one type. For a broad
`default`, the template must carry the full API identity if it can catch any
sensitive resource. For ordered rules, "narrows to exactly one served resource
type" means the rule names one API group, one API version, and one resource, with
no wildcard or omitted type field.

This rule is intentionally conservative. A user might know that
`{namespace}/secret-{name}.yaml` is unique because they only watch core Secrets,
but the controller can only rely on that if the match proves it:

```yaml
placement:
  byType:
    v1/secrets: "{namespace}/secret-{name}.yaml"
```

If the match does not narrow to one type, use the full identity path:

```yaml
placement:
  default: "{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.yaml"
```

`.sops.yaml` and `.sops.yml` remain good conventions, and the built-in secure
canonical fallback may use them, but they are not required for correctness. Some
GitOps repositories use SOPS metadata inside ordinary `*.yaml` files. The
operator should classify encryption from file content and write behavior, not
from suffix alone.

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
| `{namespaceOrCluster}` | namespace, or `_cluster` (an illegal-namespace sentinel, so it never collides with a real namespace) for cluster-scoped resources |
| `{name}` | metadata name |
| `{sensitiveSuffix}` | Optional convention helper: `.sops.yaml` for sensitive writes, `.yaml` otherwise |

With those variables, the built-in canonical layout is **namespace-first, no
version segment** (as implemented in `ResourceIdentifier.ToGitPath`):

```text
{namespaceOrCluster}/{groupPath}/{resource}/{name}{sensitiveSuffix}
```

The scope leads (a real namespace, or the literal `_cluster` for a cluster-scoped
resource — an illegal Kubernetes namespace name, so it can never collide with a real
namespace) so a repository browses namespace-first; the group is omitted for core
resources, and the API version is deliberately left out — the operator writes one
version per object, so a version segment only adds noise and would churn the path on
a preferred-version bump. For a core `v1` ConfigMap named `app` in namespace
`default`, empty segments are removed, so the canonical result is:

```text
default/configmaps/app.yaml
```

For an `apps/v1` Deployment:

```text
default/apps/deployments/app.yaml
```

For a cluster-scoped resource the scope segment is the literal `_cluster`, e.g. a
ClusterRole `admin`:

```text
_cluster/rbac.authorization.k8s.io/clusterroles/admin.yaml
```

For a Secret under the suffix convention:

```text
default/secrets/app.sops.yaml
```

The equally valid suffix-neutral form is:

```text
default/secrets/app.yaml
```

(An earlier revision seeded a REST-first `{groupPath}/{version}/{resource}/
{namespace}/{name}` path; the namespace-first, version-less shape above replaced it
before release. Because placement is match-first for existing files, the change only
affects newly-created files and never moves one already in Git.)

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

Sensitive resources need one more invariant: if the selected resource is
sensitive, the selected path must be identity-complete and the write must produce
encrypted content. The filename does not prove encryption.

- every selected sensitive path must be identity-complete;
- a sensitive `byType` entry can rely on the type key for type identity, but must
  include scope identity and `{name}`;
- a `default` that can catch sensitive resources must include type identity,
  scope identity, and `{name}`;
- if no declared placement applies, sibling inference may follow existing
  encrypted siblings, otherwise the controller uses the built-in secure canonical
  fallback.

A Secret rule that renders `secrets/{name}.yaml` is only valid for cluster-scoped
secrets (which do not exist in core Kubernetes) or for a narrowed single
namespace. For ordinary namespaced core Secrets, it is not identity-complete
because two namespaces can both contain `name`.

## Collision and append behavior

Plaintext placement intentionally allows many resources to render to the same
file:

```yaml
placement:
  byType:
    v1/configmaps: "configmaps.yaml"
  default: "all-else.yaml"
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
- a sensitive resource must not be appended to any multi-document file;
- a plaintext resource must not be appended to a file classified encrypted,
  regardless of suffix.

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
- every path template must reference only known variables;
- rendered paths for the current desired snapshot must pass path validation;
- selected paths for sensitive resources must be identity-complete;
- selected paths for sensitive resources must be written encrypted, regardless of
  filename suffix;
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
  byType:
    v1/secrets: "{namespace}/secret-{name}.yaml"
    v1/configmaps: "{namespace}/configmaps.yaml"
  default: "{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.yaml"
```

The keys are plural resource keys, so use `v1/secrets` and `v1/configmaps`, not
`v1/secret` or `v1/configmap`. A ConfigMap can share
`{namespace}/configmaps.yaml`. A Secret can also be placed by the same `byType`
map, but the resolved path must be identity-complete and the writer must produce
encrypted content. The `.sops.yaml` suffix is not required; `secret-app.yaml`
can be encrypted SOPS YAML just as much as `secret-app.sops.yaml`.

### Namespace bundle with ordered rules

Group every namespaced resource into one file per namespace. Cluster-scoped
resources get their own bundle.

```yaml
placement:
  sensitiveRules:
    - path: "{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.yaml"
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
      path: "{namespace}/secrets/{name}.yaml"
  normalRules:
    - path: "{groupPath}/{version}/{resource}/{namespace}/{name}.yaml"
```

This keeps sensitive resources one-per-file and encrypted while leaving
everything else in the current canonical layout. A `.sops.yaml` suffix could be
used here as a repository convention, but it is not part of the safety contract.

### ConfigMaps grouped with ordered rules

```yaml
placement:
  sensitiveRules:
    - path: "{groupPath}/{version}/{resource}/{namespaceOrCluster}/{name}.yaml"
  normalRules:
    - match:
        apiGroups: [""]
        resources: ["configmaps"]
      path: "{namespace}/configmaps.yaml"
    - path: "{groupPath}/{version}/{resource}/{namespace}/{name}.yaml"
```

This is a reasonable middle ground: only the low-risk, plaintext resource type is
bundled.

### Broad B2 default

```yaml
placement:
  byType:
    v1/secrets: "{namespace}/secrets/{name}.yaml"
  default: "all.yaml"
```

This is valid because the sensitive type is explicitly covered by an
identity-complete path. The broad default then catches plaintext resources only.
If `v1/secrets` were not covered, `default: "all.yaml"` would be invalid for a
GitTarget that can write Secrets, because a sensitive resource could otherwise
land in a non-identity-complete bundle.

### Brownfield import with no policy

A user points a GitTarget at an existing folder and sets **no** `placement`. The
folder already looks like:

```text
clusters/prod/
  all.yaml                       # 9 ConfigMaps, multi-document
  v1/secrets/app/db.yaml         # one Secret, encrypted, one-per-file
```

A new ConfigMap `cache` in namespace `app` arrives. There is no declaration and no
kustomization, so it lands at the canonical path
**`clusters/prod/app/configmaps/cache.yaml`** — *not* appended to `all.yaml`. The same for a
new Secret, at the canonical encrypted path. The user's bundle is not extended, because
nothing told the operator it was a convention rather than a coincidence, and it counts:

```promql
sum by (gittarget_name, group, version, resource) (
  increase(gitopsreverser_placements_total{source="canonical"}[24h])
)
```

That series is the prompt to declare what the folder means:

```yaml
placement:
  byType:
    v1/configmaps: "all.yaml"
```

after which the same ConfigMap is appended to `all.yaml`, and the metric moves to
`source="declared", disposition="appended"`. **This is the behaviour change**: before the
Option C deletion the bundle was extended with no declaration at all. One line of YAML buys
back the old behaviour, and it says on the page what used to be a guess.

If instead the folder is a kustomize overlay — one `kustomization.yaml` governing the whole
subtree — no declaration is needed: the new document lands beside it and joins its
`resources:` list, `source="kustomize_root"`, because a file that root cannot reach would
never be applied.

## Keeping it small

The placement model can get too clever quickly. The first version should stay
inside these limits:

- GitTarget-level only;
- three resolution steps and no more: declared, the folder's single kustomize root,
  canonical. Nothing reads the layout of other documents to guess an intent (that was
  Option C, removed);
- a layout the ladder cannot reach is **declared**, not inferred — and the
  `placements_total{source="canonical"}` series is how its absence is noticed;
- keep sensitivity as an internal write-safety classifier, not a second public
  placement namespace;
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
Secret-specific encrypted paths, ConfigMap bundles, namespace files, and a
catch-all layout.

## Implementation sketch

1. Settle the surface: **B2 is the declared API; the fallbacks carry no API at all.**
   - B2: one top-level type map (`placement.byType`) plus one
     `placement.default`; sensitivity is applied as write policy after placement
     resolves;
   - the kustomize-root fallback and the canonical path: no API surface, and no
     off-switch — see the note on `spec.placement.mode` in
     [`open-asks-priority.md`](../design/open-asks-priority.md);
   - A: ordered `sensitiveRules` / `normalRules`, a later escape hatch only, not
     implemented.
2. Add the CRD field:
   - `GitTargetSpec.Placement *GitTargetPlacementSpec`
   - the B2 type-map shape (`ByType`/`Default` at top level)
   - policy/path validation that can be done statically.
3. Introduce a placement policy interface in the writer/manifestreport layer:

   ```go
   type PlacementPolicy interface {
       LocateNew(resource types.ResourceIdentifier, objectMeta PlacementObjectMeta) (ManifestLocation, error)
   }
   ```

4. The fallback is the **kustomize root, then bare canonical.** B is consulted first;
   with no `spec.placement` the resolver looks for exactly one supported writable
   kustomization and otherwise returns `ResourceIdentifier.ToGitPath()`. It must:
   - read the store from the **pre-plan snapshot** only, so a batch of new creates is
     order-independent — one new resource must never become another's context (P2);
   - never let a sensitive resource join a plaintext file, or a plaintext resource an
     encrypted one; refuse instead, with a bounded reason;
   - count every resolution by `{source, disposition}` and every refusal by `{reason}`,
     labelled with the GitTarget and the type, which is the smaller obligation that
     replaces P8's never-built cohort trace.
5. Parse and validate path templates once per GitTarget reconcile. Sensitive
   resources must resolve to identity-complete paths and must be written through
   the encrypted writer; the selected filename suffix is not the contract.
   Type-map keys must parse to exact GVR keys. Store compiled templates in the
   resolved target metadata passed to the BranchWorker.
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
10. Surface placement outcomes. **Implemented as:** every resolution increments
    `gitopsreverser_placements_total{source, disposition, gittarget_namespace,
    gittarget_name, group, version, resource}` and every refusal
    `gitopsreverser_placement_refusals_total{reason, …}` with the same target/type labels;
    a new file's `resources:` entry is counted by
    `gitopsreverser_placement_kustomization_entries_total{outcome}`, whose `failed` value
    is the otherwise-invisible "committed but outside every render". Each fail-safe skip
    is still logged per-resource at the skip site and counted in the resync summary
    (`ResyncStats.PlacementSkipped`, distinct from the planner's `Skipped`). A GitTarget
    status surface (`status.layout`) remains future work — see
    [`open-asks-priority.md`](../design/open-asks-priority.md), B2.
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
- sensitive resources do not require `.sops.yaml`;
- sensitive resources require identity-complete selected paths;
- sensitive resources are written encrypted regardless of filename suffix;
- an unmatched sensitive resource still uses the built-in secure canonical
  fallback;
- a broad default such as `all.yaml` is rejected if it can catch sensitive
  resources;
- plaintext same-path creates produce deterministic multi-document YAML;
- sensitive same-path creates fail;
- existing moved manifests are updated in place and do not re-run placement;
- policy changes do not move existing files.

Resolution-ladder unit tests:

- an empty repo reproduces `ResourceIdentifier.ToGitPath()` exactly;
- **every layout sibling inference used to read resolves canonical** — a bundle of the same
  type in the same namespace, one-document-per-file, a per-namespace bundle, a directory per
  namespace, a single directory holding one namespace, a bundle spanning two namespaces, and
  a shared directory spanning two. One table, because it is one contract: the destination
  does not depend on where the other documents of the type live;
- the `kube-root-ca.crt`-shaped case keeps its own named test: an object that exists under
  the same name in every namespace is never filed onto the first namespace's file, which is
  the cascade the deletion retires;
- a declared entry outranks both fallbacks;
- a batch of new creates against one snapshot is order-independent;
- two supported kustomizations are ambiguous and fall to canonical; one supported
  kustomization places beside it and reports the `resources:` entry to add; an unsupported
  one is never edited or used as a root;
- namespace inheritance: omitted when the governing kustomization's `namespace:` matches the
  resource's own namespace (for a declared path as well as the kustomize-root one), written
  explicitly when it names a different namespace, and never claimed with no context;
- a sensitive resource never joins a plaintext file and never reuses an existing
  `.sops.yaml` directory without a declaration;
- a declared path onto a file holding a non-editable construct never appends;
- refusals carry their bounded reason, and each is counted: an escaping template
  (`invalid_path`), a sensitive collision (`sensitive_append`), a plaintext resource routed
  at an encrypted file (`plaintext_onto_encrypted`), and mixed sensitivity onto one new file
  (`mixed_sensitivity_new_file`);
- a resolved path that collides with a `.gittargetignore` pattern fails via the write-time
  precondition, naming the path (P7).

Metric tests (`internal/git/placement_metrics_test.go`), driving the real write path:

- a canonical fall-back is labelled with the GitTarget and the type key a `byType` line
  would name — the labels are the feature, so they are asserted rather than the count alone;
- declared and kustomize-root placements are distinguishable from canonical, and
  `kustomize_root` is never counted as a fall-back;
- a declared bundle records `disposition="appended"`;
- a refused resource is counted as a refusal and **never also** as a placement;
- a `resources:` entry that could not be added is counted as `failed`;
- a resync's create carries the same target labels as a live create.

Integration/e2e tests:

- a GitTarget with ConfigMaps grouped into `configmaps.yaml` creates and updates
  multiple ConfigMaps without duplicate files;
- Secret placement writes encrypted YAML and never creates cleartext Secret YAML,
  even when the configured path ends in ordinary `.yaml`;
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
- ~~For Option C, should step 3 (same namespace, any type) ship at all (P5)?~~ **Closed by
  the removal**: step 3 was never implemented and the ladder it belonged to is gone.
- ~~When B and C disagree, confirm B always wins.~~ **Closed**: there is nothing left to
  disagree with. A declaration is consulted first and the fallbacks are structural.
- Should a one-time, opt-in "adopt/normalize" pass ever move *existing* files to a declared
  layout, or is non-retroactive placement absolute? Today nothing moves an existing
  document; this would be a deliberate, separate, destructive feature. The removal makes it
  more interesting, not less: a brownfield folder that wants its bundle back declares it,
  and the documents already at canonical paths stay where they are.
- How much of the "why here" answer belongs in GitTarget *status* rather than in a metric?
  `placements_total{source}` says which (target, type) fell back; it does not say what the
  operator understood about the folder, and it expires with the scrape window. That is what
  `status.layout` (B2 in the config-surface doc) is for, and the two are complements: a
  status field is what a `kubectl get -o yaml` in a bug report contains.
- Whether the first canonical fall-back for a (target, type) should also raise a
  `corev1.Event` on the GitTarget for timeliness. Placement runs on the branch worker, which
  has no recorder and no reconcile context, so this is a hand-back over the existing
  refusal→condition seam rather than a one-line addition — see the same section of
  [`open-asks-priority.md`](../design/open-asks-priority.md).
- Should the Validated gate learn about operator-configured *additional* sensitive
  types (beyond core Secrets) so a bundling `default` that could catch one is
  rejected up front, instead of relying on the write-time guards to skip those
  resources fail-safe at commit time? Doing so means threading the operator's
  sensitive-resource configuration into GitTarget validation, which today is
  spec-only. For v1 the write-time guards cover the safety; this would only upgrade
  a fail-safe skip into a faster, more visible up-front error.
