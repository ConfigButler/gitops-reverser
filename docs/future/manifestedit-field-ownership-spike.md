# Field-ownership design spike: does the reverser own the whole object or a declared subset?

> Status: spike (design decision, not yet implemented)
> Related: [manifestedit-abstraction-plan.md](manifestedit-abstraction-plan.md)
> (step 5), [manifest-inventory-file-agnostic-placement.md](manifest-inventory-file-agnostic-placement.md),
> POC decision record: `internal/git/manifestedit/DECISION.md`

This is the short product/design doc the abstraction plan calls for before any
ownership code lands. The mechanism seam already exists — `EditOptions.Owns
func(FieldPath) bool`, threaded through the merge walk with a default of
"own everything". What is *not* decided is the **policy**: when GitOps Reverser
writes a resource into an existing Git document, does it own the entire object,
or only a declared subset of fields, leaving the rest of Git untouched?

This is the single most consequential knob in the package, because it changes
what GitOps Reverser *promises to preserve*. It deserves an explicit decision,
not a default that drifts in.

## The question, concretely

The structural merge already gives field-level specificity: it rewrites only the
nodes whose value actually differs. The open question is narrower and sharper —
**what happens to a field that is present in Git but absent from the desired
object?**

```yaml
# Git (the existing document)            # desired (the reverser's projection)
apiVersion: apps/v1                      apiVersion: apps/v1
kind: Deployment                         kind: Deployment
metadata:                                metadata:
  name: app                                name: app
  annotations:                             # (no annotations)
    team: payments        # <-- ???
spec:
  replicas: 3                              spec:
                                             replicas: 3
```

Two honest answers:

- **Whole-object truth (today's default).** The desired object is the *entire*
  truth. `metadata.annotations.team` is absent from desired, so it is deleted
  from Git. GitOps Reverser asserts "this object looks exactly like what I
  computed."
- **Declared-subset ownership.** The reverser owns only certain field paths (the
  ones it manages); `metadata.annotations.team` is unowned, so it stays in Git.
  GitOps Reverser asserts "the fields I manage look like what I computed; I leave
  everything else alone."

The `Owns(path)` predicate is exactly the seam that picks between them: an absent
field is deleted **only when owned**. `Owns == nil` means own-all, which is
whole-object truth — what the POC and its convergence tests assume today.

## Why this is not the same as the projection

It is tempting to fold this into the sanitizer, but ownership and projection are
two different axes and conflating them hides the decision:

- **Projection** (the caller's `internal/sanitize`, injected as the desired
  object) decides *what the desired object contains* — it drops `status`, server
  metadata (`resourceVersion`, `managedFields`, …), and keeps `spec`/`data`/etc.
  It shapes the **right-hand column** above.
- **Ownership** (`Owns`, inside this package) decides, *for a field in Git that
  the desired object does not mention*, whether to delete it. It governs the
  **reconciliation of the gap** between the two columns.

They compose. A field dropped by projection **and** owned is deleted from Git
(this is how `status` and `resourceVersion` get cleaned today — desired omits
them, own-all deletes them). A field dropped by projection but **not** owned is
left in Git. So ownership is precisely the lever that protects human-authored
fields the projection never carries.

This is also why ownership must live in `manifestedit`, not in the sanitizer:
the sanitizer only ever sees the API object, never the Git document, so it
cannot reason about "a field that exists in Git but not in the API projection."

## Options

### Option A — Whole-object truth (status quo)

`Owns == nil`. The reverser owns everything; any Git field absent from the
desired projection is removed.

- **Pro:** simplest; one source of truth; Git is a faithful mirror of the
  (projected) cluster object; convergence is trivially defined and already
  proven.
- **Pro:** no per-field configuration, no surprising "why didn't my deletion
  propagate?" support questions.
- **Con:** hostile to **shared documents**. Anything a human or another tool
  added to the same document — a `team` annotation, a comment-bearing field, an
  overlay-injected label — is silently deleted on the next reconcile. This is the
  opposite of the file-agnostic-placement vision, whose entire point is attaching
  to *existing* repositories that GitOps Reverser did not author.
- **Con:** makes GitOps Reverser an exclusive writer of every byte it touches,
  which is a strong claim to make implicitly.

### Option B — SSA managed-fields-driven ownership

Derive ownership from Kubernetes Server-Side Apply: the API object carries
`metadata.managedFields`, which records exactly which field paths each manager
owns. Project that into an `Owns(path)` predicate keyed on GitOps Reverser's (or
the original applier's) field set.

- **Pro:** uses Kubernetes' own answer to "who owns this field," so the reverser
  preserves precisely what other controllers/users manage. This is the
  principled, correct-by-construction model.
- **Pro:** aligns the reverser's write semantics with how the cluster already
  reasons about shared ownership.
- **Con:** `managedFields` is dropped by the current projection and is famously
  fiddly: `fieldsV1` is a nested set-encoding, ownership can be shared across
  managers, and apply-vs-update operations differ. Building a faithful
  `FieldPath` predicate from it is real work and a real test surface.
- **Con:** ties the merge's behavior to data only available from a live API
  object with SSA metadata intact — harder to reason about offline and in the
  package's cluster-free tests (the predicate would be built above this layer and
  injected, which the seam already supports, but the *derivation* needs its own
  tests and probably its own package).

### Option C — Static path policy (declared managed paths)

A fixed, configured set of owned path prefixes — e.g. own `spec`, `data`,
`metadata.labels[app.kubernetes.io/*]`; do not own arbitrary
`metadata.annotations`. Expressed as an `Owns(path)` predicate built from
configuration (a GitTarget field, or a built-in default).

- **Pro:** predictable, explainable, testable offline; no dependency on SSA
  metadata; a sensible default ("own spec/data, leave foreign metadata alone")
  covers the common shared-document case.
- **Pro:** small, declarative, and a natural fit for the existing seam.
- **Con:** coarser than SSA; a static policy cannot know that *another specific
  controller* owns a particular `spec` subfield, so it is a heuristic, not the
  truth. Picking the default set is itself a judgement call.

## Convergence implications (must hold for any option)

The package's load-bearing invariant is convergence: after the first `Apply`,
`Decide` with the same desired must return `NoChange`, byte-stable
(`assertConverges`). Ownership interacts with it directly:

- An **unowned** field left in Git is, by construction, also absent from desired.
  On the next reconcile the object-equality fast path in `Decide` compares raw
  Git (which still has the unowned field) against desired (which does not) — they
  are **not equal**, so `Decide` returns `Patch`, and `Apply` re-runs the merge,
  which again leaves the unowned field in place with `changed == false`. The
  result is `NoChange`-by-merge and byte-stable. **Convergence holds**, but note
  the subtlety: the cheap equality short-circuit in `Decide` will *not* fire when
  unowned fields are present, so every reconcile pays for a full merge to
  reach the no-op. That is acceptable, but it means the "true no-op preserves
  bytes without re-encoding" path narrows once ownership is partial.
- Therefore any ownership option must run through `assertConverges` with a corpus
  that *includes unowned fields in Git*, to pin both the byte-stability and the
  decide-path behavior above.

A concrete follow-up the implementer should weigh: teach `Decide`'s equality
check to ignore unowned Git fields, so a partial-ownership no-op can still take
the cheap byte-preserving path. That keeps the folded-scalar/no-reflow guarantee
intact for shared documents. It is an optimization, not a correctness
requirement, and should land with its own test.

## Recommendation

Adopt **Option C now, with a default that makes the reverser a polite co-tenant**,
and keep Option B as a documented future upgrade behind the same `Owns` seam.

Rationale:

- The file-agnostic-placement vision is explicitly about attaching to existing,
  human-authored repositories. Whole-object truth (Option A) contradicts that the
  moment a second writer touches the same document, so it should not be the
  long-term default — even though it is the correct default for documents the
  reverser *authored* (a freshly placed file is entirely owned).
- Option B is the principled end state but is a project of its own; gating step 6
  (the integration milestone) on it would stall the comparison work that is
  otherwise ready.
- Option C delivers the safety that matters (don't clobber foreign fields) with a
  small, offline-testable predicate that plugs into the seam already built.

Proposed default policy (Option C):

- **Own** `spec`, `data`, `binaryData`, and the other top-level payload fields the
  projection already preserves (`rules`, `subjects`, `roleRef`, `webhooks`, …).
- **Own** `metadata.labels` and `metadata.annotations` *only for keys the desired
  object sets*; do **not** delete foreign label/annotation keys. (This is the one
  place where "own a subtree" must become "own specific keys within a subtree.")
- **Do not own** anything else in `metadata`, nor unknown top-level fields.

Crucially, ownership is **per (target, document)**: a file GitOps Reverser placed
itself is fully owned (Option A semantics), while a pre-existing shared document
gets the polite Option C policy. The integration layer chooses which predicate to
inject; the package stays mechanism-only.

## What to build (minimal, in-package)

This spike does not change the `manifestedit` public API — the `Owns` seam is
already there. The in-package work is:

1. A small set of **predicate constructors** (still policy-free helpers, or in the
   integration layer) that produce `Owns(FieldPath) bool` from a declared path
   policy. The "own labels/annotations only for desired keys" case needs the
   predicate to be evaluated against *desired* as well as path — confirm the
   current signature (`func(FieldPath) bool`) is enough, or widen it to
   `func(FieldPath, desiredHasKey bool) bool`. (Today the merge only consults
   `Owns` on the *absent-from-desired* branch, so "desired has the key" is
   already implied false at the call site — the current signature is likely
   sufficient; verify before widening.)
2. Decide whether to teach `Decide`'s equality fast path to ignore unowned fields
   (the convergence optimization above), or accept the full-merge-to-no-op cost.
3. Tests: a new `ownership_test.go` group covering (a) foreign metadata preserved,
   (b) owned payload still fully reconciled including deletions, (c) per-key
   label/annotation ownership, all routed through `assertConverges` with unowned
   fields present in Git.

## Open questions for the implementer

- **Granularity of the predicate.** Is path-prefix ownership enough, or do we need
  per-key ownership inside maps (labels/annotations) as a first-class case? The
  recommendation assumes the latter for `metadata`.
- **List items.** Keyed-list matching (step 4) does not yet attach ownership to
  individual list items; the item path is `…/<list>/<key>`. If ownership ever
  needs to protect a *foreign list item*, that path scheme must be honored by the
  predicate. Out of scope for the default policy, but note it.
- **Configuration surface.** Where does the policy come from — a built-in default,
  a `GitTarget` field, or both? This is a CRD/API decision and belongs with the
  integration milestone (step 6), not this package.
- **Authored vs adopted documents.** How does the integration layer know a
  document was authored by the reverser (→ Option A) versus adopted (→ Option C)?
  The inventory (`DocumentRecord`) is the natural place to carry that bit; decide
  it alongside placement.
