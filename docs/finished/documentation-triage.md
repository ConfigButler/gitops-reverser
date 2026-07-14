# Documentation triage: what binds, what is history, what to do

> **finished** — this plan was executed on 2026-07-11. Kept as the record of *why* the
> tree looks the way it does. For what to read now, see [`../INDEX.md`](../INDEX.md).
>
> Outcome: 180 documents -> 117. `docs/spec/` created (25 docs the code depends on),
> `docs/design/manifest/` deleted (19 -> 1 summary + 4 promoted specs), ~50 dead documents
> removed, and every dangling citation repaired — including the 17 that were already broken
> before this started.
>
> Scope as surveyed: all 180 markdown files under `docs/` (~55,000 lines).

## The headline

**About 35 documents actually bind. The other ~145 are history.** You cannot
currently tell which is which without reading them, and that — not the volume — is
the problem.

| Group | Files | Of which still binding |
|---|---:|---:|
| `docs/*.md` (user/operator guides) | ~20 | all — they are the product surface |
| `docs/facts/` | 4 | all — durable reference |
| `docs/design/*.md` + `design/stream/` | 57 | **9 active**, 9 reference |
| `docs/design/support-boundary/` | 13 | **all — the live workstream** |
| `docs/design/manifest/` + `version2/` | 19 | ~3 — the rest shipped |
| `docs/finished/` | 41 | **10 load-bearing** (see below — this is the bug) |
| `docs/future/` | 13 | 8 still wanted |
| `docs/ci/`, `docs/audit-setup/`, misc | 13 | reference |

## The diagnosis: the folder names lie, in both directions

This is the whole finding, and it explains why the tree feels unmanageable.

**`docs/design/` contains 23 documents whose work has already shipped**, sitting
beside the 9 that are still open, with nothing to distinguish them. Several still
say `Status: PROPOSAL` while the code that implements them has been in `main` for
weeks (`unsupported-folder-refusal-plan.md` is the clearest case — it ships as
`GitPathAccepted` with an e2e test, and its header still says "PROPOSAL").

**`docs/finished/` contains 10 documents that are the *sole written record* of a
live invariant.** They are not finished at all. The worst case:

> **`docs/finished/current-manifest-support-review.md`** is the live specification
> of `internal/manifestanalyzer`. It is cited by section name from eight Go files,
> and it is a cited input to the *active* gitops-api workstream. It holds the
> non-negotiable rules — *a GitTarget makes an all-or-nothing claim on a folder*;
> *never partially materialize a multi-doc file*; *refuse the folder rather than
> prune unwatched KRM*. It is filed under "finished."

Nine more like it are listed below. **Archive `finished/` wholesale and you delete
the contract.**

Only 74 of 180 files carry a `> Status:` line at all, and several of those are
stale. There is no mechanical way to ask "what is still true?"

## Hard constraint: 17 doc citations in the Go code are already broken

A previous reorganisation moved documents and never fixed the pointers. Today,
`grep`ping the Go sources for `docs/**.md` yields **17 paths that do not exist**:

```
docs/guide.md                                             docs/keep.md
docs/design/manifest/implementation-plan.md               docs/design/commit-window-refactor.md
docs/design/manifest/current-manifest-support-review.md   docs/design/audit-readiness-probe-plan.md
docs/design/stream/api-source-of-truth-reconcile.md       docs/design/support-boundary/f1-...md
docs/future/secret-value-retention-plan.md                docs/design/support-boundary/f7-...md
docs/design/stream/audit-diagnostic-streams-plan.md       …and 6 more
```

Six of those point at documents that no longer exist anywhere. The rest point at
documents that *moved into* `docs/finished/`.

Two consequences, and they shape the entire plan below:

1. **The code already thinks the load-bearing docs live in `docs/design/`.** That
   is an argument for moving them back rather than for leaving them where they are.
2. **Any move must fix citations in the same commit,** or it makes this worse. This
   is exactly how the current mess was made.

## What actually binds — the read-this list

If you read nothing else, read these. This is the set that describes how the system
works *today* or what is being decided *now*.

### The live workstream (13) — `docs/design/support-boundary/`

All of it. This is the branch you are on. My separate proposal
([expansion-boundary-and-corpus-organisation.md](../design/support-boundary/expansion-boundary-and-corpus-organisation.md))
recommends adding a `support-contract.md` front door and folding four restatements
of the fan-in rule into one.

### Load-bearing, but misfiled under `finished/` (10) — **promote these**

Each is the only place a still-current invariant is written down, and most are cited
from the code:

| Doc | The invariant that lives only here |
|---|---|
| `current-manifest-support-review.md` | all-or-nothing folder claim; never partially materialize a multi-doc file; refuse rather than prune |
| `manifestedit-field-ownership-spike.md` | "the API wins" — full-object ownership, no field-subset ownership, plus the explicit do-not-build list |
| `sops-single-file-no-multidoc.md` | SOPS encrypts a *file* as one cryptographic unit → one document per encrypted file |
| `scale-subresource-audit-rehydration.md` | the `/scale` → `FieldPatch` translation rules and denied-subresource list |
| `commit-window-refactor.md` | one grouped commit = exactly one (author, GitTarget) tuple |
| `commitrequest-multi-finalize-design.md` | why `MaxConcurrentReconciles=1` on the CommitRequest controller |
| `type-lifecycle-events-and-wobble-settling.md` | the settle-window / flap-coalescing spec `internal/typeset` implements |
| `gittarget-isolation-on-rule-change.md` | a rule change on target A must never re-snapshot target B |
| `audit-readiness-probe-plan.md` | why liveness must never depend on Redis (the restart-loop trap) |
| `gvk-gvr-mapping-layer.md` | GVK↔GVR contract; still cited by four active design docs |

### Still-open design (9) — `docs/design/`

`e2e-coverage-gaps-and-improvements-plan.md`, `e2e-finish-plan.md`,
`metrics-observability-plan.md`, `multi-cluster-audit-ingestion-implications.md`,
`reconcile-triggering.md`, `release-image-reuse-plan.md`,
`sensitive-resource-diagnostics-follow-up.md`, `watch-and-catalog-architecture.md`,
`stream/residual-e2e-flakes-2026-06-19.md`.

### Durable reference (9 + `docs/facts/`)

`status-conditions-guide.md`, `e2e-serial-registry.md`, `e2e-test-design.md`,
`kubernetes-api-discovery.md`, `kubernetes-api-resource-catalog.md`,
`kubernetes-apf-and-inflight-tuning.md`, `audit-webhook-api-server-connectivity.md`,
`watch-event-ordering-and-attribution-grace.md`, `sops-repo-bootstrap-out-of-scope.md`.

### Still wanted (8) — `docs/future/`

`idea-application-editing.md` (the product seed of the gitops-api workstream —
still holds the branch/session grouping strategies nothing else covers),
`ha-gittarget-distribution-plan.md` (cited three times by `architecture.md`),
`least-privilege-remaining-work.md`, `design-commit-request-phase-2.md` (verified
live gap: nothing GCs `CommitRequest` objects), `idea-unify-pending-write-kinds.md`,
`idea-cross-kind-dependency-watches.md`, `publish-age-recipients-as-metadata.md`,
`idea-commit-signing-key-validation.md`.

## What is history

### Shipped, but still sitting in `docs/design/` (23) — move to `finished/`

`audit-webhook-tls-design`, `commitrequest-admission-authorship`,
`deletecollection-attribution-expander`, `dynamic-analysis-fuzzing-plan`,
`e2e-aggregated-apiserver-test-design`, `e2e-bi-directional-corner`,
`e2e-ci-runner-sharding-plan`, `e2e-signing-followups`, `e2e-speedup-plan`,
`git-credentials-interop`, `gitpath-foreign-content-stringency`,
`image-refresh-test-design`, `mutation-capture-lab-design`, `redis-key-schema-v3`,
`sensitive-resource-classification-plan`,
`sops-repo-bootstrap-and-key-management-architecture`,
`startup-robustness-cert-and-crd-wobble`, `tilt-playground-plan`,
`typeset-owns-discovery-grace`, `unsupported-folder-refusal-plan`,
`watch-first-ingestion-architecture`, `stream/commitrequest-design`,
`stream/streaming-readiness-status-machine-design`.

Note `watch-first-ingestion-architecture.md` is the *most* important of these — it
is the rewrite that deleted half the audit world — but it has landed, so it is now
an explainer, not a plan. It should arguably graduate into `docs/architecture.md`
rather than into `finished/`.

### Superseded (6 + 13) — **delete**

Git is the archive. These have named successors and nothing cites them:

- In `design/`: `e2e-aggregated-apiserver-proxy-hookup-plan`,
  `gittarget-lifecycle-and-repo-architecture`,
  `gittarget-state-machine-bootstrap-assessment`, `status-design-git-target`,
  `watchrule-wildcard-and-resolution-semantics`,
  `stream/per-type-streaming-readiness-plan`.
- In `finished/`: `api-source-of-truth-reconcile`,
  `commitrequest-attribution-divert-reliability`, `internal-audit-type-demand`,
  `design-commit-request-api`, `deletecollection-resync-nudge-plan`,
  `audit-metrics-overhaul-plan`, `materialization-tail-and-live-readiness-review`,
  `watch-list-checkpoint-plan`, `per-resource-type-rv-keyed-streams-experiment`,
  `idea-audit-enrichment-side-channel`, `rule-set-snapshot-discovery-lag-fix`,
  `watchrule-gvr-resolution-plan`, `manifestedit-new-file-placement-spike`.

One catch before deleting `materialization-tail-and-live-readiness-review.md`:
`internal/typeset/materializer.go:34` still cites its "Rec 6" as an open
optimisation. Move that note into the code comment first.

### Closed investigations (9) — **delete**

Forensics whose bugs are fixed: `e2e-full-suite-shared-state-investigation`,
`e2e-watchrule-cross-spec-interference`, `upgrade-finding`,
`watch-audit-rule-matching-improvement`, `watch-first-merge-readiness`, and four
under `stream/`. These are the genre with the worst read-value-to-length ratio —
long, dramatic, and entirely about bugs that no longer exist.

Keep one exception: `stream/signing-snapshot-tail-replay-failure-investigation.md`
is cited from `internal/git/branch_worker.go:330` as the reason a watermark gate
exists. Move that reasoning into the code comment, then delete.

### Archaeology (18 in `finished/`) — cold-store or delete

Mostly the pre-watch-first audit world: `demand-gated-audit-ingestion`,
`canonical-stream-retirement`, `shallow-audit-event-misclassification`,
`partial-object-audit-event-handling`, `design-audit-ingestion-hardening`, and 13
more. `internal/gate` and the audit-as-state pipeline they describe are **deleted
code**. Nothing forward-looking depends on them.

### Overtaken / stale (5) — delete

`future/design-snapshot-engine-evolution` (every part landed elsewhere in a
different shape), `future/idea-end-user-commit-messages` +
`future/addendum-end-user-commit-messages-audit-transports` (both rest on "the audit
stream is the source of truth", which watch-first made **false**),
`future/watchrule-wildcard-support-plan`,
`future/further-away/accesspolicy-design` (targets `GitRepoConfig`, a CRD two
renames dead), `design/voter-gitops-demo-instances`.

## The plan

Phased so that the cheap, zero-risk win lands first and nothing breaks.

### Phase 0 — the index (zero risk, most of the benefit)

**Write `docs/INDEX.md`: the ~35 documents that bind, grouped by topic.** Move
nothing. Delete nothing. You immediately get "read these 35, the rest is history",
which is the actual thing you asked for. Everything below is optional cleanup.

### Phase 1 — status headers (mechanical)

Put a `> Status:` line on every design doc, from a closed vocabulary:
`active` | `shipped` | `superseded-by: <path>` | `closed-investigation` |
`reference`. Then `grep '^> Status: active'` answers the question forever, and the
next person does not have to redo this triage.

### Phase 2 — fix the 17 broken citations, and promote the 10 load-bearing docs

Do these **in one commit**, because the fix and the move touch the same lines. The
code already points at `docs/design/` for most of them, so promoting them back
*is* the fix.

Suggested destination: a `docs/spec/` folder meaning **"this is true now and the
code depends on it"** — distinct from `design/` ("we are still deciding") and
`finished/` ("this happened"). The three-way split is what the current two-way one
cannot express, and it is precisely the distinction that broke.

### Phase 3 — move the 23 shipped docs out of `design/`

Now `docs/design/` means what it says: undecided work. It shrinks from 57 files to
~9.

### Phase 4 — delete the dead (~50 files)

Superseded, closed investigations, archaeology, stale. Extract the three code-cited
facts first (noted above). Git keeps everything; nothing is lost that a
`git log --follow` cannot recover.

### Phase 5 — the gitops-api consolidation

Separately proposed in
[expansion-boundary-and-corpus-organisation.md](../design/support-boundary/expansion-boundary-and-corpus-organisation.md):
add a `support-contract.md` front door, fold the four restatements of the fan-in
invariant into one, and de-duplicate the `--dry-run=server` evidence table.

## What I would *not* do

- **Do not bulk-archive `docs/finished/`.** Ten of those documents are the contract.
  That is the single highest-risk action available here, and it is the one that
  looks most obviously safe.
- **Do not move anything without fixing the Go citations in the same commit.** That
  is how the current 17 broken pointers were created.
- **Do not delete the investigations to save space.** Delete them because their bugs
  are gone. Two of them still hold the *reason* a guard exists in live code; those
  reasons must land in the code comment first, or you will lose them and someone
  will remove the guard.

## Open question

**Should `docs/design/manifest/` survive at all?** Its three subsystems —
`internal/typeset`, `internal/git/manifestedit`, `internal/manifestreport` — have
all shipped, so most of its 19 files are archaeology. But it is also where the
*reasoning* behind the manifest model lives, and the gitops-api workstream is built
directly on top of it. My instinct is that two or three docs graduate to `spec/`
(`contextual-namespace-and-kustomize-folder-editing.md`,
`version2/gittarget-new-file-placement-rules.md`,
`version2/type-followability.md`) and the other sixteen go, but that is the call I
am least sure of and it is worth your eye rather than mine.
