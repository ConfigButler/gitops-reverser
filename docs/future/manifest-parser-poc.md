# POC: in-document YAML editor choice

> Status: implemented — see `internal/git/manifestedit` and its
> [DECISION.md](../../internal/git/manifestedit/DECISION.md).
> Outcome: `gopkg.in/yaml.v3` node editing + per-document text splitting clears
> every hard requirement; goccy/kyaml/text-slice were not needed.
> Related: [manifest-inventory-file-agnostic-placement.md](manifest-inventory-file-agnostic-placement.md)

## Goal

The manifest-inventory vision document already decides the editing architecture.
This POC has one narrow job: prove whether `yaml.v3` can edit a **single YAML
document** with enough formatting fidelity to honor the preservation
requirements, and if not, pick the fallback.

The decision is driven by tests, not preference.

## What is already decided (not part of this POC)

These come from the vision document and are not re-litigated here:

- **Document isolation is textual.** A file is split into its documents on
  document boundaries, and only the changed document is re-rendered; unrelated
  documents are spliced back verbatim. A whole-file `yaml.v3` round-trip is
  rejected, because re-encoding the whole file normalizes indentation, quoting,
  and spacing across every document and so cannot keep unrelated documents
  byte-for-byte identical.
- **Editing is a structural merge.** The desired (sanitized) object is merged
  onto the existing document's node tree, touching only changed nodes. There is
  no separate "diff then map field paths back to nodes" pass.
- **`kyaml` is not a separate candidate.** `sigs.k8s.io/kustomize/kyaml` is built
  on `yaml.v3` and inherits its formatting and comment fidelity, so it cannot
  fix a `yaml.v3` preservation gap — it only adds KRM ergonomics. It is dropped
  from the parser comparison.
- **Encrypted files are out of POC scope.** SOPS files are single-document,
  never decrypted, and re-encrypted through the existing encryptor on write
  (see the vision document). The only inventory behavior the POC needs to honor
  is reading identity from a partially-encrypted file and skipping a file whose
  identity is encrypted.

## The one open question

Within a single changed document, is `yaml.v3` node fidelity good enough?

The fallback order if it is not:

1. `yaml.v3` — primary; already the closest to today's code.
2. `goccy/go-yaml` (`github.com/goccy/go-yaml`) — fallback if `yaml.v3` loses
   formatting; it exposes token/CST-level detail and generally has stronger
   comment and position fidelity.
3. Per-scalar text-slice within the changed document — last resort, only if
   neither parser can preserve directly changed-adjacent scalars.

| Option | Shape | Strength | Risk |
|---|---|---|---|
| `yaml.v3` node merge | Parse the one document into `yaml.Node`, merge the desired object onto it, encode back | Comments, node kinds, scalar styles, line/column metadata; smallest step from today | Known round-trip drift on block scalars, indentation, and comments |
| `goccy/go-yaml` | Same merge, richer token/CST access | Better comment and position fidelity | New dependency and behavior to learn |
| Per-scalar text-slice | Keep the document as text, replace only the spans of directly changed scalars | Best chance at exact preservation | Byte ranges and YAML edge cases are hard |

## POC boundaries

Build a small isolated prototype, not production integration. It must be easy to
delete or rewrite after the decision.

Suggested package:

```text
internal/git/manifestedit
```

A deliberately small API:

```go
type Location struct {
    Path          string
    DocumentIndex int
}

type EditResult struct {
    Content []byte
    Mode    EditMode
}

func IndexFile(path string, content []byte) (Inventory, []Diagnostic)
func PatchDocument(content []byte, documentIndex int, desired *unstructured.Unstructured) (EditResult, []Diagnostic)
```

The exact API is less important than the tests.

## Experiment order

This order front-loads the cheapest decisive experiment.

1. **No-op round-trip baseline.** Parse → encode each document in a real manifest
   corpus with no change, and measure how much `yaml.v3` drifts before any
   editing. If unrelated nodes already drift here, that bounds everything else.
2. Document splitting and inventory detection.
3. Semantic no-op detection using the existing sanitizer path.
4. Whole-document replacement for one document in a multi-document file.
5. Structural merge for simple maps and scalars.
6. Run the ConfigMap script-block tests.
7. Run comment and quote preservation tests.
8. Decide whether `yaml.v3` is sufficient.
9. Only if it is not, repeat the relevant tests against `goccy/go-yaml`, then a
   per-scalar text-slice approach.

## Required test cases

Hard requirements are marked. The rest are strong preferences whose limitations
must be documented if unmet.

### 1. No-op round-trip drift (hard, gating)

Parse and re-encode a corpus of real manifests with no edits. Record exactly
where the encoder changes bytes (block scalar re-wrapping, indentation, quoting,
comment placement). This is the baseline the merge editor inherits.

### 2. Multi-document inventory

A file with a ConfigMap, an empty document, and a Deployment should index the
ConfigMap as document 0, ignore or diagnose the empty document without failing
the file, index the Deployment as document 2, and preserve document indexes.

### 3. Non-KRM YAML

Ordinary YAML without `apiVersion`, `kind`, and `metadata.name` should be
ignored or get a non-fatal diagnostic, and must not block editing valid
manifests in the same folder.

### 4. Duplicate identity

The same object in two files, or twice in one file, should be detected as a
duplicate set with a diagnostic. Resolution follows the vision document: the
first occurrence by a stable scan order wins, and the extra copies are deleted.
The investigation still matters here — the POC must detect the duplicate set
reliably and pick the deterministic winner — so the result is one authoritative
copy kept and the other removed.

### 5. Semantic no-op vs cleaning (hard)

Two opposite directions that must not be confused. The API is the truth and Git
holds only clean, GitOps-compatible manifests; the sanitizer defines that clean
projection.

**True no-op — preserve bytes.** Operational fields the sanitizer strips
(`resourceVersion`, `managedFields`, `status`, and similar) are removed from the
*desired* side, so they are never written. A Git manifest that is already clean
and otherwise matches is therefore equal: return "no change" and preserve the
original bytes exactly.

```yaml
# already in Git (clean); API returns the same object plus
# metadata.resourceVersion and managedFields -> no write
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
data:
  color: blue
```

**Cleaning — not a no-op.** If the *Git* document itself carries operational
noise such as `resourceVersion`, that field is absent from the clean desired
projection, so Git differs and must be rewritten to remove it. This is field
deletion (see test 12), not a preserved no-op.

```yaml
# in Git (dirty) -> resourceVersion must be deleted, rest preserved
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
  resourceVersion: "12345"
data:
  color: blue
```

The asymmetry is the point: sanitizer fields are stripped from the desired object
so they are never written, and if Git already carries them they are cleaned out.

### 6. Document-scoped update (hard)

In a multi-document file where only the second document changes, documents 0 and
2 remain byte-for-byte identical, only document 1 changes, and the separators
stay valid.

### 7. Comment preservation

```yaml
# app config
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config # stable name
  namespace: default
  labels:
    app: demo # selector label
```

Adding or changing one label should preserve comments not attached to the
changed node. Comments on the changed node should be preserved if the parser can
do so cleanly; otherwise the behavior must be documented.

### 8. ConfigMap script block survives unrelated edits (hard)

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

When the desired object only adds `metadata.labels.app: demo`, `data.start.sh`
must remain byte-for-byte identical. The literal block style and chomping
indicator must not change.

### 9. ConfigMap script block changes intentionally

When `data.start.sh` itself changes, the editor should keep literal block style
if possible — a changed script should still render as a readable block scalar,
not an escaped one-line string. Strong preference; record the limitation if
exact chomping preservation is not possible.

### 10. Quoted and string-like values

```yaml
data:
  build: "00123"
  enabled: "false"
```

Unrelated edits should preserve quoting; direct edits must not convert these into
numeric or boolean YAML values.

### 11. List item update

Updating one container image in a multi-container Deployment should change only
that image field when the list item can be identified safely. Index-based
patching is fine for the POC; field-keyed matching is a later nicety.

### 12. Field deletion

A field present in Git but absent from the sanitized desired object should be
removed without rewriting unrelated fields, including deleting one label from a
map that has comments and sibling labels.

### 13. Disallowed constructs are ignored, not materialized (hard)

A document using anchors, aliases, or merge keys (`&`, `*`, `<<`) — including a
deliberately crafted alias-expansion bomb — must be ignored as non-editable with
a diagnostic, and must not be fully materialized (no memory/CPU blowup). Files
with duplicate keys or unusual tags fall in the same bucket: diagnose and skip,
never silently rewrite. Symlinked files under the scan root are skipped.

### 14. Line-ending and boundary fidelity (hard)

CRLF line endings, a UTF-8 BOM, presence or absence of a trailing newline, a
leading `---`, and a trailing `...` must survive an unrelated edit. These are
where "byte-for-byte" quietly fails.

### 15. Partially-encrypted manifest indexing

A SOPS file with cleartext identity and encrypted `data` is indexed by its
identity. A file whose identity fields are encrypted is skipped with a
diagnostic. The POC does not decrypt or edit encrypted content.

## Decision criteria

The editor is acceptable only if it satisfies the hard requirements:

- parse multi-document YAML and preserve document indexes
- identify KRM resources from YAML content
- detect duplicates
- avoid writes for semantic no-ops
- update only the matching document, keeping unrelated documents byte-for-byte
- keep an unrelated ConfigMap script block byte-for-byte
- ignore disallow-listed constructs without materializing them
- preserve line-ending and document-boundary bytes on unrelated edits
- index partially-encrypted manifests by cleartext identity and skip
  identity-encrypted files
- emit diagnostics when preservation is unsafe

Nice-to-have:

- preserve comments around changed nodes
- preserve scalar style for directly changed strings
- field-level deletion without whole-document replacement
- safe list-item updates
- output stable enough to keep Git history clean

If `yaml.v3` passes the hard requirements, use it. If it fails mainly on
ergonomics but preserves formatting, wrap it with local helpers. If it fails on
preservation, evaluate `goccy/go-yaml`. If both fail on exact document/scalar
preservation, use a per-scalar text-slice approach within the changed document.

## Expected outcome

A short decision record:

- chosen in-document editor (`yaml.v3`, `goccy/go-yaml`, or text-slice)
- tests passed and failed
- preservation guarantees GitOps Reverser can honestly promise
- cases that produce diagnostics or whole-document replacement
- implementation impact for the manifest inventory feature

The default expectation is an AST-based editor with canonical Kubernetes
serialization retained for semantic comparison. The open question is how much
exact source preservation that AST delivers on its own within a single document.
