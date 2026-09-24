# Status surfaces for a repository and its folders

> **design**: still being decided. Index: [`../INDEX.md`](../INDEX.md)

Two objects describe one mirror. A `GitProvider` is a repository; a `GitTarget` is one folder on
one branch of it. Between them sits a branch worker, which is not an API object and must not
become one. This page fixes what each surface reports, and at what rate.

## The surface

| Information | Where it lives |
| --- | --- |
| Branches configured, and how many folders reference each | `GitProvider.status.branches[{name, gitTargets}]` (new) |
| Configuration and health verdicts | the existing conditions on both objects |
| The latest sampled branch observation | the existing `GitTarget.status.remote` |
| Queue depth, pushes, failures, retries, drops | the existing metrics |

Nothing else. No worker totals, no pending-write counts, no per-branch clocks, and no commit SHA as
a metric label.

`branches` is **the branches that configured `GitTarget`s reference**, including ones that are
blocked or suspended. It is not the branches that exist on the remote, and not the workers
currently running: it answers "what is this repository being used for", which is a question about
configuration and moves only when somebody edits a `GitTarget`. Keyed by name, sorted, patched only
when the contents change.

## Delivery is not publication

An observation of a branch is one fact shared by every `GitTarget` on it. Two separate decisions
follow, and conflating them is the mistake this section exists to correct.

**Delivery** is in memory and immediate. A confirmed look at branch B, by a push or by a fetch, is
available to every target on B at once, carrying the time it was proved. Distributing a
fact is not re-proving it, and it costs nothing.

**Publication** is a status write, and it has to be bounded. Fanning a revision out to ten targets
that share a branch turns one push into ten status patches, each an etcd write that invalidates the
cached copy held by every watcher of the type. Moving revisions from the `GitProvider` to its
`GitTarget`s does not reduce that. The revision belongs on the `GitTarget` because that is where
the question is asked, which is a different argument; on write volume alone it is worse.

So the `remote` tuple is **sampled onto the target's own reconcile cadence** rather than published
on every change. Two things stay prompt, because they are health rather than throughput: a
condition transition, and clearing `remote` when the repository identity no longer matches.

Today's quantizer does not achieve this, and the plan has to change it.
[`remoteStatusIsNews`](../../internal/controller/gittarget_remote.go) treats a changed revision
*or* a changed `verifiedBy` as news and skips the interval entirely, so under fan-out every target
would patch on every commit to its branch. `verifiedBy` moving from `Push` to `Fetch` is the same
answer proved a second way, and the watch plane already ignores it for that reason; the two layers
should agree.

**This changes a promise, so it is worth stating plainly.** `status.remote.revision` becomes a
sampled value that can lag the branch by up to one reconcile interval, so an operator who pushes and
immediately reads status may still see the previous revision. The alternative is a status field
that moves at commit rate, which is what the [status rate rule](../spec/status-conditions-guide.md)
rejects.

## Example: two `GitTarget`s on one `GitProvider`

`apps` and `infra` write different folders of the same branch, so they share one branch worker.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitProvider
metadata: {name: platform, namespace: shop}
status:
  lastVerifiedAt: "2026-09-24T18:04:11Z"
  branches:
    - name: main
      gitTargets: 2
  conditions:
    - type: Ready
      status: "True"
      reason: Succeeded
      message: Repository is reachable with the configured credential
```

```yaml
# GitTarget shop/apps: published after its own push
status:
  streams: {summary: 5/5, total: 5, ready: 5, replaying: 0, blocked: 0}
  placement: {mode: KustomizeRoot, renderRoot: clusters/prod}
  remote:
    revision: 4f2b9c1e8a77d3f0b6e5c4a39182d7e0fa6c5b41
    lastVerifiedAt: "2026-09-24T18:04:09Z"
    verifiedBy: Push
  conditions: [{type: Ready, status: "True", reason: Succeeded}]
```

```yaml
# GitTarget shop/infra: same branch, and has not published since
status:
  streams: {summary: 3/3, total: 3, ready: 3, replaying: 0, blocked: 0}
  remote:
    revision: 9c07ab53d1e64f82c5b0937ae41d268fbb3a90e7
    lastVerifiedAt: "2026-09-24T17:58:40Z"
    verifiedBy: Fetch
  conditions: [{type: Ready, status: "True", reason: Succeeded}]
```

`infra` holds an **older revision together with the older time that proved it**. The tuple is always
internally consistent: one observation from one moment, never the newest revision with a stale
clock. That is what makes it readable, and sampling has to preserve it.

Both targets keep their own conditions, and that is why one worker cannot summarize the branch:
`apps` can be `Ready=True` while `infra` sits at `GitPathAccepted=False` over content in its folder.

## Example: an empty `GitProvider`

A repository is declared, nothing mirrors to it yet. No branch worker exists, and none should.

```yaml
status:
  lastVerifiedAt: "2026-09-24T18:04:11Z"
  branches: []
  conditions:
    - type: Ready
      status: "True"
      reason: Succeeded
      message: Repository is reachable with the configured credential
```

`Ready=True` with `branches: []` is the state this surface most needs to distinguish: **configured
and unused** looks nothing like **broken**, and today an operator has to list `GitTarget`s to tell
them apart.

## Example: a wrong `GitProvider`

The URL resolves, the credential reads, and it has no write scope.

```yaml
status:
  lastVerifiedAt: "2026-09-24T18:04:11Z"
  branches:
    - name: main
      gitTargets: 2
  conditions:
    - type: Ready
      status: "False"
      reason: WriteAccessDenied
      message: The remote refused a write session for this credential
```

The two `GitTarget`s go `Ready=False` with `GitProviderReady=False`. Their `status.remote` keeps
**advancing**: reads still work, so the refresher goes on proving where the branch is. A stale
`lastVerifiedAt` is not the signal here and must not be read as one. Writes failing is carried by
the condition and by the push metrics. Remote freshness and publication success are separate
questions, and they want separate alerts.

## Finding out that a destination is wrong

**No copy is needed.** Cloning answers folder questions, which belong to the `GitTarget` and already
have `GitPathAccepted`. Everything about the destination is answerable from a handshake that
transfers no objects.

Reading is what is checked today: it rules out a bad URL, a dead credential and a missing
repository, and it yields the branch tips as a side effect. It does not prove we can write, and that
is the gap worth closing: a read-only token looks healthy right up to the first push.

A write probe opens a push session and sends nothing. This was tried by hand against GitHub and
behaved as expected. Three limits keep it a **separate change** rather than part of this one:

- It proves the server would open a session. Branch protection and pre-receive hooks are applied
  when refs are updated, so a green probe is necessary and not sufficient. A red one is definitive.
- It cannot replace the read probe. Git lets the two services hide refs differently, so a write
  advertisement is not guaranteed to carry the same branches, and an absent branch there cannot be
  read as "this branch does not exist".
- Making `Ready` depend on it **changes what `Ready` means**. Adding `branches` is additive; this is
  not, and a provider that reads but cannot write flips from `True` to `False` on upgrade. That
  deserves its own decision and its own upgrade note.

**One kind of wrong cannot be found out at all.** A URL that resolves, reads and writes may still be
a fork, a stale mirror, or somebody else's repository. Nothing on the wire identifies a repository,
so only the operator knows which one they meant. This is why the branch worker's identity is
`{GitProvider UID, URL}`: it stops state **crossing** between repositories, and it cannot stop a
destination being mistaken in the first place.

## Order of work

1. The inventory: `branches[{name, gitTargets}]`, derived from the configured `GitTarget`s.
2. Shared delivery plus bounded publication, including the `remoteStatusIsNews` change.
3. The write probe, on its own, with the `Ready` semantics decided explicitly.
