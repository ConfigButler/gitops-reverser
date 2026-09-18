# Bi-directional usage guide

GitOps Reverser publishes **live → Git**. Flux or Argo CD supplies **Git → cluster**.
Together, they let operators edit resources through the Kubernetes API and through Git.

**Whether that is safe depends on who owns each field.** Give every field a single
writer and the two directions never compete. Let API edits and Git edits reach the same
field and nothing in this system arbitrates between them: a push webhook shortens the
window in which a reconciler applies stale desired state, but it only notifies, and it
reserves nothing. Shared-field operation is therefore experimental, and it needs an
operating procedure that people follow.

If every field has one writer, read [Recommended modes](#recommended-modes) and
[Configure the reconciler](#configure-the-reconciler), then stop. The rest of the guide
is about shared fields: the [handoff](#operate-a-shared-field) that makes them workable,
and the [mechanics](#when-two-people-change-things-at-almost-the-same-time) that decide
which value survives when two writes overlap.

## Recommended modes

Choose the ownership model before configuring reconciliation.

| Mode | Ownership | Typical use |
| --- | --- | --- |
| **1. Audit only** | Reverser captures live changes; no reconciler writes the captured path back. | Audit trails and discovery |
| **2. Human in the loop** | Reverser captures changes; a human reviews or promotes them. | Hotfix capture and migration |
| **3. Split ownership** | Each field has one writer; API and Git workflows own different fields. | Interactive operations alongside GitOps |
| **4. Controlled bi-directional** | API and Git workflows share fields and coordinate when they write. | Experimental editing environments |

Modes 1–3 avoid competing writes when their ownership boundaries are enforced.
The rest of this guide focuses on mode 4.

## How the webhook loop works

**A configured Git push webhook removes the need for a direct trigger from Reverser
after each successful push.** GitHub or Gitea sends the notification to Flux or Argo CD:

```mermaid
sequenceDiagram
    actor User
    participant K8s as Cluster
    participant Rev as GitOps Reverser
    participant Git as Git host (GitHub / Gitea)
    participant Recon as Flux / Argo CD

    User->>K8s: edit through the Kubernetes API
    K8s-->>Rev: watch event
    Rev->>Git: commit and push
    Git-->>Recon: push webhook (Flux Receiver / Argo CD api/webhook)
    Recon->>Git: refresh tracked branch
    Recon->>K8s: apply new desired state if needed
    K8s-->>Rev: watch events if objects changed
    Note over Rev: matching content produces no new commit
```

Flux's `Receiver` requests a source refresh; the new artifact revision triggers dependent
`Kustomization` objects. Argo CD's webhook requests an application refresh; automated sync
applies changed desired state. Reverser itself neither triggers these controllers nor waits
for their applied revisions.

The webhook carries a notification, not a lock or an apply acknowledgment. A reconciler
can fetch a later branch tip when pushes arrive close together. It need not apply each
intermediate commit, and it can already be applying another revision when an API edit lands.

## Configure the reconciler

| Setting | Flux Kustomization | Argo CD Application |
| --- | --- | --- |
| Push notification | `Receiver` targets the `GitRepository` | Git host calls `/api/webhook` |
| Shared-field operation | Unsuspended, long apply interval | Automated sync enabled, `selfHeal: false` |
| Remaining overwrite triggers | Timer, source/configuration changes, retries, restart, explicit reconcile | New desired revision, application changes, retries, explicit sync |
| Convergence check | `Ready=True` and `status.lastAppliedRevision` | `Synced` and `status.sync.revision` |

### Flux

Configure a [Flux webhook Receiver](https://fluxcd.io/flux/guides/webhook-receivers/)
for the **`GitRepository`**. Source-controller fetches Git before the artifact revision
change wakes dependent Kustomizations. Targeting only the Kustomization could apply the
source artifact that Flux already has cached.

For a shared editing path, start with **`Kustomization.spec.interval: 24h`** and a
shorter **`GitRepository.spec.interval: 1m`** as a fallback for missed webhooks:

```yaml
# Fields on the GitRepository:
spec:
  interval: 1m
---
# Fields on the Kustomization for the shared editing path:
spec:
  interval: 24h
  retryInterval: 1m
  timeout: 2m
  suspend: false
```

These are pilot settings. The long apply interval reduces periodic correction of live
edits, while the source interval bounds the usual polling delay after a missed webhook.
New source revisions still trigger applies immediately. Failures retry at `retryInterval`,
which can also interrupt an editing session.

**Flux has no separate maximum interval in hours in the current Kustomization API.**
The [interval implementation](https://github.com/fluxcd/kustomize-controller/blob/main/api/v1/kustomization_types.go)
uses a Go duration without an additional hours-based cap. `24h` is an operating recommendation.
Controller-runtime's [10-hour cache resync](https://github.com/kubernetes-sigs/controller-runtime/blob/main/pkg/cache/cache.go)
is not a hard apply limit: Flux's
[watch predicates](https://github.com/fluxcd/kustomize-controller/blob/main/internal/controller/kustomization_manager.go)
filter unchanged generation/request tokens and unchanged source revisions.

An active Kustomization still reconciles periodically, and source changes, configuration
changes, retries, or controller restarts can cause earlier applies. A long interval does
not reserve an editing window. See the
[Flux reconciliation contract](https://fluxcd.io/flux/components/kustomize/kustomizations/#interval).

Keep the Kustomization **unsuspended** for the automatic webhook loop. Suspension blocks
source-triggered applies too. A workflow that suspends it must explicitly resume it after
publishing, wait for convergence, and suspend it again before accepting another edit.
A manual reconcile request does not bypass suspension.

For an unsuspended path, a manual source-first trigger is:

```bash
flux reconcile kustomization editing -n flux-system --with-source
```

### Argo CD

Enable automated sync and disable self-heal on the shared path:

```yaml
spec:
  syncPolicy:
    automated:
      enabled: true
      selfHeal: false
      prune: true
```

Use `prune: true` only when Git deletions should delete live objects. Configure the
Git host's [Argo CD webhook](https://argo-cd.readthedocs.io/en/stable/operator-manual/webhook/)
to `https://<argocd-server>/api/webhook`, with a shared secret. A refresh can lead to an
apply only when a sync is permitted; configuring a webhook does not enable automated sync.

With self-heal off, live drift alone does not trigger another automated sync after a
successful sync of the same revision and parameters. Polling still discovers new Git
commits without a webhook; the default period is `120s` plus up to `60s` jitter. See
[automated sync semantics](https://argo-cd.readthedocs.io/en/stable/user-guide/auto_sync/).

**`selfHeal: false` permits live editing; it does not freeze deployments.** A new commit,
a changed application parameter, or an explicit sync can still overwrite an unpublished
edit. A change to another source in a multi-source application can also trigger a sync.

For split ownership, `ignoreDifferences` excludes fields from drift comparison. It does
not alone protect them during a sync triggered for another reason.
[`RespectIgnoreDifferences=true`](https://argo-cd.readthedocs.io/en/stable/user-guide/sync-options/#respect-ignore-differences-configs)
also preserves the live value during sync for existing resources. Those fields become
API-owned, so this setting does not suit fields that Git must also drive.

[Why `selfHeal` cannot be made selective](design/support-boundary/argocd-bi-directional.md)
records the remaining alternatives, including the ones that look like they would work.

## Operate a shared field

For shared fields, establish a handoff between API editing and Git deployments:

1. Keep conflicting Git deployments and manual reconciles out of the editing session.
   A webhook or a long interval alone does not enforce this rule.
2. Let API edits reach the remote branch and check for refused writes or push failures.
3. Have the Git-side writer fetch that updated branch before preparing a deployment.
4. Apply the intended revision, confirm convergence, and then reopen API editing.

This is an operating procedure, held up by the people and pipelines that follow it.
Reverser implements no edit lock and reserves no transaction when a `CommitRequest` is
submitted. For unattended concurrent operation, prefer split ownership or separate
reconciliation scopes. Use Kubernetes preconditions for competing API editors and normal
Git review for competing Git authors. Treat simultaneous API and Git writes to shared
fields as a case that requires an explicit conflict policy.

## When two people change things at almost the same time

**Reverser publishes one branch's writes in order, one at a time.** Its own commits never
race each other, and its retained writes replay in the order they were accepted.

That ordering is the only guarantee on offer, and it covers only Reverser's own work.
Bob's direct Git push and the reconciler's fetch and apply run outside that queue
entirely. Knowing whose push reached Git first does not establish when the reconciler
applied it, or where the resulting watch event sits in Reverser's queue. No shared
transaction spans those operations.

The timelines use one notation throughout. Alice edits through the Kubernetes API and Bob
pushes to Git. `E5` is a captured watch event carrying `replicas: 5`, and `B`, `C`, and
`D` are commits on the tracked branch, named in the order they are created.

| Concurrent actions | What handles the collision | What remains unprotected |
| --- | --- | --- |
| Two API edits to one object | Kubernetes update preconditions and field ownership | Unconditional patches can overwrite the same field |
| Two API edits to different objects | Independent API writes; Reverser's branch worker serializes its commits | No atomic transaction across the objects |
| Two pushes to one Git branch | Git branch update checks; Reverser retries its own rejected push | Conflicting application intent still needs a decision |
| One API edit and one Git deployment | Each side has its own local checks | No ordering barrier prevents the deployment from overwriting the live edit |

### Two people edit through the Kubernetes API

Alice and Bob both read `replicas: 3`. Alice requests `5`; Bob requests `7`.

- If both submit updates with the resource version they read, Alice's accepted update
  changes that version. Bob's stale update gets `409 Conflict`; he must read again and
  decide whether to overwrite Alice's value.
- If both submit unconditional patches to `spec.replicas`, both can succeed. If Bob's
  patch is accepted last, the live value is `7`, until another writer changes it.
- Patches to different fields can preserve both edits. Replacing a list or parent object
  can overwrite another person's change.

Use `resourceVersion` preconditions or conditional JSON Patch when the user should decide
whether to overwrite a concurrent edit. These checks apply within Kubernetes. See
[Kubernetes update semantics](https://kubernetes.io/docs/reference/using-api/api-concepts/#updates-to-existing-resources).
[Server-side apply](https://kubernetes.io/docs/reference/using-api/server-side-apply/#conflicts)
adds field-ownership conflicts between managers, but forced ownership changes can overwrite
values. It does not coordinate publishing to Git.

Reverser observes accepted live state. A request rejected by Kubernetes does not become
a state change to publish. Separate objects can be captured independently; editing two
objects together does not make their API updates or their eventual applies atomic.

### What appears in Git history

**Two accepted API edits do not necessarily produce two commits.**
[`spec.commit.window`](configuration.md#the-commit-window-speccommitwindow) defaults to a
rolling `5s` silence window. Within one window, Reverser keeps the latest event for each
resource path. If `replicas: 5` is followed by `replicas: 7` in that window, the commit can
contain only `7`.

A window contains one target, author identity, and attribution outcome. A change to any
of those closes it. When attribution identifies Alice and Bob separately, their changes
split windows. Shared credentials or events without distinct attribution can place both
people's changes in the same window. Git history therefore is not a complete record of
every accepted API write.

Setting `window: 0s` requests per-event local commits in normal operation. It does not
make each push immediate: the branch worker has a separate push cooldown. No-op writes
need no commit, and replay after a moved remote can change unpublished commit SHAs.
Neither a shorter window nor more commits creates a lock against Flux or Argo CD.

The [commit-window specification](spec/commit-window-refactor.md) defines the flush rules.

### What sits between an edit and an apply

**A captured event, a local commit, a published commit, and an applied revision are
different states, and a change can be waiting in any of them.** Three behaviors follow,
and they are the ones that decide who wins:

- **Reverser fetches the branch tip before it writes.** A publication cycle starts with a
  fetch, so the commit is built on the branch as it stood at that moment. If a competing
  push moves the branch afterward, the push is rejected, and Reverser fetches again and
  replays its retained writes in order.
- **Replay uses the captured object.** It does not reread the live value or compare
  timestamps to pick a winner. What the watch event captured is what gets
  written onto the new base.
- **Nothing paces this end to end.** The commit window is a rolling `5s` silence timer, a
  branch worker pushes at most once every `5s`, and webhook delivery, source refresh, and
  apply each have their own queues and retries. There is no "five seconds to commit, then
  five to push".

A failed push keeps its writes for a later retry. A successful one clears what it
published, and says nothing about whether more work is already queued behind it.

### Two writers push to Git

For ordinary human Git clients, a competing push can make the local branch stop being a
fast-forward of the remote. The rejected writer fetches and integrates the new commits
before retrying. Git can merge independent changes, but conflicting edits to one field
require a choice. See [Git push behavior](https://git-scm.com/docs/git-push).

Reverser resolves its own rejected push by fetching the new tip, rebuilding its
unpublished commits on top of it, and retrying. That preserves the other writer's commits
in branch history. It **does not guarantee that both writers' field values survive in the
final manifests**, because replaying a captured live object can supersede a Git-side
change to the same object.

Different fields of one object need the same care. If Alice changes replicas in the
cluster while Bob updates an image in Git, Alice's captured object still carries the old
image, and replaying it against Bob's revision can replace his value. Supported-layout and
render checks can refuse a write. They do not merge two people's intentions.

### Timeline: a captured live value wins publication

**Within a successful publish, a retained live snapshot can overwrite a newer Git-side
value because Reverser writes it after fetching that Git revision.** Suppose both sides
start at `replicas: 3`. Alice's event contains `5`; Bob subsequently pushes `7`. Assume
the write is supported, no later event replaces Alice's snapshot, and the reconciler
has not yet applied Bob's revision:

```mermaid
sequenceDiagram
    actor Alice
    actor Bob
    participant K8s as Cluster
    participant Rev as GitOps Reverser
    participant Git as Git host
    participant Recon as Flux / Argo CD

    Alice->>K8s: set replicas to 5
    K8s-->>Rev: capture event E5
    Bob->>Git: push commit B with replicas 7
    Git-->>Recon: push webhook for B
    Note over Recon: B has not been applied
    Note over Rev: window closes with no retained pending writes
    Rev->>Git: fetch current branch tip
    Git-->>Rev: commit B with replicas 7
    Rev->>Rev: write captured E5 onto B and create C
    Rev->>Git: push C with replicas 5, parent B
    Git-->>Recon: push webhook for C
    Recon->>Git: refresh tracked branch
    Git-->>Recon: current tip C
    Recon->>K8s: reconcile C (live replicas already 5)
```

The published history is `B (7) → C (5)`. Bob's commit remains in history, but Alice's
older snapshot supplies the value at the new tip. The reconciler can skip applying `B`.
Reverser's fetch and the reconciler's source fetch are independent operations; neither
waits for the other to finish.

The same outcome is possible if Bob pushes after Reverser's initial fetch: the conditional
push rejects the stale base, then Reverser fetches `B`, replays `E5`, and retries.
For this single retained edit, the boundaries are:

| When Bob's `7` reaches the branch | Reverser's response | Tip after the successful publication described |
| --- | --- | --- |
| Before Reverser's initial fetch | Write `E5` onto Bob's revision | Reverser's `5` |
| After that fetch, before Reverser's branch update | Fetch and replay `E5` after a rejected push | Reverser's `5` |
| After Reverser's successful push | Bob can push a new commit based on the updated branch | Bob's `7` |

The final row requires Bob to integrate the new branch tip; an ordinary stale push can
be rejected. Continued contention or a refused replay can prevent Reverser's publication.
“The event wins” therefore describes a successful publication of that retained snapshot,
not a permanent priority for API edits over Git edits.

### When the reconciler's own apply comes back as an event

A reconciler's apply produces a watch event like any other write. **That event is a no-op
when its captured content matches the checkout used to build its commit**, which is the
ordinary case: Bob pushes `7`, the reconciler applies `7`, Reverser sees `7`, Git already
says `7`, and no commit is made. This is why a healthy loop does not ring forever.

It stops being a no-op when Reverser still holds unpublished work for the same field. If
Alice's `E5` is pending when the reconciler's `E7` arrives, the worker publishes `E5`
first, moving Git to `5`. By the time `E7` is finalized it is a real change, so it earns
its own commit. History reads `B (7) → C (5) → D (7)`, and both sides settle at `7`.

Two consequences belong in an operating procedure:

- **A push webhook does not prove the resulting event will be a no-op.** To establish that
  the loop has settled, check the reconciler's applied revision, the live object, and
  whether Reverser still has outstanding work. A commit that only announces a branch
  update is not the same as the value you asked for.
- **Editing different objects is not isolation** when they share a Kustomization or an
  Application. A deployment triggered by Bob's commit can reapply Alice's object too.
  Isolation requires controlling the apply scope.

## Confirm that a change is published and applied

Publishing and applying are separate observations. A successful Kubernetes edit confirms
only the API write; a successful webhook delivery confirms only receipt of a notification.

For an explicit save workflow, a [`CommitRequest`](configuration.md#commitrequest) with
`Pushed=True`, `status.sha`, and `status.branch` identifies its published commit.
`Ready=True` also includes successful no-commit outcomes. Ordinary watch-driven commits
do not automatically create a `CommitRequest`.

Then check the reconciler:

- **Flux:** require `Ready=True` and match the SHA in `status.lastAppliedRevision`.
  The revision is formatted like `main@sha1:<sha>`, rather than a bare SHA.
- **Argo CD:** require `status.sync.status: Synced` at the intended `status.sync.revision`.
  If Git already matches live state, no new sync operation is needed. For an explicit sync,
  check that the new operation succeeded at the requested revision.

If a later commit supersedes the one you are waiting for, inspect the new revision and
its content. A later descendant can revert the edit; ancestry alone does not prove it
survived. Workflows requiring an exact apply must coordinate or pin the revision.

## Handle failed pushes and refused edits

A live edit can succeed while its publication fails. The cluster already contains the
change; Git does not yet contain it. Reverser retains pending writes after push failures,
but that does not stop a reconciler from applying older or competing desired state.
Monitor push failures and the target's write-acceptance conditions.

**Retained work is not a durable event log.** It does not survive a process restart, and
an edit that failed before it became a pending write is gone from Reverser's side
entirely. Recovery is a resync, which derives state from the cluster as it exists then, so
it cannot reproduce an intermediate value that has since been overwritten. Nor does a
failure retry on a fixed schedule.

If Reverser refuses to publish an edit, **there is no commit and no push webhook**.
Choose whether to repair the destination so the edit can be saved or restore the approved
Git state. With Argo self-heal off, refreshing an unchanged, already-synced revision does
not restore the live object; restoration needs an explicit sync. Flux can restore it on
reconciliation, but a long apply interval can leave the edit live for hours.

Reverser has no automatic restoration and no applied-revision barrier. It also has no
inbound Git push receiver, so nothing tells it that somebody else moved the branch: it
finds out when it next writes. That gap and its consequences are worked out in
[inbound push notification](design/inbound-push-notification.md), and the broader
coordination question in the
[orchestrator integration design](design/support-boundary/orchestrator-reconcile-trigger.md).
The admission and audit webhooks it does serve are for validation and attribution.

## Keep controller metadata out of Git

Reverser strips Flux's operational `kustomize.toolkit.fluxcd.io/` labels and annotations,
Argo CD's `argocd.argoproj.io/tracking-id` and `installation-id`, and
`kubectl.kubernetes.io/last-applied-configuration`. This avoids commits caused only by
controller bookkeeping. The rules are in [`internal/sanitize/types.go`](../internal/sanitize/types.go).

Use **annotation resource tracking** for Argo CD on Reverser-managed paths.
`app.kubernetes.io/instance` is preserved because it can be meaningful user metadata;
Argo label tracking can therefore introduce reverse commits.

## What the e2e tests cover

The bi-directional corner exercises controlled round trips against both reconcilers:
Git/API round trips, commit counts, applied revisions, a SOPS Secret, revert and prune for
[Flux](../test/e2e/flux_bi_directional_e2e_test.go); metadata cleanup, explicit sync,
self-heal, ignored fields, and one shared field edited from both sides for
[Argo CD](../test/e2e/argocd_bi_directional_e2e_test.go).

**Neither test establishes safe simultaneous API and Git editing on a shared field.** Both
run with settling windows of a few seconds and long reconciler intervals, so a later
scheduled loop is out of scope. Push-conflict tests verify the rebuild-and-retry
mechanism; they do not establish a conflict policy. Treat the corner as regression
coverage for the configurations above, and look elsewhere for evidence about mode 4.

Run `task test-e2e-bi-directional` for the corner, which is also a CI job. `task test-e2e`
excludes it, and `task argocd-ui` opens the installed Argo CD UI. The
[corner specification](spec/e2e-bi-directional-corner.md) describes the setup and the
coverage still missing.
