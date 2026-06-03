# Manifest inventory for file-agnostic placement

> Status: proposed (vision)
> Related: [file-agnostic-placement.md](file-agnostic-placement.md),
> [manifest-parser-poc.md](manifest-parser-poc.md),
> [bi-directional.md](../bi-directional.md)

This is the vision document. It holds the requirements and the bigger "why".
The first concrete step is scoped separately in
[manifest-parser-poc.md](manifest-parser-poc.md).

## Summary

GitOps Reverser currently treats the generated file path as part of the storage
contract. A Kubernetes resource is written to a deterministic path derived from
its API identity, and Git state is discovered by parsing that path back into a
resource identifier.

That is simple, but it makes GitOps Reverser hard to attach to existing
repositories. Kubernetes manifests already carry their API identity in
`apiVersion`, `kind`, `metadata.name`, and, for namespaced resources,
`metadata.namespace`. The file path should be placement metadata, not the source
of truth for identity.

The proposed direction is to introduce a manifest inventory layer:

- scan a target folder for YAML manifests
- parse Kubernetes resources from their content
- map each resource identity to its exact file location
- update existing resources in place
- use a configurable placement policy only when a new resource has no known
  location yet

## Why this matters

Existing GitOps repositories rarely follow GitOps Reverser's current generated
layout. They may use application folders, environment overlays, namespace
folders, multi-document YAML files, bootstrap files, or generated output from a
larger delivery pipeline.

If GitOps Reverser can only write:

```text
{group}/{version}/{resource}/{namespace}/{name}.yaml
```

then it can mirror resources into Git, but it cannot comfortably become a GitOps
API for an existing repository. A file-agnostic placement model lets users point
at a folder and have GitOps Reverser discover what is already there.

## Principle

Resource identity comes from Kubernetes object identity. File path is an
implementation detail of where that object is stored.

For a normal manifest this means:

- `apiVersion` and `kind` identify the Kubernetes type as written in YAML
- API discovery / RESTMapper maps that GVK to the watched GVR
- `metadata.name` identifies the object name
- `metadata.namespace` identifies the namespace for namespaced resources

Namespace elision is a special case. If a manifest omits
`metadata.namespace`, then the YAML content alone no longer fully identifies the
live object. The missing namespace must come from an explicit context, such as a
GitTarget setting or a very small supported Kustomize namespace rule. That should
be treated as contextual identity, not as pure content identity.

For the first implementation, keep writing `metadata.namespace`. Namespace
elision can come later behind an explicit GitTarget setting, such as
`writeNamespace`, after the contextual identity rules are clear.

## Authority model

The API remains leading. The manifest inventory changes how GitOps Reverser
finds and edits files, but it does not change the reconciliation direction.

On initial reconcile, GitOps Reverser expects the normal GitOps system, user, or
managing layer to have already synced valid KRM from the target folder into the
watched Kubernetes API surface. GitOps Reverser then compares:

- valid watched KRM found in Git
- resources currently present in the watched Kubernetes API

The reconciliation behavior stays the same as today:

- if a watched resource exists in the API but not in the Git folder, add it to
  Git using the placement policy
- if valid watched KRM exists in Git but is not present in the API, remove it
  from Git
- if a resource exists in both places, update the Git manifest from the API
  state while preserving the existing file location and formatting where safe

Only watched GVRs participate in add/remove. Documents and files for unwatched
types are never pruned and are left untouched.

### Initial-reconcile prune risk

The remove rule has a real risk worth stating plainly: on initial reconcile,
valid watched KRM that exists only in Git is deleted. If the surrounding system
has not yet synced the source-of-truth manifests into the watched API, those
files look like deletions and are removed even though they should have stayed.

GitOps Reverser does not try to make this safe on its own, and that is
intentional. The API is leading; the orchestration around GitOps Reverser must
sequence initialization so the watched API is populated before reverse pruning
runs. This tooling must be orchestrated well to prevent disasters. Making the
prune path self-protecting is explicitly out of scope for this feature.

### No separate reconciliation-state layer

Because the API is leading and almost every change originates from GitOps
Reverser itself, the design must converge to "no change" on repeated reconciles
without keeping a separate persisted projection or shadow state. The guard
against edit oscillation (a commit that is immediately reverted on the next
reconcile) is a single, consistent desired-content projection plus semantic
no-op detection, validated by tests — not a stateful reconciliation layer. In
practice the existing bi-directional patterns have not shown this oscillation.
Keeping extra state is not forbidden, but it is explicitly not the first measure.

## Encrypted manifests (SOPS)

GitOps Reverser already manages SOPS encryption (the `.sops.yaml` bootstrap and
the existing encryptor path). File-agnostic placement has to coexist with
encrypted manifests, and the rules here are opinionated and load-bearing — they
will make or break the implementation.

- **Detection is by file extension for now.** A file is treated as SOPS-managed
  based on its extension (the existing encrypted-file convention). Anything more
  precise is deferred.
- **Partial encryption only.** An encrypted manifest must keep its identity
  fields — `apiVersion`, `kind`, `metadata.name`, `metadata.namespace` — in
  cleartext. Only data-bearing fields (`data`, `stringData`, and similar, per
  the `.sops.yaml` rules) are encrypted. The inventory reads identity straight
  from the file, exactly as it would for a plaintext manifest.
- **Must have readable identity and a `sops` key, or it is invalid.** A
  SOPS-managed file must have both readable cleartext identity and a `sops` key.
  If either is missing — including full-file encryption that hides the identity —
  the file is flagged invalid and ignored with a diagnostic. This is a hard
  requirement, not best effort.
- **One document per encrypted file.** Encrypted files are single-document.
  Multi-document handling does not apply to them.
- **No decryption, ever.** GitOps Reverser never decrypts SOPS material. It does
  not read, compare, or preserve encrypted values. This is a deliberate security
  measure: the reverser should not need the decryption keys to do its job.
- **Re-encrypt on write.** Because it cannot read the existing ciphertext, the
  writer renders the desired object and re-encrypts the whole file through the
  existing encryptor on initial reconcile and on every write.
- **No formatting preservation for encrypted files.** Re-encryption rewrites the
  file, so comment and scalar-style preservation goals do not apply to encrypted
  content. This is an accepted, intentional trade-off.
- **Optional caching to avoid spurious commits.** Re-encrypting always produces
  fresh ciphertext, which would otherwise look like a change on every reconcile.
  An optional cache of the last-encrypted desired plaintext (by identity) lets
  the writer skip a real commit when the underlying object has not changed. This
  is an optimization, not a correctness requirement.

The net effect: strong security posture (no keys needed to read secrets) in
exchange for losing formatting fidelity on encrypted files only.

Open questions are tracked in [Remaining questions](#remaining-questions).

## Manifest inventory

The inventory is an index built from a GitTarget folder. It can start as
process-local state. It only needs to be rebuilt when the GitTarget is
initialized, when the tracked branch moves externally, or when the local worktree
is refreshed from an incoming remote change. Normal API-driven writes can update
the inventory incrementally as part of the write.

### Manifest identity and resource identity

The inventory deals with two views of the same object, and the document keeps
them distinct on purpose:

- **Manifest identity** is what is written in the YAML: `apiVersion` and `kind`
  (a GroupVersionKind), plus `metadata.name` and, for namespaced resources,
  `metadata.namespace`. This is content identity — what a human reads in the
  file.
- **Resource identity** is the API-side key GitOps Reverser already uses on the
  watch/reconcile path: group, version, resource (a GroupVersionResource), plus
  namespace and name. The manifest GVK is mapped to the watched GVR through API
  discovery / RESTMapper.

So manifest identity is the on-disk representation and resource identity is the
normalized API key the inventory indexes by. The inventory's core job is to keep
the mapping `resource identity -> file location` while remembering the manifest
identity that produced it.

Each discovered resource should record:

- resource identity: group, version, resource, namespace, name
- manifest identity: apiVersion, kind, namespace, name
- file path relative to the GitTarget path
- document index inside that file
- byte range or AST node location for the document, if available
- whether the manifest came from plain YAML, basic Kustomize context, or an
  unsupported source
- diagnostics such as duplicate identity, invalid YAML, unsupported template, or
  missing namespace context

This changes the write decision from:

```text
resource id -> generated path
```

to:

```text
resource id -> existing inventory location, else placement policy
```

### Document index is authoritative

GitOps Reverser is in control of the Git side. Almost every write originates
from it, so it updates the document index as part of writing, and any external
change to the branch triggers a full rescan that rebuilds the inventory from
scratch. Within a given inventory state the document index is therefore
authoritative: the writer locates the document to edit directly by its index and
does not need to re-derive the position from manifest identity on every edit.

### Rebuild and rescan

The rescan stays deliberately simple. A rebuild is triggered when something
changed under the GitTarget base path. The cheap gate is "did anything in the
base folder change" — if nothing changed, do nothing. If something did, do a
full rebuild. Because the API is leading and most changes originate from GitOps
Reverser itself, a full rebuild on change is acceptable and we do not want
anything smarter than the change gate. The rescan must stay doable and no more
advanced than strictly needed.

### Untrusted input and disallowed constructs

The folder is parsed as untrusted input, so the scan must be bounded:

- **Alias / anchor expansion bombs.** YAML anchors (`&x`) and aliases (`*x`),
  and merge keys (`<<`), allow a small file to reference nodes repeatedly and
  expand exponentially when fully materialized — the classic "billion laughs"
  YAML bomb that can exhaust CPU and memory. These constructs are also unsafe to
  edit through. Anchors, aliases, and merge keys go on a disallow list: a file
  using them is ignored as non-editable, with a diagnostic, and must not be
  fully materialized.
- **Symlink traversal.** A symlink under the base folder could point outside the
  tree or form a cycle. The scan does not follow symlinks; it skips them.

Both behaviors must have tests.

## Edit preservation

File-agnostic placement must be more careful than the current canonical writer.
If a user has comments, document ordering, blank lines, or nearby resources in a
YAML file, GitOps Reverser should avoid rewriting the entire file when it only
needs to update one object.

The target behavior:

- preserve comments when possible
- preserve unrelated YAML documents in the same file byte-for-byte
- update only the matching document in a multi-document YAML file
- detect whether the desired object is semantically different before writing
- when possible, update only the changed fields instead of re-rendering the
  whole object
- delete only the matching document on resource deletion
- avoid changing sibling resources in the same file
- preserve scalar style for unchanged values, especially block strings
- keep existing file names and folder structure for known resources
- fall back to canonical YAML only for newly placed resources or cases where
  round-trip preservation is not possible

A pure whole-file round-trip through `yaml.v3` cannot meet the byte-for-byte
requirement for unrelated documents, because re-encoding a whole file normalizes
indentation, quoting, and spacing across every document. So the baseline is
decided: split a file into its documents textually and only re-render the one
document that changed; unrelated documents are spliced back verbatim. The
in-document editor and its parser are the subject of
[manifest-parser-poc.md](manifest-parser-poc.md).

## Editing strategy: structural merge

GitOps Reverser already avoids commits when existing and desired manifests are
semantically equal. File-agnostic placement should go one step further: when a
resource does change, touch only the parts of the YAML document that changed.

The mechanism is a **structural merge of the desired object onto the existing
document's node tree**, not a "compute a diff, then map field paths back to
nodes" pass. Path-string-to-node mapping is the brittle part (sequences, merge
keys, ambiguous matches); walking both trees together keeps the target node in
hand at every step, and add, update, and delete all fall out of the same
traversal:

- for a mapping: for each key in the desired object, find the matching key in
  the existing node
  - both are maps or sequences → recurse
  - scalar and unchanged → leave the existing node untouched (its style and
    comments are preserved for free)
  - scalar and changed → update the value, keeping the existing scalar style
    when the new value is safely representable in it, otherwise let the encoder
    choose
  - key absent in the existing node → insert it
  - key present in the existing node but absent in the desired object → delete it
- for sequences, start with index-based alignment; Kubernetes-aware keyed
  matching (for example by container `name`) is a later nicety
- a merge that mutates no node is a semantic no-op and produces no write

Because the merge only ever visits nodes that exist in the desired object,
unrelated nodes are preserved by construction. A common example is a ConfigMap
containing a script:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: startup-scripts
  namespace: default
data:
  start.sh: |-
    #!/bin/sh
    set -eu

    echo "starting app"
    exec /app/server
```

If the only change is `metadata.labels.app: demo`, the merge walks
`metadata` → `labels`, inserts one key, and never descends into `data.start.sh`.
The block scalar style, indentation, blank lines, and chomping indicator (`|-`)
survive because that node is never visited. Rewriting it as an escaped one-line
string, a folded block, or a differently chomped literal block would make the
Git diff noisy and damage readability.

This does not require preserving every byte of an edited field. It requires that
unrelated fields — especially human-authored strings such as scripts,
certificates, templates, and policy snippets — are not reformatted as collateral
damage. When a node cannot be merged unambiguously, fall back to replacing the
whole document with a diagnostic.

## Multi-document files

YAML files can contain multiple Kubernetes resources separated by `---`. The
inventory must treat each document as a distinct editable slot.

Example:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
```

If the Deployment changes, only the second document should be updated. The
ConfigMap document should be preserved, including comments and formatting when
possible.

Deletion should remove just the target document and leave a valid YAML file. If
the deleted document was the only document in the file, the file can be removed.

## Duplicate resources

The same Kubernetes object may appear in more than one file, or more than once
in a multi-document file. Because the API is leading and there must be exactly
one source location per resource, GitOps Reverser resolves this deterministically
instead of refusing to act: **the first occurrence wins, and the extra manifests
are deleted automatically** so Git converges to a single copy.

"First" is defined by a stable scan order (for example lexicographic file path,
then document index) so the outcome is reproducible. A diagnostic records what
was removed:

```text
apps/v1/deployments/default/app: keeping apps/app.yaml document 2,
removing duplicate in overlays/dev/app.yaml document 1
```

Scan ordering can be annoying when the "wrong" copy wins, but a single
authoritative location matters more than preserving an ambiguous duplicate. This
applies to plain manifests; a future overlay-aware mode could model intentional
duplicates explicitly.

## Placement policy for new resources

The inventory solves updates and deletes for resources that already exist in
Git. New resources still need a destination.

The first version can keep a conservative default:

```text
{group}/{version}/{resource}/{namespace}/{name}.yaml
```

but this should become a placement policy rather than a hardcoded identity rule.
Possible policy knobs:

- default generated layout
- namespace folder layout
- resource-kind folder layout
- one file per resource
- append new resources to a specific file
- custom template using resource identity fields

The important distinction is that placement policy only applies when the
inventory has no existing location for the resource.

## Bootstrap files

GitTarget path bootstrapping already creates `.sops.yaml`. The same idea could
extend to simple GitOps bootstrap files, especially `kustomization.yaml`.

This should stay modest:

- create missing bootstrap files for clean folders
- preserve existing bootstrap files
- avoid taking ownership of complex user-authored bootstrap files
- make this optional per GitTarget

## Kustomize

Kustomize should be supported carefully and in phases.

The first useful step is detection:

- detect `kustomization.yaml`
- identify listed resource files when the structure is simple
- detect `namespace:` as contextual namespace only when it is unambiguous
- report unsupported generators, patches, remote bases, components, and complex
  overlays instead of editing through them

Basic support could allow GitOps Reverser to understand a simple folder that a
normal GitOps tool can apply, without pretending to reverse-engineer every
Kustomize transform.

## Helm

Helm source editing should be out of scope for this feature.

GitOps Reverser can still write normal Kubernetes resources such as Flux
`HelmRelease` objects, because those are plain KRM resources. But it should not
edit Helm chart templates, rendered chart output mixed with source templates, or
values-driven manifests.

The desired behavior is detection with a clear diagnostic:

```text
Skipping chart templates in charts/api/templates: Helm source editing is not supported.
```

## Phased plan

### Phase 1: plain manifest inventory

- recursively scan `.yaml` and `.yml` files below a GitTarget path
- parse Kubernetes-looking documents
- build `ResourceIdentifier -> ManifestLocation`
- read identity from partially-encrypted SOPS files; skip fully-encrypted ones
- ignore disallow-listed constructs (anchors, aliases, merge keys) and symlinks
- detect invalid YAML, empty documents, non-KRM YAML, duplicates, and Helm
  source folders
- surface inventory diagnostics in GitTarget status
- keep the current generated path for new resources

This phase can improve snapshot reconciliation immediately because Git state can
be discovered from YAML content rather than path shape. It does not change the
API-first reconcile contract: Git-only watched KRM is removed, and API-only
watched resources are added.

### Phase 2: in-place updates and deletes

- update existing resources at their inventory location
- update only the matching document in multi-document files
- structurally merge the desired object onto the existing node tree, touching
  only changed nodes
- delete only the matching document
- preserve comments and unrelated documents when possible
- preserve scalar styles such as literal blocks, folded blocks, quoting, and
  chomping indicators for unchanged fields
- refuse or warn when round-trip preservation is unsafe

This is the phase that makes the feature feel respectful of real repositories.

### Phase 3: configurable placement policy

- expose placement options on GitTarget
- support one or two simple layouts first
- keep the generated layout as the default fallback
- add status diagnostics showing where new resources will be placed

Appending new resources to an existing multi-document file should be a later,
explicit placement mode. It would be useful to recognize local patterns, such as
"all ConfigMaps in this folder are appended to `configmaps.yaml`", but GitOps
Reverser should not infer that behavior until the pattern is clear enough to
explain and override.

### Phase 4: basic Kustomize context

- detect simple `kustomization.yaml` files
- use `namespace:` as contextual namespace where safe
- optionally maintain the `resources:` list when GitOps Reverser creates a new
  file in that folder
- reject complex transforms with diagnostics

## Non-goals

- creating pull requests directly
- full Kustomize reverse transformation
- Helm chart or values editing
- decrypting SOPS material
- preserving every formatting detail in every YAML edge case
- making two autonomous GitOps controllers safely own the same resources
- making the initial-reconcile prune path safe on its own

## Current decisions

- The Kubernetes API remains leading.
- The initial reconcile expects valid KRM in the target folder to have already
  been synced into the watched API by the user or a normal GitOps tool. The
  surrounding orchestration is responsible for sequencing this; the prune path
  is not self-protecting.
- Valid watched KRM found only in Git is removed from Git; only watched GVRs
  participate in add/remove.
- Watched API resources missing from Git are added to Git.
- Duplicate copies of the same resource are resolved first-occurrence-wins by a
  stable scan order; the extra copies are deleted automatically.
- Convergence to no-change relies on a consistent desired-content projection plus
  semantic no-op detection, not a separate reconciliation-state layer.
- SOPS files are detected by file extension and must have both readable cleartext
  identity and a `sops` key, otherwise they are flagged invalid and ignored. They
  are single-document, never decrypted, and re-encrypted on write. Formatting is
  not preserved for encrypted files.
- Identity is the inventory key. The document index is authoritative within an
  inventory state, because GitOps Reverser updates it on write and rebuilds it
  via a full rescan on any external change.
- The rescan is gated on "did anything under the base path change" and is a full
  rebuild otherwise — nothing smarter.
- Anchors, aliases, and merge keys are disallow-listed; symlinks are not
  followed.
- A whole-file `yaml.v3` round-trip is rejected; per-document text splitting is
  the baseline.
- Inventory diagnostics start in GitTarget status.
- `metadata.namespace` stays written for now.
- Namespace elision is deferred until there is an explicit GitTarget setting and
  a clear contextual identity model.
- Appending to multi-document files is a later configurable placement mode, not
  the initial default.

## Remaining questions

- **`.sops.yaml` rule changes:** when the encryption rules change which fields
  are covered, every matching file must be re-encrypted. Is that an intended mass
  commit, or should it be gated?
- **New encrypted resources:** when a brand-new encrypted resource is created,
  which fields get encrypted — derived purely from the in-repo `.sops.yaml`, or
  from a GitTarget setting? This ties into the existing TODO about non-Secret
  sensitive custom resources.
- **Diagnostics surface:** GitTarget status will not scale to thousands of
  manifests. Start with high-level stats, and consider a small read API on
  GitOps Reverser itself for the per-resource detail later. Open.
- How far can structural-merge patching be pushed before the safer behavior is to
  replace the whole document?
- Which preservation requirements are hard guarantees, and which are best-effort
  niceties with diagnostics?
