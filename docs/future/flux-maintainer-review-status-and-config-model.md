# Review: the configuration model and status implementation, read as a Flux maintainer

> Status: external review — findings open, nothing here binds until scheduled.
> Date: 2026-07-21
> Reviewed at: branch `feat/gittarget-prune-mode-pr5`, commit `f37a7ba`.
>
> **Scheduled and done so far:** F12's *enum casing* only — `PruneMode` is now
> `Never`/`OnEvent`/`Always`, taken before the release because it was the last moment it was free.
> Every other finding, including the rest of F12, is still open.
> Stance: reviewed as if this API were proposed for the GitOps Toolkit, with Flux's own
> source (`external-sources/flux/`) and kstatus (`sigs.k8s.io/cli-utils/pkg/kstatus`) as ground
> truth rather than recollection.

## What was read

- The API surface: `api/v1alpha3/*.go` (all six kinds plus `NamespaceMatcher`, `PrunePolicy`).
- The status implementations: `internal/controller/{gittarget,watchrule,clusterwatchrule,gitprovider,clusterprovider,commitrequest}_controller.go`, `condition_helper.go`, `stream_status.go`, `gittarget_dependency_status.go`, `gittarget_source_cluster.go`, `internal/watch/stream_readiness.go`.
- `docs/configuration.md`, `docs/spec/status-conditions-guide.md`, `docs/spec/where-validation-lives.md`, `docs/design/reconcile-triggering.md`.
- Flux as ground truth: `external-sources/flux/pkg/apis/meta`, `external-sources/flux/pkg/runtime/{conditions,patch}`, `external-sources/flux/pkg/apis/acl`, `external-sources/flux/flux2/rfcs/`, `external-sources/flux/flux-operator/api/v1`.
- kstatus itself: `sigs.k8s.io/cli-utils/pkg/kstatus/status` from the module cache.

Read-only review: no builds, no tests, no edits to the tree.

---

## 1. The standard

It is **kstatus** — `sigs.k8s.io/cli-utils/pkg/kstatus`, from Kubernetes SIG-CLI. It is what
`kubectl apply --wait`, kpt, Config Sync, Argo's health model (in spirit), and the Flux CLI all
lean on. Its upstream is the **Kubernetes API conventions**, section *"typical status properties"*,
which defines the abnormal-true polarity rule. Flux codifies both in
`fluxcd/pkg/apis/meta` — `ReadyCondition` / `StalledCondition` / `ReconcilingCondition`
(`external-sources/flux/pkg/apis/meta/conditions.go:43-59`) — and the flux-operator boils
readiness down to one CEL expression
(`external-sources/flux/flux-operator/api/v1/common_types.go:23`):

```text
status.conditions.filter(c, c.type == 'Ready').all(c, c.status == 'True' && c.observedGeneration == metadata.generation)
```

**The precise rules kstatus enforces** (verified by reading
`cli-utils/pkg/kstatus/status/generic.go`, not from memory):

1. `metadata.deletionTimestamp` set → `Terminating`.
2. `status.observedGeneration != metadata.generation` → `InProgress`.
3. any condition `Reconciling` **with status True** → `InProgress`.
4. any condition `Stalled` **with status True** → `Failed`.
5. otherwise → `Current`.

Two consequences that matter enormously here, and that are easy to get wrong:

- kstatus **never reads `Ready`** for a custom resource. `Ready=False` with `Reconciling` and
  `Stalled` both False reads as **`Current`** — "done, healthy". The `Ready` condition is for
  `kubectl wait` and humans; the trio is for machines. They must never disagree.
- kstatus only reacts to those conditions **when True**. That is why the convention says they
  MUST NOT be present when False.

### Verdict on compatibility

**This project is on the standard, deliberately and knowledgeably — closer than most projects at
this stage.** `docs/spec/status-conditions-guide.md` states the contract correctly, and
`internal/controller/gittarget_kstatus_test.go` asserts it against the *real* kstatus library
rather than a hand-rolled reimplementation. That is mergeable work.

What is not yet mergeable is the gap between that stated contract and what the reconcilers
actually emit. Three defects (F1–F3) make the trio lie in states production will reach, and the
tests do not catch them because they assert hand-built condition sets rather than the output of a
reconcile.

| Area | Grade | Note |
|---|---|---|
| Condition types (`Ready`/`Reconciling`/`Stalled`) | **A** | Correct vocabulary, correct meanings, documented. |
| `observedGeneration` on object **and** per-condition | **A** | Passes the flux-operator CEL health expr as written. |
| `+listType=map` / `+listMapKey=type` on conditions | **A** | Correct on all six kinds. SSA-safe. |
| kstatus **conformance tests** | **A–** | Real library, real `Compute()`. Fixtures are synthetic (see F1/F2). |
| kstatus **behaviour under real reconciles** | **D** | F1 masks `Failed`; F2 never reaches `Current`. |
| Abnormal-true polarity (MUST NOT be present when False) | **C** | Always written as `False`. Tolerated by kstatus, violates the contract. |
| Status write discipline (no-op suppression) | **D** | Unconditional writes + always-moving timestamps (F3). |
| Reason vocabulary | **C** | Ad-hoc; `Reason == Type` in several places. |
| Flux object contract (`suspend`, `interval`, `requestedAt`, Events) — see layer 3 below | **F** | None of it present. Known — `docs/design/reconcile-triggering.md`. |
| `no phase string` discipline | **A** | Conditions only, everywhere. Correct. |

### Three different things get conflated below — only one of them is a standard

This review keeps saying "the GitOps Toolkit object contract". That is shorthand, not an official
name, and it is worth separating the three layers because they carry very different weight.

**1. The Kubernetes API conventions — normative, upstream.**
`kubernetes/community`, `contributors/devel/sig-architecture/api-conventions.md`, section *"typical
status properties"*. This is where abnormal-true polarity comes from, where "conditions are a map
keyed by type" comes from, and where the UpperCamelCase enum rule (F12) comes from. Anything that
calls itself a Kubernetes API is measured against this. Flux's own condition docs link to it by
URL (`external-sources/flux/pkg/apis/meta/conditions.go:49-50, 57-58`).

**2. kstatus — a real, named, vendor-neutral convention.**
`sigs.k8s.io/cli-utils/pkg/kstatus`, owned by Kubernetes SIG-CLI. It is a *library plus a
convention*: implement the trio the way it expects and any kstatus consumer can compute your
object's status without knowing what your CRD is. Consumers include cli-utils' own applier,
`kpt live apply`, and the Config Sync / Nomos family built on cli-utils. Flux designs to it
explicitly and says so in the comment above its condition constants.

Worth knowing what it is *not*: **Argo CD does not use kstatus.** Its health model
(`gitops-engine`, built-in checks plus Lua) is a separate, older implementation of the same idea.
So conforming here buys interoperability with the Flux/kpt/cli-utils side of the ecosystem, not
with Argo. That is still the larger side for anything condition-driven, and it is the side this
project's users already live on.

**3. The Flux object contract — not a published cross-vendor standard, but real, importable code.**
There is no RFC and no spec document for it. What exists instead is
**`github.com/fluxcd/pkg/apis/meta`** — its own Go module, Apache-2.0, versioned independently of
the controllers, which this repo *already pins at `v1.31.0`* (`go.mod:9`, for
`KubeConfigReference`). It is written in RFC-2119 language and is deliberately the shared artifact
rather than a doc:

| Codified in `pkg/apis/meta` | Where |
|---|---|
| `ReadyCondition` / `StalledCondition` / `ReconcilingCondition` / `HealthyCondition` | `conditions.go:43-64` |
| Generic reasons: `Succeeded`, `Failed`, `Progressing`, `ProgressingWithRetry`, `Suspended`, `DependencyNotReady`, `InvalidPath`, `InvalidURL` | `conditions.go:80-115` |
| `ReconcileRequestAnnotation = "reconcile.fluxcd.io/requestedAt"` — *"any change in value SHOULD trigger a reconciliation"* | `annotations.go:23` |
| `ForceRequestAnnotation = "reconcile.fluxcd.io/forceAt"` — explicitly *"used to standardize the mechanism across controllers"* | `annotations.go:32` |
| `ReconcileRequestStatus` — an **embeddable struct** carrying `lastHandledReconcileAt`, plus `StatusWithHandledReconcileRequest` / `ObjectWithAnnotationRequests` interfaces | `annotations.go:49-133` |
| `LocalObjectReference`, `NamespacedObjectReference`, `NamespacedObjectKindReference`, `SecretKeyReference`, `KubeConfigReference` | `reference_types.go` |
| `AccessDeniedCondition` / `AccessDeniedReason`, `AccessFrom` | sibling module `pkg/apis/acl` |

**What is deliberately *not* in that module: `spec.suspend` and `spec.interval`.** `meta` ships
`SuspendedReason` but no `Suspend` field — the fields themselves are hand-repeated in every fluxcd
controller's own API types. So "adopt the contract" means *import the type* for conditions,
reasons, annotations and references, but *copy the field shape* for suspend/interval. F6 is
therefore a convention-matching argument, not a dependency argument.

**Who adheres.** Verifiable from this checkout: every fluxcd controller (source-, kustomize-,
helm-, notification-, image-\*) via `flux2`; and ControlPlane's **flux-operator** — a different
vendor under a different licence (AGPL) — which adheres and then extends it with its own
`FluxObject` interface and a CEL readiness expression
(`external-sources/flux/flux-operator/api/v1/common_types.go:23, 44-64`). That second one is the
interesting data point: an independent project chose to implement the same contract rather than
invent one, because it is what makes `flux`-shaped tooling work against non-Flux kinds.

**Why this matters for F6/F7 specifically.** The argument is interop, not compliance. Nobody will
fail an audit for lacking `spec.suspend`. But a platform team that already runs Flux has muscle
memory — `suspend` to pause, annotate `requestedAt` to force, `kubectl describe` to see what
happened, notification-controller to route failures — and every one of those reflexes currently
returns nothing here. Meanwhile the marginal cost is close to zero: the module is already a
dependency, so adopting the condition types, reason constants and `ReconcileRequestStatus` is an
import and a struct embed, not a new supply-chain decision.

---

## 2. Findings

### F1 — `downgradeReady` erases a terminal `Stalled`, downgrading kstatus `Failed` → `InProgress` (High)

`internal/controller/gittarget_controller.go:240` runs `applyDataPlaneConditions`, which for a
refused Git path sets the correct terminal trio (`Ready=False`, `Reconciling=False`,
`Stalled=True`, reason `UnsupportedContent`) — `gittarget_controller.go:526-543`.

Then `internal/controller/gittarget_controller.go:258` runs `projectSourceAndProvider`, which on
any source/provider imperfection calls `downgradeReady`
(`internal/controller/gittarget_source_cluster.go:224-233`):

```go
r.setCondition(target, GitTargetConditionReady, readyStatus, reason, message)
r.setCondition(target, GitTargetConditionReconciling, metav1.ConditionTrue, reason, message)
r.setCondition(target, GitTargetConditionStalled, metav1.ConditionFalse, ReasonProgressing, ...)
```

It unconditionally stamps `Stalled=False, Reconciling=True`, wiping the stall set 18 lines
earlier. The doc comment says it "only ever DOWNGRADES Ready" — true of `Ready`, but for
**kstatus** it *upgrades* the object from `Failed` to `InProgress`.

**Trigger, most likely path:** a remote-source GitTarget before first discovery has
`SourceClusterReachable=Unknown` (`gittarget_controller.go:249-255`), which hits the
`reachStatus == metav1.ConditionUnknown` branch (`gittarget_source_cluster.go:212`). Any
GitProvider blip does it too. So: a GitTarget with unsupported kustomize content in its folder, or
`RenderMatchesLive=False`, reports "still working on it, please wait" forever to every kstatus
consumer, instead of failing fast. Nothing in the folder is being written, and nothing will be.
`GitPathAccepted=False` is still there for a human who goes looking; `kubectl wait` and any
kstatus-driven CI gate hangs to timeout.

This is exactly why Flux computes the summary **once, at the end, from a declared precedence
order** rather than by successive mutation — the `summarize` pattern over `conditions.Set` with
`patch.WithOwnedConditions` (`external-sources/flux/pkg/runtime/patch/options.go:67-72`).

**Fix.** Collect `(status, reason, message)` candidates from every gate into a list, then derive
the trio once at the end with stated precedence: `Stalled=True` wins over `Reconciling=True` wins
over `Ready=True`. `docs/spec/status-conditions-guide.md:69-73` already states the canonical
reads — make one function the single writer of the trio, and make every gate a *contributor*, not
a *setter*. Add a table-driven test that runs the actual reconcile and feeds the resulting object
to `kstatus.Compute`, including the "path refused **and** provider unready" cell.

---

### F2 — A GitTarget with no WatchRules is never `Current`, and requeues every 10 s forever (High)

`internal/watch/stream_readiness.go:75-77`:

```go
func (s StreamSummary) StreamsRunning() bool { return s.Total > 0 && s.Ready == s.Total }
```

Zero tracked types → `StreamsRunning()==false` → the `!streams.StreamsRunning()` branch at
`gittarget_controller.go:562` → `Ready=False`, `Reconciling=True`, reason `NoResolvedTypes`.
Permanently, because nothing will ever resolve. And because `streamsSettling` is true
(`gittarget_controller.go:232`), the reconcile returns `RequeueAfter: RequeueStreamSettleInterval`
= **10 s** (`constants.go:117`), forever.

That state is not exotic — it is **step 3 of the documented setup flow**
(`docs/configuration.md:29-34`: create GitTarget, *then* create WatchRules), and it is the steady
state of any target whose rules were deleted. A user following the docs has an object that
`kubectl wait --for=condition=Ready` never returns on, and that burns a reconcile plus a status
write every 10 seconds indefinitely.

Empty is not in-progress. Nothing is pending. "I have nothing to mirror" is a **converged**
state — Flux's Kustomization with an empty path is `Ready=True` with a "no objects" message, not
InProgress.

**Fix.** Split "nothing resolved" from "resolving". `Total == 0` should be `Ready=True`,
`Reconciling=False`, `Stalled=False` with reason `NoWatchRules` (or `NoResolvedTypes`) and a
message saying so — kstatus `Current`, honest, and it drops back to the 5 min cadence. Keep
`status.streams.summary: "0/0"` so the zero stays visible. If that feels like hiding a
misconfiguration, that is what an Event or a `Ready` *reason* is for, not a permanent
`Reconciling=True`.

---

### F3 — Unconditional status writes with always-moving timestamps, plus no self-predicate, create a self-triggering reconcile edge (High)

Three things compose badly.

1. **Every reconcile stamps a fresh timestamp.**
   `gittarget_controller.go:129` — `target.Status.LastReconcileTime = metav1.Now()`, unconditional.
   `internal/watch/stream_readiness.go:244` — `StreamSummary{..., ObservedTime: metav1.Now()}`,
   surfaced into `status.streams.observedTime` for GitTarget *and* both rule kinds
   (`gittarget_controller.go:1065-1078`, `stream_status.go:26-40`).

2. **The status write is unconditional and full-object.** `updateStatusWithRetry`
   (`gittarget_controller.go:1081-1113`, and the near-identical copies in
   `watchrule_controller.go:405`, `clusterprovider_controller.go:283`,
   `gitprovider_controller.go:403`) does `latest.Status = target.Status; r.Status().Update(...)`.
   There is no "did anything change?" check — and the timestamps guarantee something always did.

3. **`For()` carries no predicate on GitTarget, WatchRule, or ClusterWatchRule**
   (`gittarget_controller.go:1117-1118`, `watchrule_controller.go:463-464`,
   `clusterwatchrule_controller.go:547`). Status-subresource writes bump `resourceVersion` and
   fire an Update watch event, which `handler.EnqueueRequestForObject` puts straight back on the
   queue — un-rate-limited.

So each reconcile enqueues itself. It is not an unbounded spin — `metav1.Time` serialises at
RFC3339 second precision, so a follow-up reconcile that lands inside the same wall-clock second
produces a byte-identical status and the apiserver no-ops it. The practical shape is therefore:
**every reconcile costs roughly two reconciles and at least one etcd write, degenerating into a
sustained self-sustaining loop whenever a reconcile takes ≥1 s** — plausible here, since
`checkForConflicts` does a cluster-wide `List` of every GitTarget on every pass
(`gittarget_controller.go:793`) on top of several `Get`s and `DeclareForGitTarget`.

Combine with F2 and an idle GitTarget writes to etcd at ~0.1 Hz forever, per object, and wakes
every controller watching GitTargets each time.

`GitProvider` and `ClusterProvider` **do** have self-predicates
(`gitprovider_controller.go:462-465`, `clusterprovider_controller.go:332-335`), so this is an
inconsistency, not a house style. `docs/design/reconcile-triggering.md:44-50` inventories
per-controller predicates but records only the *dependency* edges — the missing `For()` predicate
on GitTarget/WatchRule is not in that table.

**Fix, in order of value:**

- Adopt `fluxcd/pkg/runtime/patch.Helper`. It computes a merge patch of *what actually changed*
  and sets `observedGeneration` "only if there is a change"
  (`external-sources/flux/pkg/runtime/patch/options.go:33-35`). No change → no request → no watch
  event → no loop. A drop-in: `fluxcd/pkg/apis/meta` is already a dependency.
- Failing that: `if !equality.Semantic.DeepEqual(latest.Status, target.Status) { update }`, and
  drop `LastReconcileTime` / `observedTime` from the comparison — or drop the fields.
- Ask whether `status.lastReconcileTime` and `status.streams.observedTime` earn their keep at all.
  Flux deliberately does not carry a "last reconcile attempt" timestamp; a condition's
  `lastTransitionTime` plus `controller_runtime_reconcile_total` answer the same question without
  making every object mutable-on-read. `LastPushTime` is genuinely useful (it records a real
  event) — keep that one.
- Add a `For()` predicate on the three controllers that lack one, e.g.
  `predicate.Or(GenerationChangedPredicate{}, <annotation-changed>)`.

---

### F4 — Abnormal-true polarity: `Reconciling`/`Stalled` are always present, including when False (Medium)

`external-sources/flux/pkg/apis/meta/conditions.go:47-59` states it twice:

> The Condition adheres to an "abnormal-true" polarity pattern, and **MUST only be present on the
> resource if the Condition is True**.

Every write path here emits both unconditionally: `setStalledConditions`
(`gittarget_controller.go:450-457`), `downgradeReady`, `setRuleProgressing`
(`stream_status.go:114-123`), `setReadyConditions` / `setProgressingConditions` on both providers.

kstatus tolerates it — it only tests for `== True` — so this is not a correctness bug. But it is a
contract violation with real costs:

- `kubectl get gittarget -o yaml` carries six condition entries where three would do.
- Any tool that treats *presence* as signal (a fair reading of the convention) misreads it.
- `Reconciling=False, reason=UnsupportedContent, message="Reconciliation is stalled"`
  (`gittarget_controller.go:538-539`) is a condition that says nothing true about reconciling. It
  exists only to be overwritten.

`docs/spec/status-conditions-guide.md:31-32` names the polarity rule correctly and the code does
the opposite. Pick one. Preferred: `DeleteCondition` on the way to False —
`conditions.Delete(obj, meta.StalledCondition)` in Flux terms — and let `Ready` carry the positive
summary.

---

### F5 — `upsertCondition` reorders the condition list on every touch (Medium)

`internal/controller/condition_helper.go:26-44` rebuilds the slice with the target type *removed*
and then appends it at the end. Touch `Ready` and it migrates to the tail; touch it again next
pass and everything else has shuffled.

Effects: gratuitous diffs in `kubectl get -o yaml` and in any GitOps repo that mirrors these
objects (which, given what this product does, is not hypothetical — a GitTarget mirroring
GitTargets would commit condition-reordering noise); a byte-level change even when nothing
semantically changed, which feeds F3; and unstable ordering for humans.

The `LastTransitionTime` handling is *correct* — preserved when `Status` is unchanged
(`condition_helper.go:39-41`), matching `apimeta.SetStatusCondition` and the API conventions.
Flux is actually stricter than the convention here, resetting on Reason/Message change too
(`external-sources/flux/pkg/runtime/conditions/setter.go:44-46`); this repo's is the more
conventional reading and worth keeping.

**Fix.** Use `k8s.io/apimachinery/pkg/api/meta.SetStatusCondition`, which updates in place. Or
adopt Flux's approach and sort deterministically, with `Stalled`, `Reconciling`, `Ready` weighted
to the front for `kubectl` legibility
(`external-sources/flux/pkg/runtime/conditions/setter.go:89-92, 196-218`) — a nice touch worth
stealing regardless.

---

### F6 — No `spec.suspend`, no `spec.interval`, no reconcile-request annotation (Medium)

Nothing in `api/v1alpha3` has `suspend` or `interval` — zero hits. The cadence is a compile-time
constant: `RequeueSteadyInterval = 5 * time.Minute` (`constants.go:113`).

Every Flux object implements all three
(`external-sources/flux/flux-operator/api/v1/common_types.go:44-64`): `GetInterval()`,
`IsDisabled()`, `SetLastHandledReconcileAt()`. They are not decoration:

- **`spec.suspend`** is the only way to say "stop touching this while I fix the repo by hand"
  without deleting the object. For a controller that *writes to a Git repository*, the absence of
  a pause button is the single most surprising gap in this API. Today the only way to stop a
  GitTarget writing is to delete it or its rules — and per the `GitTargetSpec` doc comment, that
  is irreversible for path/branch/provider.
- **`spec.interval`** — `GitProvider` does a real network `ls-remote` against the git host on
  every pass (`gitprovider_controller.go:220`, `checkRemoteConnectivity`), hardcoded at 5 min,
  with no jitter. N providers against github.com from one operator, all re-synchronised into a
  thundering herd after each restart. Flux ships `fluxcd/pkg/runtime/jitter` precisely for this.
  At minimum: jitter the requeue. Better: `spec.interval` per object, since a GitProvider pointing
  at a rate-limited enterprise host and one pointing at a local Gitea do not deserve the same
  cadence.
- **`reconcile.<group>/requestedAt` + `status.lastHandledReconcileAt`** is the universal
  "reconcile now" idiom (`flux reconcile`, `kubectl annotate`, webhook receivers). This is the one
  piece of F6 that is *shared code* rather than a copied field shape: embed
  `meta.ReconcileRequestStatus` and call `meta.ReconcileAnnotationValue`
  (`external-sources/flux/pkg/apis/meta/annotations.go:23-64`) and the semantics — including the
  "any change in value SHOULD trigger" token comparison — come with it. Already identified as F1
  in `docs/design/reconcile-triggering.md`; still unbuilt.

---

### F7 — Zero Kubernetes Events (Medium)

There is no `EventRecorder` anywhere in `internal/controller` or `cmd/main.go` — no `Recorder`,
`Eventf`, or `Event(` call sites.

Consequences: `kubectl describe gittarget` shows no history; a transient push failure that
resolves before anyone looks is invisible; and there is **no integration path with
notification-controller** or any Event-driven alerting, because there is no Event to route.
Metrics say a counter moved; they cannot say *which* GitTarget failed to push and why.

`docs/design/reconcile-triggering.md:222-227` already prescribes the fix ("F4. Conditions **and**
Events, every loop") and cites `fluxcd/pkg/runtime/events`. This ranks above the webhook-receiver
work in the same doc: Events are cheap, and a controller that writes to Git without emitting an
Event on write failure is hard to operate.

---

### F8 — Reason vocabulary is ad-hoc, and in several places `Reason == Type` (Medium)

`gitprovider_controller.go:325`, `clusterprovider_controller.go:226`, `stream_status.go:45`:

```go
r.setCondition(gitProvider, ConditionTypeReady, metav1.ConditionTrue, ConditionTypeReady, message)
//                          ^^ type                                   ^^ reason == "Ready"
const ruleReadyReason = "Ready"
```

`Ready=True, reason=Ready` conveys nothing. A reason answers *why*. Flux's generic set
(`external-sources/flux/pkg/apis/meta/conditions.go:80-115`) is `Succeeded`, `Failed`,
`Progressing`, `ProgressingWithRetry`, `Suspended`, `DependencyNotReady`, `InvalidPath`,
`InvalidURL`, `AccessDenied` (the last from `pkg/apis/acl`) — and it is a *shared vocabulary*, so
one alerting rule works across every kind.

Here it is `OK`, `Ready`, `Checking`, `Resolved`, `Progressing`, `Stalled`, `Validated` scattered
across `constants.go:95-145` and four controllers. `Progressing` and `Stalled` already match Flux.
Suggested:

- Alias the generic ones to `meta.SucceededReason` / `meta.ProgressingReason` /
  `meta.FailedReason` / `meta.DependencyNotReadyReason` — `github.com/fluxcd/pkg/apis/meta` is
  already imported (`clusterprovider_types.go:6`), so it is free.
- Replace `OK` and reason-equals-type with `Succeeded`.
- Keep the excellent domain-specific reasons (`UnsupportedContent`, `IgnoreShadowsManagedPath`,
  `WriteBoundaryRefused`, `NoAdmittedSourceNamespaces`) — those are exactly what the Flux docs
  mean by "declaration of domain common Condition reasons in the API specification is
  RECOMMENDED". Consider promoting them from `internal/controller` constants to exported constants
  in `api/v1alpha3`, so consumers can compile against them.

`GitTargetReasonProviderNotFound` should probably be `meta.DependencyNotReadyReason` or at least
`DependencyNotFound`, for the same cross-kind-alerting reason.

---

### F9 — A stored `ClusterWatchRule` with `scope: Namespaced` may be unable to report its own refusal (Medium — needs verification)

`api/v1alpha3/clusterwatchrule_types.go:130-133` narrows the enum to `Cluster` only, deliberately
keeping the field so a re-apply *fails*. The reasoning in the comment above it is sound.
`DeclaresNamespacedScope()` (`:144`) then refuses a stored value at compile time.

The concern: for CRDs, the apiserver validates the **whole object** against the OpenAPI schema on
**status-subresource** updates too, not just spec updates. If that holds here, the controller
cannot write `Stalled=True` onto an object whose stored `spec.rules[].scope` is `Namespaced` — the
status update is rejected 422, and the one object that most needs to explain itself is the one
that cannot.

**Mitigating factor:** CRD Validation Ratcheting (beta and default-on in 1.30, GA in 1.33) skips
re-validation of *unchanged* fields, which would make this a non-issue on modern clusters. So the
exposure is older clusters, or a cluster with the feature gate off.

Not confirmed by execution. **Worth one envtest:** create a ClusterWatchRule with
`scope: Namespaced` via a client that bypasses the enum (or against an older CRD), then attempt a
status update, on the minimum supported Kubernetes version. If it fails, the fallback is to widen
the enum back and rely solely on the compile-path refusal plus a loud `Stalled` condition —
refusing at admission is nice, but not at the cost of being unable to report the refusal.

---

### F10 — CommitRequest objects accumulate forever, and the controller cannot delete them (Medium)

`CommitRequest` is a one-shot imperative object with an immutable spec
(`commitrequest_types.go:14`). Every "save now" leaves an object in etcd. There is no TTL, no
`ownerReference`, no GC path — and the reconciler's RBAC is
`commitrequests, verbs=get;list;watch` (`commitrequest_controller.go:108`), so it could not delete
them even if it wanted to.

A team using this as an interactive save button generates hundreds per namespace per week.
Nothing reaps them.

The broader Flux-maintainer objection is that **this should be an annotation, not a kind**.
`reconcile.<group>/requestedAt` is the established idiom for "act now", it is free to issue, it
self-GCs by being overwritten, and it needs no CRD. The counter-argument here is accepted: a
CommitRequest carries a verbatim commit message, a collect-delay, and (via the admission webhook)
the submitter's identity, and it reports back a SHA. That is genuinely more than a trigger, and
identity capture at admission is the textbook justification made well in
`docs/spec/where-validation-lives.md:50-56`.

But if it stays a kind, it needs a lifecycle:

- `spec.ttlSecondsAfterFinished` (the Job precedent) or a controller-side "delete terminal
  requests older than N", plus the `delete` verb.
- Or an `ownerReference` to the GitTarget so it is at least cascade-deleted.
- At minimum, document the retention expectation and ship a
  `kubectl delete commitrequests --field-selector` recipe.

---

### F11 — `observedGeneration` can record a generation that was never observed (Low)

`updateStatusWithRetry` re-`Get`s the object and then does `latest.Status = target.Status`
(`gittarget_controller.go:1102`). `target.Status.ObservedGeneration` was set from the generation
read at the *top* of the reconcile (`:128`). If the spec changed in between, the new object is
stamped with an `observedGeneration` equal to a generation never actually processed — kstatus then
reports `Current` for a spec nobody looked at, until the next pass corrects it.

Narrow window, self-correcting, low severity. Flux avoids it structurally by patching with
optimistic concurrency and setting `observedGeneration` from the object being patched
(`patch.WithStatusObservedGeneration`). Another thing that comes free with the patch helper (F3).

---

### F12 — Small API-conventions nits (Low)

- **Enum casing — DONE.** `PruneMode` values were `never` / `onEvent` / `always`
  (`prune_policy.go:17-23`). Kubernetes API conventions call for UpperCamelCase enum values —
  compare `imagePullPolicy: Always|Never|IfNotPresent`, `persistentVolumeReclaimPolicy:
  Retain|Delete`. Flux is itself inconsistent here (`driftDetection.mode: enabled|warn|disabled`),
  so this was in company, but `Never`/`OnEvent`/`Always` is the conventional spelling. This was the
  moment to decide — the field was brand new and unreleased on this branch, and the branch already
  carried two `feat(api)!` commits, so the rename rode along in a release that was breaking anyway;
  after that release it would have been a second breaking change for a cosmetic gain.
  **Resolved on `feat/gittarget-prune-mode-pr5`: the values are now `Never` / `OnEvent` /
  `Always`.** The typed constants (`PruneNever`, `PruneOnEvent`, `PruneAlways`) are unchanged, so
  no Go call site moved; the wire values, the CRD enum and default, and the docs did.
  (Conversely, `OperationType`'s `CREATE`/`UPDATE`/`DELETE` **is** right, because it matches
  `admissionregistration.k8s.io` `rules[].operations` verbatim.)
- **Duplicated representation.** `status.streams.summary` is `fmt.Sprintf("%d/%d", Ready, Total)`
  (`stream_readiness.go:70-72`) over fields that are right there in the same struct.
  `status-conditions-guide.md:37-38` says don't do this. It exists only to feed a printer column —
  a legitimate reason, but worth naming as such in the field doc so the next reader doesn't
  "clean it up".
- **Printer-column sprawl.** GitTarget declares 13 columns, 7 of them default-priority
  (`gittarget_types.go:238-252`). `kubectl get gittargets` will wrap on any normal terminal. Flux
  ships 3–4 (Age, Ready, Status). Push `Provider`, `Branch`, `Path` to `priority=1` and keep
  `Ready`, `Reason`, `Streams`, `Age`.
- **Reference types.** Six near-identical shapes — `GitProviderReference`,
  `ClusterProviderReference`, `LocalTargetReference`, `NamespacedTargetReference`,
  `LocalSecretReference`, `KnownHostsReference` — while already depending on
  `fluxcd/pkg/apis/meta`, which offers `LocalObjectReference`, `NamespacedObjectReference`,
  `NamespacedObjectKindReference`, `SecretKeyReference`. Reusing `meta.KubeConfigReference`
  (`clusterprovider_types.go:69`) clearly paid off. The `Group`+`Kind` enum-with-default pattern is
  defensible (it documents what is accepted and leaves room to widen), but consider embedding
  `meta.LocalObjectReference` for the name half so downstream Go consumers get interoperable types.
- **`metav1.ObjectMeta` json tags are inconsistent** — `omitempty,omitzero` on GitTarget /
  GitProvider / ClusterProvider / CommitRequest, plain `omitempty` on WatchRule
  (`watchrule_types.go:277`) and ClusterWatchRule (`clusterwatchrule_types.go:203`).
- **`GitProviderStatus.Conditions` lacks `+patchStrategy=merge` / `+patchMergeKey=type`**
  (`gitprovider_types.go:143-146`) while the other five kinds have them. Harmless with
  `listType=map`, but inconsistent.

---

## 3. The configuration model itself

Setting status aside — this is the part worth defending as-is, and that deserves saying before the
criticism.

**What is genuinely strong:**

- **`docs/spec/where-validation-lives.md` is better than what Flux has written down.** The
  schema → CEL → reconciler ladder, and specifically the argument that *reconcile-time is the
  stronger gate because admission cannot see a policy tightened after creation*, is correct and is
  the thing most projects get backwards. Flux arrived at the same place empirically; here it is a
  stated rule. The one webhook shipped is justified on exactly the right grounds (identity exists
  only in the `AdmissionRequest`).
- **`NamespaceMatcher`'s absent-vs-declared-vs-empty trichotomy** (`namespace_matcher.go:14-34,
  106-123`). Flux's `acl.AccessFrom` (`external-sources/flux/pkg/apis/acl/`) is a flat
  `namespaceSelectors` list with no way to express "declared and empty", and Flux has been bitten
  by exactly that ambiguity. The `selector: {}` = everything / `{}` = nothing / absent = legacy
  distinction, anchored to `LabelSelectorAsSelector`'s own `Nothing()`/`Everything()` asymmetry, is
  more precise than the incumbent. Rejecting `names: ["*"]` because Kubernetes treats it as a
  literal name is the kind of detail that only comes from having been burned.
- **The two-key delegation for cross-namespace source watching** —
  `ClusterProvider.spec.allowSourceNamespaceOverride` (platform admin) AND
  `GitTarget.spec.allowedSourceNamespaces` (destination owner) — is a materially better answer than
  Flux's. RFC-0001 (`external-sources/flux/flux2/rfcs/0001-authorization/README.md:97-101`)
  documents that Flux controllers simply **do not respect namespace isolation** when dereferencing
  cross-namespace refs, a long-standing multi-tenancy sore spot. Deny-by-default and two-party is
  the right shape.
- **Immutability where identity is at stake.** `providerRef`/`branch`/`path`/`clusterProviderRef`
  immutable via CEL (`gittarget_types.go:45-53`), with the reasoning in the type doc: a folder's
  meaning is constituted by those four. `spec.prune` deliberately mutable
  (`gittarget_types.go:134-139`) because forcing a delete-and-recreate to re-enable convergence
  would destroy the one thing that cannot be rebuilt. Well drawn.
- **The `prune.mode` design.** Separating "the source told me it was deleted" from "a snapshot
  didn't mention it" is the correct decomposition, and `onEvent` as the effective default —
  resolved in code via `EffectiveMode()` rather than relying on CRD defaulting, so a *stored*
  pre-field object is also safe (`prune_policy.go:52-63`) — is careful in the right way. Flux's
  `spec.prune` is a single bool and cannot express the middle mode.
- **`no phase string`, anywhere.** Six kinds, zero `.status.phase`. Rarer than it should be.

**Where to push back on the model:**

- **The missing pause button** (F6) is the biggest hole. This writes to Git.
- **`GitProvider` is doing three jobs**: remote+credentials (Flux: `GitRepository` + `Secret`),
  commit identity/templates/signing (Flux: `ImageUpdateAutomation.spec.git.commit`), and push
  batching policy. Defensible cohesion — they are all "how this repo gets written" — but note the
  consequence: `spec.push.commitWindow` and `spec.commit.message.*` are properties of a *workload*,
  yet they live on the *connection*, so two GitTargets sharing a repo cannot have different
  batching or message templates. If that ever needs to differ per target, the field has to move,
  and moving it is breaking. Worth writing down as a known constraint now.
- **The `GitProvider` (namespaced) / `ClusterProvider` (cluster) asymmetry** is justified well in
  `docs/configuration.md:39-63` and the reasoning holds: a Git destination is a team's write
  boundary, a source cluster is a shared physical identity. It *will* still surprise people, and
  the doc already anticipates the follow-up ("if a platform later needs a shared, platform-owned
  Git destination, that should be a separate cluster-scoped concept"). Keep that paragraph.
- **`clusterProviderRef` defaults to `{name: "default"}` for an object the operator never
  creates.** A GitTarget applied to a fresh cluster is unready with `ClusterProviderNotFound` until
  someone creates it. Deliberate and well defended (`docs/configuration.md:419-436`; the chart
  renders it by default), and the substance is right — silently defaulting to in-cluster
  credentials would bypass the authorization model. One ask: make the `ProviderNotFound` message
  for the literal name `default` say *"the ClusterProvider named 'default' does not exist; the
  operator never creates one — see `clusterProvider.createDefault` in the chart, or commit the
  object"*. A generic "provider not found" for the **defaulted** value is the single most likely
  first-run support ticket.
- **`ClusterWatchRule` is unbounded by `allowedSourceNamespaces` by construction** — correct
  (cluster-scoped objects have no namespace) and clearly documented
  (`clusterwatchrule_types.go:172-177`, `docs/configuration.md:935-938`). The mitigation "give each
  tenant its own ClusterProvider and credential" is the right answer and matches how Flux tells
  people to do hard multi-tenancy. Fine as-is; make sure the security model doc says it in one
  place.
- **`sourceNamespace: "*"` fan-out.** One watch stream per (type × admitted namespace)
  (`watchrule_types.go:132-134`), a cost flagged honestly in the type doc. On a broad policy across
  a big cluster this is the scalability cliff — worth a hard cap with a `Stalled` reason
  (`TooManyStreams`) rather than discovering it as apiserver watch pressure.

---

## 4. Suggested order

**Before the next release** (observable behaviour, cheap):

1. **F1** — one function owns the trio; every gate contributes a candidate, precedence stated once.
   Add a reconcile-output-driven `kstatus.Compute` test covering "refused path + unready provider".
2. **F2** — `Total == 0` is `Ready=True` / `Current`, not perpetual `Reconciling`.
3. **F3** — adopt `fluxcd/pkg/runtime/patch.Helper` (or a `DeepEqual` guard), drop or exclude the
   always-moving timestamps, add `For()` predicates to the three controllers missing one.

**Next** (contract alignment, low risk, high interop value):

1. **F5** — `apimeta.SetStatusCondition`, or Flux's sorted `Set`.
2. **F8** — alias generic reasons to `fluxcd/pkg/apis/meta`; kill `reason == type`; export the
   domain reasons from `api/v1alpha3`.
3. **F4** — delete `Reconciling`/`Stalled` rather than writing them False.
4. **F7** — wire an `EventRecorder`; emit on every terminal outcome and every push failure.

**Then** (API surface — do the breaking ones while still `v1alpha3`):

1. **F6** — `spec.suspend` on GitTarget/WatchRule/ClusterWatchRule/GitProvider; `spec.interval` on
   GitProvider at minimum; jitter the requeue; `reconcile.configbutler.ai/requestedAt` +
   `status.lastHandledReconcileAt`.
2. **F12** — ~~decide `PruneMode` casing **now**~~ (done, pre-release); trim printer columns; unify
   ObjectMeta tags.
3. **F10** — CommitRequest lifecycle (TTL or ownerRef) and the `delete` verb.
4. **F9** — verify the `scope: Namespaced` status-write path on the minimum supported Kubernetes
    version.

---

## 5. Bottom line

The question asked was whether the status implementation is *compatible enough* with the open
standard.

**The model is compatible. The implementation is compatible in three of the four states it can be
in.** The right conditions, the right per-condition `observedGeneration`, the right list semantics,
no phase string, a written contract, and conformance tests against the real kstatus library — more
than most projects have at v1alpha3, and the reason the two real defects are worth fixing rather
than redesigning around. F1 makes a `Failed` object look `InProgress`; F2 makes an idle object
never reach `Current`. Both are localized. Fix those and this passes a kstatus conformance review.

The larger gap is not kstatus at all — it is the **Flux object contract** (§1, layer 3): `suspend`,
`interval`, reconcile-on-annotation, and Events. That one is not a formal standard and nobody fails
an audit for missing it; it is codified only as an importable Go module,
`github.com/fluxcd/pkg/apis/meta`, which this repo already depends on. But it is what makes an
object *operable* by the reflexes a platform team already has from Flux, and right now none of
those reflexes return anything here. Most of it is already designed in
`docs/design/reconcile-triggering.md`; it needs building. `spec.suspend` first, because this
controller writes to Git and there is currently no way to make it stop.
