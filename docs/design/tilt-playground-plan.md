# Playground Plan

## Goal

Make the local Tilt loop feel like a small, reliable playground for GitOps Reverser:

- one click to bootstrap a reusable example repo and starter reverse-GitOps resources
- a few focused actions to create, update, and delete watched objects
- predictable cleanup when the developer wants to reset everything

The Tilt layer should stay close to the current repo philosophy:

- Task/e2e owns cluster and repo orchestration
- Tilt owns the manual control surface
- the flow should be robust when buttons are clicked more than once

## Requirements

### Functional requirements

- Use a fixed playground namespace: `tilt-playground`
- Use a fixed playground repo name: `playground`
- Bootstrap must create or reconcile:
  - a reusable Gitea repo
  - Git credentials in the playground namespace
  - the `sops-age-key` Secret in the playground namespace
- Starter `GitProvider`, `GitTarget`, and `WatchRule` resources must live in a shared
  `test/playground` kustomize package so Tilt and e2e use the same source of truth
- Tilt must expose manual actions for:
  - bootstrap
  - status/inspection
  - create or update a watched `ConfigMap`
  - delete the watched `ConfigMap`
  - create or update a watched `Secret`
  - delete the watched `Secret`
  - cleanup

### Robustness requirements

- Bootstrap must be safe to click repeatedly
- Bootstrap should no-op quickly when the playground is already healthy
- Bootstrap must survive partial state, including:
  - repo exists but namespace is gone
  - namespace exists but starter resources are missing
  - Gitea repo exists from a previous run
  - Gitea SSH key title already exists from a previous run
- Cleanup must be safe to run even when some resources are already gone
- Cleanup must remove enough state that a later bootstrap can rebuild cleanly

### UX requirements

- The flow should prefer stable names over per-run random names
- The logs should explain whether bootstrap is doing real work or skipping because the playground is ready
- Status should show both cluster state and recent repo commits
- The workflow should stay small and playable, not become a second full demo harness

## Non-goals

- Full custom forms or arbitrary user input in the Tilt UI
- Replacing the existing e2e/demo flows
- Managing every possible Kubernetes resource type from Tilt
- Building a general-purpose repo fixture manager

## Proposed design

### 1. Stable playground identity

Use stable names so developers always know where to look:

- namespace: `tilt-playground`
- repo: `playground`
- provider: `playground-provider`
- target: `playground-target`
- watch rule: `playground-watchrule`

Keep these fixed in the Task/e2e playground flow rather than exposing a second
set of local override environment variables. The point of the playground is a
known place to click around, not another configurable matrix.

### 2. Task-backed bootstrap

Keep the bootstrap path in Task/e2e, but make it intelligent:

- preflight checks determine whether the playground is already healthy
- if healthy, bootstrap exits successfully without re-running repo setup
- if not healthy, bootstrap runs the dedicated e2e playground setup path
- the e2e setup path applies `test/playground` with `kubectl apply -k` after the namespace and
  Secrets are ready

This keeps the Tilt button fast on the happy path while still reusing the tested setup logic for repair/reconcile.

### 3. Idempotent repo bootstrap

The repo bootstrap path must treat fixed names as normal, not exceptional.

In particular:

- admin transport SSH key registration must be idempotent by title as well as by key material
- reusing a fixed repo name must not fail because a previous run left behind a Gitea key with the same title

### 4. Shared playground manifests

The starter custom resources should live in `test/playground/`:

- `kustomization.yaml`
- `gitprovider.yaml`
- `gittarget.yaml`
- `watchrule.yaml`

That gives us one obvious place to:

- tweak repo, branch, path, or watch rules
- temporarily remove a resource from the playground by editing the kustomization
- keep Tilt auto-apply and e2e bootstrap aligned

Tilt should load that package with `k8s_yaml(kustomize('test/playground'))` and group the objects
into a single `playground` resource that waits for `playground-bootstrap`.

### 5. Small set of watched-object actions

Tilt should still expose a very small set of manual actions:

- upsert `ConfigMap`
- delete `ConfigMap`
- upsert `Secret`
- delete `Secret`

These are enough to validate the main reverse-GitOps loop:

- create
- update
- delete
- encrypted Secret handling

### 6. Explicit cleanup

Cleanup should remove:

- the playground namespace
- the local repo checkout under `.stamps/repos/`
- generated repo artifact files under `.stamps/e2e-repo-artifacts/`
- the Gitea repo
- the Gitea admin transport SSH key used for the playground repo

Cleanup should use a direct helper instead of the full e2e `BeforeSuite` path.
That keeps reset operations fast and avoids unrelated cluster-setup failures from
blocking a simple playground reset.

Cleanup does not need to remove every possible historical Gitea token or user
account. The important requirement is that bootstrap works again after cleanup.

## Testing strategy

### Automated tests

#### Unit tests

Add focused Gitea client tests for:

- finding a user key by title
- replacing a stale key when the title already exists with different material
- deleting admin-managed keys

This is the lowest-cost way to lock down the failure that originally broke repeated playground bootstrap.

#### Targeted playground e2e

- `playground`
  - bootstrap the fixed playground
  - apply `test/playground`
  - verify starter resources become `Ready`

#### Playground manifest validation

The shared kustomize package is validated by the same bootstrap flow:

- `kubectl apply -k test/playground` must succeed after bootstrap creates the namespace and Secrets
- Tilt must be able to load `k8s_yaml(kustomize('test/playground'))` without introducing a second
  copy of the starter resources

#### Direct cleanup helper validation

Use a dedicated cleanup helper, invoked by Task/Tilt, to verify:

- cleanup succeeds when the namespace or repo is already gone
- local repo and artifact directories are removed
- the Gitea repo and fixed-title transport key are deleted when present

#### Future e2e follow-up

A stronger follow-up scenario would be:

1. run cleanup
2. run bootstrap
3. run bootstrap again
4. run cleanup
5. run cleanup again

That would validate both idempotent bootstrap and idempotent cleanup as a lifecycle instead of as isolated operations.

### Manual validation

Recommended manual check from Tilt:

1. Click `playground-bootstrap`
2. Confirm the `playground` Tilt resource auto-applies the manifests from `test/playground/`
3. Click `playground-status`
4. Click `playground-upsert-configmap`
5. Click `playground-upsert-secret`
6. Confirm new commits land in the `playground` repo
7. Click `playground-delete-configmap`
8. Click `playground-delete-secret`
9. Confirm delete commits land
10. Click `playground-cleanup`
11. Click `playground-bootstrap` again and confirm recovery

### Required repo validation

Because this touches Go code and executable workflow, the normal repo validation still applies:

- `task lint`
- `task test`
- `task test-e2e`
- `task test-e2e-quickstart-manifest`
- `task test-e2e-quickstart-helm`

## Possible next steps

- add one more watched-resource action for a Deployment or Service if ConfigMap/Secret is too narrow
- add a repo diff/log helper resource in Tilt
- consider Tilt `uibutton` later if the current `local_resource` approach starts feeling too rigid

For now, the priority is reliability, not UI sophistication.
