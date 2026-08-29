# TODO

This file is meant to track the smaller current backlog, not historical notes.

## Current backlog

- [ ] **High availability (near-term priority).** Support running more than one controller replica;
  `replicaCount > 1` is currently hard-rejected by the chart. Redis/Valkey is already the shared store
  for watch resume cursors, command-author facts, and — since the fact-stream switchover — the
  per-`(route, group/resource)` attribution fact streams, whose independent cursors are the primitive
  HA needs for an audit POST and a watch shard that land on different replicas. The remaining
  work is leader/ownership coordination so two replicas never write the same `GitTarget`, plus the
  durable worker queue below, plus one small ordering call: whether a replica warms its fact index
  before starting a watch it has taken over. Redis becomes required (not just advised) in this mode.

- [ ] Optional: capture CommitRequest authors without Redis in single-pod mode.
  The admission webhook is on by default but no-ops author capture without Redis (it writes the author
  to Redis and the controller reads it back). Since both run in one process today (`replicaCount=1`),
  an in-memory `CommandAuthorStore` fallback would make capture work with no Redis. Now the *only*
  thing that still forces Redis on a single replica, since `--author-attribution-transport=memory`
  runs attribution in-process. Only useful until HA lands (HA needs the shared Redis store); low
  priority.

- [ ] Prevent same-repository write collisions across multiple `GitProvider` objects.
  Decide whether the fix should be validation, a shared queue/lock per repo, or both.
  Until then, keep recommending one `GitProvider` per repository.

- [ ] Re-assess unifying `PendingWriteAtomic` and `PendingWriteCommit` into one shape.
  Trade-off and complexity sketch in
  [docs/future/idea-unify-pending-write-kinds.md](future/idea-unify-pending-write-kinds.md).
  Currently deferred; worth revisiting if a second snapshot-style producer appears or
  the reconciler's fabricated `UserInfo` causes a bug.

- [ ] Improve queue and worker observability.
  Better metrics, queue visibility, and more high-load test coverage would help.

- [ ] Make the `BranchWorker` queue durable and move watch cursor advancement behind a durable
  worker acknowledgment.
  The current Redis watch cursor only remembers the last resourceVersion handed to the in-memory
  worker queue. That detects queue-full drops, but a controller crash after cursor advancement and
  before the queued write lands in Git can still skip work on restart. The intended direction is a
  durable worker queue/journal so replay, live events, and resyncs are acknowledged only after the
  write is recoverable. This is also the realistic boundary for Kubernetes watch history: the API
  server does not guarantee every old revision remains available long enough for us to rebuild from
  resourceVersion alone.

- [ ] Fix recurring full e2e flakiness around WatchRule/snapshot convergence.
  This has shown up more than once as timeout-based failures in manager SOPS bootstrap and
  signing snapshot-message specs, then passed on rerun. Capture and mitigation notes live in the
  [e2e serial registry](spec/e2e-serial-registry.md).
  This should be addressed before the next feature that expands commit-message, snapshot, or
  write-window behavior, otherwise new failures will be hard to separate from existing timing debt.

- [ ] Use [bestpractices.dev](https://www.bestpractices.dev/en) as a project maturity checklist.
  Review the current gaps, decide which items matter for this project, and turn the useful ones
  into concrete follow-up work instead of treating the badge as the goal.

- [ ] Filter more cluster-generated noise.
  Examples include Kubernetes-generated ConfigMaps such as `kube-root-ca.crt` and similar
  cluster-specific resources that do not belong in a portable Git view by default.

- [ ] Decide how SOPS rules should cover sensitive custom resources that are not Secret-shaped.
  The current bootstrapped `.sops.yaml` encrypts `data` and `stringData`, which fits Kubernetes
  Secrets and CozyStack `tenantsecrets`; resources with sensitive fields under shapes such as
  `spec.credentials` need an explicit field policy or full-file encryption decision.

- [ ] Revisit output layout. **Now designed and postponed, not open-ended, and the answer has
  reversed since [#293](https://github.com/ConfigButler/gitops-reverser/issues/293) was filed.** The
  path template **stays**; what it could not express becomes two additive members of
  `spec.placement`, `serializeNamespace` and `kustomizeRoot`. [layout/model.md](layout/model.md) carries the reversal
  and [layout/implementation-plan.md](layout/implementation-plan.md) the order. The placement work
  is no longer breaking, so it no longer needs
  [#294](https://github.com/ConfigButler/gitops-reverser/issues/294); the issues still describe the
  discriminated union and want updating.
  Deliberately **not** in 0.41.0, which already carries the new attribution model and the
  sibling-inference removal. Multiple resources per file is bundle support, which exists for
  match-first today and is a separate question from where a *new* file goes.

- [ ] Reduce duplication between `WatchRule` and `ClusterWatchRule` code paths where it makes sense.

- [ ] Collapse wildcard source-namespace stream fan-out.
  `WatchRule.spec.rules[].sourceNamespace: "*"` expands to one selection per admitted namespace, and
  `targetWatchSpecs` opens one stream per cell (one type in one named namespace, or one type
  cluster-wide) while `git.ResyncScope` names a single namespace, so a wildcard over N admitted
  namespaces and M matched types costs N×M informers and N×M resync scopes, where a cluster-wide
  ClusterWatchRule costs M. Expansion is deliberate — one
  stream per namespace is what keeps each mark-and-sweep bounded by exactly the slice it gathered —
  but the cost grows with tenant count. The direction is a cluster-wide stream whose resync scope
  carries a namespace **set** rather than one name, so the gather stays exactly as narrow while the
  stream count drops to M. Also revisit `WatchRuleStreamsStatus.PendingSample`, whose five-entry cap
  stops being representative at N×M.

- [ ] Subscribe the watch plane to `typeset.Registry` lifecycle events.
  `Registry.Subscribe` has **no production observer**: the events are computed on every `Update`
  and dispatched to nobody, because the Materializer that consumed them is gone. It is the producer
  of the only signal that can separate a type genuinely withdrawn (a settled `TypeRemoved`, past
  `RemovalGrace`) from a discovery wobble, and mistaking the second for the first deletes a user's
  manifests. Its intended consumer is the `stop` classification in
  [target-watch-plan.md](design/target-watch-plan.md), "What a cell leaving means": a settled
  removal drops its cell from the plan and the Git-side sweep converges the mirror under the
  target's existing `spec.prune.mode`. Nothing has to be decided first — only removal on *intent*
  waits on an open question. Tracked here so an unconsumed producer with a good comment on it does
  not quietly rot into dead code.

- [ ] Decide whether the watch-plane settle window should be configurable.
  `settleWindow` (2s) and `maxSettleWait` (10s) in [owner.go](../internal/watch/owner.go) are fixed,
  on the same reasoning `DefaultCommitWindow` gives one layer down: what a user cares about is how
  quickly their config takes effect, not how the controller batches its internal work. Revisit if a
  real deployment reports either that config changes feel slow or that a busy config plane replans
  too often — and note the observable consequence already shipped, that toggling a rule off and on
  *inside* the window is no longer a replay.

- [ ] Settle whether a retention report can be dropped on a revision mismatch.
  `MarkTargetRetention` discards any report whose revision does not match the scope's currently
  installed one, and `retainTargetRetentionScopes` deliberately KEEPS the previous count when a
  stream is restarted rather than zeroing a scope nobody re-measured — both in
  [retention_rollup.go](../internal/watch/retention_rollup.go). Together those produce a stale
  count with no notification involved: sweep succeeds, `status.retention.retainedDocuments` stays
  at its old value, and on a converged GitTarget the next correction is ~5 minutes away.

  That is exactly the signature of an intermittent `E2E (full-manager)` failure in the
  `prune_mode` spec "converges an existing orphan when `prune.mode` is widened"
  (`retainedDocuments` frozen at 1 for 30s while the files it counts had already been swept).
  A shared-event-channel fix landed for that failure and is the more likely cause, but this path
  is **independent of it** and would produce the same symptom, so a green run does not retire it.

  Full evidence, the controller-log timeline, and the diagnostics now armed for it are in
  [watch-plane-status-convergence-failures.md](design/watch-plane-status-convergence-failures.md)
  §3.

  **The question to answer:** widening `prune.mode` sets `force`, which classifies every cell as
  `restart` and issues fresh revisions, while the 30s periodic sweep also re-runs
  `retainTargetRetentionScopes` for every declared target. Can a pass landing *while a replay is
  in flight* install a revision that differs from the one the in-flight replay captured at start?
  If yes, that replay's report is dropped on arrival and the count stays stale until something
  else re-measures it. CI is slower than a dev machine, which would widen that window and explain
  why it has never reproduced locally.

- [ ] Re-enable the `goconst` linter with a path-scoped exclusion instead of the current repo-wide
  disable in [.golangci.yml](../.golangci.yml). Exempting `test/` and `internal/git/commit.go`
  would silence the existing noise (~45 findings, mostly test fixtures) while still catching
  genuine new string repetition.

- [ ] Preserve more user-facing file structure where feasible.
  Comments, ordering, and other low-noise formatting details are still easy to lose when rewriting
  manifests.

- [ ] Handle resources whose GVK cannot be resolved against the live cluster.
  A manifest may reference a `apiVersion`/`kind` whose CRD is not installed, so the RESTMapper
  cannot map it to a GVR. This is already a problem today and also blocks the manifest-inventory
  work in [docs/design/manifest/manifest-inventory-file-agnostic-placement.md](spec/manifest-system.md):
  indexing must record the manifest identity and defer rather than fail the whole scan.

## Future directions worth revisiting

- [ ] Simpler setup flows, including more Git provider bootstrap automation.

- [ ] Constrained reverse actions for simple, known Kustomize-style mutations.

- [ ] Better branching and promotion strategies.

- [ ] Bi-directional GitOps alignment with controllers such as Flux and Argo CD.

Research work:

- Replace metrics mechanism with
  <https://docs.victoriametrics.com/helm/victoria-metrics-operator/> (so that it's also helm and
  so that we can have proper deps)
- Read more on how resource versions work (and can work in the HA rebruild): <https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions>
