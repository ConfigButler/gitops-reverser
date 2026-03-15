# Bi-Directional E2E Plan

## Goal

Prove whether GitOps Reverser can safely coexist with a normal GitOps loop for the same resources without
creating a commit/reconcile loop.

The scenario uses:

- FluxCD for the forward GitOps flow
- Gitea as the Git remote
- the existing `IceCreamOrder` CRD and resource fixtures already used in e2e tests
- GitOps Reverser watching the same resources from the start

## Desired Test Shape

Create a new dedicated e2e spec alongside the existing suite:

- new file in `test/e2e/`
- separate Ginkgo label, for example `bi-directional`
- separate Make target: `make test-e2e-bi`

This test should reuse the existing suite preparation from `test/e2e/e2e_suite_test.go`:

- cluster preparation
- controller installation
- Gitea bootstrap
- repo checkout export
- Flux availability in the cluster

The scenario should use its own repo content and unique object names so it remains isolated from the existing
e2e tests.

## High-Level Flow

Start with normal GitOps:

1. Commit the `IceCreamOrder` CRD into the test repo.
2. Commit one `IceCreamOrder` instance into the test repo.
3. Configure Flux to sync that repo path into the cluster.
4. Verify Flux applies the CRD and the first `IceCreamOrder`.

Create a second normal GitOps commit:

5. Update the first `IceCreamOrder`.
6. Add a second `IceCreamOrder`.
7. Verify Flux reconciles both changes successfully.

Start the reverse direction:

8. From the beginning of the scenario, GitOps Reverser is already configured with `GitProvider`, `GitTarget`,
   and `WatchRule` for the same `IceCreamOrder` resources and the same repo path.
9. Change one `IceCreamOrder` through the Kubernetes API.
10. Verify GitOps Reverser creates a commit in Git for that live-cluster change.
11. Verify Flux observes the new revision and reports success.
12. Verify there is no further drift and no repeating commit loop.

## Detailed Plan

### 1. Add a dedicated e2e spec

Create a new file:

- `test/e2e/bi_directional_e2e_test.go`

Characteristics:

- use `Ordered`
- add a unique Ginkgo label such as `bi-directional`
- keep setup self-contained within the spec
- reuse existing helpers where possible instead of adding a second e2e framework

### 2. Prepare isolated test state

Inside the new spec:

- use the suite-provided `E2E_REPO_NAME` and `E2E_CHECKOUT_DIR`
- create a dedicated namespace for the scenario
- generate unique names for:
  - Flux `GitRepository`
  - Flux `Kustomization`
  - `GitProvider`
  - `GitTarget`
  - `WatchRule`
  - `IceCreamOrder` instances

Also install the existing sample CRD fixture:

- `test/e2e/templates/icecreamorder-crd.yaml`

### 3. Bootstrap the forward GitOps flow

Write repo content into the prepared checkout using a dedicated folder for this scenario, for example:

- `bi-directional/`

Suggested layout:

- `bi-directional/crds/icecreamorder-crd.yaml`
- `bi-directional/live/shop.example.com/v1/icecreamorders/<namespace>/icecream-a.yaml`
- `bi-directional/live/kustomization.yaml`

Then:

- commit and push the initial content to Gitea
- create Flux `GitRepository`
- create Flux `Kustomization` pointing to the scenario path
- wait for Flux reconciliation

Assertions:

- CRD becomes Established
- first `IceCreamOrder` exists in the cluster
- live spec matches the Git content

### 4. Add the second normal GitOps commit

In the repo checkout:

- update `icecream-a`
- add `icecream-b`
- commit and push

Then wait for Flux to reconcile the new revision.

Assertions:

- both objects exist in the cluster
- updated fields on `icecream-a` are present
- new object `icecream-b` is present
- repo head advanced exactly once for this user-authored GitOps change

### 5. Enable reverse sync from the start

The reverse direction should already be configured before or at the same time as Flux so we test the real
question: can both systems watch the same resources without churn?

Create:

- `GitProvider` pointing at the same Gitea repo and branch
- `GitTarget` pointing at the same repo path Flux reads from
- `WatchRule` for `shop.example.com/v1` `icecreamorders`

Reuse existing e2e patterns:

- `createGitProviderWithURL`
- `createGitTarget`
- `test/e2e/templates/watchrule-crd.tmpl`
- `verifyResourceStatus`

Assertions:

- `GitProvider` becomes Ready
- `GitTarget` becomes Ready
- `WatchRule` becomes Ready
- no immediate commit churn occurs once both systems are watching the same desired state

### 6. Exercise the reverse path

Patch one `IceCreamOrder` through the Kubernetes API.

Example actions:

- change container type
- add or change a topping
- change scoop quantities

Then verify:

- GitOps Reverser observes the API-driven change
- exactly one new commit appears in the repo
- the YAML in Git reflects the changed spec
- Flux sees the new revision and reconciles successfully

Expected result:

- Flux should find the cluster already converged, or at least converge without causing further semantic changes

### 7. Detect loops explicitly

This is the most important part of the test.

Capture the repo state before the API patch:

- commit count
- HEAD revision

After the reverse-sync commit appears:

- verify commit count increased by exactly one
- verify HEAD changed once

Then wait through a settling window and assert:

- commit count remains stable
- HEAD remains stable
- Flux `Kustomization` stays Ready on the latest revision
- controller logs do not show repeated commit/push cycles for the same object

If repeated commits appear, the test should fail and preserve enough diagnostics to understand the loop.

## Helper Changes Likely Needed

### Repo helpers

Small helpers for:

- writing files into the prepared checkout
- running `git add`, `git commit`, `git push`
- retrieving `git rev-parse HEAD`
- retrieving commit count

### Flux manifest helpers

Possible new templates for:

- Flux `GitRepository`
- Flux `Kustomization`

These can live in `test/e2e/templates/` if that keeps the spec readable.

### Assertion helpers

Possible helpers for:

- waiting on Flux Ready conditions
- reading live `IceCreamOrder` JSON
- comparing expected spec fields
- waiting for repo head to stabilize

## Makefile Changes

Add a dedicated target:

```make
.PHONY: test-e2e-bi
test-e2e-bi:
	export CTX=$(CTX)
	export INSTALL_MODE=$(INSTALL_MODE)
	export NAMESPACE=$(NAMESPACE)
	export E2E_AGE_KEY_FILE=$(CS)/age-key.txt
	go test ./test/e2e/ -v -ginkgo.v -ginkgo.label-filter=bi-directional
```

This should follow the same pattern used by:

- `test-e2e-quickstart-helm`
- `test-e2e-quickstart-manifest`
- `test-e2e-audit-redis`

## Success Criteria

The experiment is successful if all of the following hold:

- Flux can manage the initial and second Git-authored changes for `IceCreamOrder`
- GitOps Reverser can watch the same resources from the start
- a live API change produces a reverse-sync commit
- Flux reconciles that commit successfully
- the repo does not keep receiving additional commits afterward

## Failure Criteria

The experiment should be considered a failure, or at least a discovery of unsupported behavior, if:

- GitOps Reverser immediately creates churn commits for already-synced resources
- Flux applying manifests causes GitOps Reverser to create repeated no-op commits
- a live API patch leads to a repeated repo/cluster feedback loop
- YAML normalization differences between Flux-applied state and reverse-written state keep changing the repo

## Likely Follow-Up

After the result is known:

- update `README.md`
- either relax the current warning about sharing resources with GitOps tools
- or document precisely why the setup still loops and is unsafe

Either outcome is valuable. A passing test demonstrates a workable bi-directional flow. A failing test gives a
reproducible loop scenario that can guide future fixes.
