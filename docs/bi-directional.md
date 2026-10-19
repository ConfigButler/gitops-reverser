# Bi-directional GitOps

GitOps Reverser writes live cluster state to Git. Flux and Argo CD apply Git to the cluster. This
page covers the one setup where the two meet on the same folder, so that a resource can be changed
from either side: who decides what, which states the system moves through, and what stays exposed.
The complete setups are [bi-directional GitOps with Flux](bi-directional-flux.md) and
[bi-directional GitOps with Argo CD](bi-directional-argocd.md).

## Do you need any of this?

Only when Flux or Argo CD applies a folder that GitOps Reverser also writes. Every other setup is
ordinary GitOps and needs nothing from this page.

| Your setup | What you configure |
| --- | --- |
| GitOps Reverser mirrors a folder that nothing applies: an audit trail, a backup, the start of a repository | A `GitTarget` and its watch rules |
| Flux or Argo CD applies other folders or other repositories, next to GitOps Reverser | Nothing. Flux and Argo CD stay exactly as they are |
| Flux or Argo CD applies a folder that GitOps Reverser writes | An apply trigger, as this page describes |

```mermaid
flowchart TD
    Q1{"Does Flux or Argo CD apply a folder<br/>that GitOps Reverser writes?"}
    Q1 -->|no| ONEWAY["One-way. Mirror only, or GitOps as usual beside it.<br/>Nothing on this page applies"]
    Q1 -->|yes| Q2{"Does an apply trigger hold that applier?"}
    Q2 -->|yes| BI["Bi-directional. GitOps Reverser decides when it applies"]
    Q2 -->|no| EXPOSED["Shared folder without a trigger.<br/>Works, and the applier can revert live edits"]

    style ONEWAY fill:#e8f5e9,stroke:#43a047
    style BI fill:#e8f5e9,stroke:#43a047
    style EXPOSED fill:#fff3e0,stroke:#ff9800
```

Everything bi-directional is opt-in and off by default: the apply triggers, the push receiver,
`onRefusal: Reapply`, the chart's `applierAccess` grants, and the `ClusterProvider`'s
`allowCrossNamespace.appliers`, which a trigger needs to hold an applier outside its own namespace,
such as one in `flux-system` or `argocd`. A `GitTarget` without `spec.applyTriggerRef` never reads,
patches or syncs anything of Flux's or Argo CD's, and neither does a trigger no `GitTarget` names, so
a `Kustomization` keeps its interval and an `Application` keeps automated sync.

**A shared folder without a trigger works, and is exposed.** The applier keeps its own schedule,
and that schedule cannot see the edits GitOps Reverser has not pushed yet
([why](#why-the-applier-cannot-decide-for-itself)). An apply at the wrong moment reverts a live edit,
and GitOps Reverser then commits the revert as the applier's change. With Argo CD's
`selfHeal: true`, a live edit is reverted within a second, before it can reach Git at all. If you
run a shared folder this way, keep [controller bookkeeping out of Git](#keep-controller-bookkeeping-out-of-git),
and consider [split ownership](#the-split-ownership-alternative) for the fields both sides change.

## How it works

GitOps Reverser decides when the cluster is applied from Git. Flux or Argo CD keeps fetching,
rendering, applying, pruning and reporting health, and applies when GitOps Reverser asks. Three
things set that up:

1. **An apply trigger holds the applier.** A `FluxTrigger` holds a Flux `Kustomization`, an
   `ArgoCDTrigger` an Argo CD `Application`, and the `GitTarget` names the trigger in
   `spec.applyTriggerRef`. A trigger acts only while an eligible `GitTarget` names it: one in its
   namespace, admitted by its `ClusterProvider`s, that writes the repository and branch the applier
   fetches.
2. **The applier's own triggers are off.** The `FluxTrigger` keeps its `Kustomization` suspended;
   the `Application` runs with automated sync off.
3. **The Git host's push webhook reaches GitOps Reverser**, so a Git-side push is applied in
   seconds. See [telling GitOps Reverser that somebody else pushed](#telling-gitops-reverser-that-somebody-else-pushed).

```mermaid
flowchart LR
    subgraph CLUSTER["Kubernetes cluster"]
        API["Kubernetes API"]
        REV["GitOps Reverser"]
        APP["Applier<br/>Flux Kustomization<br/>or Argo CD Application"]
    end
    GIT[("Git branch")]
    HUMAN(["Somebody pushes to Git"])

    API -->|"watch event"| REV
    REV -->|"commit, then compare-and-swap push"| GIT
    HUMAN --> GIT
    GIT -->|"push webhook"| REV
    REV ==>|"apply request"| APP
    APP -->|"fetch and apply"| API

    style REV fill:#e8f4fd,stroke:#2196f3
    style APP fill:#fff3e0,stroke:#ff9800
```

The two appliers get the same contract by different means:

| | Flux | Argo CD |
| --- | --- | --- |
| Held by | A `FluxTrigger` | An `ArgoCDTrigger` |
| Kept from applying on its own by | `spec.suspend`, which the trigger owns | Automated sync off, which you set on the `Application` |
| One apply is | A resume for one reconcile, then a suspend | A sync operation pinned to the commit |
| Catches up with GitOps Reverser's own pushes | Yes, once the branch is quiet for `quietPeriod` | No: nothing it holds goes stale |
| The tool's own push webhook | Removed: GitOps Reverser's receiver replaces it | May stay: with automated sync off it only compares |
| A human override | `flux resume` applies once; it is suspended again within a minute | Sync applies; GitOps Reverser waits for it to end |
| Handing it back | Delete the trigger, or stop naming it from every `GitTarget`: the `Kustomization` is resumed | Delete the trigger, or stop naming it, then switch automated sync on |
| Complete setup | [Bi-directional GitOps with Flux](bi-directional-flux.md) | [Bi-directional GitOps with Argo CD](bi-directional-argocd.md) |

## Why the applier cannot decide for itself

An apply is safe only when nothing live is missing from the commit it applies. Between an edit and
its push, the edit is live in the cluster and absent from Git: it sits in an open commit window or
in the branch worker's pending writes. An apply at that moment writes Git's older value over it,
GitOps Reverser mirrors that revert faithfully, and the edit is gone.

Only GitOps Reverser can see the pending edits, so every trigger the applier runs by itself is an
apply nobody checked:

- **A Git-host webhook into the applier** fires on every push, GitOps Reverser's own included. Two
  API edits a few seconds apart are enough for the first commit to be applied over the second edit.
- **Argo CD's automated sync** syncs every commit it has not synced yet, and every push GitOps
  Reverser makes is one. Two to three minutes after each push (`timeout.reconciliation`, 120s plus
  jitter), the `Application` is synced over whatever is pending. `selfHeal: false` does not change
  that.
- **Flux's intervals** apply on every reconcile, whatever woke it: on the interval, on a new
  artifact, on a controller restart.

None of these can be filtered from outside. They are switched off, and GitOps Reverser makes the
requests instead.

## The quiet branch

Every apply GitOps Reverser asks for waits for a **quiet branch**: no open commit window and no
pending writes, on this branch worker or on any other worker writing the same repository and branch
through another `GitProvider`. That is the moment nothing live is missing from the tip.

```mermaid
stateDiagram-v2
    direction LR
    Quiet --> Busy: an API edit opens a commit window
    Busy --> Busy: more edits, commits and pushes
    Busy --> Quiet: every window closed, every write pushed
    note right of Quiet
        An apply starts only here,
        after the decision is checked again
    end note
```

A foreign commit does not wait for open windows to close on their own. GitOps Reverser closes them,
replays the pending writes onto the foreign commit, pushes, and asks for the commit it just pushed.
The applied commit holds the API side's values, so applying it reverts nothing:

```mermaid
sequenceDiagram
    actor Bob as Git-side author
    participant Git
    participant Rev as GitOps Reverser
    participant App as Applier
    participant API as Kubernetes API

    Note over Rev,API: Alice's edit W is live, in an open commit window
    Bob->>Git: push F
    Git-->>Rev: push webhook: the branch is at F
    Rev->>Rev: close the window, fetch F, replay W onto F
    Rev->>Git: compare-and-swap push F + W
    Rev->>App: apply request: commit F + W, reason ForeignCommit
    App->>API: apply F + W
    Note over API: W's objects already hold W's values,<br/>so only Bob's change lands
```

## When GitOps Reverser asks for an apply

| Reason | When | Which applier |
| --- | --- | --- |
| `ForeignCommit` | The branch gained a commit GitOps Reverser did not push | Every applier declared on the branch |
| `Refusal` | An edit was refused and the target has `onRefusal: Reapply` | The refusing target's applier |
| `Repair` | An apply reverted an API edit made while it ran; Git kept the edit | That applier |
| `CatchUp` | The branch has been quiet for the `FluxTrigger`'s `quietPeriod` after GitOps Reverser's own pushes, since the operator started, or since a `GitTarget` first named the trigger | Every Flux `Kustomization` on the branch |

GitOps Reverser's own commits hold values it read from the cluster, so applying them changes
nothing, and only Flux is asked to catch up with them: its inventory learns of an object only from
an apply of a commit that holds it, and an object it never applied is not pruned when Git later
deletes it. An `Application` with automated sync off applies nothing by itself, so nothing it holds
goes stale.

Several `GitTarget`s may name one trigger, for a `Kustomization` or `Application` whose path covers
several folders: the applier is asked once for all of them.

## The life of an apply

An apply is an **operation**, recorded on the trigger's `status.operation` before the applier is
asked, so a restarted operator finds it and follows it to its end:

```mermaid
stateDiagram-v2
    [*] --> Due: ForeignCommit, Refusal, Repair or CatchUp
    Due --> Due: tip moved or an edit is pending, decide again later
    Due --> Requested: quiet branch, decision still holds, operation recorded
    Requested --> Running: the applier is seen working on it
    Running --> Settling: Flux only, applied and suspended again
    Running --> Succeeded
    Running --> Failed
    Settling --> Succeeded
    Settling --> Failed
    Requested --> Unresolved: not completed in 5 minutes
    Running --> Unresolved
    Settling --> Unresolved
    Unresolved --> Succeeded
    Unresolved --> Failed
    Failed --> Due: after the applier's backoff
    Succeeded --> Due: an edit was withheld during the apply (Repair)
    Succeeded --> [*]
```

`Due` lives only in the branch worker's memory: an apply it means to ask for, and nothing shows it
until it is asked. Every other state is `status.operation.phase`. While an operation is in flight
(`Requested`, `Running`, `Settling` or `Unresolved`), the applier is asked nothing else.

- **Just before it asks, GitOps Reverser checks the decision again**: the commit is still the branch
  tip, no edit waits to be published, and each `GitTarget` still names this trigger on this branch,
  is still admitted by its `ClusterProvider`s, and, for a `Refusal`, still consents; and the trigger
  still holds the applier. When any of that
  changed, it asks nothing and decides again at the next quiet moment. It never asks for something
  weaker than what it was told.
- **An edit made while the apply runs keeps its value in Git.** The apply writes the older commit
  over it on the cluster. GitOps Reverser recognizes that write by the applier's field manager,
  withholds it from Git, and asks for a `Repair`, which applies the tip that holds the edit. Only a
  write proven to be the applier's is withheld: its field manager alone, with an entry newer than the
  edit. One that ties with the edit is mirrored, and a delete never is withheld. Each such write is
  counted in `apply_window_collisions_total`.
- **A failed operation is decided again from scratch**, after that applier's own backoff (2 seconds
  doubling to 5 minutes), on a quiet branch, at the tip as it is then.
- **An operation still in flight after 5 minutes is `Unresolved`.** It is still followed, nothing
  ends it by timer, and nothing more is asked of that applier until it completes.

A request asks; it never reports that the apply happened. Requests run off the publication path,
and a failed one never fails a publication that succeeded. Read the applier's own status for what
it applied.

## Telling GitOps Reverser that somebody else pushed

GitOps Reverser learns that the branch moved in three ways, and each feeds the same decision:

| Signal | Latency | Configure |
| --- | --- | --- |
| A push webhook from the Git host | Seconds | `GitProvider.spec.receiver` |
| A rejected compare-and-swap push | At the next publication | Nothing: it is how every push works |
| The periodic refresh | Within `--git-refresh-interval` (`10m`) plus the 5-minute reconcile tick | Nothing: on by default |

To serve the receiver:

1. Set the chart's `receiver.bindAddress` (for example `":9292"`). The chart adds a
   `<release>-receiver` `Service`. The listener is plain HTTP; expose it to your Git host with an
   Ingress or Gateway route that terminates TLS, the way Flux's `webhook-receiver` is exposed.
2. Create a Secret with the webhook secret under the key `token`, in the `GitProvider`'s namespace.
   It is the key a Flux `Receiver` Secret uses, so an existing one can be copied over.
3. Set `GitProvider.spec.receiver` with that Secret and the host's `type`: `github`, `gitlab`,
   `gitea`, or `generic-hmac` for a relay of your own.
4. Read the path from `status.webhookPath` and register `https://<your route><path>` as a push
   webhook with the same secret:

   ```bash
   kubectl get gitprovider platform -n team-a -o jsonpath='{.status.webhookPath}'
   # /git-push/3f9a…c41e
   ```

   The path is derived from the token, as a Flux `Receiver` path is. A wrong path and a wrong
   secret both answer `404`; the operator's log says which. Rotating the token moves the path.

A delivery is only a hint. GitOps Reverser reads the branch's ref advertisement and decides from
what the remote says, so a late delivery, a duplicate, or one for its own push applies nothing it
should not. When it cannot tell whether a commit is its own, it treats it as foreign. The
[configuration reference](configuration.md#receiving-push-notifications-specreceiver) has every
field.

**A push never triggers a cluster snapshot.** The cluster still holds the values from before the
push, so a snapshot would publish them over the incoming change.

## When both sides edit the same object

The API side wins. When a push is rejected because somebody else pushed first, the branch worker
fetches the new tip, re-plans every pending write against it, and pushes again. Each pending write
carries the whole object as the watch captured it, so writing it onto the new tree either changes
nothing (the other writer made the same change) or writes the API side's value over the other
writer's. There is no merge: a watch event has no baseline that says which fields changed on each
side. [API-first publication](api-first-publication.md) has the replay in detail.

### What a write does to the file

The writer edits fields in place within the existing document, so what matters is which fields
change rather than which files:

| In the file | What a write does to it |
| --- | --- |
| A field the live object also carries | Set to the live value |
| A field in Git that the live object does not have | **Removed** |
| Comments, key order, other documents in the file | Preserved |
| An edit that cannot be placed safely | Refused, and the file is left untouched |

In a kustomize layout, the edit is projected back through the build first, and only the fields that
document is responsible for are touched. An edit the projection cannot place is refused.

**The consequence to plan around.** If Alice changes `replicas` through the API while Bob changes
`image` in Git on the same object, Alice's captured object carries her `replicas` and the `image` she
was looking at. Bob's change to that object does not survive, and a field Bob added that the cluster
never had is removed. Separate objects are independent of each other; separate fields of one object
are not.

### The split-ownership alternative

Hand a field to one side instead. Argo CD's `ignoreDifferences` with `RespectIgnoreDifferences=true`
in `syncOptions`, and Flux's `Kustomization.spec.ignore` (kustomize-controller `v1.9.0` and later),
make the applier leave that field alone. The cost is that the field stops being Git-driven on an
existing object: Git seeds it at creation, and later Git edits to it do not reach the object. It
protects no other field from the whole-object replay. The
[source review](facts/gitops-apply-and-field-ignore.md) has the code paths.

## There are no timing guarantees

Committing and pushing takes on the order of a second at best, and the Kubernetes API accepts a
great many changes in a second. Treat a live edit as a change you observe arriving, not a save that
returns a confirmation. Two edits inside one commit window produce one commit holding the later
value, so Git history records the states the worker captured, with gaps between them. When a
specific change has to be confirmed in Git, use a [`CommitRequest`](configuration.md#commitrequest).

## Keep controller bookkeeping out of Git

Argo CD stamps `argocd.argoproj.io/tracking-id` on every non-CRD object it applies, Flux stamps
`kustomize.toolkit.fluxcd.io/` labels and annotations, and a client-side apply writes
`kubectl.kubernetes.io/last-applied-configuration`. None of it is user intent. If it reached Git,
the live object would stop matching the committed file, and every apply would produce a commit.

GitOps Reverser strips all of it before writing, along with `argocd.argoproj.io/installation-id`,
`kro.run/`, `applyset.kubernetes.io/`, and `kcp.io/cluster`; the rules and reasons are in
[`internal/sanitize/types.go`](../internal/sanitize/types.go). Under `argocd.argoproj.io/` only the
exact tracking keys go: `sync-wave`, `sync-options` and `hook` are user intent that belongs in Git.
That is also why the loop settles: an applied commit GitOps Reverser wrote matches the live object,
so the watch event it produces commits nothing.

**Leave Argo CD on its default `annotation` resource tracking for mirrored paths.** The `label` and
`annotation+label` methods stamp `app.kubernetes.io/instance`, the same label Helm and Kustomize set
for application reasons, so GitOps Reverser cannot strip it, and Argo CD's bookkeeping would reach
your commits.

## Choosing `spec.onRefusal`

A refused edit has no legal destination in Git, so nothing is committed for it, and it stays live
until something applies Git over it.

| Policy | Response to a refused edit | Trade |
| --- | --- | --- |
| `Ignore` (default) | Report the refusal | Recovery is left to a human or a later apply |
| `Reapply` | Ask the target's applier to apply the tip, once the branch is quiet | The refused edit is reverted; nothing still waiting to be published is |

`Reapply` needs an apply trigger. It fires only for an edit to a document the folder already holds,
never for a write that removes one, and a reverted edit does not clear the refusal: the target
returns to `Ready=True` only after a resync covering the refused scope. The eligibility rules are in
[reverting a refused edit](configuration.md#reverting-a-refused-edit-speconrefusal).

## What is still exposed, and what it costs

- **An edit made while a requested apply runs** is reverted on the cluster until the `Repair`. It
  keeps its value in Git. It can happen only during an apply GitOps Reverser asked for: after a
  foreign push, a refusal with `Reapply`, or a Flux catch-up.
- **A human** running `flux resume` or pressing Sync applies whenever they do. GitOps Reverser
  suspends a resumed `Kustomization` again within a minute.
- **GitOps Reverser becomes the only path to a deployment.** While it is down, the shared folders
  wait: a push reaches the cluster only through a manual sync or `flux resume`. A graceful stop
  suspends a `Kustomization` it had resumed; after a crash, the apply that was running finishes,
  and the next process suspends it. Monitor the operator the way you monitor the applier.
- **A trigger may hold any applier its `ClusterProvider` reaches.** With
  `allowCrossNamespace.appliers`, every namespace the provider admits may hold a `Kustomization` or
  `Application` in another namespace whose source is the repository its `GitProvider` names. That
  is a delegation by the platform admin: nothing proves the namespace owns the applier, or may
  write to the repository, since the binding compares URLs.
- **Pending writes live in worker memory.** They do not survive a restart; recovery is a resync of
  the cluster as it is then.
- **Round trips to the Git host are the seconds you wait through.** A foreign push costs a fetch, a
  replay and one push. If your workload is mostly Git-side edits with occasional live changes, you
  are using the tool against its grain. The measured numbers are in
  [`git-roundtrip-ledger.golden`](../internal/git/testdata/git-roundtrip-ledger.golden).

## What the tests prove

Each tool guide lists what its unit and end-to-end tests cover:
[Flux](bi-directional-flux.md#what-the-tests-prove) and
[Argo CD](bi-directional-argocd.md#what-the-tests-prove). The bi-directional corner runs against
deployed Flux and Argo CD, each configured as those guides say; `task test-e2e-bi-directional` runs
it, and the [corner specification](spec/e2e-bi-directional-corner.md) describes the setup.

**No spec tests simultaneous writes to one field from both sides**, because the outcome depends on
an arrival order the corner does not control. The replay is covered; a conflict policy is not,
because there is not one.
