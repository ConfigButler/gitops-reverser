# Author attribution silently loses the actor

> **Status: OPEN.** Found 2026-07-20 while landing
> [PR 2](../design/watchrule-source-namespace/pr2-stream-scope-collapse.md); **not caused by
> it**. Predates that work.

## The symptom

A commit that should be authored by the human or service account that made the change is
authored by the configured committer instead. **Silently**, and **invisibly by inspection**:
`git.DefaultCommitterName` is `"GitOps Reverser"` ([internal/git/types.go:22](../../internal/git/types.go#L22)),
which is *also* the configured-author identity — so a lost-actor commit is byte-identical to a
correct configured-author commit. The only way to see it is to count it.

Naming the actor is the product's core promise, so this is a correctness defect, not cosmetics.

## How it happens

`ResolveAuthor` ([internal/watch/author_resolver.go](../../internal/watch/author_resolver.go))
polls the Redis attribution index for a fact matching the live event's `(uid, rv)`, waiting up
to `--author-attribution-grace` (default 3s). No match → commit as the committer.

## The number, and how far it can be trusted

`absent / total` from `gitopsreverser_attribution_resolutions_total`: **7.1%, 8.0%, 9.6%**
across three runs. Read it as *roughly 1 in 12–14*, not as a precise figure.

**What supports it.** Attribution runs **only on live events** — `attachAuthor` has one call
site, in `routeLiveTargetWatchEvent`
([target_watch.go:728](../../internal/watch/target_watch.go#L728)). `foldTargetReplayEvent`
never calls the resolver, so initial-replay of pre-existing objects contributes nothing to
either side of the ratio.

**What weakens it.**

- The queries use `sum(max_over_time(…[2h]))`, reconstructing totals across the deliberate
  controller restart in the `restart-reconcile` spec rather than counting exactly. The same
  population read instantaneously gave 92 where `max_over_time` gave 791.
- `absent` means *no fact matched*, which is not identical to *a human's name was lost*. A
  live change with no audited actor belongs there legitimately.

**To make it precise**, the resolver's metric needs a label separating *no fact was ever
produced* (correct) from *a fact should exist and we failed to match it* (the bug). Today both
collapse into `absent`, which is exactly why the number cannot yet carry much weight.

## Ruled out

| Hypothesis | Why not |
|---|---|
| **PR 2 (stream-scope collapse fix)** | Loss rate 8.0% with it vs 9.6% without; a PR 2 run passed the spec outright; all six CI e2e legs green. Its only runtime effect is `targetWatchSpecs`, whose output is byte-identical unless one `WatchedType` holds both `""` and a named key. |
| **Late fact arrival / grace too short** | Raising grace 3s → 10s left the rate **unchanged** (7.1%). Only ~1 resolution per run falls in the 3–10s band. Facts missing at 3s are still missing at 10s — they never arrive. |
| **Watermark / high-water gating** | `older_than_high_water = 0` in every run. |
| **Extra load from PR 2's duplicate streams** | Audit volume does not correlate: the *failing* run had the lowest (5667), a *passing* run the highest (9040). |
| **Wrong fact key shape** | Every ConfigMap uid in Valkey has **both** an exact-`rv` key and a `:last` key (46/46). Facts are indexed correctly. |
| **Audit webhook misrouted** | Posts to `/audit-webhook/default`, the correct **named** route ([webhook-config.yaml](../../test/e2e/cluster/audit/webhook-config.yaml)). ~92% of resolutions succeed, so delivery fundamentally works. |
| **Audit policy excluding the events** | Policy captures create/update/patch/delete/deletecollection broadly ([policy.yaml](../../test/e2e/cluster/audit/policy.yaml)). |
| **Initial-replay events with no actor** | Replay never invokes the resolver (see above). |
| **The e2e suite being "just flaky"** | Argued three times without evidence and wrong each time — though the spec *is* ~10%-flaky, because it samples this defect once per run. |
| **Controller restart mid-run disrupting the spec** | `restart_reconcile_e2e_test.go` is `Serial`; it cannot overlap parallel specs. |
| **`playground` Gitea key collision** | Self-inflicted contamination from a prior partial run, not a defect. |

## Fixed along the way

| Fix | Why it mattered |
|---|---|
| **`ci.yml` validates every PR** | `branches: [main]` meant stacked PRs ran **no** CI — #255 merged with no lint/test/e2e. Retargeting doesn't re-trigger (`edited` isn't a default trigger type). |
| **Author-mode probe reads the Deployment** | Was `kubectl logs --since=30m` grepping for a banner, failing **open**. Could silently swap in a weaker assertion that passes. |
| **`test/e2e/Taskfile.yml` → `Taskfile-e2e.yml`** | Running `task` from `test/e2e/` stopped Task's upward search there, so root-relative paths resolved nowhere. Caused `clean-cluster` to delete the cluster but not its stamps → next `prepare-e2e` skipped cluster creation. |
| **Hook blocking unguarded `task … \| …`** | A pipeline reports the *last* command's status, so a failed `prepare-e2e` looked successful and the suite ran against a nonexistent cluster. |
| **Per-stream declare logging** | `targetWatchSpecs` now names every stream (`<gvr>@<ns\|*cluster-wide*>=<ops>`) instead of a bare count. Found the real two-stream fan-out on `unsupported-folder-dest`. |
| **e2e after-suite attribution report** | Prints outcome + wait distribution every run, split resolved vs absent. |
| **Histogram `le` label bug** | Buckets render as `1.0`/`3.0`/`10.0`, not `1`/`3`/`10`; `%g` matched nothing above `0.5` and produced a *plausible but wrong* distribution. |

## Tools

- [`hack/attribution-diagnostics.sh`](../../hack/attribution-diagnostics.sh) — separates *fact
  never delivered* from *fact arrived too late*. Fact keys carry a 10-minute TTL, so the
  remaining TTL back-computes when each landed. Run **immediately** after an e2e run.
- After-suite report — outcome counts plus cumulative wait buckets, split by result.
- Valkey: `valkey-cli -a "$(kubectl -n valkey-e2e get secret valkey-auth -o jsonpath='{.data.*}' | base64 -d)" --scan --pattern '*author:v1:audit:*'`

## Measured facts worth keeping

- Fact delivery lag, **idle** cluster: `835–1101 ms`, bimodal — the signature of the
  apiserver's `--audit-webhook-batch-max-wait=1s`.
- Resolver mean wait by result: `weak` 0.001s · `exact_user` 0.533s · `absent` 2.268s (at the
  3s grace).
- Wait distribution (92-sample snapshot): `≤0.5s` 66 · `≤1s` 84 · `≤2s` 88 · `≤3s` 89 ·
  `≤10s` 90 · `+Inf` 92.

## Next steps

1. **Label the metric** to split *no fact existed* from *fact existed, match failed*. Until
   then the rate cannot be acted on.
2. If the second bucket is non-empty, compare the fact's recorded `rv` against the live event's
   `rv` for a failing object — the exact-key join is the likeliest place to lose a match.
3. **Decide the shipped default.** e2e now runs `--author-attribution-grace=10s`, but the
   default is still 3s and the evidence says the grace is *not* the constraint. Raising it
   ships latency for no proven benefit.
4. **Make the fallback loud.** Losing the actor should be observable at the commit, not only in
   a counter nobody reads.
