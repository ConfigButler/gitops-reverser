# Investigation — the author-attribution e2e failure seen while landing PR 2

> Working document, opened 2026-07-20 while landing
> [PR 2](pr2-stream-scope-collapse.md). **Status: unresolved.** It records what was
> measured, what each measurement does and does not establish, and where the reasoning
> went wrong, so the next person does not repeat the loop.
>
> **RESOLVED — see [§0](#0-resolution). PR 2 did not cause this.** The failure is a
> pre-existing ~10% attribution loss: the 3s grace window is too tight against an audit
> delivery lag that is ~1s even on an idle cluster. Everything below §0 is the working
> record, including three conclusions that were later overturned; it is kept because the
> reversals are the useful part.
>
> The headline: a commit that should have been authored `jane@acme.com` was authored
> `GitOps Reverser`. Attribution was genuinely lost — this is not a test asserting the
> wrong branch.

## 0. Resolution

**The mechanism, measured.**

Attribution resolves a watch event against an audit fact, waiting up to
`DefaultAttributionGraceWindow` = **3 seconds**
([author_resolver.go](../../../internal/watch/author_resolver.go)). If no fact matches, the
commit is authored by the configured committer — silently, because
`git.DefaultCommitterName` ("GitOps Reverser") is *also* the configured-author identity.

Measured fact-delivery lag on an **idle** cluster (12 samples, ConfigMap creates with
`--as=jane@acme.com`, polling Valkey for the fact key):

~~~text
835  837  838  839  839  1092  1095  1096  1099  1099  1100  1101   (ms)
~~~

Bimodal at ~840ms and ~1100ms — the signature of the apiserver's
`--audit-webhook-batch-max-wait=1s` ([start-cluster.sh](../../../test/e2e/cluster/start-cluster.sh)).
So the 3s budget runs against a baseline lag of ~1s that is **dominated by a fixed batching
delay**, leaving roughly 2s of headroom for queueing. Under parallel load the tail crosses
3s and the actor is lost.

Resolver wait times by result confirm it:

| result | mean wait |
|---|---|
| `weak` | 0.001s |
| `exact_user` | 0.533s |
| `absent` | **2.268s** — polled the grace out and gave up |

**Not a key-shape problem.** Every ConfigMap uid in Valkey has *both* an exact-`rv` key and a
`:last` key (46 of 46), so facts are indexed correctly. They simply are not there yet.

**PR 2 is exonerated, by rate rather than by argument.** Attribution loss measured across
whole runs (~85 resolutions each, far more power than one spec's pass/fail):

| Code | absent | total | loss |
|---|---|---|---|
| PR 2 present | 7 | 88 | **8.0%** |
| PR 2 absent (`4f37759`) | 8 | 83 | **9.6%** |

Indistinguishable, with baseline marginally *worse*. Per-resource on baseline:
**configmaps = 5 absent / 49 = 10.0% loss** — the failing spec's own resource already loses
1 in 10 actors *without* PR 2. That makes the spec ~10%-flaky per run on its own, and the
2-fail-of-3 versus 0-fail-of-3 split that looked decisive is within the noise of that base
rate.

**What to fix.** The grace window, not the stream scoping:

1. Raise `DefaultAttributionGraceWindow` well above the batching delay (3s → ~10s). Cost: a
   write with genuinely no audit fact waits the full grace before committing.
2. And/or lower `--audit-webhook-batch-max-wait` in the recommended audit config, which
   attacks the dominant fixed term rather than padding around it.
3. Independently: **the fallback is silent.** `absent` is a counter nobody looks at. Losing
   the actor is a correctness failure for a product whose promise is naming the actor; it
   should be observable at the commit, not only in a metric.

Reusable diagnostic: [`hack/attribution-diagnostics.sh`](../../../hack/attribution-diagnostics.sh)
separates "fact never delivered" from "fact arrived too late".

## The failing spec, end to end

`Manager WatchRule ConfigMap and Secret >> should create Git commit when ConfigMap is added
via WatchRule`
([watchrule_configmap_secret_e2e_test.go:446](../../../test/e2e/watchrule_configmap_secret_e2e_test.go#L446)),
failing at [:581](../../../test/e2e/watchrule_configmap_secret_e2e_test.go#L581):

~~~text
Expected <string>: GitOps Reverser
to contain substring <string>: jane@acme.com
~~~

What the spec does:

1. Creates a `GitTarget` (`watchrule-configmap-test-dest`) and a `WatchRule` selecting
   ConfigMaps in the spec's own namespace, then waits for `Ready`.
2. Creates a ConfigMap **impersonating a user** — `kubectl --as=jane@acme.com`
   ([:491](../../../test/e2e/watchrule_configmap_secret_e2e_test.go#L491)). The
   impersonation is the whole point: the identity must survive into the Git commit.
3. `Eventually`-polls a local checkout of the Gitea repo until the ConfigMap's YAML exists,
   the commit message carries `[CREATE]` and `v1/configmaps/test-configmap`, and the commit
   **author** (`git log -1 --pretty=%an`) is the impersonated user.

The author assertion is branched
([:576-579](../../../test/e2e/watchrule_configmap_secret_e2e_test.go#L576-L579)):

~~~go
if configuredAuthorModeEnabled() {
    g.Expect(author).To(ContainSubstring("GitOps Reverser"))
} else {
    g.Expect(author).To(ContainSubstring("jane@acme.com"))
}
~~~

### How attribution is supposed to work

The controller runs in one of two modes.

- **configured-author mode** — no Redis / audit disabled. Every commit is authored by the
  configured committer identity. `cmd/main.go` announces it at startup
  ([main.go:274-277](../../../cmd/main.go#L274-L277)).
- **attribution mode** — the default here. The live watch event that produced the write is
  matched against an audit-webhook fact carrying the acting user, and that user becomes the
  commit author. When no fact matches, the commit falls back to
  `DefaultCommitterName = "GitOps Reverser"`
  ([internal/git/types.go:22](../../../internal/git/types.go#L22)).

**`GitOps Reverser` is therefore ambiguous.** It is both the configured-author identity and
the fallback when attribution finds nothing. The observed string alone does not say which
happened — which is exactly why the first reading of this failure was wrong.

### The mode probe is a log grep, and it fails open

`configuredAuthorModeEnabled()`
([e2e_suite_test.go:124-130](../../../test/e2e/e2e_suite_test.go#L124-L130)) decides which
branch applies by shelling out to `kubectl logs deployment/gitops-reverser --since=30m` and
grepping for `configured-author mode:`. It returns `false` on **any** kubectl error.

Two independent ways that misreports:

- The controller has been up longer than 30 minutes, so the startup banner ages out of the
  window.
- `kubectl logs deployment/...` errors or picks the wrong pod mid-rollout. The failing run's
  event dump shows two ReplicaSets scaled up at once, so a rollout was in progress.

Either makes the probe answer `false` — "attribution mode" — and demand `jane@acme.com`
regardless of how the controller is actually configured. It is also re-invoked on every
`Eventually` retry, which is why the failure timeline is full of repeated `kubectl logs`
calls.

**This was checked and is NOT what happened.** The local deployment emits no
`configured-author mode:` banner at all, so it genuinely runs in attribution mode, the probe
correctly returned `false`, and the spec correctly demanded `jane@acme.com`. The commit fell
back to the default committer, so **attribution was really lost**.

The probe is still a latent trap worth fixing independently — it can only ever fail in the
direction of demanding attribution that may not be configured.

## What was measured

| # | Code under test | Scope | Attribution spec | Notes |
|---|---|---|---|---|
| 1 | PR 2 branch | full suite, 56 specs | **FAILED** | also failed `playground` (see contamination below) |
| 2 | PR 2 branch | full suite, 56 specs | **FAILED** | clean cluster; 55 passed / 1 failed |
| 3 | `4f37759` — PR 2 code absent | `manager` subset, 43 specs | **passed** | 43 passed / 0 failed |
| 4 | `10a530a` — main, PR 2 present | `manager` subset | *running* | the controlled comparison |
| 5 | `10a530a` — main, PR 2 present | **CI**, all 6 sharded legs | **passed** | run 29734882468 |

Run 5 is the strongest evidence so far that PR 2 did not break attribution. The merged
change was validated on clean GitHub infrastructure by `release.yml` (whose first job is
`uses: ./.github/workflows/ci.yml`), and **every** e2e leg passed — including
`E2E (full-manager)`, the leg that contains this exact spec, since
`Manager WatchRule ConfigMap and Secret` carries the `manager` label.

That is not the same as proving the local failure is environmental. CI shards the suite
across six legs, so each leg runs at materially lower contention than one 56-spec local run
at `--procs=4` — which is precisely the load difference hypothesised below. CI passing is
consistent with both "PR 2 is innocent" and "the spec is load-sensitive and CI never applies
enough load". It rules out a deterministic regression; it does not rule out a latent race
that this change makes marginally easier to hit.

Run 4 is the one that discriminates, because it differs from run 3 in exactly one variable.
Runs 2 and 3 differ in **two** — the code *and* the suite scope/parallel load — so run 3
alone does not convict PR 2.

Why load plausibly matters: the assertion is an `Eventually` with a 30s budget, and
attribution requires an audit fact to arrive and match within a window. A 56-spec run at
`--procs=4` on a dev container is a materially different timing environment from a 43-spec
run.

## Why PR 2 was argued to be irrelevant — and why that argument was over-trusted

PR 2 replaced `SnapshotNamespaces()` with `WatchScopes()` and made both read sites project
one stream per namespace scope. The claim was that this is a **strict no-op** unless a single
`WatchedType` holds both the cluster-wide `""` key and a named-namespace key:

- only named keys → old returned the sorted names, new returns the same;
- only `""` → old returned `nil` and the caller synthesised `targetWatchKey{GVR, ""}`, new
  returns `[""]` and builds the identical key.

The failing spec creates one `GitTarget` with a single namespaced `WatchRule` and no
co-resident `ClusterWatchRule`, so it never reaches the changed branch.

The code reading still looks correct. The error was **procedural**: this argument was used
to explain away three successive failures without ever running the cheap control that would
test it. A structural argument about which branch executes is not evidence about a
timing-sensitive integration failure. The control costs ~6 minutes and should have been run
after the first failure, not the third.

## Confounders that invalidated earlier runs

- **Self-inflicted cluster contamination.** A targeted `commit-request`-only run left Gitea
  SSH keys registered; the next full run failed `playground` with
  `HTTP 422: Key title has been used`. Any verdict from that run is untrustworthy.
- **`clean-cluster` run from the wrong directory.** It does `rm -rf .stamps/cluster/<ctx>` on
  a *relative* path, so running it from `test/e2e` deleted nothing real while k3d still
  removed the cluster — leaving a stale `ready` stamp. `prepare-e2e` then skipped cluster
  creation and everything failed with `No nodes found for given cluster`.
- **Exit status masked by a pipe.** `task prepare-e2e | tail -15 && task test-e2e` takes
  `tail`'s exit status, so a failed prepare reported success and the suite ran against a
  cluster that did not exist. Use `set -o pipefail` or capture exit codes separately.

Net: of four runs, one was contaminated and two tested nothing.

## Not to be confused with

Three consecutive runs each failed a *different* timing-sensitive spec — `playground` +
`Commit Request Bundle (UC2)`, then `Commit Request generateName`, then this one. The
commit-request failures were separately re-run in isolation and **all 4 commit-request specs
passed**, which is why they were written off. That pattern is what made "this suite is flaky
here" attractive. It may still be true. It is not established.

## Open questions

1. **Does the `manager` subset fail on main?** (run 4). If yes, PR 2 is implicated and main
   is currently broken — revert first, diagnose second. If no, the variable is suite load.
2. **Does CI reproduce it?** `release.yml`'s first job is `uses: ./.github/workflows/ci.yml`,
   which has an `e2e` job, so the main build validates the merged PR 2 on clean
   infrastructure — an independent read.
3. **If it is load, where exactly is the window lost?** The audit fact must arrive and match
   before the write is authored. Worth instrumenting rather than inferring.
4. **Should the mode probe be fixed regardless?** It is a `--since=30m` log grep that fails
   open. A deterministic signal — a status field, or asserting the deployment's flags — would
   remove a whole class of misreads.

## If it turns out to be PR 2

The place to look first is the no-op claim, since that is the load-bearing assumption. Test
it directly: build the `WatchedTypeTable` this spec actually produces and assert
`targetWatchSpecs` output is byte-identical before and after the change. If some GitTarget in
that namespace acquires both a `""` and a named scope through a path not anticipated, the
extra stream would deliver the same object twice, and a duplicate delivery racing the audit
match is a plausible mechanism for losing the attributed author.
