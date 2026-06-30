# Idea: cross-kind dependency watches as a first-class concern

> Status: idea — captured after issue #145.
> Date: 2026-05-20
> Triggering bug: [#145](https://github.com/ConfigButler/gitops-reverser/issues/145) — GitTarget / ClusterWatchRule / WatchRule did not react to their referenced dependency appearing, recovering only on the periodic `RequeueShortInterval` (~2 min).

## The bug class in one sentence

A controller whose `Reconcile` reads a sibling kind by name but whose `SetupWithManager`
does not `Watches(...)` that kind will treat "dependency arrived" as an event it cannot see;
recovery time is bounded by the controller's `RequeueAfter`, not by the cluster's actual
state-change latency.

Issue #145 was the third instance of this pattern in this repo (`GitTarget → GitProvider`,
`ClusterWatchRule → GitTarget`, `WatchRule → GitTarget`) and is the kind of thing every
operator team rediscovers once per project. ConfigButler — both the operator and the
audit/lint surface around it — is well placed to turn this into a solved problem instead
of a tribal-knowledge one.

## Why per-pair plumbing is the wrong layer to keep iterating on

The fix for #145 was three near-identical `Watches(...)` calls plus three near-identical
map functions:

```go
// gitProviderToGitTargets, gitTargetToClusterWatchRules, gitTargetToWatchRules,
// gitProviderToClusterWatchRules, gitProviderToWatchRules — same shape.
List dependents in the dependency's scope.
Filter by spec.<refField>.name (and namespace, for cross-namespace refs).
Return as []reconcile.Request.
```

Every new reference adds one of these. Forgetting one looks identical to the bug we just
fixed: dependent gets stuck on `*NotFound`, status self-heals on the periodic requeue, no
loud failure. Reviewers don't catch it because the dependent's `Reconcile` *does* read the
dependency — the missing piece is in the wiring file three hundred lines away.

## The ladder

Four options, weakest to strongest. Not mutually exclusive — (2) and (4) are the
recommended pair.

### 1. Convention + generator (cheap, partial)

Document the rule: "for every `*Ref` field in a CRD spec, the controller must `Watches`
the referenced kind." Optionally emit map functions from kubebuilder markers:

```go
// +configbutler:dependsOn=GitProvider,refField=spec.providerRef.name,namespaceField=metadata.namespace
type GitTargetSpec struct { ... }
```

Cheap to ship; still requires authors to remember the marker. Doesn't help downstream
operators that aren't ours. Skip on its own.

### 2. Library helper — `WatchesRef[T]` (recommended, near-term)

A small wrapper around `Watches(...) + handler.EnqueueRequestsFromMapFunc(...)` that
takes a typed extractor from the dependent to the dependency it points at, and indexes
candidates by that reference. One line per dependency edge replaces the current 10–30
lines per pair.

Sketch:

```go
func WatchesRef[Dep client.Object, Dependent client.Object](
    b *builder.Builder,
    deps DependentLister[Dependent],
    refOf func(Dependent) types.NamespacedName,
    scope ListScope, // namespaced or cluster-wide
) *builder.Builder
```

Caller (replacing today's `gitProviderToGitTargets`):

```go
WatchesRef[*GitProvider, *GitTarget](
    b, gtList,
    func(t *GitTarget) types.NamespacedName {
        return types.NamespacedName{Name: t.Spec.ProviderRef.Name, Namespace: t.Namespace}
    },
    ListScope{InNamespaceOfDependency: true},
)
```

What this buys:

- Eliminates copy-paste map functions. The five we just shipped collapse to five
  one-liners.
- Refs become greppable: every cross-kind dependency in the codebase is a `WatchesRef[...]`
  call site. New ones show up in review.
- A simple unit-test pattern carries over (the existing
  [dependency_watches_test.go](../../internal/controller/dependency_watches_test.go)
  pattern works unchanged — the helper is just where the loop body lives).

What it doesn't buy: downstream operator authors don't see this unless they import the
ConfigButler library, which most won't.

### 3. Status-driven auto-wiring (the structural answer)

The signal is already in the bug: every dependent emitted `Ready=False,
reason=ProviderNotFound, message="Referenced GitProvider 'gitops-reverser/cozystack-example'
not found"`. The dependency is named in the status. A generic "unmet-dependency reconciler"
can:

1. Watch every CRD in the controller's scope for `Ready=True` transitions.
2. For every dependent whose status carries a structured `Unmet` reference matching the
   object that just went Ready, enqueue it.

To make this work we need to upgrade the convention beyond a free-text `message` field.
Two options:

- **Annotation-level**: dependents carry `configbutler.ai/unmet: configbutler.ai/v1alpha3/GitProvider/gitops-reverser/cozystack-example` while their dependency is missing; cleared on Ready.
- **Status-level** (preferred): a `status.unmetReferences []TypedObjectReference` field on every CRD that participates. Same idea, schema-validated, no annotation churn.

This is the same shape as kstatus's reason taxonomy and Crossplane's composition references;
it scales because the per-controller code is "fill in `unmetReferences`", not "wire up
Watches per kind." The auto-wiring loop is written once.

Cost: a CRD field convention and a small generic controller. Worth it once we have ≥ 4–5
cross-kind references, or once external CRDs (Flux's `GitRepository`, third-party providers)
need to participate without us editing them.

### 4. The auditor angle (recommended, where ConfigButler differentiates)

Static analysis can detect this bug class today, against any operator's repo, without
asking the author to adopt a library.

Detection rule, informally:

> For each controller, intersect the set of kinds read in `Reconcile` (via `r.Get`,
> `r.List`) with the set of kinds in `For(...)` / `Owns(...)` / `Watches(...)` calls in
> `SetupWithManager`. Any kind in the first set but not the second is a candidate
> "stuck dependent" — flag with the controller's `RequeueAfter` as the expected recovery
> time.

This is tractable with `go/ssa` or even AST + a small whitelist of known controller-runtime
call signatures. Cluster-api, Crossplane, and Flux's own controllers all have past
instances of exactly this bug; an auditor that catches it on first install would land hard.

Concrete deliverable: an `auditor check stuck-dependents` rule in the ConfigButler audit
tool, with the exit message naming the missing `Watches`, the kind, and the
`RequeueAfter`-bounded delay the operator will see in production.

## Recommendation

Ship **(2)** as a small internal helper this quarter — it's a refactor of code we already
have, removes the boilerplate, and makes future refs single-line. Treat **(4)** as the
product opportunity: the value to external operator authors is high, the implementation
is one focused analyzer, and it's the rare static check that turns an invisible operator
bug into a CI failure. **(3)** is the durable answer if/when the cross-kind reference
graph grows past what `WatchesRef` is pleasant for, or once non-ConfigButler CRDs need to
participate; defer until that pressure is real.

## Non-goals

- Removing the periodic `RequeueShortInterval`. It stays as a safety net regardless of
  which option above is taken — Watches can be missed (informer restarts, RBAC gaps).
- A generic "ref resolver" that hides the per-controller validation. Status messages
  still need to be precise; only the *enqueue* path is mechanizable.

## Risk / open questions

- **`WatchesRef` and generics**: controller-runtime's `builder.Builder.Watches` is not
  generic-friendly; the helper likely ends up taking a non-generic `client.Object` plus
  a type-asserting extractor. Acceptable; the call site is still one line.
- **Status-driven autowiring vs. RBAC**: cluster-wide watch of every CRD is a privilege
  ask. The generic controller would need to either be scoped or rely on dynamic informers
  per-CRD; both have ergonomics cost. This is why (3) is deferred.
- **Auditor false positives**: a controller may legitimately read a sibling kind only on
  an out-of-band path (e.g. webhook validation) and not need a watch. The check needs a
  way to silence those — likely a `// configbutler:no-watch-required` comment marker.

## References

- The fix for the triggering bug:
  [`gittarget_controller.go:903`](../../internal/controller/gittarget_controller.go#L903),
  [`clusterwatchrule_controller.go:308`](../../internal/controller/clusterwatchrule_controller.go#L308),
  [`watchrule_controller.go:328`](../../internal/controller/watchrule_controller.go#L328).
- The unit-test pattern that survives the (2) refactor:
  [`dependency_watches_test.go`](../../internal/controller/dependency_watches_test.go).
- Periodic-requeue safety net: [`constants.go`](../../internal/controller/constants.go) (`RequeueShortInterval`).
