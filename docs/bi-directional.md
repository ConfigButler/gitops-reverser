# Bi-directional usage guide

GitOps Reverser publishes live cluster state to Git. Flux or Argo CD applies Git back to the
cluster. Run both and a resource can be changed from either side. This guide covers what that
costs you, how to configure the two reconcilers for it, and which parts are proven by tests.

**The short version.** Point your Git host's push webhook at the reconciler, turn Argo CD's
`selfHeal` off, and accept that for anything edited live the Kubernetes API is the source of
truth. There is no merge, no lock, and no timing guarantee.

**What you are trading.** Round trips to the Git host are the seconds you wait through, so a
publication talks to the remote **twice**: it opens the push, reads where the branch is, and sends
the commits, never reading the branch beforehand. A publication with nothing to change costs one
request; a foreign push costs a handful of extra requests and a recomputation on a checkout
already on disk, and the branch still ends up correct. If your workload is mostly Git-side edits
with occasional live changes, you are using the tool against its grain. The per-operation numbers
are measured, in
[`git-roundtrip-ledger.golden`](../internal/git/testdata/git-roundtrip-ledger.golden).

```mermaid
flowchart LR
    subgraph CLUSTER["Kubernetes cluster"]
        API["Kubernetes API"]
        REV["GitOps Reverser"]
        REC["Flux / Argo CD"]
    end
    GIT[("Git branch")]

    API -->|"watch event"| REV
    REV -->|"commit, then compare-and-swap push"| GIT
    GIT -->|"push webhook"| REC
    REC -->|"apply"| API
    GIT -. "push webhook: not built yet" .-> REV

    style REV fill:#e8f4fd,stroke:#2196f3
    style REC fill:#fff3e0,stroke:#ff9800
```

Both solid legs are configuration you control. The dashed leg does not exist: nothing tells
Reverser that somebody else moved the branch. [Telling Reverser that somebody else
pushed](#telling-reverser-that-somebody-else-pushed) covers what that costs and how to work
around it.

## One queue per branch

Every watched event for a given `GitProvider` and branch lands on one queue owned by one branch
worker. Events are applied in arrival order and never overtake each other, and two `GitTarget`
objects on the same provider and branch share that worker and its ordering.

1. **Commit.** Events collapse into one local commit per author after
   [`spec.commit.window`](configuration.md#the-commit-window-speccommitwindow) of silence,
   `5s` by default. `0s` gives one commit per event.
2. **Push.** Local commits go to the remote at most once every five seconds, as a
   compare-and-swap. The push declares the SHA it expects the branch to be at, and the Git host
   refuses the update if the branch has moved off the commits' base.

That compare-and-swap is what detects a foreign push, and it does so from the remote's ref
advertisement before uploading a single object, on a connection the cycle was making anyway.

**Reverser never polls the remote and holds no timer against it**, so an idle target generates no
Git traffic at all. That is measured rather than approximate, and there is no interval setting
that changes it.

Planning does not read the branch either: the worker plans on the checkout it already has and lets
the compare-and-swap catch a move. The exceptions are a worker that has not yet seen the remote, a
previous write that failed and left the checkout in an unknown state, and a resync, which keeps
reading because it can finish without ever opening a push.

## What happens when somebody else pushed first

Your push is rejected and the worker **replays**: it fetches the new tip, re-plans every retained
write against it, and pushes again. Three attempts, then the writes stay retained for a later try.

Replaying is the cheap half: the re-planning is local and the branch ends up correct either way.
What a rejection costs is the extra conversations with the Git host around it, which is what
[a webhook pointed back at Reverser](#telling-reverser-that-somebody-else-pushed) would remove.

```mermaid
flowchart TD
    PUSH["Push, declaring the SHA the commits were based on"] --> MOVED{"Is the branch still at that SHA?"}
    MOVED -->|yes| DONE["Accepted"]
    MOVED -->|"no: somebody else pushed"| FETCH["Fetch the new tip"]
    FETCH --> REPLAN["Re-plan every retained write against the new tree"]
    REPLAN --> DIFF{"Does the captured object still differ from what Git now holds?"}
    DIFF -->|no| NOOP["No commit. The other writer's value stands."]
    DIFF -->|yes| COMMIT["Commit on top, then push again"]
    COMMIT --> MOVED

    style DONE fill:#e8f5e9,stroke:#43a047
    style NOOP fill:#e8f5e9,stroke:#43a047
```

Replay re-plans, it does not rebase a diff. Each retained write still carries the object as the
watch captured it, and writing that object onto the new tree has one of two outcomes:

- **No change, so no commit.** The file in Git already says what the captured object says,
  because whoever pushed made the same change. Git had it first and that is the end of it.
- **A change, committed on top.** The captured object is written over the other writer's value,
  and the branch ends at the API side's value.

There is no third outcome, and there is no merge: a watch event carries the object's whole state,
with no retained baseline that would say which fields changed on each side. The
[deferred merge investigation](future/git-api-three-way-comparison.md#what-three-way-merge-means-here)
defines the comparison that would take.

### What a write does to the file

The writer edits fields in place within the existing document, so what matters is which fields
change rather than which files. For a plain manifest:

| In the file | What a replay does to it |
| --- | --- |
| A field the live object also carries | Set to the live value |
| A field in Git that the live object does not have | **Removed** |
| Comments, key order, other documents in the file | Preserved |
| An edit that cannot be placed safely | Refused, and the file is left untouched |

In a kustomize layout the live object is the rendered result rather than the contents of any one
file, so the edit is projected back through the build first and only the fields that document is
responsible for are touched. An edit the projection cannot place is refused rather than guessed
at, which is why the last row exists.

**The consequence to plan around.** If Alice changes `replicas` through the API while Bob changes
`image` in Git on the same object, replaying Alice's captured object writes her `replicas` and
also the `image` she was looking at. Bob's change to a different field of the same object does
not survive, and a field Bob added that the cluster never had is removed outright. Separate
objects are independent of each other. Separate fields of one object are not.

## There are no timing guarantees

Committing and pushing takes on the order of a second at best, and the Kubernetes API accepts a
great many changes in a second. Nothing paces the two against each other and nothing reserves a
window, so treat live editing as a different way of making changes rather than a faster
`git push`:

- You make a change and then observe that it arrived. You do not save and receive a confirmation.
- Intermediate values may never reach Git. Two edits inside one commit window produce one commit
  holding the later value.
- Git history records the states the worker captured and published. Expect gaps between them.

When a specific change has to be confirmed in Git, use a
[`CommitRequest`](configuration.md#commitrequest). `Pushed=True` with `status.sha` and
`status.branch` names the commit it produced.

## Why the loop stops

The obvious fear is a feedback loop: Reverser commits, the reconciler applies, the apply produces
a watch event, Reverser commits again. With no intervening edits or admission changes, it settles
instead.

```mermaid
sequenceDiagram
    actor Alice
    participant API as Kubernetes API
    participant Rev as GitOps Reverser
    participant Git as Git branch
    participant Rec as Flux / Argo CD

    Alice->>API: set replicas to 5
    API-->>Rev: watch event, replicas 5
    Rev->>Git: commit and push
    Git-->>Rec: push webhook
    Rec->>Git: fetch the new revision
    Rec->>API: apply, replicas 5
    Note over API: live value is already 5,<br/>so the apply changes nothing
    API-->>Rev: watch event, if one is produced at all
    Note over Rev: sanitized content equals<br/>the file in Git, so no commit
```

When the reconciler applies a commit Reverser wrote, the live object already holds those values,
because they were read from that object in the first place. The apply changes nothing, the event
it produces carries content identical to the file already in Git, and no commit is made. In
general: **applying a resource that already matches does not produce a change.** What can break
that reasoning is controller bookkeeping.

## Keep controller bookkeeping out of Git

Argo CD stamps `argocd.argoproj.io/tracking-id` on every non-CRD object it applies, Flux stamps
`kustomize.toolkit.fluxcd.io/` labels and annotations, and a client-side apply writes
`kubectl.kubernetes.io/last-applied-configuration`. None of it is user intent. If it reached Git
the sanitized live object would stop matching the committed file, and every reconciler sync would
produce a commit.

Reverser strips all of it before writing, along with `argocd.argoproj.io/installation-id`,
`kro.run/`, `applyset.kubernetes.io/`, and `kcp.io/cluster`. The rules and the reasoning for each
are in [`internal/sanitize/types.go`](../internal/sanitize/types.go). Under
`argocd.argoproj.io/` only the exact tracking keys go: siblings such as `sync-wave`,
`sync-options`, and `hook` are user intent that belongs in Git.

**Leave Argo CD on its default `annotation` resource tracking for Reverser-managed paths.** The
`label` and `annotation+label` methods stamp `app.kubernetes.io/instance`, which is
indistinguishable from the standard recommended label that Helm and Kustomize set for application
reasons. Reverser therefore does not strip it, and label tracking would put Argo's bookkeeping
into your commits.

## Configure Argo CD

`selfHeal` is the setting that decides what happens to a live edit.

| `selfHeal` | What happens to a live edit | Use when |
| --- | --- | --- |
| `true` | Argo reverts it, in about a second in our e2e corner, usually before Reverser can publish. The edit is lost and Git records a two-commit flap. | The path is Git-owned and drift is an error |
| `false` | It survives, is captured to Git, and is applied onward from there. Both directions work on the same field. | Anything edited through the API |

Both rows are measured outcomes. Neither is an ordering guarantee: self-heal is watch-driven and the first
revert of fresh drift has zero backoff, while Argo notices a new Git revision only on its timed
refresh (`timeout.reconciliation`, 120s plus up to 60s of jitter) or when a webhook tells it. And
`selfHeal: false` only suppresses repeating a sync of the same revision and parameters, so a new
revision or a manual sync still applies Git values over an unpublished API edit.

There is no knob that delays that first revert.
[Why `selfHeal` cannot be made selective](design/support-boundary/argocd-bi-directional.md) walks
the code paths, including the options that look like they would work, and the
[apply and field-ignore source review](facts/gitops-apply-and-field-ignore.md) separates Argo's
scheduling, comparison, and write paths.

```yaml
spec:
  syncPolicy:
    automated:
      selfHeal: false
      prune: true
```

Use `prune: true` only when a deletion in Git should delete the live object. Then configure the
[Argo CD webhook](https://argo-cd.readthedocs.io/en/stable/operator-manual/webhook/) on your Git
host, pointing at `https://<argocd-server>/api/webhook` with a shared secret. In the e2e corner
that path drives a sync about four seconds after the push. Without it you wait for the timed
refresh. A webhook only requests a refresh: an apply follows only where a sync is permitted, so it
does not substitute for automated sync.

### The split-ownership alternative

`ignoreDifferences` on a field, with `RespectIgnoreDifferences=true`, keeps `selfHeal: true`
everywhere else and hands that one field to the API. Its differences alone do not make the
Application `OutOfSync`, and the respect option copies the live value into the apply target, so an
ordinary sync preserves it. Without that option, a later sync can still write the field from Git.

The cost is that the field stops being Git-driven on an existing object: Git seeds it at creation,
but later Git edits to it do not reach the object while the ignore is active. It buys authority for
one named field, and protects no other field from Reverser's whole-object replay. The e2e corner
checks that an API edit survives and is published back to Git; it does not test a later Git edit to
the ignored field.

## Configure Flux

Flux has no `selfHeal` toggle: kustomize-controller corrects drift on every reconcile, so a live
edit survives until the next apply of a revision that still carries the old value. The two
settings that matter are therefore the apply interval and how quickly a push reaches Flux.

```yaml
# GitRepository: short, so a missed webhook becomes a delay rather than a stall.
spec:
  interval: 1m
---
# Kustomization on the shared path: long, so the timer is not what reverts live edits.
spec:
  interval: 24h
  retryInterval: 1m
  suspend: false
```

Configure a [Flux `Receiver`](https://fluxcd.io/flux/guides/webhook-receivers/) against the
**`GitRepository`**, not the Kustomization. Source-controller fetches Git and publishes a new
artifact revision, which wakes the dependent Kustomizations. A Receiver aimed at the Kustomization
can re-apply the artifact Flux already has cached, which is the old content.

A long interval delays periodic correction. It reserves nothing: a new source revision, a
configuration change, a retry, or a controller restart all trigger an apply sooner. Keep the
Kustomization **unsuspended**, because suspension blocks source-triggered applies as well and a
manual reconcile request does not bypass it. To trigger the source and the apply together:

```bash
flux reconcile kustomization editing -n flux-system --with-source
```

Flux's equivalent of split ownership is `Kustomization.spec.ignore`, added in kustomize-controller
`v1.9.0`, with the same trade as Argo's: selected live fields survive later applies, Git seeds them
at creation, later Git edits to them do not land, and Reverser still captures them back into Git.
Our e2e installs Flux `2.9.5` but no spec exercises `spec.ignore`, so it is upstream behavior we
have read in the source and have no test of our own behind; the
[source review](facts/gitops-apply-and-field-ignore.md) has the code paths.

## Telling Reverser that somebody else pushed

Everything above points a webhook from the Git host at the reconciler. The other direction does
not exist yet, so what a moved branch costs depends on what the target is doing:

- **Actively writing:** nothing breaks. The compare-and-swap catches the move and replay handles
  it. It is only slower than it needs to be, because the move is discovered by a push that was
  always going to fail.
- **Refused:** it recovers on its own. While `GitPathAccepted` is `False` the target is not
  converged, so it requeues every 10 seconds and each pass forces a re-read. Fix an unsupported
  folder in Git and the condition clears within about ten seconds.
- **Healthy and idle:** it does not. A converged target requeues every 5 minutes and those passes
  publish status without touching Git, so it holds its previous answer about the folder until it
  next writes.

Force a re-read of an idle target:

```bash
kubectl annotate gittarget editing -n gitops-reverser \
  reconcile.configbutler.ai/requestedAt="$(date +%s)" --overwrite
```

To bound it instead of watching for it, set `--base-trust-max-age` on the controller: a `GitTarget`
that has not re-read its folder within that age is made to, whether or not it has anything to
publish. It is off by default, because it trades requests for freshness: one folder re-read per
target per interval, including targets that are perfectly quiet. The age is per target rather than
per branch, so that one busy target cannot postpone a quiet one beside it indefinitely by renewing
the shared checkout.

A receiver that does this automatically is designed in
[inbound push notification](design/push-notification-and-reconcile-trigger.md); the other half of
that design, removing the head-of-cycle fetch, has shipped. Its request shape is
[§8.3, the wire contract](design/push-notification-and-reconcile-trigger.md#83-the-wire-contract-for-whoever-calls-it):
one signed `POST /git-push/<route>` carrying the repository, the branch, and the SHA the branch now
points at. It deliberately does not take a Git host's native payload, so the mapping from your
host's webhook belongs in the host's own configuration or in a relay.

One rule matters if you build anything similar yourself: **a notification that a branch moved must
not trigger a fresh cluster snapshot.** Our handler and the reconciler's both fire on the same
push, and ours has less work to do, so it would reliably publish the pre-push cluster state over
the incoming change before the reconciler ever applied it. That is also why the design does not
wire a webhook to the annotation above, which asks for exactly such a resync. A push tells you
about Git; it says nothing about the cluster.

## When a write is refused or a push fails

A live edit can succeed while its publication does not. The cluster holds the change and Git does
not, and **a refused write writes no file, so nothing reaches the reconciler through the ordinary
path**. Watch push failures and the target's write-acceptance conditions: a successful Kubernetes
edit confirms the API write and nothing beyond it.

Retained writes are held in worker memory. They do not survive a restart, and an edit that failed
before becoming a pending write is gone from Reverser's side. Recovery is a resync, which derives
state from the cluster as it exists at that moment, so it cannot reproduce an intermediate value
that has since been overwritten.

Reverser performs no restoration of its own, with one opt-in exception:
[`spec.onRefusal: PushEmptyCommit`](configuration.md#reverting-a-refused-edit-specconrefusal)
answers a refused edit with a commit that changes no file, which moves the branch and so asks both
reconcilers to re-apply. It is off by default and its blast radius is the branch, not the object.
Otherwise, with Argo `selfHeal` off, refreshing an already-synced revision will not put the
approved value back; that needs an explicit sync, and Flux restores it on its next apply, which a
24h interval can leave hours away.

## What the tests prove

The bi-directional corner runs against deployed Flux and Argo CD. Its timing and event-count
assertions describe those scenarios; they do not guarantee the ordering of concurrent writers.

| Covered | Where |
| --- | --- |
| Argo bookkeeping never reaches Git, asserted as zero commits during a sync | [Argo phase 1](../test/e2e/argocd_bi_directional_e2e_test.go) |
| `selfHeal: true` loses the API edit, and the flap is exactly two commits | Argo phase 2 |
| `ignoreDifferences` keeps the edit and holds the Application `Synced` | Argo phase 3 |
| `selfHeal: false` keeps the API edit and still applies a Git-side change to the same field | Argo phase 4 |
| Git to cluster to Git round trip settles without a commit loop | [Flux](../test/e2e/flux_bi_directional_e2e_test.go) |
| A SOPS-encrypted Secret round-trips without a reverse commit | Flux |
| A Git revert removes the live object and the committed file | Flux |
| Waking only the `GitRepository` makes the Kustomization apply a new revision by itself, reverting live drift, with an **empty** commit as that revision | Flux |

Two gaps worth knowing. The **Flux spec drives reconciliation manually** with 30m intervals, so
only the Argo webhook is exercised; the mechanism a `Receiver` depends on is covered by the last
row, which wakes the `GitRepository` alone and lets the Kustomization apply on its own. And **no
spec tests simultaneous writes to one field from both sides**, because the outcome depends on
arrival order the corner does not control. The replay mechanism is covered; a conflict policy is
not, because there is not one.

Run `task test-e2e-bi-directional` for the corner, which is also a CI job. `task test-e2e`
excludes it, and `task argocd-ui` opens the installed Argo CD UI. The
[corner specification](spec/e2e-bi-directional-corner.md) describes the setup and the coverage
still missing.
