# gitops-reverser in production: how we use it, and what we'd still like

_From the [gitops-api](https://github.com/ConfigButler/gitops-api) team. **Revision 17, 2026-10-26**
(first sent 2026-07-09). Written to be readable from the gitops-reverser repo — no gitops-api
knowledge assumed. Every ask is traced to something we measured on a running system. **We run
`v0.54.0`.**_

## Where this stands

**`0.54.0` closed every ask we had open but three, and we adopted it the day it was tagged.** It
reshaped every connection object with no in-place migration, so we rebuilt our cluster rather than
migrate it; loop conformance 21/21 and our editor click-through pass on it. #30 shipped as
`GitReceiver` and deleted more of our code than any release since #1: one receiver for the whole
platform, so our relay now forwards GitHub's delivery verbatim, with no per-repository token, Role
or receiver. Adoption write-up: [reverser-0.54-adoption.md](https://github.com/ConfigButler/gitops-api/blob/main/docs/design/reverser-0.54-adoption.md).

**One new ask, #35, and unlike the last few it costs us something today.** `0.54.0` stopped
applying the forward leg after the reverser's own pushes, which is right for the cluster and leaves
the applier's own state — its applied revision, its source artifact — behind the branch until
somebody else pushes. We read that state to publish. Details and a proposal below.

**We accept your reasoning for deferring #28**: a namespace split leaves the CRDs and the
ClusterRole shared, and the cost that reaches users (a restart dropping an open window) is better
fixed inside one instance — which is the second vote below.

| Open | What it is |
|---|---|
| **35** 🟡 new | Converge the applier on the reverser's own pushes, at a bounded rate |
| **34** 🟡 | An e2e for an in-cluster applier with a remote source cluster — our production topology |
| **10** ⚪ | Sibling inference can place an object in another namespace's directory — cosmetic |
| **28** ⏸ | Two instances on one config-plane cluster — deferred, by agreement |

Plus two **votes for items already on your TODO**, with our evidence: filtering `kube-root-ca.crt`,
and the durable worker queue.

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

### 35. Converge the applier on the reverser's own pushes, at a bounded rate

**Ask.** A periodic converge: when an applier's last applied commit is behind the branch tip, and
nothing else is pending for it, apply the tip — **at most once per interval**. For example
`FluxClaim.spec.convergeInterval` / `ArgoCDClaim.spec.convergeInterval`, defaulting to something like
`1m`, with `0` keeping `0.54.0`'s behaviour. The decision stays in the branch worker, like every
other one; the claim's own periodic reconcile may be the natural clock. Batched by construction: a
burst of saves inside one interval costs one apply, not one per push.

It is a timer on a reason you already have. A `Converge` already applies the tip "because nothing
says the applier runs it", and already asks nothing when the applier's last succeeded operation
applied that commit. Today it fires once at worker start and once per new `GitTarget`; this asks for
it on a clock as well.

**Why.** `applier-echoes.md` S2 is right about the cluster: after the flush, an apply following the
worker's own push changes no object, so it protects nothing there. But the applier's **own state**
is read by other things, and on `0.54.0` that state stops moving:

- the `Kustomization`'s `status.lastAppliedRevision`;
- the `GitRepository`'s artifact. It fetches only when the claim asks it to apply, so it stays at
  the last foreign commit as well;
- everything downstream of those two: Flux's commit-status notifications, `flux get kustomizations`,
  any promotion or packaging step that consumes "what was applied".

Measured on `0.54.0`, after one conformance run and one editor click-through: branch tip
`be3b08d2`, artifact **and** applied revision `c81500d0`, the last commit somebody else pushed —
**15 commits behind, and it moves only when somebody pushes in GitHub.** On `0.53.0` the quiet-branch
`CatchUp` kept them level.

It costs us one feature. We publish each repository's folder as an OCI image from what the forward
leg **applied** (applied revision == artifact revision), downloaded from source-controller. On
`0.54.0` an edit made in our editor is mirrored, committed and attributed perfectly, and then never
published until an unrelated push happens. We cannot work around it from our side: the only lever
we have is to fetch or reconcile the Flux objects ourselves, and reconcile ownership tells us — rightly
— that a fetch of ours between your check and your resume applies a commit nobody checked.

**Why not after every push.** That was `0.53.0`'s `CatchUp` minus the quiet period, and it would make
every save cost an apply. A bounded interval keeps the applier eventually level for whoever reads its
state, and the cluster pays at most one no-op apply per interval per applier.

### 34. An e2e for an in-cluster applier with a remote source cluster

**Ask.** One e2e where the `GitTarget`'s `KubeProvider` names a remote cluster (`secretRef`) and the
`FluxClaim`'s names the operator's own (`inCluster`), with an API edit landing during an apply.

**Why.** It is our production topology: Flux runs in the management cluster and applies into a remote
workspace through `spec.kubeConfig`; the reverser reads that workspace remotely and holds the
`Kustomization` locally. We have proven the hold, the apply cycle, the receiver and `ForeignCommit`
in it (21/21 in our conformance probe, on `0.53.0` and on `0.54.0`). We have **not** seen the apply
window withhold anything in it — `apply_window_collisions_total` stayed at zero across every run —
so the part of #29 that reads `managedFields` on the remote object is untested in this topology by
anyone. An e2e that provokes one collision would settle it.

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

Both are already on your list. These are the measurements behind our vote, not new asks.

**Filter `kube-root-ca.crt`** (`docs/TODO.md`, "Filter more cluster-generated noise"). Our tenants
mirror ConfigMaps, so every tenant repository carries the workspace's cluster CA certificate — a
file nobody wrote, that means nothing outside that one cluster. Every rebuild of the cluster gives
it a new CA, and the first snapshot commits the change as a `message_source="reconcile"` commit by
the committer. That is the only commit-without-a-human our alarm
(`git_commits_total{author_kind="committer"}`) sees, and it fires once per rebuild on every
tenant. A default filter, with an opt-back-in for whoever really wants it, would remove both.

**The durable worker queue** (`docs/TODO.md`, "Make the worker queue durable"). This is the one cost
of sharing an instance that our users can see. `RenderMatchesLive` starts `Unknown` after a restart,
an open commit window is dropped, and a save made in that window loses the message its author typed
and its attribution. `0.54.0`'s #443 now holds live edits while fidelity is pending, which fixes the
edit; as far as we can tell the window's message is still dropped. It is the last item standing
between us and "a deploy of the reverser is invisible to the people editing".

## Process notes

- **Retire a field by rejecting it first.** `0.44.0` kept `eventTemplate`/`groupTemplate` in the
  schema for one release with a CEL rule that refuses them, and `0.53.0` did the same for
  `allowAnySourceNamespace`. Both failed loudly in our operator instead of silently changing
  behaviour. When a field is simply removed, the API server prunes it and the object stays
  `Ready=True`, so we write tests asserting the _absence_ of old spellings. Making
  rejection-before-removal the default would make those tests unnecessary.
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
| 5 | A `CommitRequest` that can assert an author | the admission webhook records the submitter (#220 itself closed unmerged) | **Happy** — a save's author comes from who filed it |
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
| — | `WatchRule` readiness lag (never filed) | `0.44.0` (#343) | **Happy** — rule readiness is usable as an alert |

**#2 — ignore writes by an identity or field manager. ⚫ WITHDRAWN 2026-07-14, our mistake.** The
forward leg's echo is self-terminating: the applier applies the commit the reverser just pushed, the
content matches, the echo commits nothing. We built the filter ourselves and **it caused a
two-and-a-half-minute flip-flop**, every commit unattributed. **Please never add
`excludeUsers`/`excludeFieldManagers` on our account** —
[why](https://github.com/ConfigButler/gitops-api/blob/main/docs/design/forward-leg-loop.md). The right
shape was the one you already had: sanitize the content, do not filter the writer.

## What is _not_ upstream's problem

So a wishlist does not become a blame list:

- **The audit muxer** was our integration with kcp, not a reverser concern — and `v0.39.0` let us
  delete it.
- **kcp's front-proxy** panicking on an anonymous request, and its per-workspace authenticator
  answering `Unauthorized` for a minute after a fresh start. kcp's, not yours.
- **Broken label references in our own alerts**, found while mapping our dashboards onto a new
  metric surface. Ours, every time — which is why we now test our rules against `0.54.0`'s labels.

## Suggested order

1. **#35** — the only open ask with a cost to us today, and a timer on a reason you already have.
2. **#34** whenever the e2e budget allows — our production topology, and the one part of #29 nobody
   has seen work in it.
3. The two TODO items above, in whichever order your plan already has them.
4. **#10** — cosmetic, whenever.
