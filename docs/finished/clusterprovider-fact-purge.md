# Fact purge for `ClusterProvider`: decision record

> **finished** — shipped or closed. Kept for context only; **nothing here binds**. For current
> behaviour see [`../architecture.md`](../architecture.md). Index: [`../INDEX.md`](../INDEX.md)
>
> Prompted by a real failure: `helm uninstall` stranded the `default` `ClusterProvider` in
> `Terminating` forever, which then blocked reinstalling. This document asked whether
> purge-on-delete was the right mechanism. **Outcome: option D — no finalizer, and no purge.**

## The decision

**No finalizer, and no purge.** The `ClusterProvider` controller takes no finalizer; it only sheds
the retired one so objects created by older operators can still be deleted.

Two steps got here. First, a finalizer is the wrong *mechanism*: purge-on-delete needs a living
operator, which `helm uninstall` does not provide. Second — and this is what settled it — the purge
itself is not earning its keep. The exact-key join is
`(cluster, group/resource, object UID, resourceVersion)`, and a re-provisioned cluster mints fresh
object UIDs, so a stale fact cannot match on that key at all. Combined with a TTL that clears
everything within minutes, and with the fact that repointing a provider name is an operator/automation
action rather than a routine one, the residual risk does not justify a mechanism.

The rest of this document is the analysis that led there, kept because the reasoning matters more
than the conclusion.

### The one caveat worth knowing

The UID argument covers the exact key and the uid-keyed `:last` pointer. It does **not** cover the
**rv-only escape hatch** (`factKeyRV`), which is written when an audit fact carries no UID at all and
read as a last resort. resourceVersions are cluster-scoped integers, so a fresh cluster restarts low
and *can* collide with a stale fact from a previous incarnation.

Reaching a wrong author through it needs all of: a no-UID fact recorded under the old incarnation, a
new object whose uid-keyed lookups miss, an RV collision between the two clusters, and all of it
inside the TTL. The result is already classed `AttributionWeak`, affects one commit's author (never
Git content), and self-clears. Accepted knowingly rather than overlooked.

## What is being protected

When author attribution is enabled, each mutating audit event is reduced to a small **fact** in Redis
(who did it, to which object, at which resourceVersion). A live watch event later looks the fact up
and the commit is authored by that user instead of the configured committer.

Facts are keyed by **source cluster**, and the source cluster is a `ClusterProvider` **name**
([`attribution_index.go`](../../internal/queue/attribution_index.go)):

```
<prefix>:author:v1:audit:cluster:<provider-name>:<group/resource>:object:<uid>:<rv>
```

That name partitioning is what stops a fact from one cluster authoring a commit for a matching object
in another — an invariant covered by `TestAttributionIndex_CrossClusterIsolation` and
`TestAttributionIndex_RVOnlyHatchIsClusterScoped`.

Two properties matter for this decision:

- **Facts expire on their own.** They carry a TTL and nothing else deletes them. They are never object
  state; a miss just means "absent".
- **A provider name can be reused for a different physical cluster.** `spec.kubeConfig` is immutable,
  and the CEL rejection message tells you so explicitly: *"delete and recreate the ClusterProvider to
  point a name at a different cluster"*. Delete-and-recreate is therefore the **supported** way to
  repoint a name — not an exotic edge case. In practice it is an operator/automation action, not
  something that happens on its own.

## The actual risk

Put those together and the hazard is precise:

> A provider name is deleted and recreated against a **different cluster**, and a watch event from
> the new cluster joins a fact left over from the old one — crediting a user who never touched it.

The window is bounded by the fact TTL. Outside that window there is no hazard at all, because the
facts are gone by themselves.

This is a **misattribution** bug, not a data-loss one: Git content is unaffected, only the commit
author is. It is nonetheless the thing this whole feature exists to get right, so it is worth
addressing — but it is worth addressing *proportionately*.

> **Note — the documented TTL is wrong.** `DefaultAttributionFactTTL` is **15 minutes**, but the
> `--author-attribution-ttl` flag help and [`configuration.md`](../configuration.md) both say
> "default 10m". Chart installs are unaffected in practice because `values.yaml` sets `ttl: "10m"`
> explicitly, but a non-chart install gets 15m while the docs promise 10m. Worth fixing regardless of
> which option below is chosen.

## What the finalizer costs

The finalizer (`configbutler.ai/clusterprovider-fact-purge`, now retired to
`LegacyClusterProviderFinalizer` and only ever removed) held the object until the controller had
scanned and deleted every fact under that provider name.

The cost is that **a finalizer is a promise only a living operator can keep**:

- **`helm uninstall` removes the operator and the `ClusterProvider` together.** Nothing is left to
  detach the finalizer. The object strands in `Terminating` permanently, and the next
  `helm install --wait` then fails on it: `resource ClusterProvider//default not ready. status:
  Terminating`. Recovering needs a manual `kubectl patch` of the finalizer. Verified on a live
  cluster, and the reason the narrow fix shipped.
- **If the operator is simply down when someone deletes a provider**, the finalizer blocks the delete
  until it comes back — and if the object is force-removed to unblock, the purge never happens and the
  facts leak anyway. So the finalizer does not even reliably deliver the guarantee it exists for.

In the default configuration the trade was worse still: with no Redis there are no facts, so the
finalizer guarded *nothing* while still stranding the object. That is what the shipped fix removes.

## Options

### A. Unconditional finalizer (the old behaviour)

Take the finalizer always; purge on delete.

- **Pro** — simple, one code path.
- **Con** — strands the object on uninstall *even when there is nothing to purge*. This is the bug
  that was hit. **Rejected.**

### B. Conditional finalizer — *shipped, then superseded*

Take the finalizer only when a `FactPurger` is wired (i.e. attribution is on).

- **Pro** — small, safe, and removes the hazard for the chart default and the quickstart, which is
  where it actually bit. No semantic change to the attribution path.
- **Con** — only moves the problem. With attribution **enabled**, `helm uninstall` can still strand
  the provider, and the operator-is-down case is untouched. A partial fix.

### C. Purge on adopt — *the right way to keep a purge, if we wanted one*

Drop the finalizer. Instead, when a provider name is observed for the **first time**, purge anything
left under it. "First time" is detectable precisely as `status.observedGeneration == 0`: empty on a
freshly created object, and never reset by an operator restart, so a running provider is never
re-purged.

- **Pro** — same guarantee ("a recreated name starts clean"), and it is the guarantee that actually
  matters, because stale facts are only dangerous once the name is *in use again*.
- **Pro** — works when the operator was **down** during the delete, which the finalizer cannot do at
  all. Strictly more robust for the stated goal.
- **Pro** — no finalizer, so no liveness hazard: uninstall, reinstall and force-delete all behave.
- **Con** — facts for a name that is never recreated linger until their TTL rather than being deleted
  promptly. This is storage hygiene, not correctness, and the TTL already bounds it.
- **Con** — a bug in the "is this newly adopted?" test would purge **live** facts and silently degrade
  attribution to committer-authored commits. This is the one real risk and it needs a test that pins
  "a steady-state reconcile never purges".
- **Con** — needs a migration step: existing objects already carry the finalizer, so the controller
  must actively strip it, or they will strand exactly as before.

### D. No purge at all — rely on the TTL — **CHOSEN**

Delete the mechanism entirely and accept the window.

- **Pro** — simplest possible; no finalizer, no adopt hook, nothing to get wrong.
- **Con** — leaves a misattribution window on a supported workflow (repointing a name). Accepted:
  the exact key includes the object UID, which a re-provisioned cluster does not reproduce, so the
  window only exists for the narrow rv-only case described at the top.

### E. Purge on adopt **and** keep the finalizer as best-effort

Both: purge on adopt for correctness, finalizer for prompt cleanup.

- **Pro** — facts also disappear promptly in the common case.
- **Con** — reintroduces the whole liveness hazard for a benefit the TTL already provides. The
  finalizer is the expensive half and the least reliable half. **Not recommended.**

## Outcome — option D

Chosen over C on the grounds that the purge protects against something the key shape already
prevents. Option C's adopt-purge would have been the right way to *keep* a purge; it just turned out
not to be worth keeping one.

What was implemented:

1. **No finalizer is ever taken.** `AddFinalizer` is gone.
2. **The retired finalizer is shed**, including on an object already stuck in `Terminating` — that is
   the only way an object stranded by an older operator becomes deletable again, so the shed runs
   *before* the deletion check rather than after it.
3. `ClusterFactPurger` and its wiring are removed. `AttributionIndex.PurgeClusterFacts` remains
   (exported and tested) so a future purge has something to call.

`PurgeClusterFacts` was deliberately left in place. If the rv-only caveat above ever shows up in
practice, purge-on-adopt (option C) is the way to reintroduce it, and the mechanism is still there.

## Status

Implemented. The rv-only collision is a known, accepted residual risk.
