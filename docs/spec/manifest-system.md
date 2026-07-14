# The manifest system: how it works today

> **spec** — current behaviour. The code depends on this document; change one, change the other. Index: [`../INDEX.md`](../INDEX.md)

> Status: reference — describes shipped behaviour.
> Captured: 2026-07-11.
> Replaces the whole of `docs/design/manifest/`, which was 19 documents of
> working notes for a subsystem that has since shipped. The reasoning is in
> `git log`; this is the outcome.

This is the one document to read to understand how GitOps Reverser turns a live
Kubernetes object into a line in a Git file. It states *what is true now*. The
detailed contracts it summarises each have their own spec in this folder, linked
inline.

## The shape of it

```text
kube-apiserver
   │  watch (sendInitialEvents + bookmarks)
   ▼
internal/watch          ── which types can we follow at all?   → type-followability.md
   │
   ▼
internal/manifestanalyzer  ── the manifest store: parse the folder, index by identity
   │                          decide accept / refuse            → current-manifest-support-review.md
   ▼
   plan  ── what would change in Git?                           (report-only surface:
   │                                                             internal/manifestreport)
   ▼
internal/git/manifestedit  ── edit the YAML in place, preserving comments
   │
   ▼
internal/git   ── flush: write, commit, push with lease
```

Four packages, one direction: **live → Git**. Nothing in this pipeline renders
Kustomize or Helm, and nothing decrypts.

## The load-bearing rules

These are the invariants. Break one and the operator is unsafe rather than merely
limited.

**Identity is the key, not the file path.** A KRM document carries its own full
identity (group, kind, namespace, name). The store indexes by that identity and
writes an edit back to wherever the document already lives — *match-first
placement*. Being strict about which file a resource "should" live in was
explicitly rejected. Only a genuinely new resource needs a placement decision, and
that is [`gittarget-new-file-placement-rules.md`](gittarget-new-file-placement-rules.md).

**A GitTarget makes an all-or-nothing claim on its folder.** It either manages
everything in the subtree or it refuses the folder. There is no partial ownership,
because a partially-managed folder cannot be reasoned about — see
[`current-manifest-support-review.md`](current-manifest-support-review.md).

**Never partially materialize a multi-document file.** If one document in a file
is out of scope, unparseable, or non-KRM, the file is not half-written. The refusal
is the whole file.

**Refuse the folder rather than prune unwatched KRM.** Encountering API-backed KRM
in the folder that no WatchRule covers means the folder is refused
(`GitPathAccepted=False`), *not* that the document is deleted as drift. Deleting a
user's manifest because we were not told to watch its type is the worst thing this
system could do.

**At most one editable document per resource identity.** Two documents claiming the
same identity is not a prune opportunity, it is an invalid repository state: it
blocks startup and takes a live target down (`RepositoryValid=False`). The older
"first-occurrence-wins, delete the losers" idea was explicitly abandoned.

**The API wins — full-object ownership, never field-subset ownership.** When the
live object and the Git document disagree, the live object is the truth for the
whole object. There is no per-field ownership model, and the list of things
deliberately *not* built to support one is in
[`manifestedit-field-ownership-spike.md`](manifestedit-field-ownership-spike.md).

**Comments and scalar styles survive an edit.** The writer edits the YAML node tree
in place rather than re-serialising it. This is not cosmetic: a
`# {"$imagepolicy": ...}` setter comment is load-bearing to Flux, and a SOPS `mac`
binds the exact bytes.

**One encrypted file is one document.** SOPS encrypts a *file* as a single
cryptographic unit, so an encrypted file holds exactly one document and may never
mix ciphertext with plaintext — [`sops-single-file-no-multidoc.md`](sops-single-file-no-multidoc.md).

**No bookmark, no sweep.** Initial reconcile is a streaming list-watch with
mark-and-sweep. The sweep is a set-difference taken at the joined
`initial-events-end` bookmark, and a partial mark must never drive a sweep — a
missing bookmark fails closed. See
[`reconcile-via-watchlist-mark-and-sweep.md`](reconcile-via-watchlist-mark-and-sweep.md).

**One grouped commit = exactly one (author, GitTarget) tuple** —
[`commit-window-refactor.md`](commit-window-refactor.md).

**Subresources are not a manifest surface.** They are ignored by default. `/scale`
is the single exception, because Kubernetes standardises it as a view that writes
the parent's desired replica state — and even then only when the parent's replica
path is known. It lands as a bounded field patch on the committed parent, never as
a document of its own. See
[`scale-subresource-audit-rehydration.md`](scale-subresource-audit-rehydration.md).

**A rule change on target A must never re-snapshot target B** —
[`gittarget-isolation-on-rule-change.md`](gittarget-isolation-on-rule-change.md).

## Kustomize

The store follows the `resources` graph rather than the filesystem, so a document's
effective namespace can be inherited from a `kustomization.yaml` that does not sit
beside it. Raw identity (what the bytes say) and effective identity (what kustomize
would produce) are tracked separately, and an inherited namespace is kept *out* of
the file bytes on write. The supported subset, and why everything outside it is
refused rather than unimplemented, is
[`contextual-namespace-and-kustomize-folder-editing.md`](contextual-namespace-and-kustomize-folder-editing.md).

The governing constraint is invertibility: an edit must round-trip in both
directions. Generators, `patches*`, `namePrefix`/`nameSuffix`, `components`, remote
bases and Helm inflation are refused because they are one-way, not because nobody
got to them. The full boundary — what is supported, what is refused, and why — is
[`../design/support-boundary/support-contract.md`](../design/support-boundary/support-contract.md).

## Types

Not every type can be followed. `internal/typeset` answers one question — *is this
type followable, and if not, what is the single reason?* — with a funnel-ordered
check list and a kebab-case reason vocabulary. GVK↔GVR must be a bijection in both
directions; an ambiguous mapping is a hard, observable refusal. A type that
disappears from discovery gets a 60-second removal grace before its informer is
dropped, and flapping types are coalesced by a settle window
([`type-lifecycle-events-and-wobble-settling.md`](type-lifecycle-events-and-wobble-settling.md)).

Full model: [`type-followability.md`](type-followability.md). The GVK/GVR contract
underneath it: [`gvk-gvr-mapping-layer.md`](gvk-gvr-mapping-layer.md).

## Known gap: external Git drift

The cluster is the source of truth, and the pipeline is driven by cluster events.
So when something changes Git *out of band* — an external push, a human deleting
files on the branch — **nothing re-runs the path's reconcile to bring Git back in
line.** The drift is detected but not acted on.

The recommended fix, not yet built, is a per-GitTarget **subtree OID** compare that
arms exactly one replay-mark session when the subtree moves underneath us. This is
the one substantive open item inherited from the old `manifest/` folder and is
worth keeping in view.

A related membership rule *is* enforced: objects with a `deletionTimestamp`
(Terminating) must never be re-materialized.

## What happened to the old folder

`docs/design/manifest/` held 19 documents written while this subsystem was being
designed. All three of its subsystems — `internal/typeset`,
`internal/git/manifestedit`, `internal/manifestreport` — have shipped. Fourteen of
those documents were working notes, superseded proposals, or closed investigations
and have been deleted; four were live specs and now sit beside this file in
`docs/spec/`.

Nothing is lost: `git log --follow` has all of it.
