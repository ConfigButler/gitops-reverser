---
status: design
date: 2026-08-25
related:
  - reconcile-triggering.md
  - watch-and-catalog-architecture.md
---

# Data-plane triggering: from an event pipe to a dirty set

> **design** — open, not yet built. Index: [`../INDEX.md`](../INDEX.md)

[`reconcile-triggering.md`](./reconcile-triggering.md) asks how the **control
plane** wakes up. This document asks the same question one layer down, about the
**data plane**: the branch worker's event queue, what travels on it, and which of
those things should not be travelling on a queue at all.

It starts from a concrete production failure, separates the two jobs the queue is
doing, and proposes moving one of them to a level-triggered dirty set while
leaving the other exactly where it is.

---

## 1. What happened

CI run 32848418546 dropped **595 resync requests in 16 seconds**, and all 595
were for a *single* GitTarget whose namespace was being deleted:

```text
total dropped resyncs: 595
distinct GitTargets   : 1
    595  …test-manager-unsupported-folder/unsupported-folder-dest
```

A branch worker's `eventQueue` is a 100-slot FIFO shared by **every GitTarget on
one provider and branch**. One target saturated it; unrelated GitTargets on the
same branch then waited on resyncs that never entered the queue, and their
WatchRules never reached `Ready`. Five different e2e specs failed this way across
three attempts, which is why it read as flakiness rather than as one bug.

```mermaid
flowchart LR
    subgraph Deleted["GitTarget being deleted"]
        S1["target watch: stream A"]
        S2["target watch: stream B"]
        S3["target watch: stream …N"]
    end
    subgraph Healthy["Unrelated GitTargets"]
        H1["resync for target X"]
        H2["resync for target Y"]
    end
    S1 --> Q
    S2 --> Q
    S3 --> Q
    H1 -.->|"dropped"| Q
    H2 -.->|"dropped"| Q
    Q["eventQueue&nbsp;(100 slots)<br/>shared per provider + branch"]
    Q --> W["branch worker"]

    classDef bad stroke-dasharray: 4 3;
    class H1,H2 bad;
```

The mechanism was edge-triggered pathology. Each stream of the deleted target
looped `replay → enqueue resync → fail → reconnect → replay`, minting a **new
payload every 2 s backoff**, for a target that could never come back.

Two fixes shipped in [#312](https://github.com/ConfigButler/gitops-reverser/pull/312):

1. A deleted GitTarget is now **terminal** for its target watches (`errGitTargetGone`),
   so the loop stops instead of reconnecting. This removed the storm at source.
2. Queued resyncs **coalesce** by `(GitTarget, scope)`, so queue depth is bounded
   by the number of distinct scopes rather than by the request rate.

Fix 2 is the interesting one, because of what it implies.

---

## 2. The queue is doing two unrelated jobs

`WorkItem` carries three shapes. They do not have the same nature.

```mermaid
flowchart TD
    WI["WorkItem"] --> R["Request&nbsp;— WriteRequest"]
    WI --> A["Attach&nbsp;— CommitRequest"]
    WI --> RS["Resync&nbsp;— ResyncRequest"]

    R --> RE["<b>Edge-triggered</b><br/>an observation of history"]
    A --> AE["<b>Edge-triggered</b><br/>a control message"]
    RS --> RL["<b>Level-triggered</b><br/>a statement about current state"]

    RE --> RN["cannot be recomputed"]
    AE --> AN["cannot be recomputed"]
    RL --> RLN["can always be recomputed"]
```

### 2.1 A write is edge-triggered and irreducible

```go
type Event struct {
    Operation     string // CREATE / UPDATE / DELETE
    AuditStreamID string // "<rv>-<seq>" on the per-type audit stream
    …
}
```

An `Event` is an *observation of something that happened*. "alice deleted this
ConfigMap at stream position X" cannot be recomputed from cluster state: the
object is gone, and the author exists only in the audit fact stream. Commit
windows are keyed per `(author, GitTarget)`, and the coverage watermark `Hc`
decides historical-versus-live suppression by stream position.

Order matters, attribution matters, and both are destroyed by collapsing this to
"something changed." **This half must stay a log.**

### 2.2 A resync is level-triggered and recomputable

```go
type ResyncRequest struct {
    Desired  []manifestanalyzer.DesiredResource // gathered from the live cluster
    Scope    *ResyncScope
    …
}
```

`Desired` is a *statement about current state*, gathered by listing the live
cluster. Throwing one away loses nothing: the next gather reconstructs it, and
reconstructs it fresher. The cluster is the source of truth.

**This half does not need a queue.**

---

## 3. The coalescing map is already a dirty set

This is the tell. What #312 added is:

```go
pendingResyncs map[resyncKey]*ResyncRequest   // key: (GitTarget, GVR, namespace)
```

That is a dirty set, with a pre-gathered payload still bolted to each entry.
Depth is bounded by *state cardinality* (how many distinct scopes exist) rather
than by *event rate*, which is exactly the property a dirty set has and a queue
does not.

Having reached a dirty set with a payload, the question is whether the payload
earns its place.

---

## 4. Proposal: drop the payload, gather at execution

Mark `(GitTarget, scope)` dirty. Gather when the worker acts, not when the
trigger fires.

```mermaid
flowchart LR
    subgraph Today["Today — gather at enqueue"]
        T1["trigger"] --> T2["gather<br/>(list live cluster)"]
        T2 --> T3["enqueue payload"]
        T3 --> T4["FIFO"]
        T4 --> T5["worker applies"]
    end
    subgraph Proposed["Proposed — gather at execution"]
        P1["trigger"] --> P2["mark scope dirty"]
        P2 --> P3["dirty set"]
        P3 --> P4["worker takes scope"]
        P4 --> P5["gather<br/>(list live cluster)"]
        P5 --> P6["worker applies"]
    end
```

What this buys:

| | Today | Proposed |
|---|---|---|
| Memory per pending item | a full snapshot (can be MBs) | one key |
| Duplicate triggers | coalesced, with supersede bookkeeping | free: setting a bit twice is setting a bit |
| Data freshness | as of enqueue time | as of apply time |
| Storm behavior | 595 payloads, queue saturated | 595 sets of one bit |
| Code | coalescing map, ownership marker, `ErrResyncSuperseded`, supersede replies | mostly deleted |

The last row matters: this proposal **removes** most of what #312 added. That is
the good kind of change: the coalescing machinery exists to make an event pipe
behave like a dirty set, so a dirty set makes it redundant.

It is also the standard controller-runtime pattern, one layer down: dedup by key,
read state at reconcile time. The control plane already works this way. The data
plane does not.

---

## 5. What has to be answered first

Three things, in descending order of risk.

### 5.1 Ordering against live events (the real design work)

A resync currently occupies a FIFO slot, so it lands **before** the live events
buffered behind it. That position is load-bearing:

```mermaid
sequenceDiagram
    participant Watch as target watch
    participant Q as eventQueue
    participant W as branch worker

    Watch->>Q: resync (snapshot at rv 100)
    Watch->>Q: live event (rv 101)
    Watch->>Q: live event (rv 102)
    Note over Q,W: FIFO order means the snapshot is<br/>applied first, then the two edits on top
    Q->>W: resync
    Q->>W: rv 101
    Q->>W: rv 102
```

A dirty bit has no position. The relationship between "this scope is dirty" and
"these attributed events are in flight for that scope" has to be defined
explicitly, and it interacts with the coverage watermark `Hc` that suppresses
historical entries. **This is where a wrong answer loses managed documents
rather than merely wasting work**, so it needs settling before any code moves.

The likely shape: a scope's dirty bit is consumed at a point where its in-flight
events are known, and the gather's revision is reconciled against `Hc` the same
way it is today. That must be written down and tested, not assumed.

### 5.2 Where the gather lives

The watch layer holds the informers; the branch worker holds the git checkout.
Gather-at-execution either moves cluster reads into the worker, or keeps the pipe
and moves only the **trigger** to a dirty set, with the watch layer gathering on
demand. The second is much smaller and is the recommended first step.

### 5.3 The scope invariant

`ResyncScope` carries the invariant that the sweep scope must be exactly the
scope the desired set was gathered over: a narrower desired set than its sweep
scope **deletes managed documents**. Today that is enforced by gathering and
sweeping together.

Gathering at execution keeps them together, so the invariant is preserved and
arguably harder to violate: there is no window in which a stale `Desired` can be
paired with a fresh scope.

---

## 6. One level up: invalidation granularity

The same argument applies to the config surface. A small change to one WatchRule
among many should not imply a full resync of its GitTarget.

```mermaid
flowchart TD
    CP["ClusterProvider"] --> GT["GitTarget"]
    GP["GitProvider"] --> GT
    WR1["WatchRule A<br/>(configmaps, ns=team-a)"] --> GT
    WR2["WatchRule B<br/>(secrets, ns=team-b)"] --> GT
    WR3["WatchRule C<br/>(deployments, ns=team-c)"] --> GT

    WR2 -.->|"edit"| D["dirty:<br/>(GitTarget, secrets, team-b)"]
    D --> N["other scopes untouched"]
```

An event pipe quietly encourages "resync everything, it is easier." A dirty set
keyed by `(target, GVR, namespace)` makes precise invalidation the natural thing
to express instead.

The relationship graph needed for this already half-exists in the control plane:
`gitTargetToWatchRules`, `gitProviderToWatchRules`, `clusterProviderToWatchRules`
mappers, and level-triggered status marks (`MarkTargetGitPathRefused` /
`MarkTargetGitPathAccepted`, render-fidelity scope clean/diverged). The control
plane already thinks in relationships and levels; the data plane still thinks in
events for both of its jobs.

---

## 7. Where we are

```mermaid
flowchart LR
    S1["<b>1. Coalesce</b><br/>keyed resyncs<br/><i>shipped in #312</i>"]
    S2["<b>2. Drop the payload</b><br/>gather at execution<br/><i>proposed here</i>"]
    S3["<b>3. Precise invalidation</b><br/>relationship graph → dirty scopes<br/><i>later</i>"]
    S1 --> S2 --> S3

    classDef done stroke-width:3px;
    class S1 done;
```

1. **Shipped.** Resyncs are keyed and coalesced; the storm source is fixed. The
   dirty set exists, carrying payloads.
2. **Proposed.** Drop the payload. Contained, and deletes more than it adds.
   Blocked on §5.1 being written down.
3. **Later.** Express invalidation as the relationship graph so a rule change
   dirties exactly the scopes it affects.

The writes stay a log throughout. Only the resync half moves.
