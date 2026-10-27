# Misconceptions we corrected

Between July and October 2026 the project held a number of beliefs that turned out to be false:
about its own code, about Kubernetes, and about Flux, Argo CD, kustomize and go-git. Most of them
sounded right, several shipped, and a few were documented as intended behavior. Applying Git back
to the cluster (the bi-directional mode) only works reliably since #419, #425 and #443, and most of
the hard lessons of the last months are about that mode.

This page collects them, so a claim is checked here before it is raised again. Each entry gives the
belief, what is true, what it broke or nearly broke, and where it was corrected. Many of the design
pages that recorded these steps have since been deleted; `git log` keeps them, and the references
below point at what still exists: pull requests, commits, specs and current design pages.

Two habits caused more of these than any single gap in knowledge. They are listed first.

## How we investigated

- **A green run supports a fix.** Two hypotheses for PR #315's status failure were implemented and
  withdrawn. One fix passed its spec once while contradicting the logs, and would have swept under
  `always` after a user tightened the prune mode. *True:* a pass on a flaky spec proves nothing; the
  diagnostic has to fire, and the logs have to agree. Recorded in
  [watch-plane-status-convergence-failures.md](design/watch-plane-status-convergence-failures.md).
- **Several failing specs are several flakes.** Five failing e2e specs were one bug: a deleting
  `GitTarget` flooded the shared 100-slot queue with 595 resyncs in 16 seconds. Fixed in #312.
- **A theory built from code reading is good enough.** The Argo CD e2e failure was read as a push
  race, then as a stale cache, and a workaround and a doc rewrite were built on that. The controller
  log showed the API server rejecting an enum value. *True:* read the applier's and the controller's
  logs before theorizing.
- **"Flux does it this way", from memory.** Twice it was the opposite. Flux is also internally
  inconsistent (its enums are not one convention). *True:* check `external-sources/flux`, see
  [definitions.md](definitions.md).
- **`task test-e2e 2>&1 | tail -N` shows whether the suite passed.** It reports `tail`'s exit code,
  so a failing suite read as green.
- **A label filter narrows the default e2e set, and a zero-match run proves something.** The filter
  replaces the default, and a run that matches nothing skips `BeforeSuite` and reports green.

## Applying Git back (bi-directional)

- **Forwarding only foreign push webhooks controls when the applier applies.** Argo CD's timed
  refresh syncs every new tip two to three minutes after a push, even with `selfHeal` off, and Flux
  applies on every interval. Every timer is a trigger. *Cost:* the applier reverted edits GitOps
  Reverser had not published yet. *Fixed:* GitOps Reverser makes every apply request itself, and the
  applier's own triggers are held off (#419, [apply-trigger.md](design/apply-trigger.md)).
- **A 24h Flux interval amounts to exclusive control.** Flux still applies on a kustomize-controller
  restart, an apply retry, a spec change, and every new `GitRepository` artifact, including one
  another consumer fetched. *Fixed:* the `FluxClaim` keeps the Kustomization suspended and resumes
  it once per apply (#419).
- **A PreSync hook, or the self-heal timeout and backoff, can delay Argo CD's revert.** A sync's
  manifests are frozen when the operation starts, a hook cannot tell a heal from a deploy, and the
  timeouts throttle only repeats, so the first revert is immediate. *Fixed:* automated sync off,
  and GitOps Reverser starts every sync
  ([argocd-bi-directional.md](design/support-boundary/argocd-bi-directional.md)).
- **Holding Argo CD `selfHeal` off is needed to stop reverts.** `selfHeal` is a step inside
  automated sync and never runs while automated sync is off. Forcing it would lose the user's value
  on hand-back. Shipped as "leave it alone" in #437.
- **Argo CD prunes an undeclared `automated.enabled` on versions before 3.1.** The API server
  refuses the server-side apply with a 500, "field not declared in schema". *Fixed:* a dry-run gate
  and a refusal reason (#437).
- **An apply must wait for a quiet branch** (no open window, no pending write). The wait starved:
  someone who kept editing held a Git-side change back for as long as they typed. *Fixed:* the flush.
  The worker closes the window, takes the flush mark, publishes up to the mark and sends the apply.
  An edit after the mark is recorded, and an apply that overwrites it is withheld and repaired (#425,
  [S1 in applier-echoes.md](design/applier-echoes.md#s1-flush-then-apply)).
- **Closing the commit window on a push notice is about latency.** It is discovery. With a window
  open the worker reads nothing from the remote first, so publishing is how a busy branch learns
  that the branch moved at all ([concurrent-author-windows.md](design/concurrent-author-windows.md)).
- **kustomize-controller's `lastAttemptedRevision` means the apply has happened.** It is written
  before the build, about 0.8 seconds early per 200 objects. An artifact holding the commit, or a
  five-minute timer, proves nothing either. *Cost:* the apply window closed early. *Fixed:* applies
  are observed operations, and one past its budget is `Unresolved` but still followed (#419).
- **Closing the apply window ten seconds after the applier reports done catches its writes.** On a
  cluster with no audit stream every event waits the full attribution grace, one after another, so
  the applier's writes arrived after the window, were mirrored as edits, and reverted the newer API
  edit. *Fixed:* the window closes on watch progress, with the mark taken as the newest
  `resourceVersion` among the collection's objects, never the list's
  ([review](review/20261026-apply-window-late-events.md), `internal/git/apply_barrier.go`).
- **The newest `managedFields` entries being the applier's alone proves the applier wrote.** An
  update that removes every field a manager owns deletes that manager's entry, so an older
  kustomize-controller entry ends up newest. *Cost:* a `kubectl label x tmp-` undo was withheld, and
  `Repair` put `tmp=1` back in the cluster and in Git. Fixed in #420 (519b483e).
- **An applier write in the same second as a recorded edit is ambiguous, so mirror it.** Events
  arrive in `resourceVersion` order, so it is the apply over the edit. Mirroring it reverted the edit
  (caught by e2e E6, #425).
- **Flux is immune to the applier-echo push race.** It passed only because its echo arrived after
  the five-second push cooldown; Argo CD failed every time. A stale pending decision was ordered
  after a Git-side push, and the replay re-planned it, reverting the push. Fixed in #425.
- **Taking over `spec.suspend` from `flux suspend` with server-side apply makes us its sole owner.**
  A forced apply of the same value co-owns the field, so the release's empty apply left the
  Kustomization suspended while the Event said it resumed. The Flux simulator had modeled one owner
  per field. Fixed in #420, measured in envtest.
- **A history entry newer than our request time proves a recovered Argo CD sync ran.** History times
  use the controller's clock, so skew lets an older sync pass. *Fixed:* a recovered sync is repeated
  when no entry proves it (#443).
- **An unanswered Argo CD update can be timed out after two minutes.** That assumed the remote API
  server's default request timeout, which nothing establishes. Replaced by a `resourceVersion`-guarded
  refresh write that makes the lost update impossible to land (#420).
- **Argo CD only writes as `kubectl-client-side-apply`.** `argocd-controller` is the field manager
  for client-side and server-side apply, `Replace=true` writes with an empty manager, and a retrying
  failed operation is `Running` with `finishedAt` set (#419).
- **A newly bound Argo CD `Application` must not converge, and Argo CD needs no `CatchUp`.** Reversed
  on 2026-10-20: Argo CD catches up and converges on first naming, the same as Flux (S2 in
  [applier-echoes.md](design/applier-echoes.md)).
- **Each tool works out which of its objects syncs a changed folder.** Neither does. Argo CD
  refreshes every `Application` with a matching `repoURL` and narrows only on a hand-written
  `manifest-generate-paths` annotation; a Flux `Receiver`'s resource list is hand-written. This ruled
  out "interpret ownership, then patch the orchestrator" (#383).
- **The sanitizer keeps GitOps-tool bookkeeping out of Git.** It stripped Flux keys and no Argo CD
  key, so `argocd.argoproj.io/tracking-id` was committed. Argo CD stamps it on every non-CRD manifest
  and never validates it against the object, and a foreign tracking-id fails another `Application`'s
  sync ("Shared resource found"). *Fixed:* an exact-key deny-list, because `sync-wave` and friends are
  user intent (`internal/sanitize/types.go`). Flux's `config.kubernetes.io/origin` joined it in #455.
- **Argo CD reconciles every 180 seconds and tracks by label.** The defaults are 120 seconds plus up
  to 60 seconds of jitter, and annotation tracking
  ([e2e-bi-directional-corner.md](spec/e2e-bi-directional-corner.md)).
- **Filtering writes by identity or field manager is needed to stop the echo of our own commit.**
  The echo ends by itself. gitops-api's filter caused a two-and-a-half-minute flip-flop with every
  commit unattributed. Withdrawn on 2026-07-14
  ([gitops-api-open-asks.md](future/gitops-api-open-asks.md)).
- **A live edit that meets a pending render-fidelity gate is safely dropped, because the replay
  covers it.** The scope was marked clean on a different goroutine from the worker's answer, so
  edits in that gap were lost for good, and
  [api-first-publication.md](api-first-publication.md) documented the drop as intended. *Cost:* the
  audit-route-override flake, nine CI failures. Fixed in #420 and Fix 2a
  ([live-edit-dropped-while-render-fidelity-pending.md](design/live-edit-dropped-while-render-fidelity-pending.md)).

## Attribution

- **A removal may be credited to the object's last writer.** That named whoever last edited the
  object as the person who deleted it. Removals wait for delete evidence, with no last-writer
  fallback, since 2026-10-22 (#431, [spec/attribution.md](spec/attribution.md)).
- **A `DELETED` frame resolves on the write at its exact (uid, `resourceVersion`).** A deletion
  never carries that `resourceVersion`: an immediate delete answers with a Status holding only the
  uid, and a finalizer-held one is stamped one step earlier. Every deletion waited out the grace, so a
  Flux prune of N objects stalled the stream for N times the grace (the E12 `NoWindow` failure). Two
  earlier theories were wrong. Fixed in #431 (a52caca2), which credits a removal by its cause.
- **The deleter's fact outranks the finalizer-clearing controller's.** It is replaced: both writes
  return the same `resourceVersion`, land on one (uid, rv) key, and the last one wins, so the commit
  named the controller. Fixed in #289 (fcdc3bcd).
- **Collection-delete tiers rank below `latest`.** `latest` answers "who last wrote"; a removal asks
  "who deleted". Ranking above the collection's uid set credited every collection removal to the last
  editor. Corrected during #283 to #287
  ([attribution-fact-stream.md](finished/attribution-fact-stream.md)).
- **Attribution tiers are tried in sequence, each with its own wait.** There is one wait, and every
  tier is evaluated again on every wake
  ([attribution-removal-wait-options.md](design/attribution-removal-wait-options.md)).
- **A `CommitRequest`'s author can be looked up by its current `resourceVersion`.** A label patch
  before the claim named whoever patched it. *Fixed:* a creation query (#459). A no-op patch then
  returned the create's own (uid, rv) and replaced the creator, so creation facts got their own slot,
  `factKindCreation` (#460).
- **`Metadata`-level audit is enough for `CommitRequest`s.** The uid and `resourceVersion` come only
  from `responseObject`, and server-side apply creates with a patch, so both create and patch need
  `RequestResponse` (#460).
- **The admission `userInfo` on the control cluster is the audit user on the source cluster.** It
  matched only because gitops-api impersonates the Dex email; for anyone else it was a coincidence.
  Admission and takeover were removed in #456
  ([review](review/20261027-save-request-simplification.md)).
- **A matched fact that names nobody is a miss for the audit-route signal.** It proves the route is
  alive; the observation was wrong in both directions. Fixed in #341 (0e38cf48).
- **`unknown_route` is a possible audit rejection.** Any named route is accepted as a partition name;
  the only route rejection is the bare endpoint, `bare_endpoint_disabled`, and configuration.md had
  said the opposite ([metrics-observability-plan.md](design/metrics-observability-plan.md)).
- **Audit ingress is authenticated per provider.** It is not, yet: one shared client certificate
  routes any `/audit-webhook/<name>`. Still open, in
  [multi-source-audit-ingress-hardening.md](design/multi-source-audit-ingress-hardening.md).

## Commit windows, saves and status

- **`closeDelay: 0` is a tight but valid default.** It was shorter than the time a window takes to
  open, because admission was the fast path and the edit's audit join the slow one, so every save
  resolved `NoWindowInGrace`. The default became two seconds, and the race went away with admission
  in #456.
- **A save can wait on the attribution fact index.** A fact arrives before the watch event, so such a
  wait fires before the window opens, and it cannot see deduplication, render fidelity or the queue.
- **`windowPathKey` is the Git path after placement.** It is the `GitTarget` path joined with the
  generated file name; the write plan resolves the destination only at commit
  ([concurrent-author-windows.md](design/concurrent-author-windows.md)).
- **The push cooldown is inert now that the commit window batches.** Under alternating authors an
  identity change closes the window, the silence timer never fires, and the cooldown is the only
  batching left. The earlier "remove it" was retracted ([push-cooldown.md](design/push-cooldown.md)).
- **An idle apply follow ends when the worker's record ages out.** That happened only when another
  save resolved, so long applies were reported `Lost` and quiet workers followed forever. *Fixed:* an
  explicit `SaveApplyTimeout` (#460).
- **Commit shaping belongs on `GitProvider`.** A branch worker serves every `GitTarget` on one
  provider and branch, so those targets could not disagree; the setting describes a folder. Moved to
  `GitTarget.spec.commit` in #330.
- **The status write that beat us re-enqueued us.** The winning write is status-only, and every
  `For()` uses `GenerationChangedPredicate`, which filters it out. A lost write left a stale `Ready`
  for five minutes, 74 times per run. Fixed in #315.
- **Our own push is not news for `status.remote`.** A new revision always writes status, and
  observation freshness is not checkout readiness: keying on trust alone sent never-published
  targets to Git on every reconcile (#388).
- **A delayed requeue (`AddAfter`) always fires.** controller-runtime v0.25's priority queue folds it
  into an item already queued, so a foreign push reached `status.remote` a whole five-minute tick
  late. Fixed in #424 (3f00e098).
- **Duration fields accept what they emit.** `0.5ms` round-trips as `500µs`, which the Flux-style
  pattern rejected, so the object could never be updated again; CEL `self == oldSelf` also compared
  spellings (`1m` against `1m0s`). Fixed in #388.
- **The `StreamsReady` condition should be absent until it knows.** `kubectl wait` errors on an
  absent condition, so it exists from the first reconcile as `Unknown`
  ([streaming-readiness-status-machine-design.md](finished/streaming-readiness-status-machine-design.md)).
- **`ReasonRefusedStructural` is a permanent boundary.** It carries `Solvable`, and a consumer
  shipped "can never be synced" for a typo
  ([open-asks-priority.md](design/open-asks-priority.md)).

## Watches and the watch plane

- **A watch ending is a failure.** The API server ends every watch on a randomized timeout. Grading
  it as `WatchError` flipped `Ready` and emitted a Warning about every 40 minutes per type. Fixed in
  #363 (d0601a59).
- **A watch stream may be parented to the plan pass's context.** The pass deadline canceled every
  stream once the plan was applied, while the logs looked healthy and readiness sat at `Replaying`.
  A stream's parent is the manager's lifetime
  ([watch-manager-ownership.md](design/watch-manager-ownership.md)).
- **A per-pass deadline gives failure isolation.** Isolation came from taking network I/O off the
  owner loop; a first cut let one unreachable cluster stall every healthy target. A uid-less delete
  also matched every incarnation and could tear down a same-name successor (same page).
- **Never debounce the first `GitTarget` declaration.** Declaring before the rules arrive builds an
  empty plan, and that empty plan cleared a write divergence in the fidelity gate. Replaced by a
  two-second rolling settle window (same page).
- **A per-`GitTarget` latch shows the initial snapshot is done.** A target declared before its rule's
  collection was planned counted as done, its initial snapshot was thrown away, and it deadlocked
  against the converge. Tracked per collection since #425.
- **A resync payload can be replaced by a bare dirty set.** The snapshot and its position in the
  queue define the snapshot-then-deltas boundary; without it, mark-and-sweep reverts edits
  ([target-watch-plan.md](design/target-watch-plan.md) keeps the fence).
- **A cluster-wide collection can absorb a named-namespace one of the same type.** Collapsing them
  widened the named rule's stream to every namespace the credential could read. They are peers, and
  the newer rule is refused ([source-scope-simplification.md](design/source-scope-simplification.md)).
- **Legacy discovery calls are bounded.** client-go's `ServerGroupsAndResources()` runs with no
  deadline.
- **A controller `ownerReference` marks a derived object.** Measured against live controllers, it
  catches one producer in five: objects a Flux `HelmRelease` renders or a flux-operator `ResourceSet`
  expands carry none. The `*.toolkit.fluxcd.io/` labels also give opposite verdicts
  ([expansion-provenance-markers.md](facts/expansion-provenance-markers.md)).

## Git, go-git and Git hosts

- **A `file://` remote is a faithful test server.** go-git v6's in-process receive-pack never
  compares the old value, so every racing push won and the compare-and-swap tests were vacuous. Tests
  use `startRealGitServer` (#297).
- **go-git v6 ignores ambient configuration, as v5 did.** It honors a global `commit.gpgSign` and
  refuses to commit without a signer, and it loads `known_hosts` whenever `HostKeyAlgorithms` is
  empty, even with a callback set. Every SSH remote would have failed in the distroless image; only
  e2e caught it. Fixed by `PinExplicitSigningPolicy` and `ssh.KeyAuth` (#297,
  [azure-devops-multi-ack.md](design/azure-devops-multi-ack.md)).
- **Azure DevOps accepts go-git, because a bare probe returned 200.** It returns 400 on any capability
  list without `multi_ack`, and v5 cannot parse the multi-ACK response. Flux escapes only because it
  never fetches. *Fixed:* go-git v6, instead of #292's bundled `git`
  ([azure-devops-multi-ack-requirement.md](facts/azure-devops-multi-ack-requirement.md)).
- **Context cancellation bounds go-git calls.** HTTP honors it only through `List` and
  `FetchContext`, and SSH ignores it past the dial, so handshakes hang
  (`internal/git/network_bound.go`, #413).
- **A head-of-cycle fetch with `Force: true` cleans a worktree a failed write left dirty.** Only for
  modified tracked files; new files and directories survive a hard reset. Fixed in #382 (9535d6a8).
- **go-git cannot learn an empty repository's default branch.** GitHub advertises it over protocol
  v2 (`unborn`), and go-git asks and then discards the reply. Status reports "unknown"; guessing
  `main` was rejected
  ([gittarget-parent-empty-repository.md](design/gittarget-parent-empty-repository.md)).
- **A receive-pack probe proves push access.** On Gitea any valid user with read access gets 200, so
  it proves only a valid credential (#429).
- **Gitea's `gpg.error.no_gpg_keys_found` means a GPG problem.** For SSH signatures it means no
  verified SSH key matched, and verification needs the web-only `verify_ssh` flow
  ([e2e-signing-followups.md](finished/e2e-signing-followups.md)).
- **Migrating to Forgejo is required to unpin Gitea.** Both moved to Go's `CrossOriginProtection`,
  which a plain Go client passes; deleting the `_csrf` scrape unpinned Gitea in place
  ([e2e-git-server-choice.md](design/e2e-git-server-choice.md)).

## Kustomize, rendering and placement

- **Our re-implemented transformers render what kustomize renders.** Three shipped bugs:
  `images[].name` is a regex over the whole image string (`ngin.` matches nginx), the replicas field
  spec includes `ReplicationController`, and image traversal rewrites OCI `volumes[].image.reference`
  but skips `ephemeralContainers`. A malformed `images[].name` also nil-dereferences inside kustomize.
  *Fixed:* ask kustomize instead of re-implementing it (#232, #233,
  [render-attribution.md](design/support-boundary/render-attribution.md)).
- **`alpha.config.kubernetes.io/transformations` records which transformers touched an object.** It
  records every transformer that ran, on every object of the build, with duplicates per `images:`
  entry (same page).
- **Rendered objects are valid unstructured with `int64` numbers.** Numbers come back as Go `int`,
  so `NestedInt64` reports not found and `DeepCopyJSON` panics (same page).
- **A structural "contains `${...}`" check fences off postBuild substitution.** Flux's own CRD
  carries `${var:=default}` literally in a description, and one such CRD refused the whole folder.
  The difference is visible only against the live object. Reverted on the #234 branch
  ([render-fidelity.md](design/support-boundary/render-fidelity.md)). Kustomize itself never emits
  `${...}` ([kustomize-never-emits-dollar-brace.md](facts/kustomize-never-emits-dollar-brace.md)).
- **Mirroring the live object into its source file is safe.** Under `labels:` or
  `commonAnnotations:` it wrote the build's output into the build's input, one commit per reconcile
  of an in-sync folder. Where live and render agree, the source keeps its bytes (#234, 8444cd00).
- **A hand-kept deny-list of kustomize fields is enough.** `vars` was tolerated, and `$(VAR)` sources
  got the substituted value mirrored back. The unsupported set is derived from kustomize's
  `Kustomization` struct (#229).
- **kustomize folds `patchesStrategicMerge` and `patchesJson6902` into `patches:`.** It does not; they
  refuse under their own names ([patch-authoring.md](design/support-boundary/patch-authoring.md)).
- **Kustomize means flat files beside one root, with no nesting.** Measured on v5.8.1: a root can
  list nested paths, nested roots work, nothing is picked up by glob, and an unlisted file renders
  nothing. The `spec.layout` redesign rested on the opposite and was reversed into two booleans
  (#319, [layout/model.md](layout/model.md)).
- **A created `kustomization.yaml` must carry `namespace:`.** A namespace-less root is an ordinary
  base; Flux `targetNamespace` and Argo CD `destination.namespace` supply the namespace, and what
  refused it was our own render-fidelity gate (#328,
  [created-root-namespace.md](design/created-root-namespace.md)).
- **Placement inference is safe when every candidate agrees.** `allSameDir` was true for a single
  directory, so a whole type collapsed into one multi-namespace file. Inference was deleted (#291).
- **Top-level `clusters/`, `apps/` and `infra/` identify a Flux fleet root.** That missed the
  canonical `infrastructure/` and matched unrelated repositories; the evidence is
  `flux-system/gotk-*.yaml`
  ([orchestrator-knowledge-boundary.md](design/support-boundary/orchestrator-knowledge-boundary.md)).
- **SOPS lets one document of a multi-document file be edited.** One data key and one file-wide MAC
  cover the file, and metadata is read from the first document only
  ([sops-single-file-no-multidoc.md](spec/sops-single-file-no-multidoc.md)).

## Kubernetes API behavior

- **Deleting a CRD field is pruned quietly and harmlessly.** Strict `kubectl apply` rejects the field
  loudly while controllers prune it silently, and removing `allowedSourceNamespaces` failed open,
  widening `"*"` to every readable namespace
  ([crd-upgrade-strategies.md](facts/crd-upgrade-strategies.md)).
- **`managedFields` separates declared values from schema defaults, and a map default is
  additive.** `managedFields` is bookkeeping, and a map default disappears once any key is set
  (#319).
- **Pinning the kubeconfig Secret to the operator namespace closes the confused deputy.** The deputy
  is data export through any `GitTarget` that references the provider, and the tenant never touches
  the Secret. Fixed with deny-by-default grants on the provider (#441,
  [provider-access-rules.md](finished/provider-access-rules.md)).

## Release, CI and e2e

- **`draft: true` in release-please is vestigial.** It is required, because immutable releases
  reject asset uploads after publish. The spurious whole-history PR came from drafts being
  invisible to the release lookup; fixed by splitting the invocations (#203).
- **Unparseable commits come from nested `* feat!:` squash lines.** The cause is a line-leading
  `word(a(b))`, or `BEGIN_COMMIT_OVERRIDE` mentioned in prose. #388's changelog was lost, so 0.50.0
  shipped three undocumented breaks (#403).
- **Several `BREAKING CHANGE:` footers in one squash all reach the changelog.** Only the first is
  kept; on #450 it described an undone intermediate state.
- **A `container:` job runs bash.** It runs dash, so `set -o pipefail` killed the 0.40.0 publish and
  stranded a draft release (#279).
- **Coverage dips mean missing tests.** go1.27's coverprofile charges each comment-separated block
  the function's whole statement count; a 17-fold inflation was observed.
- **A metric e2e check that skips an absent series, or asserts `increase() == 0`, proves the
  producer works.** One reason had no producer at all, and deleting every recording call left the
  suite green; `increase()` cannot see an event before the first sample
  ([proving-the-metric-surface.md](design/proving-the-metric-surface.md)).
- **Gauges published from inside a loop show that loop stalling.** Queue depth read 0 through the
  first stall, and oldest-dirty-age froze when the loop hung
  ([metrics-observability-plan.md](design/metrics-observability-plan.md)).
- **The local e2e suite covers the Helm chart.** `INSTALL_MODE=config-dir` never renders it, and a
  `{{ . | quote }}` in an argument leaked literal quotes and crash-looped the manager (#221).
- **`k3d image import` succeeding means the image is imported.** Exec polling misreads completion
  and node failures are only logged; fixed with `-m direct` and a retry (#185).
- **k3s DiskPressure means a full disk.** The percentage thresholds fire with gigabytes free on a
  shared runner; absolute thresholds since #187.
- **A zero-agent CI cluster behaves like the local three-agent one.** The node restart bounced
  CoreDNS, Valkey and the manager together, and DNS died on k3s 1.37. Mitigated in #415; the root
  cause is unproven ([k3s-1.37-dns-after-node-restart.md](ci/k3s-1.37-dns-after-node-restart.md)).
- **Name isolation made `crd_lifecycle` safe to run in parallel.** Cluster-wide CRD discovery churn
  made it flaky again, and it was serialized again
  ([e2e-serial-registry.md](spec/e2e-serial-registry.md)).
- **Gating the docs lint on the Git diff is safe.** A shallow CI checkout made the merge base
  unreachable, the file list came back empty, and the gate passed; it reads a committed list,
  `.docs-lint-scope` ([docs-linting.md](design/docs-linting.md)).
