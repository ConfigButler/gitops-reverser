# Prompt: tolerate `patches:`, and route a simple one

Copy everything below the line into a fresh session.

---

Continue the kustomize support-boundary workstream. The next thing is `patches:`, and it
comes in two halves that must not be confused: **tolerating a patch** (accept the folder,
mirror what it renders) and **authoring a patch** (write one from scratch). The first is the
job. The second stays deferred and this change must not drift into it.

## Read first

- `AGENTS.md`.
- The memory note `kustomize-renderer-workstream`.
- **`docs/design/support-boundary/render-root-scoping.md` §6** — this is the design. It is
  titled "Why a patch still blocks the folder, and why it should not", and it already
  separates the two gates you need: *renderable* (per folder) and *routable* (per object,
  per field).
- `docs/design/support-boundary/render-attribution.md` §3 (the dye and its sink proof), §5
  (*attribution may be heuristic, verification may not*), §7 (the guardrails).

## Where things stand

PR #233 shipped the projection swap. Attribution is a **dyed render**: a nonce is written
into every declared override entry, the root is rendered a second time, and the entry a value
came from is read off the output. Verification is a **real re-render** (`VerifyBatchRenders`):
before any kustomize-governed flush is committed, the whole tree is rebuilt with the write
applied, every written document must reproduce its live object, and **every object the flush
did not write must come out byte-identical**. A proposal that fails refuses the flush and
names the file and the object.

The re-implemented transformers (`renderImage`, `imageSuppliers`, `simulateImageRender`,
`isReplicaKind`) are gone. Nothing left in the write path models kustomize; it asks kustomize.

## Why patches, and why now

Counted across the layout corpus, `patches` is **the single biggest refusal cause**: 8
occurrences, against `namePrefix` 6 and `configMapGenerator` 6. And it refuses the **whole
GitTarget**, not the edit. `flux-monorepo/apps/production`, whose patch touches replicas and
an env var, therefore also loses `images:`/`replicas:` edit-through, which the patch has
nothing to do with.

`render-attribution.md` §3 names the blocker in as many words: *"Per-field refusal requires
per-field attribution. The dye is the mechanism that milestone is waiting for."* That
mechanism now exists. This is the milestone it unblocks.

## Four things measured, not assumed. Do not re-derive them; do extend them.

1. **A folder containing `patches:` builds fine.** kustomize renders it without complaint.
   The refusal is *our fence*, not kustomize's. (Probe: add a `patches:` block to an
   `imageFixture`-style tree and call `renderRoot`. No build failure.)

2. **Patches run BEFORE `images:`/`replicas:`, and the transformers WIN.** Measured with a
   patch and an entry both aimed at the same fields:

   ```
   patch says image app:patched, images: says newTag v2   ->  renders app:v2
   patch says replicas 7,        replicas: says count 3   ->  renders 3
   ```

   **A patch that sets a field an entry also governs is DEAD TEXT.** This is the fact that
   makes "just edit the patch" wrong, and it is the first thing to get right.

3. **The dye already answers both cases correctly, with no model of patches.** Where the entry
   still matches, the dye lands and the value is attributed to the *entry* (the patch's value
   is dead text). Where the patch changes the image *name*, the entry stops matching, no dye
   lands, nothing is attributed, and the oracle refuses the write. Neither outcome required
   knowing what a patch is.

4. **A patch-owned field is currently unroutable, and fails safe.** No dye, so no attribution,
   so nothing routes to an entry; the write falls back to the source document, the re-render
   shows the patch overriding it, and the flush is refused. Wrong *outcome* for the user,
   right *direction*: it refuses rather than corrupting.

## The one thing not to get wrong

**Do not read the patch and reason about who wins.** That is re-implementation wearing a
better hat, and (2) is exactly the ordering rule you would get wrong. The patch is a sparse
KRM document, so its fields are trivially readable, and that is precisely the trap: reading
them tells you what the patch *asks for*, never what the build *does*.

**Ask kustomize.** Extend the dye to scalars inside the patch: write a nonce into the patch's
value, render, and see whether it survives to the output. If it does, the patch supplies that
field and an edit belongs in the patch. If it does not, something downstream overrode it, and
the dye that *did* land tells you who. Ordering, precedence and dead text all fall out for
free, and none of it has to be modelled.

**The sink proof is not optional.** §3: a dye is sound only where the dyed value is a **pure
sink**, never an input to a matcher. Inside a patch this bites harder than it does for images:

- **a list merge key is a selector, not a sink.** `containers[].name` is how strategic merge
  decides *which* container to merge into. Dye it and the patch merges into the wrong element,
  or creates a new one. Never dye a merge key. Enumerate the ones you rely on and say why each
  is safe.
- `metadata.name` / `metadata.namespace` in the patch body are the object selector. Same rule.
- `$patch: delete` / `$patch: replace` directives change what the merge *means*. A dye near one
  is not a sink.

**Baseline first, then dye** (§7): if the dyed build errors where the real one did not, the dye
hit something that is not a sink. Fall back to **NO ATTRIBUTION**, never to another heuristic.

## Order of work. Each stage is independently shippable; the risky one is last.

1. **Tolerate.** Stop refusing the folder for `patches:`. It comes out of the unsupported set
   for *acceptance*; it stays unsupported for *authoring*. Nothing routes to a patch yet, so a
   patch-owned edit is refused by the oracle exactly as it is today, with a message that says
   so. **Regenerate the corpus baseline and read it**: this is the stage whose whole point is
   the rows that move, and it is where you find out what tolerating patches actually exposes
   (`flux-monorepo` should go from Partial to more-accepted; look at what else it drags in).
   *Ship this alone if the rest slips.* It converts 8 whole-folder refusals into per-field
   ones.

2. **Attribute.** Extend the dye to patch scalars, with the merge-key guard above. A field the
   patch supplies is now attributable to `(patch file, field path)`.

3. **Route.** A live change to a patch-supplied scalar edits the **patch document**, at the
   same path, preserving its bytes and comments (this is `manifestedit` territory, the same
   way `PatchKustomization` preserves hand-authoring). The oracle proves it or refuses it.

4. **Refuse per field, not per folder.** Name the patch that owns the field and say that
   authoring is not supported. This is the tier-2 accounting in
   `unreflectable-edits-and-write-gating.md`.

## Scope: what "simple" has to mean, and it must be enforced, not assumed

Start with **one** shape and refuse the rest by name:

- a **single** strategic-merge patch per object (two patches touching one field is a precedence
  question you have not earned yet);
- **scalar** fields (`spec.replicas`, an env value, a resource limit). A change *inside a list*
  is merge-key semantics and is a different problem;
- `patches:` with a **`path:`** to a sparse KRM document. Inline `patch: |` and **JSON6902**
  (`op`/`from`/`value`) are not sparse KRM at all and should be refused explicitly rather than
  fall through.

Check what `FixKustomization` does with the deprecated `patchesStrategicMerge` and
`patchesJson6902` spellings before you write the parser: the analyzer already runs it
(`kustomization_parse.go`), and folded fields will arrive in `Patches` looking like something
they are not. **Measure it; do not assume.**

## The test net

The 12 `TestSplitDesired_*` tests now build a real tree, render it with kustomize, read the
attribution off a dyed render, and drive the projection with the result. Extend that harness;
do not build a second one. The corpus invariant to hold onto is
`TestProjection_InSyncCorpusFolderIsANoOp`: **an in-sync folder must project to a complete
no-op.** A patch that we mis-attribute will show up there as a phantom edit, across every
fixture in both corpora.

Add the ordering fact (2) as a fixture. It is the one a future refactor will break.

## Validation and delivery

Full sequence per `AGENTS.md`: `task fmt` → `generate` → `manifests` → `vet` → `lint` →
`test` → `test-e2e` (needs Docker; check `docker info`; run sequentially). Regenerate
`task gitops-layouts-baseline` and **explain every row that moves** — in stage 1 the moving
rows are the deliverable. Branch off `main`, push, open a PR, and report the honest line
delta, including when it is unflattering.

## How this workstream finds bugs

Every stage of it has found a real, shipped bug, and not one came from reading kustomize's
source. They came from making kustomize the arbiter and probing it with a throwaway test: the
regex image matcher, the missing `ReplicationController`, the digest that silently clears the
tag, the `int` that panics `DeepCopyJSON`, and the patch-versus-transformer ordering above.

**When you want to know what kustomize does, do not reason about it. Ask it.**
