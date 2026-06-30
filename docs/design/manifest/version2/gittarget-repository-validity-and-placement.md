# GitTarget repository validity and placement controls

> Status: proposed
> Related: [manifest-inventory-file-agnostic-placement.md](../manifest-inventory-file-agnostic-placement.md),
> [manifestedit-new-file-placement-spike.md](../../../finished/manifestedit-new-file-placement-spike.md),
> [manifestedit-integration-readonly-reconcile.md](../manifestedit-integration-readonly-reconcile.md)

## Summary

GitTarget startup should include a repository-content validity gate. A GitTarget
is valid only when the configured branch/path contains at most one editable KRM
document for each Kubernetes resource identity. If Git contains two manifests for
the same resource, the API is still the source of truth, but Git no longer has a
single safe destination for that truth. The GitTarget must not start, and a
running GitTarget must transition back to invalid as soon as the branch scan sees
the duplicate.

This replaces the older "first occurrence wins, delete duplicate losers" idea in
the file-agnostic placement vision. Duplicate KRM is not a prune candidate; it is
an invalid repository state that requires a human or upstream Git process to
choose the authoritative file.

This same round should add two GitTarget placement controls:

- `spec.discovery.recurse`: whether repository discovery scans only the immediate
  GitTarget path or recursively scans child folders.
- `spec.newFilePath`: a template that decides where newly-discovered API objects
  are written when no existing Git document owns them. If multiple new resources
  resolve to the same file path, the writer may create or append a multi-document
  YAML file.

## Desired API shape

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
spec:
  providerRef:
    name: platform
  branch: main
  path: clusters/prod
  discovery:
    recurse: true
  newFilePath: "{{ .GroupPath }}/{{ .Version }}/{{ .Resource }}/{{ .Namespace }}/{{ .Name }}.yaml"
```

`discovery.recurse` defaults to `true` to preserve the current inventory
direction, which recursively scans YAML below the GitTarget path. Users with flat
GitOps folders can set it to `false`; then only files directly under
`spec.path` are discovered, and subdirectories are ignored for duplicate
detection and repo-state snapshots.

`newFilePath` replaces any enum-style choice for new-file placement. It is a
single template evaluated relative to `spec.path`. The default renders the
current canonical layout. Suggested template variables:

| Variable | Meaning |
|---|---|
| `.Group` | API group, empty for core resources |
| `.GroupPath` | API group as a path segment, omitted for core resources |
| `.Version` | API version |
| `.Resource` | plural resource name |
| `.Kind` | manifest kind when available |
| `.Namespace` | namespace, empty for cluster-scoped resources |
| `.Name` | object name |

Rules:

- The rendered path must be relative, clean, and stay under `spec.path`.
- Empty path segments are removed before joining.
- Sensitive resources still use `.sops.yaml` unless `newFilePath` already
  renders that suffix.
- Existing-resource writes always follow the inventory location. `newFilePath`
  applies only when Git has no document for the API object.

## Multi-document creation

`newFilePath` makes multi-document files a natural outcome instead of a separate
mode enum. If two API objects with no existing Git location render to the same
file, the writer appends the second object as another YAML document in that file:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: app-config
  namespace: default
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: app
  namespace: default
```

This is allowed only for plaintext manifests. Encrypted SOPS files remain
single-document because the writer re-encrypts whole files and must not combine
multiple secrets or secret-adjacent objects into one encrypted document stream.

If an existing file at the rendered `newFilePath` contains valid KRM documents,
the writer may append the new document when doing so does not create a duplicate
identity. If the file is invalid YAML, non-editable, encrypted, or outside the
discovery scope, the write is refused and surfaced as a repository validity
problem instead of overwriting user content.

## Repository validity gate

Add a new condition:

```text
RepositoryValid
```

Suggested reasons:

| Reason | Meaning |
|---|---|
| `OK` | Branch/path scanned and no blocking repository-content issues were found |
| `DuplicateResourceManifests` | Two or more KRM documents target the same resource |
| `InvalidRepositoryContent` | YAML or placement content is unsafe enough to block startup |
| `ScanFailed` | The branch/path could not be fetched or scanned |
| `NotStarted` | Blocked by an earlier gate |

Gate order:

1. `Validated`: provider, branch allow-list, GitTarget path conflicts.
2. `EncryptionConfigured`.
3. `RepositoryValid`: fetch/sync the branch and scan `spec.path` with the
   configured recursion policy.
4. `SnapshotSynced`.
5. `EventStreamLive`.
6. `Ready`.

When `RepositoryValid=False`, set:

- `SnapshotSynced=Unknown` with reason `Blocked`
- `EventStreamLive=False` with reason `RepositoryInvalid`
- `Ready=False` with reason `RepositoryInvalid`

The GitTarget must not accept live events while invalid. If the target had
already reached live processing and a later branch scan discovers duplicate KRM,
the controller disables or unregisters that GitTarget's event stream and deletes
its `FolderReconciler`. The branch worker may remain alive for other GitTargets
on the same provider/branch.

## Duplicate identity definition

The validity scan should use the API-side resource identity wherever possible:

```text
group/version/resource/namespace/name
```

Manifest content gives `apiVersion`, `kind`, `metadata.namespace`, and
`metadata.name`. The scanner maps GVK to GVR through the same discovery/catalog
path used by WatchRule resolution. If mapping is unavailable, the scanner may use
manifest identity as a fallback diagnostic, but a production duplicate block
should be based on API identity so aliases or preferred-version details do not
hide conflicts.

Cluster-scoped resources use an empty namespace. Documents with no namespace for
a namespaced resource are invalid unless an explicit future namespace-context
feature supplies that namespace.

Duplicate examples that must block:

- `apps/v1 Deployment default/app` in `app.yaml` and `copy.yaml`
- the same ConfigMap repeated twice inside one multi-document YAML file
- one copy at the canonical path and one user-placed copy under `apps/app.yaml`

Non-KRM YAML, empty YAML documents, and unwatched resource kinds do not
participate in duplicate blocking unless they are otherwise invalid enough to
block scanning.

## Detecting external pushes

Repository validity is not only a startup check. Every time the controller syncs
or observes a newer remote branch tip for a GitTarget, it must rescan the
configured path before keeping the target live.

Implementation model:

1. Fetch branch metadata for the GitTarget.
2. If the remote HEAD changed since the last repository-valid scan, checkout the
   new tree and rebuild inventory for `spec.path`.
3. If duplicate identities are found, transition the GitTarget to
   `RepositoryValid=False` in the same reconcile and stop its event stream.
4. If a later push removes the duplicate, the next scan returns the GitTarget to
   `RepositoryValid=True`, runs snapshot sync again, then resumes live events.

"Immediately" means the next GitTarget reconcile after the remote change is
observed. In e2e tests this should be driven by an explicit reconcile trigger or
by waiting for the controller's branch metadata poll interval; if a Git webhook
or provider callback is added later, it can shorten the same path without
changing the state machine.

## Implementation steps

1. Extend the API:
   - add `GitTargetSpec.Discovery *GitTargetDiscoverySpec`
   - add `GitTargetDiscoverySpec.Recurse *bool` defaulting to `true`
   - add `GitTargetSpec.NewFilePath string`
   - add kubebuilder validation for relative path template output constraints
     where possible; runtime validation still owns rendered paths.
2. Extend manifest scanning:
   - add `IndexDir(root, options)` or `IndexDirWithOptions`
   - support recursive and flat scans
   - expose duplicate groups, not only duplicate losers
   - keep diagnostics bounded for status.
3. Add repository validation to `BranchWorker`:
   - fetch/prepare branch
   - scan the GitTarget path
   - return summary counts plus duplicate details.
4. Add the `RepositoryValid` gate to `GitTargetReconciler`.
5. Make `EventRouter`/`GitTargetEventStream` stoppable for invalid targets.
6. Replace new-file placement enum usage with `newFilePath` rendering.
7. Add append-to-existing-file support for same-path new resources, with
   plaintext-only and no-duplicate guards.
8. Update CRDs, samples, Helm chart values, and configuration docs.

## E2E tests

### Existing folder with duplicate KRM blocks startup

Purpose: prove the simple invalid-at-start case.

Setup:

1. Create a fresh e2e repo/branch.
2. Commit two plaintext YAML files under the future GitTarget path:

   ```text
   clusters/prod/app-a.yaml
   clusters/prod/app-b.yaml
   ```

   Both files contain the same `v1/ConfigMap default/app`, with different data so
   it is obvious they are separate copies.
3. Create a `GitProvider`, `GitTarget`, and `WatchRule` for ConfigMaps. Set
   `spec.path: clusters/prod` and leave `discovery.recurse` at the default.

Expected assertions:

- `GitTarget.status.conditions[RepositoryValid]` becomes `False`.
- Reason is `DuplicateResourceManifests`.
- The condition message names the duplicated resource and both locations, bounded
  if there are many duplicates.
- `Ready=False` with reason `RepositoryInvalid`.
- `SnapshotSynced` is `Unknown` or `False` and did not create a write request.
- `EventStreamLive=False` or `Unknown`; live API edits to `default/app` do not
  produce a commit while invalid.

### External push with duplicate KRM invalidates a live GitTarget

Purpose: prove a valid running target becomes invalid after another actor pushes
a duplicate manifest into the branch/path.

Setup:

1. Create a fresh e2e repo/branch with one valid manifest under
   `clusters/prod/app.yaml`.
2. Create `GitProvider`, `GitTarget`, and `WatchRule`.
3. Wait until the GitTarget is `Ready=True`,
   `RepositoryValid=True`, `SnapshotSynced=True`, and `EventStreamLive=True`.
4. From an external clone or direct Gitea API helper, push
   `clusters/prod/copy.yaml` containing the same `v1/ConfigMap default/app`.
5. Trigger or wait for the GitTarget reconcile that observes the new remote HEAD.

Expected assertions:

- The GitTarget transitions from `Ready=True` to `Ready=False`.
- `RepositoryValid=False` with reason `DuplicateResourceManifests`.
- `EventStreamLive=False` with reason `RepositoryInvalid`, and the stream no
  longer accepts live events for that GitTarget.
- The controller does not auto-delete either duplicate file.
- An API update to the ConfigMap while the GitTarget is invalid does not create a
  Git commit.
- After removing `copy.yaml` with another external push, the next reconcile makes
  `RepositoryValid=True`; the target runs snapshot sync before returning to
  `Ready=True`.

### Discovery recursion coverage

The duplicate tests should be extended with a table once `discovery.recurse`
lands:

| `recurse` | Duplicate location | Expected |
|---|---|---|
| `true` | `clusters/prod/app.yaml` and `clusters/prod/nested/app.yaml` | invalid |
| `false` | `clusters/prod/app.yaml` and `clusters/prod/nested/app.yaml` | valid; nested file ignored |
| `false` | `clusters/prod/app-a.yaml` and `clusters/prod/app-b.yaml` | invalid |

This can be unit-tested first around the scanner and covered in e2e by one
representative `recurse=false` GitTarget.

## Open questions

- Should invalid YAML block `RepositoryValid`, or only the subset that looks like
  KRM and cannot be safely classified?
- How much duplicate detail belongs in GitTarget status before it becomes too
  large? A bounded summary plus first N examples is likely enough.
- Should a branch push notification be added so external invalidating pushes are
  observed faster than the normal reconcile/poll interval?
- Should `newFilePath` support conditionals, or only simple variable expansion
  plus path cleanup?
