# Argo CD and bi-directional GitOps

## Thesis

Argo CD's `selfHeal` and bi-directional GitOps are **opposite intents on the same
event**. Self-heal exists to *erase* live drift; bi-directional exists to
*capture* it. On a genuinely shared resource they cannot both be satisfied, and no
amount of configuration reconciles them — because the moment a live write lands,
Argo already has everything it needs to revert it, and there is no hook where an
external system can say "wait, this one is legitimate."

The one Argo CD configuration that supports a shared field changing from **both**
directions is:

- **automated sync with `selfHeal: false`**, plus
- **a push webhook** (Gitea/GitHub → argocd-server `/api/webhook`) for the
  Git → cluster direction.

Everything else that looks like a solution — `ignoreDifferences`,
`managedFieldsManagers`, three-way merge, PreSync hooks, self-heal timing knobs —
is either **split-ownership dressed up as bi-directional** or **does not work**.
This document explains why, with the code paths that prove it, so the conclusion
is not folklore.

All citations are against the Argo CD tree vendored at
`external-sources/argo-cd` (`VERSION` = 3.5.0 dev; the e2e installs the v3.4.x
line, which is materially identical for these paths). Exercised end-to-end in
[`../../../test/e2e/argocd_bi_directional_e2e_test.go`](../../../test/e2e/argocd_bi_directional_e2e_test.go);
corner design in
[`../e2e-bi-directional-corner.md`](../e2e-bi-directional-corner.md).

## How fast is self-heal, and why it cannot be slowed

The first question anyone asks is "how is it *so* fast?" — fast enough that a
`kubectl edit` is reverted before you've switched windows. The answer is two
design choices, and they are also why you cannot make it patient.

**1. It is watch-driven, not poll-driven.** The application-controller keeps a
live cluster cache backed by a real Kubernetes `watch` per resource type
(`gitops-engine/pkg/cache/cluster.go:863` `watchEvents`). A live write is
delivered by the apiserver's watch stream in **milliseconds**, batched on a
default **100 ms** ticker (`cluster.go:82` `defaultEventProcessingInterval`), then
mapped straight back to the owning Application via the tracking label/annotation
stamped on the cached object (`controller/cache/cache.go:659` `OnResourceUpdated`
→ `getApp` → `requestAppRefresh`). The refresh is enqueued after ~1 ms
(per-item rate limiting is disabled by default,
`pkg/ratelimiter/ratelimiter.go:21`). No timer sits in this path.

**2. Self-heal compares against cached desired state — it never fetches Git on
the hot path.** The event-driven refresh runs at compare level `CompareWithRecent`,
which pins the revision to `app.Status.Sync.Revision` — the **last-synced SHA, not
the branch HEAD** (`controller/appcontroller.go:2119`). It then calls the
repo-server with `NoCache=false`, and the manifest cache is consulted *before any
git work* — a hit returns immediately with no clone and no Helm/Kustomize render
(`reposerver/repository/repository.go:415`). This is the literal code path for
"replays the stale cached revision": the desired state Argo re-applies is the one
it already had in memory, which does not include whatever GitOps Reverser just
committed.

**3. The "2 seconds" is a repeat throttle, not the first revert.** On the first
drift, `SelfHealAttemptsCount == 0`, so the backoff is **zero** — the revert is
essentially immediate, sub-second (`controller/appcontroller.go:2679`). The
`2s → 6s → 18s → 54s → 162s → 300s` sequence
(`--self-heal-backoff-timeout-seconds=2`, factor `3`, cap `300s`;
`cmd/argocd-application-controller/commands/argocd_application_controller.go:313`)
only throttles **repeated** heals to the *same* revision. And crucially, both the
backoff and the legacy flat `--self-heal-timeout-seconds` are measured from the
*last sync's finish time*, not from the drift: `retryAfter = delay − timeSinceLastOp`
(`appcontroller.go:2805`). So a drift that happens any meaningful time after the
last sync yields `retryAfter ≤ 0` → immediate. **There is no knob that delays the
first revert of a fresh drift.** Argo is designed to close drift as fast as
physically possible, and it succeeds.

Do not conflate this event path with `--app-resync` (default 120s). That periodic
full refresh re-resolves Git (`CompareWithLatest`) and is the *slow* path; it is
the fallback safety net, not what drives self-heal.

## The core incompatibility

With `selfHeal: true` on a shared path:

1. Argo has applied revision `A` and the app is Synced.
2. An operator edits the live object through the API.
3. The watch fires; within ~100 ms Argo compares against cached revision `A`,
   sees drift, and re-applies `A` — **the edit is gone**, sub-second.
4. GitOps Reverser, which also saw the live edit, commits revision `B`… into a
   cluster that has already been reverted. Now Git says `B`, the cluster says `A`,
   and the two loops thrash: history flaps between the change and its own revert.

This is the causality failure, with Argo's timing weighted maximally against the
capturing side. It is not a race you can win by being faster on the reverser; the
reverser writes to *Git*, and self-heal never reads Git.

## Dead ends (the distractions)

Each of these is proposed regularly. Each is a dead end for a *shared, both-ways*
field, for a concrete reason.

### `ignoreDifferences` / `managedFieldsManagers` / three-way merge — this is split-ownership, not bi-directional

`ignoreDifferences` (whether by `jsonPointers`, `jqPathExpressions`, or
`managedFieldsManagers`) removes a field from the diff, so it never registers as
`OutOfSync` and self-heal never touches it — auto-sync only fires on `OutOfSync`
(`controller/appcontroller.go:2602`). `managedFieldsManagers` is the same thing
with fancier selection: it drops fields owned by a named field manager
(`util/argo/managedfields/managed_fields.go:15`).

But removing a field from Argo's diff removes it in **both** directions. A field
Argo ignores is a field Argo will **never apply from Git**. So the moment you
carve `/spec/scoops` out to protect an API-side edit, a *Git-side* change to
`/spec/scoops` silently stops reaching the cluster. That is precisely **not**
bi-directional — it is [split ownership](../../bi-directional.md#recommended-modes):
"this field belongs to the cluster, Git never drives it." Split ownership is a
legitimate mode, and `ignoreDifferences` implements it well. It is just not the
thing we are trying to build, and presenting it as "safe self-heal" is misleading:
it is safe precisely because it has opted the field out of GitOps.

Verdict: **valid for split ownership; a distraction for bi-directional.**

### PreSync hooks — cannot delay-then-apply-fresh, and cannot tell a heal from a deploy

The natural next idea is a PreSync hook: gate the sync, buy time for Git to catch
up. Two independent blockers, both in `gitops-engine/pkg/sync/sync_context.go`:

- **The operation's target manifests are frozen at the start.** `Sync()` builds
  its task list once (`sync_context.go:496` `getSyncTasks`) and walks phases —
  PreSync wave, then Sync wave — without ever re-resolving Git. So a slow
  PreSync that then *succeeds* applies exactly the manifests captured at operation
  start: your live change is reverted, only later. Slowness buys nothing.
- **A hook cannot distinguish a self-heal from a real deployment.** PreSync runs
  before *every* sync, including the legitimate Git → cluster deploys you want. The
  only way to abort a self-heal is to *fail* the PreSync — but that same failure
  aborts your real deployments too, because the hook has no signal for "this sync
  is drift-correction."

So PreSync can *delay* (uselessly) or *blanket-veto* (destructively), but it cannot
express "wait, and if Git caught up, skip the revert." Verdict: **does not work.**

### Self-heal timing knobs — throttle repeats, not fresh drift

Covered above: `--self-heal-timeout-seconds` and the backoff are inter-attempt
throttles measured from the last sync, so the first revert of a fresh drift is
always immediate. There is no "pause N seconds before reverting new drift"
setting. Verdict: **cannot be used to be slow.**

## The configuration that works

Turn self-heal **off** and let the two directions flow without a reverting
controller in the middle:

- **Cluster → Git.** With `selfHeal: false`, Argo does not revert live drift — it
  marks the app `OutOfSync` and waits. GitOps Reverser captures the live change to
  Git at its own pace. You can be as slow as you like; nothing overwrites the
  cluster in the meantime.
- **Git → cluster.** A push webhook (which the corner wires up:
  `argocd-server` `/api/webhook`, Gogs-format from Gitea) makes Argo apply a new
  Git commit within seconds — *including a change to the same shared field from the
  Git side*, which `ignoreDifferences` would have broken.

```
   API edit ─▶ Cluster ──watch──▶ GitOps Reverser ──commit──▶ Git
      ▲                                                        │
      └──────────── apply ◀── Argo CD (selfHeal off) ◀─push webhook┘
```

This is genuinely bi-directional on a shared field, and it is exactly the
configuration the e2e corner exercises once you drop the `selfHeal` flag. The
webhook is what makes it comfortable rather than sluggish — without it, the
Git → cluster direction waits on Argo's 120s poll.

**What you give up** is self-heal's one unique service: reverting drift that
*should not* be captured (a rogue or accidental change). That loss is inherent —
you cannot ask one flag to both "erase unauthorized drift" and "keep authorized
drift," because Argo has no notion of authority.

## If you need "revert unauthorized drift" as well

"Erase the bad edits, keep the good ones" is a **per-write authority decision**,
and it must be made where the write happens — a Kubernetes admission gate — not by
Argo, which sees only "the live object differs from cache." This is GitOps
Reverser's existing territory (it already runs admission webhooks): a policy could
decide, at write time, whether an edit is a sanctioned bi-directional change (let
it through, to be captured) or unsanctioned drift (reject it). That is the only
mechanism that recovers a self-heal-like guarantee without self-heal's
incompatibility, and it is out of scope here — noted so the option is not
forgotten.

## One-line verdicts

| Mechanism | Bi-directional on a shared field? | Why |
| --- | --- | --- |
| `selfHeal: true` | **No** | reverts the live edit sub-second, from cached Git |
| `ignoreDifferences` / `managedFieldsManagers` | **No** (split-ownership) | carves the field out of GitOps in *both* directions |
| PreSync hook (delay) | **No** | target frozen at operation start; reverts stale later |
| PreSync hook (veto) | **No** | also aborts legitimate deploys; can't tell them apart |
| self-heal timeout / backoff | **No** | throttles repeats; first drift reverted immediately |
| `selfHeal: false` + push webhook | **Yes** | nothing reverts; both directions flow, fast |
| admission gate on Argo's writes | (recovers "revert unauthorized") | per-write authority decision; external to Argo |

## References

- User-facing guidance: [`../../bi-directional.md`](../../bi-directional.md)
- E2E corner design: [`../e2e-bi-directional-corner.md`](../e2e-bi-directional-corner.md)
- Exercised behavior: [`../../../test/e2e/argocd_bi_directional_e2e_test.go`](../../../test/e2e/argocd_bi_directional_e2e_test.go)
- Argo CD source (vendored): `external-sources/argo-cd` — cited inline by file:line.
