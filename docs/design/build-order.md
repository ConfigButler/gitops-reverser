# Build order: three tracks, and what actually blocks what

> **design**: a sequencing summary, not a plan of record. Nothing here binds until scheduled.
> Date: 2026-08-31. Index: [`../INDEX.md`](../INDEX.md)
>
> **This page holds only the order.** Every item below is specified somewhere else, and the
> specification is the authority on *what* is built — this page is the authority on *when*, and on
> which releases the items have to share. If a detail appears both here and in a linked document,
> the linked document wins and the copy here is the bug.

Five changes are in flight at once and they are spread across five documents, none of which can see
the other four. This page is the missing top: what ships together, what can ship alone, and the two
couplings that are real.

## The three tracks

```mermaid
flowchart TB
  subgraph A["Track A — additive placement (no consumer bump)"]
    direction LR
    A1["PR 1<br/>corpus"] --> A2["PR 2<br/>suspend"] --> A3["PR 3<br/>status.placement<br/>+ post-scan pass"] --> A4["PR 4<br/>useKustomize + serializeNamespace<br/>+ one-source-namespace refusal"]
  end
  subgraph B["Track B — the breaking wave (one coordinated bump)"]
    direction LR
    B1["delete allowedSourceNamespaces"] --- B2["redefine sourceNamespace: *"]
    B2 --- B3["commit.window / commit.message move<br/>+ riders"]
  end
  subgraph C["Track C — patch authoring (no API surface at all)"]
    direction LR
    C1["field-path ownership<br/>attribution"] --> C2["update + retract<br/>lifecycle"]
  end
```

**The tracks do not block each other.** That is the finding worth having: the documents read as one
large interlocked programme, and they are not one. Any track can go first, and two can run in
parallel, because no item in one track reads a field or a semantic that another track changes.

| Track | Breaking? | Waits for | Specified in |
|---|---|---|---|
| **A** — placement | No | nothing | [`../layout/model.md` § How it gets built](../layout/model.md#how-it-gets-built) |
| **B** — source scope | **Yes**, one bump | nothing | [`source-scope-simplification.md` § Migration](source-scope-simplification.md#migration), sequenced with its co-members in [`gittarget-api-wave.md`](gittarget-api-wave.md) |
| **C** — patch authoring | No | nothing | [`support-boundary/patch-authoring.md` § Delivery sequence](support-boundary/patch-authoring.md#delivery-sequence) |

## The two couplings that are real

Everything else is independent. These two are not, and both live inside a single track:

- **Track B is one wave, not two items.** `sourceNamespace: "*"` is *defined* in terms of
  `allowedSourceNamespaces`, so deleting the field without deciding `*` leaves a value with no
  meaning. They ship in the same release or neither does. The
  [definition of record](source-scope-simplification.md#sourcenamespace--needs-its-own-decision)
  carries both readings.
- **Inside track A, `useKustomize` depends on the one-source-namespace rule.** A created
  `kustomization.yaml` carries `namespace:` only when the folder is single-namespace, and the rule
  is what guarantees that for an explicit `serializeNamespace: false`. Both are in PR 4; build the
  rule first.

## Three couplings people expect and that do not exist

Worth stating, because each one has been assumed at least once:

- **The one-source-namespace rule does not depend on `allowedSourceNamespaces`.** It computes
  `{the target's own namespace} ∪ {the explicit rules[].sourceNamespace names of the WatchRules
  pointing at it}` by reading `WatchRule` objects, not the policy field track B deletes. And a `*`
  item is refused under *both* readings of `*`, since neither is provably one namespace from the
  spec alone. So PR 4 needs no rewrite after the wave, and does not have to wait for it.
- **Track A is not part of the breaking wave.** The layout model reversed: the path template stayed
  and gained two optional fields, so nothing in track A changes an existing field's meaning. A Tier
  2 entry belongs to the wave only if it changes a `GitTarget` field in a breaking way.
- **Track C touches no API.** No CRD field, no migration, no persisted state; backing it out returns
  to today's refusal, and patch files already committed stay valid kustomize. Its only real cost is
  a durable one and it is not technical — it moves the boundary from *we invert what kustomize
  declares* to *we author patches*.

## What is left in each track

**Track A.** PRs 1–3 are unbuilt. PR 4's two halves are smaller than they look: registration into an
existing root shipped in [#319](https://github.com/ConfigButler/gitops-reverser/pull/319), and
inference is what `namespaceIsInheritedFromContext` already does. What is genuinely new is writing a
`kustomization.yaml` that does not exist — build that last and on its own, since it is the only
thing that writes a file nobody asked for by name.

**Track B.** Unbuilt, and it is mostly a deletion: 4,569 lines in files that exist for nothing else.
The one thing to *build* is the `SelfSubjectAccessReview` pass, which is additive and can ship after
the wave rather than in it.

**Track C.** Steps 2 and 3 of its delivery sequence already shipped for another reason — the
`$patch: delete` work built the patch-file author, and the render oracle built the verification. What
remains is step 1 (ownership of a field path) plus the update/retract lifecycle the original sequence
omitted. Ballpark: a spike over env vars is a couple of days; a slice worth shipping is one and a
half to two weeks.

## The corpus is the test, and it is not wired up yet

Track A's PR 1 is the one item every other item benefits from, and it is worth saying why it is
first rather than merely convenient. The eighteen fixture folders under
[`../layout/shapes/`](../layout/shapes/README.md) and
[`../layout/specific-examples/`](../layout/specific-examples/README.md)
are read today by **nothing but a human**: no Go file references either directory. Wiring them up
converts every later review from *"does this prose hold together"* into *"does the diff match the
patch"*.

**The seam already exists, at both levels, and neither needs inventing:**

| What | Where | Does |
|---|---|---|
| Golden-directory runner | [`contextual_namespace_corpus_test.go`](../../internal/manifestanalyzer/contextual_namespace_corpus_test.go) | walks `testdata/` folders, asserts a per-document outcome — the exact shape PR 1 needs |
| Write-path driver | `newWorktreeForTest` + `flushEventsToWorktree` ([`inplace_edit_test.go`](../../internal/git/inplace_edit_test.go)) | seeds a worktree, folds events through the real plan-then-flush path |
| Precedent for a refusal fixture | [`namespace_context_refusal_test.go`](../../internal/git/namespace_context_refusal_test.go) | pins the two folder shapes where the store's view and kustomize's disagree |

So PR 1 is assembly, not construction: seed a worktree from `repository/`, build the event from
`input/`, derive the policy from `config/gittarget.yaml`, flush, and compare a normalized diff with
`expected-*.patch`. A `-update` flag that rewrites the patches keeps the corpus cheap to extend.

**Three rules for the corpus, each of which has already been learned the hard way here:**

- **Scenarios for unbuilt behavior are written now and skipped**, with the PR that unskips them named
  in the skip message. PR 4 is finished when the last skip is gone.
- **`config/gittarget.yaml` parses into a harness-local struct** until PR 4 deletes that mapping —
  which is itself a check that the API the examples describe is the API that got built.
- **Refusals are fixtures too.** Every scenario that only ever succeeds is advertising rather than
  specification. The set needs at least: `serializeNamespace: false` with no supplier, a second
  source namespace against an explicit `false`, a folder covering two render roots, and a
  base-owned field edit — each asserting an `expected-status.yaml` rather than a patch.

### The behavior reference this leaves missing

A passing corpus tells a maintainer what happens. It does not tell a **user** what happens, and
there is no page that does: the behavior is currently spread across
[`support-contract.md`](support-boundary/support-contract.md) (what we will and will not touch),
[`status-conditions-guide.md`](../spec/status-conditions-guide.md) (condition shapes), and
[`configuration.md`](../configuration.md) (fields). None of them answers "I changed X in the
cluster — what lands in Git, and what do I see if it refuses?"

That page should be **generated from the corpus rather than written beside it**, so it cannot drift:
one row per scenario, naming the situation, the Git outcome, and the condition a user would read.
Not scheduled, and deliberately not started before PR 1 — it has no source to generate from until
the fixtures execute.

## What this page deliberately does not do

It does not rank the tracks. [`open-asks-priority.md`](open-asks-priority.md) is the priority
argument and still holds its Tier 0–3 ordering for everything *inside* a track; what it could not
carry is the cross-track picture, because it predates the layout reversal. Read that page for what
matters most, and this one for what can move without waiting.
