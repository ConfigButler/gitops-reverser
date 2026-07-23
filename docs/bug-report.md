# Bug report — attribution is silently `unresolved` for objects mirrored through a non-`default` ClusterProvider

**For:** the gitops-reverser team
**Found by:** gitops-api, while spiking the one-reverser consolidation against branch
`feat/gittarget-prune-mode-pr5`
**Date:** 2026-07-21
**Reverser version:** branch `feat/gittarget-prune-mode-pr5` @ `b9c2e495` (past v0.38.0), built locally
as `gitops-reverser:e2e-local`, run in configured **attribution mode** (`--author-attribution`,
`--author-attribution-grace=10s`, `--redis-addr` set).
**Severity:** medium — mirroring is correct; **author attribution is silently lost**. It is only
visible at all because of the new `unknown (attribution unresolved)` author on this branch — which is
exactly what surfaced it.

## Summary

Every object mirrored through a GitTarget whose `spec.clusterProviderRef` names a **dedicated
in-cluster ClusterProvider** (one created with `kubeConfig` omitted, so it points at the operator's
own cluster, but with a **name other than `default`**) is committed as
`unknown (attribution unresolved) <attribution-unresolved@gitops-reverser.invalid>`, even though the
same actor's writes through the **`default`** ClusterProvider attribute correctly.

The audit facts *are* being written; they are simply never matched for objects whose GitTarget
resolves attribution under a non-`default` cluster identity. Our leading hypothesis is that the
attribution fact index is keyed per source-cluster (ClusterProvider), and a ClusterProvider with no
audit route delivering facts under **its own** identity — which a dedicated in-cluster provider is,
because the local apiserver's audit is filed under `default` — has an empty fact index, so every
lookup misses.

## Evidence (two back-to-back runs, reverser restarted between them to zero the counters)

Workload: the branch's own `manager`-labelled specs, focused to two Describe blocks —
`WatchRule source namespace` (`test/e2e/source_namespace_e2e_test.go`) and
`Manager GitTarget prune policy` (`test/e2e/prune_mode_e2e_test.go`). Both create `configmaps` on the
same in-cluster apiserver, as the same e2e identity, under the same audit policy.

**`gitopsreverser_attribution_resolutions_total`, `sum by (result,resource)` — identical across runs:**

| result | resource | Run A | Run B |
|---|---|---|---|
| `exact_user` | configmaps | 9 | 9 |
| `weak` | configmaps | 3 | 3 |
| `absent` | configmaps | **5** | **5** |

**The 5 `absent` are, in both runs, exactly the objects the source-namespace spec mirrors** — and
*only* those (the prune spec's objects all resolved):

```text
[CREATE] v1/configmaps/srcns-mirrored            → unknown (attribution unresolved)
[CREATE] v1/configmaps/srcns-wildcard-admitted   → unknown (attribution unresolved)
[CREATE] v1/configmaps/srcns-two-cm-a            → unknown (attribution unresolved)
[CREATE] v1/configmaps/srcns-two-cm-b            → unknown (attribution unresolved)
[CREATE] v1/configmaps/srcns-refused-cm          → unknown (attribution unresolved)
```

**The `absent` resolutions waited the FULL grace and no fact ever arrived**
(`gitopsreverser_attribution_resolution_wait_seconds_bucket{result="absent"}`):

```text
wait <= 10.0s : 0
wait <= +Inf  : 5     ← all 5 exceeded the 10s grace; a fact was never matched
```

**Facts are being written — they are just not matched for these objects**
(`gitopsreverser_attribution_fact_events_total`, Run B):

```text
written                 = 66
matched                 = 12      ← == exact_user(9) + weak(3); the resolved population only
deletecollection_expanded = 18
(no expired_unmatched, no late)
```

`written=66 / matched=12` with **zero `expired_unmatched`** means the resolver is looking up a key
under which no fact was ever filed — not a fact that was written and then aged out.

## The one structural difference between resolved and unresolved objects

| | resolves (`exact_user`/`weak`) | unresolved (`absent`) |
|---|---|---|
| spec | `Manager GitTarget prune policy` | `WatchRule source namespace` |
| `GitTarget.spec.clusterProviderRef` | omitted → defaults to `{name: default}` | a **dedicated** in-cluster provider `srcns-delegating` (created via `applyInClusterClusterProvider`, `kubeConfig` omitted) |
| `WatchRule` source namespace | own namespace | overridden (`rules[].sourceNamespace`, incl. `"*"`) |

The split is **100% clean and reproducible**: every `default`-provider object resolved, every
dedicated-provider object was `absent`. That rules out a stochastic grace-window/load effect (which
would be flaky and would not respect the provider boundary).

The source-namespace override and the dedicated ClusterProvider are both present on the srcns objects,
but the code path below shows the **override is irrelevant** and the **ClusterProvider identity is the
whole cause**.

## Root cause (confirmed in code + config, not just correlation)

The attribution fact index is keyed by **provider name**, and the write side and read side derive that
name from two different places that do not agree for a non-`default` in-cluster ClusterProvider:

1. **Facts are written under the provider name of the audit ROUTE.** `AuditHandler.resolveRoute`
   (`internal/webhook/audit_handler.go`) maps `/audit-webhook/<name>` → provider `<name>`, or resolves
   it per-event from `--audit-cluster-annotation-key` on the bare `/audit-webhook`. It then calls
   `RecordFact(ctx, providerName, event)`, which stores `factKeyExact/Last/RV(providerName, …)`
   (`internal/queue/attribution_index.go:129,182,385-395`).
   - In this environment the apiserver posts to **`/audit-webhook/default`** (its
     `webhook-config.yaml`) and the reverser has **no** `--audit-cluster-annotation-key`, so **every**
     local fact is filed under provider `default`.
2. **Facts are read under the GitTarget's ClusterProvider name.** The resolver calls
   `LookupAuthorResolution(ctx, providerName, …)` with `providerName = GitTarget.SourceCluster()` — the
   `spec.clusterProviderRef` name. For the srcns targets that is **`srcns-delegating`**.

`default` (write) ≠ `srcns-delegating` (read) → every lookup misses → `absent`, full grace, `written`
but never `matched`. The object's namespace matches on both sides, which is why the `sourceNamespace`
override plays no part.

**In short:** a ClusterProvider that attribution *reads* under its own name, but under which no audit
route ever *writes* a fact, yields silent `unresolved` for everything mirrored through it. A dedicated
in-cluster ClusterProvider (`kubeConfig` omitted, name ≠ `default`) is exactly that, because the local
apiserver's audit is filed under `default`.

## Why this matters to us (gitops-api)

Our one-reverser consolidation gives **every tenant workspace its own (non-`default`)
ClusterProvider** and relies on per-actor attribution as its core value. In our real topology each
workspace is *remote* and posts to the bare `/audit-webhook` with `--audit-cluster-annotation-key=
kcp.io/cluster`, so facts *are* filed under the provider name a GitTarget resolves to — provided the
provider is named exactly the annotation value and the flag is set. This finding is therefore not
(we believe) a blocker for the remote path, but it is a **loud warning about how silent the failure
mode is**: the moment the audit-write name and the GitTarget-read name diverge — a missing
annotation-key flag, a provider named differently from its `kcp.io/cluster` hash, or an in-cluster
provider whose audit still routes to `default` — **every commit becomes `unresolved` with no error,
no condition, and no failed reconcile**. Attribution is our product's core value, so a
misconfiguration that silently drops it (rather than failing loudly) is high-consequence for us.

## What is NOT affected

- **Mirroring is correct.** All five objects are mirrored to Git under the right folders; only the
  commit *author* is wrong.
- The `default` provider path is unaffected.

## Reproduction

1. Bring up the e2e harness on the branch (`task _cluster-ready && task prepare-e2e`; needs
   `controller-gen` on PATH and `HOST_PROJECT_PATH` set — see this repo's `hack/spikes/README.md`).
2. `kubectl -n gitops-reverser rollout restart deploy/gitops-reverser` to zero the counters.
3. Run the two specs:

   ```bash
   go run github.com/onsi/ginkgo/v2/ginkgo --label-filter='manager' \
     --focus='WatchRule source namespace|Manager GitTarget prune policy' ./test/e2e/
   ```

4. Query Prometheus: `sum by (result,resource) (gitopsreverser_attribution_resolutions_total)` and
   `sum by (op) (gitopsreverser_attribution_fact_events_total)`; and `git log --author` the mirrored
   repos under `.stamps/repos/*/` for `attribution unresolved`.

## What we'd most like fixed

**Make this failure loud.** A GitTarget (or ClusterProvider) with attribution enabled whose provider
has **received zero audit facts under its own name** should surface a condition / warning, instead of
silently authoring every commit as `unresolved`. The `unknown (attribution unresolved)` author on this
branch already makes it *visible in git*, which is how we found it — a status condition would make it
*actionable* before a single commit lands wrong.

## Open questions for the team

1. Is a **dedicated in-cluster ClusterProvider** (`kubeConfig` omitted, name ≠ `default`) intended to
   be a supported attribution configuration at all? If yes, where should its facts come from, given the
   local apiserver's audit is filed under `default`? If no, admission could reject/warn on it.
2. Confirmation that the **remote path we depend on** — bare `/audit-webhook` +
   `--audit-cluster-annotation-key`, provider named exactly the annotation value — files facts under
   the same name the GitTarget resolves to. (Our reading of the code says yes; we'd value a confirmation
   and, ideally, an e2e that asserts a *remote-CP* commit is attributed, since the current
   `source-cluster` spec only exercises an unreachable kubeconfig.)
3. Would you accept a small e2e that pins this: a GitTarget on the `default` provider **with** a
   `sourceNamespace` override attributes correctly (isolating the override as innocent), while a
   dedicated-provider GitTarget does not? We have the harness set up and can contribute it.

## Root-cause code references

- `internal/queue/attribution_index.go` — `RecordFact(providerName,…)` (:129), `writeFactKeys` (:182),
  `matchFactKey` under `factKeyExact/Last/RV(providerName,…)` (:385–395), `LookupAuthorResolution(providerName,…)` (:360).
- `internal/webhook/audit_handler.go` — `resolveRoute` / `providerRouteForPath` (:155–204): route → provider name.
- This environment: apiserver `webhook-config.yaml` server `…/audit-webhook/default`; reverser args carry
  no `--audit-cluster-annotation-key`; srcns GitTargets `clusterProviderRef: srcns-delegating`.
