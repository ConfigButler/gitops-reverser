# Decision record: in-document YAML editor

> Outcome of the POC specified in
> [docs/future/manifest-parser-poc.md](../../../docs/future/manifest-parser-poc.md).
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
`yaml.v3` cleared every hard requirement.

## What the tests showed

All 15 POC test categories pass (`go test ./internal/git/manifestedit/`,
coverage 91%). Highlights:

- **Round-trip baseline (gating):** a representative manifest with comments,
  quoted string-like values, and a literal block re-encodes **byte-for-byte
  identical**. This is the result that made `yaml.v3` viable.
- **Unrelated documents** stay byte-for-byte through an edit (text splice), incl.
  a CRLF+BOM document and a trailing `...` marker.
- **Unrelated block scalars** (a ConfigMap script) survive byte-for-byte when an
  unrelated label changes; a *changed* script stays a literal block.
- **Comments** on unchanged *and* changed nodes are preserved; a new sibling key
  is appended in place rather than reordering the map.
- **No-op vs cleaning:** a clean Git doc that matches is left untouched; a Git doc
  carrying `resourceVersion` is rewritten to remove it (API is the truth).
- **Duplicates** resolve first-occurrence-wins by stable path order, extras
  flagged for deletion.
- **Anchors / aliases / merge keys** are detected at the node level and marked
  non-editable *without* materializing them — an alias bomb does not blow up.
- **SOPS** files (by extension) are indexed by cleartext identity when they have
  a `sops` key; fully-encrypted (identity hidden) or sops-less files are skipped.

## Guarantees we can honestly promise

Hard guarantees:

- unrelated documents preserved byte-for-byte
- unrelated scalars/block scalars in the edited document preserved (see caveat)
- semantic no-ops cause no write; dirty server fields are cleaned out
- disallowed constructs are ignored with a diagnostic, never silently rewritten

Best-effort (with fallback + diagnostic when not possible):

- comment preservation around changed nodes
- scalar-style preservation for directly changed strings

## Caveats / out of scope

- The **edited** document is re-encoded, so fidelity of its *untouched* scalars
  relies on `yaml.v3` round-trip. Representative manifests round-trip
  byte-identical, but this is not universal: unusual indentation, flow style, or
  exotic tags may normalize. Those cases take the **whole-document replacement**
  path with a diagnostic rather than a silent surprise.
- Line endings/BOM are preserved for *unrelated* documents via splicing; an
  *edited* document is normalized to LF.
- **Manifest identity only.** Mapping a GVK to a watched GVR needs a live
  RESTMapper and is out of scope here (tracked separately in docs/TODO.md), as is
  namespace elision.

## Implementation impact

The two halves map directly onto the vision document's decisions ("per-document
text split is the baseline", "structural merge, not diff-then-map"). Graduating
this into the real writer means reusing `internal/sanitize` for the desired
projection (already done here) and wiring `PatchDocument` behind the inventory's
`resource identity -> location` lookup. The package is intentionally isolated so
it can be rewritten if integration surfaces new edge cases.
