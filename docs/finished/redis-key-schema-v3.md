# Redis key schema v3 — author and watch namespaces

> Status: **IMPLEMENTED** — 2026-06-29. The `v2 → v3` key-format bump landed for the live Redis families:
> the audit author index, the watch-resume cursor, and the new command-author **store** (storage
> mechanics only — the CommitRequest admission/controller rewrite in
> [commitrequest-admission-authorship.md](../spec/commitrequest-admission-authorship.md) is a separate round and
> is **not** wired up yet). Scope: (1) replace the opaque `attr` namespace with a top-level `author`
> domain; (2) re-lay audit-sourced resource author facts as a readable, **object-grouped** hierarchy
> (`<group/resource>:object:<uid>:<rv>` + `:last`, plus a type-scoped `:rv:<rv>` escape hatch), carrying
> name/namespace in the value; (3) put command authorship from admission in the same `author` domain but
> a separate `command` subfamily; and (4) replace the old `watch-cursor` family with a smaller `watch`
> domain whose leaf is the per-shard `last-rv`.
>
> **Implementation note (resolver / rv: hatch).** §4.2 reads as if an exact-capable event consults
> *only* `object:<uid>:<rv>`. As shipped, an exact-capable event tries the exact key **and then** the
> type-scoped `rv:<rv>` key (still never `:last`). This is what makes the `rv:` escape hatch reachable —
> §5's own parenthetical ("…the watch side always carries a UID, so it would resolve via
> `object:<uid>:…` first and never reach it") confirms `rv:` sits in the fallback chain. Without that
> fallback the `rv:` key would be dead. `rv:` is type-scoped and per-write, so consulting it does not
> reintroduce the stale-LWW hazard `:last` would.
> Related:
> [`internal/queue/attribution_index.go`](../../internal/queue/attribution_index.go),
> [`internal/queue/redis_store.go`](../../internal/queue/redis_store.go),
> [`internal/watch/author_resolver.go`](../../internal/watch/author_resolver.go),
> [CommitRequest authorship from admission](../spec/commitrequest-admission-authorship.md),
> [deletecollection attribution & deletion-as-intent](../spec/deletecollection-attribution-expander.md),
> [watch event ordering & attribution grace](../facts/watch-event-ordering-and-attribution-grace.md),
> [watch-first ingestion architecture](watch-first-ingestion-architecture.md).

## 1. Today (`v2`) and the companion plan

Two live key families share the `gitops-reverser` prefix today. The CommitRequest admission plan adds a
third family unless it is folded into this naming pass before implementation:

| Family | Owner | Key (`:`-joined, fields escaped) | TTL |
|---|---|---|---|
| Audit-sourced resource author fact | `AttributionIndex` | `gitops-reverser:attr:v2:<variant>:<group>:<resource>:<ns>:<name>:<uid>[:<rv>]` | 10 min (fact), 20 min (`:seen`), 20 min (`:miss`) |
| Watch cursor | `RedisStore` | `gitops-reverser:watch-cursor:v2:<gitTargetUID>:<group>:<version>:<resource>:<ns>` | 1 h |
| Command author (planned) | `CommandAuthorStore` | `gitops-reverser:command-author:v1:<uid>` in the companion sketch | 1 h |

Older `docs/finished/` material mentions per-type Redis streams, object snapshots, and demand gates from
the retired `poc/redis-copy` line. Those are not live Redis owners in this branch, so this proposal does
not assign them new keys.

The audit author family writes **up to three keys per audit event**, strongest first
([`factKeyVariants`](../../internal/queue/attribution_index.go#L429)):

- `e` — **exact**: `(group, resource, ns, name, uid, rv)` — only when both uid *and* rv are known.
- `u` — **uid-only**: `(group, resource, ns, name, uid)` — only when uid is known.
- `r` — **rv-only**: `(group, resource, ns, name, rv)` — only when rv is known.

A watch event's resolver reads the same three variants in the same order and takes the first hit. In v2,
each fact key carries a `:seen` tombstone (2× TTL — lets a late lookup return `expired` vs `absent`) and a
`:miss` marker (lets a late `RecordFact` emit `op="late"`). The v3 proposal deletes both. Correctness
moves into the resolver (§4.2), and late-arrival metrics become a separate observability problem rather
than extra Redis keys in this schema.

Two real `:seen` tombstones, verbatim:

```
gitops-reverser:attr:v2:u::configmaps:1782713884-test-commit-author:commit-author-oidc-cm-1782713884:bbdecba6-4d33-40ee-8007-8269e28e799c:seen
gitops-reverser:attr:v2:u:apps:deployments:1782713884-test-commit-request-bundle:bundle-deploy-a-1782713884:bb1c4d09-4dce-4cc2-be34-f9bff17406c4:seen
```

Note the `u::configmaps` double-colon: the core group is empty, so it occupies an empty key field. The
key is wide, redundant (ns+name are also on the value-to-be), and the variant prefix `e/u/r` is opaque.
`attr` has the same problem at the family level: it is short, but it hides the thing Redis is actually
storing. The clearer root is **`author`**. `audit` belongs one level below it as provenance, not as the
top-level domain.

## 2. The proposal — `author:v1:audit`, grouped under the object

The same three join keys as v2, but **grouped under the object** and made self-describing. Everything
that was a key field except the type, the UID, and the RV moves into the value.

```
# exact — one immutable fact per write (uid + rv)
gitops-reverser:author:v1:audit:apps/deployments:object:bb1c4d09-4dce-4cc2-be34-f9bff17406c4:101
# uid latest — last-writer-wins pointer for the object (the RV-mismatch fallback)
gitops-reverser:author:v1:audit:apps/deployments:object:bb1c4d09-4dce-4cc2-be34-f9bff17406c4:last
# rv-only — type-scoped escape hatch for a fact that has an RV but no UID (see §5)
gitops-reverser:author:v1:audit:apps/deployments:rv:101
```

- **GroupResource** = `resource` for core (`configmaps`, `secrets`), `group/resource` otherwise
  (`apps/deployments`, `rbac.authorization.k8s.io/roles`). Readable, `SCAN`-friendly per type.
- **`object:<uid>:…`** groups every fact for one object under one prefix — `SCAN
  author:v1:audit:apps/deployments:object:<uid>:*` shows the whole history-in-flight for that object.
- **`author:v1`** is the durable naming decision. `audit` is the source-specific subfamily for
  persisted-resource authorship; command authorship uses `author:v1:command` (§9).
- **`:<rv>`** is the exact per-write fact; **`:last`** is the latest-writer pointer.
- **`rv:<rv>`** is the rv-only fallback, sitting under a `rv:` sibling of `object:` (no UID in it),
  **type-scoped** because RV is not globally unique (§5). Shipped from the start, not deferred.
- **Both `:last` and `:<rv>` hold the full author fact** — `:last` is just a copy of the most recent
  write, so the resolver never needs a second round-trip. (In the end that *is* what `:last` is.)
- **Value** carries what the key no longer encodes: `groupResource`, `namespace`, `name`, `uid`, plus
  the existing author/evidence fields.

```jsonc
// Audit AuthorFact in author:v1:audit (added fields marked +)
{
  "groupResource": "apps/deployments",   // +
  "namespace": "team-a",                 // +
  "name": "web",                         // +
  "uid": "bb1c4d09-…",                   // +
  "author": "alice@example.com",
  "displayName": "Alice", "email": "alice@example.com",
  "verb": "update", "auditID": "…", "resourceVersion": "101", "stageTimestamp": "…",
  "isServiceAccount": false
  // note: no "conflict" field — see §4
}
```

No sibling keys are retained: no `:seen`, no `:miss`. Without tombstones, an aged-out fact is
indistinguishable from one that never arrived, so the new contract treats both as `absent`. Late-arrival
metrics still matter, but they should be designed separately from this key schema.

### 2.1 GroupResource format — recommendation

Use the **`group/resource`** form (core drops the group: `configmaps`). It reads like an API path. The
Go stdlib `schema.GroupResource.String()` is **not** a drop-in: verified against this repo's pinned
[apimachinery v0.36.2](../../go.mod), it returns the *reversed, dot-separated* form —

```go
// k8s.io/apimachinery/pkg/runtime/schema/group_version.go
func (gr GroupResource) String() string {
	if len(gr.Group) == 0 {
		return gr.Resource
	}
	return gr.Resource + "." + gr.Group   // "deployments.apps", not "apps/deployments"
}
```

So `{apps, deployments}` → `deployments.apps`. Two differences from the proposal: **order** (resource-first)
and **separator** (`.`). The dot is also *ambiguous* — group names contain dots
(`roles.rbac.authorization.k8s.io`), so the dot form can't be split back into group/resource; the `/`
form can (and `/` never appears in a group or resource name). We keep `/`. A two-line shared helper keeps
write-side and read-side identical (they must never drift):

```go
func groupResourceKey(group, resource string) string {
    if group == "" {
        return resource
    }
    return group + "/" + resource
}
```

Group/resource and a UUID never contain `:` or `/`, so the per-field escaping
([`joinKeyFields`](../../internal/queue/attribution_index.go#L524)) that v2 needed for RBAC names like
`system:node-proxier` in the *name* field is no longer load-bearing for attribution (the name is a value
now). RVs are numeric in practice; treat them as opaque and reject/escape a stray delimiter defensively.

## 3. How the join works — the three lookups map cleanly to v2

| Lookup (resolver order) | v3 key | v2 equivalent | When it wins |
|---|---|---|---|
| exact | `…:object:<uid>:<rv>` | `e` | normal create/update — the watch event's post-write RV matches the fact. |
| uid latest | `…:object:<uid>:last` | `u` | known RV-mismatch events: deletes and deletecollection. |
| rv-only | `…:rv:<rv>` | `r` | a fact that has an RV but no UID (§5) — the escape hatch. |

The resolver keeps the same join power as v2 but no longer uses the same unconditional fallback chain:
exact-capable events stop at the exact key, while known RV-mismatch events may use `:last` (§4.2).
CommitRequest authorship leaves this path entirely and uses `author:v1:command` from admission (§9).

## 4. The win: `:<rv>` is immutable, so conflict-marking retires

A given `(uid, rv)` had **exactly one writer** — that RV exists *because* of that single write. So
`object:<uid>:<rv>` is **written once and never contended**. That dissolves the machinery v2 needed:

- **v2:** the shared `u` (uid-only) key is written by every author of the object, so a second, different
  author collapses it to a `{conflict:true}` marker ([`storeFactKey`](../../internal/queue/attribution_index.go#L457)),
  and only the `e` (exact) key rescues precise per-write credit.
- **v3:** each write owns its own immutable `:<rv>` key, so there is **nothing to conflict**. The
  resolver rule becomes **"exact for exact-capable events; `:last` only for known RV-mismatch events."**

### 4.1 `:last` is last-writer-wins, and that is correct here

`:last` is overwritten with the newest author on every write. It is consulted **only** when the exact
`:<rv>` misses — i.e. RV-mismatch events:

- A **delete** writes `:last` = the deleter; the `DELETED` watch event (RV ≠ any write RV) resolves the
  deleter. Correct.
- A **deletecollection** expands one `:last` per member = the actor; each per-object removal joins it
  (see [deletecollection §3](../spec/deletecollection-attribution-expander.md)). The whole reason that design
  pinned itself to the uid-only variant — body RV ≠ removal RV — is just "use `:last`" here.
- A **burst** (author₁ rv₁, author₂ rv₂): `:<rv1>`=author₁, `:<rv2>`=author₂ (both precise), `:last`=author₂.
  Watch events for rv₁ and rv₂ each hit their exact key → both precise. This is the precision an earlier
  single-key sketch gave up; the grouped scheme keeps it.

### 4.2 Safety rule — exact-capable events do not fall through to `:last`

No tombstone. The fundamental correctness rule is simpler and stronger:

- If a watch event has a UID **and** RV and is an exact-capable event (`ADDED` / `MODIFIED`), lookup only
  `object:<uid>:<rv>`; if it misses after the attribution grace, return `absent` and commit as the
  committer.
- Use `:last` only for known RV-mismatch events (`DELETED`, deletecollection-expanded removals, and any
  future path that deliberately records author intent without a matching post-write RV).
- Use `rv:<rv>` only when the audit fact had RV but no UID; a normal UID-bearing watch event tries exact
  first and never needs the rv-only key.

This removes the whole "was there once a fact here?" tombstone question. It also means `expired` goes away
as a resolver outcome: after the 15 minute audit fact TTL (§10), a miss is just `absent`.

## 5. The `rv:` escape hatch — type-scoped, shipped from the start

A `resourceVersion` is **opaque** per Kubernetes' API contract: you may not assume it is comparable or
unique across resource types, and aggregated/CRD API servers can mint their own. So an rv-only key
**must** include the type — `author:v1:audit:apps/deployments:rv:101`, never a bare
`author:v1:audit:rv:101`. The proposal gets this right.

`rv:<rv>` is the join path for a fact that has an RV but **no** UID — a hollow / metadata-only body *and*
an `objectRef` without a UID. Under the `RequestResponse` audit level this project relies on,
`IdentityFromAuditEvent` ([`identity.go`](../../internal/auditutil/identity.go)) backfills the UID from
the body even for `generateName` creates, so a no-UID fact is uncommon — but when it happens, `rv:`
is the difference between a precise author and a committer fallback. It is **cheap and shipped now**: one
extra key, written *only* when the fact has no UID (a UID-bearing fact's `rv:` key would be dead — the
watch side always carries a UID, so it would resolve via `object:<uid>:…` first and never reach it). This
is a deliberate improvement over v2, which wrote a dead rv-only key for *every* event.

The `object:`/`rv:` discriminator is what keeps the two cohabiting unambiguously: without it, `…:<uid>`
and `…:<rv>` would collide in the same key position, distinguishable only because a UUID happens to look
unlike a number (fragile; don't rely on it). With the discriminator, the rv-only path is self-describing
and can never be mistaken for an object key.

## 6. Reason codes simplify

Because `:<rv>` is a real exact match again, the v2 reason tiers keep their meaning — unlike a single-key
collapse, which would have erased `exact` vs `weak`:

| Reason | Source |
|---|---|
| `exact_user` / `exact_serviceaccount` | `object:<uid>:<rv>` matched, `isServiceAccount` flag |
| `weak` | `object:<uid>:last` or `rv:<rv>` matched (known RV-mismatch or rv-only fallback) |
| `exact_deletecollection_item` | matched fact with `verb == deletecollection` (read from the **value**, not a key variant) |
| `absent` | no usable author fact matched before the grace elapsed |

`conflict` **disappears** as a resolver outcome (§4 — there is nothing to conflict). `expired` also
disappears because `:seen` tombstones are gone (§4.2). Net dashboard change: drop the `conflict` and
`expired` series; everything else on `AttributionResolutionsTotal{result=…}` is stable.

## 7. deletecollection stops being a special case at the key layer

[`RecordDeleteCollectionFacts`](../../internal/queue/attribution_index.go#L205) exists to write "only the
uid-only variant" because the body RV is dead. In v3 that is simply "write `object:<uid>:last`" — the
same key any RV-mismatch event uses. The only deletecollection-specific thing left is the **reason code**,
now driven by `fact.verb` in the value, not by which key variant matched.

## 8. The GitTarget watch store

Applying "the same line of thinking" honestly: the object-grouping core **does not transfer** — the
cursor is not keyed by an object UID. But `watch-cursor` is still too much ceremony. The durable thing is
a watch shard's **last processed RV**, so the family should say that directly:

```
v2: gitops-reverser:watch-cursor:v2:<gitTargetUID>:<group>:<version>:<resource>:<ns>
v1: gitops-reverser:watch:v1:target:<gitTargetUID>:<group/resource>:<scope>:last-rv
```

- **`watch:v1`** is the durable namespace. This is not an author record, so it should not live under
  `author`, and `cursor` does not need to be in the family name.
- **`target:<gitTargetUID>`** keeps the recreated-GitTarget safety property. The UID is the identity; the
  namespace/name are only labels.
- **`<group/resource>`** uses the same helper as §2.1. Drop the version: a `resourceVersion` is a
  per-resource counter shared across served versions of that resource, so the version is redundant in a
  resume-cursor key. If one GitTarget ever watched a resource at two versions, their cursors would merge;
  that is harmless because the RV space is shared. In practice each shard watches one version.
- **`<scope>` stays, without a `scope:` label.** This is where I would push back on the shorter
  `gitops-reverser:watch:v1:uid:<group/resource>:last-rv` form: the live data plane opens one raw watch
  per `(GitTarget, GVR, namespace scope)`, not just per `(GitTarget, type)`. A namespaced `WatchRule` and
  a cluster-wide `ClusterWatchRule` are different watch collections, and two named namespaces can also be
  separate watch shards. Sharing one RV key across those shards risks resuming the wrong collection.
- **`:last-rv`** names the leaf. The value is just the resourceVersion string.

`scope` encoding:

| Watch scope | Key segment |
|---|---|
| Cluster-wide watch, including cluster-scoped resources and ClusterWatchRule over a namespaced resource | `cluster` |
| One namespace-scoped watch | `namespace:<namespace>` |

Examples:

```
gitops-reverser:watch:v1:target:gtuid-3:apps/deployments:namespace:team-a:last-rv
gitops-reverser:watch:v1:target:gtuid-3:configmaps:cluster:last-rv
```

The GitTarget UID stays — it is what stops a recreated GitTarget inheriting a dead predecessor's cursor
([`types/reference.go`](../../internal/types/reference.go#L26)).

## 9. The command-author store

The CommitRequest admission plan should use the same top-level author namespace, but it should **not**
reuse the audit subfamily. It has different provenance, different lookup semantics, and no wait:

```
gitops-reverser:author:v1:command:<uid>
```

- **`author:v1`** says what the record is: a commit-author candidate.
- **`command`** says which invariant applies: admission captured the command submitter before the
  object became visible, and the controller only uses it after the object persisted.
- **`<uid>`** is enough. Command objects are read back by persisted object UID; namespace/name/kind are
  value/debug fields at most.
- **No `:seen` / `:miss`.** There is no asynchronous audit arrival and no grace-window join. A miss is
  present-or-never and immediately falls back to the committer.
- **No RV, auditID, or conflict marker.** Those belong to audit-sourced resource joins, not command
  authorship.

This replaces the companion sketch's `gitops-reverser:command-author:v1:<uid>` before it lands. Keeping
`command-author` as a separate top-level family is workable, but less clear once resource attribution has
already moved under `author`.

## 10. Migration & rollout — no data migration

All families here are **ephemeral**: audit author facts ≤ 15 min, cursors ≤ 1 h, command authors ≤ 1 h.
A version-prefix bump needs **no dual-read and no backfill** — old keys TTL out while new pods write/read
the new families.

| Family | Rollout effect of the bump | Self-heals by |
|---|---|---|
| Audit author facts | Facts written by old `attr:v2` pods aren't read by new `author:v1:audit` readers during the overlap → those events ship as **committer** for the rollout window. | Next writes land in `author:v1:audit`; gap ≤ the 15 minute fact TTL. |
| Watch cursor | A new pod reads `watch:v1` (miss) and **rebuilds each shard from a fresh replay** — one cold resync per shard, the same cost as any pod cold-start. | First successful `watch:v1` cursor write; one replay. |
| Command author | No live key exists yet if the admission plan has not landed. If a prototype used `command-author:v1`, let it TTL out and read only `author:v1:command`. | Next command create. |

The cursor consequence is heavier: **bumping the cursor version forces a one-time full resync** per shard
on the upgrading pod (≈ a normal restart's cost, since watch-first replays on restart anyway). So bump
author facts and cursor together only if a resync is already expected; otherwise bump them independently.

## 11. Code changes (sketch)

- **`attribution_index.go`**
  - Rename `attributionKeySuffix` from `:attr:v2:` to `:author:v1:audit:`.
  - Raise `DefaultAttributionFactTTL` from 10 minutes to 15 minutes.
  - Replace `factKeyVariants`/`factKey` with three small builders: `factKeyExact(gr, uid, rv)`,
    `factKeyLast(gr, uid)`, `factKeyRV(gr, rv)`, all over the `groupResourceKey` helper.
  - `RecordFact`: write `…:<rv>` (immutable `SET NX`-ish — last write for an RV is identical anyway) **and**
    overwrite `…:last`; **also** write `rv:<rv>` when `uid == ""` (the §5 escape hatch — shipped, just
    skipped for UID-bearing facts whose `rv:` key would be dead). No conflict read/merge.
  - `AuthorFact` gains `GroupResource`, `Namespace`, `Name`, `UID`; **drop `Conflict`**.
  - `storeFactKey`'s conflict-detection block is **deleted** (§4).
  - Delete `factTombstoneSuffix`, `factTombstoneTTLMultiplier`, `factTombstoneKey`, `factMissSuffix`,
    `factMissKey`, and `RecordAuthorMiss`; no sibling keys.
  - Resolver: exact-capable events try `:<rv>` only; known RV-mismatch events use `:last`; no tombstone
    branch, no `expired` result, and no `op="late"` metric from Redis miss markers.
  - `attributionResultForMatch` derives the reason from the matched key kind + `fact` (§6); remove
    `AttributionConflict` and `AttributionExpired`; keep `AttributionExactDeleteCollectionItem` (now keyed
    off `fact.Verb`).
  - `RecordDeleteCollectionFacts`/expander: write one `…:last` per member (§7).
  - `recordFactIndexSize` scans `author:v1:audit:*`.
- **`redis_store.go`** — `watchCursorKeySuffix` → `:watch:v1:`; `watchCursorKey` emits
  `target:<uid>:<group/resource>:<scope>:last-rv` (drop `gvr.Version`). `CursorStore` interface
  signatures unchanged.
- **`command_author_store.go`** — add the CommitRequest plan's store with
  `commandAuthorKeySuffix = ":author:v1:command:"`, not `:command-author:v1:`.
- **`author_resolver.go` / `watch`** — drop the `conflict` result label; otherwise no logic change.
- **Tests** — key-shape assertions in `attribution_index_test.go`; burst → both-precise; delete → `:last`;
  no-uid fact → `rv:` written and joined (no dead `rv:` for a UID-bearing fact); deletecollection
  join-shape; `watch:v1` cursor shape; command-author key shape and miss-is-immediate behavior.

## 12. Consequences at a glance

| Change | Win | Cost / risk |
|---|---|---|
| `author` top-level namespace | Names what Redis stores; avoids `attr` ambiguity | Slightly longer keys |
| `audit` and `command` subfamilies | Keeps provenance and invariants legible | Callers must choose the right builder |
| Object-grouped keys (`object:<uid>:…`) | Self-describing, `SCAN`-per-object, ns/name off the key | Same ~3 keys/write as v2 (no key-count reduction — readability over count) |
| `:<rv>` immutable + `:last` LWW | Retires conflict-marking; burst stays precise; `:last` only for known RV-mismatch events | Requires event-kind-aware resolver branching (§4.2) |
| `rv:<rv>` type-scoped, shipped | Closes the no-UID-fact gap precisely; one extra key only when needed | Needs the `object:`/`rv:` discriminator (already in the scheme) |
| Reason codes simplified | `conflict` and `expired` disappear | Dashboards drop both series |
| deletecollection un-specialed at key layer | Less special-casing | None (reason via `fact.verb`) |
| Watch: `watch:v1:target:...:last-rv` | Names the store and leaf directly; version redundancy gone | One cold resync per shard on the bump (§10) |
| Command author under `author:v1:command` | CommitRequest plan fits the same naming system | None if changed before code lands |
| Version bump | No migration code | Brief committer/replay window during rollout (§10) |

## 13. Recommendation

Adopt `author` as the top-level domain. Use `audit` below it for mirrored-resource facts:

```
gitops-reverser:author:v1:audit:<group/resource>:object:<uid>:<rv>    # exact, immutable
gitops-reverser:author:v1:audit:<group/resource>:object:<uid>:last    # latest, LWW
gitops-reverser:author:v1:audit:<group/resource>:rv:<rv>              # rv-only, type-scoped
gitops-reverser:author:v1:command:<uid>                               # command submitter
gitops-reverser:watch:v1:target:<uid>:<group/resource>:<scope>:last-rv
```

For audit facts: value-carried name/namespace, **conflict-marking retired** in favor of immutable
`:<rv>` + LWW `:last`, no tombstones, a 15 minute audit fact TTL, and an event-kind-aware resolver:
exact-capable events do not fall through to `:last` (§4.2). Reason codes are preserved minus `conflict`
and `expired` (§6). All three audit keys ship together; the `rv:` escape hatch is written only for a
no-UID fact (§5). For command authorship: keep the separate `command` subfamily from the first
implementation, with no audit fields and no wait (§9). Move watch cursors to the simpler
`watch:v1:...:last-rv` shape, keeping the namespace scope because the live watch shard includes it (§8).
Bump author facts and watch cursors in the same release only if a resync is already expected.

### Definition of done (when implemented)

- Audit author keys are under `:author:v1:audit:` as `…:object:<uid>:<rv>`,
  `…:object:<uid>:last`, and `…:rv:<rv>`, with no `:seen` or `:miss` siblings. `AuthorFact` carries
  group-resource/namespace/name/uid and **no `Conflict`**.
- Command author keys are under `:author:v1:command:<uid>`; records carry only command author fields and
  a cleanup TTL.
- Audit fact TTL is 15 minutes.
- Conflict-marking removed; resolver is event-kind-aware: exact-capable events try exact only; known
  RV-mismatch events may use `:last`; `rv:` keys are type-scoped and written only for no-UID facts (§5).
- Reason set: `exact_user` / `exact_serviceaccount` / `weak` / `exact_deletecollection_item` / `absent`;
  `conflict` and `expired` dropped; dashboards updated.
- Watch-cursor key is `:watch:v1:target:<uid>:<group/resource>:<scope>:last-rv`; version dropped,
  namespace scope retained.
- Unit tests assert the v3 key shapes and the burst→both-precise, delete→`:last`, and no-UID→`rv:`
  behaviours; the deletecollection join-shape test passes against `:last`.
- Full validation per AGENTS.md once code lands: `task fmt → generate → manifests → vet → lint → test →
  test-e2e` (e2e sequential).
