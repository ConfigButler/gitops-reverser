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

> `diverges(gitDoc, live)` := the Git document carries a `${...}` token at a field where the live
> object holds a **different** resolved value.

kustomize never emits a `${...}` token, so our render preserves the ones the source carries; a
diverged live value at such a field is out-of-band substitution, not a user edit. Read **parsed
values, not raw bytes** (so a comment mentioning `${var}` never counts). Token regex (from the
reverted `substitution.go`, recoverable from git history):
`` `\$\{[A-Za-z0-9_.][^}]*\}` `` — matches `${cluster_domain}` and `${schema.spec.replicas}`; not
`$(POD_IP)` (native Kubernetes env expansion) and not `${}`.

- **6a — per-write refusal.** In the write path, refuse a write that would overwrite a token with a
  diverged live value, aborting the flush. Mirror the existing `sourceFormRefusal` /
  `SourceFormRefusedError` pattern.
- **6b — the blocking condition.** The same predicate at the reconcile-on-acceptance. You get this
  almost for free: the resync runs through the **same** write path, so the per-write refusal already
  fires during the initial resync and blocks the folder. Add a dedicated issue kind and a clear
  reason (`RenderDoesNotMatchLive`) so the GitTarget status says exactly why. Whether to also add a
  distinct `RenderMatchesLive` status condition beside `GitPathAccepted`, or to reuse
  `GitPathAccepted` with the new reason, is a call to make — the reason approach is smaller and
  already delivers blocking + a legible message. Decide it against how the other write-boundary
  refusals surface.

The predicate is git-vs-desired, so it covers **plain and kustomize** documents alike and does not
need `dm.Rendered`.

## Where it hooks (entry points, verified in the code)

- **Per-write:** [`internal/git/plan_flush.go`](../../../internal/git/plan_flush.go) `patchExisting`
  already holds the Git doc (`gitDocRawObject(buf.current, idx)`) and the desired projection. Run
  `diverges()` there; on divergence return the refusal (see `sourceFormRefusal` for the shape). Note
  it fires on `patchExisting` — an *existing* Git token being overwritten — which is exactly the
  corruption (a new doc goes through `createNew` with no token to protect).
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
  **not** a token.
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
