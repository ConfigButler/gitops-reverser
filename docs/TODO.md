# TODO

This file is meant to track the smaller current backlog, not historical notes.

## Current backlog

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

- [ ] Fix recurring full e2e flakiness around WatchRule/snapshot convergence.
  This has shown up more than once as timeout-based failures in manager SOPS bootstrap and
  signing snapshot-message specs, then passed on rerun. Capture and mitigation notes live in
  [docs/design/e2e-watchrule-cross-spec-interference.md](design/e2e-watchrule-cross-spec-interference.md).
  This should be addressed before the next feature that expands commit-message, snapshot, or
  write-window behavior, otherwise new failures will be hard to separate from existing timing debt.

- [ ] Make `Manager CRD Lifecycle` e2e (`test/e2e/crd_lifecycle_e2e_test.go`) parallel-safe
  and drop its `Serial` marker. It was marked `Serial` after the initial CRD snapshot/resync
  flaked when running concurrently with other specs that create/delete CRDs (e.g. the
  bi-directional suite), under a transient apiserver RBAC/list window. The spec passes
  reliably in isolation; the goal is to make CRD mirroring robust to cluster-wide CRD churn
  so this can run in parallel again. Likely the same root cause as the WatchRule/snapshot
  convergence flakiness above.

- [ ] Use [bestpractices.dev](https://www.bestpractices.dev/en) as a project maturity checklist.
  Review the current gaps, decide which items matter for this project, and turn the useful ones
  into concrete follow-up work instead of treating the badge as the goal.

- [ ] Filter more cluster-generated noise.
  Examples include Kubernetes-generated ConfigMaps such as `kube-root-ca.crt` and similar
  cluster-specific resources that do not belong in a portable Git view by default.

- [ ] Reduce sensitive data persisted in the audit queue.
  Redacting or minimizing `payload_json` before it lands in Valkey would shrink the blast radius,
  especially for Secret-bearing audit events.

- [ ] Decide how SOPS rules should cover sensitive custom resources that are not Secret-shaped.
  The current bootstrapped `.sops.yaml` encrypts `data` and `stringData`, which fits Kubernetes
  Secrets and CozyStack `tenantsecrets`; resources with sensitive fields under shapes such as
  `spec.credentials` need an explicit field policy or full-file encryption decision.

- [ ] Revisit output layout.
  Think about better control over target folders and whether some use cases should support multiple
  resources per file.

- [ ] Reduce duplication between `WatchRule` and `ClusterWatchRule` code paths where it makes sense.

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
  work in [docs/design/manifest/manifest-inventory-file-agnostic-placement.md](design/manifest/manifest-inventory-file-agnostic-placement.md):
  indexing must record the manifest identity and defer rather than fail the whole scan.

- [ ] Resolve the unused `GitTarget.status.lastCommit` field.
  It is documented as "the SHA of the last commit processed" but is never populated — the only
  writer blanks it in [gittarget_controller.go](../internal/controller/gittarget_controller.go),
  and the GitTarget tests assert it stays empty. Either wire it up from the branch worker (and add
  a printer column, matching the `SHA` column `CommitRequest` now has) or drop the dead field.

## Future directions worth revisiting

- [ ] Simpler setup flows, including more Git provider bootstrap automation.

- [ ] Implement real `GitTarget.spec.providerRef` support for Flux `GitRepository`.
  The API shape already allows it, but the controller path is still effectively GitProvider-first.
  That would make the "bring your own Flux repo" story real instead of only partially modeled.

- [ ] A mode that commits changes without end-user author attribution, using the watch/reconcile
  path instead of kube-apiserver audit integration.
  This should stay explicitly framed as a simpler but lower-fidelity mode, not as equivalent to the
  audit-backed path.

- [ ] Constrained reverse actions for simple, known Kustomize-style mutations.

- [ ] Better branching and promotion strategies.

- [ ] Bi-directional GitOps alignment with controllers such as Flux and Argo CD.

- [ ] End-user supplied commit messages for UI-driven workflows.
  Prototype audit-carried options such as `user.extra` enrichment and transient metadata stripped by
  admission before committing to an aggregated API or CRD. Notes in
  [docs/future/idea-end-user-commit-messages.md](future/idea-end-user-commit-messages.md).

Research work:

* Replace metrics mechanism with https://docs.victoriametrics.com/helm/victoria-metrics-operator/ (so that it's also helm and so that we can have proper deps)
* Read more on how resource versions work (and can work in the HA rebruild): https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions
