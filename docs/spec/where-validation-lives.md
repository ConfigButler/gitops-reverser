# Where validation lives: schema, then CEL, then the reconciler

> **spec** — current behaviour. The code depends on this document; change one, change the other. Index: [`../INDEX.md`](../INDEX.md)

One rule, repo-wide, so it stops being re-argued per feature.

## The ladder

Validate at the **first** rung that can express the rule:

1. **OpenAPI schema** — types, enums, required, min/max, immutability where the shape allows.
2. **CEL `XValidation`** — anything expressible from the object *itself*, including
   transitions against `oldSelf`. Example: `spec.kubeConfig` immutability on `ClusterProvider`
   ([`clusterprovider_types.go:68`](../../api/v1alpha3/clusterprovider_types.go#L68)).
3. **The reconciler** — everything **cross-object**, because CEL cannot read another object.
   Refusal surfaces as `Validated=False` with a reason, and **returns before** the object
   reaches the data plane.

**A validating admission webhook is not a rung on this ladder.** It is only correct when the
information being validated *exists only at admission time* and is nowhere on the persisted
object. That is a narrow case, and we ship exactly one instance of it (below).

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

The worked example is `ClusterProvider.spec.allowedNamespaces`. An earlier design proposed
enforcing it "in two places", one of them a webhook. What shipped enforces it in **one**, on
every reconcile, before `DeclareForGitTarget`
([`gittarget_controller.go:311`](../../internal/controller/gittarget_controller.go#L311),
[`gittarget_source_cluster.go:68`](../../internal/controller/gittarget_source_cluster.go#L68)) —
and that is why the quickstart is a single command again.

## The one webhook we ship, and why it is not an exception to the rule

[`/validate-operator-types`](../../internal/webhook/validate_operator_types_handler.go#L26)
is **not a validation gate**. It captures the *submitter's identity* on a command kind
(`CommitRequest`) so a commit can be authored by a real Kubernetes user, and it **always
allows**. Identity is the textbook case for admission: it exists in the `AdmissionRequest` and
on no persisted field, so no reconciler could recover it later. See
[`commitrequest-admission-authorship.md`](commitrequest-admission-authorship.md).

(`/validate-all` — [`admission_allow_handler.go`](../../internal/webhook/admission_allow_handler.go#L12)
— is an always-allow observation surface wired only by the e2e SUT. The chart does not ship it.)

## Applying this

When a new rule is cross-object, do not open the webhook question. Write the check in the
reconciler, return before the data-plane declaration, and surface `Validated=False` with a
specific reason. Where a same-object CEL rule can express *part* of it, add CEL too — but never
as the only gate, for the tightened-after-creation reason above.
