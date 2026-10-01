# GitTarget parent branch: hardening pass before #407 merges

> **Plan**: open, nothing built. Dated 2026-10-01. It comes from a three-way review of #407 and two
> rounds of second opinions; §7 records the design choices those rounds settled.
>
> **The merge boundary.** #407 merges when this pass is done: the existing parent-branch feature
> behaves correctly under races and recovery, and stays as cheap as it is today. New policy
> (empty-repository bootstrap rules, ancestry status) is not part of it.
>
> Related: [`gittarget-parent-branch.md`](gittarget-parent-branch.md), the feature as built;
> [`gittarget-parent-empty-repository.md`](gittarget-parent-empty-repository.md) and
> [`gittarget-parent-observation.md`](gittarget-parent-observation.md), deferred follow-ups.

## 0. Handoff: read this first

### Where the code is

- Branch `fix/new-branch-checks-parent` = PR #407. #408 (the `spec.parentBranch` field) is already
  merged into it.
- Run `git fetch` and `git log origin/main..HEAD` before starting.

### Prerequisite: CommitRequest liveness

The recovery in §4.4 can hold work for up to its 5m backoff cap. Today the controller fails a
CommitRequest closed after `attachTimeout + maxDuration + 120s` (about 124s with defaults), counted
from creation, and the worker keeps the attach. So a request retained through a recovery can be
reported `FinalizeFailed` and then committed anyway.

Fix this first, in a small PR against main (it is the first item in [`../TODO.md`](../TODO.md)).
Raising the timeout only moves the contradiction. Controller and worker must agree:

- **While the worker holds the request** (waiting, collecting, retained for a push, or blocked on a
  missing parent), the worker is authoritative and the controller keeps polling. A blocked request
  reports a phase that names the cause.
- **The controller's bound applies only to a request the worker does not know** (a vanished worker).
- **Failing closed withdraws the attach first**, so a request reported failed can never be
  committed later.

Test: a request retained through a recovery longer than the old bound resolves `Committed`.

### Out of scope for this pass

- [`gittarget-parent-empty-repository.md`](gittarget-parent-empty-repository.md): which branch an
  empty repository's first commit may create. Keep today's bootstrap behavior here.
- [`gittarget-parent-observation.md`](gittarget-parent-observation.md): the parent's state after
  the write branch exists.

### Process

- Follow `AGENTS.md`. Its validation sequence is `task fmt`, `generate`, `manifests`, `vet`,
  `lint`, `test`, `test-e2e`. Run the e2e commands sequentially, and check `docker info` first.
- **When to push: the user's standing instruction overrides AGENTS.md's "When to push".** AGENTS.md
  says Git-write-path changes wait for a local e2e pass. The user has said, more than once:
  "Don't wait for passing of e2e, as soon as linting is green you push", and "never ever keep
  these commits unpushed: now we have to wait another 30 minutes for CI".
  - So commit and push in one step as soon as `task lint` and `task test` pass.
  - Keep local e2e running in the background, or read the CI legs, and fix forward.
  - The PR is not ready until e2e is green somewhere.
- Use one commit per work item. Each item's red tests come first: either in their own commit, or
  in the same commit with the red-then-green recorded in the PR body.
- At the end, update the #407 PR body. Add one entry per item to its "Review fixes" section.
  After each push, read the CodeRabbit inline comments on `pulls/407/comments`; they are not the
  same as `gh pr view --comments`.

### Test harness facts

- Any claim about server-side compare-and-swap, or about a refused upload, needs
  `startRealGitServer` (canonical `git http-backend` over HTTP). The go-git v6 `file://`
  transport never compares `cmd.Old`, so such a test passes vacuously over `file://`. Tests that
  only assert our client-side rejection may use `file://`.
- `pushAtomicFn` is a package variable, so a test can wrap it. `fetchRemoteBranchHashFn` is one
  too. Where §4.1 needs a hook at a specific point, add a nil-by-default package variable and set
  it only in tests.
- `TestGitRoundTripLedger` runs against its golden file `internal/git/testdata/git-roundtrip-ledger.golden`.
  The golden file counts connections to the Git host for each operation. It is the cost gate
  (§1).
- Run controller envtest alone. It binds `:8080`, so a parallel run on the same host fails 21
  unrelated specs.

**Do not re-raise.** These were fixed in earlier review rounds:

- the concurrently created write branch, at the advertisement;
- the stale repository identity behind `ParentBranchNotFound`;
- retained writes not rebuilt on a parent change;
- path validity gating parent conflicts;
- the mutability of `parentBranch`, which is now immutable;
- the parent-change flag lost on a failed rebuild (`dcbc8b82`). §4.1 replaces that mechanism, but
  its regression test `TestBranchWorker_AParentChangeSurvivesAFailedRebuild` must stay green.

## 1. The contract this pass must not break

**API first, not API only.** The design is optimized for a remote that rarely moves under us. The
normal path is:

> use the trusted checkout → validate it through the push advertisement (a connection the push
> opens anyway) → fetch, reset and replay **only** when that advertisement says the base is
> stale.

There is **no unconditional preflight fetch on a trusted checkout**. A pull before a push cannot
close the race anyway: the remote can move between any fetch and any push. What closes it is the
compare-and-swap plus the bounded replay, and both already exist. The legitimate fetches are:

- bootstrap;
- a base that was invalidated, including by a parent-configuration change (§4.1);
- recovery after a detected change, a failed write, or a refused push.

The guarantee we offer, and the only one:

- The parent matches the push advertisement.
- The server's compare-and-swap protects the write-branch update. `Old` is the advertised tip, or
  zero for creation.
- The parent can still move after the advertisement, and nothing pretends otherwise.

### Acceptance criteria for every item

1. **Cost.** Unchanged remotes keep today's request counts. Ledger rows **1–12** must not change.
   §2 names the new rows.
2. **Bounded recovery.** A detected change triggers recovery that is bounded in attempts, in
   connections, and in how long it waits.
3. **Progress.** Pending work eventually lands **without another cluster edit**, with
   `--git-refresh-interval` on or `0`. That includes a recovery attempt that fails transiently
   (§4.4).
4. **Loud failure.** A situation we cannot resolve correctly surfaces as a condition reason with
   an actionable message. It never produces an orphan branch, or a branch on the wrong parent.

## 2. Use cases, ranked by how often they happen

The ranking decides what must stay fast:

- **Hot paths (H)** have a connection budget that the ledger enforces.
- **Rare paths (R)** may spend a fetch, but each one needs a deterministic test and either a
  correct outcome or a clear failure.

| # | Situation | Expected | Budget (connections) |
|---|---|---|---|
| H1 | Write branch exists, remote unchanged, steady edits | commit and push | 2 per publication, 1 when there is nothing to commit (rows 3–5, unchanged) |
| H2 | Write branch absent (standby), idle | nothing written; the refresher only reads | 1 advertisement per refresh tick, 0 fetches (row 12, plus `TestRefresh_AnAbsentBranchWithAnUnmovedParentCostsOneConnection`) |
| H3 | Standby; a no-op live write (a deployment echo) | the branch stays absent; the request resolves `AlreadyPresent` | 1, the push advertisement; 0 fetches. **New row** |
| H4 | Standby; first real edit; parent unmoved since the checkout | branch created on the parent's tip | 2, 0 fetches (row 11, unchanged) |
| H5 | Standby; parent moved since the checkout | client-side refusal → contention fetch → replay → push | **New row**, pinned for the new-branch case (expected to equal row 6) |
| H6 | Write branch deleted after a merge; next edit | back to standby, recreated from the parent | one replay (existing lifecycle test) |
| H7 | Explicit `spec.parentBranch`, and the parent exists | as H2–H6, against that branch | same budgets |
| R1 | Parent configuration changes on a live worker (all targets replaced before the worker sweep) | the next push not yet admitted uses the new parent (§4.1) | at most one fetch per change |
| R2 | Empty repository gains its first ref while writes are pending | writes rebuilt onto the new default branch and published, or refused per R3 | one replay |
| R3 | Non-empty repository with no resolvable default branch, parent omitted | refuse with an actionable reason; no orphan | probe budget (§4.4) |
| R4 | Configured parent missing, later pushed | recover and publish without another cluster edit | probe budget (§4.4) |
| R5 | Remote default branch switched (parent omitted) | followed at the next discovery; documented | one fetch at that discovery |
| R6 | Write branch created by someone else after our advertisement, with the parent unmoved | the server refuses the upload → replay onto their commit | **its own new row**, measured (§4.7). It is not row 6: row 6 is refused at the advertisement, before any upload |

**Probe budget.** While a parent is missing or unresolved, the remote is asked at most once per
backoff deadline per worker. That holds however many targets share the worker, and however many
reconciles and refresh ticks occur. §4.4 defines it and tests it.

## 3. Decisions already taken (do not reopen)

- **Keep `status.remote.parent`.** Revise the vocabulary rule instead (§4.5).
- **An omitted parent means the default branch resolved at the last discovery** (a fetch, a
  refresh, or a probe).
  - Publication validates that branch's tip in the push advertisement. The receive-pack
    advertisement carries no `HEAD`, so publication cannot see a switch, and we do not add a
    lookup before pushes to rediscover it.
  - Stable intent is what `spec.parentBranch` is for.
- **The push admission boundary** (§4.1) decides when a configuration change takes effect.
  - A change before admission makes that attempt replay.
  - A change after admission does not recall the push. If that push created the write branch from
    the old parent, the branch now exists, and an existing branch does not follow its parent.
    That is consistent with the contract; document it.
- **The e2e contract.**
  - e2e asserts ancestry and content: the first commit's parent is B, the files are present, and
    the branch stays absent on standby.
  - Deterministic worker tests with no refresher prove that publication alone finds a moved
    parent.
  - e2e does not assert which fetch reason found B (§4.6).
- **Default-branch resolution follows go-git, not a stricter rule of our own** (§4.3).
- **Recovery comes before cost.** §4.4 writes the recovery tests first. If the worker-side probe
  grows larger than expected, ship the recovery obligation and its tests with a simpler
  controller-side backoff (§4.4, fallback). Never weaken recovery to save fetches.

## 4. Work items, in order

### 4.1 Parent changes on a live worker (review items 1–2) → R1

#### Problem

`SetParentBranch` runs on the controller goroutine. It drops base trust once and raises
`parentChangedState`. The event loop can then undo or bypass that:

- **During a push or fetch.**
  - `runPushCycle` calls `setBaseTrusted(true)` after the push returns (`branch_worker.go`
    ~2021). `updateBranchMetadataFromPullReport` does the same after a fetch (~2953). Either can
    run *after* the drop.
  - `recoverRetainedWrites` with nothing retained then swaps the flag to false (~1416), relying on
    a distrust that is gone.
  - The `newBranchParent` shortcut in `ensureWriteBranch` (~2346) then roots the cycle on
    `(main, A)`, and the push is accepted.
  - Reproduced in review.
- **Between recovery and publication.**
  - `pushPending` (~1656) runs `recoverRetainedWrites`, then `pushPendingCommits`. A change that
    lands between the two is pushed on the old root.
  - Reproduced.

#### Design: one configuration snapshot, generation-scoped trust, and one admission boundary

1. **Configuration snapshot.**
   - Replace `parentBranchName` + `parentChangedState` with one atomic pointer to an immutable
     `parentConfig{name string; gen uint64}`.
   - `SetParentBranch` installs a new value, with `gen+1`, only when the name changes. It keeps
     its `invalidateBase` as a belt.
   - Every operation that consults the parent (fetch/sync, refresher advertisement, the §4.4
     probe, the rebuild) loads the snapshot **once, at its start**, and uses that `{name, gen}`
     throughout.
   - The snapshot also goes into the observation that operation records: `ParentRequested` (see
     §4.3) and the generation.
   - Nothing re-reads the current name after the network call returns. That way an old lookup can
     never report a result about a newly configured parent.
2. **Generation-scoped trust.**
   - The worker keeps `baseParentGen` (under `repoMu`): the generation of the snapshot whose fetch
     produced the checkout.
   - Trust means `baseTrustedState && baseParentGen == current gen`, so `setBaseTrusted(true)`
     after an old-generation push or fetch can never vouch for the current configuration.
   - `newBranchParent` is honored only when `baseParentGen` is current.
   - The root's generation, `pushCycleRootGen`, is stamped wherever `pushCycleRootBranch` is set:
     in `ensureWriteBranch` and in the rebuild.
3. **Retained writes.**
   - In `recoverRetainedWrites`, "parent changed" becomes `pushCycleRootGen != current gen`.
   - A failed rebuild leaves the generations mismatched, so the next attempt rebuilds again. This
     replaces the `dcbc8b82` hand-back.
4. **The admission boundary.** One local point in `runPushCycle`, under `repoMu`, immediately
   before each `pushAtomicFn` call, including the retries:
   - load the snapshot and compare its generation with `pushCycleRootGen`;
   - **on mismatch, do not push.** Take the existing replay branch: `markReplayRequired`, sync
     with the new snapshot, rebuild, re-stamp, loop. This adds no extra connection;
   - on match, the attempt is **admitted**. Any change from here on is in flight, per §3.

   Add a test-only hook, `beforePushAdmission`, called just *before* the comparison. A change
   injected there is a "before" case. A change injected inside the wrapped `pushAtomicFn` is an
   "after" case.
5. **An existing write branch is unaffected.**
   - When the cycle root is the write branch itself, and it was proven present, a parent change
     cannot alter the plan.
   - Re-stamp the generation without fetching. The push's compare-and-swap still guards the
     branch.

#### Tests (red first; run with `-race` too)

1. **Change during a fetch.**
   - Inject `SetParentBranch("release")` inside the sync of an initial fetch and of a contention
     fetch.
   - Assert that `feature`'s first commit sits on `release`'s tip.
   - Assert that the observation recorded for that fetch names the parent it actually used
     (`main`), not `release`.
2. **Change during a no-op push** that settles as `PushNoBranch`. This is the review's
   reproduction, an "after" case. Assert that the next real write lands on `release`.
3. **Change before admission** (the `beforePushAdmission` hook) in a cycle with retained writes.
   - Assert that no push leaves on the old root.
   - Assert that the replay builds on `release`.
4. **Change after admission** (inside the wrapped `pushAtomicFn`), in a cycle that creates the
   branch.
   - Assert that the branch is created from `main`, which is the documented in-flight rule.
   - Assert that later writes go to the existing branch, with no attempt to re-root it.
5. **The new parent is missing**, in cases 1 and 3.
   - Assert that `feature` is never created.
   - Assert that the work stays retained.
   - Assert that §4.4's recovery picks it up.
6. **An existing write branch, then a parent change.**
   - Assert zero extra connections.
   - Assert that the next push goes to the write branch.
7. `TestBranchWorker_AParentChangeSurvivesAFailedRebuild` and
   `TestBranchWorker_AParentChangeRebuildsRetainedWrites` stay green.

**Cost.** Everything added here is local. The ledger is unchanged.

### 4.2 An empty repository gains a ref (review item 3) → R2

#### Problem

Cloning an empty repository leaves `HEAD` unborn on the write branch, with the cycle root
`(feature, zero)`. If somebody then pushes `main`, neither check fires:

- `validatePushState`'s missing-root check needs a non-zero `rootHash`.
- `remoteMovedSinceBase` compares `refs[feature]`, which is absent and therefore zero, with the
  zero root.

So the push creates `feature` as an orphan (`git_atomic_push.go` ~192–259). Reproduced on the real
server.

**Defining "empty"**, once, and using it in discovery, publication and the observation:

- A repository is empty when its advertisement carries **no hash refs at all**: no branches, no
  tags, nothing else.
- A symbolic `HEAD` pointing at an unborn branch does not count. go-git reports that as
  `RemoteRefs.Unborn`.
- A repository with only tags is **not empty**.

#### Design

- **A new typed error, `RepositoryNotEmptyError{Refs int}`, in `git_atomic_push.go`.** Do not
  reuse `RemoteMovedError`: its `Branch` and `Advertised` fields mean "the root, compared", and
  filling them with another branch's hash would make them lie.
  - `validatePushState` returns it when `rootHash.IsZero()`, the write branch is not advertised,
    and the advertisement is not empty by the definition above.
  - The check reads the advertisement the push already holds, so it costs zero extra connections.
- **`remoteMovedDuringPush` classifies it directly as contention**, without the fallback probe. The
  existing replay then runs:
  1. fetch, which resolves the default branch through §4.3, or refuses;
  2. rebuild, which **re-plans** from the retained writes rather than reusing the orphan commits;
  3. push.
- **Tags only:** discovery finds a non-empty repository with no resolvable default branch. It
  refuses per §4.3, keeps the work retained, and §4.4 takes over.

#### Tests (red first; real git server)

1. **Empty remote with retained work, then `main` is pushed externally.** Start from an empty
   remote holding a retained live write and a `CommitRequest`. Push `main` from outside before
   publication. Assert:
   - `feature`'s first commit has `main`'s tip as its parent;
   - the content is present;
   - `git merge-base feature main` is non-empty;
   - exactly one contention fetch happened;
   - the request resolves `Committed`.
2. **The same with a no-op write.** The branch stays absent, and nothing is orphaned.
3. **Empty remote, then only a tag is pushed.** Assert:
   - no orphan is created;
   - the work stays retained;
   - the observation is `Missing` per §4.3;
   - recovery happens once a branch with a resolvable `HEAD` appears.
4. **Empty remote throughout.** Ledger row 1 is unchanged, and the first push still creates the
   root commit.

### 4.3 Empty versus unresolved default branch (review item 4) → R3

#### Problem

`SmartFetchFrom` returns `""`, the "empty repository" result, in two situations:

- when the advertisement is empty (`git_smart_fetch.go` ~62);
- when refs exist but no default branch resolves and no parent is configured (~77–83, via
  `analyzeRemoteRefs`, which only logs "Remote HEAD is broken").

The caller then runs `makeHeadUnborn`, and the result is an orphan. The surrounding status is
wrong in the same way:

- the observation says `Unborn`;
- `recordAdvertisement` in `refresh.go` (~260) says the same;
- `parentBranchReadiness` treats every omitted parent as fine (`gittarget_controller.go` ~823).

#### What the evidence looks like after go-git (verified at v6.0.0-alpha.5)

`Remote.List` does not hand over the raw advertisement. A hash-only `HEAD`, from a server without
the `symref` capability or from a detached `HEAD`, is rewritten **before** `List` returns.
`packp.ResolveHeadFromHashHeuristic` turns it into a symbolic `HEAD`:

- pointing to `master` if `master` has that hash;
- otherwise to the alphabetically first branch at that hash.

That is git's own spirit too: `guess_remote_head` in `remote.c` prefers fallbacks over requiring a
unique match. So a stricter "exactly one branch matches" rule cannot be built on `Remote.List`
without dropping beneath it.

**Decision: adopt go-git's resolution.** This is a deliberate choice, not an oversight.

- Bypassing `Remote.List` to keep the raw evidence would mean a bespoke advertisement path for a
  rare server configuration.
- The resolved branch is visible in `status.remote.parent.branch`.
- Anyone who needs certainty sets `spec.parentBranch`.

The rules are then:

- **`Unborn`**: the repository is empty by the §4.2 definition, and only then.
- **Resolved**: after `List`, `HEAD` is symbolic and its target is an advertised branch. This
  covers go-git's heuristic.
- **Unresolved**: the repository is not empty, but `HEAD` is absent, symbolic to a missing branch,
  or still a bare hash with no matching branch.
  - Return a new sentinel, `ErrDefaultBranchUnresolved`.
  - Handle it exactly like `ErrParentBranchNotFound` on every path: fetch, publication, refresh
    and the §4.4 probe.
  - The observation is `ParentState=Missing`. `ParentBranch` is the dangling `HEAD` target (for
    example `master`), or empty when there is none.
  - No write is planned. The work is retained, or the dropped writes are remembered (§4.4).

#### Make an omitted parent able to stall

- Add `ParentRequested` to `RemoteObservation`: the spec value, `""` when omitted, taken from the
  §4.1 snapshot.
- `parentBranchReadiness` compares `ParentRequested` with `spec.parentBranch`, instead of the
  resolved name, so an omitted parent can stall too.
- Use the same reason, `ParentBranchNotFound`, so that recovery stays one mechanism. Only the
  message differs:
  - "the remote's default branch (`HEAD` → `master`) does not exist; fix the remote's HEAD or set
    `spec.parentBranch`";
  - or, with no `HEAD` at all: "the remote has no default branch".

Update the documentation in three places:

- the doc comment of `ParentUnborn` in `remote_observation.go`;
- the API types;
- the parent-branch contract table in `configuration.md`. That table should also record the
  go-git resolution decision.

**Tests**, all through the transport, against the real git server, not on synthetic ref lists:

1. **`HEAD` symbolic to a missing branch.** Seed a bare repository with
   `git symbolic-ref HEAD refs/heads/master` and only a `main` branch. Assert:
   - the init observation and the refresh observation are both `Missing` with branch `master`;
   - a write produces no orphan and stays retained;
   - readiness is `ParentBranchNotFound` with the `HEAD` message.
2. **Detached `HEAD`.** Use `git update-ref --no-deref HEAD <hash>` with two branches at that
   hash. Assert that the resolution is the one go-git chooses (`master` if present, otherwise
   the first alphabetically), and that the status names it. This pins the decision above.
3. **Recovery.** Repair the remote's `HEAD`, then recover through §4.4 with no new cluster edit.
4. **A truly empty remote** is still `Unborn`, and ledger row 1 is unchanged.

### 4.4 Recovery from a missing or unresolved parent (review item 6) → R3, R4

#### Problem today

- While `ParentBranchNotFound` holds, every reconcile sets `forceRecheck`
  (`gittarget_controller.go` ~189, ~275). A target that is not Ready requeues every 10s.
- Each forced recheck restarts all of the target's streams. That means:
  - a re-LIST of every watched type, which is the real cost;
  - a replay that spends one advertisement connection.
- A typo such as `parentBranch: relase` keeps this up indefinitely.

#### Why "force only on the transition, let the refresher probe" strands work

- The refresher skips a branch that is mid-cycle (`refresh.go` ~101): retained writes, an open
  window, a dirty worktree, or a required replay.
- A failed `pushPending` stops its push timer (~1673), and nothing re-arms it until the next
  event.
- With `--git-refresh-interval=0` there is no refresher at all.
- An observation flip is not an obligation:
  - If the first recovery attempt after `Found` fails transiently, the observation stays `Found`,
    the timer is stopped, a probe gated on `Missing` no longer runs, and the work is stranded.
  - A controller that keys off "the stored Ready reason was `ParentBranchNotFound`" can miss a
    whole missing→found episode that happens between two of its reconciles.

#### Design

1. **A recovery obligation on the worker.** Its lifetime is decoupled from observations.
   - The worker latches `parentRecovery` the moment a cycle fails with `ErrParentBranchNotFound`
     or `ErrDefaultBranchUnresolved`.
   - The latch has two independent halves:
     - **`retained`**: set when writes were kept. It clears only when `pushPending` actually
       publishes them, or settles them as `PushNoBranch`.
     - **`snapshotNeeded`, per scope**: the set of `(target, collection)` scopes whose writes the
       worker **dropped**, or whose resync it refused, because of the missing parent. One worker
       serves several targets, and resyncs run per collection, so a successful ConfigMap
       snapshot for target A proves nothing about a Deployment of A, or about target B. (The
       refusal recovery in `resync_flush.go` is scoped the same way, for the same reason.)
       Before implementing, verify which paths drop and which retain: `noteMissingParent` at
       ~2425 and its callers.
     - A scope clears only when a resync of that scope, started *after* the parent was found, is
       **published**: its commit pushed, or settled as a no-op. A resync reply is not enough,
       because a resync can succeed while its write still waits for the push.
   - Neither half clears on an observation, on a probe result, or on a failed attempt. A failed
     attempt re-arms the probe deadline (step 3).
2. **The worker drives recovery.** It runs on the event loop, while the latch is set, whatever the
   refresh configuration and whether or not writes are retained.
   - When the probe deadline fires, read one advertisement through `advertiseRemoteBranch`, with
     the §4.1 snapshot. This is a read, not a fetch.
   - If the parent is still missing or unresolved, record that and back off.
   - If the parent (or the write branch) is now present, record it. Then:
     - with `retained` set, call `pushPending`. It recovers through the normal replay: one fetch,
       a rebuild, a push. A failure leaves the latch set and re-arms the deadline;
     - for every target with an open scope, raise a **snapshot request**: a monotonically
       increasing `snapshotRequestSeq` per target, exposed alongside the observation. A scope
       that fails raises its target's sequence again on the next deadline.
3. **One probe deadline per worker, shared by every background probe.**
   - The worker keeps `nextRemoteProbeAt`, with backoff 10s, doubling, capped at 5m. It resets on
     a generation change (§4.1) and on success.
   - While the latch is set, a **refresher tick** that arrives before the deadline is a no-op: no
     connection. A tick after it counts as the probe and advances the deadline.
   - **Reconciles never probe.** That holds however many targets share the worker.
   - Live events, while the latch is set and before the deadline, are retained or dropped as
     today, **without** a fetch. The deadline governs them too.
4. **The controller acts on the request sequence, not on the Ready reason.**
   - It remembers, in memory per target (like `reconcileRequests.take`), the last
     `snapshotRequestSeq` it acted on.
   - When the worker's sequence is higher, it forces **one** recheck and records the sequence. A
     missing→found episode between two reconciles is therefore never lost.
   - After a controller restart the memory is empty. That is harmless: every stream starts fresh
     with an initial list, which is itself the snapshot.
   - Drop the per-reconcile `parentWasMissing` force.
   - Ready reporting:
     - `ParentBranchNotFound` stays Ready=False/Stalled while the parent is missing.
     - While the obligation is open but the parent has been found, report Ready=False with
       `Reconciling=True`, reason `RecoveringParentBranch`. This keeps the not-Ready requeue
       cadence, so the controller sees the sequence within one requeue, and the state stays
       visible. Verify that the not-Ready requeue applies to that reason.
     - The 10s requeues read local observations only, so they cost zero connections.

#### Tests

Write the recovery tests first, against current code. They are the safety net for the design.

1. **Worker, retained writes, real server.** The parent is missing while writes are retained.
   Then:
   - the parent is pushed externally;
   - the **first recovery attempt fails** (inject a GitProvider read failure, as the reproduction
     for `dcbc8b82` did);
   - the next deadline recovers and publishes, **without another cluster edit**.

   Run it with refresh enabled and with refresh at `0`.
2. **Worker, dropped writes, two scopes.** Two targets share the worker; writes for both are
   dropped while the parent is missing. When it appears, one scope's resync succeeds and the
   other's fails once. Assert that only the published scope clears, that the failing target's
   sequence rises again, and that it clears after a later success. A resync that succeeded but
   has not been pushed yet must not clear its scope.
3. **Controller unit.** A sequence bump forces exactly one recheck. A bump that happens entirely
   between two reconciles is still acted on. Ten reconciles at an unchanged sequence force
   nothing.
4. **Probe budget.** Take three targets sharing one worker, a parent that stays missing, refresh
   ticks every few seconds, and repeated reconciles, over a simulated window of N backoff
   periods. Assert that the advertisement connections stay ≤ the backoff schedule's count, and
   that there are zero fetches.

   This needs an injectable clock on the probe deadline. Add a small `now func() time.Time` field
   rather than sleeping.
5. **The §4.3 cases.** An unresolved `HEAD`, once repaired, recovers through the same path.
6. **e2e: dropped-write integration.** There is no controller+watch+worker harness outside e2e,
   and building one is out of scope; e2e is that integration test. Add a third context to
   `new_branch_parent_e2e_test.go`:
   - set `parentBranch: release` while `release` is absent;
   - assert `ParentBranchNotFound` and that no write branch exists;
   - push `release` from outside;
   - assert Ready, and the content on a new branch whose first commit's parent is `release`'s
     tip;
   - make no cluster edit after the push.

   It also covers recovery from `ParentBranchNotFound`, which no test exercises today.
7. **Ledger rows.** Add "parent probe while missing" = 1 connection, 0 fetches. Row-level counts
   do not prove frequency; test 4 does.

#### Fallback, if step 2 or 3 grows too large

- Keep step 1 (the obligation) and step 4 (the request sequence). The obligation is the
  correctness part and is non-negotiable.
- Replace the worker probe with a controller-side forced recheck under exponential backoff, still
  gated on the latch rather than on the Ready reason.
- **The schedule is per worker, not per target**, so the per-worker budget in §2 still holds: key
  `lastForcedAt` by the worker's `(provider, branch)` and force all of that worker's affected
  targets in one round. Per-target backoff would give three targets on one worker three
  independent retry streams.
- Tests 1–3 and 6 still apply. Test 4 then asserts the controller-side schedule across three
  targets on one worker.
- Record the worker probe as deferred work in the PR body.

### 4.5 Vocabulary (review item 5)

Keep `status.remote.parent`. Reword the new rule in `docs/definitions.md` (~132):

> "parent branch"; a bare "parent" is fine where the subject is already a branch, such as the
> `status.remote.parent` stanza or a sentence that has just named the parent branch. Use the full
> term wherever a parent *resource* could be meant.

Then sweep `configuration.md`, `architecture.md` and the Go doc comments, but only for genuinely
ambiguous uses.

### 4.6 e2e: no dependence on refresh timing (review item 7; CodeRabbit comment on `:238`)

The last step, "the publication check, not the refresher, is what found B", asserts both that
`contention` rose and that `refresh` did not change.

- With `--git-refresh-interval=30s`, a refresh can find B first. `contention` then legitimately
  never rises.
- Relaxing only the `refresh` equality does not fix this.

#### The fix

1. Delete that step, and keep the ancestry and content assertions.
2. Add a comment pointing at the deterministic worker tests that prove publication alone finds B,
   with no refresher:
   - `TestBranchWorker_LiveWriteOnAbsentBranchStartsFromTheParentsCurrentTip`;
   - `TestBranchWorker_NewBranchIsNotPublishedOnAnUncheckedParent`.
3. Reply on the CodeRabbit thread with that rationale.

### 4.7 Default-branch switches, and the refused upload (review item 8) → R5, R6

#### R5: make the decision in §3 true, and stop the status flapping

- Today `followParent` (`refresh.go` ~276) compares hashes only. Make it also compare
  `advertisement.parentBranch` with the branch the checkout was rooted on: `newBranchParent`, or
  the branch the last sync resolved.
- On a mismatch, call `syncWithRemote(fetchReasonRefresh)`. That is one fetch, on a rare event.
- This also ends the flapping of `status.remote.parent.branch`, where no-op pushes record `main`
  while refreshes record `trunk`.
- **Tests.**
  - Switch `HEAD` from `main` to `trunk`, both at the same commit. Assert that a refresh resyncs
    once, and that the next no-op push records `trunk`.
  - With refresh at `0`, assert that the worker keeps `main` until the next discovery. That is the
    documented behavior.
- **Docs.** Document the omitted-parent semantics in `configuration.md`.

#### R6 needs a production fix, not only a test

How it fails today:

- `feature` is created by somebody else after our advertisement, and the parent `main` is
  unmoved.
- Our upload with `Old=zero` is refused by the server, as an untyped error.
- `remoteMovedDuringPush` (~2084) finds no typed rejection. It falls back to
  `fetchRemoteBranchHashFn` for the **root** branch, `main` (~2100).
- That probe sees `main` unmoved and concludes "not moved". There is no replay, and the work is
  stuck.

The fix:

- Only on this exceptional path: when the cycle creates the branch (`rootBranch` is not the write
  branch) and the failure is untyped, make the fallback discovery read **both** refs from one
  advertisement.
- If the write branch is now advertised, treat it as contention, as the typed write-branch case
  already does.
- Do not add any discovery to the normal path.

**Tests** (real server; add a test-only seam between `validatePushState` and `performPush`):

- Create `feature` from outside inside that seam.
- Assert that the server refuses the upload.
- Assert that the worker replays onto their commit, and that their commit is an ancestor of ours.

**Ledger.** Add R6 as its own measured row. It includes a refused upload plus the fallback
discovery, so it will not equal row 6. Record whatever it measures, check that the number is
explained by the steps above, and do not tune toward row 6.

## 5. Finish

1. Regenerate the ledger golden file (`-update`) and **diff it by hand**.
   - Rows 1–12 must be byte-identical.
   - Only the new rows may appear: H3, H5, the parent probe, and R6.
2. Run `task lint` and `task test`, then commit and push per item (§0).
3. Update the #407 PR body:
   - one "Review fixes" entry per item;
   - the cost table from §2;
   - the e2e contract and the go-git resolution decision from §3 and §4.3.
4. Read the CodeRabbit inline comments again after each push.
5. Run `task test-e2e`, and the `full-manager` leg in CI. §4.4's new e2e context must pass at least
   once before calling the PR ready.
6. Fold what shipped into [`gittarget-parent-branch.md`](gittarget-parent-branch.md) (the
   built contract) and `architecture.md`, and mark this plan done.

## 6. Out of scope

- The two deferred follow-ups named in §0.
- Resetting, force-pushing or deleting a surviving write branch, and PR management. Ancestry
  status or folder equality is not enough authorization for those mutations.
- Any fetch before publication on a trusted checkout.

## 7. Design choices the review rounds settled

Kept here so they are not re-litigated. Each was checked against the code.

- **No pull before push.** A pull cannot close the race; the compare-and-swap plus bounded replay
  does. Every fix in this pass reads evidence the push or discovery already holds.
- **Recovery is an obligation, not an observation.** It stays open until the work is published,
  so a transient failure after the parent reappears cannot strand it (§4.4). It is tracked per
  `(target, collection)` scope.
- **A configuration change is applied at a push admission boundary**, with one `{name, gen}`
  snapshot per operation (§4.1). A push already admitted is not recalled.
- **One probe budget per worker**, whichever variant of §4.4 ships.
- **An empty repository has no hash refs**, tags included, and gaining one is reported with its own
  typed error, not a `RemoteMovedError` that would misstate its fields (§4.2).
- **Default-branch resolution follows go-git**, which rewrites a hash-only `HEAD` before `List`
  returns; a stricter rule would need a bespoke advertisement path for a rare server setup (§4.3).
- **The refused upload (R6) needed a production fix**: the fallback probe looked at the root only
  (§4.7). It gets its own ledger row, because a refused upload costs more than a refusal at the
  advertisement.
- **The dropped-write integration test is e2e.** No controller, watch and worker harness exists
  outside it, and building one would be larger than the bug.
