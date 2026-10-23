# One writer per branch: where it stands

> **status**, 2026-10-23. This page was the plan of 2026-10-21. Most of it has shipped, unreleased,
> in #428, #429, #430 and #432. It now records what exists, how that differs from the plan, and what
> the plan asked for that is still missing. The contract itself is in
> [configuration.md](../configuration.md#sharing-connections-across-namespaces) and
> [architecture.md](../architecture.md); the phased API break that carried most of it is
> [gitbranch-and-kubeprovider-plan.md](gitbranch-and-kubeprovider-plan.md).

## The direction it served

GitOps Reverser is moving toward an event-sourced state machine per branch, with durable execution
([branch worker event model](branch-worker-event-model.md)). The plan judged every option against
five properties:

| | Property |
|---|---|
| **E1** | One owner per branch: one loop orders every decision about a branch |
| **E2** | Every input is an event, in order, on that branch's stream |
| **E3** | Nothing waits by polling another owner |
| **E4** | Decisions are recorded as actions (pending writes) |
| **E5** | The journal key is the branch itself: canonical repository plus branch |

In October 2026 the worker was keyed by `GitProvider` and branch, so two `GitProvider`s for one
repository gave a branch two workers ("siblings"). Siblings broke E1, E2, E3 and E5, and every
defect the #425 self-review found in the sibling flush came from that shape.

The plan recommended option F (one shared `GitProvider` per repository, referenced across
namespaces, the Flux model) together with five rules. All five properties now hold.

## What we have now

### One worker per repository branch (rule 1): done, #428

- The worker key is [`BranchKey{Repository, Branch}`](../../internal/git/types.go), with the
  repository as `repourl.Normalize` returns it. Same-host SSH and HTTPS spellings are one key.
- `apply_siblings.go` and `apply_sibling_flush.go` are deleted, and with them the shared flush
  generation, the copied observations and the cross-worker source-group claims. Source groups are
  local to the one worker.
- The worker keeps the owning provider for credential, committer, signing and metric labels.
- The HA plan's `BranchWriteShard` is this key
  ([shard identity](../future/ha-gittarget-distribution-plan.md#shard-identity)).

### One credential per repository branch (rule 2): done, in a stronger form, #429

The plan said: elect implicitly from the `GitTarget`s and refuse a branch written through two
`GitProvider`s. #428 built exactly that, with the oldest `GitTarget` winning. That left a squat gap:
any namespace could hold a branch without a working credential. #429 replaced the rule with an
explicit object:

- **`GitBranch`** is one branch of the repository its `GitProvider` names. It holds the branch for
  every `GitTarget` that references it: one worker, one clone, one credential, one signing key.
- **The branch claim**, in [`branch_owner.go`](../../internal/git/branch_owner.go): a `GitBranch`
  becomes a candidate only once its credential opens a receive-pack session. The oldest candidate
  holds the branch. The holder keeps it through later proof failures. The claim is in memory and is
  recomputed after a restart.
- A younger `GitBranch` for a held branch reports `Stalled=True`, reason `BranchConflict`, naming
  the holder. Status carries this; the plan's admission refusal was dropped
  so that a folder of manifests can be applied in any order.
- `GitBranch.spec.gitProviderRef` is a `LocalObjectReference`, so creating a claim takes the right
  to create objects where the credential lives.

So option B's per-write credential selection never had to exist, which was the purpose of rule 2.

### The shared credential (rule 3, option F): done, with a different reference, #428 + #429

- `GitProvider.spec.accessFrom` admits other namespaces by name or label selector. A provider
  always admits its own namespace.
- The cross-namespace reference is **`GitTarget.spec.gitBranchRef`** (namespaced), not
  `gitProviderRef` as the plan drew it. One policy governs both hops: a `GitTarget` in another
  namespace may use a `GitBranch` while that branch's `GitProvider` admits the target's namespace
  ([`gitBranchAdmitted`](../../internal/authz/kubeprovider_admission.go)). This is checked on every
  reconcile, so a policy that is tightened later revokes access.
- One Secret in the platform namespace serves every team. Self-service stays: a team can still own
  a `GitProvider`, a `GitBranch` and a Secret in its own namespace.
- Committer and signing come with the credential, so a branch is signed by one key.

### The decoupled receiver (rule 5, gitops-api #30): done, #432

The plan's text was one endpoint, routed by repository and branch, with a `GitProvider`'s own
receiver kept for self-service. What shipped is simpler:

- `GitReceiver` is a namespaced kind. `GitProvider.spec.receiver` is gone.
- A verified delivery wakes the worker of **every** `GitProvider`, in any namespace, whose
  `spec.url` names the pushed repository, for that branch only. It has no reference and no
  `accessFrom`.
- Waking across namespaces grants nothing: the worker reads the remote with its own credential and
  holds at most one pending push notice. The `202` has no body.
- For self-service, a team creates a `GitReceiver` in its own namespace.
- `GitBranch.status.lastPushNoticeAt` shows whether deliveries arrive.

### Decided differently from the plan

| Plan | What we did | Why |
|---|---|---|
| Report `status.branches` to every routed `GitProvider` | Removed; `GitBranch` status replaces it (`status.remote`, `gitTargetCount`) | Listing `GitBranch` objects answers the question |
| Move metric labels to the repository-branch key (breaking) | Kept `{provider_namespace, provider_name, branch}`, taken from the holding `GitBranch`'s provider | gitops-api dashboards; the label set still names one branch worker |
| Refuse a second `GitProvider` for one repository in one namespace, before anything else | Not built; no longer needed | The claim is per `GitBranch`, so extra providers on one repository are harmless, and useful (see "Still missing", item 1) |
| Ship additively, then the key change a release later | One release, hard cuts, no migration | No users yet |
| C2 (`ClusterGitProvider` or `GitCredential`) only if one object per repository is too many | Not built | Nobody has hit the limit |

## Still missing against the original

Ordered by how much they matter now.

1. **Branch-scoped access ("who may write what").** The plan's rule 3 and decision 3 wanted
   `accessFrom` to say which namespaces may write *which branches*, optionally *which paths*. What
   shipped is namespace-only and provider-wide: every namespace a `GitProvider` admits may use
   every `GitBranch` of that provider, and write any folder that the overlap check does not already
   hold. Branch isolation is possible today without code: create one `GitProvider` per team in the
   platform namespace. Every one of them references the same Secret, with
   `allowedBranches: [teams/team-a/*]` and `accessFrom: {names: [team-a]}`. Path isolation is not
   possible at all. [gitbranch-and-kubeprovider-plan.md](gitbranch-and-kubeprovider-plan.md)
   parks this as "Folder and branch grants per namespace", and
   [who may use a provider](../future/provider-access-rules.md) built it as grants
   (`allowedGitBranches`, `GitBranch.allowedGitTargets`), and also covers item 2 below.

2. **The claim is weaker than "one credential per branch, enforced".** Three gaps:
   - **The push-session proof proves less on some hosts.** On Gitea and Forgejo, a credential that
     can only *read* opens a receive-pack session (the AGit flow). On a host that offers push
     sessions anonymously, it proves only that the repository is reachable.
   - **Who claims first holds the branch.** Anyone who can create a `GitProvider` and a `GitBranch`
     with a credential that passes the proof can claim a branch that no older `GitBranch` holds.
     That includes a tenant in its own namespace with a copy of the token. The plan's rule 2 tied
     the branch to the shared provider. The claim instead ties it to the oldest proven object on
     that repository, wherever it lives.
   - **An older `GitBranch` takes the branch from a live holder** once it proves itself, and after
     a restart a holder whose credential broke loses its claim. A persisted claim belongs to the
     HA work.

   Only a per-repository authority closes these: option C's object per repository URL, or a
   cluster-level rule naming the one provider through which a repository's branches may be
   claimed. Nothing of that exists.

3. **Alias detection (rule 4, option E).** Not built. `repourl.Normalize` still cannot unify Gitea's
   SSH on `:2222` beside HTTPS, a separate SSH hostname, a vanity domain, or a renamed repository.
   Two such URLs get two keys, two claims and two workers on one branch: the sibling problem,
   undetected. The HA plan's HA-0 question about aliases is still open. The design is unchanged:
   a warning when a commit we pushed under one key shows up under the other, in both directions.

4. **The #425 self-review items that are not about siblings.** R2 and R3 went with the siblings,
   and R5 (the `UPGRADING.md` entries) is done. Reading the code, R1 and R4 still look open.
   - **R1:** an applier that joins a flush already in progress starts `flushEdits` at the moment it
     joins, not at the flush mark
     ([`flushed`](../../internal/git/applier_claim.go)).
   - **R4:** `forgetSettledSnapshots` still runs only on `GitTarget` teardown, so a replaced worker
     can read the old worker's settled-snapshot record.

   Both affect a user with a single `GitProvider`. The local review is
   `docs/reviews/20261021-review-425.md`.

5. **A rate bound on deliveries.** Folding bounds the queue but not the rate, so a token holder can
   keep a worker reading continuously. This is accepted for now; a per-worker cooldown would bound
   it.

6. **Per-tenant identity on the Git host.** This is by design. A shared provider
   pushes as the platform's account, so the host's audit log names the platform instead of the
   team, and one team's credential cannot be revoked separately. A team that needs either keeps its
   own provider. The plan named this as Flux's argument for an identity per object and accepted the
   trade.

## Stale text elsewhere

- [HA plan, shard identity](../future/ha-gittarget-distribution-plan.md#shard-identity) says "one
  `GitProvider` owns a repository branch: a `GitTarget` writing it through another one is
  refused". Since #429 it is the holding `GitBranch`, and a younger `GitBranch` reports
  `BranchConflict`.
- [gitbranch-and-kubeprovider-plan.md](gitbranch-and-kubeprovider-plan.md) still draws
  `FluxTrigger`/`ArgoCDTrigger` and `applyTriggerRef` (now `FluxClaim`/`ArgoCDClaim` and
  `applierClaimRef`, #438). Its status line says phase 3 is still to come, but it shipped in #432.
