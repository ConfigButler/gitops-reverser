# Inbound push notification: telling a GitTarget its branch moved

> **design**: open, not yet built. Index: [`../INDEX.md`](../INDEX.md)
> Date: 2026-09-18.
> Related: [`reconcile-triggering.md`](reconcile-triggering.md) (§5 is the general mechanism this
> page narrows), [`../bi-directional.md`](../bi-directional.md),
> [`support-boundary/orchestrator-reconcile-trigger.md`](support-boundary/orchestrator-reconcile-trigger.md)

**The short version.** GitOps Reverser learns that its tracked branch moved only as a side effect
of writing to it. Nothing polls the remote, and nothing outside the process can ask it to look.
The chain that would fix this is already built except for its first link:
`reconcile.configbutler.ai/requestedAt` already forces a remote re-read, so the missing piece is
the adapter that sets that annotation when a Git host reports a push.

**This is a freshness and efficiency change. It is not a correctness fix, and the difference is
the reason it lives here rather than in the bi-directional guide.** A write never lands on a stale
base: a publication cycle fetches and resets to the remote tip before it plans anything. What
suffers is what an operator can *see* between somebody else's push and our next write, plus some
recomputation when the remote moves while a cycle is in flight.

## 1. Every fetch in the system today

Five call sites reach the remote. None is on a timer, and none can be reached from outside the
process:

| Call site | Trigger | Covers |
| --- | --- | --- |
| [`ensureRepositoryInitialized`](../../internal/git/branch_worker.go) | worker start | first clone |
| `bootstrapPathIfNeeded` | first-time path bootstrap | one-off |
| `commitPendingWrites` | start of a publication cycle, when nothing is retained | the common case |
| `fetchRemoteBranchHash` | a rejected conditional push | contention |
| `syncWithRemote` | nothing: `SyncAndGetMetadata` has no caller | dead |

That last row is the archaeology worth recording. `SyncAndGetMetadata` still carries the comment
"*This is now called by SyncAndGetMetadata() during controller reconciliation*", and it is not.
There was a periodic drift-detection path and it is no longer wired to anything. Deleting it or
reviving it is a decision this page wants made, because leaving a plausible-looking poller in the
tree is how the gap below stays invisible.

`GitTarget` requeues on `RequeueSteadyInterval` (5 minutes), but that pass publishes status and
never touches Git.

## 2. What the gap costs

### 2.1 An idle cluster never notices a foreign push

With no live edits there is no publication cycle, so there is no fetch. Somebody repairs the
folder in Git, or breaks it, and Reverser holds its previous answer until the next write or an
explicit request.

The sharpest form of this is a refusal. The acceptance gate judges the local clone, which the code
says in as many words: `refreshRemoteAndRebuildPendingWrites` exists "so the acceptance gate
evaluates the newest remote tree instead of a stale local clone". Fix an unsupported folder in Git
and `GitPathAccepted` stays `False` until something forces the re-read. The operator's repair and
the operator's feedback are disconnected, which is the worst property a status field can have.

### 2.2 A mid-cycle move discards a finished plan

The cycle-start fetch means the wasteful window is narrower than it first appears. It opens only
when the remote moves **between** that fetch and the push. Then `runPushCycle` takes the
rejection, fetches, and calls `rebuildPendingWrites`, which re-runs `executePendingWrites` over
every retained write: re-plan, re-render, re-commit against the rebased worktree.

Be precise about what that costs, because the obvious guess is wrong. It is CPU and Git object
writes. It is **not** API traffic: the prune-policy re-read in `tightenPendingPruneModes` goes
through the manager's informer cache (only `corev1.Secret` is cache-disabled, see
[`cmd/main.go`](../../cmd/main.go)) and is memoized per key for the pass. Retained writes are held
in worker memory, so nothing is re-fetched from the cluster either.

So §2.2 is a real cost but a small one, and it scales with contention rather than with traffic.
**§2.1 is the case that justifies building anything.**

## 3. What is already built

The annotation chain works end to end today, and it is worth naming each link because the
remaining work is only the link in front of it:

```text
reconcile.configbutler.ai/requestedAt changes
  -> reconcileRequestTracker.take()            (once per distinct value)
  -> forceRecheck                              (gittarget_controller.go)
  -> observeDataPlane(force)
  -> startTargetWatchStreams(refreshRemote)
  -> enqueueScopedResync{RefreshRemote: true}
  -> refreshRemoteAndRebuildPendingWrites()    (fetch, then replay retained writes)
```

The annotation is deliberately spelled after Flux's, and the doc comment on
[`ReconcileRequestAnnotation`](../../internal/controller/gittarget_reconcile_request.go) already
states the intended use: "It matters most for a folder someone else edits."

A human can therefore do this now:

```bash
kubectl annotate gittarget editing -n gitops-reverser \
  reconcile.configbutler.ai/requestedAt="$(date +%s)" --overwrite
```

## 4. What is missing

One adapter: something that turns "the Git host says branch `X` of repo `Y` moved" into that
annotation patch on the `GitTarget`s that track it.

[`reconcile-triggering.md`](reconcile-triggering.md) §5 designed this as part of a larger trigger
overhaul, and its conclusions hold unchanged. The three that matter here:

- **§5.1: reuse the pattern, not Flux's `Receiver` CRD.** Its `spec.resources[].kind` enum rejects
  our kinds and its RBAC is Flux-scoped, so forking it is fragile and lost on upgrade. Validating
  a signed payload and patching an annotation is a pattern we can implement in one handler.
- **§5.3: dedupe our own pushes.** We write to the same branch, so our commits fire the same
  webhook. Compare the incoming head SHA against the worker's `lastCommitSHA` before requesting
  anything, or the loop feeds itself.
- **§5.4: piggyback on the webhook the user already has.** Most Flux and Argo CD users already
  point their Git host at a receiver. Asking for a second git-host webhook is the config cost this
  feature has to justify.

We already serve inbound HTTP for admission, conversion, and audit
([`cmd/main.go`](../../cmd/main.go) builds the audit mux), so the transport exists.

## 5. Shape of the options

| Option | What it is | Cost | Note |
| --- | --- | --- | --- |
| **A. Do nothing** | Operators annotate by hand when they edit the folder | zero | Honest for audit-only and split-ownership use. §2.1 stays. |
| **B. Poll** | Revive `SyncAndGetMetadata` on the steady cadence | small, ongoing | Bounded staleness with no Git-host config. Costs a fetch per target per interval whether or not anything changed. |
| **C. Receiver** | Our own validated push endpoint, patching the annotation | one handler plus a CRD or config surface | Fresh within a second. Needs secret handling, URL-to-target mapping, and §5.3 dedupe. |
| **D. Forward an existing trigger** | Let a user's Flux `Receiver` or Argo webhook fan out to us | smallest user-facing config | Depends on what those tools can be made to call. The open question below. |

B and C are not exclusive. A poll is the safety net that makes a missed webhook a latency problem
instead of a stuck one, which is the same argument Flux makes for keeping a short
`GitRepository.spec.interval` alongside a `Receiver`.

## 6. Open questions

1. **Is D reachable at all?** Flux's `Receiver` cannot name our kinds (§F2), and `flux reconcile`
   has the same hard-coded kind list. Argo CD's webhook endpoint drives Argo. Neither obviously
   fans out. If forwarding needs a user-run relay, D collapses into C with extra steps.
2. **Does the annotation want to be per-`GitProvider` too?** A push moves a branch, and several
   `GitTarget`s can track it. Fanning out from repo URL and branch to targets is a lookup the
   worker layer already has; whether the annotation lands on each target or on the provider is
   unsettled.
3. **Delete or revive `syncWithRemote`?** It is option B, already written, currently unreachable.
   Leaving it in the tree as dead code with a comment claiming it is called is the worst of the
   three states.
4. **Is §2.2 worth optimizing at all?** Skipping the re-render when the fetched tree does not
   touch our paths is possible. Nothing has measured how often a mid-cycle move happens outside a
   contended bi-directional corner.
