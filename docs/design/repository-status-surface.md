# Status surfaces for a repository and its folders

> **design**: steps 1 and 2 are implemented; steps 3 and 4 are not.
> Index: [`../INDEX.md`](../INDEX.md)

Two objects describe one mirror. A `GitProvider` is a repository; a `GitTarget` is one folder on
one branch of it. Between them sits a branch worker, which is not an API object and must not
become one. This page fixes what each surface reports, and at what rate.

## The surface

| Information | Where it lives |
| --- | --- |
| Branches configured, and how many folders reference each | `GitProvider.status.branches[{name, gitTargets}]` (new) |
| Whether the credential may write | `GitProvider.status` condition `Writable` (new) |
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

So the `remote` tuple is **sampled** rather than published on every change. Two things stay prompt,
because they are health rather than throughput: a condition transition, and clearing `remote` when
the repository identity no longer matches.

### The deadline is a wall clock, not a cadence

"Sampled onto the target's reconcile cadence" is not a bound, and saying it that way hides two
different failures. The rule is one number, `RemotePublicationInterval`:

- **At most one `status.remote` write per `GitTarget` per interval**, measured in wall clock from
  the observation currently published. Not from the gap between two observations, which is what
  today's quantizer compares and which stops moving exactly when the remote does.
- **A target holding something newer requeues on the remainder of that interval.** Without this the
  deadline would be decorative: the write would land on whatever tick came next, which for a
  converged target is five minutes. This is what makes an interval SHORTER than the reconcile
  cadence mean what it says, and it is also what keeps the data plane from waking anything:
  delivery notifies nobody, so no sibling is woken by a commit.
- **The lag is stated against the latest OBSERVATION, never against the branch.** No maximum lag
  against the actual branch is available at any price: an unreachable remote yields no observation at
  all, and the pair (`revision`, `lastVerifiedAt`) is precisely what says so.

Removing the revision bypass from
[`remoteStatusIsNews`](../../internal/controller/gittarget_remote.go) is necessary and not
sufficient. That function also treats a changed `verifiedBy` as news, which it is not: `Push` to
`Fetch` is the same answer proved a second way, and the watch plane already ignores it for that
reason. News becomes "a different revision, or the same one proved later", and the rate bound above
applies to whatever passes it.

**This changes a promise, so it is worth stating plainly.** `status.remote.revision` becomes a
sampled value that can lag the last observation by up to one publication interval, so an operator
who pushes and immediately reads status may still see the previous revision. The alternative is a
status field that moves at commit rate, which is what the
[status rate rule](../spec/status-conditions-guide.md) rejects.

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
    - type: Writable
      status: "False"
      reason: WriteAccessDenied
      message: The remote refused a write session for this credential
    - type: Ready
      status: "False"
      reason: WriteAccessDenied
      message: The remote refused a write session for this credential
```

Their `status.remote` keeps **advancing**: reads still work, so the refresher goes on proving where
the branch is. A stale `lastVerifiedAt` is not the signal here and must not be read as one. Writes
failing is carried by the condition and by the push metrics. Remote freshness and publication
success are separate questions, and they want separate alerts.

### Why `Writable` is a condition of its own

Folding the verdict into `Ready` alone would say "this provider is broken" without saying how, and
it would make the check a change to what `Ready` means rather than something added beside it. The
house pattern already answers this: a `GitTarget` publishes `StreamsRunning`, `GitPathAccepted` and
`RenderMatchesLive` as conditions in their own right AND contributes each to the trio through one
documented precedence, so the axis is readable on its own and the roll-up stays honest.

`Writable` follows that shape:

- `False` only on an explicit refusal. A probe that could not run leaves it `Unknown`, which by this
  project's convention does not downgrade `Ready`, so a transient failure never turns a healthy
  provider red.
- Not latched, with one asymmetry that has to be written down because it is where the two rules
  above collide. **A failed probe never clears a proven denial.** `False` moves to `Unknown` only
  when nothing has been proven yet; once the remote has refused a write session, the condition
  stays `False` until a probe SUCCEEDS. Otherwise a repository whose token was revoked would go
  green the moment the network flapped, and green here means "this destination works" to every
  consumer, including the `Ready` gate step 4 adds. Uncertainty may withhold an affirmative
  verdict; it may not overturn a proven negative one.
- Write access comes back when a token is regranted, and the condition follows: the successful
  probe is the evidence, and it is the only thing that clears the denial.
- It gets a printer column, because "reachable but not writable" is the case an operator cannot
  currently see at all.

**Whether it also gates `Ready` is a separate decision, and the answer is yes, eventually.** There
is no such thing as a legitimately read-only destination here: every folder is mirrored by pushing.
A provider that cannot be written to cannot do its job, and a green `Ready` beside it would be the
kind of half-truth that teaches people to ignore conditions. Gating also costs nothing to plumb,
because a `GitTarget` already projects its provider's readiness as `GitProviderReady`; without the
gate, every consumer would have to learn a second condition and project that too.

So: publish `Writable` first, and flip the `Ready` gate as its own release with an upgrade note,
since a provider that reads but cannot write goes from `True` to `False` the day it lands.

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
- It reports its own condition rather than only a `Ready` reason, for the reasons above.

**One kind of wrong cannot be found out at all.** A URL that resolves, reads and writes may still be
a fork, a stale mirror, or somebody else's repository. Nothing on the wire identifies a repository,
so only the operator knows which one they meant. This is why the branch worker's identity is
`{GitProvider UID, URL}`: it stops state **crossing** between repositories, and it cannot stop a
destination being mistaken in the first place.

## Order of work

1. **Done.** The inventory: `branches[{name, gitTargets}]`, derived from the configured
   `GitTarget`s, kept current by a filtered `Watches()` edge on `GitTarget`.
2. **Done.** Shared delivery plus bounded publication. The observation is recorded once, where it
   is proved, and kept per branch by the worker manager, so it outlives the worker that proved it
   and the per-target projection is gone. Publication follows the deadline rule above.
3. The write probe, publishing `Writable` and nothing else.
4. `Writable` gates `Ready`, with the upgrade note. Separate, because it is the only step that
   changes what an existing field means.
