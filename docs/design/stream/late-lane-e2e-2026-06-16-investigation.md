# Late-lane e2e diagnostics, 2026-06-16

Status: **analysis / no code change yet.**

This note records the late-lane contents from a fresh-cluster `task test-e2e` run after
`task clean-cluster`. The suite passed:

```text
Random seed: 1781599706
Ran 48 of 56 specs
48 Passed / 0 Failed / 8 Skipped
Ginkgo total: 9m7.194712491s
```

The goal of this investigation is narrower than "make the late lane empty". It is to decide
whether the entries in the late lane are evidence of real audit delivery reordering, or whether
they are diagnostic noise that should be classified away so real late events are easier to see.

Raw snapshots:

- [`late-lane-e2e-2026-06-16-midrun.tsv`](late-lane-e2e-2026-06-16-midrun.tsv)
- [`late-lane-e2e-2026-06-16-final.tsv`](late-lane-e2e-2026-06-16-final.tsv)

The TSV columns are:

```text
base id reason verb api_group resource namespace name rv last_rv rv_gap user stage_millis
```

## Executive Summary

The final snapshot contains **54** late-lane entries:

| Reason | Count | Interpretation |
|---|---:|---|
| `older-than-high-water` | 40 | Numeric RV was below the type stream's current high-water. |
| `rv-missing-before-high-water` | 14 | Event had no usable RV and the stream had no high-water to attach to. |
| `non-numeric-rv` | 0 | No aggregated/non-decimal RVs observed. |

The signal is concentrated in a small number of buckets:

| Base | Late entries | Dominant cause |
|---|---:|---|
| `gitops-reverser:core:secrets` | 18 | Flux server-side apply dry-run patches in the bi-directional spec. |
| `gitops-reverser:wardle.example.com:flunders` | 12 | Namespace-controller `deletecollection` during namespace teardown, no RV. |
| `gitops-reverser:k3s.cattle.io:addons` | 8 | k3s supervisor re-applies old Addon objects during bootstrap. |
| `gitops-reverser:bi-directional.e2e.example.com:icecreamorders` | 9 | Flux dry-run apply/patch of CRs, including two RV-less cold entries. |
| Other small buckets | 7 | Warmup ConfigMap/Namespace/CRD entries and kubelet Service re-apply. |

**Conclusion:** this run does not justify building a pre-sorter or more queue machinery in front
of the ordered per-type streams. Most late-lane entries appear to be one of these classes:

- dry-run requests that cannot change cluster state;
- no-op/stale-RV re-applies that return an old object resourceVersion;
- RV-less namespace cleanup for a cold type stream;
- bootstrap/system-controller re-application of already-existing state.

Those are useful facts, but they are not the small bounded "event delivered slightly late" pattern
that a pre-sorter would solve.

## Observed Distribution

Final counts by reason:

```text
40 older-than-high-water
14 rv-missing-before-high-water
 0 non-numeric-rv
```

Final counts by base and reason:

```text
18 gitops-reverser:core:secrets                                  older-than-high-water
12 gitops-reverser:wardle.example.com:flunders                    rv-missing-before-high-water
 8 gitops-reverser:k3s.cattle.io:addons                            older-than-high-water
 7 gitops-reverser:bi-directional.e2e.example.com:icecreamorders   older-than-high-water
 2 gitops-reverser:core:services                                   older-than-high-water
 2 gitops-reverser:core:configmaps                                 older-than-high-water
 2 gitops-reverser:bi-directional.e2e.example.com:icecreamorders   rv-missing-before-high-water
 2 gitops-reverser:apiextensions.k8s.io:customresourcedefinitions  older-than-high-water
 1 gitops-reverser:core:namespaces                                 older-than-high-water
```

For the 40 `older-than-high-water` entries:

```text
min gap: 2
p50 gap: 123
mean gap: 625.65
max gap: 2511
```

By user:

```text
27 system:serviceaccount:flux-system:kustomize-controller
12 system:serviceaccount:kube-system:namespace-controller
10 system:k3s-supervisor
 3 system:admin
 2 system:serviceaccount:prometheus-operator:prometheus-operator
```

By verb:

```text
28 patch
12 update
12 deletecollection
 2 create
```

This is already diagnostic: if this were primarily a queue-ordering problem, I would expect a
spread across normal user writes and small RV gaps. Instead the entries cluster around controllers
that repeatedly reconcile existing resources.

## How The Current Code Routes These Events

`RedisByTypeStreamQueue.Enqueue` resolves a resourceVersion using:

1. `responseObject.metadata.resourceVersion`
2. `requestObject.metadata.resourceVersion`
3. `objectRef.resourceVersion`

A numeric RV is appended to the main stream as `<rv>-*`. Redis/Valkey rejects that append if the
stream already has a higher last-generated ID. The code then diverts the event to
`:audit:late` with:

```text
reason = older-than-high-water
lastRV = stream top RV
```

An RV-less event normally attaches to the current stream high-water. If the stream has no entries
yet, there is no high-water to attach to, so it is diverted with:

```text
reason = rv-missing-before-high-water
lastRV = ""
```

That behavior is correct for a strict RV-ordered stream. The question is whether every diverted
event is a meaningful "late" event. This run says no.

## Example Bodies

The raw late-lane rows contain full `payload_json` bodies in Valkey. The examples below are compact
field extracts from representative rows, not complete payloads.

### 1. k3s Addon Update With Old RV

TSV row:

```text
base=gitops-reverser:k3s.cattle.io:addons
id=1781599888849-0
reason=older-than-high-water
name=ccm
rv=281
last_rv=2355
rv_gap=2074
```

Payload extract:

```json
{
  "verb": "update",
  "user": "system:k3s-supervisor",
  "userAgent": "deploy@k3d-gitops-reverser-test-e2e-server-0/v1.35.2+k3s1 (linux/amd64) k3s/13563feb",
  "requestURI": "/apis/k3s.cattle.io/v1/namespaces/kube-system/addons/ccm",
  "objectRef": {
    "resource": "addons",
    "namespace": "kube-system",
    "name": "ccm",
    "apiGroup": "k3s.cattle.io",
    "apiVersion": "v1",
    "resourceVersion": "281"
  },
  "requestRV": "281",
  "responseRV": "281",
  "responseGeneration": 2,
  "responseCode": 200
}
```

Interpretation:

- The request and response both carry RV `281`.
- The type stream high-water had already reached `2355`.
- This looks like a system-controller re-apply of old bootstrap state, not a fresh write that was
  delivered a little late.
- A short pre-sorter would not help a gap of 2074. The event RV itself is old.

### 2. Flux Secret Patch With `dryRun=All`

TSV row:

```text
base=gitops-reverser:core:secrets
id=1781600041231-0
reason=older-than-high-water
name=bi-secret-1781599981478403287
rv=3673
last_rv=3741
rv_gap=68
```

Payload extract:

```json
{
  "verb": "patch",
  "user": "system:serviceaccount:flux-system:kustomize-controller",
  "userAgent": "kustomize-controller/v0.0.0 (linux/amd64) kubernetes/$Format",
  "requestURI": "/api/v1/namespaces/1781599706-test-bi-directional/secrets/bi-secret-1781599981478403287?dryRun=All&fieldManager=kustomize-controller&force=true",
  "objectRef": {
    "resource": "secrets",
    "namespace": "1781599706-test-bi-directional",
    "name": "bi-secret-1781599981478403287",
    "apiVersion": "v1"
  },
  "requestRV": null,
  "responseRV": "3673",
  "responseCode": 200
}
```

Interpretation:

- The request is explicitly `dryRun=All`.
- It cannot persist a cluster-state change.
- It nevertheless carries a response object with an older RV than the per-type stream high-water.
- This is late-lane noise from the perspective of the git mirror. It should not force us toward
  queue ordering machinery.

### 3. Flux IceCreamOrder Patch With `dryRun=All`

TSV row:

```text
base=gitops-reverser:bi-directional.e2e.example.com:icecreamorders
id=1781600031692-0
reason=older-than-high-water
name=bi-bob-order-1781599981478403287
rv=3325
last_rv=3492
rv_gap=167
```

Payload extract:

```json
{
  "verb": "patch",
  "user": "system:serviceaccount:flux-system:kustomize-controller",
  "requestURI": "/apis/bi-directional.e2e.example.com/v1/namespaces/1781599706-test-bi-directional/icecreamorders/bi-bob-order-1781599981478403287?dryRun=All&fieldManager=kustomize-controller&force=true",
  "objectRef": {
    "resource": "icecreamorders",
    "namespace": "1781599706-test-bi-directional",
    "name": "bi-bob-order-1781599981478403287",
    "apiGroup": "bi-directional.e2e.example.com",
    "apiVersion": "v1"
  },
  "requestRV": null,
  "responseRV": "3325",
  "responseGeneration": 1,
  "responseCode": 200
}
```

Interpretation:

- Same dry-run class as the Secret entries.
- The response RV is not new. It identifies the existing object state used to answer Flux's
  dry-run request.
- This is probably safe to exclude from the per-type mirror entirely, because no persisted object
  changed.

### 4. Flux IceCreamOrder Dry-run Create Before High-water

TSV row:

```text
base=gitops-reverser:bi-directional.e2e.example.com:icecreamorders
id=1781599997497-0
reason=rv-missing-before-high-water
name=bi-bob-order-1781599981478403287
rv=""
last_rv=""
```

Payload extract:

```json
{
  "verb": "patch",
  "user": "system:serviceaccount:flux-system:kustomize-controller",
  "requestURI": "/apis/bi-directional.e2e.example.com/v1/namespaces/1781599706-test-bi-directional/icecreamorders/bi-bob-order-1781599981478403287?dryRun=All&fieldManager=kustomize-controller&force=true",
  "objectRef": {
    "resource": "icecreamorders",
    "namespace": "1781599706-test-bi-directional",
    "name": "bi-bob-order-1781599981478403287",
    "apiGroup": "bi-directional.e2e.example.com",
    "apiVersion": "v1"
  },
  "requestRV": null,
  "responseRV": null,
  "responseGeneration": 1,
  "responseCode": 201
}
```

Interpretation:

- This is another `dryRun=All` event.
- Because the response object has no persisted RV and the stream was cold, it entered the late lane
  as `rv-missing-before-high-water`.
- Treating this as "late" is actively misleading. It is a dry-run create-shaped response for a
  write that did not persist.

### 5. Wardle Flunder Namespace Teardown

TSV row:

```text
base=gitops-reverser:wardle.example.com:flunders
id=1781599901657-0
reason=rv-missing-before-high-water
verb=deletecollection
namespace=gitops-reverser-audit-warmup
rv=""
last_rv=""
```

Payload extract:

```json
{
  "verb": "deletecollection",
  "user": "system:serviceaccount:kube-system:namespace-controller",
  "userAgent": "k3s/v1.35.2+k3s1 (linux/amd64) kubernetes/13563fe/system:serviceaccount:kube-system:namespace-controller",
  "requestURI": "/apis/wardle.example.com/v1alpha1/namespaces/gitops-reverser-audit-warmup/flunders",
  "objectRef": {
    "resource": "flunders",
    "namespace": "gitops-reverser-audit-warmup",
    "apiGroup": "wardle.example.com",
    "apiVersion": "v1alpha1"
  },
  "requestRV": null,
  "responseRV": null,
  "responseCode": 200
}
```

Interpretation:

- Namespace teardown triggers collection deletes for namespaced resources.
- `deletecollection` has no single object and no object RV.
- The `flunders` stream was cold, so there was no high-water to attach the RV-less event to.
- This is a cleanup diagnostic, not evidence of out-of-order audit delivery.

### 6. Prometheus Operator Kubelet Service Re-apply

TSV row:

```text
base=gitops-reverser:core:services
id=1781600179084-0
reason=older-than-high-water
name=kubelet
rv=1426
last_rv=3937
rv_gap=2511
```

Payload extract:

```json
{
  "verb": "update",
  "user": "system:serviceaccount:prometheus-operator:prometheus-operator",
  "userAgent": "PrometheusOperator/0.89.0",
  "requestURI": "/api/v1/namespaces/kube-system/services/kubelet",
  "objectRef": {
    "resource": "services",
    "namespace": "kube-system",
    "name": "kubelet",
    "apiVersion": "v1",
    "resourceVersion": "1426"
  },
  "requestRV": "1426",
  "responseRV": "1426",
  "responseCode": 200
}
```

Interpretation:

- This is a controller re-applying a long-lived object whose RV did not change.
- The event is far behind the type high-water because many unrelated Service events advanced the
  stream after the kubelet Service's stable RV.
- A pre-sorter would not help this class either.

## Cause Classes

### C1. Dry-run audit events are mirrored

Evidence:

- Flux examples include `dryRun=All` in `requestURI`.
- Some dry-run events return old RVs.
- Some dry-run create-shaped responses return no RV and enter the cold RV-less path.
- `rg` did not find an existing dry-run filter in the audit handler or per-type queue path.

Effect:

- Dry-run requests can enter both the main stream and late lane even though they cannot change
  persisted cluster state.
- They can inflate `lateCount`.
- If they enter the main stream, consumers may have to rely on later no-op behavior or checkpoint
  convergence to avoid visible churn.

Prevention:

- Parse `requestURI` query parameters and drop events with `dryRun=All` before canonical/per-type
  mirroring.
- Alternatively record them in a separate diagnostic counter such as `dryRunCount` and do not feed
  them to the ordered stream.

Do we want this?

Probably yes. A dry-run event cannot be the source of truth for Git because it did not mutate the
API server's stored state. Filtering dry-run events is cleaner than trying to order them.

### C2. No-op or stale-RV controller re-applies

Evidence:

- k3s Addon and kubelet Service examples have `requestRV == responseRV`.
- The RV is hundreds or thousands behind the type high-water.
- The users are controllers (`system:k3s-supervisor`, Prometheus Operator), not the e2e action that
  expects a mirrored Git commit.

Effect:

- These look like old events to the RV-ordered stream, but the old RV appears to be the object's
  stable current RV, not merely a delayed fresh write.
- The event cannot be placed in the main stream without breaking strict type-level RV ordering.
- The late lane records it, which is accurate mechanically but noisy diagnostically.

Prevention options:

1. Classify `requestRV == responseRV` update/patch entries as `unchanged-rv` and keep them out of
   the "real late" count.
2. Drop unchanged-RV events from the per-type mirror if we can prove they did not persist a change.
3. Keep recording them, but send them to a separate diagnostic stream/counter.

Do we want this?

Maybe, but with more care than dry-run filtering. `requestRV == responseRV` is strong evidence of
no persisted write, but the code should not infer too much from partial or malformed bodies. A
classification-only first step is safer: keep the raw record, but do not let it pollute the main
late-lane alert.

### C3. Cold RV-less `deletecollection`

Evidence:

- All `wardle.example.com/flunders` late entries are `deletecollection`.
- User is the namespace controller.
- `rv`, `last_rv`, and object name are empty.
- These happened during namespace cleanup across many e2e namespaces.

Effect:

- `deletecollection` cannot name one object and often carries no usable RV.
- On a warm stream, `ingestRVLess` attaches it to the current high-water.
- On a cold stream, there is no high-water, so it is currently recorded as late.

Prevention options:

1. Reclassify as `cold-rvless-deletecollection`, separate from late.
2. Do not increment `lateCount` for cold RV-less events.
3. If the type is claimed/materialized, use the existing checkpoint/resync path as the correctness
   mechanism, not the ordered log.

Do we want this?

Yes for diagnostics. Whether the event should still be stored somewhere is a product/debugging
choice. It should not count as "out-of-order audit delivery"; it is "unorderable cleanup before a
stream anchor exists".

### C4. Real out-of-order delivery remains possible

The late lane still has a legitimate job. A numeric event with a fresh object RV can arrive after a
later RV for the same type. That event is not safe to append to the main stream, and the checkpoint
must backstop it.

The problem in this run is not that the late lane is wrong. It is that different phenomena share
the same lane and counter.

## Prevention Options

### Option A: Reporting-only classification

Keep ingestion unchanged. Add diagnostic queries/reports that subtract:

- `dryRun=All`
- `rv-missing-before-high-water` with `deletecollection`
- unchanged-RV controller re-applies

Pros:

- No correctness risk.
- Fastest way to make future investigations clearer.

Cons:

- The stream still contains noise.
- Consumers still need to survive dry-run events if they reach the main stream.

### Option B: Filter dry-run events before mirroring

Drop events whose `requestURI` has `dryRun=All` before the canonical and per-type mirrors.

Pros:

- Dry-run requests cannot mutate stored cluster state, so they should not drive Git.
- Removes a large noisy class from both main stream and late lane.
- Prevents dry-run create-shaped responses from becoming RV-less stream entries.

Cons:

- Requires careful URL parsing and tests.
- Need to decide whether early debug logs/metrics should still record them for observability.

Recommendation:

This is the strongest candidate for a real code change.

### Option C: Split late-lane reasons/counters

Keep raw records but split counters:

```text
lateCount                 # true numeric older-than-high-water candidates
dryRunCount               # dry-run mutation-shaped audit events
coldRVLessCount           # RV-less event before stream high-water
unchangedRVCount          # response RV equals request/objectRef RV, no apparent persisted change
nonNumericRVCount         # aggregated/non-decimal RVs
```

Pros:

- Preserves forensic data.
- Makes "real late" visible.
- Avoids hiding weird behavior.

Cons:

- More schema and docs work.
- Consumers and diagnostics need to learn the new counters.

Recommendation:

Worth doing if we want the late lane to remain a decision input for queue improvements.

### Option D: Build a pre-sorter

Buffer incoming events briefly by RV so a slightly-late event can still enter the main stream before
a higher RV advances the type high-water.

Pros:

- Solves genuine small reorder windows.

Cons:

- Does not solve dry-run events.
- Does not solve stale/no-op re-applies with old RVs.
- Does not solve cold RV-less deletecollection.
- Adds latency and complexity.
- The observed gaps are often large: p50 123, max 2511. A sane sorter window would not catch most
  of this run's late entries.

Recommendation:

Not justified by this run.

### Option E: Lua/atomic ingestion

Use a server-side script to atomically compare, append, and update idstate.

Pros:

- Useful if multi-writer ingestion introduces races around stream top and idstate.

Cons:

- This run used one controller pod for most of the suite and still saw late entries.
- The observed entries are mostly old-RV/dry-run/cold-RV-less classes, not idstate races.

Recommendation:

Not justified by this run.

## Recommended Next Step

Do not build queue-front machinery yet.

Instead:

1. Add explicit dry-run detection and decide whether to drop dry-run events before mirror ingestion.
2. Split the diagnostic taxonomy so `lateCount` means "numeric RV below high-water and potentially
   meaningful".
3. Track cold RV-less events separately.
4. Track unchanged-RV re-applies separately, initially as diagnostics only.
5. Re-run `task clean-cluster && task test-e2e` and compare:
   - true late count;
   - dry-run count;
   - cold RV-less count;
   - unchanged-RV count;
   - per-type distribution and RV-gap stats.

Only after that should we revisit the pre-sorter. If the filtered true-late bucket still contains
normal persisted writes with small bounded RV gaps, a sorter becomes plausible. If it drops close to
zero, the better fix was diagnostic hygiene, not queue machinery.

## Open Questions

- Should dry-run events be dropped before both canonical and per-type streams, or only before
  per-type materialization?
- Is `requestRV == responseRV` sufficient evidence to suppress an update/patch, or should it only
  classify the event for diagnostics?
- Should cold RV-less `deletecollection` still nudge materialization when the type is claimed?
- Do we want a separate `:audit:ignored` stream for forensics, or are counters enough?
- Should e2e add an assertion that dry-run audit events do not produce Git commits?
