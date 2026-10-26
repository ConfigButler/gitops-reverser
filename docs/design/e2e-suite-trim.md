# E2E suite: what is left to trim

Status: **open plan**, condensed on 2026-10-24 from the 2026-10-20 timing review (three green CI
runs: a4c2e633 on main, e880cdc6 for #422, 2a6c1ab2 for #420). The timing half is done:
`SpecPriority` on the long containers, `--wait=false` namespace teardown, the remote-refresh spec
reduced to one foreign-push spec after #424 fixed the `status.remote` requeue, and the `Serial` CRD lifecycle
spec moved off the manager leg. What follows is the part not built.

**Do not shorten `RequeueSteadyInterval` for e2e.** A test-only 60s tick would rescue a broken event
path through the periodic reconcile, and the spec would pass, just slower. Today the 5-minute tick
is longer than nearly every bound, so an event-wiring regression fails loudly.

## Run image-refresh only when its inputs change

The leg tests the developer Taskfile's stamp chain (rebuild, k3d import, rollout), not the product,
and 266s of its 369s suite is the BeforeSuite rebuild. Gate it on a path filter in
[`ci.yml`](../../.github/workflows/ci.yml): `Taskfile*.yml`, `test/e2e/Taskfile-e2e.yml`,
`Dockerfile*`, `hack/**`, `.dockerignore`, `go.mod`, and `charts/**`, since the leg installs the
packaged chart (`HELM_CHART_SOURCE=./gitops-reverser.tgz`). Run it on every push to main as a backstop. S3
("second Go change also restarts") and S6 ("pod uses the imported image", already asserted in S2)
can go; each costs a full rebuild. This removes a 14–18 minute leg from most PRs.

## Detail gaps: things that look tested and are not

These matter more than the duplicates below.

1. **[`save_request_e2e_test.go`](../../test/e2e/save_request_e2e_test.go)**: a comment says the
   generateName spec "is skipped in configured-author mode", but it has no `Skip`. Add it, or fix
   the comment.
2. **[`signing_e2e_test.go`](../../test/e2e/signing_e2e_test.go)**: its own comment says the
   `GitTarget` recreate "writes nothing", and the asserted commit is the one from before the
   recreate. The step costs a reconcile and proves nothing; delete it, or make it the thing under
   test.
3. **[`watchrule_configmap_secret_e2e_test.go`](../../test/e2e/watchrule_configmap_secret_e2e_test.go)**:
   the backfill spec says it guards the "GVR already covered by other rules" early return. The
   earlier specs delete their rules first, so that condition is probably not met (reader-reported,
   unverified). As written it is a plain backfill.
4. **[`tilt_playground_e2e_test.go`](../../test/e2e/tilt_playground_e2e_test.go)**: `Serial`, and
   the default label filter does not exclude `playground`, so it holds a CI serial phase for a
   fixture check. Gate it like `demo`.

## Redundancy: what to move, merge or drop

Hygiene, not wall clock: the duplicates are mostly 5–30s specs, and per-container setup is cheap
(`SetupRepo` median 0.7s).

### Move out of e2e: no cluster, Gitea or controller needed

| Spec | Why it does not need e2e | Already covered at |
| --- | --- | --- |
| `watchrule_generation_barrier`: the 8-entry `DescribeTable` "accepts only a current generation that is streaming" | a pure Go predicate over an `unstructured` value | nothing yet; move it to a plain `_test.go` |
| `prune_mode`: "defaults, omits, and validates spec.prune.mode at the API server" | CRD schema defaulting and an enum | no envtest yet; `internal/controller` envtest loads the CRDs (`applierclaim_schema_test.go` is the pattern) |
| `gittarget_overlap`: sibling paths Validated, nested path `TargetConflict` | controller logic | `internal/controller/gittarget_controller_test.go` |
| `source_cluster`: the "fails KubeProvider Validated (no dial)" specs and the `KubeProviderNotFound` spec | no dial, no kcp; they run serially on a procs=1 leg | `internal/kubeconfig/kubeconfig_test.go` and the `KubeProvider` controller tests |
| `gitprovider_validation`: "BranchNotAllowed" | controller logic, now a `GitBranch` grant check | `internal/controller/gitbranch_controller_test.go` (verify before deleting) |

### Merge, keeping the unique detail

| Cluster | Keep | Fold in (and the detail it brings) |
| --- | --- | --- |
| Basic create/update/delete | `aggregated_apiserver` "separate commits for Flunder create, update, delete" (three commits, three hashes) | `crd_lifecycle`'s three CR specs into ONE CR lifecycle spec, keeping the field-filtering checks; the `watchrule_configmap_secret` delete spec into its update spec |
| Author attribution | `audit_route_attribution`, `deletecollection_intent` | `commit_author_attribution`'s spec is another with other data; drop it. `deployment_scale_subresource` folds into `deployment_scale_author_attribution` (author check conditional on configured-author mode) |
| Collection delete | `deletecollection_intent`'s selector-scope and finalizer specs | the two specs that are subsets of those. Keep `aggregated_deletecollection` (no response body is a real difference) |
| Signing | generated key, late joiner | BYOK shares every step but the key source, so make it a table entry; the committer/message spec needs no signing provider |
| Smoke checks | none | `watchrule_configmap_secret`'s "provider Ready" and "rule Ready", `aggregated_apiserver`'s, `gitprovider_validation`'s "HTTP provider Ready": every other spec proves these |
| New write branch | parent omitted | the `parentBranch: release` run repeats the same 3-minute path under another name; keep one, or make the second a short variant |
| Source cluster | three workspaces | one workspace is strictly subsumed |

Keep, despite looking similar: the Flux and Argo CD bi-directional specs (different contracts), and
`foreign_content` against `unsupported_folder` (first materialization against after a healthy
start), and the two values-file Describes (different producers).

Past this, the remaining lever is the fixed ~6 minutes each leg pays before its first spec (runner
disk cleanup, cluster bring-up, prepare): a CI-infrastructure project, not a test-content one.
