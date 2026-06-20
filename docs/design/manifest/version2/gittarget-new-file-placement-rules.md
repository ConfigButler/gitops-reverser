# GitTarget new-file placement rules

> Status: proposed
> Captured: 2026-06-05
> Related:
> [gittarget-repository-validity-and-placement.md](gittarget-repository-validity-and-placement.md),
> [current-manifest-support-review.md](../../../finished/current-manifest-support-review.md),
> [manifestedit-new-file-placement-spike.md](../../../finished/manifestedit-new-file-placement-spike.md),
> [reconcile-via-watchlist-mark-and-sweep.md](../reconcile-via-watchlist-mark-and-sweep.md),
> [per-type-reconcile-and-streaming-tail.md](per-type-reconcile-and-streaming-tail.md)

## Summary

New-resource placement should become an explicit GitTarget-level policy. There
are two viable shapes:

- **Option A: ordered rule lists** (`sensitiveRules` / `normalRules`), evaluated
  top to bottom.
- **Option B: type maps plus defaults** (`sensitive.byType` / `normal.byType`),
  using exact GVR keys such as `v1/secrets` and `apps/v1/deployments`.

The current recommendation is to ship the nested type-map API first. It covers
the common "this type goes here, everything else goes there" case with less
surface area, while keeping ordered rules as a future extension if exact type
lookups are too limiting.

Existing manifests are still match-first: once a resource already has a document
in Git, updates and deletes use that document's current location instead of
re-running placement.

This keeps the useful part of the older `newFilePath` proposal, but makes the
per-type policy explicit:

```yaml
apiVersion: configbutler.ai/v1alpha2
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
([api/v1alpha2/gittarget_types.go](../../../../api/v1alpha2/gittarget_types.go)).

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

1. ship the nested type-map API first;
2. keep ordered rules as a future extension only if users hit the type-map limit;
3. keep the same template renderer, SOPS validation, and append rules for both.

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

## Keeping it small

The placement model can get too clever quickly. The first version should stay
inside these limits:

- GitTarget-level only;
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

1. Choose the API surface:
   - preferred first version: nested type map (`placement.sensitive.byType`,
     `placement.sensitive.default`, `placement.normal.byType`,
     `placement.normal.default`);
   - more flexible fallback: ordered `sensitiveRules` / `normalRules`.
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

4. Keep the current canonical policy as the default implementation. With no
   `spec.placement`, output must be byte-identical to today's
   `ResourceIdentifier.ToGitPath()` behavior.
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
