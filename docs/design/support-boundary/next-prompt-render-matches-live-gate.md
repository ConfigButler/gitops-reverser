# RenderMatchesLive gate: implementation record

> **completed (2026-07-15)** — the predicate, fixture corpus, scoped epoch gate, worker enforcement,
> GitTarget condition, CRD print column, and unit/end-to-end validation are shipped. This document is
> retained as the original implementation brief, with corrections where its assumptions differed from
> the delivered runtime.

## Delivered

- Parsed render-vs-live `${...}` comparison for both plain and kustomize-governed documents.
- Refusal of both live-event and scoped-resync writes before Git bytes change.
- `RenderMatchesLive` state machine: `Unknown` and `False` close normal write windows; only every
  current scope clean makes it `True`; stale results and later clean results cannot clear a divergence.
- Separate `RenderDoesNotMatchLive` reporting; it does not change `GitPathAccepted`.
- Fixture, gate, writer, watch, controller, CRD, lint, unit, and end-to-end coverage.

## Still open

- An incoming remote Git revision does not yet refresh the local source and begin a new epoch. A Git
  repair therefore does not automatically reopen a false gate.
- That recovery must be coupled to the retained-intent/orchestrator barrier in
  [orchestrator-reconcile-trigger.md](orchestrator-reconcile-trigger.md), not added as an unsafe
  periodic fetch.
- The dedicated Flux postBuild end-to-end fixture and the general non-token fence (5b) remain future
  work.

---

## Historical implementation brief

The completed work implemented the **render-vs-live gate** — `RenderMatchesLive` — the fence that refuses to track a
folder whose live objects differ from our render because of context we cannot see (Flux `postBuild`
substitution, Argo `spec.source.kustomize` overrides, a divergent kustomize version). Build the
**token form (5a)**, which is the part we block on. This is the implementation of a design that is
already written and decided; do not re-derive it, and do not widen it.

## Read first

- `AGENTS.md`.
- The memory notes: `substitution-token-check-breaks-crds` (**the lesson — read it first**),
  `kustomize-renderer-workstream`, `diagnose-e2e-via-controller-logs`.
- **`docs/design/support-boundary/render-fidelity.md`** — the whole design. §4 (the fence), §5a
  (what to build), §6 (the two surfaces: per-write + the blocking condition), §7 (where it runs —
  option A, a precondition on the reconcile-on-acceptance), §8 (what is deferred, and why 5b waits).
- **`docs/design/support-boundary/render-fidelity-scenarios.md`** — the red-first fixture corpus and
  the folder-gate state traces. Implement those tests before production code.

## Where things stand, and the one trap

- #234 shipped `sourceForm` ([`internal/manifestanalyzer/source_form.go`](../../../internal/manifestanalyzer/source_form.go)):
  where the live object and our render agree, the source keeps its bytes. It stops *our* render's
  output leaking into the source — but **not context we do not render** (postBuild etc.). That gap is
  this gate.
- A **structural** "refuse any managed doc containing `${...}`" acceptance check was **tried and
  reverted**. It broke CRD mirroring: a CRD schema `description` carries literal `${var:=default}`
  (the Flux Kustomization CRD documents postBuild in its own schema), and a structural check cannot
  tell a *literal* token from a *substituted* one. **The discriminator is the live object, not the
  disk.** Do not re-attempt anything structural.

## The one thing not to get wrong

The check is render-vs-**LIVE**, never a pattern on disk. A `${...}` token is dangerous **only** when
the live object holds a *different, resolved* value at that field. Where `live == our render` — a CRD
description, a KRO `${schema.spec.*}` template, an nginx/envsubst ConfigMap — the folder **mirrors
fine and must never be refused**. Write those "must still mirror" cases FIRST and keep them green;
they are the guardrail the reverted check failed.

## What to build (5a, both surfaces of one predicate)

The predicate, computed once:

> `diverges(doc, live)` := the **RENDER** carries a `${...}` token at a field where the live object
> holds a **different** value — where the render is `dm.Rendered.Object` for a kustomize-governed
> document and the **Git document itself** for a plain manifest.

**It must be render-vs-live, NOT git-vs-live.** kustomize does not *resolve* a `${...}` token, but it
does not *preserve* every token-bearing source field either: a supported `labels` / `commonLabels`
transform overwrites `metadata.labels[...]` via `SetEntry`, so a source `env: ${ENV}` under
`labels: {env: prod}` renders to `env: prod` — equal to live. A git-vs-live check would **falsely
refuse** that faithful folder; render-vs-live does not, and it also catches a token a `patches:` block
*injects* that the source never had. (See `docs/facts/kustomize-never-emits-dollar-brace.md` and
render-fidelity.md §5a.)

Read **parsed values, not raw bytes**. Comments never enter parsed data. A CRD schema `description`
*is* a parsed scalar and must be compared normally: its literal token is safe because the render and
live value are equal, not because descriptions receive an exemption. The shipped token regex is
`\$\{[^{}]+\}`: any non-empty non-nested brace expression matches; `$(POD_IP)` (parens are native /
kustomize var syntax) and `${}` do not.

**Do not over-claim the cause, and bias toward blocking.** A rendered token + a diverged live value
proves only that our render did not produce that value — it could be Flux postBuild, a direct live
edit, an admission mutation, or another controller. Refusal is safe regardless, which is why the
reason is `RenderDoesNotMatchLive` (a fact) not "substituted" (a guess). And the guiding bias:
**blocking a shade too soon is fine; failing to block is not.** A simple, slightly-over-eager token
gate is the right first cut — do not reach for cleverness to avoid the rare over-block.

- **6a — per-write refusal.** In the write path, refuse a write that would overwrite a rendered token
  with a diverged live value, aborting the flush. Mirror the existing `sourceFormRefusal` /
  `SourceFormRefusedError` pattern.
- **6b — the blocking folder condition.** The same predicate ORed across the folder powers a distinct
  `RenderMatchesLive` GitTarget condition. Do **not** reuse `GitPathAccepted`: that condition remains
  the structure/write-boundary claim, whereas fidelity depends on current Git and live state.

  The shipped epoch state machine uses `(GVR, namespace)` scopes. A target-watch declaration or scope
  replacement starts an epoch with every scope pending and `RenderMatchesLive=Unknown`; only every scope
  clean makes it True; any divergence makes it False; stale results are ignored; and only a new,
  complete epoch can clear False. Both Unknown and False block normal write windows. Resync remains
  allowed while blocked so it can measure that epoch. A Git revision or arbitrary GitTarget generation
  does **not** yet start an epoch. Beginning an epoch closes the worker gate; an already-open window is
  discarded if it later finalizes while closed. The worker, not a status update, is the enforcement point.

  The implementation adds a dedicated issue kind + `RenderDoesNotMatchLive` reason. The condition
  reports one deterministic `(field, token)` representative, while the write refusal also names the
  file. The status derives from the same epoch state; no scoped-resync
  success may unconditionally mark a target healthy.

## Where it hooks (entry points, verified in the code)

- **Per-write:** [`internal/git/plan_flush.go`](../../../internal/git/plan_flush.go) `patchExisting`
  holds the Git doc (`gitDocRawObject(buf.current, idx)`), the desired/live projection, and `dm`. Use
  **`dm.Rendered.Object` as the render** for a kustomize doc and the Git doc for a plain one; run
  `diverges()` against the live projection; on divergence return the refusal (see `sourceFormRefusal`
  for the shape). It fires on `patchExisting` — an existing *rendered* token being overwritten — which
  is the corruption (a new doc goes through `createNew`, with no token yet to protect).
- **Resync and folder gate:**
  [`internal/git/resync_flush.go`](../../../internal/git/resync_flush.go)
  `applyResyncToWorktree` → `applyResyncPlan` → `applyUpsert` → `patchExisting`. The refusal fires
  here during a scoped resync, so it must abort that resync before a flush. But that is only the
  per-write half: begin and reduce the epoch at the watch/worker boundary, record each scope result
  before the next queued target write can run, and leave regular writes closed until the reduction is
  True. The current target watch scopes include namespace as well as GVR.
- **Enforcement and surfacing:** add the state owner that can atomically answer "may this target open a
  write window?" from the branch worker, then project the same state to the controller as the new
  condition. Update [`internal/manifestanalyzer/acceptance.go`](../../../internal/manifestanalyzer/acceptance.go)
  with the issue kind, [`internal/watch/event_router.go`](../../../internal/watch/event_router.go) with
  the dedicated reason, and the controller's condition/status derivation. Do not implement this as a
  `MarkTargetGitPathAccepted` variant: that is last-result status, not a folder gate.

## The test net

- **Predicate fixtures (`internal/manifestanalyzer/testdata/render-fidelity/`)**: build the complete
  red-first matrix in `render-fidelity-scenarios.md §2`, including CRD/KRO/nginx literals, comments,
  `$(VAR)`, absent live fields, nested lists, a source token overwritten by labels, and a token injected
  into the **render** by supported labels. The last two are non-negotiable render-not-source guardrails.
- **Gate state unit tests:** build the §3 epoch trace before wiring watches: pending scopes deny writes;
  a later clean scope cannot erase a divergence; stale results are ignored; a complete explicitly
  started epoch reopens the target; and a per-write divergence immediately closes it.
- **Writer/watch integration:** a substituted document refuses the resync with no commit; a clean event
  queued behind it cannot open a write window; an existing uncommitted target window is discarded if it
  later finalizes while a fresh epoch has the gate closed; and a full fresh recheck can recover. Assert the distinct `RenderMatchesLive=False` /
  `RenderDoesNotMatchLive` status rather than `GitPathAccepted=False`.
- **Corpus:** regenerate `task gitops-layouts-baseline` and confirm **nothing moves** — the corpus has
  no live objects, so nothing can be diverged. In particular the KRO row must **not** move this time
  (it did under the reverted structural check; that is the difference between this fence and that one).
- **e2e (still open):** add the dedicated Flux `postBuild` fixture from `render-fidelity-scenarios.md §5`, then keep
  the **CRD-lifecycle spec** green — it is the one the reverted structural check broke. Run
  `task test-e2e` and **capture the full log** (`task test-e2e 2>&1 | tail -N` reports `tail`'s exit
  code, not the suite's — a failing suite reads as green; assert on the `Passed | Failed` summary line
  or redirect to a file). Docker required (`docker info`).

## Validation and delivery

The required sequence was run successfully: `task fmt` → `generate` → `manifests` → `vet` → `lint` →
`test` → `test-e2e` (sequential, with Docker available). The implementation was delivered on
`fix/kustomize-source-form-projection`; future work should begin from the current branch head rather
than relying on the historical branch/restack instructions.

## How this workstream finds bugs

**Measure against real content, not just the corpus.** The reverted structural check looked clean on
the corpus and broke on the real Flux CRD in e2e — because the corpus has no live objects and no
real-world CRD schemas. Before claiming a detection is clean, run it against the actual resources the
e2e installs. And the standing rule: when you want to know what the orchestrator does, do not reason
about it — measure our render against the live object.
