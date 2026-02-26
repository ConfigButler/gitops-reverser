# Simple Fix Plan: "CREATE then immediate DELETE" for namespaced resources

## Goal
Stop false DELETE commits (for example `oeps3`) while the resource still exists in the cluster.

## Root Cause (confirmed)
Repo-state listing parses file paths relative to repo root instead of GitTarget base path.

Example today:
- Actual file: `live-cluster/v1/configmaps/gitops-reverser-example-audit/oeps3.yaml`
- Parsed as if `group=live-cluster` (wrong)
- Cluster state uses core group (`group=""`)
- Reconciler sees mismatch and emits DELETE for the Git file.

## Design Principle
Keep the fix minimal and local:
- Fix path normalization at the source (`BranchWorker.listResourceIdentifiersInPath`)
- Do not redesign reconciliation or event stream behavior in this change

## Implementation Plan
1. Update path handling in `listResourceIdentifiersInPath`
- File: `internal/git/branch_worker.go`
- Change:
  - Keep scanning `basePath := repoPath + targetPath`.
  - Compute `relPath` relative to `basePath` (not `repoPath`) when `targetPath` is set.
  - Normalize separators with `filepath.ToSlash(relPath)` before parsing.
- Expected effect:
  - Parser receives `v1/configmaps/<ns>/<name>.yaml` instead of `live-cluster/v1/...`.

2. Add regression unit test for path-prefixed repos
- File: `internal/git/branch_worker_test.go` (or dedicated small test file in `internal/git/`)
- Test scenario:
  - Create test repo tree containing `live-cluster/v1/configmaps/ns1/oeps3.yaml`.
  - Call `ListResourcesInPath("live-cluster")`.
  - Assert returned identifier is:
    - `Group: ""`
    - `Version: "v1"`
    - `Resource: "configmaps"`
    - `Namespace: "ns1"`
    - `Name: "oeps3"`
- Add negative case:
  - Ensure marker files (like `.configbutler`) are still ignored.

3. Add focused reconciler regression test (small)
- File: `internal/reconcile/folder_reconciler_test.go` (or integration test if already present)
- Scenario:
  - Cluster state has `/v1/configmaps/ns1/oeps3`.
  - Repo state from parsed path should match same key.
  - Assert no DELETE is emitted for identical resource identity.

## Validation Plan
Run in this order:

```bash
make fmt
make vet
make lint
make test
```

Then run required suite before merge:

```bash
docker info
make test-e2e
make test-e2e-quickstart-manifest
make test-e2e-quickstart-helm
```

## Out of Scope (for this fix)
- Changing `GitTargetEventStream` behavior for nil-object CREATE events.
- Reworking snapshot/reconcile architecture.

## Risk
Low.
- Change is isolated to repo path parsing in one function.
- Regression tests directly cover the broken case.
