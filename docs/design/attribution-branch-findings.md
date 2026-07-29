# Findings on the attribution fact switchover

What this branch's loose ends turned out to be, and which of them are measured rather than reasoned.
Five findings: one lab defect that hid every other measurement, three results about how an audit event
identifies its object that the corpus now carries, and one product race that the e2e suite was
failing to report and that is now root-caused and fixed.

The three identity results are measurements, captured by the mutation lab against the e2e cluster and
committed to the corpus. The race was measured from the resolver's own histogram against a live
cluster. Everything here is evidence rather than inference, except where it says otherwise.

## 1. The lab was serving the wrong audit route, so nothing was audited

The mutation lab registered exactly one audit path, `/audit-webhook`. The e2e cluster's audit
kubeconfig posts to `/audit-webhook/default`. The route is NAMED after the `default`
ClusterProvider, because the bare path is the product's shared, annotation-routed endpoint. Go's
`ServeMux` treats a pattern without a trailing slash as an exact match, so every audit event the API
server sent got a 404.

The store held 133 admission records and 10 watch records, and zero audit records. Every scenario
that requires an audit event timed out (including the seventeen long-standing ones), which reads
exactly like a broken cluster rather than a one-line routing mismatch. That is the expensive part:
the lab's failure mode is indistinguishable from the environment's.

Fixed by serving the whole `/audit-webhook/` subtree. Row 15 went from a 91-second timeout to passing
in 6.6 seconds, and every scenario passes. The seventeen previously committed corpus rows re-captured
byte-identical, which is the positive result for a re-capture.

## 2. An aggregated-API removal carries no uid

Corpus `flunder/aggregated-api-delete/`. The kube-apiserver proxies the request to the extension
server and never decodes what came back, so the audit event's `objectRef` carries:

```yaml
objectRef:
  apiGroup: wardle.example.com
  apiVersion: v1alpha1
  name: fl-del          # from the URL path
  namespace: <ns-1>
  resource: flunders
verb: delete
```

No `uid`. No `resourceVersion`. No `responseObject` to recover either from. Meanwhile the watch
`DELETED` for the same object carries the full body, uid included.

## 3. An aggregated deletecollection returns no response body

Corpus `flunder/aggregated-api-deletecollection/`. One audit record, name-less, with the selector
visible only in the `requestURI`, and no response body: the collection fact therefore carries no uid
set and the join can only proceed by scope. This is precisely the case the deleted response-body expander
produced nothing at all for, and it is the case `deletecollection_scope` exists to serve.

The scenario deletes three flunders to make the asymmetry unmistakable: three watch `DELETED` events
and three admission records against one audit record that names none of them.

## Where each verb falls out

```mermaid
flowchart TD
    A[Audit event on an aggregated type] --> B{objectRef has a name?}
    B -->|no: create| C[No fact published<br/>rejected at the name gate]
    B -->|yes: update, patch, delete| D{body present to backfill<br/>uid and resourceVersion?}
    B -->|deletecollection: name-less by nature| E[Collection fact<br/>selector + namespace]

    D -->|no: proxied, so no body| F{uid or resourceVersion<br/>on the fact?}
    D -->|yes: ordinary bodied type| G[uid and rv recovered]

    F -->|neither: name only| H[name tier<br/>namespace + name]
    G --> I[exact tier: uid + rv<br/>latest tier: uid]
    E --> J[deletecollection_body_uid if a uid set arrived<br/>deletecollection_scope otherwise]

    C --> K[Committer-authored]
    H --> L[Attributed to the actor]
    I --> L
    J --> L

    style C fill:#7f1d1d,color:#fff
    style K fill:#7f1d1d,color:#fff
    style H fill:#14532d,color:#fff
    style L fill:#14532d,color:#fff
```

Every verb but the create now reaches an author. Before the name tier, only the collection delete
did: an update, a patch or a single delete published a fact the index discarded on arrival, because
it could be keyed on nothing. The create still reaches no one, and cannot, because its audit event
never says which object it was about.

## 4. The missing name changes behavior only where there is no body

This was worth checking, because "the name is not available yet" describes both a `generateName`
create and an aggregated write, and it would be reasonable to expect them to fail the same way. They
do not, and the reason is the fork in the middle of the diagram.

`IdentityFromAuditEvent` takes namespace, name and uid from `objectRef`, then backfills whatever is
still missing from the event's body:

```go
preferred, fallback := bodyPriority(event, op)
backfillIdentityFromBody(&id, preferred)
backfillIdentityFromBody(&id, fallback)
```

For a `generateName` create on an ordinary type, `objectRef.name` is empty (the API server assigns
the name), but the policy captures at `RequestResponse`, so the `responseObject` carries the assigned
name and uid and the backfill recovers both. The fact publishes and joins normally.

For an aggregated write there is no body to backfill from, so nothing is recovered. The same empty
field is fatal in one case and harmless in the other, and the discriminator is the body, not the
name. So: yes, the unavailable name does change behavior, but only where the body cannot cover for
it, and that is exactly the aggregated population.

This is captured rather than left as reasoning. Corpus `configmap/generate-name-create/` is row 18,
the control for the two aggregated rows: it asserts that the `objectRef` carries no name and that the
response body does, so the recovery is evidence in the tree rather than a claim in a paragraph. Put
the three rows side by side and the discriminator is visible without reading any code.

## 5. The CommitRequest that missed its window: root-caused

The e2e spec `finalizes a CommitRequest created with metadata.generateName` failed in CI, in the full
local suite, and in an isolated local run. It was not a flake, and it is now fixed at its cause.

```mermaid
sequenceDiagram
    participant T as e2e spec
    participant K as kube-apiserver
    participant C as controller
    participant W as commit window

    T->>K: create Deployment
    Note over T,K: +105 ms
    T->>K: create CommitRequest (generateName)
    C->>C: author resolved from admission record
    loop every 2s for ~8s
        C->>W: attach enqueued
        W-->>C: no open window
    end
    C->>T: Ready=True reason=NoWindowInGrace sha=""
    Note over C,W: 2 seconds later
    C->>W: Opening commit window
```

Both runs showed the same two-second miss, so it was not load. Two earlier Deployments against the
same GitTarget opened their windows within a couple of seconds; the third did not open one for about
ten, and the request's grace expired at eight.

### What it was

Measured, on the branch, from the resolver's own histogram after one run of the commit-request specs:

| result | resolutions | total wait | mean |
|---|---|---|---|
| `exact_user` | 6 | 1.06s | 0.18s |
| `weak` | 3 | **20.18s** | **6.73s** |

(These are the label values of the day. `result` has since become `tier` plus `actor_kind`, so
`exact_user` reads `tier="exact", actor_kind="user"`, and `weak` splits: what was measured here is
the uid tier, now `tier="latest"`. The measurement stands; only the names moved.)

Three removals spent twenty seconds between them. The e2e cluster runs
`--author-attribution-grace=10s`, so each was waiting out most of a full grace, and `weak` is
precisely the tier that holds a removal matched to a WRITE fact.

That wait is not free, and this is the part that turns a slow resolution into a stalled pipeline.
`streamLiveTargetWatchEvents` processes one shard's events on a single goroutine, and `attachAuthor`
blocks it: a removal that waits out its grace is **head-of-line blocking** for every later event of
that type. Three of them ahead of a create is ten seconds before the create is even looked at, and
the commit window cannot open until it is. The CommitRequest's own grace expires first, and it
reports `NoWindowInGrace` about a window that had not been allowed to exist yet.

The two-second miss in the diagram is not a tuning problem between two graces. It is where the
serialized waits happened to land.

### Why the removals were waiting at all

A removal holds a per-object write fact as a fallback and keeps waiting for evidence about the
deletion, which is correct and deliberate. The question is why that evidence never arrived, when the
delete IS audited and the audit batch interval is one second.

Because the delete fact was there, and the lookup could not reach it.

Whether a delete fact carries a uid depends on what the API server answers the request with, and it
answers differently for different deletes. Both shapes are in the corpus:

- `configmap/finalizer-delete/audit.delete.yaml`: `responseObject` is the **ConfigMap**, so the uid
  is recoverable and the fact is filed under the uid tier, where a removal finds it at once.
- `configmap/owner-ref-cascade/audit.delete.cm-parent.yaml`: `responseObject` is a **`Status`**.
  There is no uid in it and none in the `objectRef`, so the fact's only key is its name. A
  `kubectl delete deployment` is this shape.

Before the name tier existed, such a fact was published and dropped as unjoinable, so the removal
had no delete evidence at all and waited out the grace. That is the original bug, and it is as old as
the audit event's shape rather than as old as any commit here.

Adding the name tier stored the fact but did not make it reachable, which is a defect in that change
rather than in the design. `Lookup` returns as soon as the removal ladder yields anything, and the
uid tier yields the object's last WRITE fact, so the name-keyed DELETE fact below was never
consulted. The caller then held the write fact and waited the full grace for evidence that was
already in the index.

The fix is an ordering rule, and it is the one the removal path already states elsewhere: a fact
about the DELETION outranks a fact about a write, whichever key each is filed under. `lookupRemoval`
now returns the object's own delete fact when the uid tier has one, then consults the name tier for a
delete fact, and only then falls back to the write fact it was holding. A name-keyed WRITE still does
not jump the queue; only removal verbs do.

### The same measurement, after

| result | before: n / total / mean | after: n / total / mean |
|---|---|---|
| `exact_user` | 6 / 1.06s / 0.18s | 6 / 0.74s / 0.12s |
| `name` | none reached | 2 / 0.60s / 0.30s |
| `weak` | 3 / 20.18s / **6.73s** | 2 / 0.28s / **0.14s** |
| **total wait** | **21.24s** | **1.63s** |

The spec passes, and the histogram says it passes for the diagnosed reason rather than by timing luck.
Two resolutions now land on the `name` tier: those are the delete facts that were being stored and
never read. The removals that still resolve `weak` no longer wait for them, because the lookup
reaches the delete evidence before it settles for a write fact, and the shard is no longer blocked
behind a grace that had nothing to wait for.

### The assertion that hid it

The spec asserted `Ready=True` and then a non-empty `status.sha`. But `Ready=True` is also the benign
rejection state: `rejectCommitRequest` sets it deliberately for `NoWindowInGrace`, `WindowMismatch`
and `AlreadyPresent`, so that kstatus reads Current rather than Failed. The Ready assertion therefore
passed on a request that committed nothing, and the spec spent the remaining two minutes re-reading
an empty string before reporting:

```text
Expected
    <string>:
not to be empty
```

The reason was sitting in the condition the spec never read. `expectCommitRequestCommitted` now
requires the Ready reason to be `Committed` and gives up as soon as any other terminal outcome
appears, because a terminal outcome is final and re-reading it cannot change the answer. The same
failure now reports in ten seconds, naming `NoWindowInGrace` and its message.

## The decision: a name tier, built

A name tier is now built rather than proposed. `AuthorFact.Name` is back on the wire, and the
index files a fact under `(namespace, name)` when it carries neither a uid nor a resourceVersion.
`Lookup` consults that tier last, below the rv-only hatch, and reports `AttributionName`.

The ordering argument is that a name is the weakest per-object evidence available: it is reused after
a delete and recreate, where a uid never is and an rv identifies one specific write. Ranking it last
costs the stronger tiers nothing, because no fact carrying a uid or an rv is ever filed there, and no
query reaches it until every stronger tier has missed.

What it fixes is exactly the two rows it can reach: an aggregated update or patch, and an aggregated
single delete. Both used to publish a fact the index then discarded, so they shipped
committer-authored whoever ran them.

Restoring the field is the part worth being explicit about. It was removed during this work on the
observation that no tier read it. That was true of the code and false of the domain: for a whole
population of writes the name is the only identity the audit event carries, so a fact without it
could not be joined at all. "No code reads it" and "nothing could ever read it" are different claims,
and only the second one justifies deleting a field.

### What the name tier still does not reach

**The aggregated create.** Its `objectRef` carries no name, and there is no response body to recover
one from, so `AuthorFactFromEvent` rejects it at the name gate and nothing is published for any tier
to join. This is not a gap in the tier; it is a request the API server logged without ever saying
which object it was about.

Two options remain open for that population, and they are not exclusive:

**B. Accept that aggregated creates are unattributable.** Document that per-object attribution does
not apply to them, and let them ship committer-authored. Honest, but it makes the guarantee
type-dependent in a way a user cannot predict from the API surface.

**C. Shorten the wait for facts that provably are not coming.** An aggregated create can be
recognized as unattributable at publish time rather than after a full grace. This attributes nothing;
it stops paying for evidence that cannot arrive.

### For the window race

The measurement answered the question this section used to pose. Neither option was right: the window
was not slow to open and the CommitRequest's grace was not too short. The write's event had not been
processed yet, because three removals ahead of it were sitting out most of a ten-second grace each
(20.2s between them, measured) for evidence the index already held.

That is fixed at its cause, in the lookup ordering. What remains open is the structural half.

**The head-of-line block is still there.** Any removal that must wait out its grace (a type the
audit policy excludes, a delete whose fact never arrives) still stalls every later event on its shard
for up to ten seconds. This fix removes the largest population that was hitting it, and
does not change the fact that a blocking resolve on a serial goroutine can do this at all.

Two directions, and they are the same two the wait-versus-poll record already frames:

1. **Bound the removal's extra wait separately from the grace.** Once a fallback is in hand, the
   fact stream for that scope is demonstrably live; what is outstanding is only whether a delete fact
   also lands, which is an audit-batch interval rather than a full grace. Small change, needs a
   number chosen with evidence.
2. **Stop blocking the shard.** Resolve attribution off the event loop and reassemble in order. This
   is the real answer and the larger one; it also fixes every other cause of a slow resolve.

Worth noting for whoever picks this up: the e2e default of `--author-attribution-grace=10s` makes the
blocking three times worse than the product default of 3s. That is a test-environment choice
amplifying a product behavior, not a product setting anyone runs.

### A note on the pre-branch probe

A run against the merge-base (`7ece7310`) was attempted to establish whether the race was new on this
branch, and it did not produce an answer: its bring-up failed before any spec ran, with the manager
rejecting the API server's audit client certificate (`tls: bad certificate`) so the audit pipeline
never warmed up. That is a worktree/cluster-provisioning problem rather than a product difference:
both trees post to the same named audit route and both serve it.

The comparison turned out not to be needed. The cause is measured directly on the branch, and the
delete-fact shape that triggers it is a property of the Kubernetes audit event rather than of any
commit here.
