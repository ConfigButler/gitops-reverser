# TODO

This file is meant to track the smaller current backlog, not historical notes.

## Current backlog

- [ ] **High availability (near-term priority).** Support running more than one controller replica;
  `replicaCount > 1` is currently hard-rejected by the chart. Redis/Valkey is already the shared store
  for watch resume cursors and command-author facts, so the data plane is HA-oriented. The remaining
  work is leader/ownership coordination so two replicas never write the same `GitTarget`, plus the
  durable worker queue below. Redis becomes required (not just advised) in this mode.

- [ ] Optional: capture CommitRequest authors without Redis in single-pod mode.
  The admission webhook is on by default but no-ops author capture without Redis (it writes the author
  to Redis and the controller reads it back). Since both run in one process today (`replicaCount=1`),
  an in-memory `CommandAuthorStore` fallback would make capture work with no Redis. Only useful until
  HA lands (HA needs the shared Redis store); low priority.

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

- [ ] Revisit output layout.
  Think about better control over target folders and whether some use cases should support multiple
  resources per file.

- [ ] Reduce duplication between `WatchRule` and `ClusterWatchRule` code paths where it makes sense.

- [ ] Collapse wildcard source-namespace stream fan-out.
  `WatchRule.spec.rules[].sourceNamespace: "*"` expands to one selection per admitted namespace, and
  `targetWatchSpecs` opens one stream per `(GVR, namespace)` while `git.ResyncScope` names a single
  namespace — so a wildcard over N admitted namespaces and M matched types costs N×M informers and
  N×M resync scopes, where a cluster-wide ClusterWatchRule costs M. Expansion is deliberate (it is
  what keeps the mark-and-sweep scope narrow, per
  [pr1-namespace-scoped-resync.md](design/watchrule-source-namespace/pr1-namespace-scoped-resync.md)),
  but the cost grows with tenant count. The direction is a cluster-wide stream whose resync scope
  carries a namespace **set** rather than one name, so the gather stays exactly as narrow while the
  stream count drops to M. Also revisit `WatchRuleStreamsStatus.PendingSample`, whose five-entry cap
  stops being representative at N×M. Context:
  [pr4-cluster-scope-only.md § wildcard fan-out](design/watchrule-source-namespace/pr4-cluster-scope-only.md#7-wildcard-fan-out-is-an-accepted-cost).

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
