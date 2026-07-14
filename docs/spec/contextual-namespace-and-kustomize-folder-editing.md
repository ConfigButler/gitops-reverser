# Contextual namespace support for real manifest folders

> **spec** — current behaviour. The code depends on this document; change one, change the other. Index: [`../INDEX.md`](../INDEX.md)

> Status: investigation → partially implemented (graph-aware namespace inference and
> ambiguity refusal landed in the manifest store; GitTarget-level refusal and
> kustomize scoping still pending)
> Captured: 2026-06-08
> Updated: 2026-06-08
> Related:
> [file-agnostic-placement.md](gittarget-new-file-placement-rules.md),
> [manifest-inventory-file-agnostic-placement.md](manifest-system.md),
> [current-manifest-support-review.md](current-manifest-support-review.md),
> [version2/gittarget-repository-validity-and-placement.md](gittarget-new-file-placement-rules.md),
> [version2/gittarget-new-file-placement-rules.md](gittarget-new-file-placement-rules.md)

## Summary

The current writer can find and edit resources by content identity instead of by
canonical path, which is the right direction for existing GitOps folders. The
next hard edge is namespace-less namespaced YAML:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: app
data:
  color: blue
```

This is valid input for Kustomize when a nearby `kustomization.yaml` supplies a
namespace, but the raw YAML no longer contains the full Kubernetes object
identity. Gitops-reverser therefore needs a namespace context model before it
can safely edit, delete, deduplicate, or create resources in these folders.

The spike in the working tree proved the useful behavior:

- a readable fixture folder with `kustomization.yaml`;
- multi-document YAML;
- nested YAML files;
- hand-authored comments;
- `kubectl apply -k` as the starting cluster state;
- in-place edits that should preserve comments and avoid canonical duplicates.

It also showed that a narrow "nearest kustomization namespace" heuristic is too
thin to bless as architecture. It can make the happy-path test pass, but it does
not yet define the repository validity rules, API surface, or failure behavior.

## Problem statement

For namespaced resources, the system needs two identities:

| Identity | Source | Used for |
|---|---|---|
| Raw manifest identity | `apiVersion`, `kind`, `metadata.name`, optional `metadata.namespace` | Editing the document as it is written in Git |
| Effective resource identity | Raw identity plus namespace context | Matching live API events, duplicate detection, deletes, status |

When `metadata.namespace` is absent, raw identity is not enough for a live
resource. The missing namespace must come from an explicit, supported context.
Without that, several unsafe outcomes are possible:

- a live update creates a canonical duplicate instead of editing the existing
  file;
- a delete cannot find the namespace-less document;
- two namespace-less documents with the same name in different app folders look
  like duplicates when they are not, or fail to look like duplicates when they
  are;
- an in-place edit writes `metadata.namespace` into a file whose layout expects
  Kustomize to own namespace injection;
- resync/mark-and-sweep treats a namespace context change as mass create/delete.

## Design constraints

- Existing documents remain match-first. Once a resource is found in Git,
  updates and deletes should use that location.
- `kustomization.yaml` is auxiliary context, not a managed KRM document. It must
  not be swept, patched as a Kubernetes object, or rewritten by the live writer.
- New resources need explicit placement policy. Existing-resource editing and
  new-file placement are related but separate decisions.
- Namespace context is part of repository validity. A GitTarget should not go
  live if it cannot explain the effective identity of the files it will manage.
- The scanner and writer must use the same effective identity model in steady
  state, resync, duplicate detection, and status.

## Option A: keep explicit namespace only

Always require `metadata.namespace` in namespaced resources. Kustomize folders
can still be scanned, but namespace-less namespaced documents are rejected as
unsupported repository content.

Pros:

- simplest and safest identity model;
- no Kustomize semantics in the writer;
- duplicate detection stays content-only.

Cons:

- rejects common GitOps layouts;
- forces gitops-reverser to write fields that users intentionally centralize in
  Kustomize;
- undermines the "point at a real folder" goal.

This remains the correct fallback when no supported namespace context exists.

## Option B: GitTarget-level "omit namespace" setting

Add a GitTarget setting that tells the writer not to write
`metadata.namespace`, for example:

```yaml
spec:
  manifestStyle:
    namespace: Omit
```

This matches the intuition that a target pointed at one namespace may not need
the namespace repeated in every document.

Pros:

- small API surface;
- easy for single-namespace targets;
- does not require parsing Kustomize to decide output style.

Cons:

- it is only an output preference, not a full identity source;
- it is unsafe for targets that can watch multiple namespaces;
- it does not explain which namespace a namespace-less existing document belongs
  to unless paired with another setting;
- it cannot handle folders where one subtree belongs to namespace `app-a` and
  another subtree belongs to `app-b`;
- it can produce YAML that `kubectl apply -f` cannot apply without additional
  context.

If this option is used, it should not be a bare boolean. It needs an explicit
namespace source:

```yaml
spec:
  manifestStyle:
    namespace:
      mode: Omit
      value: app
```

That shape is only safe when the GitTarget's selected resources are constrained
to the same namespace.

## Option C: infer namespace from supported Kustomize context

Scan `kustomization.yaml` files as auxiliary context. A namespace-less
namespaced resource can be indexed when the scanner can prove that exactly one
supported Kustomize namespace applies to it.

Possible supported subset:

- local `resources` entries that point to YAML files;
- local `resources` entries that point to child directories with their own
  `kustomization.yaml`;
- `namespace:` as the only namespace transformer initially;
- no generators, remote bases, components, patches, replacements, plugins, or
  Helm inflators in the first write-capable subset.

Pros:

- fits real GitOps folders;
- lets `kustomization.yaml` stay on the ignore/retain list while still providing
  context;
- supports multi-document files and nested folders when the graph is simple;
- avoids writing `metadata.namespace` back into files whose namespace is owned by
  the kustomization.

Cons:

- Kustomize semantics are graph-based, not simply "nearest parent file";
- patches and generators can create or mutate resources with no direct source
  document;
- parent and child kustomizations can intentionally override namespace;
- a resource file can be included by more than one kustomization;
- Kustomize version drift matters if gitops-reverser tries to emulate too much.

This option is viable only if the first implementation deliberately supports a
small graph and rejects ambiguous cases.

## Option D: render with Kustomize, edit source files by source map

Run `kustomize build` to discover effective resources, then map each rendered
resource back to its source file/document for in-place editing.

Pros:

- closest to what Flux/Argo/Kustomize will apply;
- handles namespace transforms more accurately than a hand-rolled folder rule.

Cons:

- Kustomize does not provide a complete, stable source map for every transform;
- generated resources and patches are not cleanly editable at the rendered
  object location;
- editing source files from rendered diffs can be surprising or impossible;
- introduces an external semantic dependency into every GitTarget scan.

This is a good future research path, but it is too broad for the first safe
namespace-context implementation.

## Recommended direction

Use a contextual namespace model, not a simple "skip namespace" toggle.

First safe version:

1. Keep the default behavior explicit: new files include `metadata.namespace`.
2. Support editing existing namespace-less documents only when the scanner can
   derive exactly one namespace context from a supported source.
3. Treat `kustomization.yaml` as retained auxiliary context, never as managed KRM.
4. Record both raw and effective identities in the manifest store.
5. Preserve the document's namespace style on update:
   - if namespace is explicit in the file, keep it explicit;
   - if namespace came from supported context, do not write
     `metadata.namespace`;
   - if context is missing or ambiguous, refuse the GitTarget as repository
     invalid rather than creating a duplicate.
6. Add placement policy later for creating new namespace-less files under a
   known context. Until then, new resources fall back to explicit namespace
   canonical placement.

This lets existing real folders work without promising that gitops-reverser can
author every Kustomize layout from scratch.

## Kustomize defines the managed set (scope)

When a `kustomization.yaml` is present, it — not the filesystem — decides which
documents are in scope. The rule:

- **Follow the `resources` graph.** A document is part of the kustomize-managed
  set only if it is reachable from a kustomization through `resources` (directly,
  or transitively via a child directory base). Namespace context is attributed by
  that graph, never by "nearest file on disk".
- **A document outside the graph is not kustomize-managed.** A YAML file sitting
  in (or under) a kustomized folder that no `resources` entry references is not
  part of the build. We do not invent a namespace for it from a nearby
  kustomization.

What to *do* with those out-of-graph documents is a deliberate, simple choice and
yes, it is easy to implement because the graph is already computed:

- If the document carries its own full identity (explicit `metadata.namespace`, or
  it is cluster-scoped), it is just an ordinary managed document — independent of
  kustomize. Manage it normally.
- If it is a namespace-less namespaced document with no graph entry, we **cannot**
  explain its effective identity, so we **refuse the GitTarget** rather than guess
  or silently mismatch. (See "Strict by default" below.)

The point: kustomize narrows scope, it does not widen it. We only ever *read*
`kustomization.yaml` as context; we never treat it as a managed object, and we
never traverse anything but its `resources`/bases edges.

## Bidirectionality: every edit must round-trip

The contextual-namespace model is constrained by a requirement that runs through
the whole writer: **an edit must be expressible in both directions.** A change
observed on the live object must map cleanly onto the *source document* in Git
(live → Git), and the source document as written must map cleanly onto the live
object's identity (Git → live). The store already encodes this as two identities:

- raw identity = what is literally in the file (may be namespace-less);
- effective identity = raw identity plus context (the namespace a kustomization
  supplies).

`NamespaceSource` records which transform was applied so the writer can invert it:
on write it strips the context-supplied namespace back out, and on lookup it
matches the live event's effective identity to the file's raw identity. That is a
clean, reversible 1:1 mapping.

This is the deeper reason the unsupported list is what it is. Generators, patches,
replacements, Helm inflation, and `namePrefix`/`nameSuffix` are **lossy or
one-way**: the rendered object has no stable, invertible source location, so a
live change cannot be written back to "the" source document, and the source
document does not determine a single live identity. A transform we cannot invert
breaks round-tripping, so it is refused for write-capable contextual management —
not because parsing it is hard, but because the edit could not travel back.

A corollary, already decided elsewhere: one source document owns exactly one live
object, and one live object has exactly one editable source document. Overlays
that apply one base to several namespaces violate this and must be refused (see
the open question on namespace overlays).

## Strict by default: refuse, with clear GitTarget status

Start by refusing the hard questions, loudly, rather than handling them softly.
The default posture for anything outside the supported subset is **refuse the
GitTarget**, not "manage what we can and ignore the rest". A half-managed folder
is the dangerous state: it is where silent duplicates, wrong-namespace edits, and
unexplained deletes come from.

Concretely:

- Unsupported kustomize features, ambiguous namespace context, out-of-graph
  namespace-less documents, remote bases, and overlay fan-out all resolve to a
  GitTarget that does **not** go live.
- The refusal is surfaced as a specific, human-readable `RepositoryValid=False`
  status: the condition names *what* is unsupported and *where* (the offending
  file or `kustomization.yaml`), so an operator can fix the repo or narrow the
  GitTarget path. Status must never dump the whole source graph; it names the
  first/representative offenders and counts the rest.
- The store side of this already exists as build-time diagnostics
  (`ambiguous-namespace`, `unresolved-mapping`, `scope-mismatch`); the pending
  work is the `RepositoryValid` projection that turns those diagnostics into the
  refusing condition.

It is always safe to *widen* support later. It is expensive to walk back a folder
we promised to manage and then corrupted. Refuse first.

## API shape to consider

A future API should separate namespace identity from output style:

```yaml
spec:
  manifestStyle:
    namespace:
      existing: Preserve
      newFiles: Explicit
```

Suggested modes:

| Field | Mode | Meaning |
|---|---|---|
| `existing` | `Preserve` | Keep the style already present in Git |
| `existing` | `Explicit` | Always write `metadata.namespace` on update |
| `newFiles` | `Explicit` | New namespaced resources include `metadata.namespace` |
| `newFiles` | `Contextual` | New resources may omit namespace only when placement selects a supported context |

`Preserve` should be the default for existing files once contextual namespace
support exists. `Explicit` should remain the default for new files until
placement policy can prove where the namespace context lives.

If a single-namespace GitTarget shortcut is still useful, model it as a context
source, not an output toggle:

```yaml
spec:
  namespaceContext:
    type: Fixed
    namespace: app
```

That should be valid only when WatchRules/ClusterWatchRules select resources
from the same namespace.

## Repository validity rules

The validity scan should fail the GitTarget before live events start when:

- a namespaced KRM document omits `metadata.namespace` and no supported context
  supplies it;
- two supported contexts claim different namespaces for the same source
  document;
- a namespace-less document is included by more than one kustomization with
  different namespaces;
- a supported context points outside the GitTarget path;
- a managed file is produced only by a generator or patch and has no editable
  source document;
- two effective resource identities resolve to the same resource;
- one effective resource identity has two editable source documents.

These conditions belong with `RepositoryValid`, not with the live writer. The
writer should receive a store whose effective identities are already trustworthy.

## Writer implications

What landed: the store keeps the **effective** identity on `DocumentModel`
(`ManifestIdentity`, with the kustomize namespace already folded in) plus a
`NamespaceSource` recording provenance. The raw identity is the effective identity
with the context-supplied namespace stripped back out — derived on demand by the
writer rather than stored twice:

```go
type DocumentModel struct {
    ManifestIdentity manifestedit.Identity // effective: raw + context namespace
    NamespaceSource  NamespaceSource
    // ...
}

type NamespaceSource struct {
    Kind NamespaceSourceKind // Explicit | Kustomize | None (Fixed reserved)
    Path string              // kustomization path when Kind == Kustomize
}
```

`NamespaceSource` replaced the spike's `NamespaceFromKustomize bool`: the bool
captured only the output decision, while the kind/path also explains the
no-context and ambiguous cases to status, duplicate diagnostics, and future
placement. `dm.NamespaceInheritedFromContext()` (Kind == Kustomize) is the single
predicate the writer reads.

Write rules:

- lookup existing documents by effective identity;
- when patching a context-namespaced document, remove namespace from the desired
  projection before passing it to `manifestedit`;
- when deleting, match current bytes by effective identity, not raw identity;
- when appending or creating, use placement policy to decide whether a
  namespace context exists; otherwise render explicit namespace;
- use the same identity logic in resync and steady-state event flushing.

## Kustomize subset proposal

For the first implementation, support only this shape:

```yaml
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: app
resources:
  - bundle.yaml
  - nested/sidecar.yaml
```

Optionally support a child directory when the child directory has its own
`kustomization.yaml` and no conflicting parent namespace rules. Do not support
remote bases, generators, patches, replacements, components, plugins, or Helm
inflation for editable namespace inference yet. Those can remain valid Git files,
but the GitTarget should mark them unsupported for write-capable contextual
namespace management.

The important distinction: the implementation should follow the `resources`
graph, not just search for the nearest `kustomization.yaml` by filesystem path.

## Supported and unsupported example folders

The supported/unsupported boundary is concrete, so it is pinned by a corpus of
small example folders rather than prose alone. They live under
`internal/manifestanalyzer/testdata/contextual-namespace/` and are exercised by a
table-driven test that builds the store over each folder and asserts the outcome
(effective namespace + `NamespaceSource`, or the refusing diagnostic). The corpus
is meant to grow — every new "can we support X?" question should arrive as a new
folder.

| Folder | Shape | Expected outcome |
|---|---|---|
| `supported/flat-namespace` | one kustomization, `namespace:`, flat `resources` | namespace inherited (`Kustomize`) |
| `supported/nested-base` | parent `namespace:` + child dir base with no namespace | namespace propagates through the graph |
| `supported/multi-doc` | a multi-document file in `resources` | every document inherits |
| `supported/explicit-namespace` | `metadata.namespace` written in the file | kept as-is (`Explicit`) |
| `unsupported/ambiguous-two-roots` | two roots assign different namespaces to one file | refused (`ambiguous-namespace`) |
| `unsupported/patches` | `patches:` present | not a namespace source (`None`) |
| `unsupported/generators` | `configMapGenerator:` present | not a namespace source |
| `unsupported/components` | `components:` present | not a namespace source |
| `unsupported/helm` | `helmCharts:` present | not a namespace source |
| `unsupported/remote-base` | `resources:` points at a remote base | not a namespace source |
| `unsupported/name-prefix` | `namePrefix:` present (identity-mutating) | not a namespace source |
| `unsupported/no-context` | namespace-less namespaced doc, no kustomization | `None` (GitTarget should refuse) |

Today the store records the per-document outcome (`NamespaceSource` and the
diagnostics); the `unsupported/*` folders that currently resolve to `None` are the
inputs the pending `RepositoryValid` refusal will turn into a failed GitTarget.

## E2E test shape

The fixture-backed e2e test is still the right acceptance test once the design is
implemented:

- `test/e2e/fixtures/inplace-edit-folder/kustomization.yaml` sets the namespace;
- resource YAML omits `metadata.namespace`;
- one file is multi-document YAML;
- one resource lives under a nested folder;
- comments are present and must survive edits;
- the test starts with `kubectl apply -k`;
- after edits, no canonical duplicate appears;
- `kustomization.yaml` is unchanged;
- resource YAML still omits `metadata.namespace`.

The negative cases live at unit level as the example-folder corpus
(`internal/manifestanalyzer/testdata/contextual-namespace/`), so the one e2e stays
a single happy-path acceptance test rather than a matrix:

- namespace-less namespaced resource with no context;
- two kustomizations assigning different namespaces to the same source file;
- unsupported Kustomize features in a write-capable target.

## Open questions

- Should `RepositoryValid` reject unsupported Kustomize files outright, or allow
  the GitTarget to go live for the explicit-namespace resources in the same
  folder?
- Should namespace context come only from Git, or can a WatchRule namespace be a
  context source for namespaced WatchRules?
- Should `ClusterWatchRule` ever allow `Fixed` namespace context, or is that too
  easy to misconfigure?
- What status payload should explain contextual namespace decisions without
  dumping large source graphs into GitTarget status?
- Is there a future need to support namespace overlays where the same base file
  is intentionally applied to multiple namespaces? If yes, that conflicts with
  "one source document owns one live object" and probably needs to be refused by
  this controller.
