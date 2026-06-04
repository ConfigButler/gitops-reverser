# Decision record: in-document YAML editor

> Outcome of the POC specified in
> [docs/design/manifest/manifest-parser-poc.md](../../../docs/design/manifest/manifest-parser-poc.md).
> This package is the throw-away prototype that POC asked for.

## Decision

Use **`gopkg.in/yaml.v3` node editing**, with two supporting pieces:

1. **Textual per-document splitting** ([split.go](split.go)) — a file is carved
   into documents as exact byte slices, block-scalar aware so a `---` inside a
   literal block is not mistaken for a separator. Only the edited document is
   re-rendered; every other document is spliced back verbatim.
2. **Structural merge onto the node tree** ([merge.go](merge.go)) — the sanitized
   desired object is merged onto the existing document's `yaml.Node`, touching
   only changed nodes. Existing key order is kept, absent keys are deleted,
   new keys are appended.
3. **2-space encoder** — `yaml.v3`'s default is 4-space; `SetIndent(2)` keeps
   common manifest style.

`goccy/go-yaml`, a hybrid text-slice editor, and `kyaml` were **not needed**.
`yaml.v3` passes the implemented hard requirements, with **one recorded drift
limitation** (flush-left sequence indentation — see Caveats). It is the right
spine; we do not jump to goccy.

## What the tests showed

The POC test categories pass (`go test ./internal/git/manifestedit/`,
coverage ~91%). Highlights:

- **Gating corpus round-trip:** every document in `testdata/corpus` (comments,
  quoted string-like values, a literal block, an indented-sequence Deployment,
  a multi-doc file) re-encodes **byte-for-byte identical**. The test fails on any
  drift and prints the exact diff.
- **Unrelated documents** stay byte-for-byte through an edit (text splice), incl.
  a CRLF+BOM document and a trailing `...` marker.
- **Edited-document framing** (CRLF, BOM, missing trailing newline, leading
  `---`, trailing `...`) is restored after re-encoding by `reskinDocument`, so it
  survives even when the edit is to another field in the **same** document.
- **Unrelated block scalars** (a ConfigMap script) survive byte-for-byte when an
  unrelated label changes; a *changed* script stays a literal block.
- **Comments** on unchanged *and* changed nodes are preserved; a new sibling key
  is appended in place rather than reordering the map.
- **No-op vs cleaning:** a clean Git doc that matches is left untouched; a Git doc
  carrying `resourceVersion` is rewritten to remove it (API is the truth).
- **Duplicates** resolve first-occurrence-wins by stable path order; the loser is
  deletable via `DeleteDocument`.
- **Disallowed constructs** — anchors, aliases, merge keys, **duplicate keys**,
  and **unusual tags** (local `!foo` tags, `!!binary`; but not `!!timestamp` from
  a plain date) — are detected at the node level and marked non-editable *without*
  materializing them, so an alias bomb does not blow up.
- **Deletion** removes only the matching document and leaves the rest
  byte-identical; removing the only document signals `FileEmpty` so the caller
  deletes the file.
- **Recursive scan** (`IndexDir`) walks a folder, indexes `.yaml`/`.yml` by
  identity, and **skips symlinks** (no following, no cycles, no escaping root).
- **SOPS** files (by extension) are indexed by cleartext identity when they have
  a `sops` key; fully-encrypted (identity hidden) or sops-less files are skipped.

## Behavior reference (observed, pinned by tests)

A running list of the small behaviors and edge cases, so the eventual user-facing
docs can describe them precisely. Each is pinned by a test; the test file is named
per row.

### Comments (`comments_chomp_test.go`)

| Case | Behavior |
|---|---|
| Standalone comment line above a field (head comment) | Preserved. |
| Comment after a value (line comment) | Preserved. |
| Line comment on a value whose value changes | Preserved and carried to the new value (`color: blue # brand` → `color: green # brand`). |
| New key added to a map | Appended after existing keys, with no comment. |
| **Head comment whose field is deleted** | **Removed together with its field** — a head comment belongs to the field below it. A sibling's own comment is unaffected. |
| Foot comment at the end of a map | Not separately tested; yaml.v3 foot-comment handling is known to be quirky. Treat as best-effort. |

### Block scalars (`comments_chomp_test.go`)

| Style | Behavior |
|---|---|
| Literal strip `\|-` | Chomp indicator and exact line layout preserved. |
| Literal clip `\|` | Chomp indicator and exact line layout preserved. |
| Literal keep `\|+` | Preserved when meaningful (a trailing blank line to keep). Without trailing blanks it equals `\|` and canonicalizes to `\|` (same value). |
| Folded `>`, `>-`, `>+` | Style, chomp, and string value kept, but yaml.v3 **re-flows line wrapping** on re-encode — folded source layout is not byte-stable. Recorded limitation. |

### Document framing (`additions_test.go`, `manifestedit_test.go`)

| Case | Behavior |
|---|---|
| Unrelated documents in a multi-doc file | Spliced back byte-for-byte. |
| Edited document: CRLF, BOM, trailing-newline presence, leading `---`, trailing `...` | Restored by `reskinDocument` after re-encode — **on the patch path only**. |
| Edited document interior content (non-block scalars) | Byte-stable for the house style; flush-left sequence indentation is re-indented (recorded limitation). |
| Whole-document fallback (`wholeReplace`) | Canonical render; does **not** restore framing (it is the explicit "preservation not possible" path). |
| Delete document 0 of a multi-doc file | The now-leading `---` separator is dropped so the file does not start with a stray separator; the survivor's *content* is unchanged (only the separator is affected). |

### Inventory / safety (`manifestedit_test.go`, `additions_test.go`)

| Case | Behavior |
|---|---|
| Duplicate identity across files/docs | First-occurrence-wins by stable path order; loser is deletable. |
| Anchors, aliases, merge keys | Detected at the node level, marked non-editable, never materialized (alias bomb safe). |
| Duplicate keys, unusual tags (`!foo`, `!!binary`) | Non-editable with a diagnostic. `!!timestamp` from a plain date is fine. |
| Non-KRM / empty document | Ignored with a diagnostic; does not block siblings. |
| Sequence (list) matching | **Index-based by default** (`limitations_test.go` pins it): an in-place item change is precise, but a **reorder** rewrites slot-by-slot and **mis-attributes item comments** (semantically correct and convergent). **Keyed matching is now available as an injected strategy** — set `EditOptions.ListMatch.KeyField` (e.g. `name`) and items are matched by key, so comments travel with their item across a reorder; it falls back to index when items are not uniformly keyed mappings (`keyedlist_test.go`). The GVK→key choice lives with the caller, never in the merge. |
| Inventory status surface | `Inventory.Summary()` gives bounded counts (documents/editable/non-editable/encrypted/duplicates) and `CountByLevel` groups diagnostics — the "stats first" seed, so status need not enumerate thousands of manifests. |
| SOPS file (by extension) | Indexed by cleartext identity when it has a `sops` key; identity-hidden or sops-less files are skipped as invalid. |
| Encrypted document patched in place | **Refused** — `PatchDocument` skips any document with a top-level `sops` key (indexed/authoritative ≠ patchable). It must go through the re-encrypt writer path, never an in-place merge. |
| Symlinks during folder scan | Skipped (never followed), with a diagnostic. |

## Guarantees we can honestly promise

Hard guarantees:

- unrelated documents preserved byte-for-byte
- unrelated scalars/block scalars in the edited document preserved (see caveat)
- semantic no-ops cause no write; dirty server fields are cleaned out
- disallowed constructs are ignored with a diagnostic, never silently rewritten
- **convergence:** the first write may normalize known drift and clean server
  fields, but every reconcile after that is a byte-stable no-op — no separate
  reconciliation-state layer needed (pinned by `convergence_test.go`)

Best-effort (with fallback + diagnostic when not possible):

- comment preservation around changed nodes
- scalar-style preservation for directly changed strings

## Caveats / out of scope

- **Recorded drift limitation:** the **edited** document is re-encoded, so the
  fidelity of its *untouched* scalars relies on `yaml.v3` round-trip. The corpus
  (house style) is byte-identical, but `yaml.v3` **normalizes flush-left sequence
  indentation** to its own indented style — proven and pinned by
  `TestRoundTrip_KnownDrift_FlushLeftSequence`. For such input the edited document
  is best-effort (semantics preserved, formatting normalized), while its framing
  is still restored. A stricter preflight or per-scalar text-slice fallback could
  close this later; it was not needed to choose the parser.
- Framing (CRLF, BOM, trailing newline, `...`) is restored on the edited document
  **only on the patch path**, and spliced verbatim on unrelated documents. The
  whole-document fallback (`wholeReplace`) renders canonically and does not restore
  framing. Internal *content* bytes of the edited document are not guaranteed for
  every input style (see above).
- **Index-based list matching by default; keyed matching is opt-in.** With no
  list strategy, updating a list item in place is precise, but reordering a list
  rewrites it slot-by-slot and moves item-attached comments to the wrong item
  (semantically correct and convergent). Injecting `EditOptions.ListMatch.KeyField`
  (e.g. `name`) matches items by key instead, so comments travel with their item
  across a reorder; the merge falls back to index when items are not uniformly
  keyed mappings. The Kubernetes GVK→key knowledge stays with the caller.
- **Encrypted records are indexed and authoritative but not patchable.** A SOPS
  document is found and owns its location, yet `PatchDocument` refuses to edit it
  in place (it would strip the sops key and write the secret in cleartext). The
  real writer must route encrypted resources through the re-encrypt path.
- **Manifest identity only.** Mapping a GVK to a watched GVR needs a live
  RESTMapper and is out of scope here (tracked in docs/TODO.md), as is namespace
  elision.
- **Bigger vision items not in this POC** (they belong to the inventory/writer
  integration, not the parser decision): Helm/Kustomize detection, watched-GVR
  filtering, placement policy for new resources, and bootstrap files. This POC is
  the in-document editing + indexing spine those build on.

## Implementation impact

The pieces map directly onto the vision document's decisions ("per-document text
split is the baseline", "structural merge, not diff-then-map", "identity is the
key", "duplicates first-wins delete", "ignore disallow-listed constructs",
"SOPS by extension"). Graduating this into the real writer means reusing
`internal/sanitize` for the desired projection (already done here) and wiring
`PatchDocument`/`DeleteDocument` behind the inventory's
`resource identity -> location` lookup. The package is intentionally isolated so
it can be rewritten if integration surfaces new edge cases.
