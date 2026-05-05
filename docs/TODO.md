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

- [ ] Use [bestpractices.dev](https://www.bestpractices.dev/en) as a project maturity checklist.
  Review the current gaps, decide which items matter for this project, and turn the useful ones
  into concrete follow-up work instead of treating the badge as the goal.

- [ ] Filter more cluster-generated noise.
  Examples include Kubernetes-generated ConfigMaps such as `kube-root-ca.crt` and similar
  cluster-specific resources that do not belong in a portable Git view by default.

- [ ] Reduce sensitive data persisted in the audit queue.
  Redacting or minimizing `payload_json` before it lands in Valkey would shrink the blast radius,
  especially for Secret-bearing audit events.

- [ ] Revisit output layout.
  Think about better control over target folders and whether some use cases should support multiple
  resources per file.

- [ ] Reduce duplication between `WatchRule` and `ClusterWatchRule` code paths where it makes sense.

- [ ] Preserve more user-facing file structure where feasible.
  Comments, ordering, and other low-noise formatting details are still easy to lose when rewriting
  manifests.

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
