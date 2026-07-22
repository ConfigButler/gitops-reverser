# KRM Status & Conditions Best Practices

> **spec** — current behaviour. The code depends on this document; change one, change the other. Index: [`../INDEX.md`](../INDEX.md)

Reference: [Superorbital — "Status and Conditions: Explained!"](https://superorbital.io/blog/status-and-conditions/)

## What status is

Controllers write it, users don't. It's a separate subresource (`/status`) — `kubectl edit` won't
persist changes to it, and RBAC for it is granted separately from the main resource:

```yaml
- resources: ["gitdestinations/status"]
  verbs: ["get", "patch", "update"]
```

## Conditions

A list treated as a map keyed by `type`. Don't append duplicates — update the existing entry.
`kubectl wait --for=condition=Ready=true` works by watching this array until the matching type
hits `"True"`.

Only update `lastTransitionTime` when `status` actually changes — not on every reconcile. Update the
existing entry **in place**: a setter that removes and re-appends reorders the list on every touch,
which is a diff in `kubectl get -o yaml` and, for a cluster that mirrors its own config objects into
Git through this operator, a stream of commits that reorder conditions and change nothing.

### One writer for the trio

`Ready`, `Reconciling` and `Stalled` are derived **together, once, at the end of a reconcile**, from
a precedence stated in one place. Gates do not set them; gates *contribute* to an accumulator
(`internal/controller/readiness.go`) and the trio falls out of the worst contribution.

This is not a style preference. When each gate wrote the trio itself, the trio said whatever the last
gate said — so a GitTarget whose Git path had been refused (terminal) was then handed to the
source/provider projection, which stamped `Stalled=False, Reconciling=True` over the refusal because
a provider happened to be mid-check. To a human reading `Ready` little changed. To kstatus — which
never reads `Ready`, only the abnormal-true pair — the object flipped from `Failed` to `InProgress`,
so `kubectl wait` and every CI gate built on it waited out its timeout on an object that was never
going to converge.

### Status writes are suppressed when nothing changed

`reconcileStatus.commit` computes the difference between the status as read and the status as
written, and sends **nothing** when they are equal. A status write bumps `resourceVersion`, which
fires an Update watch event, which the controller's own `For()` turns straight back into a queued
request — so an unconditional write makes every reconcile cost roughly two. Status fields that move
on every pass (a "last reconcile attempt" timestamp) defeat this by construction and were removed;
`lastTransitionTime` plus `controller_runtime_reconcile_total` answer the same question without
making every object mutable on read.

The write is a status **patch with optimistic concurrency**, and a conflict is dropped rather than
retried: a conflict means the object moved under this reconcile, so the status just computed
describes a generation that is no longer current, and the write that beat us has already enqueued a
fresh pass.

### Reason vocabulary

Generic reasons are aliases of [`github.com/fluxcd/pkg/apis/meta`](https://pkg.go.dev/github.com/fluxcd/pkg/apis/meta)
— `Succeeded`, `Failed`, `Progressing`, `DependencyNotReady` — a module this project already depends
on. Sharing the vocabulary means one alerting rule works across every kind here *and* across every
Flux kind in the same cluster. A reason that restates the condition type (`Ready=True, reason=Ready`)
answers nothing and is not used.

Domain reasons stay this project's own — `UnsupportedContent`, `WriteBoundaryRefused`,
`IgnoreShadowsManagedPath`, `NoAdmittedSourceNamespaces` — because they carry information a generic
reason cannot. Declaring domain reasons is exactly what the upstream vocabulary asks projects to do.

### One deliberate deviation: the abnormal-true pair is written when False

The Kubernetes API conventions say an abnormal-true condition SHOULD only be present when `True`, and
Flux deletes `Reconciling`/`Stalled` rather than writing them `False`. This project writes them
either way. kstatus tolerates it (it tests for `== True` and ignores everything else), and both
`kubectl wait --for=condition=Stalled=false` and this repo's e2e suite read the explicit `False`. A
condition that vanishes is harder to reason about than one that reads `False`. This is the only place
the project knowingly departs from the conventions, and it is recorded here so the code and this
document cannot silently disagree.

## Best practices

1. **Always have a summary condition.** `Ready` for long-running objects, `Succeeded` for
   bounded ones. This is what operators and scripts will `kubectl wait` on.

2. **Consistent polarity, except for kstatus.** Domain conditions should be positive
   (`True` = healthy). `Reconciling` and `Stalled` are the sanctioned kstatus exceptions:
   they are abnormal-true because generic tooling expects that vocabulary.

3. **Names describe states, not transitions.** `ScaledOut` not `Scaling`. This way `True` =
   success, `False` = failed, `Unknown` = in progress — all unambiguous.

4. **Don't duplicate between conditions and status fields.** A string field that mirrors a
   condition is redundant noise. Pick one representation. The one exception in this project is
   `status.streams.summary` (`"3/4"`), which restates `ready` and `total` beside it: a printer
   column can read one JSONPath, not format two. Its field doc says so, so the next reader does not
   "clean it up".

5. **Emit an Event on every persisted `Ready` transition.** Conditions say what is true now; Events
   say what happened. A transient failure that clears before anyone looks is invisible without them,
   and an Event-driven alerting pipeline has nothing to route. They are emitted after the status
   patch lands, not beside each condition write, so a reconcile that writes `Ready` twice (a
   placeholder, then the real outcome) announces only the value that was actually stored.

## Applied to this project

GitTarget, WatchRule, and ClusterWatchRule use the kstatus trio as the generic layer:

```go
const (
    TypeReady       = "Ready"       // True when the latest observed generation is fully satisfied
    TypeReconciling = "Reconciling" // True while coarse progress is in flight
    TypeStalled     = "Stalled"     // True when a human-fixable block prevents progress
)
```

GitTarget adds domain conditions that explain the summary:

```go
const (
    TypeValidated            = "Validated"
    TypeEncryptionConfigured = "EncryptionConfigured"
    TypeStreamsRunning       = "StreamsRunning"
    TypeGitPathAccepted       = "GitPathAccepted"
    TypeGitTargetReady       = "GitTargetReady"

    // WatchRule only: whether every rule item's RESOLVED source-namespace scope is authorized.
    TypeSourceNamespaceAuthorized = "SourceNamespaceAuthorized"
)
```

Canonical reads:

* fully mirrored: `Ready=True`, `Reconciling=False`, `Stalled=False`, reason `Succeeded`
* **nothing to mirror** — no WatchRule has claimed the GitTarget yet, or its rules were deleted:
  `Ready=True`, `Reconciling=False`, `Stalled=False`. "I have nothing to mirror" is a *converged*
  state, not a pending one; `status.streams.summary` keeps showing `0/0` and `StreamsRunning` keeps
  reason `NoResolvedTypes`, so the zero stays visible without being reported as a failure to
  converge. (Flux's Kustomization with an empty path reports the same.)
* initial replay or recheck: `Ready=False`, `Reconciling=True`, `Stalled=False`
* refused Git path, invalid provider, RBAC denial, or broken encryption: `Ready=False`, `Reconciling=False`,
  `Stalled=True`
* Git path refusal details live on `GitPathAccepted=False` and `Stalled=True`, reason `UnsupportedContent`
* WatchRule and ClusterWatchRule carry target dependency health in `GitTargetReady`
* WatchRule carries source-namespace authorization in `SourceNamespaceAuthorized`, a positive
  state-style condition set even for legacy own-namespace rules (reason `LegacySourceNamespace`), so
  the effective authorization is always visible and automation has one condition to inspect. It is an
  additional prerequisite of `Ready`, and is deliberately kept out of `GitTargetReady`, which stays
  the referenced target's own health.

  Its three values are not interchangeable. `False` is a **refusal** — terminal, `Stalled=True`,
  stream stopped — with reason `SourceNamespaceNotAllowed`, or `SourceNamespacePolicyUnavailable`
  when a selector policy is permanently unevaluatable *and* no scope was ever resolved for the rule.
  `Unknown` is "cannot say yet": either the answer is still being established
  (`CheckingSourceNamespacePolicy`), or a rule that already holds a resolved scope has lost the
  ability to re-evaluate its policy and is **retaining** that scope
  (`SourceNamespacePolicyUnavailable`, `Stalled=False`, still mirroring).

  That asymmetry is deliberate. While *establishing* a grant, failing closed means "do not start the
  stream", which is accurate and actionable. While *maintaining* one, failing closed would mean
  "narrow to nothing" — and a narrowed scope is the input to a resync sweep, so it would delete a
  tenant's Git content over a transient outage. An unevaluatable policy therefore never produces a
  resolved namespace set: not the empty one, and not the full one.

  **It is one condition per object, aggregated over every `spec.rules[]` item.** The precedence is
  stated rather than derived, because two implementations of "worst wins" would otherwise disagree
  about a mixed rule. First match wins:

  1. any item **denied** → `False` / `SourceNamespaceNotAllowed` / `Stalled=True`
  2. any item **permanently unevaluatable** while establishing → `False` /
     `SourceNamespacePolicyUnavailable` / `Stalled=True`
  3. any item retaining a scope it can no longer re-evaluate → `Unknown` /
     `SourceNamespacePolicyUnavailable` / `Stalled=False`
  4. any item **still resolving** → `Unknown` / `CheckingSourceNamespacePolicy`
  5. every item admitted, at least one naming a namespace other than the rule's own → `True` /
     `SourceNamespaceAllowed`
  6. every item omitted → `True` / `LegacySourceNamespace`

  A **denied explicit name refuses the whole rule**; the item is never trimmed away so the rest can
  run, because mirroring two of the three namespaces a rule asked for is worse than a loud failure.
  Messages therefore name the deciding item by index *and* by its resources and requested namespace —
  an index alone goes stale the moment somebody reorders the list while reading the message.

  One more `True` reason exists so a no-op cannot look healthy: `NoAdmittedSourceNamespaces`, when
  every item is authorized but the resolved scope is **empty** (a `sourceNamespace: "*"` against a
  policy that currently admits nothing). The rule is not stalled — nothing is wrong with it — but it
  mirrors nothing, and `Ready=True` with no explanation would hide that. The existing
  `StreamsRunning` and `ResourcesResolved` surfaces show the zero.

### CommitRequest (one-shot)

CommitRequest runs once to a terminal outcome. Best-practice 1 above would suggest a `Succeeded` summary,
but it deliberately keeps `Ready` so every CRD in this project shares one summary type and the kstatus
trio; the bounded "did the work actually happen" signal lives on the `Pushed` domain condition instead.

```go
const (
    TypeReady            = "Ready"            // True at a non-error terminal outcome
    TypeReconciling      = "Reconciling"      // True while the close delay/finalize is in progress
    TypeStalled          = "Stalled"          // True when the finalize failed (kstatus Failed)
    TypeAuthorAttributed = "AuthorAttributed" // Whether admission captured the command submitter
    TypePushed           = "Pushed"           // True once the commit is in the remote repository
)
```

A request has one progress wait: its optional `closeDelaySeconds` collect window, followed by finalization
and push. `AuthorAttributed` settles at first sight because command authorship is captured synchronously
at admission; it never has an `Unknown` or audit-wait state.

Canonical reads:

* waiting for the close delay: `Reconciling=True` reason `WaitingForCloseDelay`, `AuthorAttributed`
  settled, `Pushed=Unknown` → kstatus InProgress
* committed: `Ready=True`, `Pushed=True`, `Stalled=False`, reason `Committed` → kstatus Current
* benign no-commit (nothing to save / already present / foreign open window): `Ready=True`, `Pushed=False`,
  `Stalled=False`, with the specific reason on `Ready` → kstatus Current (a correct, non-error outcome)
* failed finalize: `Ready=False`, `Stalled=True`, reason `FinalizeFailed` → kstatus Failed

`AuthorAttributed=True` (`AttributedFromAdmission`) means the command submitter was captured. `False`
(`CommitterFallback`) means capture ran but no admission author record exists; `False`
(`AuthorCaptureDisabled`) means capture is disabled. Both claim no actor and can attach only to an unnamed
window. That condition does not itself determine the Git author: the attached watch window is
configured-author (the committer) when watch attribution is disabled, or explicitly unresolved when watch
attribution ran but could not name an actor.
