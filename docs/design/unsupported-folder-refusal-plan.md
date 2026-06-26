# Refuse unsupported folder content — implementation plan

> Status: PROPOSAL — 2026-06-26. **Decision recorded** in
> [e2e-coverage-gaps-and-improvements-plan.md §4.1](e2e-coverage-gaps-and-improvements-plan.md): the
> operator must **refuse** a GitTarget folder it cannot safely manage, for the cases where we already
> know the content is a problem — not silently keep writing. This doc is the grounded implementation
> design, written against the **actual** current code (not the superseded `Synced`/`materialization`
> model in [status-design-git-target.md](status-design-git-target.md)).

## 1. The problem, precisely

The acceptance gate ([manifestanalyzer/acceptance.go](../../internal/manifestanalyzer/acceptance.go))
already classifies a folder's content and produces blocking refusals — but it is wired **only into the
`manifest-analyzer` CLI**, never the running controller. The live writer builds its store with an empty
allowlist and **never calls `Accept`**:

- [plan_flush.go:95](../../internal/git/plan_flush.go#L95) (live) — `BuildStoreFromFiles(..., Allowlist{})`,
  comment at :94 says the gate is "applied upstream, not here" — but no upstream caller applies it.
- [resync_flush.go](../../internal/git/resync_flush.go) (first-materialization / mark-and-sweep) — builds
  a plan directly, no acceptance.

So today a folder with duplicate identities, impure managed files, non-KRM passengers, or a
hard-Kustomize `kustomization.yaml` is **detected but not refused**: the operator writes anyway. We will
change that.

## 2. What "the cases where we know it's a problem" means (scope)

Two tiers. We implement **both**, because the second is the user's literal example.

**Tier 1 — structure-only refusals (already fully implemented in `Accept`).** These need no API source,
no registry, no scope predicate — they are unambiguous, purely structural facts about the folder:

| Refusal | IssueKind | Source |
|---|---|---|
| Duplicate manifest identity | `IssueDuplicate` | [acceptance.go:180](../../internal/manifestanalyzer/acceptance.go#L180) |
| Impure managed file (managed file with an empty / non-KRM / invalid passenger) | `IssueImpureManagedFile` | [acceptance.go:207](../../internal/manifestanalyzer/acceptance.go#L207) |
| Standalone non-KRM / invalid YAML | `IssueNonKRM` / `IssueInvalidYAML` | [acceptance.go:228](../../internal/manifestanalyzer/acceptance.go#L228) |
| Managed resource hiding in an allowlisted `kustomization.yaml` | `IssueMixedFile` | [acceptance.go:268](../../internal/manifestanalyzer/acceptance.go#L268) |

`Accept` already runs all of these with no API source ([acceptance.go:165](../../internal/manifestanalyzer/acceptance.go#L165)
gates the *mapping-aware* refusals behind `hasAPISource`, so a structure-only store skips them cleanly).

**Tier 2 — hard-Kustomize refusal (NEW, the named example).** A `kustomization.yaml` that uses
generators / patches / components / helmCharts / replacements / transformers / namePrefix|Suffix /
remote bases is **detected** today (`kustomizationDoc.unsupported` via
[store.go:687](../../internal/manifestanalyzer/store.go#L687) `hasUnsupportedKustomizeFeature`) but only
used to disqualify it as a namespace source — it is **not** a refusal. We add a new acceptance issue
that refuses it, because the operator cannot map such a folder back to editable source documents.

**Out of scope (deliberately):** the mapping-aware refusals (`IssueUnresolvedKRM`, `IssueOutOfScope`)
need a live followability registry + a namespace `InScope` predicate. They are real, but they are *not*
"cases where we already know it's a problem" with structure alone — they depend on cluster discovery and
can blink on a discovery wobble (see [typeset-owns-discovery-grace.md](typeset-owns-discovery-grace.md)).
Refusing on those risks false refusals on a transient. **Defer to a follow-up.**

## 3. The surface: refusal is a Blocked stream

The current data-plane status (NOT the superseded `Synced`/`materialization` doc) is:

- Condition `StreamsReady` ([gittarget_types.go:157](../../api/v1alpha2/gittarget_types.go#L157) printcolumn)
  + `status.streams` roll-up `{Total, Ready, Replaying, Blocked, summary}`
  ([gittarget_types.go:130](../../api/v1alpha2/gittarget_types.go#L130)).
- Driven by [watch.StreamSummary](../../internal/watch/stream_readiness.go#L73), whose per-type state is
  one of `Streaming` / `Replaying` / **`Blocked`** ([stream_readiness.go:34](../../internal/watch/stream_readiness.go#L34)),
  each carrying a `reason` + `message`.
- The controller reads it via `StreamSummaryForGitTarget(gitDest)`
  ([gittarget_controller.go:209](../../internal/controller/gittarget_controller.go#L209)) and projects it
  into the `StreamsReady` condition + phase.

A folder we refuse is exactly a type that **cannot currently be materialized** — which is what
`StreamStateBlocked` means ("the watch cannot currently run"). So we surface a refusal by marking the
type's stream **Blocked** with a new reason `UnsupportedContent` and a message naming the offending file.
This flips `StreamsReady=False`, increments `status.streams.blocked`, and puts the file name in the
condition message — a real, user-visible refusal, reusing the surface that already exists. No new API
field, no reviving the unwired Materializer.

> Note on overloading "Blocked": stream state was conceived as "can we open the watch." A write-target
> refusal is a different axis, but from the user's view "this type isn't being mirrored because the
> folder is unsupported" is the same outcome, and the explicit reason string keeps it unambiguous. If we
> later want a dedicated `Writable`/acceptance condition (the §2.1 idea in
> [status-design-git-target.md](status-design-git-target.md#21-eventstreamlive-is-not-a-real-condition--fold-it-into-ready)),
> this refusal is the first concrete driver for it. For now, Blocked is the pragmatic, correct-enough fit.

## 4. The seam: refuse at first-materialization resync

The acceptance gate is a **whole-folder, first-materialization** check (it is the M4 "adoption gate").
The natural moment is the resync / mark-and-sweep apply, which scans the entire GitTarget subtree:

- `applyResync` ([resync_flush.go:102](../../internal/git/resync_flush.go#L102)) already builds the plan,
  and on a build/commit error replies `ResyncResult{Err: ...}` and **commits nothing**
  ([resync_flush.go:120-131](../../internal/git/resync_flush.go#L120)). This is the abort path we reuse.
- The error flows back to `drainScopedResync`
  ([event_router.go:227](../../internal/watch/event_router.go#L227)), the one place that already handles a
  resync outcome — the seam where we translate a refusal into a Blocked stream.

We also guard the **live** path ([flushEventsToWorktree](../../internal/git/plan_flush.go#L52)) with the
same check, so that if a refusal somehow races a live event, the live flush refuses too rather than
writing into an unsafe folder. Both share one helper.

### Data flow

```
watch replay → enqueueScopedResync → BranchWorker.applyResync
   └─ scan subtree → build store (DefaultAllowlist) → Accept(structure-only + hard-kustomize)
        ├─ accepted  → commit as today
        └─ REFUSED   → reply ResyncResult{Err: *AcceptanceRefusedError{Issues}}  (commit nothing)
                          └─ drainScopedResync sees the typed error
                               └─ markTargetStreamState(gitDest, key, Blocked,
                                      "UnsupportedContent", "<file>: <why>")
                                    └─ StreamSummary.Blocked++ ; StreamsReady=False
                                         └─ controller projects → StreamsReady condition + message
```

## 5. Implementation phases

Each phase is independently compilable and unit-testable; validate per phase before moving on.

### Phase 1 — manifestanalyzer: structure-only + hard-Kustomize gate

- Add a typed refusal error usable by the writer: `AcceptanceRefusedError` (wraps `[]AcceptanceIssue`,
  `Error()` names the first offending file + count). Lives in `manifestanalyzer`.
- Add a convenience entrypoint the writer can call on an already-built store, e.g.
  `AcceptStructureOnly(store) Acceptance` (or reuse `Accept(store, AcceptancePolicy{})` — confirm the
  zero policy already yields the structure-only set; from [acceptance.go:160](../../internal/manifestanalyzer/acceptance.go#L160)
  it does, since `hasAPISource` is false for a writer store built without followability mappings — verify
  during implementation, the live store *does* carry a `mapper`, so it may have an API source; if so, add
  an explicit structure-only entrypoint that skips `mappingRefusals`).
- **Tier 2:** surface `kustomizationDoc.unsupported` to the store's public model and add a new
  `IssueUnsupportedKustomize` refusal in `Accept` for any retained kustomization marked unsupported.
  (Requires threading the `unsupported` flag from `parseKustomizations`/the namespace pass into
  `store.Retained` or a parallel list the gate can read.)
- Unit tests: a folder with duplicates → refused; impure file → refused; non-KRM standalone → refused;
  hard-Kustomize `kustomization.yaml` (patches) → refused; a clean folder + plain `kustomization.yaml`
  with only `namespace:` + `resources:` → **accepted** (no false refusal).

### Phase 2 — git writer: call the gate, abort the commit

- In the resync apply path, after the store is built and before commit, run the gate; on refusal return
  the typed `AcceptanceRefusedError` (no commit). Mirror in the live `flushEventsToWorktree`.
- Build the store with `manifestanalyzer.DefaultAllowlist()` (not `Allowlist{}`) so a legitimate
  `kustomization.yaml` is *retained*, not mis-refused as non-KRM. (Confirm the live writer still indexes
  every managed doc for placement with the default allowlist.)
- Unit tests at the `internal/git` level: a seeded worktree with an unsupported file → flush/resync
  returns the refusal error and writes nothing; a clean worktree → unchanged behavior.

### Phase 3 — watch/status: surface refusal as Blocked

- Add reason constant `StreamReasonUnsupportedContent` ([stream_readiness.go:46](../../internal/watch/stream_readiness.go#L46)).
- In `drainScopedResync`, detect `errors.As(result.Err, *AcceptanceRefusedError)` and call
  `markTargetStreamState(gitDest, key, StreamStateBlocked, StreamReasonUnsupportedContent, msg)` where
  `msg` names the offending file(s). Keep the existing failure metric.
- Confirm the reason/message propagate into the `StreamsReady` condition message via
  `deriveStreamsReadyCondition`; add the reason to any reason allow-list if one exists.
- Unit tests: a resync drain with a refusal error marks the stream Blocked with the reason; a normal
  error keeps today's behavior (metric only).

### Phase 4 — e2e (Test D from the e2e plan)

- New `test/e2e/unsupported_folder_e2e_test.go`: seed a GitTarget path with a hard-Kustomize
  `kustomization.yaml` (a `patches:` block), create the GitTarget + a WatchRule, and assert:
  - `StreamsReady` goes `False` with reason `UnsupportedContent` (and the message names the file), and
  - **no commit** is produced for the refused type (the folder is not mutated).
- A second case (cheap): a duplicate-identity folder → same refusal, proving Tier 1.

### Phase 5 — validation & docs

- `task fmt` → `task generate` → `task manifests` (new reason/printcolumn if any) → `task vet` →
  `task lint` → `task test` (commit the coverage-baseline bump if it rises) → `task test-e2e`
  (sequential; needs Docker).
- Update [architecture.md](../architecture.md): the "untracked, non Kubernetes, unresolved, or unsafe
  YAML is left alone per analyzer policy" line is now only half-true — structure-unsafe and
  hard-Kustomize content is **refused**, not left alone. Update the
  [Mark and Sweep Resync](../architecture.md#mark-and-sweep-resync) and Operational Boundaries text.
- Flip the e2e plan's Test D from "blocked" to "implemented."

## 6. Open sub-decisions (resolved, recorded for review)

1. **Whole-folder vs type-scoped refusal.** The acceptance gate is whole-folder; a refusal blocks the
   types whose materialization touches the unsafe content. **Decision:** evaluate whole-folder (the gate's
   natural unit) but surface Blocked on the resync's scoped type(s); a whole-target resync blocks all.
   Revisit if it proves too coarse.
2. **Default allowlist in the writer.** Switching the live/resync store build from `Allowlist{}` to
   `DefaultAllowlist()` changes how `kustomization.yaml` is treated (retained vs materialized as non-KRM).
   **Decision:** use `DefaultAllowlist()` — required so a legit kustomize entrypoint is not a false
   refusal. Verify no placement regression (the writer indexes managed docs; retained files are not
   placement targets anyway).
3. **Live-path gating.** **Decision:** gate both, share one helper — a refused folder must not be written
   by a racing live event either.
4. **Mapping-aware refusals (unwatched/out-of-scope).** **Decision:** out of scope here (discovery-blink
   risk); separate follow-up with the followability registry + InScope predicate.

## 7. Risks

- **False refusals** break a previously-working folder. Mitigation: Tier-1 set is purely structural and
  already unit-tested in `acceptance_test.go`; Tier-2 reuses the existing, tested
  `hasUnsupportedKustomizeFeature`. Phase-1 tests explicitly assert a clean kustomize folder is accepted.
- **e2e flakiness.** The suite is actively being de-flaked. Keep Test D `Serial`/small and reuse existing
  repo-setup + `StreamsReady` wait helpers rather than inventing new polling.
- **Surface overloading.** Blocked-for-write-refusal is pragmatic; documented in §3 with the dedicated-
  condition follow-up noted so intent is not lost.
