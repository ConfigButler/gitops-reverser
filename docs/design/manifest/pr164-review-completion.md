# PR #164 Review Completion Plan

> Status: Phases 1–3 implemented (2026-06-04); full validation green —
> `task fmt/vet/lint/test` pass and `task test-e2e` passed (47/49, 2 skipped, 0 failed)
> Source: triage of the 16 bot review comments (4 gemini, 12 coderabbit) on PR #164
> (branch `poc/manifestedit`), cross-checked against the actual code.
> Related: [manifestedit-abstraction-plan.md](manifestedit-abstraction-plan.md),
> [manifestedit-integration-readonly-reconcile.md](manifestedit-integration-readonly-reconcile.md)

A phased plan to close the review. Three phases, ordered so the riskiest
production change lands with the most validation. Cosmetic/optional items are
explicitly out of scope unless opted in.

## Phase 1 — Production correctness (the only behavioral changes)

- [x] **1.1 Guard the multi-doc wholesale-overwrite fallback** —
  [git.go:764-786](../../../internal/git/git.go#L764-L786)
  - Before the fallback `os.WriteFile(fullPath, content)`, refuse to clobber a
    file that holds documents other than the target. If the existing file is
    multi-document and the in-place edit did not apply, skip the write and emit a
    diagnostic rather than overwriting siblings.
  - **Do not** adopt coderabbit's suggested diff — it removes the `canonicalize`
    gate and breaks the "operator-authored canonical files stay byte-identical
    wholesale" guarantee.
  - *Acceptance:* a new test seeding a hand-authored multi-doc file where the
    in-place edit fails proves siblings survive;
    `TestHandleCreateOrUpdate_CanonicalFileStaysWholesale` still passes.

- [x] **1.2 Propagate replacement failure in `mergeValue`** —
  [merge.go:115-125](../../../internal/git/manifestedit/merge.go#L115-L125)
  - Change all three `return replaceNode(node, desired), true` to
    `changed := replaceNode(...); return changed, changed`, so an encode failure
    flips `ok=false` and triggers whole-document fallback instead of a silent drop.
  - *Acceptance:* existing merge tests stay green; coverage holds >90%.

- [x] **1.3 Nil-object guards** —
  [render.go:97](../../../internal/manifestreport/render.go#L97),
  [report.go:99](../../../internal/manifestreport/report.go#L99)
  - Add `if obj == nil { return nil, false }` at the top of `EditInPlace`;
    `if obj == nil { continue }` in the `BuildReport` loop. This subsumes gemini's
    git.go:821 comment — no separate change needed there.
  - *Acceptance:* a table-test row passing `nil` returns the guard result instead
    of panicking.

## Phase 2 — Test fix

- [x] **2.1 De-vacuum the alias-bomb test** —
  [manifestedit_test.go:522](../../../internal/git/manifestedit/manifestedit_test.go#L522)
  - Switch `bomb.sops.yaml` → `bomb.yaml`, add
    `require.Len(t, inv.Records, 1, …)`, and assert `inv.Records[0].Editable ==
    false`. A `.sops.yaml` file with no `sops:` key produces zero records, so the
    current `for _, r := range inv.Records` loop never runs and asserts nothing.
  - *Acceptance:* test fails if the alias bomb were ever marked editable.

## Phase 3 — Doc consistency (docs-only)

- [x] **3.1 Fix status contradiction** —
  [manifestedit-integration-readonly-reconcile.md:3](manifestedit-integration-readonly-reconcile.md):
  change "writer wiring is deferred" → "writer wiring landed (narrowly)" to match
  the body.

- [x] **3.2 Remove dangling scratchpad URLs** —
  [TODO.md:96-98](../../TODO.md): delete the three loose URLs and the `Replace with…`
  line (or fold the VictoriaMetrics link into a real backlog bullet if still
  wanted). Resolved directly in TODO.md: the URLs were moved into a structured
  "Research work:" section with one-line context each, which satisfies the
  reviewers' intent.

## Validation

- After Phases 1–2 (Go code): full [AGENTS.md](../../../AGENTS.md) sequence —
  `task fmt` → `task generate` → `task manifests` → `task vet` → `task lint` →
  `task test` → `task test-e2e` (e2e sequentially; `docker info` first).
- Phase 3 alone qualifies for the docs-only exception if committed separately —
  link sanity check only.
- Suggested commits: one for Phases 1–2 (code, full suite), one for Phase 3 (docs).

## Explicitly out of scope (optional — confirm before doing)

- `PatchDocument` nil guard ([patch.go:42](../../../internal/git/manifestedit/patch.go#L42))
  — defensive only, not on the prod path.
- `hasDocEndMarker` inline-comment edge
  ([framing.go:61](../../../internal/git/manifestedit/framing.go#L61)) — marginal;
  `reskinDocument` still drops the comment even if detection is fixed.
- Spelling in [file-agnostic-placement.md](file-agnostic-placement.md) and MD040
  fenced-code-language in
  [wildcard-ci-failure-findings.md](../../wildcard-ci-failure-findings.md) —
  markdownlint is not CI-enforced; cosmetic.

## False positive / already handled (no work)

- coderabbit "critical" *"Don't delete the whole file for a single-resource
  prune"* (git.go 604-623): already correct.
  [handleDeleteOperation](../../../internal/git/git.go#L683-L713) routes through
  `manifestedit.DeleteDocument` and only unlinks when `result.FileEmpty`;
  otherwise it writes surviving documents back. The cited lines point at `locate`,
  not the delete path.
