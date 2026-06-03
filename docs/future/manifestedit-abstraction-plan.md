# Follow-up plan: the `manifestedit` package abstraction

> Status: proposed (follow-up to the parser POC)
> Related: [manifest-parser-poc.md](manifest-parser-poc.md),
> [manifest-inventory-file-agnostic-placement.md](manifest-inventory-file-agnostic-placement.md),
> POC decision record: `internal/git/manifestedit/DECISION.md`

The parser POC is done: `gopkg.in/yaml.v3` node editing plus per-document text
splitting clears the hard requirements and converges. This document is about the
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
| absent | present | **create** (placement policy — out of scope for this package) |
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
  should be there" — with no hidden cleaning step;
- the only thing to replace is the whole-document fallback render, which moves to
  a tiny internal canonical encoder (apiVersion, kind, metadata, then the rest),
  so the package owns its fallback with zero policy dependency.

Open sub-decision: input type. `*unstructured.Unstructured` is idiomatic and
ergonomic; `map[string]interface{}` would remove even the apimachinery import.
Recommendation: keep `unstructured` — it is the natural currency and a stable
dependency — but treat the object purely as data.

## Recommended API shape

Split the "decide" from the "apply", so the two-version comparison is a value you
can test and log, separate from byte production:

```go
// Decide compares a parsed Git document against the desired object and returns
// what should happen, without producing new bytes.
func Decide(doc Document, desired *unstructured.Unstructured) Decision

type Decision struct {
    Action DecisionAction // NoChange | Patch | Replace | Delete | Skip
    Reason string         // human-readable, for diagnostics
}

// Apply produces the new file content for a decision (no-op returns the input).
func Apply(file []byte, docIndex int, desired *unstructured.Unstructured) (EditResult, []Diagnostic)
```

`PatchDocument`/`DeleteDocument` remain as thin convenience wrappers over
`Decide` + `Apply`. The win is that `Decide` is a pure, exhaustively testable
function over the two versions — exactly the thing we "constantly compare".

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
   a key (`name`, and a small per-GVK key set) so an item is compared to its
   counterpart, not its slot. This is a strategy injected into the merge walk; it
   does not change the rest of the model.
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

## Work sequence

1. **Decouple `sanitize`** — caller supplies the desired projection; add the tiny
   internal canonical-fallback encoder. Small, unlocks true standalone status.
2. **Introduce `Decide`/`Apply`** — make the two-version comparison a first-class,
   pure value; keep `PatchDocument`/`DeleteDocument` as wrappers.
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

## Open questions

- Input type for the desired object: `*unstructured.Unstructured` vs plain
  `map[string]interface{}` (apimachinery dependency or not).
- Field-ownership model: whole-object truth vs declared managed paths. This most
  shapes what "specific edits" ultimately means.
- Does the document model graduate to a `manifestedit/yamldoc` sub-package, or
  stay file-separated within one package until something else reuses it?
- Where does the projection (sanitizer) live once the dependency is inverted —
  reused from `internal/sanitize` by the caller, or promoted to a shared spot?
