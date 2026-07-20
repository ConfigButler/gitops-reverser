# Session log — landing PR 2, and the attribution failure it surfaced

> Written 2026-07-20, at the end of the session that landed
> [PR 2](pr2-stream-scope-collapse.md) and opened the branch for
> [PR 3](pr3-clusterwatchrule-target-admission.md). Companion to
> [pr2-e2e-attribution-investigation.md](pr2-e2e-attribution-investigation.md), which holds
> the mechanism detail; this page is the readable top-to-bottom account: what was done, what
> was measured, what was **ruled out and on what evidence**, and what is still open.
>
> **Bottom line: PR 2 is merged to main (#255) and is NOT the cause of the e2e failure.**
> The cause is a pre-existing ~10% attribution loss — the 3s grace window is too tight
> against an audit delivery lag of ~1s (dominated by apiserver audit batching). Mechanism,
> measurements, and fix options: [§0 of the investigation](pr2-e2e-attribution-investigation.md#0-resolution).
>
> This page keeps the working record, including **three conclusions I reached confidently and
> then had to reverse**. That sequence is the most useful thing here.

---

## 1. What was set out to do

1. Settle an open question left by PR 1 — one unexplained e2e failure.
2. Implement PR 2 (a cluster-wide selection must not collapse named-namespace streams).
3. Prepare PR 3.

All three happened. A fourth thing happened that nobody planned: a genuine, pre-existing
attribution defect surfaced — ~10% of actors silently lost under load — plus four pieces of
test/CI infrastructure that were quietly lying to us.

---

## 2. PR 1's open question — closed

**Question:** the full suite failed `Commit Request Bundle (UC2)`
([commit_request_e2e_test.go:377](../../../test/e2e/commit_request_e2e_test.go#L377)) waiting
for a CommitRequest to become Ready. PR 1 changed `renderReconcileCommitMessage`'s signature
and added `Namespace` to `ReconcileCommitMessageData` — the only plausible contact point.

**Done:** re-ran with
`E2E_LABEL_FILTER='!image-refresh && !bi-directional && !source-cluster && commit-request'`.

**Result: all 4 commit-request specs passed.** Did not reproduce.

**Conclusion:** not a PR 1 regression. Note the caveat that only became visible later — a
single non-reproduction is weak evidence, and this suite turned out to have a *different*
failing spec on each subsequent run. This closure is "did not reproduce", not "proven
unrelated".

---

## 3. PR 2 — what shipped

**The defect.** `SnapshotNamespaces()` returned `nil` whenever a `WatchedType` carried the
`""` namespace key, and `nil` meant *all namespaces* at every read site. A WatchRule scoped
to one namespace and a ClusterWatchRule scoped cluster-wide, on the same GVR and GitTarget,
collapsed into one all-namespaces stream. The named scope survived only in the plan hash, and
`targetWatchSpecs` substituted `NamespaceOps[""]` for its operation set — so a `CREATE`-only
named rule co-resident with an `UPDATE`-only cluster-wide rule lost its filter too.

**The fix.** `SnapshotNamespaces` → `WatchedType.WatchScopes`, returning every scope
including the cluster-wide `""` instead of collapsing to it, with **both** read sites
projecting one stream per scope:

- `targetWatchSpecs` keys a watch per scope, each carrying `NamespaceOps[ns]`.
- `snapshotGVRsFromTable` **and** `resolveSnapshotGVRForType` project through one shared
  `snapshotGVRScopes` helper. `snapshotGVR.namespaces []string` became a single
  `namespace string`, matching `targetWatchKey` and matching how a dynamic client `List`
  takes a namespace (`""` = all).

**Design choice — distinct streams, not subtraction.** The spec allowed either. "All
namespaces except `team-a`" is not expressible as a watch, and the subtraction variant would
need a sweep scope shaped as a complement, which `git.ResyncScope` cannot represent.

**The replaced test.** `TestBuildWatchedTypeTable_ClusterWideOverridesNamedNamespaces`
asserted the collapse *as intended*. Deleted, not skipped. Three replacements, each verified
to fail against a restored pre-fix collapse, each on its own assertion: table level,
`targetWatchSpecs` (streams + op sets), `snapshotGVRsFromTable`.

**Merged** as #255. Base was auto-retargeted from the PR 1 branch to `main` when #254 merged.

---

## 4. The e2e story, in order

| # | Code | Scope | Result | Verdict on this run |
|---|---|---|---|---|
| 0 | PR 1 branch | `commit-request` only | 4/4 passed | closed §2 |
| 1 | PR 2 branch | full, 56 specs | 2 failed: `playground`, `CommitRequest generateName` | **void** — cluster contaminated by run 0 |
| 2 | PR 2 branch | full, 56 specs | 55 passed, **1 failed**: attribution | valid |
| 3 | `4f37759`, PR 2 **absent** | `manager`, 43 specs | 43 passed, **0 failed** | valid control |
| 4 | `10a530a`, PR 2 **present** | `manager` | 40 passed, **1 failed**: attribution | **valid, controlled** |
| 5 | `10a530a` main, PR 2 present | **CI**, 6 sharded legs | **all passed** | valid, different environment |
| 6 | `4f37759`, PR 2 absent | `manager` | 43 passed, **0 failed** | second control sample |
| 7 | PR 2 + stream instrumentation | `manager` | 43 passed, **0 failed** | **PR 2 passes** — split the "controlled" story open |
| 8 | PR 2 (run 7), metrics | — | absent **7 / 88 = 8.0%** | the measurement that settled it |
| 9 | `4f37759` baseline, metrics | — | absent **8 / 83 = 9.6%** | baseline is *worse* |

Runs 3, 4 and 6 looked decisive — same subset, same machine, control passing 2/2 and PR 2
failing 2/2, on a diff verified to be exactly PR 2's 8 files (the Gitea unpin `27bf27c` is an
ancestor of **both** commits, so it cannot explain anything). **That conclusion was wrong.**
Run 7 passed with PR 2 present, and runs 8–9 replaced the coin-flip with a rate measured over
~85 resolutions per arm: **8.0% loss with PR 2, 9.6% without**. Per-resource on baseline,
configmaps alone lose **10.0%**. The spec is ~10%-flaky on its own; three samples per arm
could never separate that from an effect, and I treated a 2-vs-0 split as proof.

**The control was verified to be a real control.** `git diff --stat 4f37759 10a530a` is
exactly the 8 `internal/watch` files of PR 2 and nothing else — in particular the Gitea unpin
(12.5.1 → 12.7.0) and dependency refresh in `27bf27c` are ancestors of **both** commits, so
they cannot explain the difference. This was checked precisely because a stacked branch can
easily lag main by unrelated commits; here it does not.

Run 5 pulls the other way: main's `Release` build runs `ci.yml` (whose first job everything
else `needs`), and **every** e2e leg passed — including `full-manager`, which contains this
exact spec. But CI shards across six legs, so each runs at materially lower contention than
one 56-spec local run at `--procs=4`. At the time this was read as "rules out a deterministic
regression but not a latent race" — correct as far as it went, and in hindsight CI was simply
sampling the same ~10% lottery at lower load.

---

## 5. The failing spec, explained

`Manager WatchRule ConfigMap and Secret >> should create Git commit when ConfigMap is added
via WatchRule`
([watchrule_configmap_secret_e2e_test.go:446](../../../test/e2e/watchrule_configmap_secret_e2e_test.go#L446)).

It proves a human's identity survives from `kubectl` into a Git commit author:

1. Create a `GitTarget` + a `WatchRule` selecting ConfigMaps in its namespace; wait for Ready.
2. Create a ConfigMap **impersonating a user**: `kubectl --as=jane@acme.com`. The
   impersonation is the point of the spec.
3. Poll a checkout of the Gitea repo until the YAML exists, the commit message carries
   `[CREATE]` and `v1/configmaps/test-configmap`, and `git log -1 --pretty=%an` is the
   impersonated user.

**The two modes.** In **configured-author mode** (attribution off, or no Redis) every commit
is authored by the configured committer. In **attribution mode** (the default here) the live
watch event is matched against an audit-webhook fact carrying the acting user; with no match
it falls back to `DefaultCommitterName`.

**The ambiguity that cost hours.** `DefaultCommitterName` is `"GitOps Reverser"`
([internal/git/types.go:22](../../../internal/git/types.go#L22)) — *also* the configured-author
identity. Observing `GitOps Reverser` means **either** "configured-author mode, working" **or**
"attribution mode, attribution lost". The string alone does not say which.

**The failure:** expected `jane@acme.com`, got `GitOps Reverser`.

---

## 6. What was ruled out, and on what evidence

Each of these was a live hypothesis. None survived.

### 6.1 A PR 1 regression — ruled out
Re-ran the commit-request specs in isolation: 4/4 passed (§2).

### 6.2 The `playground` Gitea failure — ruled out (self-inflicted)
`HTTP 422: Key title has been used`. A previous targeted run had registered the SSH key and
never ran playground's cleanup. Environmental contamination from my own partial run, not a
code defect. Invalidated run 1 entirely.

### 6.3 A mid-run controller restart disrupting the spec — ruled out
A spec does `rollout restart` the controller (explaining a 3-minute-old pod in a 10-minute
suite), but [restart_reconcile_e2e_test.go:34](../../../test/e2e/restart_reconcile_e2e_test.go#L34)
is decorated `Serial`, so Ginkgo runs it after all parallel specs. It cannot have overlapped.

### 6.4 The test asserting the wrong branch — ruled out
The probe *could* misreport (§7.2). It did not here: the Deployment carries `--redis-addr=…`
with no `--author-attribution=false`, and the controller emits no `configured-author mode:`
banner. So the deployment is genuinely in attribution mode, the probe genuinely returned the
right answer, and the spec was right to demand `jane@acme.com`.

**This also rules out the "after 30 minutes it stops detecting the mode" theory** for *this*
failure. Aging pushes the probe toward `false` = "expect `jane@acme.com`" — which is what it
already correctly returned. Aging can only cause a *false failure* in the opposite
configuration: a controller truly in configured-author mode, up >30 minutes. Real bug, wrong
suspect.

### 6.4a The control being contaminated by unrelated main commits — ruled out
See §4: `4f37759` and `10a530a` differ by exactly PR 2's 8 files. Checked explicitly.

### 6.5 "This suite is just flaky here" — RULED OUT
Argued three times on a code-reading (§6.6), then declared dead when the control came back
2/2 versus 0/2 — and then **reinstated by measurement**. Aggregate attribution-loss rates are
8.0% (PR 2) versus 9.6% (baseline), and configmaps alone lose 10.0% on baseline, so the spec
is ~10%-flaky per run on its own and the fail/pass split was noise. The spec *is* flaky here;
what was wrong was calling it flaky without evidence, and then calling it non-flaky on
evidence too thin to carry the claim. See [§0](pr2-e2e-attribution-investigation.md#0-resolution).

### 6.6 "PR 2 is a strict no-op in this configuration" — code-true, but insufficient
The claim: the change alters nothing unless a single `WatchedType` holds **both** the `""`
key and a named key.
- only named keys → old returned the sorted names, new returns the same;
- only `""` → old returned `nil` and the caller synthesised `targetWatchKey{GVR, ""}`; new
  returns `[""]` and builds the identical key.

The failing spec has one `GitTarget` with a single namespaced `WatchRule` and no co-resident
`ClusterWatchRule`, so it should never reach the changed branch. **The code reading still
looks correct — and the experiment contradicts it.** The experiment outranks the reading.

**The search space is small.** `WatchScopes()` has exactly ONE production consumer:
`targetWatchSpecs` ([target_watch.go:218](../../../internal/watch/target_watch.go#L218)).
Everything else PR 2 touched — `snapshotGVRsFromTable`, `resolveSnapshotGVRs`,
`resolveSnapshotGVRForType`, `snapshotGVRScopes` — has **no production callers at all**
(verified by grep on main, excluding tests). So the entire runtime effect of PR 2 reduces to
whatever `targetWatchSpecs` now returns.

**One genuine behavioural difference found so far**, and it is not obviously the culprit:
when a `WatchedType` has an **empty** `NamespaceOps` map, the old code fell into its
`len(namespaces) == 0` branch and synthesised a **cluster-wide** watch key
`targetWatchKey{GVR, ""}`; the new code iterates an empty scope list and creates **no key at
all**. Production should never produce an empty map (`buildWatchedTypeTable` always records
at least one namespace per selection), and the failure mode does not match — a missing watch
would mean the ConfigMap never reaches Git, whereas here the file and commit landed and only
the *author* was wrong. Recorded because it is a real, untested divergence.

Next step is instrumentation rather than more reading: capture the controller's
`watch-first target watch set reconciled` lines (they log `watchCount`) from a failing main
run and a passing baseline run and diff the declared watch sets for the spec's GitTarget. If
they are identical, the cause is downstream of the watch set and the reading is right; if
they differ, the difference names the bug.

---

## 7. Infrastructure defects found (all real, all fixed or filed)

### 7.1 Stacked PRs ran no CI at all — fixed
`ci.yml` triggered on `pull_request: branches: [main]`. A stacked PR based on the previous
PR's branch never matched. **#255 merged having run only CodeRabbit and GitGuardian — no
lint, no unit tests, no e2e.** Retargeting does not rescue it: when the base merges, GitHub
retargets and fires `pull_request` with action `edited`, which is not in the default trigger
set, so nothing re-runs.

Fixed by dropping the `branches` filter (PR #257). Without it, four of this workstream's five
PRs would merge unvalidated. **Caveat: the fix cannot validate itself** — #257 targets `main`,
so it runs CI regardless. First real proof is the next PR against a non-`main` base.

### 7.2 The author-mode probe was a log grep that fails open — fixed
`configuredAuthorModeEnabled` ran `kubectl logs … --since=30m`, grepped for a startup banner,
and returned `false` on any error. Three ways to misreport: banner ages out after 30 minutes;
`kubectl logs deployment/…` errors or picks the wrong pod mid-rollout; **the quickstart specs
deliberately redeploy the controller in configured-author mode, leaving that banner in the
shared log window for later specs to misread.** Given §5's ambiguity, a wrong answer silently
swaps in a weaker assertion that *passes*.

Fixed in #257: read the Deployment's container args, fail loudly if unreadable, with a pure
`configuredAuthorModeFromArgs` under unit test pinning the defaults (`cmd/main.go` defaults
**both** `--author-attribution` true and `--redis-addr` `valkey:6379`, so an absent flag means
attribution mode). The change can only make the assertion **stricter**.

That quickstart contamination path is also a plausible reason this spec looks healthier in CI
than locally: CI shards `quickstart-install` and `full-manager` into separate legs; locally
they share one cluster and one log window.

### 7.3 `task clean-cluster` is cwd-sensitive — filed, not fixed
It does `rm -rf .stamps/cluster/<ctx>` on a **relative** path. Run from `test/e2e` it deletes
nothing real while k3d still removes the cluster, leaving a stale `ready` stamp. `prepare-e2e`
then skips cluster creation and everything fails with `No nodes found for given cluster`. Cost
one wasted cycle. **Always run task targets from the repo root.**

### 7.4 Piping a task to `tail` masks its exit status — process fix
`task prepare-e2e | tail -15 && task test-e2e` takes `tail`'s status, so a failed prepare
reported success and the suite ran against a nonexistent cluster, producing a `BeforeSuite`
failure that then had to be unwound. Use `set -o pipefail` or capture exit codes separately.

---

## 8. Process lessons (mine)

- **A structural argument is not evidence about a timing-sensitive integration failure.** The
  §6.6 no-op reading was used to wave off three consecutive failures. The control that tested
  it cost ~6 minutes and should have been run after the *first* failure, not the third.
- **Two variables changed at once.** Run 2 (PR 2, full suite) vs run 3 (baseline, subset)
  differed in both code and load, so run 3 alone could not convict or acquit. Only runs 3+4
  isolated it.
- **I contaminated my own evidence twice** — the Gitea key (§6.2), then the cwd/pipefail pair
  (§7.3, §7.4) which produced two runs that tested nothing. Of six runs, one was void and two
  were empty.
- **Stated confidence outran the evidence** in three successive summaries. The maintainer's
  "I don't remember flakes around this test" was worth more than any of them.

---

## 9. Open questions — resolved

1. **Did the control hold at n=2?** Yes (baseline 2/2 passed) — and it did not matter. The
   aggregate loss rate settled it instead: 8.0% with PR 2 versus 9.6% without.
2. **How could §6.6 be code-true and the experiment still fail?** Both were right. The code
   reading was correct (PR 2 is a no-op for that spec's GitTarget); the experiment was
   sampling a ~10%-lossy process three times per arm and I read the split as signal.
   Instrumentation did find real case-C fan-out
   (`unsupported-folder-dest` gets two `configmaps` streams), but on a different GitTarget in
   a different namespace, and audit volume did not correlate with failure — the failing run
   had the *lowest* volume.
3. **Is attribution reliable under 4-proc load?** **No, and that is the real defect.** ~10% of
   ConfigMap resolutions lose the actor, independent of PR 2.
4. **Does #257 go green?** Open at time of writing.

## 10. State at end of session

- **Merged to main:** #254 (PR 1), #255 (PR 2). PR 2 is correct and stays.
- **Open:** #257 on `feat/watchrule-src-ns-pr3-clusterwatchrule-target-admission` — the CI
  trigger fix, the author-mode probe fix, per-stream declare logging,
  `hack/attribution-diagnostics.sh`, and these findings. **PR 3's actual ClusterWatchRule
  admission fix is not written yet.**
- **The real open defect:** attribution silently loses ~10% of actors under parallel load.
  The 3s grace window is too tight against a ~1s audit batching delay. Fix options in
  [§0](pr2-e2e-attribution-investigation.md#0-resolution). This is a correctness bug in the
  product's core promise — naming who made a change — and it is worth more than the PR 2
  question that surfaced it.
- **PR 3 groundwork:** design verified against the tree; every load-bearing claim holds. One
  gap found: the design says only a ClusterProvider → ClusterWatchRules mapper is missing, but
  admission is evaluated against **namespace labels**, and the GitTarget controller carries a
  matching `namespaceToGitTargets` watch. The CWR controller watches only GitTarget and
  GitProvider, so revocation by *relabelling a namespace* would converge for GitTargets and
  never for ClusterWatchRules. **PR 3 needs both mappers plus a revocation test for the label
  path.**
- **Also fixed by a parallel agent, and worth knowing:** `test/e2e/Taskfile.yml` is now
  `Taskfile-e2e.yml` so that `task` invoked from inside `test/e2e/` cannot stop its upward
  search there and run with the wrong working directory — the structural cause of the
  `clean-cluster` incident in §7.3. A `PreToolUse` hook now refuses unguarded `task … | …`
  pipelines (§7.4).
