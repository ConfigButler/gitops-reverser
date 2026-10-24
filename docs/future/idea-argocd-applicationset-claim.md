# Claiming an Argo CD ApplicationSet: parked design

Status: **parked**, 2026-10-23. Nothing is built. The direct-claim refusal it builds on shipped
in #437 (`OwnedByApplicationSet`). Argo CD facts below were read from Argo CD's source at 3.5.0
(`external-sources/argo-cd`, commit `3cbae653d`).

## Where this comes from

Since #437 an `ArgoCDClaim` behaves like a `FluxClaim`. It owns
`spec.syncPolicy.automated.enabled: false` on its `Application` through server-side apply, switches
it off again when someone switches it on (with an Event), and releases it on hand-back.

That can't work on an `Application` an `ApplicationSet` generated, because the `ApplicationSet`
rewrites the generated spec on every reconcile. So a direct claim on a generated `Application` is
refused, with a reason that names the `ApplicationSet` (`OwnedByApplicationSet`).

This document is the other half: let the claim name **the `ApplicationSet` itself**. The user
then chooses where control sits. A direct claim to an `Application` someone else manages stays an
error, with a clear message that points here.

## The use case: rolling out multi-branch editing

1. A team creates a `GitTarget` on a new branch (a `GitBranch` with `parentBranch: main`), and
   GitOps Reverser creates the branch.
2. An `ApplicationSet` discovers the branch and generates an `Application` for it, with
   `targetRevision` set to the branch.
3. One claim on the `ApplicationSet` keeps every generated `Application` manual and coordinates
   each of them with the `GitTarget` that writes its branch.

Steps 1 and 2 already work today. Step 3 is what is missing: today each generated `Application`
would need its own claim, and those are refused.

## What Argo CD does

| Fact (3.5.0) | Consequence for us |
| --- | --- |
| The controller replaces the whole spec of each generated `Application` with the rendered template (`found.Spec = generatedApp.Spec`, `applicationset_controller.go`), minus `spec.ignoreApplicationDifferences` | Any field we own on a generated `Application` is wiped on the next reconcile. **Control has to sit on the `ApplicationSet`'s template**, not on its `Application`s |
| It sets a controller `ownerReference` from each generated `Application` to its `ApplicationSet`, and removes it when the `ApplicationSet` is deleted with orphaning | A generated `Application` can be recognized from its `ownerReferences`. The direct-claim refusal can name the owner |
| `spec.syncPolicy.applicationsSync`: `create-only`, `create-update`, `create-delete`, `sync` (the default). Only `create-update` and `sync` update existing `Application`s | Under `create-only` / `create-delete`, a template change never reaches `Application`s that already exist. Taking the template switch is not enough there; the claim must also check (or own) the switch on each existing `Application` |
| `spec.templatePatch` is a Go-templated patch applied per `Application` after the template | One generated `Application` can switch automated sync back on even when the template has it off. **Check the generated `Application`s, not only the template** |
| **`RollingSync` (progressive syncs)** forces `automated.enabled: false` on the generated `Application`s, and the `ApplicationSet` controller starts their syncs itself by setting `operation` (`progressivesync/progressive_sync.go`) | Two parties would start syncs. An `ApplicationSet` with `strategy.type: RollingSync` must be refused |
| `status.resources` lists the generated `Application`s (capped; `status.resourcesCount` has the full count) | Where we read which `Application`s exist. The cap means we should list by owner, not trust `status.resources` alone |
| The SCM Provider generator lists branches (`allBranches`, `filters[].branchMatch`) and re-scans every 30 minutes by default (`requeueAfterSeconds`). The `ApplicationSet` webhook refreshes Git, Pull Request and Plugin generators, **not** the SCM Provider generator | Branch discovery through SCM Provider is slow unless `requeueAfterSeconds` is lowered. See [the generator question](#which-generator-finds-the-branches) |

## The shape

### Option A: a reference on `ArgoCDClaim` (lean)

```yaml
kind: ArgoCDClaim
metadata: {name: checkout, namespace: team-a}
spec:
  kubeProviderRef: {name: management}
  applicationSetRef: {namespace: argocd, name: checkout}   # exactly one of applicationRef / applicationSetRef
```

- CEL: exactly one of `applicationRef` and `applicationSetRef`. Both immutable, like today.
- It reuses the claim's existing machinery: `KubeProvider`, its `allowedApplierClaims` grants
  (`applierNamespaces`), `Configured` / `Ready`, finalizer.
- `GitTarget.spec.applierClaimRef` is unchanged: a `GitTarget` names the claim, and the claim
  finds the generated `Application` that applies that `GitTarget`'s branch.

### Option B: a separate kind, `ArgoCDApplicationSetClaim`

Clearer status (a per-`Application` list instead of one `operation`), but a fourth kind and a
third value in the `applierClaimRef` kind enum. Choose this only if the status shapes turn out too
different to share.

### What the claim does to the `ApplicationSet`

- **Takes the switch on the template:** owns
  `spec.template.spec.syncPolicy.automated.enabled: false` through server-side apply. It switches
  it off again on drift (with a `Normal` Event) and releases it on hand-back, as the
  `Application` claim does.
- **Refuses** (`Configured=False`, each with a message that names the field to change):
  - `strategy.type: RollingSync`: the `ApplicationSet` starts syncs itself.
  - `applicationsSync` that doesn't update (`create-only`, `create-delete`) while an existing
    `Application` has automated sync on: the template switch can't reach it.
  - a `templatePatch` that leaves automated sync on for an `Application` a `GitTarget` needs.
  - a template with more than one source, or a `targetRevision` that isn't a branch (the existing
    `UnsupportedSource` / `SourceMismatch` rules, applied to the template).
- **Matches `GitTarget`s to `Application`s:** a generated `Application` belongs to the `GitTarget`
  that writes the repository and branch it syncs (`repoURL` + `targetRevision`). That is today's
  eligibility rule, applied per `Application`. A `GitTarget` with no matching `Application` yet
  waits: the generator may not have found its branch.
- **Syncs per `Application`:** each sync is still started on the generated `Application`
  (`operation`, pinned to the commit), exactly as today. Setting `operation` is safe: the
  `ApplicationSet` replaces `spec`, and only copies `operation` from the template when the template
  sets one.

### Status

One claim now covers many `Application`s, so `status.operation` and `status.repairPending` become
per `Application`:

```yaml
status:
  applications:            # listType=map, listMapKey=name; only Applications a GitTarget is bound to
  - name: checkout-feature-x
    branch: feature-x
    operation: {phase: Succeeded, commit: 1a2b3c4, reason: ForeignCommit}
    repairPending: false
```

Printer columns would show counts (bound / applying), not one operation.

## Which generator finds the branches

| Generator | How it finds GitOps Reverser's branches | Fit |
| --- | --- | --- |
| SCM Provider (`allBranches` + `branchMatch`) | Lists branches through the Git host's API (GitHub, GitLab, Gitea, Bitbucket, Azure DevOps, CodeCommit) | Works today. Needs a host API token, and has a 30-minute default re-scan with no webhook refresh |
| Pull Request | One `Application` per open PR | Fits the editing product (branch → PR → preview). An edit branch only appears once its PR is opened |
| **Plugin**, served by GitOps Reverser | The `ApplicationSet` calls `POST /api/v1/getparams.execute` on an HTTP service; GitOps Reverser would answer with the branches its `GitTarget`s write (`branch`, `path`, `namespace`, …) | The most direct answer to "watch for GitOps Reverser's branches". It's refreshed on every `ApplicationSet` webhook. It's also a new network surface with its own auth; out of scope until the claim exists |

## Open questions

- **Who manages the `ApplicationSet`?** It is often applied by another Argo CD `Application`
  (app-of-apps) or by Flux. If that manifest sets `template.spec.syncPolicy.automated`, the fight
  moves one level up. Same answer as Flux's "the manifest must omit `suspend`": detect a foreign
  manager of that field in `managedFields` and refuse with the name of that manager.
- **Several `GitTarget`s on one generated `Application`** (different paths, same branch): allowed
  today for one `Application`, so allowed here. The per-`Application` status entry holds one
  operation.
- **Hand-back across many `Application`s:** releasing the template switch turns automated sync on
  for all of them at once. Does hand-back need to wait for every in-flight sync, as the Flux
  hand-back waits for its last apply?
- **Applications in any namespace:** generated `Application`s may land outside `argocd`. The
  `KubeProvider`'s `allowedApplierClaims` grant must name every namespace they land in in
  `applierNamespaces`, or the claim refuses the ones it doesn't reach.
- **RBAC:** `get`, `list`, `watch`, `patch` on `applicationsets`, on top of what the `Application`
  claim needs.

## Not doing

- **Claiming generated `Application`s one by one.** The `ApplicationSet` owns their spec; that is
  exactly the fight the direct-claim refusal prevents.
- **Using `ignoreApplicationDifferences` to let us own the switch per `Application`.** It works,
  but asks the user to edit the `ApplicationSet` to make room for us, and the switch then lives in
  two places. Owning the template switch is simpler.
