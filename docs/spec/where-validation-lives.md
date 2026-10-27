# Where validation lives: schema, then CEL, then the reconciler

> **spec** — current behaviour. The code depends on this document; change one, change the other. Index: [`../INDEX.md`](../INDEX.md)

One rule, repo-wide, so it stops being re-argued per feature.

## The ladder

Validate at the **first** rung that can express the rule:

1. **OpenAPI schema** — types, enums, required, min/max, immutability where the shape allows.
2. **CEL `XValidation`** — anything expressible from the object *itself*, including
   transitions against `oldSelf`. Example: `spec.secretRef` and `spec.inCluster` immutability on
   `KubeProvider` ([`kubeprovider_types.go:28`](../../api/v1alpha3/kubeprovider_types.go#L28)).
3. **The reconciler** — everything **cross-object**, because CEL cannot read another object.
   Refusal surfaces as `Validated=False` with a reason, and **returns before** the object
   reaches the data plane.

**A validating admission webhook is not a rung on this ladder.** It is only correct when the
information being validated *exists only at admission time* and is nowhere on the persisted
object. That is a narrow case, and GitOps Reverser ships no webhook at all (below).

## Why cross-object checks go to the reconciler, not a webhook

Three things, in order of how often they get argued backwards:

- **Reconcile-time is the *stronger* gate.** Admission is one-shot at write time and cannot see
  a policy tightened *after* the object was created. The reconcile gate re-evaluates
  continuously, so tightening a policy stops work that is already running. An admission-only
  check is strictly less safe.
- **The data-plane ordering is what provides the security property**, not the rejection. Nothing
  is read from a source cluster and nothing is written to Git until the check has passed. A
  rejected-at-admission object and a `Validated=False` object have **identical blast radius:
  zero**.
- **What admission actually buys is feedback latency** — `kubectl apply` fails in the user's
  terminal instead of succeeding and leaving a condition they have to go look at. That is a UX
  property, and it is worth less than the install cost of a webhook: cert wiring in the chart,
  a failure mode that can block tenant writes, and an extra moving part in every install path.

The worked example is the `KubeProvider`'s namespace admission (then `spec.accessFrom`, now
`spec.allowedGitTargets`). An earlier design proposed
enforcing it "in two places", one of them a webhook. What shipped enforces it in **one**, on
every reconcile, before `DeclareForGitTarget`
([`gittarget_controller.go:479`](../../internal/controller/gittarget_controller.go#L479),
[`gittarget_source_cluster.go:36`](../../internal/controller/gittarget_source_cluster.go#L36)),
and that is why the quickstart is a single command again. The `KubeProvider` audit route follows
the same rule: which namespace holds a route depends on every other provider carrying it, so a
provider that loses it reports `Stalled=True` (`AuditRouteConflict`) on reconcile rather than
being refused at admission.

## No webhook ships

GitOps Reverser installs no admission webhook. The one it had captured the creator of a
`CommitRequest` at admission, the textbook case for one: identity exists in the `AdmissionRequest`
and on no persisted field. It went once the audit pipeline could give a request's author the same
way it gives an edit's: from the request's own audit event, awaited from the same index with the
same grace ([`attribution.md`](attribution.md)). The same endpoint also rejected a `WatchRule` that
would bring a second source namespace to a `GitTarget` declaring `spec.serializeNamespace: false`.
That was feedback, never the gate: the writer refuses the configuration whether or not the webhook
ran, and the `GitTarget` reports it as `MultipleSourceNamespaces`.

## Applying this

When a new rule is cross-object, do not open the webhook question. Write the check in the
reconciler, return before the data-plane declaration, and surface `Validated=False` with a
specific reason. Where a same-object CEL rule can express *part* of it, add CEL too — but never
as the only gate, for the tightened-after-creation reason above.
