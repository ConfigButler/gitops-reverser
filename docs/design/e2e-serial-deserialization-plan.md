# E2E Serial de-serialization plan

> Status: **partly executed** (2026-06-02).
> Generated 2026-06-02 as the follow-up to section 3 of
> [refactors-branch-review.md](refactors-branch-review.md).
> Authoritative Serial list: [e2e-serial-registry.md](e2e-serial-registry.md).
>
> **Outcome so far:**
> - **Part A (`crd_lifecycle`)** — **done**: de-serialized and verified green
>   under `--procs=4`. See the registry's "De-serialized" section.
> - **Part B (audit pipeline)** — the heavy B1 producer-side refactor was
>   **declined** as planned, but turned out to be unnecessary. Re-reading the
>   specs showed their real serial cause was a shared *repo* (one `sync.Once`
>   Gitea repo + whole-branch `HEAD`/count assertions), not the shared audit
>   *pipeline* — namespace-scoped WatchRules already route each spec's events to
>   its own GitTarget. Giving each spec its **own** repo via `SetupRepo`
>   de-serialized both `Commit Window Batching` and `Commit Request` with no
>   production change. The redundant `Audit Redis Queue`/`Consumer` specs were
>   also retired and their unique OIDC-author assertion moved to the **parallel**
>   `commit_author_attribution_e2e_test.go`.
> - **`bi_directional`** — also de-serialized: it already owned its repo, and
>   finding #2's plan-hash-gated resnapshot fix removed the cluster-wide catalog
>   churn that produced its "+2 commits under parallelism".
>
> All de-serializations need a stability run under `E2E_GINKGO_PROCS=2` before the
> labels are trusted (see Part A step 3).

## Why this is not "just remove the `Serial` labels"

Every entry in the Serial registry is **correctness-driven (shared, cluster-wide
state), not flakiness-driven**. The race fixes that landed (`03b95ab`,
`0787b1b`) made the suite *stable*; they did not make the shared state
disappear. Removing a `Serial` label without first isolating the underlying
state re-introduces cross-spec bleed. So de-serialization is an isolation
*refactor*, gated on verification — not a cleanup.

Only 3 Serial containers are genuinely serial and stay:

- `Restart Snapshot Safety`, `image refresh dependency chain` — restart/reimage
  the singleton controller.
- `Aggregated API server` — installs/removes a cluster `APIService`; perturbs
  apiserver discovery for every client.

`Bi Directional` was previously listed here as "genuinely serial" on the theory
that any concurrent controller activity breaks its exact-count loop assertions.
That turned out to be too pessimistic: with its own repo and finding #2's
plan-hash-gated resnapshot fix, no concurrent spec writes to its `main` and no
catalog refresh perturbs its target, so it has been de-serialized. The two
audit-pipeline candidates below were also de-serialized — by per-spec repo
isolation, not the B1 refactor (see the outcome note at the top of this doc).

---

## Part A — `crd_lifecycle` (smaller; likely already unblocked)

### Current Serial cause

[crd_lifecycle_e2e_test.go:32](../../test/e2e/crd_lifecycle_e2e_test.go#L32) is
`Serial` because it installs/deletes a CRD and a (cluster-scoped)
`ClusterWatchRule`. Per the registry, that changes cluster-wide discovery /
the GVR catalog, which historically forced *unrelated* GitTargets to resnapshot;
the resulting `reconcile: sync …` commits hide other WatchRule specs' exact
`[CREATE]`/`[DELETE]` event-commit assertions.

### What already changed

1. **Finding #2 fix** ([manager.go:1191](../../internal/watch/manager.go#L1191)):
   a target is only re-snapshotted when its *resolved* plan hash changes, and a
   target whose rules currently resolve to nothing is kept as an empty plan
   rather than evicted. A transient discovery gap no longer makes a target
   vanish and resnapshot on recovery.
2. **Per-file CRD groups** ([icecream.go:30](../../test/e2e/icecream.go#L30)):
   `crd_lifecycle` owns the API group `crd-lifecycle.e2e.example.com`, so its
   `IceCreamOrder` CRD cannot collide by name with any other spec's CRD.

### Why it is probably safe now (to be verified, not assumed)

When `crd_lifecycle` installs its CRD, a *new* GVR becomes resolvable. A
concurrently-running spec would only be dragged into a resnapshot if it has a
rule that newly matches that GVR — i.e. a wildcard `ClusterWatchRule`. A scan of
the suite shows the only wildcard `ClusterWatchRule`
([restart/clusterwatchrule-wildcard.tmpl](../../test/e2e/templates/restart/clusterwatchrule-wildcard.tmpl))
belongs to `restart_snapshot`, which is itself `Serial` and therefore never runs
concurrently. `demo_e2e_test.go` also uses a `ClusterWatchRule` and must be
checked (step 1). With no concurrent wildcard matcher, the icecream CRD only
changes `crd_lifecycle`'s own (name-isolated) targets, and finding #2 keeps
every other target's plan hash stable across the catalog refresh.

### Plan

1. **Confirm no concurrent wildcard matcher.** Audit `demo_e2e_test.go`'s
   `ClusterWatchRule` scope (and any future broad rules). If it is scoped to
   specific groups/resources that exclude the icecream group, it is safe; if it
   is wildcard, either scope it down or keep `crd_lifecycle` Serial.
2. **Flip the label.** Drop `Serial` from
   [crd_lifecycle_e2e_test.go:32](../../test/e2e/crd_lifecycle_e2e_test.go#L32)
   (keep `Ordered`). Update the registry row.
3. **Verify under parallelism.** Run a `procs=2` (ideally higher) loop of the
   `manager`-labelled specs together, e.g. several repetitions of
   `task test-e2e` with `E2E_GINKGO_PROCS>=2`, and grep the WatchRule specs'
   commit messages for unexpected `reconcile: sync` commits interleaved with
   their `[CREATE]`/`[DELETE]` assertions. The gittarget-isolation spec already
   encodes exactly this assertion
   ([gittarget_isolation_e2e_test.go:140](../../test/e2e/gittarget_isolation_e2e_test.go#L140))
   and is the canary.
4. **If it bleeds**, do not force it. Capture the residual mechanism (likely a
   `RefreshAPIResourceCatalog` churn path that perturbs targets even without a
   matching rule) and fold it into the "settled-empty refinement" follow-up that
   finding #2 already defers.

### Effort / risk

**Small–medium, low blast radius.** Mostly a label flip plus a verification
loop; the supporting code fix already shipped. The realistic failure mode is
"verification shows residual bleed," in which case we learn the exact remaining
mechanism and stop — no production code is touched.

---

## Part B — the audit pipeline (historically 4 specs; reduced)

### Scope

This originally covered `Audit Redis Queue`, `Audit Redis Consumer`,
`Commit Window Batching`, and `Commit Request` — historically all labelled
`audit-redis`.
The queue/consumer containers have since been retired; the remaining
audit-consumer containers are `Commit Window Batching`
([commit_window_batching_e2e_test.go](../../test/e2e/commit_window_batching_e2e_test.go))
and `Commit Request`
([commit_request_e2e_test.go](../../test/e2e/commit_request_e2e_test.go)).
The unique author-attribution assertion moved to the parallel
`Commit Author Attribution`
([commit_author_attribution_e2e_test.go](../../test/e2e/commit_author_attribution_e2e_test.go))
container.

### Current Serial cause (and the real shape of the problem)

The audit pipeline is a **cluster-wide singleton firehose**:

```
apiserver audit policy → ONE audit webhook (internal/webhook/audit_handler.go:496)
   → ONE Redis stream (cfg.auditRedisStream, default)
   → ONE consumer group "gitopsreverser-consumer" (redis_audit_consumer.go:53)
   → routes each event to GitTargets by matching WatchRules → commits
```

These specs assert commit **exclusivity** ("my commit touched only my file";
commit-window batching asserts a batch contains only its own events). The
pollution source is *not just the other 3 audit specs* — it is **any** parallel
spec that writes a watched resource, because every such write produces an audit
event that flows through the same stream/consumer and can land in the same
batched commit. That is why they are fully `Serial`, not merely serial among
themselves.

### Why the cheap approaches do not work

- **Per-test stream name via config flip:** the apiserver audit policy targets a
  single webhook endpoint on the singleton controller; tests cannot make the
  apiserver route their events to a different stream. The stream name is a
  *deploy-time* config, not a per-request property.
- **Per-test Redis consumer groups:** Redis consumer groups *fan out* — every
  group receives every message. A new group sees all tests' events, so it
  isolates nothing.
- **Namespace-scoped assertions only:** scoping each spec's "touched only my
  file" assertion to its own namespace helps the simple cases, but
  commit-window batching deliberately coalesces multiple events into one commit;
  concurrent events from other specs still share that batch window. Assertion
  scoping alone cannot fix the batching specs.

### Options

**B1 — Producer-side per-namespace (or per-target) stream routing.** Make the
webhook handler write to a stream keyed by the event's namespace/target instead
of one global stream, and have the consumer manage a group per active stream.
*Pro:* true isolation; the 4 specs go fully parallel. *Con:* significant change
to load-bearing production code (`audit_handler.go`, `redis_audit_queue.go`,
`redis_audit_consumer.go`), adds routing/lifecycle complexity that production
does **not** want (production is happy with one stream), and risks regressing
the very ordering/exclusivity guarantees the specs protect. Highest payoff,
highest risk.

**B2 — Test-only routing mode.** Same as B1 but gated behind a test-only flag so
production keeps the single stream. *Pro:* shields production semantics.
*Con:* a test-only code path through the audit hot path is a maintenance smell
and can drift from the real path it is meant to validate — partially defeating
the point of an e2e test.

**B3 — A dedicated "audit" lane: concurrent with the rest, serial among
themselves.** Keep the 4 specs mutually exclusive but let them run *alongside*
non-audit specs, with the non-audit specs taught not to pollute the audit
window. Concretely: (a) give the 4 audit specs a shared ordering/semaphore so
only one is "live" at a time, and (b) ensure no *other* parallel spec writes a
watched resource into a GitTarget the audit specs assert on — which in practice
means the audit specs must assert on a path/namespace no other spec writes, and
the consumer's commit for an audit spec must be scoped to that path.
*Pro:* no production change. *Con:* only works if the exclusivity assertions can
be made path-isolated despite shared batching — which is exactly the open
question; if batching coalesces a foreign event, the assertion still breaks.

### Recommendation

Do **not** attempt B1/B2 as part of routine speedup work. The audit pipeline's
singleton design is intentional, and bending it for test parallelism trades a
real production-correctness surface for a few minutes of CI time. If the audit
specs' runtime becomes a genuine bottleneck, pursue **B3 first** as a spike:
prove (or disprove) that the exclusivity assertions can be made path-isolated
under the shared consumer/batcher. Only if B3 is proven impossible *and* the
speedup is worth it should B1 be scheduled as its own design + review cycle.

### Effort / risk

**Large, high blast radius** for B1/B2 (touches the audit producer + consumer,
the most correctness-sensitive path in the system). **Medium** for the B3 spike,
but with a real chance the answer is "can't be isolated, leave Serial."

---

## Sequencing recommendation

1. **`crd_lifecycle` (Part A)** — do this first; it is mostly verification on top
   of a fix that already shipped, and it removes one Serial entry with low risk.
2. **Audit B3 spike (Part B)** — only if audit-spec runtime is a measured
   bottleneck; timebox it and accept "stays Serial" as a valid outcome.
3. **Audit B1** — only as a deliberately scheduled design cycle, never as a
   drive-by.

The `e2e-serial-registry.md` table is the source of truth; update the relevant
row in the same PR as any label change.
