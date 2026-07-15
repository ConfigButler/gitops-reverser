# Kustomize `images:` / `replicas:` edit-through

> Status: shipped ([#198](https://github.com/ConfigButler/gitops-reverser/pull/198)) —
> phases A–C landed together with this doc; the ambiguity refusal projection into
> GitTarget status remains future work. Filed under `finished/`.
>
> **Historical record.** Later render-root work supersedes this document's old
> no-renderer and no-multi-root limitations. Current behaviour is stated in the
> [support contract](../support-contract.md).
> Captured: 2026-07-06
> Related:
> [../README.md](../README.md),
> [contextual-namespace-and-kustomize-folder-editing.md](../../../spec/contextual-namespace-and-kustomize-folder-editing.md),
> [manifestedit/DECISION.md](../../../../internal/git/manifestedit/DECISION.md)

## Problem

`hasUnsupportedKustomizeFeature` deliberately treats `images:` and `replicas:`
as benign: they do not create resources or change identity, so a kustomization
using them is accepted. But the writer does not *understand* them. With an
overlay like:

```yaml
# kustomization.yaml
namespace: app
resources: [deployment.yaml]
images:
  - name: ghcr.io/example/podinfo
    newTag: "6.5.0"
```

the live Deployment carries `ghcr.io/example/podinfo:6.5.0` while
`deployment.yaml` says `:6.4.0`. Today `patchExisting` sees that as drift and
writes `6.5.0` into `deployment.yaml`. The render stays correct (the
transformer is idempotent), but:

- the source file's tag becomes dead text shadowed by the override;
- every deliberate tag bump lands in the wrong place (the source file instead
  of the override entry a human would edit);
- the repo silently stops being the file a human authored.

`replicas:` has the same shape with `spec.replicas`.

## Decision

**The edit lands where the value lives.** For every governed component (image
name / tag / digest per container; replica count), determine its current
*supplier*: the last override entry in the document's build chain that sets
that component, or the source document when no entry does. A live change to a
component is applied to its supplier:

- supplier = override entry → update `newTag` / `newName` / `digest` /
  `count` **in the kustomization file**, preserving comments and order; the
  source manifest keeps its bytes.
- supplier = source document → the normal in-place patch path (today's
  behavior).
- live already equals the forward render → **no change anywhere** (this is
  the write-through fix: today the source file is patched).

### Boundaries (v1)

1. **Only existing entries are edited.** The operator updates fields that are
   already present on an entry that already matches. It never adds or removes
   `images:`/`replicas:` entries, never adds missing fields (`newTag` on an
   entry that lacks it), and never creates a kustomization file. The human
   declares where the knob lives; the operator turns the knob. Whether drift in
   one environment should *create* a new override entry is a separate policy
   question, and is not supported.
2. **Unambiguous context only.** Override routing applies when every supported
   render root that reaches the document applies an identical override chain.
   Distinct chains from multiple roots emit an `ambiguous-kustomize-overrides`
   diagnostic and originally fell back to plain write-through.

   > **Superseded (write boundary).** The write-through fallback is gone.
   > A planned write into a file flagged `ambiguous-kustomize-overrides` now
   > refuses the flush (`write-fan-in`) and fails the GitTarget with
   > `WriteBoundaryRefused`; nothing is committed. See
   > [../gittarget-granularity-and-cross-environment-edits.md §1](../gittarget-granularity-and-cross-environment-edits.md).
   > Render-root scoping now generalises the check to any file reached by more
   > than one render root; this historical section records the earlier scope.
3. **Acceptance tightens only for garbage.** A kustomization whose `images:`
   or `replicas:` value is present but not structurally parseable (not a list
   of maps, missing `name`, non-string image fields, non-integer count) is
   marked unsupported and refused — such a file fails `kustomize build`
   anyway, and we can no longer claim to understand the render. Well-formed
   entries keep the folder accepted exactly as today.
4. **Plain documents only.** Sensitive (SOPS) documents keep the
   re-encrypt-wholesale path with no override routing. Among field patches
   (subresource events), the `/scale` case is routed — a `spec.replicas`
   assignment governed by a replicas entry updates the entry, never the file;
   other field patches are already bounded and skip routing.
5. **New containers write through.** A container present live but absent from
   the source document is written as-is. If its written source form then
   matches an override entry, the supplier rule converges the tag into the
   entry on the next event/resync — self-healing, no special case.

### Transformer semantics implemented

- `images:` entry = `{name, newName?, newTag?, digest?}`. An entry applies to
  an image whose *current* name (at that point in the chain) equals `name`.
  `newName` replaces the name, `newTag` the tag, `digest` the digest. Entries
  apply in listed order within a kustomization; kustomizations apply
  innermost-first along the reference path from the file to the render root
  (kustomize renders bases before applying a parent's transformers).
- Image fields are any string `image` inside items of a sequence field named
  `containers`, `initContainers`, or `ephemeralContainers`, at any depth —
  mirroring the builtin transformer's generic traversal.
- `replicas:` entry = `{name, count}`. Applies to `spec.replicas` of
  Deployment / ReplicaSet / StatefulSet documents whose `metadata.name` equals
  `name` (the builtin transformer's fieldspec set).

## Mechanics

The single integration point is `patchExisting`
(`internal/git/plan_flush.go`): both the steady-state event path and the
resync mark-and-sweep upsert funnel through it, so fixing it fixes both.

### Phase A — model (`internal/manifestanalyzer`)

- `parseKustomizations` also parses `images` and `replicas` entries;
  present-but-unparseable marks the kustomization unsupported (Decision 3).
- The existing render-root graph walk (`assignFromRoot`) additionally records,
  per resource file, the ordered **override chain**: the kustomizations along
  the reference path (innermost → root) that carry any override entries. Every
  distinct path within a root is recorded (cycle protection is on the current
  path, not per walk), so a diamond — one root reaching a shared base through
  two overlays — trips the ambiguity refusal instead of silently attributing
  the first path.
- `DocumentModel` gains `Overrides *KustomizeOverrides` — the collapsed,
  unambiguous chain (nil when none or ambiguous), each entry carrying its
  source kustomization path so the writer knows which file to edit.
- Conflicting chains from multiple roots emit the
  `ambiguous-kustomize-overrides` diagnostic and leave `Overrides` nil.
- Corpus: `testdata/contextual-namespace/supported/images-overlay`,
  `supported/replicas-overlay`, `unsupported/ambiguous-images`.

### Phase B — projection (`internal/manifestanalyzer`)

A pure function: given the source document's object, the live (projected)
object, and the override chain, return

- the **desired-for-file** object: governed components rewritten back to the
  source values wherever the supplier is an override entry (so the file diff
  disappears), everything else untouched;
- the list of **override edits**: `(kustomization path, images|replicas,
  entry name, field, new value)` for components whose live value diverges
  from the forward render and whose supplier is an entry.

### Phase C — writer (`internal/git`, `internal/git/manifestedit`)

- `manifestedit` gains a narrow kustomization editor: set the scalar value of
  an existing field on an existing entry (`images[name=X].newTag`,
  `replicas[name=Y].count`) via yaml.v3 node editing — comments and order
  preserved, single-document files only, refuse-with-diagnostic otherwise.
- `patchExisting` runs the phase-B projection when the document has an
  override chain, hands desired-for-file to `Decide`/`Apply` as today, and
  applies override edits to the kustomization file's buffer through the same
  `fileBuffer` machinery (so `.gittargetignore` shadow checks and the flush
  path apply unchanged).

### Phase D — docs and observability

- `docs/architecture.md`: the Manifest Aware Writer and acceptance-gate
  sections document the supported subset change ("images/replicas overrides
  are understood and edited through").
- Log one line per override edit (kustomization path, entry, field). The
  ambiguity fallback is recorded as a store diagnostic, surfaced by the
  analyzer CLI / scan report and logged by the live writer once per write
  batch at debug verbosity (`manifest store diagnostic`). A GitTarget status
  surface for override routing is deferred.

## Known limitation: shared entries with divergent live consumers

One `images:` entry is a **shared knob**: kustomize rewrites every matching
image in the build, so bumping `newTag` moves all consumers together. The
router mirrors that faithfully for the normal case — when consumers move
together (the entry is bumped Git-side, or a caller bumps "the app version"
live for all of them), the routing is stable and idempotent: the first event
updates the entry, later events for sibling consumers see live == render and do
nothing.

But if consumers **diverge live** — one Deployment is hot-bumped to a new tag
while another keeps the old one — that state is *unrepresentable* in the
source layout: one entry cannot hold two tags. No deterministic writer can
mirror it. The behavior is then: the entry follows whichever consumer's
change was processed last, and a full resync can alternate the entry between
the divergent values (each pass "corrects" it toward a different consumer).
The render always matches at least one consumer, never both.

Remedies: align the consumers live, or give each workload its own image name
(and entry) so each has its own knob. A cross-consumer consistency refusal at
edit time needs the siblings' live state, which the steady-state writer does
not have — batch-wide edit reconciliation is not built.

## What this deliberately does not do

> **Superseded in part.** The implementation now uses Kustomize's renderer,
> source-form projection, and render-root scoping. This historical list is retained
> only to delimit the #198 delivery. The current contract is
> [Kustomize support boundary](../kustomize-support-boundary.md): entry creation,
> patch authoring, name mutation, and generators remain out of scope; narrow
> external-base overlay editing is shipped.

## Test plan

- Corpus folders pin chain attribution and ambiguity (phase A).
- Table-driven unit tests for forward render + supplier resolution + inversion
  (phase B): tag change → entry edit; name change with `newName` → entry edit;
  tag change with no `newTag` on the matching entry → file patch; chained
  kustomizations; multiple containers; initContainers; replicas on the three
  supported kinds and ignored elsewhere.
- Writer tests in the `inplace_edit_test.go` style (phase C): governed tag
  change edits kustomization.yaml only; ungoverned change patches the source
  file only; mixed change routes both; no-op when live equals render; resync
  does not churn a governed folder; comment preservation in the kustomization
  file; SOPS documents unaffected.
