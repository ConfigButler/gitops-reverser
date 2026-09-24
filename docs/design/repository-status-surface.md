# Status surfaces for a repository and its folders

> **design**: still being decided. Index: [`../INDEX.md`](../INDEX.md)

Two objects describe one mirror. A `GitProvider` is a repository; a `GitTarget` is one folder on
one branch of it. Between them sits a branch worker, which is not an API object and must not
become one. This page fixes what each surface reports, so the same fact stops being claimed by two
of them with different staleness.

## The question each object answers

| Question | Proved by | Reported on |
| --- | --- | --- |
| Can this repository be reached with this credential? | the `GitProvider` reconcile | `GitProvider.status`: `Ready`, `lastVerifiedAt` |
| May we **write** to it? | the `GitProvider` reconcile | `GitProvider.status`: `Ready` (see [below](#what-wrong-looks-like-and-how-it-is-found-out)) |
| Which branches does it serve, and for how many folders? | the `GitProvider` reconcile, from the API | `GitProvider.status.branches` |
| Where is branch B on the remote, and when was that proved? | the branch worker | `GitTarget.status.remote`, on every target on that branch |
| Is this folder accepted, streaming, and matching live? | the watch plane, per target | the `GitTarget`'s conditions and `status.placement` |

## Revisions live on the `GitTarget`, counts live on the `GitProvider`

The branch tip is a fact about the repository, so putting it on the `GitProvider` is tempting. The
[status rate rule](../spec/status-conditions-guide.md) settles it without appealing to taste: a
field belongs in status when it moves because a **user** changed something or health flipped, and
belongs in a metric when it moves because the **workload** moved.

- `branches[].gitTargets` moves when somebody creates or deletes a `GitTarget`. That is
  configuration. It is status.
- A branch revision moves on every commit to that branch, whichever folder caused it. That is
  throughput. On a `GitProvider` serving ten busy branches it would rewrite one object on every
  commit in the whole repository, and invalidate the cached copy held by every watcher each time.

So the `GitProvider` says **which branches exist and how much depends on them**, and never where
they point. The revision stays where the question "is my folder in Git, and at what
revision" is asked.

`GitTarget.status.remote` keeps its current shape. What changes is only its documented meaning: it
is a **branch** fact **delivered per target**. The revision and its `lastVerifiedAt` are the branch
worker's single observation, so a push by one target renews it for every target on that branch. A
fresh `lastVerifiedAt` says the branch tip was recently proved. It does not say this target's
folder was scanned; the conditions say that. Two targets on one branch can hold different times,
and that is delivery lag rather than a difference in what was proved.

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
      message: Repository is reachable and writable with the configured credential
```

```yaml
# GitTarget shop/apps
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
# GitTarget shop/infra: same branch, same worker, same tip
status:
  streams: {summary: 3/3, total: 3, ready: 3, replaying: 0, blocked: 0}
  remote:
    revision: 4f2b9c1e8a77d3f0b6e5c4a39182d7e0fa6c5b41
    lastVerifiedAt: "2026-09-24T17:58:40Z"
    verifiedBy: Push
  conditions: [{type: Ready, status: "True", reason: Succeeded}]
```

Both carry the same revision, because there is one branch and one tip. `infra` carries an older
`lastVerifiedAt` because `apps` pushed more recently and `infra` has not ticked since; the value it
holds was still proved at the time it names. Read as "when was this branch last proved" it is
correct on both. Read as "when was `infra` last checked" it is not, which is the misreading this
page exists to close.

The healthy-and-refused pair is the reason both targets keep their own conditions: `apps` can be
`Ready=True` while `infra` sits at `GitPathAccepted=False` over content in its folder. One worker
cannot summarize both, and the `GitProvider` must not try.

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
      message: Repository is reachable and writable with the configured credential
```

`Ready=True` with `branches: []` is the honest report, and it is the state this surface most needs
to distinguish: **configured and unused** looks nothing like **broken**, and today an operator has
to go and list `GitTarget`s to tell them apart. The credential is verified on the repository, so a
typo is caught here rather than at the first write of the first folder somebody adds.

## Example: a wrong `GitProvider`

The URL resolves, the credential reads, and the token has no write scope. This is the case that is
invisible today: the current check lists refs over `upload-pack`, which only proves **read**.

```yaml
status:
  lastVerifiedAt: "2026-09-24T17:12:03Z"   # not cleared; it dates the last time this WORKED
  branches:
    - name: main
      gitTargets: 2
  conditions:
    - type: Ready
      status: "False"
      reason: WriteAccessDenied
      message: >-
        The remote accepts reads but refused a receive-pack session for this credential.
        Nothing can be mirrored until the credential is granted write access.
```

The two `GitTarget`s stay `Ready=False` with `GitProviderReady=False`, and their `status.remote`
keeps whatever was last proved, with a `lastVerifiedAt` that stops advancing. Nothing is cleared:
the revision is still where the branch was when it was last seen, and the frozen timestamp is how
long it has been since.

## What "wrong" looks like, and how it is found out

**No, a copy is never needed to judge the destination.** Cloning answers folder questions, and
those belong to the `GitTarget` and already have `GitPathAccepted`. Everything on this page is
answerable from a handshake that transfers no objects.

A ladder, cheapest first:

1. **Connect and read**: the `upload-pack` ref advertisement, which is what `CheckRepo` already does.
   Rules out: bad URL, DNS, TLS, unusable credential, missing repository. Yields every branch tip
   as a side effect.
2. **Open a write session**: the `receive-pack` handshake, the same one `PushAtomic` opens before it
   sends anything ([`git_atomic_push.go`](../../internal/git/git_atomic_push.go)). No packfile, no
   ref update, nothing written. A server that will not let this credential write refuses the
   service outright.
3. **Branch policy**, purely local once step 1 has answered: does `allowedBranches` admit the
   branches the `GitTarget`s ask for. A branch that does not exist yet is **not** an error; the
   first push creates it.

Step 2 subsumes step 1, and that is the shape worth taking: a `receive-pack` advertisement carries
the same refs as an `upload-pack` one, so **one handshake proves reachability, proves write access,
and yields every branch tip**, replacing today's read-only probe at the same cost of one
connection.

Two honest limits:

- A `receive-pack` handshake proves the server would **open** a write session. Branch protection,
  pre-receive hooks and per-branch permissions are evaluated at ref-update time on most forges, so a
  green probe is necessary and not sufficient. A red one is definitive.
- Many forges answer `404` rather than `403` for a repository the credential may not see, so
  "missing" and "not permitted" are not always distinguishable. The message should say both.

**And one thing Git cannot tell us at all.** A URL that resolves, reads and writes may still be the
*wrong repository*: a fork, a stale mirror, somebody else's. Nothing on the wire carries a
repository identity, and the root commit is shared by every fork of a history, so it identifies a
history and not a remote. Only the operator knows which repository they meant. This is why the
branch worker's identity is `{GitProvider UID, URL}`: it stops state **crossing** from one
repository to another, and it cannot stop a destination being mistaken in the first place.

## Not proposed here

- Revisions, publication failures or pending-work counts on the `GitProvider`. The first fails the
  status rate rule; the other two are per folder, and the `GitTarget` conditions already carry them.
- Branch-worker liveness as API. It is a goroutine, and the API should not depend on a particular
  implementation of one. `branches[].gitTargets` says what an operator needs: something depends on
  this branch.
- Removing `status.remote`, or changing what `Ready` means. Both are compatibility events and
  neither is needed for anything above. Adding `branches` and a write-access reason is additive.
