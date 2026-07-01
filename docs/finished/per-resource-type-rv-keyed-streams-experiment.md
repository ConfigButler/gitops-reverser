# Per-resource-type keyspace in Redis

> Status: **implemented (write-only)** — write-first. This is the *real* per-type
> data structure we intend to keep; the write path landed, no consumer / read path
> yet. Two sinks now share one per-type key root (`gitops-reverser:<group>:<resource>`):
> the **audit stream** (`:audit:stream`) and the **current-objects snapshot**
> (`:objects:items`/`:objects:rv`/`:objects:state`).
> Implementation: `internal/queue/redis_bytype_queue.go` (audit sink, tapped in
> `internal/webhook/audit_handler.go`'s `mirrorByType` /`enqueueCanonicalEvent`);
> `internal/queue/redis_objects_snapshot.go` (objects sink) driven from
> `internal/watch/type_objects_mirror.go` on the `TypeActivated`/`TypeRemoved`
> lifecycle edges; both wired always-on in `cmd/main.go`.
> **First e2e observations measured 2026-06-10 — see §12.** Headline correction: the
> late lane is *not* empty — it carries ~3–4% of events and is the §7 INV-1 signal
> firing exactly as designed (it is *why* the main stream stays RV-ordered). Confirmed on
> a **clean cluster** (§12.0) — genuine within-run reordering, not cross-run contamination.
> Captured: 2026-06-09
> Updated: 2026-06-09
> Owner: Simon
> Related:
> [../watch-and-catalog-architecture.md](../design/watch-and-catalog-architecture.md),
> [../audit-ingestion-decision-record.md](../design/audit-ingestion-decision-record.md),
> [../best-practices-webhook-ingress.md](../design/best-practices-webhook-ingress.md),
> [../manifest/reconcile-via-watchlist-mark-and-sweep.md](../design/manifest/reconcile-via-watchlist-mark-and-sweep.md)

## 0. Key layout

Every key is `gitops-reverser:<group-or-core>:<resource>:…`, colon-separated, so a
type's audit history and its current objects sit under one shared base key. The
core API group renders as `core`; a subresource is folded onto the resource
segment with a dot (`deployments.scale`); any colon inside a name is scrubbed.

```text
# audit — the event history (stream + late lane + idstate implemented; pending:rv reserved)
gitops-reverser:<group>:<resource>:audit:stream        # XADD per canonical event   ← implemented
gitops-reverser:<group>:<resource>:audit:late          # diagnostic late lane        ← implemented
gitops-reverser:<group>:<resource>:audit:idstate       # high-water + counters       ← implemented
gitops-reverser:<group>:<resource>:audit:pending:rv    # (reserved)

# objects — the current cluster state, loaded when the type starts being watched
gitops-reverser:<group>:<resource>:objects:items       # HASH identity → object JSON ← implemented
gitops-reverser:<group>:<resource>:objects:rv          # list resourceVersion        ← implemented
gitops-reverser:<group>:<resource>:objects:state       # {phase,count,rv,updated_at} ← implemented

# enumeration
gitops-reverser:__index__                              # SET of per-type base keys
```

Examples: `gitops-reverser:apps:deployments:audit:stream`,
`gitops-reverser:core:configmaps:objects:items`,
`gitops-reverser:gitops.koudijs.dev:gittargets:objects:items`,
`gitops-reverser:apiextensions.k8s.io:customresourcedefinitions:objects:items`.

## 1. One sentence

Tap `enqueueCanonicalEvent` — the point where the fully-merged, body-complete
`StageResponseComplete` event is written to the canonical stream — and mirror it
into **one Redis stream per resource type**, as a **single entry** carrying the
compact **overview** fields plus the full event JSON in a `payload_json` field,
ordered **millisecond-first**, and *just write* for now: no consumer yet.

## 2. Goal — what we want to learn

We are not reading the data back yet. Writing it first lets us observe, after
the fact, things we currently only guess at:

- **Volume shape per type.** How lopsided is event traffic across resource
  types? Which types dominate the firehose?
- **Ordering reality.** Does the millisecond-first stream ID produce a sane
  per-type history, and how often does event time disagree with arrival order?
- **RV availability.** How often is a usable ResourceVersion *present* on an
  incoming event vs. missing (deletes, collection verbs, sub-resources, shallow
  bodies)?
- **Duplication.** These are already joiner-deduped canonical events, so per
  `audit_id` duplication should be ~0 — worth confirming it actually is.
- **Key cardinality.** How many distinct type streams appear over an hour / a
  day on a real cluster?

The deliverable for now is **the populated Redis keyspace** plus the inspection
notes below. The streams themselves are the structure we plan to consume later.

## 3. Non-goals (for this first pass)

- No consumer, no read path, no replay, no Git writes yet.
- No change to the existing canonical or debug streams, or to the join
  pipeline. This sits *beside* them.
- Not wired into WatchRules — every type is captured; relevance is a later,
  consumer-side concern.

## 4. Where this plugs in — `enqueueCanonicalEvent`

Current ingress path, in
[internal/webhook/audit_handler.go](../../internal/webhook/audit_handler.go):

1. API server (official) and the additional-source proxy POST an
   `auditv1.EventList` to `/audit-webhook` / `/audit-webhook-additional`.
2. `decodeEventList` parses the list; `enqueueDebugEvents` mirrors every raw
   event to the debug stream (unchanged).
3. `processEvent` applies the gates and — importantly — **early-returns on any
   stage other than `StageResponseComplete`**
   ([audit_handler.go:294](../../internal/webhook/audit_handler.go#L294)),
   then runs the join pipeline (`eventForCanonicalStream`), which merges the
   additional-source **body** into the event.
4. `enqueueCanonicalEvent` writes that fully-merged event to the canonical
   stream.

The per-type push lives **inside `enqueueCanonicalEvent`**, right after the
canonical `Queue.Enqueue` succeeds. This is the right tap for two reasons:

- The `*event` it receives is the **post-join, body-complete** event — the work
  we wanted to happen before mirroring has already happened.
- It is only ever reached for **`StageResponseComplete`** (every earlier stage
  was dropped by the early-return upstream). We *want* only ResponseComplete
  here: it is the single canonical, deduped, body-bearing event per change,
  whereas the earlier stages are partial and duplicative. This supersedes the
  earlier "keep all stages" idea.

```
EventList POST ─▶ decode ─▶ enqueueDebugEvents              (existing)
                            processEvent
                              ├─ drop unless StageResponseComplete
                              ├─ gates / quality / join + body merge
                              └─ enqueueCanonicalEvent
                                    ├─ Queue.Enqueue → canonical stream (existing)
                                    └─ push to per-type streams ◀── NEW (this doc)
```

This sink is **always active** — no enable flag, no separate DB. Keep it simple,
and keep it best-effort (see §7) so it can never fail the canonical write.

## 5. The fields we extract per event

### 5.1 Resource type → which stream

Derive from `event.ObjectRef` via the existing `gvrParts` helper (group /
version / resource). Stream identity: **`group` + `resource`
(+ subresource suffix)** — version-agnostic, so all versions of a kind share one
ordered history (RV is an etcd-global revision, not per-apiVersion).

- Core group → empty group string; render as `core`.
- Sub-resources (`deployments/scale`, `pods/status`) → suffix the stream name,
  e.g. `apps.deployments.scale`.
- Sanitize for a Redis key: lowercase, `/`→`.`, strip anything outside
  `[a-z0-9._-]`.
- Events with no `ObjectRef` / no resource → a single `__unknown__` stream
  (worth seeing how big it gets).

### 5.2 Millisecond value → leads the stream ID

> **Superseded.** The shipped ingestion re-keys the main stream to `<resourceVersion>-<subseq>`
> (RV-first), not millisecond-first, and routes strictly-older events to `:audit:late` instead
> of falling back to a looser ID. The millisecond is kept as the `stage_millis` field only. See
> `audit-log-ingestion-and-ordering.md` §5/§9 for the
> current design; the rest of this subsection records the original experiment.

`event.StageTimestamp` is a `metav1.MicroTime`; use
`event.StageTimestamp.Time.UnixMilli()`. This is "the millisecond value first":
the event's own millisecond is the leading ID component.

**RV-in-ID experiment.** A Redis stream ID is `<ms>-<seq>`, both 64-bit. Instead of
letting Redis auto-assign the sequence (`*`), we **fold the event's resourceVersion
into the sequence component**: the ID becomes **`<stageMillis>-<rv>`**, so the ID
itself encodes `(event-time, RV)`. Within a millisecond, entries then order by RV —
the etcd commit order — rather than by arrival. RV fits a stream sequence (it is an
int64 etcd revision).

Stream IDs must strictly increase, so we try candidates in increasing looseness and
fall back on the "equal or smaller" rejection (`streamIDCandidates` in
[internal/queue/redis_bytype_queue.go](../../internal/queue/redis_bytype_queue.go)):

1. `<stageMillis>-<rv>` — the experiment (skipped when RV is absent/non-numeric —
   deletes, collection verbs, shallow bodies, §5.3).
2. `<stageMillis>-*` — auto sequence within the millisecond. Covers two events sharing
   an `(ms, rv)` (close `deletecollection`s repeat a collection RV — see §9) and the
   no-RV case.
3. `*` — fully server-assigned, for a genuinely **older** millisecond arriving late.

Count how often we fall past candidate 1; together with `stage_millis` /
`resource_version` (kept as fields for the true order) it directly answers the
"ordering reality" and "RV availability" questions in §2.

### 5.3 ResourceVersion → a field, when available

RV is **not always present**, so it is **not** part of the key — it is just a
field. When present it comes from the object body's `metadata.resourceVersion`
(mirroring [internal/git/content_writer.go](../../internal/git/content_writer.go)'s
`event.Object.GetResourceVersion()`), because `ObjectRef.ResourceVersion` is
usually empty on writes (it is the *precondition* RV).

Extraction order, stored as the `resource_version` field (empty if none):

1. `event.ResponseObject.Raw` → `metadata.resourceVersion`.
2. else `event.RequestObject.Raw` → `metadata.resourceVersion`.
3. else `event.ObjectRef.ResourceVersion`.
4. else `""` (and the field is simply absent/empty — we'll measure how common).

## 6. One stream per type: overview fields + payload field

For each event we write **one** entry into the per-type audit stream, using the
stream-ID strategy from §5.2:

- **Audit stream** — the compact overview fields *and* the full event JSON, on the
  same row: `gitops-reverser:<group-or-core>:<resource>[.<subresource>]:audit:stream`

The overview fields still make the stream scannable with `XRANGE` (the big blob is
one field you can ignore when skimming), and the payload rides along on the same
entry — no second stream, no `audit_id` join, no chance of the overview and its
body diverging or trimming at different rates.

> Earlier this was two streams (an overview stream and a `.payload` sibling
> correlated by `audit_id`). Collapsed to one entry: the payload is now just the
> `payload_json` field below.

### 6.1 Entry fields

| field              | source                                              |
|--------------------|-----------------------------------------------------|
| `audit_id`         | `event.AuditID`                                     |
| `stage`            | `event.Stage` — always `ResponseComplete` here (§4) |
| `verb`             | `event.Verb`                                        |
| `api_group`        | `gvrParts` group                                    |
| `api_version`      | `gvrParts` version                                  |
| `resource`         | `gvrParts` resource                                 |
| `subresource`      | `ObjectRef.Subresource`                             |
| `namespace`        | `ObjectRef.Namespace`                               |
| `name`             | `ObjectRef.Name`                                    |
| `resource_version` | §5.3 (empty when unavailable)                       |
| `stage_millis`     | `StageTimestamp.UnixMilli()` (also the ID prefix)   |
| `user`             | `event.User.Username`                               |
| `payload_json`     | `json.Marshal(event)` — the full merged event       |

The webhook source (official / additional) is intentionally **not** a field: by
the time we mirror, the joiner has merged the additional-source body into the
single canonical event, so "source" no longer identifies a distinct record.

### 6.2 Concrete write

```
XADD gitops-reverser:apps:deployments:audit:stream <stageMillis>-<rv> \
     audit_id <uid> stage ResponseComplete verb update \
     api_group apps api_version v1 resource deployments subresource "" \
     namespace prod name web resource_version 184467 \
     stage_millis 1749470400123 user system:... payload_json {...}
```

The type's **base** key is registered in a set so the keyspace can be enumerated
later without `SCAN` (shared with the objects snapshot, §11):

```
SADD gitops-reverser:__index__ gitops-reverser:apps:deployments
```

## 7. Config & safety

- **Always on.** No enable flag. Reuse the existing Redis connection
  (`RedisAuditQueueConfig`: addr, auth, DB, TLS) wired in
  [cmd/main.go](../../cmd/main.go).
- **Bounded growth.** Streams support approximate trimming — set `MaxLen` with
  `Approx` per stream, exactly like the existing canonical/debug streams, so a
  busy type can't grow Valkey memory without limit. Reuse the same MaxLen knob.
- **Best-effort, after the canonical write.** Mirror only *after*
  `Queue.Enqueue` succeeds, so the per-type streams reflect exactly what reached
  the canonical stream. Swallow-and-count errors here; this sink must never fail
  an audit request, release a join decision, or perturb the canonical/debug
  paths. Log the first error per stream, then a counter metric.

## 8. How we'll inspect it (later, manually)

All read-only, ad hoc (`<base>` = `gitops-reverser:<group>:<resource>`):

- `SMEMBERS gitops-reverser:__index__` → the set of per-type base keys.
- `XLEN <base>:audit:stream` per type → volume distribution (§2 "shape").
- `HLEN <base>:objects:items` / `GET <base>:objects:state` → current-object count
  and snapshot freshness per type (§11).
- `XRANGE <base>:audit:stream - +` → the per-type history, scan for places where
  `resource_version` or `stage_millis` move backwards vs. ID order → ordering /
  RV-gap anomalies. Skip the `payload_json` field to keep the scan readable.
- Entries with empty `resource_version` → RV-availability rate.
- Entry count vs. distinct `audit_id` → duplication / multi-stage rate.
- The `payload_json` field on any row → the full body, already on the same entry.

## 9. Open questions

1. **Stream ID strategy** — now `<stageMillis>-<rv>` (event-time-first, RV folded
   into the sequence; §5.2), falling back to `<stageMillis>-*` then `*`. Measure the
   fall-past-candidate-1 rate: a high rate (e.g. many same-`(ms, rv)`
   `deletecollection`s, or many RV-less events) tells us how often the in-ID RV is
   actually usable vs. when the `resource_version` field is the only carrier.
2. **Stream granularity** — `group.resource` (proposed) vs. include version vs.
   include namespace. Namespace-in-key explodes cardinality; keep it in fields.
3. **Trim size** — per-type `MaxLen`; pick a starting value once we see volumes.

## 10. Evolution / teardown

Because this *is* the intended real structure, "teardown" mostly means changing
the write rules, not deleting a throwaway. If we abandon it: remove the sink
wiring (audit tap in the handler, objects mirror on the lifecycle edges, both in
`cmd`) and `DEL` the `gitops-reverser:*` keyspace. No downstream coupling exists
until a consumer is built.

## 11. Current-objects snapshot (`:objects:*`)

The audit stream is *history*; the objects snapshot is *current state*. When a type
becomes followable and **settles** (the `TypeActivated` lifecycle edge in
[internal/typeset/lifecycle.go](../../internal/typeset/lifecycle.go)), we list
its objects **once** and write them under the same base key. This is loaded at the
moment we start watching a type — not per GitTarget — so we do not re-list on every
GitTarget change. `TypeRemoved` clears the snapshot (leaving a `removed` tombstone).

- **Where.** [internal/watch/type_objects_mirror.go](../../internal/watch/type_objects_mirror.go)
  (`mirrorTypeObjects`), called from the Manager's lifecycle drain goroutine, so a
  large list never blocks the registry updater. The sink is
  [internal/queue/redis_objects_snapshot.go](../../internal/queue/redis_objects_snapshot.go)
  (`RedisObjectsSnapshot`), an optional `Manager.ObjectMirror` (nil disables it).
- **Why activation, not every rule change.** `TypeActivated` fires off the full
  catalog scan for every served, stable type — so the snapshot is keyed to the type,
  not to any GitTarget. The same revision-pinned current state then serves every
  GitTarget that watches the type. This is the cluster-state half the
  [mark-and-sweep reconcile](../design/manifest/reconcile-via-watchlist-mark-and-sweep.md)
  needs, computed once per type instead of per target.

### 11.1 Keys

| key                  | type   | contents                                                    |
|----------------------|--------|-------------------------------------------------------------|
| `<base>:objects:items` | HASH   | field `<namespace>/<name>` (cluster-scoped: `<name>`) → an **envelope** (§11.2) |
| `<base>:objects:rv`    | string | the LIST `resourceVersion` the items are pinned to          |
| `<base>:objects:state` | string | `{phase,count,resource_version,updated_at}` — `synced` or `removed` |

A HASH (not a stream) because this is *current* state keyed by identity: a re-list
**replaces** the set in one transaction, so a deleted object cannot linger.

### 11.2 The item envelope

Each item value is an envelope that **lifts the identity, resourceVersion, and
generation out of the body** and stores them beside the sanitized object, using the
same field names as the audit overview (§6.1) so the two structures are directly
joinable. This matters because `internal/sanitize` (the same pass the Git writer uses,
so the embedded `object` is directly comparable to a materialized manifest) **strips**
`uid`/`resourceVersion`/`generation` — without lifting them out, the rv would be
unreadable.

```json
{
  "api_group": "apps", "api_version": "v1", "resource": "deployments",
  "kind": "Deployment", "namespace": "prod", "name": "web",
  "uid": "…", "resource_version": "184467", "generation": 7,
  "object": { …sanitized Deployment… }
}
```

### 11.3 Concrete write

```
# one transaction per type at activation (full replace)
SADD gitops-reverser:__index__ gitops-reverser:apps:deployments
DEL  gitops-reverser:apps:deployments:objects:items
HSET gitops-reverser:apps:deployments:objects:items \
     prod/web {"name":"web","resource_version":"184467",…,"object":{…}} prod/api {…}
SET  gitops-reverser:apps:deployments:objects:rv 184467
SET  gitops-reverser:apps:deployments:objects:state {"phase":"synced","count":2,"resource_version":"184467","updated_at":"…"}
```

### 11.4 Caveats (this is a prototype)

- **Best-effort.** A nil mirror, a missing dynamic client, or a list/write error is
  logged and swallowed; the mirror never disturbs the watch/reconcile path.
- **Unbounded list.** A single `LIST` (no chunking) loads the whole type. Fine for a
  prototype; high-cardinality types (events, pods) are exactly what we want to *see*
  the size of before deciding on paging or a trim/exclude policy.
- **No live updates yet.** The snapshot is refreshed on activation/re-activation, not
  per watch event. Folding steady-state watch events into `:objects:items` is the
  natural next slice (and where `:audit:pending:rv` / `:audit:late` come in).

### 11.5 Proposed refinement — gate the LIST on the followability / disallow checks

> Status: **proposed, not implemented.**

Today [`mirrorTypeObjects`](../../internal/watch/type_objects_mirror.go#L59) lists
**every** object of an activated type and stores all of them. Activation is already
gated at the **type** level — `TypeActivated` only fires for a settled-Followable type
([internal/typeset/lifecycle.go](../../internal/typeset/lifecycle.go)) — so a
disallowed *type* is never listed. The refinement pushes the same decision **down to the
object**: include only the objects that pass the followability / disallow checks the
rest of the pipeline applies, so large objects on the disallow list are not
listed-and-stored for nothing.

- **Why.** The snapshot is the desired-state anchor a reconcile splices against
  ([reconcile doc §6](api-source-of-truth-reconcile.md)). Storing objects that will be
  filtered out downstream wastes Valkey memory and payload size on exactly the big
  objects we explicitly do **not** follow — pure overhead, no consumer benefit.
- **Where.** Filter `list.Items` through the per-object disallow predicate before
  building the envelope / `HSET`; the type-level gate stays where it is. It must be the
  **same** predicate the audit/reconcile path uses, so the snapshot and the log agree on
  what is in scope (typeset owns the *type* decision; the per-object disallow predicate
  is the new seam).
- **Open.** Whether the disallow is purely object-level (filter items) or can also skip
  the `LIST` **entirely** for a type none of whose objects could pass — saving the API
  call, not just the storage.

## 12. First e2e observations (2026-06-10)

Read-only inspection with `valkey-cli` against the e2e Valkey
(`valkey-e2e/valkey` in the k3d test cluster). This is the experiment's deliverable (§2):
measured, not guessed. Measured **twice** — see §12.0 — to rule out cross-run
contamination. The cluster is **bursty** (fixture create/delete plus k3s bootstrap), so
these numbers are closer to a worst case for ordering than a steady-state cluster.

### 12.0 Two runs — contamination ruled out

The first read came after several back-to-back `task test-e2e` runs against a Valkey that
was **not** flushed between them. That raised a fair question: are the late entries just a
*previous* run's high-water rejecting a *fresh* cluster's low RVs? So the experiment was
repeated on a **clean cluster** — namespace torn down, fresh etcd (RVs reset to ~1), empty
Valkey (new `run_id`, 7-min-old pod). The late lane **still filled, with the same profile**.
With no stale high-water possible, that is decisive: the late entries are **genuine
within-run out-of-order webhook delivery**, not contamination. The clean run is the
authoritative measurement below; the multi-run figures corroborate it.

| metric | clean single run | earlier multi-run |
|---|---:|---:|
| distinct type streams (`__index__`) | 98 | 98 |
| main-stream entries (Σ `XLEN :audit:stream`) | 1475 | 2497 |
| ↳ RV-bearing (idstate `mainCount`) | 1354 | 2290 |
| ↳ RV-less, attached to high-water (`rvMissingCount`) | 121 | 207 |
| objects snapshot items (Σ `HLEN :objects:items`) | 599 | 589 |
| late-lane entries (= idstate `lateCount`) | 49 | 112 |
| late ratio (late / all events) | **~3.2%** | ~4.3% |

The counters **reconcile exactly** both runs: `mainCount + rvMissingCount = ` main entries
(1354 + 121 = 1475), and Σ`lateCount` = late-lane length. The `idstate` cache is not
drifting from the streams it summarizes.

### 12.2 What held up

- **Main stream is strictly RV-ordered.** IDs are `<rv>-<subseq>`, not millisecond-first —
  the §5.2 re-key landed and holds.
- **Objects snapshot loads cleanly** per activated type (serviceaccounts, secrets,
  replicasets, services …), keyed to the type and version-agnostic.
- **RV availability is high** — ~92% of events carry a usable RV. The RV-less remainder
  (deletes / collection verbs) is placed by the declared policy (§5.3 / ingestion IR5),
  never crashing.
- **Zero `non-numeric-rv`** in both runs. Even the wardle aggregated apiserver
  (`wardle.example.com:flunders`) emitted numeric etcd RVs.

### 12.3 The late lane is NOT empty — and that is the point

Correcting the field impression that "there were no late streams": the clean run has **49**,
~**3.2%** of all events, **all `older-than-high-water`** (0 `rv-missing-before-high-water`,
0 `non-numeric-rv`). This is the late lane **working as designed** — diverting genuinely
out-of-order webhook delivery is exactly *why* the main stream stays RV-clean (ingestion
doc P1/P2). It is the §7 **INV-1** signal firing.

**RV-gap distribution** (`last_rv − resource_version`, clean run):

| stat | value | | bucket | count |
|---|---:|---|---|---:|
| min | 1 | | ≤2 (trivial skew) | 1 |
| median | 152 | | 3–10 | 4 |
| mean | 686 | | 11–100 | 19 |
| max | 2732 | | >100 | 26 |

The tail is **large and heavy** — ~52% of late events are >100 revisions behind, not
1–2-revision skew (the earlier multi-run agreed: median 168, max 4650, 63% >100).

### 12.4 What the late entries actually *are* (which resources, and why)

The clean-run late entries fall into two mechanisms, plus a tiny trivial-skew tail:

- **A — controller re-apply of old objects (large gaps, >1000).** `k3s.cattle.io:addons`
  (11 entries, `update`, gaps ~1785–2031: every k3s bootstrap addon —
  `local-storage`, `ccm`, `metrics-server-*`, `auth-*`, …), `core:services`
  (`kube-system/kubelet`, 3× the **same** body `rv=1438`, gaps 1557–2732), `apps:deployments`
  (`coredns`, `metrics-server`, gaps ~1500). These are old objects (low body RV) re-touched
  by their controllers and **delivered long after the type's high-water advanced**. No
  reorder window of sane size catches a 2000-revision-old re-apply.
- **B — burst writes on test fixtures (moderate gaps, 36–409).** `core:secrets` (19, mostly
  `patch` on the bi-directional fixtures — `git-creds-*`, `sops-age-key`, `bi-secret`, plus
  one `deletecollection`) and `bi-directional…icecreamorders` (6, `patch` on the alice/bob
  orders). The test rapid-fires patches across many objects of one type, so per-type delivery
  order scrambles vs. RV order.
- **Trivial skew (≤10, window-catchable):** only ~5 entries —
  `discovery…endpointslices` (gaps 9, 12), a CRD (5), `core:configmaps`/`coredns` (3), a
  helmchart CRD (1).

**Cross-cutting — these are no-op write amplification, and main already supersedes them.**
Many late entries are *repeated touches of an object whose body `resourceVersion` never
moved* — `kubelet` `rv=1438` ×3, `bob-order` `rv=2782` ×4, several secrets at the same RV ×2.
Because **etcd bumps `resourceVersion` on every real mutation**, several events at an identical
body RV mean **at most one was a real write; the rest are no-op patches** (a controller
re-applying identical content). Per-object check of every bi-directional `core:secrets` /
`…icecreamorders` late entry confirmed each was **`redundant`** — the main stream already held
the object at an **equal-or-higher** RV — so the splice consumer loses no freshness, and the
writer's no-op detection would drop them anyway. Event-time skew was 0–10s and delivery latency
~0.01s (the reorder is a tight concurrent burst, not slow delivery). Two consequences for the
pre-sorter, detailed in **ingestion `§8.2`**: a ≤30s window
*would* catch mechanism B but would suppress a signal that costs the consumer nothing; and
**you must never make a time field the sort key** — a no-op patch carries a *fresh* timestamp
with a *stale* body RV, so time-ordering would let stale state overwrite fresh (RV is the only
trustworthy order; time may bound the buffer, not order it).

### 12.5 Implication for the deferred pre-sorter (ingestion §7 / §8.2)

This data **argues against** building the pre-sorter for now. A bounded reorder window only
catches small gaps; here only ~10% of late events (gap ≤10) are window-catchable, while the
dominant mass is large-gap controller re-applies (mechanism A) a window of any sane size
would miss. Those are exactly what the **checkpoint backstop** ([reconcile doc
DEC-5](api-source-of-truth-reconcile.md)) is for: the next `LIST` already holds the object's
current state, so the late event never needs to reach main. Net: the late lane is
**correct, observable, and backstopped** — **no pre-sorter is justified by these runs.**
Re-measure on a steady-state cluster (and check the >1-pod delta, ingestion §8.1) before
revisiting.
