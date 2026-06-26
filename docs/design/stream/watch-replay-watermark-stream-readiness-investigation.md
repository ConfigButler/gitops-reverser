# Watch-first replay watermark: a stream-readiness timing bug

> Status: investigation note / bug report. Facts first; analysis and a proposed
> direction follow and are labelled as such.
> Captured 2026-06-26 · Branch: `investigate` · Author: investigation.
>
> Context: GitHub Actions CI run
> [28231170122](https://github.com/ConfigButler/gitops-reverser/actions/runs/28231170122)
> (and its re-run), PR #174, commit `445c5b33d7f9065babb7f199f9e940be33bf599a`.
>
> Related:
> [watch-first-ingestion-architecture.md](../watch-first-ingestion-architecture.md),
> [materialization-tail-and-live-readiness-review.md](../../finished/materialization-tail-and-live-readiness-review.md)
> (the `Initializing / PartiallyLive / Live` liveness proposal this aligns with),
> [signing-snapshot-tail-replay-failure-investigation.md](./signing-snapshot-tail-replay-failure-investigation.md),
> [mutation-capture-lab-design.md](../mutation-capture-lab-design.md).

## 1. Facts

Everything in this section is observed and reproduced, not inferred.

### 1.1 CI symptom

- The `E2E (full)` job on branch `investigate` failed three consecutive runs, each
  on a **different** spec; specs that failed on one run passed on a re-run. The
  Ginkgo suite exited `201` (test failure), every other job (build, lint, unit,
  e2e-quickstart) passed.
- In the re-run of run `28231170122`, two specs failed:
  - **Commit Signing** — *"should not replay already-reconciled configmaps as
    per-event commits to a late-joining target"* —
    [signing_e2e_test.go:603](../../../test/e2e/signing_e2e_test.go#L603).
  - **Commit Author Attribution** — *"attributes a commit to the OIDC display name
    and email from user.extra"* —
    [commit_author_attribution_e2e_test.go:171](../../../test/e2e/commit_author_attribution_e2e_test.go#L171).

### 1.2 Failure 1 — a CREATE never surfaced as a per-event commit

The spec seeds a live per-event tail by creating then labelling a ConfigMap
`probe-a`, and expects a `[CREATE] v1/configmaps/probe-a` commit. The assertion
timed out after 30s; the git log for the path contained only:

```text
[UPDATE] v1/configmaps/probe-a
e2e-reconcile: synced 42 v1/configmaps@3814 to signing-overlap-a
e2e-reconcile: synced 8 v1/secrets@3813 to signing-overlap-a
```

The controller emitted exactly **one** per-event commit for `probe-a`, and it was
classified `[UPDATE]`:

```text
worker-manager.branch-worker  Opening commit window  resource=v1/configmaps/probe-a  author=system:admin
git commit created  messageKind=event  events=1  message="[UPDATE] v1/configmaps/probe-a"
```

The create-then-label sequence produced **no distinct `[CREATE]`** — the create
moment was not emitted as its own commit.

### 1.3 Failure 2 — a CREATE was absorbed into the unattributed baseline

The spec impersonates an OIDC user carrying `user.extra` claims
(`display-name: "Simon Koudijs"`, `email: something@configbutler.ai`), creates a
ConfigMap, and expects the commit author to be that identity. It timed out after
180s; the commit author was the **default committer**:

```text
Expected <string>: GitOps Reverser <noreply@configbutler.ai>
to equal  <string>: Simon Koudijs <something@configbutler.ai>
```

The controller log for the test namespace `1782476630-test-commit-author` shows the
ConfigMap landing inside the initial watch replay and being committed as a baseline
resync, not as a live per-event commit:

```text
12:27:45  watch-first target watch set reconciled  watchCount=1
12:27:46  target watch replay complete  gvr=/v1,configmaps  count=2  resourceVersion=3899
12:27:46  Handling resync request  resources=2  revision=3899
12:27:46  First commit written to local repository  commits=1
12:27:46  Resync request applied  committed=true  created=2  updated=0  deleted=0
```

The impersonated create was logged at `12:27:46.002` — the same second the replay
snapshot (`count=2`, rv `3899`) was taken. The baseline resync ships with the
GitProvider's committer identity and never consults the attribution index
([attribution_index.go:223 `LookupAuthor`](../../../internal/queue/attribution_index.go#L223)),
so the OIDC identity could not reach the commit. No subsequent per-event
`[CREATE] .../commit-author-oidc-cm…` commit exists.

### 1.4 The transport mechanism, reproduced deterministically

The product's watch-first replay
([target_watch.go:259-264](../../../internal/watch/target_watch.go#L259-L264))
opens a watch with:

```go
SendInitialEvents:    ptr.To(true),
ResourceVersionMatch: metav1.ResourceVersionMatchNotOlderThan,
AllowWatchBookmarks:  true,
```

It buffers every event **before** the `k8s.io/initial-events-end` BOOKMARK into a
baseline replay and applies it as one resync; only events **after** the bookmark
stream as attributable per-event commits. The boundary lives entirely in
`internal/watch/target_watch.go`: `handleTargetWatchSessionEvent`
([target_watch.go:370](../../../internal/watch/target_watch.go#L370)) dispatches on a
`replaying` flag — while replaying, `foldTargetReplayEvent`
([target_watch.go:401-437](../../../internal/watch/target_watch.go#L401-L437)) folds
`ADDED`/`MODIFIED` into the baseline slice until the `initial-events-end` bookmark
(`metav1.InitialEventsAnnotationKey`), at which point it flips to live and the resync
is enqueued; thereafter `routeLiveTargetWatchEvent`
([target_watch.go:533](../../../internal/watch/target_watch.go#L533)) emits per-event
commits and calls `attachAuthor` → `AuthorResolver.ResolveAuthor`
([author_resolver.go](../../../internal/watch/author_resolver.go)) for attribution.
The baseline resync path has no `attachAuthor` call, which is why a create folded
into it carries the committer identity, never the audit author.

(The "streaming-list watch" comment in
[materializer.go](../../../internal/typeset/materializer.go) describes the *checkpoint*
revision model and is explicitly a *planned, not-yet-built* optimisation — see its
header, Gap 5 — so it is not the owner of this runtime boundary.)

A probe using that exact transport against the live e2e cluster, contrasting an
identical create-then-modify performed before vs. after the watch opens:

| Timing | Events the watch delivered |
|---|---|
| writes **before** the watch opens | a single `ADDED` at the **post-modify** rv (3952), *before* `initial-events-end` — no CREATE, no MODIFIED; the create's rv (3949) is invisible |
| writes **after** `initial-events-end` | two events: `ADDED` (3953) then `MODIFIED` (3954) — a distinct, attributable CREATE |

A `SendInitialEvents` replay collapses an object's whole pre-watch history into a
single synthetic `ADDED` carrying its latest resourceVersion. There is no signal
distinguishing "just created" from "long-existing and modified".

### 1.5 Regression assets added

- A mutationlab `replay` watch-probe mode
  ([watch_probe.go](../../../internal/mutationlab/recorder/watch_probe.go)) that
  captures the full replay window (every initial `ADDED` + the terminating
  `initial-events-end` BOOKMARK) using the product's transport.
- Driver scenario `TestWatchReplayCollapsesCreateThenModify`
  ([watch_transport_test.go](../../../test/mutationlab/e2e/watch_transport_test.go))
  → corpus `configmap/watch-replay-collapse/`.
- A cluster-free unit test `TestWatchProbe_ReplayKeepsCollapsedAdded`
  ([recorder_test.go](../../../internal/mutationlab/recorder/recorder_test.go)).

## 2. Mechanism (analysis)

Both failures are one race, against the `initial-events-end` watermark:

- The product treats the watermark as the boundary between **baseline** (everything
  in the initial-events replay: applied as an unattributed mark-and-sweep resync)
  and **live tail** (everything after: per-event, attributed via the audit index).
- An object whose creation is observed by the replay — because the watch opened at
  or after the create's resourceVersion — is delivered as a single collapsed
  `ADDED` in the baseline. It therefore (a) never appears as a per-event `[CREATE]`
  (Failure 1) and (b) is committed under the committer identity rather than the
  audit-attributed author (Failure 2).
- Win the race — create *after* the watch is live and past the bookmark — and the
  same object is a distinct, attributable per-event CREATE. Which spec loses the
  race on a given run is timing-dependent, which is exactly the observed flake
  signature (different spec red each run).

This is a faithful consequence of the watch-first design, not a defect in the
replay itself: Kubernetes genuinely does not carry per-object history across a
watch that joins late (see [mutation-capture-lab-design.md](../mutation-capture-lab-design.md)
Findings 1 and 6 — the live-watch caveat).

## 3. Framing: a timing problem, not a correctness bug in the stream

The stream does the right thing. Nobody should expect a write issued in the same
instant the watch is being established to be classified as a live, attributed
event — the replay has to draw its baseline somewhere, and a brand-new object that
falls inside that baseline is, by construction, indistinguishable from existing
state.

The real defect is that **callers act before the stream is demonstrably live**. The
e2e specs create their probe/attributed objects and immediately expect per-event,
attributed commits, but nothing they waited on actually guarantees the watch for
that resource type has crossed `initial-events-end` and is tailing live. The
existing `waitForGitTargetSynced` gate (used by the attribution spec) does not
imply per-type live-tailing past the watermark.

## 4. Chosen direction

The fix is an **observable per-type stream-readiness signal, separate from `Ready`**,
that callers wait on before acting when they need live, attributed per-event
behaviour. The full design is in the two companion docs:

- [per-type-streaming-readiness-plan.md](./per-type-streaming-readiness-plan.md) —
  the per-type state model (Replaying / Streaming, plus a Blocked dead-end), where
  the state lives, and the e2e conversion for all specs.
- [streaming-readiness-status-machine-design.md](./streaming-readiness-status-machine-design.md)
  — the concrete status objects, fields, printer columns, the `StreamsReady`
  condition, the Kubernetes-convention naming rationale, and the source-code
  simplification it enables.

The only rationale that needs to live *here* (it is the reason the signal is new
rather than folded into an existing one): **it must be separate from `Ready`.**
Replay for a large watched set can take meaningful time, so gating `Ready` on full
per-type stream readiness would make `Ready` slow and size-dependent and would
conflate "configured and valid" with "caught up and streaming". `Ready` keeps meaning
"admitted and watches established"; stream readiness is a second, finer axis named
`StreamsReady`.

> Superseded: earlier drafts floated an `Initializing / PartiallyLive / Live` axis
> (from [materialization-tail-and-live-readiness-review.md](../../finished/materialization-tail-and-live-readiness-review.md)).
> The companion docs replace it with two happy-path states (`Replaying` → `Streaming`)
> plus a `Blocked` dead-end, surfaced as a single `StreamsReady` condition. Treat the
> `Initializing / PartiallyLive / Live` phrasing as historical context, not an active
> candidate.

## 5. Reproduce / regress

- Live, deterministic: the `replay` mutationlab probe (Section 1.5) — drive a
  create-then-modify before the watch, observe the single collapsed `ADDED` inside
  the replay window; create after the bookmark, observe `ADDED` + `MODIFIED`.
- Corpus row `configmap/watch-replay-collapse/` defends the Kubernetes-side fact on
  version bumps.
- Note: the mutationlab reproduces the **Kubernetes transport determinant**, not the
  product's baseline/attribution handling (the lab deliberately does not run the
  product ingestion). The product-side fix and its proof belong in `internal/watch`
  and the e2e specs that already catch it.
