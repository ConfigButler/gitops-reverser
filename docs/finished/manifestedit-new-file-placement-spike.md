# New-file placement: where does a brand-new resource's file go?

> Status: decided (first version) — supersedes the original open brainstorm seed
> Related: [file-agnostic-placement.md](../design/manifest/file-agnostic-placement.md),
> [manifest-inventory-file-agnostic-placement.md](../design/manifest/manifest-inventory-file-agnostic-placement.md),
> [manifestedit-integration-readonly-reconcile.md](../design/manifest/manifestedit-integration-readonly-reconcile.md),
> [manifestedit-field-ownership-spike.md](manifestedit-field-ownership-spike.md),
> [manifestedit-abstraction-plan.md](../design/manifest/manifestedit-abstraction-plan.md),
> [architecture.md](../architecture.md)

## The one question

When the reverser must write a resource that has **no existing document in Git**,
*where does the file go?* The manifestedit comparison is explicit that this is
out of its scope: the two-version comparison requires an existing `Git` document,
and the read-only report already classifies this cell as `ActionCreate` with the
reason *"no existing document in Git; placement is an upstream decision"*
([internal/manifestreport/report.go](../../internal/manifestreport/report.go)).
Placement is precisely that upstream decision, and it is currently unowned as a
configurable policy.

## What is already true (don't re-litigate)

- **Today's placement is deterministic and implicit.** New files are written to
  `{spec.path}/{group}/{version}/{resource}/{namespace}/{name}.yaml`
  (`.sops.yaml` for encrypted Secrets), via `generateFilePath` /
  `ResourceIdentifier.ToGitPath()`
  ([internal/git/git.go](../../internal/git/git.go),
  [internal/types/identifier.go](../../internal/types/identifier.go)). Repo state
  was historically discovered by parsing that path back into an identity.
- **Identity is content, not path.** The inventory
  ([internal/git/manifestedit](../../internal/git/manifestedit)) keys resources by
  GVK + namespace + name read from the YAML, so a resource can be *found* at any
  path, not only the deterministic one.
- **In-place editing already landed (narrowly).** When a document already exists
  for a resource, the writer edits it in place preserving formatting
  ([git.go](../../internal/git/git.go) `preserveExistingFormatting` →
  `manifestreport.EditInPlace`). Placement is the missing other half: it only
  matters when the resource is *absent* from Git.
- **The policy north star is API-first, whole-object truth**
  ([manifestedit-field-ownership-spike.md](manifestedit-field-ownership-spike.md)).
  Whatever placement does, the API stays the source of truth.

## The reframe that drives every decision

The thing that makes today's behavior feel rigid is that the path is a *pure
function of identity, recomputed on every write* (`ToGitPath`). The file-agnostic
vision flips that: identity is content, and **location is data the inventory
owns**.

So placement is not "a better path formula." It is a **resolver that runs once,
when a resource is brand-new, and whose result is then recorded in the inventory**
like any other location. Recomputing a path on every write is what couples the
layout to the storage contract; resolving once and recording removes that
coupling — and quietly defuses most of the original open axes (renames, append)
before they have to be designed.

## Decisions (first version)

### 1. Match-first is an invariant, not a setting

The write path always asks the inventory "do we already have a location for this
identity anywhere under the GitTarget path?" first, and only invokes placement
when the answer is no. This is not a user knob — it is simply correct, and it is
the bridge from "edit at the deterministic path" to true file-agnostic placement.

Cost: the inventory is built from the already-checked-out commit and cached for
the lifetime of the Git transaction (never a per-write re-scan). The
rebuild-on-change gate in
[manifest-inventory-file-agnostic-placement.md](../design/manifest/manifest-inventory-file-agnostic-placement.md)
already governs when it is rebuilt.

### 2. Placement is create-time and non-retroactive

Once a file is placed, its location lives in the inventory. Changing the
placement policy later affects **only resources created after the change** — it
does **not** move existing files. This is load-bearing:

- No rename/move machinery in this version.
- No new prune-hazard surface: the writer never relocates a file, so it never
  produces a "delete the old path + write the new path" pair.
- A future explicit "reorganize / migrate layout" action can come later as its
  own opt-in operation, separate from placement.

### 3. A `Placement` seam returning a `ManifestLocation`

Placement is *policy* and lives in the integration/writer layer
(`manifestreport` / the writer), never in the `manifestedit` mechanism — the same
mechanism-vs-policy line drawn elsewhere. Shape it so the type can express
everything we will ever want, even though the first version only uses part of it:

```go
type ManifestLocation struct {
    Path          string // relative to the GitTarget path
    DocumentIndex int    // -1 == new file
    Mode          PlacementMode // CreateFile | AppendToFile
}

type Placement interface {
    Locate(id types.ResourceIdentifier, spec GitTargetSpec) (ManifestLocation, error)
}
```

The first version always returns `{path, -1, CreateFile}` (a discrete,
one-resource file). Because the *type* can already say "append to this file at
this document index," multi-document append becomes a new `Placement`
implementation later with **zero reshaping** of the seam.

### 4. `spec.placement.layout` is a closed enum, default `apiStructure`

The user-facing surface is a small closed enum, not a free-text template. Every
layout must encode the **full identity** (group/version/resource + namespace +
name) somewhere in its path, just arranged into a different folder shape. That
makes each layout a **bijection with identity**: two distinct resources can never
resolve to the same path, so the enum needs **no uniqueness/collision validation
at all**. "Is this layout bijective with identity?" is the admission test for any
future layout.

```text
spec.placement.layout:
  apiStructure    # DEFAULT — {group}/{version}/{resource}/{namespace}/{name}.yaml
                  #   byte-identical to ToGitPath today → an absent spec.placement
                  #   field reproduces current behavior exactly (zero migration).
  namespaceFolder # {namespace}/{group}/{version}/{resource}/{name}.yaml
                  #   namespace on top: a namespace's resources live under one
                  #   folder, which is what makes dropping a kustomization.yaml in
                  #   it (and hooking it into Flux/Argo per namespace) natural.
```

Ship exactly these two. `flat` (single folder, identity encoded into the
filename) and a kind-first variant are each a few lines to add later on the same
seam and are intentionally *not* shipped speculatively — they are listed as
future-trivial, not as v1 knobs. Per-target setting for now; a provider-level
default with per-target override is a later refinement if it is ever asked for.

### 5. The SOPS extension decision lives inside the placer

The `.sops.yaml` extension for sensitive resources is decided **inside**
`Locate`, not bolted on around it. This guarantees no current or future layout
can route a Secret (or other sensitive resource) to a cleartext path — the
sensitive-path rule is structurally inside the one place that computes the
destination.

## Deferred (and absorbed by the seam, so no future reshaping)

- **Multi-document append** — a future `Placement` implementation; the
  `ManifestLocation` type already expresses it.
- **Renames / reorg / layout migration** — a future explicit opt-in action;
  decision 2 keeps existing files put in the meantime.
- **Namespace elision** (`writeNamespace`) — already deferred in the vision doc;
  orthogonal to placement.
- **Path templates** — deliberately not in the closed enum; would reintroduce
  collision/path-safety validation that the bijection rule avoids. Can be added
  later as one more `Placement` implementation if a concrete need appears.

## Hard constraints any answer must respect

- **The Git transaction boundary.** Placement decisions are valid only for the
  checked-out commit: fetch → index → compare → place/edit → commit →
  push-with-lease → replay-on-race
  ([manifestedit-integration-readonly-reconcile.md](../design/manifest/manifestedit-integration-readonly-reconcile.md)).
- **The prune hazard stays deferred.** Moving/placing files must not turn a
  partial cluster view into spurious deletions; deletes are still report-only.
  Decision 2 (non-retroactive placement) adds no new prune surface.
- **Encryption.** Secrets must land at `.sops.yaml` paths and never be written in
  cleartext — enforced by decision 5.
- **Convergence.** A placed file, re-read next reconcile, must be a byte-stable
  no-op (the property `assertConverges` guards inside manifestedit).
- **Mechanism vs policy.** Placement is *policy* and belongs in the integration
  layer (`manifestreport` / the writer / a GitTarget setting), never baked into
  the `manifestedit` mechanism (decision 3).

## Suggested implementation order

1. Introduce the `Placement` seam + `ManifestLocation` with a single
   `apiStructure` implementation that reproduces `ToGitPath` exactly, and wire the
   writer to call it (behind match-first) instead of `generateFilePath`. Pure
   refactor — output is byte-identical, existing e2e assertions unaffected.
2. Add `spec.placement.layout` to `GitTargetSpec` defaulting to `apiStructure`;
   confirm an absent field is still byte-identical.
3. Add the `namespaceFolder` implementation + tests (including a placed-then-
   re-read convergence assertion and a SOPS-extension test).
