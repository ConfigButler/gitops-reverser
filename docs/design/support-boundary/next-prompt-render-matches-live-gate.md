# Prompt: implement the RenderMatchesLive gate

Copy everything below the line into a fresh session.

---

Implement the **render-vs-live gate** — `RenderMatchesLive` — the fence that refuses to track a
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

Read **parsed values, not raw bytes** (so a `${var}` in a comment or a CRD schema description never
counts). Token regex (from the reverted `substitution.go`, recoverable from git history):
`` `\$\{[A-Za-z0-9_.][^}]*\}` `` — matches `${cluster_domain}` and `${schema.spec.replicas}`; not
`$(POD_IP)` (parens are native / kustomize var syntax) and not `${}`.

**Do not over-claim the cause, and bias toward blocking.** A rendered token + a diverged live value
proves only that our render did not produce that value — it could be Flux postBuild, a direct live
edit, an admission mutation, or another controller. Refusal is safe regardless, which is why the
reason is `RenderDoesNotMatchLive` (a fact) not "substituted" (a guess). And the guiding bias:
**blocking a shade too soon is fine; failing to block is not.** A simple, slightly-over-eager token
gate is the right first cut — do not reach for cleverness to avoid the rare over-block.

- **6a — per-write refusal.** In the write path, refuse a write that would overwrite a rendered token
  with a diverged live value, aborting the flush. Mirror the existing `sourceFormRefusal` /
  `SourceFormRefusedError` pattern.
- **6b — the blocking folder condition.** The same predicate ORed across the folder — and it has **two
  integration requirements the per-write refusal alone does NOT give you** (do not assume it comes for
  free):
  - **Aggregate across every scoped resync.** Reconcile runs per type (the M12 scoped resyncs); the
    folder's verdict is the OR over *all* scopes. A divergence in any one type must fail the whole
    folder — a "last-successful-GVR" status would let a clean type mask a diverging one.
  - **Stop writing while failed.** `RenderMatchesLive=False` must prevent any further write window from
    opening for the target; a status-only refusal that keeps mirroring violates the gate.
  Add a dedicated issue kind + reason (`RenderDoesNotMatchLive`). Whether to add a distinct
  `RenderMatchesLive` status condition beside `GitPathAccepted`, or reuse `GitPathAccepted` with the
  reason, is a call to make against how the other write-boundary refusals surface.

## Where it hooks (entry points, verified in the code)

- **Per-write:** [`internal/git/plan_flush.go`](../../../internal/git/plan_flush.go) `patchExisting`
  holds the Git doc (`gitDocRawObject(buf.current, idx)`), the desired/live projection, and `dm`. Use
  **`dm.Rendered.Object` as the render** for a kustomize doc and the Git doc for a plain one; run
  `diverges()` against the live projection; on divergence return the refusal (see `sourceFormRefusal`
  for the shape). It fires on `patchExisting` — an existing *rendered* token being overwritten — which
  is the corruption (a new doc goes through `createNew`, with no token yet to protect).
- **Resync (this is what makes it blocking):**
  [`internal/git/resync_flush.go`](../../../internal/git/resync_flush.go)
  `applyResyncToWorktree` → `applyResyncPlan` → `applyUpsert` → `patchExisting`. The refusal fires
  here automatically; **verify** the error aborts the resync and surfaces as a blocked stream
  (`commitPendingWrites` → `applyResync` replies `Err`), rather than being swallowed.
- **Surfacing:** a new `IssueKind` in
  [`internal/manifestanalyzer/acceptance.go`](../../../internal/manifestanalyzer/acceptance.go), and
  map it to the reason in
  [`internal/watch/event_router.go`](../../../internal/watch/event_router.go) `gitPathRefusalReason`.

## The test net

- **Unit (`internal/manifestanalyzer`)** for `diverges()`: CRD `${var:=default}` in a description
  with `live == git` → **not** diverged; KRO `${schema.spec.*}` `live == git` → **not** diverged;
  nginx ConfigMap `${host}` `live == git` → **not** diverged; Deployment env `${REGION}` with
  `live = us-east` → **diverged**; a token only in a comment → **not** diverged; native `$(VAR)` →
  **not** a token. **The render-not-source guardrail (do not skip it):** a source
  `metadata.labels.env: ${ENV}` under a kustomization `labels: {env: prod}` (so the render is
  `env: prod`) with `live = prod` → **not** diverged — a git-vs-live implementation fails this, a
  render-vs-live one passes.
- **Write-path (`internal/git`)**: an out-of-band-substituted doc refuses the flush
  (`WriteBoundaryRefused` / `RenderDoesNotMatchLive`); a folder whose tokens match live (`live == git`)
  mirrors with no refusal.
- **Corpus:** regenerate `task gitops-layouts-baseline` and confirm **nothing moves** — the corpus has
  no live objects, so nothing can be diverged. In particular the KRO row must **not** move this time
  (it did under the reverted structural check; that is the difference between this fence and that one).
- **e2e:** the **CRD-lifecycle spec must pass** — it is the one the reverted check broke. Run
  `task test-e2e` and **capture the full log** (`task test-e2e 2>&1 | tail -N` reports `tail`'s exit
  code, not the suite's — a failing suite reads as green; assert on the `Passed | Failed` summary
  line or redirect to a file). Docker required (`docker info`).

## Validation and delivery

Full sequence per `AGENTS.md`: `task fmt` → `generate` → `manifests` → `vet` → `lint` → `test` →
`test-e2e` (sequential; needs Docker). Commit on `fix/kustomize-source-form-projection` (#234), then
restack `feat/kustomize-tolerate-patches` (#235) onto the new HEAD. Report the honest line delta.

## How this workstream finds bugs

**Measure against real content, not just the corpus.** The reverted structural check looked clean on
the corpus and broke on the real Flux CRD in e2e — because the corpus has no live objects and no
real-world CRD schemas. Before claiming a detection is clean, run it against the actual resources the
e2e installs. And the standing rule: when you want to know what the orchestrator does, do not reason
about it — measure our render against the live object.
