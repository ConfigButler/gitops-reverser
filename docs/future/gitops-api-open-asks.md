# gitops-reverser in production: how we use it, and what we'd still like

_From the [gitops-api](https://github.com/ConfigButler/gitops-api) team. **Revision 21, 2026-11-27**
(first sent 2026-07-09). Written to be readable from the gitops-reverser repo — no gitops-api
knowledge assumed. Every ask is traced to something we measured on a running system. **We run
`v0.57.0`.**_

## Where this stands

**`0.56.0` answered four of our six asks, and we adopted `0.57.0` the day after it was tagged.** A
save is reported as the worker reaches each phase (#40), its push skips the cooldown (#41), a
misdirected request ends `NotAllowed` (#38), and your answer to #39 came with a regression test. On a
rebuilt cluster: loop conformance 21/21, our editor click-through 81/81, and a save now commits
**4–5 s** after it is filed, where it took ~6.8 s on `0.55.0`. `0.57.0`'s object `status.author`
lets the editor name the saver as the Git author header does ("authored by Simon Koudijs"); kcp
carries the name and email claims onto the save's own audit event. Adoption write-up:
[reverser-0.56-adoption.md](https://github.com/ConfigButler/gitops-api/blob/main/docs/design/reverser-0.56-adoption.md).

| Open | What it is |
|---|---|
| **43** 🟠 new, a bug | A source cluster's `CommitRequest`s go unseen for 10 s every 5 min 10 s ([report](https://github.com/ConfigButler/gitops-api/blob/main/hack/spikes/BUG-commitrequest-watch-reconnect-gap.md)) |
| **42** ⚪ new | Tell a `CommitRequest` when a `path_conflict` closed its window early |
| **37** ⚪ | Default `gitTargetRef.namespace` on a source cluster to the serving `KubeProvider`'s namespace |
| **36** ⚪ | `AuthorUnresolved` as a failure (`Ready=False`), not a success |
| **10** ⚪ | Sibling inference can place an object in another namespace's directory — cosmetic |
| **28** ⏸ | Two instances on one config-plane cluster — deferred, by agreement |

Plus one **vote for an item already on your list**: the durable worker queue.

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

### 43. A source cluster's `CommitRequest`s go unseen for 10 s every 5 min 10 s

**Ask.** In `CommitRequestServer.watchNamespace`, wait `commitRequestReconnect` only after a watch
that ended in an error; after a clean end (the 5-minute `TimeoutSeconds`), list and watch again at
once, or resume from the last resourceVersion.

**Why.** Every clean end is followed by a 10 s wait in which nothing watches the namespace, so a
request created then is claimed up to 10 s late: ~3% of our saves. Reproduced on demand by filing
requests inside predicted gaps (9.5 s against 0.6 s outside them), and it is not audit lag (the
create's fact was published the same second). A client that waits for registration before writing,
as the protocol asks, can time out and lose its message. Full report, evidence and a suggested test:
[BUG-commitrequest-watch-reconnect-gap.md](https://github.com/ConfigButler/gitops-api/blob/main/hack/spikes/BUG-commitrequest-watch-reconnect-gap.md).

### 42. Tell a `CommitRequest` when a `path_conflict` closed its window early

**Ask.** When a window an attached request holds is closed by `path_conflict` (another author's write
overlapped its footprint), say so on the request: a condition reason, or a line in `Ready`'s message.

**Why.** It was our open question 4 on `concurrent-author-windows.md`, and `0.56.0` shipped the
stage without it: `path_conflict` is a close reason on `git_commit_windows_total` and nothing on the
request (**checked** in `v0.57.0`: `window_admission.go` finalizes the window, the request sees an
ordinary close). A save that commits less than its author meant to save, silently, is the bug we
once built our own lock for. Today each tenant has one human, so it is polish until that changes.

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
| 38 | Refuse a source-cluster request whose `GitTarget` does not mirror that cluster | `0.56.0` (#465), `NotAllowed` naming both clusters | **Happy** — and it is collected after the TTL now |
| 39 | `CurrentOrNext` labels an earlier write | `0.56.0` (#465): the shift was #40's poll, with a regression test | **Happy** — we keep `attach: Next` until we have measured both |
| 40, 41 | Report a request's phases without the poll; skip `PushCooldown` for a save's push | `0.56.0` (#465) | **Very happy** — create → `Committed` 6.8 s → 4–5 s |
| — | `concurrent-author-windows.md` stage 2 (a vote) | `0.56.0` (#467, #469, #470) | **Happy** — question 4 is now #42 |
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

0. **#43**, the watch gap: a bug, small to fix, and every saving client feels it.
1. **The durable worker queue** (our vote) — the last thing that makes a reverser deploy visible to
   someone editing.
2. **#36**, **#37**, **#42** and **#10** — polish, whenever.
