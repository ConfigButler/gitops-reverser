# Follow-up plan: the `manifestedit` package abstraction

> Status: proposed (follow-up to the parser POC)
> Related: [manifest-parser-poc.md](../../finished/manifest-parser-poc.md),
> [manifest-inventory-file-agnostic-placement.md](manifest-inventory-file-agnostic-placement.md),
> POC decision record: `internal/git/manifestedit/DECISION.md`

The parser POC is done: `gopkg.in/yaml.v3` node editing plus per-document text
splitting passes the implemented hard requirements (with recorded drift
limitations) and converges. This document is about the
*next* concern — getting the **abstraction** right so the package stays a small,
self-contained, well-tested library that the rest of GitOps Reverser builds on,
rather than something tangled into the controller and writer.

## The core idea: we always compare two versions of a resource

Everything this package does is a function of **two representations of the same
Kubernetes object**:

- the **Git version** — a document at a known location: raw bytes, a parsed node
  tree, and a manifest identity;
- the **desired version** — what Git *should* contain for that resource: a clean
  Kubernetes object (typically the API object after the reverser's projection).

Every operation is one cell in this table:

| Git version | Desired version | Decision |
|---|---|---|
| absent | present | **create** — out of scope: placement is upstream, and this is *not* a valid `Comparison` (`Git` is required; see invariants) |
| present | absent | **delete** the matching document |
| present | present, equal | **no-op** (preserve bytes) |
| present | present, different | **patch** in place (or whole-document replace fallback) |
| present (encrypted) | present | **refuse** in-place; route to the re-encrypt writer |
| present (anchors/dupes/…) | present | **skip** with a diagnostic |

Making this comparison explicit and first-class is the whole game. The package
should express it directly, not bury it inside a `PatchDocument` side effect.

## Principle: mechanism, not policy

The package is **mechanism**: "make this Git document equal this desired object,
with the smallest, most faithful edit possible." It must not own **policy**:

- what "clean" means (which server fields to drop, kind-specific rules) →
  that is the **projection/sanitizer**, owned by the caller;
- which resources are watched, prune sequencing, placement of new files →
  the **reconcile loop**, owned by the integration layer.

Keeping mechanism and policy apart is what lets the package stand on its own and
be tested without a cluster, a worktree, or the controller.

### Concretely: drop the `internal/sanitize` dependency

Today `PatchDocument` calls `sanitize.Sanitize(desired)` internally. That couples
the library to one projection policy. **Invert it: the caller passes the desired
Git projection already computed.** Then:

- the package depends only on `gopkg.in/yaml.v3` and
  `k8s.io/apimachinery/.../unstructured` (both stable, neither GitOps-Reverser
  specific);
- the comparison becomes honest and symmetric — "Git bytes vs the object you say
  should be there" — with no hidden cleaning step.

What must **not** happen is reinventing the canonical render. The whole-document
fallback and new-file rendering define the house output format, so that render is
**policy, not mechanism**. Inject it — a `func(*unstructured.Unstructured)
([]byte, error)`, satisfied today by `sanitize.MarshalToOrderedYAML` — rather than
duplicating it in the package, where it could silently diverge from the existing
writer's output contract. There is **no silent production default**: a path that
needs canonical output (`Replace`, or a new file) with no renderer injected fails
loudly with a diagnostic, so a missing wiring cannot mask itself as plausible
YAML. The preservation and delete paths need no renderer at all; tests inject a
small one explicitly. This keeps the preservation editor (mechanism, owned)
cleanly separate from canonical rendering (policy, injected).

Input type decision: keep `*unstructured.Unstructured`. It is already the
currency used by the watch and Git writer paths, keeps metadata access ergonomic,
and is a stable dependency. `manifestedit` still treats the object as plain data:
the caller passes the already-computed Git projection, and this package must not
import `internal/sanitize`.

## Recommended API shape

Make the comparison itself a value, and split deciding from applying:

```go
type Comparison struct {
    Git     *Document                  // required: an existing parsed document
    Desired *unstructured.Unstructured // nil means "absent" -> delete
    Options EditOptions                // injected strategies (see below)
}

type EditOptions struct {
    // Render is the canonical renderer for whole-document replacement and new
    // files — the house output format, so it is policy: injected, not owned here.
    // Nil is allowed only when no canonical output is needed (pure patch, no-op,
    // delete); a path that needs it with no Render fails loudly.
    Render func(*unstructured.Unstructured) ([]byte, error)
    // ListMatch aligns sequences (default: by index). A keyed strategy names the
    // field to match on; the GVK->field choice is made above this layer.
    ListMatch ListMatchStrategy
    // Owns reports whether a field path is owned by the reverser (default: all).
    // The field-ownership seam: an absent field is deleted only when owned.
    Owns func(path FieldPath) bool
}

// Decide is a pure preflight: it inspects and compares, never mutating Git.
func Decide(c Comparison) Decision

type Decision struct {
    Action   DecisionAction // NoChange | Patch | Replace | Delete | Skip
    Reason   string         // human-readable, for diagnostics
    Snapshot SnapshotRef    // observed identity + target-doc hash; Apply validates against this
}

// Apply is authoritative: it re-parses c.Git, performs the edit, and returns what
// actually happened. There is no separate file argument — c.Git is the single
// source of truth for the bytes.
func Apply(c Comparison, d Decision) (EditResult, []Diagnostic)
```

`Document` is immutable data: the **whole file content**, the target document
index, and the manifest identity — enough for `Apply` to splice the edited
document back among untouched siblings. The snapshot fingerprint is *not* part of
`Document`; it is derived by `Decide` and carried in the `Decision` (see
invariants). `Document` is deliberately not a shared mutable node tree (see below).

`Desired == nil` models deletion as just another cell of the same comparison, with
no second code path. The *trigger* for deletion still belongs to the reconcile
layer (its set-difference: "Git has watched KRM the API lacks"); the package only
formalizes the per-document consequence.

`PatchDocument`/`DeleteDocument` become thin wrappers over `Decide` + `Apply`.
`Options` is the one seam where later strategies plug in (renderer, keyed-list
key, field-ownership predicate), so the core merge stays small and pure.

### API invariants

These contracts keep the comparison honest; make them explicit before
implementing:

- **`Git` is required.** A `Comparison` always describes an *existing* document.
  `Git == nil, Desired != nil` is not a valid comparison: creating a brand-new
  resource is a **placement** decision (where does the file go?) owned upstream.
  Once a path is chosen, a new file is just `Options.Render(desired)` — it never
  goes through `Decide`. So the table's `absent | present -> create` row lives in
  the integration layer, not here.
- **`Apply` uses `c.Git` and validates the snapshot.** There is no separate file
  argument. `Apply` re-parses `c.Git`, confirms the document at the recorded index
  still has the identity (and a content fingerprint) that `Decide` compared, and
  refuses with a conflict diagnostic if the file drifted in between. One source of
  truth; no stale edit applied to a changed shape.
- **`Decision` is preflight; `EditResult.Mode` is authoritative.** Because
  `Decide` does not merge, it states an *intent* (e.g. `Patch`). `Apply` re-parses
  and merges, and may legitimately land elsewhere — `Replace` if a node turns out
  ambiguous, `Skip` if the snapshot drifted. The returned `EditResult` is the
  truth about what happened, not the `Decision`.
- **Deletion is content-agnostic, so refusals never block prune.** The encrypted
  and disallowed-construct refusals apply only to *content edits* (`Patch` /
  `Replace`), which read and rewrite the object. `Delete` only removes a document
  (splicing siblings verbatim) — it never decrypts or merges — so an encrypted
  resource, a disallowed-construct document, or a duplicate loser can always be
  pruned.

### Decide must not mutate

The current merge mutates the node tree as it walks while returning `changed`.
That is fine inside `Apply`, but it must never leak into `Decide`, or a "decision"
could silently change Git before anything is applied. Two rules keep it honest:

- **The node tree is never shared.** `Document` carries raw bytes, identity, and
  index — not a mutable parsed tree. `Decide` and `Apply` each parse internally,
  so nothing one mutates can affect the other.
- **`Decide` does not run the structural merge at all.** It needs only cheap,
  non-mutating checks: parseable? disallowed construct? encrypted? non-mapping
  root (→ `Replace`)? and the object-level equality the no-op path already uses
  (Git-as-written vs desired → `NoChange` or `Patch`). The node mutation happens
  only in `Apply`, on its own fresh parse.

This is simpler than clone-before-merge or a dry-run merge: by not sharing the
tree and not merging in `Decide`, there is nothing to clone. We also deliberately
avoid building a serializable "patch plan" in `Decide` and replaying it in
`Apply` — that duplicates structure for no current benefit; revisit only if we
ever need to *show* a diff before applying.

## Internal layering (one package, clear seams)

Keep a single package but with hard internal seams, so a layer can later graduate
to a sub-package if it earns reuse:

1. **Document model (pure YAML, no Kubernetes):** `split`/`join` (byte-preserving),
   decode/encode nodes, `reskin` framing, the structural `merge`. Knows nothing
   about KRM. This is the part most likely to become `manifestedit/yamldoc`.
2. **Manifest model (Kubernetes identity):** `Identity`, `Inventory`,
   KRM/SOPS/disallowed-construct detection. Maps content to identity and location.
3. **Decision layer (the two-version comparison):** `Decide` and `Apply`, plus the
   encrypted-refusal and skip rules.

Rule of thumb: nothing in layers 1–2 imports the controller, git worktree,
rulestore, or telemetry. If it needs those, it belongs in the integration layer,
not here.

## "More specific edits": granularity and ownership

The structural merge already gives **field-level specificity** — it only rewrites
nodes whose value actually differs, so unrelated fields, comments, and block
scalars are untouched. "More specific" is really a question of **how granular the
comparison is and what the reverser owns**. Three axes, in increasing ambition:

1. **Keyed list matching.** Today lists are index-based, so a reorder rewrites
   slots and mis-attributes item comments (pinned limitation). Match list items by
   a key field so an item is compared to its counterpart, not its slot. Crucially,
   the pure document model only ever sees a generic *list-match strategy* ("match
   this list by field X"); the Kubernetes knowledge of which key applies to which
   GVK (`name` for containers, and so on) lives in the manifest/decision layer or
   the caller, never baked into the YAML merge.
2. **Field ownership (the big one).** Right now the desired object is the *whole*
   truth: any field present in Git but absent from desired is deleted. A more
   specific model owns only certain field paths (server-side-apply-style managed
   fields) and leaves user-managed fields in Git alone. This is a real product
   decision — "does the reverser own the entire object or a declared subset?" —
   and it lives naturally as a predicate over the merge walk (own this path / skip
   that path). It should be decided explicitly, not drifted into.
3. **Per-path preservation policy.** Narrower than ownership: always keep certain
   annotations/labels, never touch a given subtree. Same mechanism (a path
   predicate), smaller scope.

All three hang off the *same* comparison engine: they are strategies that answer
"for this node/path, what does it mean to be equal, and do we own it?" Keeping
them as injected strategies (not branches baked into `merge`) keeps the core
small and each strategy independently testable.

## Invariant that must survive every change: convergence

The POC proved repeated reconciles settle to a byte-stable no-op after the first
write (`convergence_test.go`). Any new strategy — keyed lists, field ownership —
must preserve this. The rule: **`Decide` after an `Apply` with the same desired
must return `NoChange`.** Make it a property test that every strategy runs through.

## Test taxonomy (the package carries its own proof)

The package already tests this way; the plan is to keep the discipline as it
grows. Each capability lands with tests in its own group:

- **document model:** split/join, framing (CRLF/BOM/`...`/leading `---`), merge.
- **manifest model:** identity, inventory, duplicates, SOPS, disallowed constructs.
- **decision:** no-op vs cleaning, patch, whole-replace fallback, delete, skip,
  encrypted refusal.
- **fidelity:** comments (head/line/delete-with-field), literal vs folded blocks,
  quoting.
- **recorded limitations:** flush-left sequences, folded reflow, list-reorder
  comment migration — pinned so a future fix announces itself by failing.
- **convergence:** the property above, ideally run across the whole corpus.
- **corpus:** `testdata/corpus` gates byte-for-byte round-trip.

A good signal that the abstraction is right: a new strategy needs a new test file
and a small predicate, and touches nothing in layers 1–2.

## Current implementation anchors

This plan is a refactor of the working POC, not a rewrite from blank paper. The
important existing anchors are:

- `PatchDocument` in `internal/git/manifestedit/patch.go` is the current mixed
  seam: it validates the target document, refuses unsafe edits, sanitizes the
  desired object, decides no-op vs patch, applies the merge, and falls back to
  whole-document rendering. `Comparison`, `Decide`, `Apply`, and injected
  rendering should tease those responsibilities apart without changing the
  proven behavior.
- `mergeMapping` in `internal/git/manifestedit/merge.go` is today's whole-object
  ownership rule: a field present in Git but absent from desired is deleted.
  Keep that as the default `Owns == nil` behavior, then add ownership predicates
  as an extension point rather than changing the baseline semantics.
- `IndexFiles` and `DocumentRecord` in `internal/git/manifestedit/index.go`
  already define the manifest identity, duplicate resolution, encrypted flag, and
  editability diagnostics that the decision layer should consume.
- `DeleteDocument` in `internal/git/manifestedit/delete.go` is already the
  content-agnostic document-splice operation. The new delete action should route
  through that behavior, including the existing `FileEmpty` signal for callers
  that should remove the file.
- `sanitize.MarshalToOrderedYAML` is already the house renderer used by the Git
  writer. `manifestedit` should receive it through `EditOptions.Render`; it
  should not grow its own canonical renderer or keep calling `sanitize`
  internally.
- `convergence_test.go` is the guardrail for the whole abstraction: after
  `Apply`, running `Decide` again with the same desired object must settle to
  `NoChange` and preserve the resulting bytes.

## Work sequence

1. **Decouple `sanitize`** — caller supplies the desired projection, and the
   canonical renderer is injected (trivial default for tests), not reinvented in
   the package. Small, unlocks true standalone status.
2. **Introduce `Comparison` + `Decide`/`Apply`** — make the two-version comparison
   a first-class, pure value (`Decide` non-mutating, `Desired == nil` = delete);
   keep `PatchDocument`/`DeleteDocument` as wrappers.
3. **Convergence property test** across the corpus, wired so every later strategy
   inherits it.
4. **Keyed list matching** as the first injected strategy (fixes the reorder
   limitation).
5. **Field-ownership design spike** — decide whole-object vs declared-subset; this
   is a product decision and deserves its own short doc.
6. **Integration milestone (separate doc):** a read-only, inventory-driven
   reconcile that *reports* what it would add/remove/update against a real cluster,
   consuming this package unchanged. Defers the prune hazard and the GVK→GVR
   mapping (`docs/TODO.md`) until after the comparison is trusted end to end.

Steps 1–4 keep all work inside `internal/git/manifestedit` with no controller
coupling. Only step 6 reaches into the writer/commit path
(`commit_executor.go`, `branch_worker.go`), and it consumes the library rather
than changing it.

## Implementation decisions

- **Desired input stays `*unstructured.Unstructured`.** This matches the current
  event and writer path: watch/audit code already routes sanitized
  `unstructured` objects, and the Git writer already renders them. The package
  must not sanitize internally; the caller passes the desired Git projection.
- **Field ownership starts as whole-object truth.** This matches today's merge
  behavior and convergence tests: a field present in Git but absent from desired
  is removed. Keep the `Owns(FieldPath) bool` seam with default "own all", but
  defer declared managed paths to a separate product/design spike because it
  changes what GitOps Reverser promises to preserve.
- **The document model stays in one package for now.** The YAML document seams
  (`split`/`join`, framing, decode/encode, merge) should remain file-separated
  inside `internal/git/manifestedit`. Graduate them to `manifestedit/yamldoc`
  only after another caller needs that API.
- **The snapshot fingerprint is the target document body, not the whole file.**
  Sibling documents can change without invalidating the target edit. `Decide`
  records the observed identity plus target-document hash in the `Decision`;
  `Apply` re-parses the current document and returns a soft `Skip` diagnostic on
  drift, so the next reconcile can re-decide cleanly.
- **Projection and canonical rendering stay in `internal/sanitize` for now.**
  The integration layer injects `sanitize.MarshalToOrderedYAML` as
  `EditOptions.Render`. Add a contract test around that adapter so
  whole-document replacement and new-file output cannot drift from the existing
  writer output.
