# `deletecollection`: nudge for correctness, parse the body for attribution

## 1. Scope and one paragraph

A `deletecollection` audit event removes many objects at once but is **name-less**:
the per-type live tail skips it, and only a later checkpoint reconcile removes the
deleted objects from git. Today nothing prompts that reconcile, so a collection
delete lingers in git until the ~1h periodic sweep. This plan is **two tiers**:

- **Tier 2 — correctness floor (do first):** fire the resync nudge that already
  exists, so the wired `RequestResync → TypeSynced → deferred heal → scoped sweep`
  chain reconciles the type promptly and to ground truth. Smallest correct change.
- **Tier 1 — attribution path (do second, on top of Tier 2):** when the response
  body is present (it usually is for core types / CRDs — §2), opportunistically
  expand it into per-object **named, attributed** deletes for items that are actually
  gone, so the common case ("alice deleted configmap x/y/z") lands precisely through
  the normal commit path. Whatever the body misses or gets wrong, the Tier-2 sweep
  backstops, and the sweep's removals carry a `Co-authored-by` cause annotation as the
  attribution floor.

The two tiers answer the same question from both ends: Tier 1 gives precise
attribution when the body is trustworthy; Tier 2 guarantees correctness (and floor
attribution) when it is not. We never make the body the *correctness* source — §6.

## 2. Current state (verified)

- A `deletecollection` is classified `AuditEventQualityCollection` and emitted by the
  joiner — [audit_joiner.go:570](../../../internal/webhook/audit_joiner.go#L570). The
  classification only means "emittable with or without a body" so an aggregated, hollow
  proxy body does not block emission
  ([audit_joiner.go:573-579](../../../internal/webhook/audit_joiner.go#L573-L579)). It
  does **not** strip a real body: `ResponseObject` is carried through the envelope and
  merge ([audit_joiner.go:476-504](../../../internal/webhook/audit_joiner.go#L476-L504)).
- **The deleted-items list is already captured and stored.** The audit policy captures
  `delete`/`deletecollection` at `level: RequestResponse`
  ([policy.yaml](../../../test/e2e/cluster/audit/policy.yaml)), so for a standard
  etcd-backed core type (and CRDs via apiextensions) the response — a `List` of the
  removed items, names included — rides in the event. The full event, including
  `user` and the complete `payload_json` (request + response bodies), is stored in the
  per-type stream entry
  ([redis_bytype_queue.go:595-596](../../../internal/queue/redis_bytype_queue.go#L595-L596))
  and round-trips via `parseAuditEvent`
  ([audit_event_parsing.go:111](../../../internal/queue/audit_event_parsing.go#L111)).
  **So Tier 1 needs no joiner/ingestion change — the body is already in the mirror;
  only the consumer must learn to read it.** And the actor is never lost (load-bearing
  for §7).
- The live per-type tail **skips it**: `auditChangeFromEntry` returns `ok=false` for any
  name-less entry (`id.Name == ""`) —
  [redis_bytype_queue.go:342](../../../internal/queue/redis_bytype_queue.go#L342).
  Intentional (DEC-5) and correct.
- A `deletecollection` carries no usable RV, so `Enqueue` routes it through
  `ingestRVLess` —
  [redis_bytype_queue.go:464](../../../internal/queue/redis_bytype_queue.go#L464).
- The resync nudge already exists end-to-end and is already the backstop, invoked from
  **exactly one** call site — the `isIDTooSmall` branch of `ingestOrdered`
  ([redis_bytype_queue.go:450-451](../../../internal/queue/redis_bytype_queue.go#L450-L451)).
  The chain it drives: `NudgeTypeResyncForLateEvent`
  ([materialization.go:70](../../../internal/watch/materialization.go#L70)) →
  `RequestResync` ([materializer.go:348](../../../internal/typeset/materializer.go#L348))
  → `TypeSynced` deferred heal + scoped sweep
  ([materialization.go:439-450](../../../internal/watch/materialization.go#L439-L450)),
  wired in cmd ([main.go:235](../../../cmd/main.go#L235)).
- Backstop today: `materializationSweepInterval = time.Hour`
  ([materialization.go:54](../../../internal/watch/materialization.go#L54)).
- Commits already author from the actor: `Author` ← `AuthorUserInfo`, with
  `authorName`/`authorEmail` mapping
  ([commit.go:198-240](../../../internal/git/commit.go#L198-L240)); the message is built
  by `buildGroupedCommitMessageData`
  ([commit.go:111](../../../internal/git/commit.go#L111)). `Co-authored-by` trailers
  (§7) extend this without touching the git `Author`/`Committer` identity.

## 3. What is already correct — do not "fix" these

- **The tail skipping name-less entries** stays
  ([redis_bytype_queue.go:342](../../../internal/queue/redis_bytype_queue.go#L342)).
  Tier 1's per-object deletes come from the body expansion, not from un-skipping the
  name-less entry.
- **The checkpoint/sweep as the correctness plane.** Tier 2 reuses it as-is; Tier 1
  never replaces it.
- **The deferred-until-idle heal** (heal=true, Rec 1) — the nudge inherits it.
- **The collection event emitting with or without a body**, and the aggregated shallow-
  drop fix (2026-06-13). Tier 1 reads the body *when present* and degrades to Tier 2
  when it is hollow/absent — it does not re-introduce a body demand at ingress.
- **The 15s per-type nudge floor** (`lateNudgeMinInterval`,
  [materialization.go:60](../../../internal/watch/materialization.go#L60)) already
  coalesces teardown bursts. We lean on it explicitly.

## 4. The gap, located precisely (Tier 2)

A successful `deletecollection` enqueue does not request a resync, so the deleted
objects linger in git for up to one sweep interval (~1h).

The gap is in **`ingestRVLess`, not the late lane.** A `deletecollection` is RV-less,
so it never reaches the one branch that nudges. Two outcomes, neither nudges:

- **Warm stream (the common case):** attaches to the high-water with `rv_present=false`;
  the tail then skips it (name-less) → silent ~1h wait. The original analysis framed
  "lands in the late lane before high-water" as dominant — that is the **cold-start
  edge**; the everyday path is this warm attach.
- **Cold stream (the edge):** diverts to the late lane → also no nudge.

The fix nudges on **both** outcomes, scoped to **name-less / `verb == deletecollection`**,
not all RV-less events — ordinary single deletes are RV-less too and the tail already
applies them by name
([redis_bytype_queue.go:370-372](../../../internal/queue/redis_bytype_queue.go#L370-L372));
nudging those would fire a checkpoint LIST per delete.

## 5. Tier 2 — the nudge (do first)

1. **Thread the collection signal from `Enqueue` into `ingestRVLess`.** `byTypeAuditKeys`
   carries only `group`/`resource`, not verb/name
   ([redis_bytype_queue.go:385-395](../../../internal/queue/redis_bytype_queue.go#L385-L395)),
   so `Enqueue` must detect `verb == deletecollection` and pass that down (a param or a
   `byTypeAuditKeys` field). Small but real plumbing — call it out in review.
2. **Fire the existing nudge for that case only.** On a successful RV-less ingest of a
   `deletecollection`, call `q.lateNotify(keys.group, keys.resource)`, guarded like the
   existing site. `Subresource == ""` for a collection delete, so `keys.group`/`resource`
   are already populated
   ([redis_bytype_queue.go:218-224](../../../internal/queue/redis_bytype_queue.go#L218-L224)).
   Fire on **both** RV-less outcomes. Best-effort, non-blocking (IR8).
3. **Nothing downstream changes** — `RequestResync → TypeSynced → deferred heal → scoped
   sweep` is unchanged; the 15s floor coalesces; unclaimed/not-Synced is a no-op.

Net: a handful of lines plus the Enqueue→`ingestRVLess` signal.

## 6. Why the body is an attribution *hint*, never the correctness plane

The user's question — "if I delete a bunch of configmaps, isn't the deleted set in the
body? shouldn't we parse it?" — is right for the common case, and §2 shows the body is
already there. But the body must stay a Tier-1 *hint* under the Tier-2 sweep, because as
a *correctness* source it fails in ways that silently corrupt git:

- **Finalizers (the headline risk).** `deletecollection` does not remove an object that
  has a finalizer — it sets `deletionTimestamp`. The returned list item is that object
  *with the timestamp set*, not a confirmation of removal. Deleting it from git would
  **over-delete** (the object still exists) **and mis-attribute** (the actual removal
  happens later, under the finalizer controller's own event, not Alice's). **Tier-1
  rule: only act on a body item that is actually gone — no pending `deletionTimestamp`
  / finalizers; skip the rest and let the sweep + their own later delete events handle
  them.**
- **Partial bodies.** A large collection delete can fail partway and return a partial
  list → **under-delete**. The sweep backstops.
- **Aggregated / external apiservers** return a hollow body or a `Status` — no usable
  list (the case the joiner already tolerates). No list → Tier 1 no-ops, Tier 2 covers.
- **Policy dependence.** A `Metadata`-only policy for these resources means no body
  arrives at all. Tier 1 must degrade silently to Tier 2; it can never *assume* a body.
- **Shape variance.** The body may be a typed `…List`, a `v1.List`, or a `Status`.
  Parse defensively: list-with-items → expand; anything else → fall back.

So the body lets us be *precise and attributed* when it is trustworthy, and we lean on
state-based reconciliation when it is not. That is the honest division of labor: events
for attribution, state for correctness.

## 7. Attribution — Tier 1 precision, `Co-authored-by` floor, and the author-binding line

> "Alice can delete a namespace without being attributed for it" — a no-go.

Two mechanisms, layered, both honest:

**Tier 1 — per-object attributed deletes (the common case).** When the body yields named
items that are actually gone (§6 rule), emit them as ordinary **named deletes carrying
the actor** (the audit event's `user`, via `resolveUserInfo`). These go through the
*normal* commit path — real name, real author — so "alice deleted configmap x" is
precise and needs **no special author-binding handling at all**: it is just a delete,
indistinguishable from a single `kubectl delete configmap x`. This is the cleanest
possible attribution and it covers the case the user cares about most.

**Tier 2 — `Co-authored-by` cause annotation on the heal (the floor).** What the sweep
removes that Tier 1 did not attribute (finalizer survivors that later vanished, partial/
absent/aggregated bodies) still deserves a name. But a heal commit is a *generic,
state-reconciling fold* and must not be stamped as one author's, because:

- **The sweep is state-based** — it removes whatever is missing, which can exceed what
  Alice deleted (a finalizer survivor, Bob's earlier missed delete). Stamping it "Alice"
  is false.
- **Coalescing merges causes** — the 15s floor + per-type resync can fold several
  `deletecollection`s on the same type (Alice in `ns-a`, Bob in `ns-b`) into one heal.

So credit actors as **`Co-authored-by:` trailers**, which the user proposed and which fit
the existing message builder
([commit.go:111](../../../internal/git/commit.go#L111),
[commit.go:226-240](../../../internal/git/commit.go#L226-L240)) exactly:

- Carry an attributable **cause** — `{operation, username, email, auditID, namespace,
  resource}` read from the stored event (§2) — into the resync request (a `ResyncCause`,
  distinct from per-object author binding).
- The heal commit keeps the **operator/reconciler** as git `Author`/`Committer` (it is a
  reconcile, not a human's window) and appends one **`Co-authored-by: Name <email>`** per
  distinct actor, reusing `authorName`/`authorEmail`. Multiple causes → multiple
  trailers — which, as the user notes, is what should be possible in theory.
- This respects the standing rule
  ([commitrequest-author-binding-steer]): identity comes from the audit path / per-type
  mirror; **no window is finalized**, no false per-object claim is made (trailers credit
  contribution, they are not the commit's `Author`); and it is **fail-closed** — an
  unattributable cause leaves a generic trailer-less portion ("…and other unattributed
  changes"), never an invented author.

**The reassurance:** even before Tier 1 ships, the actor is never lost — the
`deletecollection` event with `user` is in the mirror (§2). So a v1 that ships only Tier
2 already records *who* via the `Co-authored-by` floor; Tier 1 then upgrades the common
case from "co-authored heal" to "precise named delete."

## 8. Unit tests — red-first

Tier 2 (`internal/queue`; red against current code, green after §5):

1. **RED → GREEN:** `deletecollection` enqueue fires the nudge on the warm high-water
   path (the common case).
2. **RED → GREEN:** fires on the cold late-lane path (first write to the stream).
3. **RED → GREEN:** the Enqueue→`ingestRVLess` collection signal is wired (guards the
   plumbing against a silent refactor drop).

Guards (green throughout): a normal named delete does **not** nudge; `ReadTypeAuditChanges`
still skips a name-less `deletecollection`
([redis_bytype_queue.go:342](../../../internal/queue/redis_bytype_queue.go#L342)).

Materialization: claimed+Synced nudges `RequestResync`; unclaimed/not-Synced no-ops;
repeats within the floor coalesce; a post-nudge `TypeSynced` sweeps only that ScopeGVR
and leaves siblings/namespaces/multi-doc intact.

Tier 1 (body fan-out):

4. **A list body with three gone items expands to three named, attributed deletes**
   crediting the actor.
5. **A finalizer-pending item (deletionTimestamp + finalizers) is skipped**, not deleted
   from git (the headline §6 rule) — the sweep is left to handle it.
6. **A hollow / `Status` / absent body no-ops Tier 1** and falls through to the Tier-2
   nudge (aggregated and `Metadata`-policy cases).
7. **A partial list under-deletes safely** — Tier 1 emits what it has, Tier 2 reconciles
   the rest.

Tier 2 attribution floor:

8. **Cause survives Enqueue → resync;** a single-cause heal emits one `Co-authored-by`;
   a coalesced multi-cause heal emits one trailer per distinct actor; an unattributable
   cause yields a generic trailer-less portion.

## 9. E2E — red-first (and flake-aware)

The policy already captures `deletecollection` at RequestResponse
([policy.yaml](../../../test/e2e/cluster/audit/policy.yaml)) — no policy change. Red/green
hinges on **promptness** (the only backstop today is the ~1h sweep), so "objects gone
from git within a bounded window" fails today and passes after §5.

**Flake warning:** the known aggregated-`deletecollection` shallow-drop / absolute-drop-
count flake means new e2e must run on a **fresh cluster** and assert **convergence**
("these specific objects gone, these siblings remain"), never a global drop count on
aggregated paths.

Scenarios (`test/e2e/deletecollection_e2e_test.go`):

1. **`kubectl delete configmap --all -n <ns>`** — objects disappear from git within a
   bounded window. RED today, GREEN after §5. With Tier 1: assert the commits are
   **named, attributed deletes** crediting the actor.
2. **Label-selector delete** with a non-matching sibling — only matching objects leave
   git; sibling survives (convergence assert).
3. **Finalizer survivor** — a configmap with a finalizer in the deleted set stays in git
   until its finalizer clears (proves Tier 1 does not over-delete / mis-attribute).
4. **Teardown burst** — convergence + the 15s floor coalesces (no resync storm); the heal
   commit carries `Co-authored-by` for each actor.
5. **Aggregated-API path — already covered** by existing fixtures (hardened 2026-06-13);
   confirm green, no new aggregated assert.

## 10. Definition of done

- **Tier 2 first:** §8.1–8.3 written red-first then green; guards green throughout; §9.1
   demonstrated RED→GREEN on a fresh cluster with convergence asserts. The `Co-authored-by`
   floor (§8.8) lands with Tier 2 so v1 already records *who*.
- **Tier 1 second, on the Tier-2 floor:** §8.4–8.7 (esp. the finalizer-skip rule) and
   §9.1's attributed-delete assert + §9.3 finalizer survivor.
- Full validation per AGENTS.md: `task fmt → generate → manifests → vet → lint → test →
  test-e2e` (e2e sequentially; clean-cluster first if k3d is stale).
- README/chart wording: collection deletes trigger **prompt reconciliation/sweep**, with
  **opportunistic attributed per-object deletes when the API server returns the deleted
  set** — not unconditional per-item fan-out (§6).
- Author-binding respected throughout: Tier 1 deletes are ordinary named/authored writes;
  Tier 2 credits actors via `Co-authored-by` trailers, fail-closed, never as the git
  `Author` of a heal and never finalizing a window.
