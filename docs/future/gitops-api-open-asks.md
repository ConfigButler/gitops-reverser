# gitops-reverser in production: how we use it, and what we'd still like

_From the [gitops-api](https://github.com/ConfigButler/gitops-api) team. **Revision 19, 2026-10-28**
(first sent 2026-07-09). Written to be readable from the gitops-reverser repo — no gitops-api
knowledge assumed. Every ask is traced to something we measured on a running system. **We run
`v0.55.0`.**_

## Where this stands

**`0.55.0` closed both our open asks and the `kube-root-ca.crt` vote, and we adopted it the day it
was tagged.** The source-cluster `CommitRequest` deleted more of our code than #30 did: our editor now
saves inside the customer's own kcp workspace, with their own token, so our save endpoint, our
per-repository commit lock, the one grant a customer held on our management cluster, and your
admission webhook with its certificate are all gone. On a rebuilt cluster: loop conformance 21/21,
our editor click-through 77/77, a save's `ConfigPublication` published 33 ms after its `Applied`, and
the first snapshot after the rebuild made no commit at all. Adoption write-up:
[reverser-0.55-adoption.md](https://github.com/ConfigButler/gitops-api/blob/main/docs/design/reverser-0.55-adoption.md).

**Six asks are new since then.** Three are small and came from the adoption. The other three came
from tuning the editor's save the day after. **#39 is the one that matters**: with your defaults, a
save's message can land on somebody else's change.

| Open | What it is |
|---|---|
| **39** 🟠 new | `CurrentOrNext` gives a save's message to an earlier, unclaimed write of the same author, and your defaults do it too |
| **40** 🟡 new | A request's progress is reported on a fixed 2 s poll, so a save's latency moves in 2 s steps |
| **41** 🟡 new | A push that resolves a `CommitRequest` waits out the 5 s `PushCooldown` |
| **38** 🟡 new | A source-cluster request naming a `GitTarget` that does not mirror that cluster gets no status at all |
| **37** ⚪ new | Default `gitTargetRef.namespace` on a source cluster to the serving `KubeProvider`'s namespace |
| **36** ⚪ new | `AuthorUnresolved` as a failure (`Ready=False`), not a success |
| **10** ⚪ | Sibling inference can place an object in another namespace's directory — cosmetic |
| **28** ⏸ | Two instances on one config-plane cluster — deferred, by agreement |

Plus two **votes for items already on your list**: the durable worker queue, and
`concurrent-author-windows.md`.

## Who we are

**gitops-api lets an external user edit their config-as-data by talking to a Kubernetes API.** They
log in with GitHub, they `kubectl` (or click) against an API surface that is theirs, and their edit
becomes a **Git commit authored by them** in their own repository. gitops-api itself **never writes
Git** — that is entirely gitops-reverser's job, and it is why the product exists. The attribution
_is_ the product.

Each tenant is a [kcp](https://kcp.io) **workspace**: a nodeless, near-namespace-cheap logical
cluster with its own CRDs and its own OIDC.

```text
                      ┌───────────────────────────────────────────┐
  a human's edit ────▶│ kcp workspace  (one per tenant)           │
  (Dex/OIDC token)    └───────────────┬───────────────────────────┘
                                      │ shard-wide audit firehose, tagged `kcp.io/cluster`
                                      ▼
                    ┌──────────────────────────────────────┐
                    │  ONE gitops-reverser (STOCK) + ONE   │  a KubeProvider per tenant;
                    │  Valkey, for the whole platform      │  attribution partitioned by
                    └───────────────┬──────────────────────┘  spec.attribution.auditRoute
                      ┌─────────────┴─────────────┐
                      ▼                           ▼
              tenant A's repo             tenant B's repo    ← attributed commits
                      ▲
                      │  Flux `Kustomization` with spec.kubeConfig → the workspace,
                      └─ held by a FluxClaim through the ONE in-cluster KubeProvider
                         (forward leg: git → cluster)
```

**We run the stock image, with no patches.** One reverser mirrors our management cluster in-cluster
and every tenant's workspace remotely. A GitHub App's single webhook reaches it through our server,
which forwards each push to one `GitReceiver`.

We also **link** `pkg/manifestanalyzer`: our onboarding wizard's folder step runs your acceptance
gate so the folders it offers are the folders your writer will accept. The reverser is therefore
pinned twice for us — the image we run and the Go module we link — with a test that fails the build
when they part.

---

## Open asks

### 39. `CurrentOrNext` gives a save's message to somebody else's writes

**Ask.** If a request attaches to a window that was already open when it registered, keep that
window's own timers until a write arrives after the registration. Alternatively make `Next` the
default. At the least, say in the CRD docs that `CurrentOrNext` can label writes the request never
saw.

**Why.** An attached request replaces the window's timers with its own, and they are measured from
the window's past. Suppose a write of the same author was claimed by no request (kubectl, or the
tail of an earlier save). It leaves a window open on the `GitTarget`'s 5 s idle timer. A save that
registers inside that window attaches to it, and the window then closes on the save's timers
(`maxDuration: 2s`, or an `idleTimeout` already past), before the save's own writes reach the worker.
The result, measured with a write made just before the request was filed:

- the request's message is committed on the earlier write;
- the save's own writes commit under the generated message;
- the request reports `Committed`.

**Your defaults do it too**, not just our tuning. Our click-through caught it as a shift by one:
three saves in a row, each message on the previous save's change. We now file `attach: Next`, which
fixes it. The cost is one extra push and then #41's cooldown, so the fix costs us about 4 s whenever
it is needed.

**Answer (gitops-reverser, PR #465): fixed by #40, and the window rule stays.** The request's timers
do not reach into the window's past: its `maxDuration` and its first idle interval start at the
attach. The shift by one came from #40's poll. Your client waits for `WaitingForWindow` before it
writes, and that status took up to 2 s to appear, as long as the default `maxDuration`. So the
window had closed under the save's message before the save's writes arrived, and those writes
opened the window the next save attached to. With #40 the registration reaches the client in about
56 ms. `TestSaveFolder_ThreeSavesInARowKeepEachMessageOnItsOwnChange` pins three saves in a row,
each message on its own change, and fails with a 2.1 s client delay.

We tried your first option, holding a save's timers until its first write after the attach, and
took it out again. It made the `GitTarget`'s timers decide when an attached save commits: a
`GitTarget` with a short `idleTimeout` brought the bug back, and a folder applied together with its
`CommitRequest` waited 5 s instead of 2 s. `CurrentOrNext` stays the default, and you can drop
`attach: Next`, so you no longer pay that extra push either. What remains: a save's writes must reach
GitOps Reverser within `maxDuration` of the attach. On a slow audit pipeline, raise `maxDuration`.

### 40. Report a request's progress without the 2 s poll

**Ask.** Wake the serving goroutine when the worker registers, resolves or applies a request,
instead of polling `commitRequestPollInterval = 2s` (`commitrequest_controller.go:63`,
`recordPhase` in `commitrequest_core.go`). Failing that, poll at ~100 ms while the phase is still
`""`.

**Why.** A save's visible latency moves in 2 s steps. Measured create → registered: ~200 ms to the
claim (author found), then ~2 s until the next poll reports `WaitingForWindow`. The same 2 s comes
again between push and `Ready`, and on the `Applied` follow. Waiting for registration before writing
is what makes `SaveInProgress` safe, so a client has no way around this poll. About 3 s of our ~6.8 s
save is this poll.

### 41. Skip `PushCooldown` for a push a `CommitRequest` waits on

**Ask.** Push at once when the commit resolves an attached request, as an apply flush already does
(`branch_worker.go`, `PushCooldown = 5s`).

**Why.** A person is watching that push. With `attach: Next` (#39), every save that finds an open
window pushes twice, and the second push waits out the cooldown: ~10.8 s instead of ~6.8 s. Two
saves in a row hit it the same way.

### 38. Refuse a source-cluster request whose `GitTarget` does not mirror that cluster

**Ask.** A `CommitRequest` on a source cluster that names a `GitTarget` mirroring some _other_
cluster should end `NotAllowed`, the way `0.55.0` already refuses the opposite direction (a request
on the control cluster for a `GitTarget` that mirrors another cluster).

**Why.** Measured: a request in one tenant's workspace naming a `GitTarget` that mirrors our
management cluster stayed with **no status at all**, and nothing was logged, until we deleted it by
hand. The serving path is not its owner, so it rechecks every 5 minutes
(`commitRequestUnownedRecheck`). The boundary holds, which is the important half: it is never served
and attaches to nothing. But its creator waits for ever, and because the request is never finished
it is also never collected; our tenants hold no `delete` on the type, so it outlives every TTL.

### 37. Default `gitTargetRef.namespace` on a source cluster

**Ask.** On a source cluster, let an omitted `spec.gitTargetRef.namespace` mean the namespace of the
`KubeProvider` the request is served through.

**Why.** Every one of our `GitTarget`s lives beside its `KubeProvider`, and that namespace is an
infrastructure compartment our customers are not meant to know about. Today it must appear in an
object they can read. It is a name, not a credential, so this is polish.

### 36. `AuthorUnresolved` as a failure

**Ask.** End a request no audit event names with `Ready=False` / `Stalled=True`, not `Ready=True`.

**Why.** It is the one outcome that means an audit event was dropped (kcp drops the event of a
YAML-bodied write, and we have been bitten), and the save's typed message is lost with it. Ready=True
reads as success to every client that waits for `Ready`. Your new
`commit_requests_total{outcome="author_unresolved"}` counter covers our alarm in the meantime.

### 10. Sibling inference can place an object in another namespace's directory — ⚪ cosmetic

**Ask.** Consider the namespace when inferring where a _new_ document goes.

**Why.** The cold-start layout is namespace-first, but once a layout exists, placement follows
siblings. Two tenants syncing a repo called `infra` produce two objects named `infra` in different
namespaces, and one lands in the other's directory. **Nothing is corrupted** — both documents and
both namespaces are correct, and deleting one removes exactly that document. Only the path suffers:
`git log -- tenants/tenant-a/` shows another tenant's history. It may well be intentional.

### 28. Two reverser instances cannot share one config-plane cluster — ⏸ deferred, by agreement

We asked for a way to partition the CR set between two instances on one cluster (`--watch-namespaces`,
then an owner selector), because we are moving onto a cluster that already runs a reverser for its
owner. You built the first step and dropped it before merge (#445): it removes the shared Valkey and
the cross-owner restart, but not the shared CRDs, the shared upgrade cadence or the shared
ClusterRole, and it adds a second tenancy model beside grants. **Agreed.** We run one shared
instance; the cost that would make us ask again is the restart below, and you want that fixed for
everyone. Our analysis: [reverser-instance-topology.md](https://github.com/ConfigButler/gitops-api/blob/main/docs/design/reverser-instance-topology.md).

## Votes for your own TODO

These are the measurements behind our vote, not new asks.

**The durable worker queue** (`docs/TODO.md`, "Make the worker queue durable"). The one cost of
sharing an instance that our users can see. `RenderMatchesLive` starts `Unknown` after a restart, an
open commit window is dropped, and a save made in that window loses the message its author typed.
It is the last item standing between us and "a deploy of the reverser is invisible to the people
editing".

**`concurrent-author-windows.md`**, for when our tenants get several editors. Today each has one
human, so `SaveInProgress` covers us. Our answer to its open question 4: report a window that a
`path_conflict` closed early, because a save that commits less than its author meant to save,
silently, is the bug we built our own lock for.

## Process notes

- **Retire a field by rejecting it first.** `0.44.0` kept `eventTemplate`/`groupTemplate` in the
  schema for one release with a CEL rule that refuses them, and `0.53.0` did the same for
  `allowAnySourceNamespace`. Both failed loudly in our operator instead of silently changing
  behaviour. When a field is simply removed, the API server prunes it and the object stays
  `Ready=True`, so we write tests asserting the _absence_ of old spellings. Making
  rejection-before-removal the default would make those tests unnecessary.
- **`SaveInProgress` is decided at registration, seconds after `Saving`.** `UPGRADING.md` tells a
  client to wait for `Ready=Unknown` before its edit. That is right for `idleTimeout: "0s"`, but a
  client relying on `SaveInProgress` to serialise saves must wait for `WaitingForWindow`: measured, a
  second save sat at `Saving` with its author set for ~3 s and was then refused. Our first build
  applied at `Saving` and wrote into the other save's window. One sentence in the guide would save
  the next adopter that run.
- **`0.54.0`'s "recreate the connection objects" was the right call** for a reshape this size, and
  `UPGRADING.md`'s eight steps were complete. We rebuilt rather than followed them, which a
  disposable cluster allows, and found nothing missing from the guide.

---

## Shipped — the record

One line each: what happened, and whether we are happy.

| # | Ask | Shipped | Verdict |
|---|---|---|---|
| 1 | Separate the config plane from the watched cluster | `v0.38.0` (#249/#251), #259 | **Happy** — one reverser now serves our whole platform; it deleted a tier of our architecture |
| 3 | A Redis key prefix | #221 | **Happy** — defaulted to the historic root, no migration |
| 4 | Export `pkg/manifestanalyzer` | #221 | **Happy** — the foundation of our wizard's folder step |
| 5 | A `CommitRequest` that can assert an author | the admission webhook (#220 closed unmerged), then `0.55.0`: the author comes from the request's own audit event | **Very happy** — no webhook, and it works on a source cluster |
| 6 | Movable `GitTarget` destination, or fail loudly | immutability that fails at admission with "delete and recreate" | **Happy** — we delete and recreate, now for a `GitBranch` too |
| 7 | Degrade without `apiregistration.k8s.io` | #223, `v0.34.0` | **Happier than asked** — un-started rather than blacklisted |
| 8 | Never sweep the mirror to empty | `v0.39.0` (#260), `prune.mode: OnEvent` | **Very happy** — an API default, not a guard we maintain |
| 9 | RBAC that matches the runtime need | #223, `v0.34.0` | **Happy** |
| 11 | Serialize `[CREATE]` and `[UPDATE]` bodies identically | `0.41.0` (#290), one encoder | **Happy** — the mirror's `git diff` is readable |
| 12 | Strip `kcp.io/cluster` from Git writes | #227, `v0.35.0` | **Happy, and grateful** — fixed before we reported it |
| 13 | Tag releases so Go can resolve them | `v0.37.0` | **Happy** — what made #4 pinnable |
| 14 | Watch a differently-named namespace than the rule's own | `v0.39.0` (#259) | **Happier than asked** |
| 15 | Make a lost audit route loud | `0.44.0` (#341), `AuditFactsReceived` | **Very happy** — a latched condition tells _not yet_ from _never_ |
| 16–21 | The analyzer asks: permanence, the report contract, `Key()`, rendered types, `FleetRoot`, `ReadScope` | `0.40.0`/`0.40.1` (#273, #275) | **Very happy** — three better than asked, two corrections of our ask |
| 22 | `ReasonRefusedStructural` no longer claims "permanent" | `0.41.0` (#290) | **Happy** |
| 23 | Deletion-as-intent names the deleter, not the finalizer-clearer | `0.41.0` (#289), pinned for UPDATE and PATCH in `0.54.0` | **Happy** |
| 26, 27 | Join the pipeline's halves; count `CommitRequest` outcomes | `git_branch_targets`, `commit_requests_total` | **Happy** — better than asked |
| 29 | Wake the reconciler only for commits the reverser did not make | `v0.53.0`, reconcile ownership | **Happy** — the forward-leg race is gone for every writer ([write-up](https://github.com/ConfigButler/gitops-api/blob/main/docs/design/reverser-feedback-forward-leg-race.md)) |
| 30 | One receiver endpoint for a GitHub App | `0.54.0` (#432), `GitReceiver` | **Very happy** — deleted our per-repository tokens, Roles and HMAC relay |
| 31 | Count "resumed elsewhere" in a metric | `0.54.0`, `apply_claimed_resource_reconciled_total` | **Happy** — we alert on it |
| 32 | A troubleshooting row for a full-spec writer | `0.54.0`, the Flux guide | **Happy** |
| 33 | Say why `prefetch` is false | `0.54.0` (#425), prefetch removed | **Happy** |
| 34 | An e2e for an in-cluster applier with a remote source cluster | `0.55.0` (#456), scenario 10 | **Very happy** — it found a real apply-window bug in our topology |
| 35 | Converge the applier on the reverser's own pushes | `0.55.0` (#456), `FluxClaim.spec.catchUpAfter` + `CommitRequest.spec.catchUp: AfterPush` | **Happier than asked** — a save is published 33 ms after its apply |
| — | Filter `kube-root-ca.crt` (a vote) | `0.55.0` | **Happy** — a rebuild makes no commit at all |
| — | `WatchRule` readiness lag (never filed) | `0.44.0` (#343) | **Happy** — rule readiness is usable as an alert |

**#2 — ignore writes by an identity or field manager. ⚫ WITHDRAWN 2026-07-14, our mistake.** The
forward leg's echo is self-terminating: the applier applies the commit the reverser just pushed, the
content matches, the echo commits nothing. We built the filter ourselves and **it caused a
two-and-a-half-minute flip-flop**, every commit unattributed. **Please never add
`excludeUsers`/`excludeFieldManagers` on our account** —
[why](https://github.com/ConfigButler/gitops-api/blob/main/docs/design/forward-leg-loop.md). The right shape
was the one you already had: sanitize the content, do not filter the writer.

## What is _not_ upstream's problem

So a wishlist does not become a blame list:

- **The audit muxer** was our integration with kcp, not a reverser concern — and `v0.39.0` let us
  delete it.
- **kcp's front-proxy** panicking on an anonymous request, and its per-workspace authenticator
  answering `Unauthorized` for a minute after a fresh start. kcp's, not yours.
- **Broken label references in our own alerts**, found while mapping our dashboards onto a new
  metric surface. Ours, every time — which is why we now test our rules against `0.54.0`'s labels.

## Suggested order

1. **#39** — the one that commits a person's sentence on the wrong change, with defaults.
2. **#38** — small, and the only one where a client can wait for ever.
3. **#40** and **#41** — together they are about half of a save's latency.
4. The two votes, in whichever order your plan already has them.
5. **#36**, **#37** and **#10** — polish, whenever.
