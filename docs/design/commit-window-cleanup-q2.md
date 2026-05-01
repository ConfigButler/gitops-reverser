# Commit Window Cleanup Q2: Unify Atomic And Per-Event Through One Window

> Status: proposed
> Date: 2026-05-01
> Scope: this plan only. Builds on [cleanup-q1](/workspaces/gitops-reverser/docs/design/commit-window-cleanup-q1.md). Once this lands, [commit-window-refactor.md](/workspaces/gitops-reverser/docs/design/commit-window-refactor.md) will need a follow-up update to remove the two-pending-write-kinds story.

## Where I Was Wrong

In the earlier Q2 analysis I argued that unifying atomic and per-event would force authorship and message-kind onto the `Event` type — "moving intent from the request level to the substrate". That was the right concern about a different proposal than the one on the table. The version of unification that actually works moves authorship out of the producer's hands entirely and lets the git layer decide.

The byte-cap framing — *"finishing off a group that is being created is something different than actively splitting an existing group"* — is the part I had no good answer to. It dissolves the byte-cap objection.

The architectural principle: **producers describe *what* changed and *why*; the git layer decides *how* it gets attributed.** This drops several layers of complexity at once: no `CommitMode`, no `PendingWriteKind`, no `WriteRequest.Author`, no producer-fabricated `UserInfo`, no two-shaped pending-write storage.

So: I think your proposal works. The rest of this doc is the concrete model.

## The Unified Model

```
WriteRequest { Events, CommitMessage?, GitTarget }
       │
       ▼
    Branch worker streams events into the open window
       │   invariant: (event author, target, has-CommitMessage)
       ▼
    Finalize triggers (any of):
       - per-event author change
       - target change
       - request boundary AND request had CommitMessage set  ← new trigger
       - commitWindow silence
       - shutdown
       - byte cap (organic windows only — see below)
       - commitWindow=0
       │
       ▼
    PendingWrite (one shape, no Kind tag)
       │
       ▼
    pendingWrites queue → push on cooldown
```

There is **one** `PendingWrite` shape. There is **one** open-window machinery. There is **one** push path.

The two flavors of incoming work are encoded as *the presence or absence of `CommitMessage` on the request*, not as separate types and not as separate code paths.

### Field semantics

`WriteRequest.CommitMessage string`
- If set: the request is a caller-declared batch. The window finalizes at the request boundary and renders this message verbatim (no template).
- If empty: the request is organic; the window finalizes on the usual triggers and renders the per-event or grouped template.

`WriteRequest.GitTargetName / GitTargetNamespace`
- Stays as-is. Used to back-fill events that lack target identity (today's atomic-pre-resolution behavior).

`WriteRequest.CommitMode` is **deleted**. The dispatch is now implicit in `CommitMessage` presence.

`WriteRequest.Author` is **not added**. Authorship is determined entirely by `event.UserInfo.Username` (set by the audit pipeline) or, when empty, by the git-layer fall-through to committer-as-author. Producers never assert authorship at the request level.

### Author and committer in git terms

Every git commit has two identity fields, both populated, both required by the format:

- **`Committer`** — who physically created the commit object. In our world, this is **always** the operator (`config.Committer.{Name,Email}`). Implemented in [commit.go:140-146](/workspaces/gitops-reverser/internal/git/commit.go#L140-L146) as `operatorSignature`. This does not change under unification.
- **`Author`** — who logically wrote the change. In standard `git commit`, if you don't pass `--author`, git copies the committer identity into the author field (the commit object always has both fields populated; tools that show "X committed" simply collapse the display when they match).

In go-git, `CommitOptions.Author` is `*object.Signature` ([commit.go:150-154](/workspaces/gitops-reverser/internal/git/commit.go#L150-L154)). Today the codebase has three signature builders:

| Path | Author Name | Author Email | Committer |
|---|---|---|---|
| `commitOptionsForEvent` | `event.UserInfo.Username` | `ConstructSafeEmail(username, "cluster.local")` | operator |
| `commitOptionsForGroup` | first event UserInfo | `ConstructSafeEmail(...)` | operator |
| `commitOptionsForBatch` | operator (same as committer) | operator email | operator |

Under unification, all three collapse into one builder driven by `pendingWrite.author()`:

```go
func commitOptionsFor(pw PendingWrite, config CommitConfig, signer git.Signer, when time.Time) *git.CommitOptions {
    committer := operatorSignature(config, when)
    author := pw.author()
    if author == "" {
        // No logical author. Match standard git's no-`--author` behavior:
        // populate Author with the committer identity. Tools display just
        // "<committer> committed" when the two match.
        return &git.CommitOptions{
            Author:    committer,
            Committer: committer,
            Signer:    signer,
        }
    }
    return &git.CommitOptions{
        Author: &object.Signature{
            Name:  author,
            Email: ConstructSafeEmail(author, "cluster.local"),
            When:  when,
        },
        Committer: committer,
        Signer:    signer,
    }
}
```

Where `pw.author()` is just:

```go
func (p PendingWrite) author() string {
    if len(p.Events) == 0 {
        return ""
    }
    return p.Events[0].UserInfo.Username
}
```

The window invariant guarantees all events in a `PendingWrite` share the same `UserInfo.Username`, so `events[0]` is the canonical author for the whole pending write.

**Behavior preservation check:**

| Today's case | Today's signature | Under unification | Same result? |
|---|---|---|---|
| Audit event with `UserInfo.Username = "alice"` | Author=alice, Committer=operator | events all carry UserInfo=alice → Author=alice, Committer=operator | ✅ |
| Grouped audit window (alice, 3 events) | Author=alice, Committer=operator | same | ✅ |
| Reconcile snapshot (today fabricates `UserInfo="gitops-reverser"`, atomic path discards it) | Author=operator, Committer=operator | producer stops fabricating; events have empty UserInfo → Author=committer=operator | ✅ (same commit shape, plus a producer-side cleanup — see *Pushbacks Worth Keeping → 1*) |

The reconcile-snapshot row produces the same git commit as today and removes a leak: the reconciler stops asserting an identity at all. The git-level fall-through is implemented by explicitly assigning the committer signature to `Author` (the same trick today's `commitOptionsForBatch` already uses). We never pass a `nil` Author or a Signature with empty Name to go-git — those would give unpredictable output.

**Why `string` (empty = fall through), not `*string`:** standard convention for empty-CommitMessage already; no third state needed; no benefit from nil-checking. The same applies to event `UserInfo.Username`: empty means "no logical author", same as today.

### MessageKind derivation

```go
func (p PendingWrite) MessageKind() CommitMessageKind {
    if p.CommitMessage != "" {
        return CommitMessageBatch     // caller-declared
    }
    if len(p.Events) == 1 {
        return CommitMessagePerEvent   // single event organic
    }
    return CommitMessageGrouped        // multi event organic
}
```

The "Atomic" case from cleanup-q1's three-line method is replaced by "CommitMessage non-empty". The `PendingWriteKind` enum is gone.

### Finalize triggers (new list)

1. Per-event author of incoming event ≠ open window's author.
2. Target of incoming event ≠ open window's target.
3. **Incoming request has `CommitMessage` set.** Force-finalize before opening, then force-finalize after appending the request's events.
4. `commitWindow` silence timer fires.
5. Byte cap is exceeded **and** the open window is organic (see byte-cap section).
6. `commitWindow=0` and the open window has any events.
7. Shutdown.

Trigger 3 is the only behaviorally new addition. Triggers 4–7 are unchanged.

## The Six Divergences, Revisited Under The Unified Model

| Concern | How the unified model addresses it |
|---|---|
| **Authorship source** | Always `event.UserInfo.Username`. Empty falls through to committer-as-author at the git layer. See *Author and committer in git terms*. |
| **Commit message source** | `WriteRequest.CommitMessage` non-empty → Batch. Empty → templates. Same source as today (the request), just used as the dispatch signal too. |
| **Finalize timing** | Request-with-CommitMessage adds finalize trigger #3. Same machinery, one more case. |
| **Byte-cap behavior** | "Don't actively split a group; do finalize an organic window early." See next section. |
| **Same-author coalescing across requests** | Trigger #3 force-finalizes batch-intent requests, so two consecutive batch requests don't merge. Two consecutive per-event requests from same (author, target) **do** merge — same as today. |
| **Target identity source** | Unchanged. Producers still populate events from the request's `GitTargetName/Namespace` when needed. |

## The Byte-Cap Rule

Your insight, restated and made explicit:

> Finishing off a group that is being created is something different than actively splitting an existing group.

Concretely:

- **Organic window**: still being grown event-by-event by streaming arrivals. If the cap is exceeded, finalize the current window early. The cap is a memory-pressure backstop. This is today's behavior for per-event.
- **Caller-declared batch**: arrives as a complete `WriteRequest` with `CommitMessage` set. The caller has already declared the boundary. The branch worker does not split it across multiple `PendingWrite`s, even if processing it would push retained bytes past the cap.

Implementation: process the entire request's events into the window first, then check the cap. The check is a "should I finalize the *next* organic window early" question, not a "should I split this request" question.

Subtle consequence I want to flag: **a multi-event per-event request that would exceed the cap mid-stream is no longer split**. Today, a 1000-event per-event request that trips the cap halfway through would split into multiple commits. Under the unified rule it would land as one window finalized at request end. I think this is an acceptable trade — per-event requests with thousands of events are pathological — but it is a behavior change worth calling out in the change log.

## Pushbacks Worth Keeping

These are the things I'd still want explicit decisions on, not arguments against the proposal.

### 1. The reconciler must not fabricate authorship

Three sites in [folder_reconciler.go](/workspaces/gitops-reverser/internal/reconcile/folder_reconciler.go) (lines 178, 186, 196) currently set `UserInfo: git.UserInfo{Username: "gitops-reverser"}` on each event. This is an architectural leak: the reconciler is asserting an identity it has no business asserting. Authorship attribution is a git-layer concern, not a reconcile-layer concern.

The principle (yours, restated): producers describe *what* and *why*; the git layer decides *how* it gets attributed.

Under the unified model, this maps cleanly onto the producer surface:

- The reconciler **describes the change**: it produces events with the resource data.
- The reconciler **declares intent**: it sets `WriteRequest.CommitMessage` so the git layer knows this is a caller-declared batch.
- The reconciler **does not** fabricate `UserInfo`. It has no opinion on attribution.
- The git layer applies the policy: "no logical author specified → fall through to committer-as-author" (the standard git CLI semantics already covered above).

Concrete migration for [folder_reconciler.go](/workspaces/gitops-reverser/internal/reconcile/folder_reconciler.go):

- Delete the three `UserInfo: git.UserInfo{Username: "gitops-reverser"}` assignments.
- Set `WriteRequest.CommitMessage = "<reconcile message>"` to declare the batch.

Resulting commit shape: Author = Committer = operator config. **Identical to today's atomic shape**, achieved without the reconciler knowing about authorship at all.

Audit producers ([git_target_event_stream.go](/workspaces/gitops-reverser/internal/reconcile/git_target_event_stream.go)) are unaffected: they carry real k8s audit identity through `event.UserInfo` because that *is* the logical author. They don't fabricate, so they don't change.

### 2. Why we are *not* adding `WriteRequest.Author`

I considered (and an earlier draft of this doc proposed) adding a `WriteRequest.Author` field as a request-level override. After the principle in #1, that field becomes exactly the kind of handle that lets producers violate the principle. We delete it before it exists.

What we lose: the ability for a request to override per-event authorship. Use cases:

- Reconcile claiming a specific author? The principle says no.
- A future producer wanting to override audit identity? Speculative.
- Tests asserting authorship? They populate event UserInfo directly.

If a future producer ever genuinely needs request-level override, add the field then. This makes the principle a hard constraint at the type level rather than a convention.

### 3. External API change

`WriteRequest.CommitMode` is in `internal/git/types.go` (not a CRD field). Two producer call sites:

- [git_target_event_stream.go:138](/workspaces/gitops-reverser/internal/reconcile/git_target_event_stream.go#L138) — `request.CommitMode = git.CommitModeAtomic`. Migrates to setting `request.CommitMessage = "<message>"` and dropping the CommitMode line.
- [folder_reconciler.go:200](/workspaces/gitops-reverser/internal/reconcile/folder_reconciler.go#L200) — currently doesn't set CommitMode (so defaults to per-event today, even though events are pre-shaped). Under the unified model, set `CommitMessage = "<reconcile message>"` to make it a batch. **This call site is interesting**: today's behavior here may already be wrong (reconcile diff events going through per-event window when they're conceptually a batch). Worth a separate look — the unification fixes it incidentally if the producer adopts a `CommitMessage`.

`internal/` types only, no external clients of this package, so we can do a clean rename without a deprecation window.

### 4. The new finalize trigger needs to compose correctly with the open window invariant

When a `WriteRequest` arrives with `CommitMessage` set:

1. If an organic window is open, finalize it (the new request has different `has-CommitMessage`, so the invariant differs).
2. Open a fresh window for the request's events.
3. Append all events of the request to that window.
4. **Force-finalize** at request boundary, regardless of the timer.

Step 4 is what makes this a batch. The window stays open for exactly the lifetime of one request and then closes.

What if step 3 hits the byte cap mid-request? See the byte-cap rule above: don't split. Land it as one over-cap commit.

What about the per-event author invariant inside a batch? In practice, a single `WriteRequest` carries events with uniform `UserInfo`: audit batches share one user, reconcile batches share an empty UserInfo. The invariant holds inside a batch by producer convention. If a future producer ever ships a batch with mixed-author events, we have to choose between "split inside the batch" (violates batch semantics) or "ignore the author invariant for batches" (special-cases the window). We avoid that decision now by not building speculative paths around it. If it ever happens, the producer is doing something architecturally wrong and that's where the fix belongs.

## Tests And Migration

### Tests to add

- **MessageKind derivation** with `CommitMessage` empty vs set, single event vs multi event (replaces today's `PendingWriteKind`-based test).
- **Audit author flows through**: a request with `CommitMessage=""` and events whose `UserInfo.Username="alice"` produces a commit with `Author=alice`.
- **Empty UserInfo falls through to committer**: a request with `CommitMessage` set and events whose `UserInfo.Username=""` produces a commit where the git Author and Committer signatures are both the operator. Asserts the no-author-info path matches today's atomic shape exactly.
- **Committer is always operator**: across all of the above, `commit.Committer.{Name,Email}` equals `config.Committer.{Name,Email}` for every produced commit.
- **Force-finalize at request boundary**: a request with `CommitMessage` set finalizes immediately, even if the commit-window timer hasn't fired and there is no author/target change.
- **Organic + batch interleave**: a per-event request that opens a window, then a batch request that arrives, finalizes the organic window first and then commits the batch as one, in arrival order.
- **Two consecutive batch requests** from same author/target/message do **not** merge — each finalizes separately.
- **Byte cap with batch request**: a batch request that would exceed the cap lands as one commit; subsequent organic windows finalize early.
- **Byte cap with per-event request**: a multi-event organic request that exceeds the cap mid-stream lands as one commit (behavior change).

### Tests to remove or rewrite

- Anything asserting on `PendingWriteKind == PendingWriteAtomic`. Replace with assertions on `MessageKind() == CommitMessageBatch`.
- The `CommitModeAtomic` / `CommitModePerEvent` switch in producer-side tests becomes `CommitMessage` set/unset.
- The atomic-specific event-loop test in [branch_worker_split_test.go:841](/workspaces/gitops-reverser/internal/git/branch_worker_split_test.go#L841) (`TestEventLoop_AtomicRequest_RespectsCooldownAndUsesNormalPushPath`) becomes a "batch request respects cooldown" test with the same assertions — same behavior, new vocabulary.
- Reconcile-producer tests that assert on the fabricated `UserInfo="gitops-reverser"` should be rewritten to assert that the git commit has Author=Committer=operator (the externally observable behavior we actually care about).

### Migration order (each step keeps the tree green)

1. **Update both producers**:
   - Reconcile ([folder_reconciler.go](/workspaces/gitops-reverser/internal/reconcile/folder_reconciler.go) lines 178/186/196 + the `WriteRequest` site at 200): **delete** the `UserInfo: git.UserInfo{Username: "gitops-reverser"}` assignments. Set `WriteRequest.CommitMessage = "<reconcile message>"` to declare batch intent.
   - Audit ([git_target_event_stream.go](/workspaces/gitops-reverser/internal/reconcile/git_target_event_stream.go)): unchanged. It already carries real k8s audit identity through `event.UserInfo`.
   - At this step the reconcile path still goes through today's atomic dispatch via `CommitMode`; behavior is unchanged because the atomic path discards UserInfo anyway.
2. **Switch `handleQueueItem` dispatch** from `CommitMode == Atomic` to `CommitMessage != ""`. Behavior is identical because every atomic producer also sets CommitMessage today (and after step 1, the reconcile producer does too).
3. **Replace the atomic code path with the open-window flow**: incoming request with `CommitMessage` set finalizes any in-flight organic window, opens a new window for the request's events, force-finalizes at request boundary.
4. **Delete `buildAtomicPendingWrite`**. The grouped path now handles both flavors via `CommitMessage`.
5. **Collapse the three signature builders.** Replace `commitOptionsForEvent`, `commitOptionsForGroup`, and `commitOptionsForBatch` with one `commitOptionsFor(pw, config, signer, when)` driven by `pw.author()`. Empty author assigns the committer signature to `Author`, exactly matching today's atomic shape.
6. **Delete `PendingWriteKind`, `PendingWriteCommit`, `PendingWriteAtomic`** from `types.go`. `PendingWrite` becomes one shape.
7. **Delete `CommitMode`, `CommitModePerEvent`, `CommitModeAtomic`** and the unused `WriteRequest.CommitMode` field.
8. **Update the byte-cap rule**: process all events of a request before checking the cap; finalize the *next* organic window early if needed; never split a single request across `PendingWrite`s.

Each step is one PR-shaped commit.

## Acceptance Criteria

- `grep -rn 'PendingWriteKind\|PendingWriteAtomic\|PendingWriteCommit\|CommitMode\|buildAtomicPendingWrite' internal/` returns zero matches.
- `WriteRequest` has `CommitMessage` (already exists) and no `CommitMode` and no `Author`.
- One `PendingWrite` type with one `MessageKind()` derivation based on `CommitMessage` presence and event count.
- One `commitOptionsFor` builder; the three former builders are gone.
- The open-window event loop is the single code path; `handleQueueItem` no longer branches on commit mode.
- The reconcile producer no longer fabricates `UserInfo`; it only sets `CommitMessage`. The audit producer is unchanged.
- `grep -rn 'Username: "gitops-reverser"' internal/` returns zero matches.
- A new test asserts that a `CommitMessage`-bearing request does not merge with an in-flight organic window.
- A new test asserts the byte-cap rule (don't split a request).
- A new test asserts that a request with `CommitMessage` set and empty event `UserInfo` produces a commit where Author = Committer = operator.

## Out Of Scope

- Multi-target atomic. Today's atomic is single-target by request shape; this plan keeps that. If multi-target atomic is ever needed, it's a separate design.
- Anything CRD-facing. `commitWindow`, `branch-buffer-max-bytes`, push cooldown — all unchanged.
- Encryption, target resolution, message templating logic — unchanged.
- Any change to the audit consumer's request batching strategy. Producers may want to revisit how they form requests, but that's not required for this plan.

## Doc Follow-Up

Once this lands, [commit-window-refactor.md](/workspaces/gitops-reverser/docs/design/commit-window-refactor.md) needs:

- Remove the `PendingWriteCommit` / `PendingWriteAtomic` distinction from "Core Types".
- Update the flow diagram so atomic is not a separate branch — it's a finalize trigger on the same window.
- Add the new finalize trigger ("request with `CommitMessage` set") to the rules list.
- Replace the `CommitModePerEvent` / `CommitModeAtomic` doc with the `CommitMessage` field semantics and the producer/git-layer responsibility split.
- Add the byte-cap rule ("don't actively split a request") to operational controls.
- Reframe the "Honest authorship" use case: the rule is the same (operator-attributed commits for reconcile snapshots, event-user-attributed for audit), but it's implemented via the git-layer fall-through rather than a `Kind` tag or a request-level override.
