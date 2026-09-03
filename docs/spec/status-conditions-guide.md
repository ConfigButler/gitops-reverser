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

## What belongs in status, and what is a metric

**Status writes are bounded by configuration changes and health transitions, never by data-plane
throughput.** That is the rule a proposed field has to pass, and it is a rule about the field's
*rate*, not its subject.

Two questions decide it:

- Does the value move when a **user** changes something, or when health flips? That is status.
- Does it move when the **workload** moves, on an event, a commit, or a mirrored object? That is a
  metric, whatever it is called.

A field that fails this is not merely expensive. It is wrong three ways, and only the first is cost:

1. Every status write is an etcd write and a `resourceVersion` bump, which invalidates the cached
   copy held by every watcher of that type, not just ours.
2. It is a **feedback loop**. A status write fires an Update event that the controller's own `For()`
   turns back into a queued request, which is the self-triggering edge the section below exists to
   suppress. A field that moves on every pass defeats that suppression by construction.
3. It destroys the field's own readability. A value that changes constantly cannot be read as a
   statement about convergence, because there is no steady state to compare against.

The rule is already visible in the fields this project ships, and they are the worked examples:

| Field | Moves when | Verdict |
|---|---|---|
| `status.placement.mode`, `.renderRoot`, `.readOnlyBases` | the folder's shape changes | status |
| `status.placement.resolvedAtRevision` | the **resolution** changes, deliberately not on every scan | status |
| `status.streams` | counts that move when a stream's readiness changes | status |
| `status.retention` | counts and a roll-up time that move when a **resync** reports, not per event | status |
| placements, placement refusals | every placed or refused document | metric (`gitopsreverser_placements_total`, `_placement_refusals_total`) |
| a "last reconcile attempt" timestamp | every pass | removed, see below |

`resolvedAtRevision` is the one worth reading twice, because it looks like a timestamp field and is
not one. Re-stamping it on every scan would write status once per commit to the branch, whichever
target caused that commit, so it dates the resolution and not the last scan. A revision older than
the branch head means the layout has been stable, not that nothing has looked.

**The bound is on rate, not on human involvement.** A field written once per deliberate user action
passes: a reconcile-request handshake such as Flux's `lastHandledReconcileAt` is bounded by how often
someone pokes the annotation, which is not a throughput. Rejecting that kind of field is stricter
than this rule requires.

**Where this bites in review:** the durable per-type record. A map in status keyed by watched type
grows with the number of types *and* is rewritten by each scan, so it fails on both halves at once.
Publish the per-type facts as metric labels and keep status to the one resolved answer for the
object.

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
describes a generation that is no longer current, and publishing it would publish a stale
observation. Convergence is deferred, not guaranteed to be immediate. A conflict caused by a *spec*
edit re-enqueues at once; one caused by a metadata- or status-only write does **not**, because every
`For()` in this project carries `GenerationChangedPredicate`. That case converges on the next
dependency event or the requeue cadence `commitRule`/`requeueFor` already picked from the same
verdict — which is why that cadence, and not a retry loop here, is the backstop.

### Reason vocabulary

Generic reasons are aliases of [`github.com/fluxcd/pkg/apis/meta`](https://pkg.go.dev/github.com/fluxcd/pkg/apis/meta)
— `Succeeded`, `Failed`, `Progressing`, `DependencyNotReady` — a module this project already depends
on. Sharing the vocabulary means one alerting rule works across every kind here *and* across every
Flux kind in the same cluster. A reason that restates the condition type (`Ready=True, reason=Ready`)
answers nothing and is not used.

Domain reasons stay this project's own — `UnsupportedContent`, `WriteBoundaryRefused`,
`IgnoreShadowsManagedPath`, `MultipleSourceNamespaces` — because they carry information a generic
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
    TypeLayoutResolved       = "LayoutResolved"

    // WatchRule only: whether every rule item's RESOLVED source-namespace scope is authorized.
    TypeSourceNamespaceAuthorized = "SourceNamespaceAuthorized"
)
```

Canonical reads:

- fully mirrored: `Ready=True`, `Reconciling=False`, `Stalled=False`, reason `Succeeded`
- **nothing to mirror** — no WatchRule has claimed the GitTarget yet, or its rules were deleted:
  `Ready=True`, `Reconciling=False`, `Stalled=False`. "I have nothing to mirror" is a *converged*
  state, not a pending one; `status.streams.summary` keeps showing `0/0` and `StreamsRunning` keeps
  reason `NoResolvedTypes`, so the zero stays visible without being reported as a failure to
  converge. (Flux's Kustomization with an empty path reports the same.)
- initial replay or recheck: `Ready=False`, `Reconciling=True`, `Stalled=False`
- refused Git path, invalid provider, RBAC denial, or broken encryption: `Ready=False`, `Reconciling=False`,
  `Stalled=True`
- Git path refusal details live on `GitPathAccepted=False` and `Stalled=True`, reason `UnsupportedContent`
- **suspended**: `Ready=True` with reason `Suspended`. Not writing on request is a configured
  outcome, so nothing goes False for it. `status.placement` keeps updating, because a suspended
  target still scans — a valve that also stopped looking would freeze it at whatever the folder
  looked like when someone panicked. `status.retention` goes ABSENT instead of zero: no resync
  sweeps while suspended, so nothing is counted, and a zero would read as converged
- `LayoutResolved` reports what the last scan resolved about the folder's shape, with
  `status.placement` carrying the detail — `mode` (`Plain`, `KustomizeRoot`, `KustomizeOverlay`),
  the governing `renderRoot`, and the `readOnlyBases` the folder renders but may not write to. It
  is an OBSERVATION and writes no part of the trio.
  `SingleKustomization` and `None` are both `True`: a folder with no kustomization is the ordinary
  case, and reporting the ordinary case as `False` is how a condition gets trained out of a
  reader's attention. Only `Ambiguous` is `False`, and the write refusal it implies is carried by
  `GitPathAccepted=False` with reason `AmbiguousLayout` rather than by a second gate
- WatchRule and ClusterWatchRule carry target dependency health in `GitTargetReady`
- WatchRule carries source-namespace authorization in `SourceNamespaceAuthorized`, a positive
  state-style condition set even for legacy own-namespace rules (reason `LegacySourceNamespace`), so
  the effective authorization is always visible and automation has one condition to inspect. It is an
  additional prerequisite of `Ready`, and is deliberately kept out of `GitTargetReady`, which stays
  the referenced target's own health.

  Its values are `True` and `False`, plus an `Unknown` that means only "not evaluated yet, because
  an earlier gate blocked this reconcile" (reason `Progressing`). `False` is a **refusal**:
  terminal, `Stalled=True`, stream stopped, reason `SourceNamespaceNotAllowed`.

  There is no "cannot say yet" verdict and no retained-scope state, and the reason that is worth
  recording rather than merely deleting: this condition used to have both, because the gate's
  selector half read `Namespace` labels in ANOTHER cluster, where the read could be still syncing,
  unreachable, or permanently `Forbidden`. That forced a three-valued verdict, and it forced the
  asymmetry between *establishing* a grant (fail closed: do not start the stream) and *maintaining*
  one (never narrow to the empty set, because a narrowed scope is the input to a resync sweep and
  would delete a tenant's Git content over a transient outage). Every input is now a control-plane
  object the reconcile already holds, so an item that is not denied is decided, and there is nothing
  left to retain a scope through.

  **It is one condition per object, aggregated over every `spec.rules[]` item.** The precedence is
  stated rather than derived, because two implementations of "worst wins" would otherwise disagree
  about a mixed rule. First match wins:

  1. any item **denied** → `False` / `SourceNamespaceNotAllowed` / `Stalled=True`
  2. every item admitted, at least one naming a namespace other than the rule's own → `True` /
     `SourceNamespaceAllowed`
  3. every item on its own namespace → `True` / `LegacySourceNamespace`

  A **denied explicit name refuses the whole rule**; the item is never trimmed away so the rest can
  run, because mirroring two of the three namespaces a rule asked for is worse than a loud failure.
  Messages therefore name the deciding item by index *and* by its resources and requested namespace —
  an index alone goes stale the moment somebody reorders the list while reading the message.

  There is no `True` reason for an empty resolved scope any more. `NoAdmittedSourceNamespaces`
  existed so a `sourceNamespace: "*"` against a policy admitting nothing could not look healthy while
  mirroring nothing; `"*"` is now one cluster-wide watch, which cannot resolve to an empty set.

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

- waiting for the close delay: `Reconciling=True` reason `WaitingForCloseDelay`, `AuthorAttributed`
  settled, `Pushed=Unknown` → kstatus InProgress
- committed: `Ready=True`, `Pushed=True`, `Stalled=False`, reason `Committed` → kstatus Current
- benign no-commit (nothing to save / already present / foreign open window): `Ready=True`, `Pushed=False`,
  `Stalled=False`, with the specific reason on `Ready` → kstatus Current (a correct, non-error outcome)
- failed finalize: `Ready=False`, `Stalled=True`, reason `FinalizeFailed` → kstatus Failed

`AuthorAttributed=True` (`AttributedFromAdmission`) means the command submitter was captured. `False`
(`CommitterFallback`) means capture ran but no admission author record exists; `False`
(`AuthorCaptureDisabled`) means capture is disabled. Both claim no actor and can attach only to an unnamed
window. That condition does not itself determine the Git author: the attached watch window is
configured-author (the committer) when watch attribution is disabled, or explicitly unresolved when watch
attribution ran but could not name an actor.
