# TODO

This file is meant to track the smaller current backlog, not historical notes.

## Current backlog

- [ ] Prevent same-repository write collisions across multiple `GitProvider` objects.
  Decide whether the fix should be validation, a shared queue/lock per repo, or both.
  Until then, keep recommending one `GitProvider` per repository.

- [ ] Batch related edits into fewer commits.
  Explore a configurable grouping model based on actor, inactivity window, and resource scope.

- [ ] Improve queue and worker observability.
  Better metrics, queue visibility, and more high-load test coverage would help.

- [ ] Use [bestpractices.dev](https://www.bestpractices.dev/en) as a project maturity checklist.
  Review the current gaps, decide which items matter for this project, and turn the useful ones
  into concrete follow-up work instead of treating the badge as the goal.

- [ ] Filter more cluster-generated noise.
  Examples include Kubernetes-generated ConfigMaps such as `kube-root-ca.crt` and similar
  cluster-specific resources that do not belong in a portable Git view by default.

- [ ] Revisit output layout.
  Think about better control over target folders and whether some use cases should support multiple
  resources per file.

- [ ] Reduce duplication between `WatchRule` and `ClusterWatchRule` code paths where it makes sense.

- [ ] Preserve more user-facing file structure where feasible.
  Comments, ordering, and other low-noise formatting details are still easy to lose when rewriting
  manifests.

## Future directions worth revisiting

- [ ] Simpler setup flows, including more Git provider bootstrap automation.

- [ ] A mode that commits changes without end-user author attribution, using the watch/reconcile
  path instead of kube-apiserver audit integration.

- [ ] Constrained reverse actions for simple, known Kustomize-style mutations.

- [ ] Better branching and promotion strategies.

- [ ] Bi-directional GitOps alignment with controllers such as Flux and Argo CD.
