# Mutation Capture Lab Design

Status: **M0 through M4** — M0/M1 (ConfigMap core moments), M2 (workload subresources +
grace-period delete), M3 (a two-version CRD + conversion webhook), and M4 (an aggregated
API) — are captured and committed under `test/mutationlab/corpus/` (against **k8s
v1.35.2+k3s1**, see `CLUSTER.md`). The harness — the lab binary
(`cmd/mutation-capture-lab/`), record model, normalizer, store, golden-corpus plumbing, and
the watch/audit/admission recorders under `internal/mutationlab/` — is built with unit
coverage. Per a steering decision, the lab **reuses the product's webhook URLs** rather than
serving its own: it listens on `/validate-admission-webhook`, `/audit-webhook`, and the
proxy-enrichment `/audit-webhook-additional`, so `task lab-e2e` integrates by **swapping the
controller image** for the lab image on the already-prepared e2e cluster — no new audit
policy, webhook config, or certificates (see [Isolated Test Setup](#isolated-test-setup)).
The live-cluster driver (`test/mutationlab/e2e/`, build tag `mutationlab_e2e`) captures the
committed scenarios; `task lab-corpus-update` writes the corpus and `task lab-e2e` re-captures
and **compares clean** (proven deterministic across repeated runs), so a corpus diff in a PR
is a real signal.

**M1 corpus-driven findings (v1.35.2)** — exactly the kind the lab exists to surface:

- a `deletecollection` fans out into **N per-object watch `DELETED`** events as expected
  (Row 9), and the apiserver runs validating admission **once per object** (three named
  `DELETE` calls), not once for the collection;
- but the single name-less audit `deletecollection` event's **`responseObject` carries the
  full removed objects** (a `List`) on this version — its `requestObject` is `DeleteOptions`,
  yet the response is *not* shallow. This refines the Row 9 hypothesis: the shallow-body
  concern is real for the *request* body, narrower for the *response* than first assumed;
- the admission→audit **breadcrumb join is confirmed**: the recorder's
  `AdmissionResponse.auditAnnotations` surface in the audit event prefixed by the webhook
  name (`all-events-test-only.configbutler.ai/scenario`);
- `dry-run` and `record-and-reject` both produce admission + audit records but **no watch
  event and no etcd object** (Rows 11–12), and the reject's denial message rides through to
  the audit `responseStatus` (`code: 403`, the lab's own policy message).

**M2 corpus-driven findings (v1.35.2)** — the headline is mechanism *silence*, and the silence
is **aligned with what GitOps Reverser is for**. The product captures *intent* — the declared
`spec` a human authored — not operational/runtime state. That is exactly why the reused audit
policy ignores `*/status` (controller-owned runtime), `*/scale` from the HPA (an autoscaler, not a
person), and `pods` outright (a Pod is rarely *direct* intent — you declare a Deployment or
StatefulSet and a controller creates the Pods). So the M2 silences below are the capture policy
correctly dropping operational noise, not a lab gap (see [Capturing intent, not
state](#capturing-intent-not-state)):

- **Row 5 (`/status`) — only watch sees it.** The audit policy drops both `apps/*/status`
  and core `*/status`, and the validating webhook matches **top-level resources only**, so a
  Deployment `/status` write reaches **neither audit nor admission**; the watch is the lone
  witness. And the watch witnesses two events, because **status is controller-owned**: the
  user write (`observedGeneration: 99`) is immediately **clobbered** by the deployment
  controller back to the real value (`observedGeneration: 1`) — a user `/status` write does
  not persist. The corpus is the two `MODIFIED` events, side by side.
- **Row 6 (`/scale`) — audit yes, admission no.** A `/scale` patch *is* audited (the policy
  drops scale only for the HPA service account, not for a user), recorded as `verb: patch`
  with `objectRef.subresource: scale`; but it still never reaches admission (subresource).
  The audit body is an `autoscaling/v1` `Scale` object that **carries none of the parent's
  labels** — a real attribution wrinkle the harness had to handle.
- **Row 7 (Pod graceful delete) — admission yes, audit no.** Core `pods` are dropped from
  the audit policy entirely, so a Pod delete is **invisible to audit**; pods are top-level,
  so the `DELETE` *does* reach the validating webhook. The watch shows the two-step removal:
  `MODIFIED` with `deletionTimestamp` set while the pod is still `Running`, then `DELETED`
  once the kubelet has terminated the container (`phase: Failed`, `exitCode: 137` from the
  post-grace `SIGKILL`). The intermediate kubelet status writes during termination are
  timing-dependent, so the corpus keeps the two load-bearing moments (deletion-pending +
  terminal) and the structured layer asserts the law over the full event stream.
- **Normalization refinement (forced by M2).** Capturing rich objects exposed that
  **relational, chronological `<ts-N>` timestamps are not stable**: Kubernetes emits
  timestamps at one-second granularity, so whether two near-simultaneous events (a Pod's
  `creationTimestamp` and its first condition, say) share a second varies run to run, which
  shuffles the indices. Timestamps now collapse to a single non-relational `<ts>`;
  object-version sequencing is carried by `resourceVersion` (relational, numeric) and the
  moment file ordering instead. M2 also added placeholders for `containerID`, `nodeName`,
  pod/host IPs, and — the subtle one — IPs **embedded in `managedFields` association keys**
  (`k:{"ip":"10.42.3.14"}`).

**M3 corpus-driven findings (v1.35.2)** — Row 14, a two-version lab CRD (`Widget`) whose schemas
differ by a webhook-converted field (`v1.spec.sizeBytes: integer` ⇄ `v2.spec.size: string`, v2 the
storage version). One create makes the three shapes literally diffable in the corpus:

- **admission** sees the **submitted** version — `apiVersion: …/v1`, `spec.sizeBytes: 1024`.
- the **watch** (opened on the v2 storage version) sees the **stored/served** version —
  `apiVersion: …/v2`, `spec.size: "1024"`.
- the **conversion webhook** is called in **both** directions for the single create
  (`conversion.to-v2` for storage, `conversion.to-v1` for the create response in the submitted
  version), captured as a new `conversion` source.
- a CRD has **no controller**, so unlike M2 the capture is deterministic with no special handling.

**M4 corpus-driven findings (v1.35.2) — the headline result for watch mode.** Row 15, an aggregated
API (the wardle `flunders` resource, which the e2e cluster runs behind the `apiservice-audit-proxy`).
One flunder create, captured three ways side by side:

- the **official** kube-apiserver audit (`/audit-webhook`) records `objectRef` and `responseStatus:
  {code: 201}` but **no `requestObject` and no `responseObject`** — the body-quality cliff is real;
  the apiserver proxies the aggregated request and has no schema to render the object.
- the **proxy-enriched** audit (`/audit-webhook-additional`) **does** carry the full request and
  response bodies — which is exactly why the `apiservice-audit-proxy` body-enrichment exists.
- the **watch** carries the **full object natively** — `spec.reference`, `spec.referenceType`,
  labels, the lot.

So the corpus **confirms the shallow-fill hypothesis**: where the official audit body goes empty, the
live watch still carries the whole object. The product consequence (Purpose goal #3) is concrete — a
watch-based capture would **not need the `apiservice-audit-proxy` body-enrichment for object
content**; watch supplies natively what that proxy reconstructs. The honest caveat stands: this holds
only while the watch is live, with a periodic reconcile as the gap backstop (see
[Watch Is Viable For State](#watch-is-viable-for-state)). (A secondary observation: the lab also saw a
validating-admission record for the flunder create, so admission is not always bypassed for
aggregated APIs on this version — but admission remains non-authoritative regardless; the corpus row
focuses on the body-quality comparison.)

Remaining are the M1 rows not yet filled in (Rows 3, 4, 8, 10, 13, 16–17 — server-side apply, no-op
apply, finalizer delete, owner-ref cascade, optimistic-concurrency conflict, watch resync, bookmark).
M0–M4 are captured and committed.

Related:

- [Issue #168: Watch mode](https://github.com/ConfigButler/gitops-reverser/issues/168)
- [Audit Ingestion Decision Record](audit-ingestion-decision-record.md)
- [Watch & Catalog Architecture](watch-and-catalog-architecture.md)
- [Architecture](../architecture.md)
- Kubernetes references for admission ordering/behavior used below:
  [Dynamic Admission Control](https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/)
  and [Admission Webhook Good Practices](https://kubernetes.io/docs/concepts/cluster-administration/admission-webhooks-good-practices/)

## Purpose

The lab has three jobs, and the first two are the durable ones:

1. **Document how Kubernetes actually behaves.** Build a small, separate application that
   records the *exact* structures Kubernetes exposes through native watches, audit webhooks,
   and validating admission webhooks — at every interesting moment — and commits those
   structures as a versioned corpus. This corpus is reference material: when we later argue
   about "what does audit put in `responseObject` for a deletecollection" or "does a finalizer
   delete produce a second audit `delete` event," we read the answer out of a file captured
   from a real apiserver instead of guessing.

2. **Validate that detailed behavior still holds on new Kubernetes versions.** The corpus is
   captured against a *pinned* apiserver version, so it doubles as a regression harness: point the
   lab at a newer Kubernetes release, regenerate, and any change in the fine-grained behavior we
   depend on — verb naming, event ordering, body presence, deletecollection fan-out, finalizer
   sequencing — surfaces as a reviewable corpus diff *before* it surprises us in production. This is
   the job that earns the lab its keep over time: it turns "did the apiserver change something
   subtle in this upgrade?" from an open worry into a test run. See
   [Validating New Kubernetes Versions](#validating-new-kubernetes-versions).

3. **Make one product decision smaller.** With the corpus in hand, the watch-vs-audit-vs-hybrid
   choice (Issue #168) stops being abstract. We can point at the recorded shapes and say which
   mechanism honestly carries which information.

The application is **not** another implementation of GitOps Reverser. It is a lab: a minimal set
of recorders and scenarios whose output is (a) machine-checked behavioral invariants and (b) a
human-readable library of captured payloads.

## Capturing Intent, Not State

GitOps Reverser exists to mirror **intent** into Git — the declared `spec` a human (or a
higher-level controller acting on a human's declaration) authored — not the operational, runtime
*state* the cluster derives from it. This single distinction explains most of how the capture
surface is tuned, and it is the lens through which the lab's findings should be read:

- **`status` is not intent.** It is controller-owned runtime truth that churns continuously and is
  reconstructed on demand, so committing it to Git would be noise, not history. The audit policy
  therefore drops `*/status`, and M2 confirms the consequence directly: a user `/status` write is
  even *clobbered* by the owning controller (Row 5). Intent lives in `spec`; `status` is downstream.
- **Autoscaler/operational writes are not (human) intent.** The policy drops `*/scale` from the HPA
  service account specifically — an autoscaler adjusting replicas is the system reacting, not a
  person declaring. A *human* scaling a Deployment via `/scale` is still captured (Row 6), because
  that is an intent edit.
- **Some whole resource types are not intent.** A `Pod` is rarely *direct* intent — you declare a
  `Deployment`/`StatefulSet`/`Job` and a controller materializes Pods — so the audit policy drops
  `pods` outright (also `events`, `endpoints`, `nodes`, leases: all operational). The intent-bearing
  object is the workload controller's `spec`, not the Pods it spawns.

So the M2 "silences" are not gaps in the lab or the wiring — they are the capture policy **correctly
declining to record operational state**. The lab's job there is to *confirm* the boundary holds (and
to show that where a mechanism does fire on an operational surface, e.g. the validating webhook still
seeing a Pod `DELETE`, it is the always-allow recorder being harmlessly broad, not a capture
decision). The flip side is the load-bearing positive result: in every place the provenance
mechanisms fall silent on operational state, the **watch still carries the full object** — which is
why watch is a credible state-mirroring source for the *intent-bearing* types a `WatchRule` selects.

This is a product framing, not a lab mechanism, but it is the reason the corpus looks the way it does
and is referenced from the [Difficult Cases Catalog](#difficult-cases-catalog) and the M2 findings
above. Capturing operational state would be a different (and probably unwanted) product.

## What "Capture The Difficult Situations" Means

The easy cases are not why this lab exists. "Create succeeds, watch sees `ADDED`" is obvious and
nobody needs a corpus to believe it. The value is concentrated in the awkward cases, where the
three mechanisms *disagree* or where one of them quietly loses information:

- a deletecollection that fans out into N watch deletes but a single name-less audit request
- a finalizer delete whose final `DELETED` has **no** corresponding audit `delete` verb
- a dry-run write that admission and audit both see but that never reaches etcd
- a multi-version CRD where the persisted object differs from the one admission validated
- an aggregated API whose audit body is empty even though the write succeeded

The lab is worth building only insofar as it pins these down. If it ever drifts toward
re-proving the easy cases, it has lost the plot. Every scenario in the matrix below earns its
place by being a situation where a naive reader would get the behavior wrong.

## Two Layers: Assertions And A Corpus

The user-facing tension — "we want structure, but we also want raw YAML for every moment" — is
resolved by capturing once and emitting twice. A single recorded observation drives both layers:

| Layer | Artifact | Audience | Failure mode it catches |
|---|---|---|---|
| **Structured invariants** | Go assertions over the `Record` summary | CI | a behavioral *law* breaks ("dry-run produced a watch event") |
| **Golden corpus** | normalized YAML files checked into the repo | humans + PR review | a *shape* drifts ("audit stopped sending `requestObject` for patches") |

The structured layer is the law: a small number of invariants that must always hold, asserted
programmatically, red when violated. The corpus layer is the evidence: the full payload, written
to disk, diffable. The two are complementary — the law tells you *that* something changed, the
corpus shows you *what* changed and is browsable on its own as documentation.

Neither layer is hand-authored from imagination. The corpus is generated by running the
recorders against a real apiserver. **Hand-writing a golden file defeats the entire purpose** —
its only value is that it is what Kubernetes actually emitted. The illustrative YAML embedded in
this document is explicitly marked as illustrative; the authoritative files come from the lab.

## Expected Observations To Verify

The lab starts with expectations, not conclusions. Some expectations are backed by Kubernetes API
contracts; others are product-shaped hypotheses we need the corpus to confirm, falsify, or narrow.

- **ResourceVersions should progress within a single resource stream.** Kubernetes documents
  `resourceVersion` as an *opaque* string that clients must not interpret or compare across resources
  (that contract is cited, not re-proven — see [Verify vs Cite](#verify-vs-cite-what-the-lab-proves-and-what-it-only-documents)).
  The narrower, testable claim we *do* want the corpus to confirm is: for built-in resources and
  CRDs on the pinned lab apiserver, object `metadata.resourceVersion` values are
  orderable and monotonically increasing within one watched resource type/namespace stream. The
  normalizer should preserve this relation (`<rv-1>`, `<rv-2>`, ...), because matching an audit
  `responseObject.metadata.resourceVersion` to a later watch object is one of the most useful
  correlation tools. This is not a license to compare RVs across unrelated resource types or
  arbitrary aggregated APIs without first proving they are orderable. Kubernetes API concepts:
  https://kubernetes.io/docs/reference/using-api/api-concepts/

- **Watch should report object-level consequences, including deletecollection fan-out.** A watch is
  over a resource collection and streams add/update/delete notifications for objects affected by API
  operations. For `deletecollection`, the working expectation is therefore: one user request, one
  audit `deletecollection` event, and **N per-object watch `DELETED` events** — no special
  collection-level watch event. This is central enough that the lab must assert it directly, because
  watch mode depends on seeing the deleted object identities while the watch is live. Kubernetes API
  concepts:
  https://kubernetes.io/docs/reference/using-api/api-concepts/

- **Audit-to-admission/watch correlation probably has no single shared ID.** Audit events carry an
  `auditID`, unique per API request. AdmissionReview carries `request.uid`, but Kubernetes documents
  that UID as the individual apiserver-to-webhook round trip, not the user request identity. We
  should not expect `auditID == admission.request.uid`. The lab should capture both and prove what,
  if anything, joins them. Kubernetes audit API:
  https://kubernetes.io/docs/reference/config-api/apiserver-audit.v1/ Admission API:
  https://kubernetes.io/docs/reference/config-api/apiserver-admission.v1/

- **Admission can deliberately leave breadcrumbs in audit.** The admission recorder should return a
  scenario/run audit annotation in its `AdmissionResponse.auditAnnotations`. Kubernetes says these
  annotations are added to the audit log with the webhook name as a prefix, which gives the lab a
  controlled way to join "this admission call" to "this audit request" without pretending Kubernetes
  provides a shared native ID. The corpus should still record whether natural correlation also works
  via namespace/name/UID, `responseObject.metadata.resourceVersion`, verb, requestURI, and
  timestamps. Admission API:
  https://kubernetes.io/docs/reference/config-api/apiserver-admission.v1/

- **Watch should carry the full object precisely where audit goes shallow.** The cases where audit is
  least useful are the *shallow* ones — a name-less `deletecollection` whose audit *request* body is
  only `DeleteOptions` and never names the removed objects (Row 9 — its *response* body does carry
  them on the pinned version, but a name-less request still cannot be relied on for per-object
  identity), and an aggregated-API write whose audit request/response body is empty (Row 15). The working hypothesis is that the **live watch event still
  carries the full object** in exactly these cases, because watch reports object-level consequences:
  each `DELETED`/`MODIFIED` should contain the object even when the matching audit event does not. If
  the corpus confirms this is robust, it is a **product finding, not just a curiosity** (Purpose goal
  #3): a watch-based capture would supply natively the object content that today's audit body-join
  path reconstructs — the `apiservice-audit-proxy` / body-enrichment proxy — so watch mode could drop
  the need for that proxy entirely. The lab must also surface the *limit* of the claim and not oversell
  it: a watch that is down or lagging loses the event (and its body) outright, so the honest
  conclusion is "watch fills shallow audit bodies *while live*, with a list + mark-and-sweep backstop
  for the gap," never "watch replaces the proxy unconditionally." The corpus should make both the fill
  and the gap visible. Kubernetes API concepts:
  https://kubernetes.io/docs/reference/using-api/api-concepts/

## Verify vs Cite: What The Lab Proves And What It Only Documents

The lab is expensive attention, so it should spend it only where Kubernetes behavior is **subtle,
surprising, or fragile across versions**. Several things a reader might expect the lab to demonstrate
are instead unambiguously documented by Kubernetes; re-proving them would add apparatus (a mutating
recorder, a second webhook, version-comparison code) without adding knowledge. The canonical example
is *"mutating admission runs before validating admission"* — true, load-bearing for why a validating
recorder cannot see user intent, and already documented — so the lab **cites** it rather than building
a mutating recorder to watch it happen.

This table draws the line. The **Verify** rows are the lab's reason to exist; their authoritative
detail lives in the [Difficult Cases Catalog](#difficult-cases-catalog) and
[Expected Observations To Verify](#expected-observations-to-verify), and this table only indexes them
(it is not a second source of truth). The **Cite** rows are documented Kubernetes contracts we depend
on but deliberately do **not** capture, each with the upstream reference that makes a lab proof
unnecessary.

| Claim | Verify or cite | Evidence |
|---|---|---|
| RVs progress (orderable, monotonic) *within one* resource stream | **Verify** (hypothesis) | corpus + invariant; [Expected Observations](#expected-observations-to-verify) |
| `deletecollection` → **N** per-object watch `DELETED` + **one** name-less audit event | **Verify** | Row 9 |
| finalizer delete's terminal `DELETED` has **no** audit `delete` verb | **Verify** | Row 8 |
| no-op apply produces **no** watch event | **Verify** | Row 4 |
| dry-run reaches admission + audit but **no** watch object / **no** etcd object | **Verify** | Row 11 |
| record-and-reject → admission record, no watch object, no etcd object | **Verify** | Row 12 |
| owner-ref cascade children are deleted by the GC system user, not the human | **Verify** | Row 10 |
| optimistic-concurrency conflict returns `409` with a `Status` body, not the object | **Verify** | Row 13 |
| multi-version CRD: persisted / admission / served shapes diverge | **Verify** | Row 14 |
| aggregated-API body-quality cliff (often **empty** request/response bodies) | **Verify** | Row 15 |
| watch carries the **full object** where the audit body is shallow (name-less `deletecollection`, empty aggregated body) — could let watch mode drop the `apiservice-audit-proxy` body-enrichment | **Verify** (product-relevant) | Rows 9, 15; [Expected Observations](#expected-observations-to-verify) |
| watch `410 Gone` → `ERROR` then relist; `BOOKMARK` is the only safe resume anchor | **Verify** | Rows 16–17 |
| what (if anything) actually *joins* an audit event to an admission call | **Verify** (investigation) | [Expected Observations](#expected-observations-to-verify); the field *definitions* are cited below |
| `resourceVersion` is an **opaque** string, not orderable across resources/streams | **Cite** | <https://kubernetes.io/docs/reference/using-api/api-concepts/> |
| `auditID` is unique per API request; admission `request.uid` identifies the apiserver↔webhook round trip (so they are **not** equal) | **Cite** | <https://kubernetes.io/docs/reference/config-api/apiserver-audit.v1/> · <https://kubernetes.io/docs/reference/config-api/apiserver-admission.v1/> |
| mutating admission runs **before** validating admission (so a validating recorder only ever sees the already-mutated object — no mutating recorder is built) | **Cite** | <https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/> |
| validating webhooks run **in parallel**; any single rejection fails the whole write | **Cite** | <https://kubernetes.io/docs/concepts/cluster-administration/admission-webhooks-good-practices/> |
| `matchPolicy: Equivalent` may deliver a **converted** object; `requestKind`/`requestResource` carry the original (the reused `Equivalent`+`*` webhook matches every version, so it observes the submitted one — see M3 design) | **Cite** | <https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/> |
| `AdmissionResponse.auditAnnotations` surface in the audit log, prefixed by the webhook name | **Cite** the mechanism; **verify** it actually joins our records | <https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/> |

The rule of thumb: if a naive reader would get the behavior *wrong*, or it could *change* in a
Kubernetes upgrade, it earns a corpus row (**Verify**). If Kubernetes documents it plainly and it is
stable contract, the lab links the docs and moves on (**Cite**).

## Non-Goals

- Do not write Git commits.
- Do not reuse the GitOps Reverser controller runtime.
- Do not implement the full `WatchRule` / `GitTarget` model.
- Do not solve production HA, retention, or multi-cluster ingestion.
- Do not prove every Kubernetes resource behaves identically.
- Do not build a **mutating** admission recorder. That mutating admission precedes validation — and
  therefore that a validating recorder cannot see the user's original submission — is a documented
  Kubernetes contract the lab cites, not a behavior it re-proves (see
  [Verify vs Cite](#verify-vs-cite-what-the-lab-proves-and-what-it-only-documents)).
- Do not re-prove well-documented, stable Kubernetes contracts at all. Anything in the **Cite** half
  of the [Verify vs Cite](#verify-vs-cite-what-the-lab-proves-and-what-it-only-documents) table is
  referenced by its upstream docs, not captured as a corpus row.
- Do not hand-author corpus files — they are only meaningful when machine-captured.
- Do not wire the lab into the main `task test-e2e` suite or the default CI lane (see
  [Isolated Test Setup](#isolated-test-setup)).
- Do not add lab-only behavior to the Helm chart.

The lab should be intentionally small. It exists to reveal capture semantics, not to become a
second operator.

## Relationship To The Product

The lab is separate from the product, but it touches one product surface that has already shipped.

**The always-allow validating admission webhook (landed).** GitOps Reverser now serves a
validating admission webhook endpoint, `/validate-admission-webhook`, that
currently allows every request. It exists as a stable extension point for future policy (for
example, refusing direct edits to objects known to be managed by kustomize). Today it is
deliberately inert but broadly wired:

- it always returns `allowed: true` (`internal/webhook/admission_allow_handler.go`);
- in `config/`, its `ValidatingWebhookConfiguration` matches a **broad** set — all groups and all
  top-level resources — so the testing deployment exercises admission across every kind, and so the
  match surface is already in place for the future policy. This is an inclusion-only superset of the
  audit capture policy (`test/e2e/cluster/audit/policy.yaml`): a webhook cannot express the audit
  policy's "everything except events/pods/leases/…" drops, and `resources: ['*']` is top-level only,
  which mirrors the audit policy dropping `*/status` and `*/scale` noise;
- its `failurePolicy` is `Ignore`, which is **mandatory** while it matches `*`: the webhook backend
  is the gitops-reverser pod itself, so `Fail` on core resources would deadlock the cluster at
  bootstrap. A real rejecting policy must move to `Fail` only alongside a `namespaceSelector` that
  excludes kube-system and the controller's own namespace;
- it is installed only through the `config/` manifests (`config/webhook/`), gated behind the
  `--admission-webhook-enabled` flag and its own cert-manager `Certificate`;
- **the Helm chart stays untouched until there is real policy behavior to expose** — this is a
  standing constraint, not an oversight. `config/` is the testing/dev surface; the broad match and
  the webhook itself do not belong in the product chart until the policy is real.

That webhook is a *product* extension point. It is **not** the lab. The lab's admission
*recorder* is a different thing: it makes and records admission decisions about arbitrary
resources in order to demonstrate why admission is not a trustworthy source for Git history. Keep
the two mentally separate — the product webhook proves the endpoint exists and stays out of the
way; the lab recorder proves admission cannot be authoritative.

## Mechanisms Under Test

### 1. Native Watch

The watch recorder opens a dynamic watch for selected GVRs and stores every event:

- `ADDED`
- `MODIFIED`
- `DELETED`
- `BOOKMARK`
- `ERROR`

For object events, the recorder stores the full object received from the API server plus extracted
metadata: group/version/resource, namespace/name/UID, resourceVersion/generation,
deletionTimestamp and finalizers, event type, and observed timestamp.

This path is expected to be the strongest source for "what object state exists or disappeared." It
is not expected to know the user who caused the change.

### 2. Audit Webhook

The audit recorder exposes a Kubernetes audit webhook endpoint and stores decoded `auditv1.Event`
items: auditID, stage, verb, user and impersonated user, source IPs and user agent, objectRef,
requestObject, responseObject, responseStatus, annotations, and stageTimestamp.

This path is expected to be the strongest source for request provenance. It is also the path most
likely to be operationally unavailable in managed clusters and least consistent for aggregated API
body shape.

### 3. Validating Admission Webhook

The admission recorder implements a validating admission webhook and stores each incoming
`AdmissionReview`: request UID, userInfo, operation, the *matched* kind/resource/subresource **and**
the request's `requestKind`/`requestResource`/`requestSubResource` (which differ from the matched ones
when `matchPolicy` converts the object), namespace/name, object and oldObject, dryRun, options, the
admission decision made by the recorder, and observed timestamp. It **allows by default** but can be
configured per scenario (keyed by namespace or label)
to **record-and-reject** — which is how Row 12 deterministically produces "admission saw it, etcd
never did" without depending on a second webhook's ordering.

The lab includes this mechanism precisely because it is *tempting* — it sees the user and the
object before the write — and the lab's job is to show why the temptation is a trap. Admission
observes *attempted* writes, not persisted writes. The admission chain order matters here: the
apiserver runs **all mutating webhooks first, then all validating webhooks in parallel**, then
persists (see
[Dynamic Admission Control](https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/)).
Specific failure modes the corpus must make visible:

- validating webhooks run **in parallel**, so another validating webhook can reject the request even
  though this recorder observed (and, if configured, allowed) it — there is no dependable "later"
  ordering between them; both are simply called, and any single rejection fails the whole write;
- because mutating admission runs *before* validation, this recorder only ever sees the
  **already-mutated** object — it cannot recover the user's original submission, so capturing
  pre-mutation intent would require a *mutating* recorder, which the lab deliberately is not;
- optimistic concurrency can fail the request after admission has already allowed it;
- dry-run requests reach admission but do not persist;
- defaulting and storage conversion can still make the persisted object differ from the object this
  recorder validated;
- the **version** the recorder sees is not guaranteed to be the submitted one: with
  `matchPolicy: Equivalent` the apiserver may convert the object to a version the webhook registered
  for before calling it, with the original request preserved in `request.requestKind`/`requestResource`.
  The lab reuses the product webhook, which is `matchPolicy: Equivalent` over `apiVersions: ['*']`;
  because that rule matches every version, no conversion happens before the call and the recorder
  observes the **submitted** version anyway. It records `requestKind`/`requestResource`/
  `requestSubResource` so any conversion is visible in the corpus rather than silent. (Row 14 needs no
  dedicated `Exact` webhook — see the [M3 design](#m3-design--crd--conversion); see also
  [Dynamic Admission Control](https://kubernetes.io/docs/reference/access-authn-authz/extensible-admission-controllers/)).

## Difficult Cases Catalog

This is the heart of the lab. Start with one built-in resource (`ConfigMap`) and one CRD-backed
resource with two served versions (to exercise conversion). Add an aggregated API last, and only
once the built-in cases read cleanly. A few rows cannot be expressed with `ConfigMap` and are
captured against a workload type instead: Rows 5 and 6 need an object with `/status` and `/scale`
subresources (a `Deployment`), and Row 7's two-event graceful delete needs a grace period (a `Pod`).
`ConfigMap` has none of these, so those rows were captured in **M2** against those types — see
[Milestones](#milestones). To keep the workload rows deterministic despite their active controllers,
the Deployment is created **paused with zero replicas** (so the only post-setup events are the
subresource write under test plus the controller's single status follow-up, never rollout/pod churn),
and the Pod sets `automountServiceAccountToken: false` (so no random `kube-api-access-XXXXX` volume
name churns the corpus).

The matrix below is the contract for what the corpus must contain. Each row maps to a scenario
directory under `test/mutationlab/corpus/<resource>/<scenario>/` holding one file per emitted
moment. "Moment" is deliberate: a single user action can produce several ordered events, and the
*ordering and count* are part of the behavior we are documenting.

The **Ready?** column flags which rows are captured today (with the milestone that filled them) and
which remain to fill in.

| # | Scenario | Ready? | Watch moments | Audit moments | Admission moments | Why it is hard |
|---|---|---|---|---|---|---|
| 1 | Create succeeds | ✅ M1 | `ADDED` (final object) | `create`, user, responseObject | CREATE, user/object | baseline anchor |
| 2 | Update / strategic-merge patch | ✅ M1 | `MODIFIED` (final) | `update` / `patch` | UPDATE, object + oldObject | verb differs by request shape |
| 3 | Server-side apply | ⬜ not yet | one or more `MODIFIED` | `apply` (or `patch`) | UPDATE with apply options | managedFields churn |
| 4 | No-op apply | ⬜ not yet | often **no** event (rv unchanged) | request still recorded | request still recorded | watch silence is the finding |
| 5 | Status subresource update | ✅ M2 | two `MODIFIED`: user write, then controller **clobber** | **none** — policy drops `*/status` | **none** — webhook ignores subresources | status is controller-owned; only watch sees it (verified M2) |
| 6 | Scale subresource patch | ✅ M2 | `MODIFIED` (scale) + controller `observedGeneration` follow-up | `patch`, `subresource: scale` | **none** — webhook ignores subresources | audited but never admitted; `Scale` body carries no labels (verified M2) |
| 7 | Graceful delete | ✅ M2 | `MODIFIED` (deletionTimestamp) then `DELETED` | **none** — policy drops `pods` | DELETE (pods are top-level) | two watch events for one delete; audit is blind (verified M2) |
| 8 | Finalizer delete | ⬜ not yet | `MODIFIED` (deletionTimestamp+finalizers), later `DELETED` | `delete`, then `patch`/`update` removing the finalizer — **no second `delete`** | DELETE, then later UPDATEs | final `DELETED` has no matching audit delete verb |
| 9 | Deletecollection | ✅ M1 | **N** `DELETED`, no collection event | **one** name-less `deletecollection` | **N** named `DELETE` (once per object, not once for the collection) | fan-out asymmetry — the watch-mode pressure test |
| 10 | Owner-ref cascade delete | ⬜ not yet | child `DELETED` events | child deletes attributed to the GC system user, not the human | DELETE by `system:serviceaccount:kube-system:generic-garbage-collector` | provenance is the system, not a user |
| 11 | Dry-run create | ✅ M1 | **no** watch event, no etcd object | event with `dryRun`, no persistence | CREATE, `dryRun: true` | seen but never persisted |
| 12 | Rejected during validation | ✅ M1 | **no** watch event | failed response (`code` 4xx) | recorder is **always called** (parallel validation) and, in this scenario, record-and-rejects | admission saw a write that never persisted |
| 13 | Optimistic-concurrency conflict | ⬜ not yet | no final change | failed response, `code: 409`, `Status` body | admission may have seen the attempted object | failure carries a Status, not the object |
| 14 | Multi-version CRD conversion | ✅ M3 | `ADDED`/`MODIFIED` in the served storage version (v2 `size` string) | bodies in the submitted version (v1 `sizeBytes`); +`conversion` source both ways | submitted version v1 (reused `Equivalent`+`*` webhook delivers it) | three different shapes for one write (verified M3) |
| 15 | Aggregated API write | ✅ M4 | **full object** (`ADDED`, spec included) | official: **empty** request/response body; proxy-enriched (`/audit-webhook-additional`): full body | validating webhook **observed** firing on this version | the body-quality cliff — and watch fills it (verified M4) |
| 16 | Watch resync (`410 Gone`) | ⬜ not yet | `ERROR`, then must relist | n/a | n/a | proves watch needs a list backstop |
| 17 | Bookmark | ⬜ not yet | `BOOKMARK` with resourceVersion | n/a | n/a | the only safe resume anchor |

The **none** cells in Rows 5–7 are not omissions — they are the reused product wiring working as
designed, because the product captures intent, not state (see [Capturing Intent, Not
State](#capturing-intent-not-state)). The audit policy (`test/e2e/cluster/audit/policy.yaml`) drops
`apps/*/status`, core `*/status`, and core `pods`, and the validating webhook
(`config/webhook/validating-webhook.yaml`) matches **top-level resources only**. So the corpus for
these rows is honestly smaller than a first reading of the matrix suggests: `/status` and `/scale`
never reach admission; `/status` and `pods` never reach audit. That the *watch* still carries the
full object in every one of these silences is precisely the "watch is viable for state" evidence the
lab exists to produce.

(The "mutating webhook precedes validation" behavior is **not** a corpus row — it is a documented
Kubernetes contract the lab cites rather than re-proves; see
[Verify vs Cite](#verify-vs-cite-what-the-lab-proves-and-what-it-only-documents). Capturing it would
require a mutating recorder the lab deliberately does not build.)

### Illustrative shapes for the hardest rows

These snippets show the *expected* shape and the normalization placeholders (`<uid-1>`, `<rv-1>`,
`<ts>`, `<auditID-1>`). The identity placeholders are *relational*: distinct values get distinct
indices and equal values repeat the same index, so identity and ordering survive normalization;
timestamps are the exception and collapse to a single `<ts>` (see [Normalization](#normalization)).
They are illustrative — the lab generates the authoritative files.

**Row 9 — deletecollection (the fan-out asymmetry).** Three ConfigMaps removed by one request.

Watch emits one `DELETED` per object; it never sees the collection verb:

```yaml
# corpus/configmap/deletecollection/watch.deleted.cm-a.yaml
# One of N. A live watch reports object-level consequences, not the collection verb.
# If the watcher is down or lagging, these events can be lost entirely; only a fresh
# list plus mark-and-sweep recovers the final state.
# The sibling files carry <uid-2>/<uid-3> and <rv-2>/<rv-3>, so the fan-out's distinct
# identities stay visible in the corpus instead of collapsing to one placeholder.
type: DELETED
object:
  apiVersion: v1
  kind: ConfigMap
  metadata:
    name: cm-a
    namespace: lab
    uid: <uid-1>
    resourceVersion: <rv-1>
  data:
    key: value
```

Audit sees a single name-less request whose *request* body is `DeleteOptions`; on this version its
*response* body is **not** shallow — it carries a `List` of the removed objects:

```yaml
# corpus/configmap/deletecollection/audit.deletecollection.yaml
# objectRef has a resource but NO name. requestObject is DeleteOptions and never names the
# removed objects; on v1.35.2 the responseObject *does* carry them as a List — so the
# shallow-body concern is real for the request, narrower for the response (see the M1 findings).
kind: Event
apiVersion: audit.k8s.io/v1
level: RequestResponse
auditID: <auditID-1>
stage: ResponseComplete
verb: deletecollection
requestURI: /api/v1/namespaces/lab/configmaps?labelSelector=lab%3Dsweep
user:
  username: kubernetes-admin
  groups: [system:masters, system:authenticated]
objectRef:
  resource: configmaps
  namespace: lab
  apiVersion: v1
responseStatus:
  metadata: {}
  code: 200
requestObject:
  kind: DeleteOptions
  apiVersion: v1
  propagationPolicy: Background
responseObject:                  # the removed objects DO appear here on this version
  kind: ConfigMapList
  apiVersion: v1
  items:
  - metadata:
      name: cm-a
      namespace: lab
      uid: <uid-1>
      resourceVersion: <rv-1>
    data:
      key: value
  # … cm-b (<uid-2>/<rv-2>) and cm-c (<uid-3>/<rv-3>) follow
stageTimestamp: <ts>
```

**Row 8 — finalizer delete (the missing audit delete).** The delete sets a tombstone; the object
lingers; a later finalizer-removal patch is what actually frees it — so the final `DELETED` watch
event has no audit `delete` verb behind it.

```yaml
# corpus/configmap/finalizer-delete/watch.modified.deletion-pending.yaml
# DELETE on a finalized object does not remove it. The apiserver sets deletionTimestamp;
# the object persists and watch reports MODIFIED. The actual removal happens later, when
# the finalizer is patched off — and that moment is a patch in audit, never a delete.
# The terminal watch.deleted.yaml file keeps the same <uid-1> but a higher <rv-2>, so the
# corpus shows it is the same object at a later resourceVersion, not a different one.
type: MODIFIED
object:
  apiVersion: v1
  kind: ConfigMap
  metadata:
    name: cm-final
    namespace: lab
    uid: <uid-1>
    resourceVersion: <rv-1>
    deletionTimestamp: <ts>
    finalizers:
    - lab.example/hold
```

**Row 11 — dry-run create (seen, never persisted).**

```yaml
# corpus/configmap/dry-run-create/admission.create.dryrun.yaml
# Reaches admission with dryRun=true. There is no watch event and no etcd object;
# the only trace is this admission record and a dry-run-flagged audit event.
request:
  uid: <uid-1>
  operation: CREATE
  dryRun: true
  userInfo:
    username: kubernetes-admin
  object:
    apiVersion: v1
    kind: ConfigMap
    metadata:
      name: cm-dry
      namespace: lab
    data:
      key: value
```

## Corpus Layout And Normalization

### Layout

The corpus is a browsable tree. One directory per scenario, one file per moment, named
`<source>.<verb-or-type>[.<discriminator>].yaml` so an ordered fan-out is self-describing:

```text
test/mutationlab/corpus/
  CLUSTER.md                      # apiserver version + k3d image the corpus was captured from
  configmap/
    create-succeeds/
      watch.added.yaml
      audit.create.yaml
      admission.create.yaml
    deletecollection/             # one request, per-object fan-out across all three mechanisms
      watch.deleted.cm-a.yaml
      watch.deleted.cm-b.yaml
      watch.deleted.cm-c.yaml
      audit.deletecollection.yaml         # the single name-less collection request
      admission.delete.cm-a.yaml          # admission fires once per object, not for the collection
      admission.delete.cm-b.yaml
      admission.delete.cm-c.yaml
  widget/                         # the two-version CRD (Row 14)
    crd-conversion/
      watch.added.yaml                    # served/storage version (v2, spec.size)
      audit.create.yaml                   # submitted version (v1, spec.sizeBytes)
      admission.create.yaml               # submitted version (v1)
      conversion.to-v1.yaml               # one representative call per direction
      conversion.to-v2.yaml
  flunder/                        # the aggregated API (Row 15)
    aggregated-api-write/
      watch.added.yaml                    # full object, spec included
      audit.create.yaml                   # official audit — empty body
      audit-additional.create.yaml        # proxy-enriched audit — full body
```

`CLUSTER.md` is load-bearing: a captured shape is only meaningful against a known apiserver
version. Pin the k3d/kind image in the harness and record the resolved server version here so the
corpus is attributable. When the cluster version bumps, regenerating the corpus and reviewing the
diff *is* the changelog of "what changed in Kubernetes between these versions."

### Normalization

Raw payloads carry volatile fields that would make every run produce a spurious diff. A single
deterministic normalizer rewrites them to stable placeholders before anything is written or
compared, so the corpus changes only when *behavior* changes.

The placeholders are **relational, not flattened**. Collapsing every UID to one `<uid>` and every
resourceVersion to one `<rv>` would erase exactly the evidence the hard rows exist to capture —
which objects in a deletecollection fan-out are distinct, which child in an owner-ref cascade is
which, that a finalizer's terminal `DELETED` is the *same* object at a *higher* resourceVersion, and
that resourceVersion actually progressed. Instead, each volatile field is replaced by an **indexed**
placeholder, scoped per scenario and assigned deterministically, so that **equal inputs map to the
same placeholder, distinct inputs to distinct placeholders, and the index order reflects real
order**:

| Field | Becomes | Indexing |
|---|---|---|
| `metadata.uid`, admission `request.uid` | `<uid-N>` | one per distinct UID, by first appearance |
| `metadata.resourceVersion` | `<rv-N>` | observed order within one resource stream; numeric order only after the lab proves the stream's RVs are orderable |
| `creationTimestamp`, `deletionTimestamp`, `stageTimestamp`, `requestReceivedTimestamp`, `managedFields[].time`, `lastTransitionTime`, `lastUpdateTime`, `startTime`, `startedAt`, `finishedAt` | `<ts>` | **collapsed to one non-relational token** — see note below |
| audit `auditID` | `<auditID-N>` | one per distinct request |
| `generateName` random suffixes | `<rand-N>` | one per distinct suffix |
| source IPs, pod/host IPs (incl. inside `managedFields` association keys) | `<ip-N>` | one per distinct value, by first appearance |
| container runtime IDs (`containerID`) | `<containerID-N>` | one per distinct value, by first appearance |
| `spec.nodeName` | `<node-N>` | one per distinct node, by first appearance |
| audit `user.extra` credential-id (`X509SHA256=…` client-cert fingerprint) | `<credential-N>` | one per distinct credential, by first appearance |

The non-timestamp categories are **relational, not flattened**: collapsing every UID to one `<uid>`
and every resourceVersion to one `<rv>` would erase exactly the evidence the hard rows exist to
capture — which objects in a deletecollection fan-out are distinct, which child in an owner-ref
cascade is which, that a finalizer's terminal `DELETED` is the *same* object at a *higher*
resourceVersion. So each is replaced by an **indexed** placeholder, assigned deterministically, so
that **equal inputs map to the same placeholder, distinct inputs to distinct placeholders, and the
index order reflects real order**.

**Timestamps are the exception: they collapse to a single non-relational `<ts>`.** An earlier
chronological `<ts-N>` scheme proved unstable once M2 captured rich objects — Kubernetes emits
timestamps at one-second granularity, so whether two near-simultaneous events (a Pod's
`creationTimestamp` and its first status condition, say) fall in the same second or adjacent seconds
varies run to run, changing how many distinct values exist and shuffling every index. Object-version
sequencing is carried by `resourceVersion` (relational, numeric) and by the moment file ordering
instead, so the timestamp value adds little and the relational form costs determinism. (If a future
row genuinely needs *timestamp* sequencing that `resourceVersion` cannot express, revisit this — but
no row to date does.)

The indexing is scenario-scoped and stable across runs: the same captured behavior always yields the
same placeholders, so the corpus stays diff-free unless identity or ordering genuinely changed.
Everything else is preserved verbatim — including `managedFields` themselves, because their growth
under server-side apply is sometimes exactly the behavior under test (M2 added one wrinkle here: a
`managedFields` *association key* can embed a volatile value, e.g. `k:{"ip":"10.42.3.14"}` for a
pod IP, so the normalizer rewrites those embedded IPs in keys as well as in values). The normalizer
lives in one place (`internal/mutationlab/normalize`) and is the *only* thing allowed to mutate a
payload on its way to disk.

### Golden workflow

- Default run: capture → normalize → compare against the committed file. A mismatch fails the test
  and prints a unified diff.
- Update run: `MUTATIONLAB_UPDATE=1 task lab-e2e` (or `task lab-corpus-update`) rewrites the
  corpus from the live capture.
- A corpus diff in a PR is a signal, not noise: either Kubernetes behavior changed, our capture
  changed, or the cluster version moved. All three deserve a human glance — which is the point.

### Validating New Kubernetes Versions

This is goal #2 made concrete, and it reuses the machinery above without adding any. The corpus is
captured from one pinned apiserver; treating a version bump as "regenerate and review the diff"
turns subtle upstream behavior changes into a test you can run on demand:

1. The corpus on `main` is the committed baseline for its pinned Kubernetes version (recorded in
   `CLUSTER.md`).
2. To vet a new release, bump the k3d/kind image in the lab harness to that version and run
   `MUTATIONLAB_UPDATE=1 task lab-e2e`.
3. The git diff of `corpus/` **is** the behavioral changelog for the upgrade — scoped to exactly the
   fine-grained behaviors GitOps Reverser depends on (verb naming, event ordering and count, body
   presence, deletecollection fan-out, finalizer sequencing, conversion/aggregated-API shapes),
   with all volatile fields normalized away so only real changes show.
4. An empty diff is a positive result: it is evidence the behaviors we rely on survived the upgrade.
   A non-empty diff is the early warning — review it, decide whether GitOps Reverser must adapt, and
   only then commit the regenerated corpus (with the bumped `CLUSTER.md`) as the new baseline.

Run this opportunistically — when a new Kubernetes minor ships, or before the project commits to
supporting one — not on every CI build. Because the lab is isolated (its own image, manifests, k3d
profile, and `task lab-e2e` target), pointing it at a different Kubernetes version never disturbs
the main suite, which stays pinned to its own supported version.

## Record Model

Both layers are driven by one envelope. The `Summary` feeds the structured assertions; the `Raw`
payload (after normalization) becomes the golden YAML.

```go
type Record struct {
    ID         string          `json:"id"`
    Source     string          `json:"source"` // watch, audit, audit-additional, admission, conversion
    Scenario   string          `json:"scenario,omitempty"`
    ObservedAt time.Time       `json:"observedAt"`
    Key        ObjectKey       `json:"key,omitempty"`
    Summary    RecordSummary   `json:"summary"`
    Raw        json.RawMessage `json:"raw"`
}

type ObjectKey struct {
    Group           string `json:"group,omitempty"`
    Version         string `json:"version,omitempty"`
    Resource        string `json:"resource,omitempty"`
    Subresource     string `json:"subresource,omitempty"`
    Namespace       string `json:"namespace,omitempty"`
    Name            string `json:"name,omitempty"`
    UID             string `json:"uid,omitempty"`
    ResourceVersion string `json:"resourceVersion,omitempty"`
}

type RecordSummary struct {
    WatchType         string `json:"watchType,omitempty"`
    AuditID           string `json:"auditID,omitempty"`
    AdmissionUID      string `json:"admissionUID,omitempty"`
    Operation         string `json:"operation,omitempty"`
    User              string `json:"user,omitempty"`
    Persisted         *bool  `json:"persisted,omitempty"`
    HasObject         bool   `json:"hasObject"`
    HasOldObject      bool   `json:"hasOldObject"`
    HasRequestObject  bool   `json:"hasRequestObject"`
    HasResponseObject bool   `json:"hasResponseObject"`
    ResponseCode      int32  `json:"responseCode,omitempty"`
}
```

The recorder should not infer too much. `Persisted` is set only by test-side correlation when the
scenario verifies the object exists or does not exist after the request — never guessed from the
payload alone.

## Isolated Test Setup

The user constraint is explicit: this must not clutter the already-complex main e2e setup. The lab
gets its *own* everything, and reuses only the integration *shape* of the real install — not its
runtime, manifests, or test suite.

- **Separate binary:** `cmd/mutation-capture-lab/main.go`, built as its own image
  (`mutation-capture-lab`), with recorders + store + normalizer under `internal/mutationlab/`.
- **Swap the image, reuse the wiring (steering decision):** rather than authoring its own audit
  policy, webhook configs, and certificates, the lab serves the **same** webhook URLs as the
  product on the same ports and TLS cert mounts, so `task lab-e2e` integrates by **swapping the
  controller image** for the lab image on the already-prepared e2e cluster
  (`test/mutationlab/swap-image.sh` patches the Deployment's image + entrypoint + args). This
  reuses the product's audit + admission wiring verbatim; there is no `test/mutationlab/manifests/`
  and no separate cluster bring-up. The trade-off — the cluster is left running the lab image — is
  restored with `task clean-cluster && task test-e2e`.
- **Separate Task targets:** `task lab-e2e` and `task lab-corpus-update`, opt-in and **serial**
  (a single `go test` package run, no Ginkgo `--procs`). They are **not** invoked by
  `task test-e2e` and **not** part of the default CI lane, and the live-cluster driver is behind
  the `mutationlab_e2e` build tag so the unit lane never needs a cluster. If the corpus is worth
  guarding in CI, give it its own manual/nightly job, not a hook into the existing one.

The app runs in-cluster and exposes the integration-relevant endpoints (the same URLs the product
serves, so the image swap needs no cluster reconfiguration):

- `POST /audit-webhook` (official kube-apiserver audit)
- `POST /audit-webhook-additional` (the `apiservice-audit-proxy` body-enrichment endpoint, recorded
  as its own source so the corpus shows what the proxy adds — and whether a live watch already
  carries it, which would make the proxy unnecessary for object content)
- `POST /validate-admission-webhook` (the recording admission endpoint — the same path as the
  product's always-allow webhook, since the lab deployment replaces the product)
- `GET /records[?scenario=<id>]` / `DELETE /records`
- `GET /healthz`, `GET /readyz`

Storage is in memory for the first version, but **clearing records between scenarios is not enough**:
audit webhook delivery is asynchronous and batched, so a late event from scenario A can land after
scenario B has already started and silently corrupt B's corpus. The lab *isolates* scenarios instead
of trusting a clean slate:

- each scenario runs in its **own ephemeral namespace** (e.g. `lab-<scenario>-<runid>`) and stamps
  every object with a `mutationlab.configbutler.ai/scenario` label, so watch/audit/admission records
  can be attributed to a scenario from the namespace or label — even for name-less requests like
  deletecollection, whose `requestURI` carries the namespace and label selector;
- the recorder sets `Record.Scenario` from that namespace/label, and reads are **filtered by
  scenario** (`GET /records?scenario=<id>`) rather than assuming the store holds only the current
  scenario's events;
- before comparing, the test performs a **bounded drain**: it waits until the expected records have
  arrived *and* the count has been quiet for a short settle window (or a timeout fails the scenario),
  so a slow audit batch is awaited rather than missed, and a stray cross-scenario event is rejected
  rather than averaged into the corpus.

The audit-enabled lane may require k3d/kind (managed clusters generally cannot configure
kube-apiserver audit webhooks); the watch/admission lane should work on any cluster where the test
can install webhooks and RBAC.

## Assertions To Capture

The structured layer asserts laws, not examples:

- A successful create produces exactly one persisted object and a watch identity for it.
- A dry-run create reaches admission and audit but produces no watch object and no etcd object.
- A request the recorder **record-and-rejects** produces an admission record but no watch object and
  no etcd object. (Validating webhooks are called in parallel
  ([Admission Webhook Good Practices](https://kubernetes.io/docs/concepts/cluster-administration/admission-webhooks-good-practices/)),
  so "rejected by a *later* webhook" is not a dependable ordering; the deterministic, self-contained
  version is for the lab recorder itself to reject. A rejection by a separate parallel webhook is an
  *observed* scenario asserted tolerantly: the recorder is still called, but whether it ran before or
  after the rejecter is not guaranteed.)
- A deletecollection produces per-object watch deletes equal in count to the objects removed, and a
  matching per-object validating-admission `DELETE` for each (admission fires once per object, not
  once for the collection), while audit sees a single name-less collection request.
- A finalizer delete's terminal `DELETED` watch event has no corresponding audit `delete` verb.
- A watch restarted from an expired resourceVersion surfaces `ERROR` and must relist before any
  correctness claim.

## Expected Conclusions

The corpus should make these conclusions mechanically visible.

### Watch Is Viable For State

Watch mode can be an honest state-mirroring mode: simple to install, works in managed clusters,
sees the final stored object shape, handles collection deletes as per-object deletions while live,
and needs a periodic/full reconcile as the correctness backstop. Its product contract should say
that commits are authored by the operator unless audit enrichment is also enabled.

**The corpus now confirms the shallow-fill hypothesis** (M1 Row 9 and M4 Row 15), so the sharper
consequence is no longer conditional: because the live watch event carries the full object exactly
where audit goes shallow, a watch-based capture would not need the `apiservice-audit-proxy` /
body-enrichment path at all for object *content* — watch supplies natively what that proxy
reconstructs. Two captured data points, side by side in the corpus:

- **`deletecollection` (Row 9):** the audit *request* body is only `DeleteOptions` (name-less), yet
  each of the N watch `DELETED` events carries the full removed object.
- **aggregated API (Row 15):** the official kube-apiserver audit event carries **no `requestObject`
  and no `responseObject`** at all, yet the watch `ADDED` carries the full flunder (spec included).
  The `apiservice-audit-proxy` posts the reconstructed body to `/audit-webhook-additional` — the very
  enrichment a watch-based capture would not require.

The caveat is unchanged and must be stated alongside the result: this holds only **while the watch is
live**, with the periodic reconcile as the gap backstop (a watch that is down or lagging loses the
event and its body outright). Body-enrichment is therefore an **audit-mode** concern, not a universal
requirement — which is the honest, product-relevant answer to Issue #168.

### Audit Is Viable For Provenance

Audit remains the high-fidelity mode: real user identity, request context, stronger commit
attribution. But it is operationally harder and has body-quality edge cases, especially aggregated
APIs without a body-enrichment path (Row 15).

### Admission Is Not Viable For Persistence

Validating admission is not a trustworthy source for Git history: it can observe writes that never
persist (Rows 11, 12); it runs *after* mutating admission (a documented ordering the lab cites — see
[Verify vs Cite](#verify-vs-cite-what-the-lab-proves-and-what-it-only-documents)) but *before* storage
defaulting/conversion, so the object it sees matches neither the user's original submission nor the
persisted result (Row 14); validating webhooks run in parallel, so it can never count on having
the final say; and it cannot prove etcd accepted the object. Useful as a teaching comparison; not a
product capture mechanism.

## Product Framing And Decision Gate

> This section is secondary. It is the *use* of the corpus, not the corpus itself.

If GitOps Reverser adds a watch-only mode, it should be deliberately simpler than audit mode
(`spec.mode: watch`, or a global `captureMode: watch`), promising "keeps Git aligned with selected
Kubernetes state," "no kube-apiserver audit configuration," and explicitly *not* "real end-user
attribution." Users who need trustworthy attribution and managed capture infrastructure are the
managed-version audience — an honest split.

After the minimal lab exists, decide explicitly between three paths:

| Path | When to choose it |
|---|---|
| Skip watch mode | The corpus shows watch-only creates too many confusing edge cases. Stay audit-backed. |
| Add watch mode | The corpus shows a small, understandable state-only contract: correct after live watch plus periodic reconcile, operator-authored commits. |
| Keep provenance managed | The corpus confirms real-user attribution needs an operated audit/enrichment pipeline; document that as the managed-version value. |

The lab's job is not to justify a feature in advance. It is to make the smallest elegant answer
obvious — including the possibility that the right answer is to not add the mode at all.

## Milestones

Each milestone ends with committed corpus files and green invariants. The lab is valuable only while
it stays small enough that the behavior is obvious, so each milestone is a deliberate stop-and-read
point, not a runway to the next.

- **M0 — Harness skeleton. ✅ done.** Lab binary, in-memory store, normalizer, golden-file
  compare/update plumbing, `task lab-e2e` (swap-image model). The loop (capture → normalize → write →
  diff) proven on a single create.
- **M1 — ConfigMap core moments. ✅ done.** The rows captured so far — Rows 1–2, 9, 11–12 — across the
  watch/admission and audit lanes. (Rows 3, 4, 8, 10, 13, 16–17 remain to fill in.) Corpus and
  `CLUSTER.md` committed.
- **M2 — Workload subresources and grace-period delete. ✅ done.** A `Deployment` for Row 5
  (`/status`) and Row 6 (`/scale`), and a `Pod` for Row 7 (the two-event graceful delete). The
  headline finding is mechanism silence under the reused product wiring (audit drops `*/status` and
  `pods`; the webhook ignores subresources) — see the Status section. Corpus committed and verified
  deterministic.
- **M3 — CRD + conversion. ✅ done** (see [M3 design](#m3-design--crd--conversion) below). The
  two-version `Widget` CRD with a conversion webhook captures Row 14: the
  submitted-vs-stored/served-vs-conversion divergence. As designed, it added exactly one cluster
  object (the CRD), installed by the scenario at runtime and removed on teardown; the conversion
  webhook reuses the admission cert/port at `/convert`, so no new certificate. Corpus committed and
  verified deterministic.
- **M4 — Aggregated API (Row 15). ✅ done.** The wardle `flunders` aggregated API behind the
  `apiservice-audit-proxy`. The headline result: the official audit body is **empty** while the watch
  carries the **full object** — see the Status section and
  [Watch Is Viable For State](#watch-is-viable-for-state). Corpus committed and verified deterministic.

Take the watch-vs-audit-vs-hybrid decision-gate conversation back to Issue #168 with the corpus as
the evidence: M1+M2 show "watch is the lone witness for `/status` and pod deletes," and M4 shows
"watch carries the full object where the aggregated-API audit body is empty" — together the
load-bearing evidence that watch is viable for state without the body-enrichment proxy.

### M3 design — CRD + conversion

> Status: **built** (M3 ✅). This section is the design that shipped; the as-built scenario installs
> the CRD programmatically at runtime (reading the conversion-webhook service + CA from the product's
> validating webhook config) and removes it on teardown, so the only standing footprint is one CRD
> while the lab runs. M1+M2 added zero cluster objects; M3 adds exactly one.

**What Row 14 needs.** One write to a multi-version CRD can produce three *different* shapes: the
object the user **submitted**, the object **admission** validated, and the object **persisted** (in
the storage version) and re-served in another version. To make that divergence visible and
deterministic, the lab needs a CRD with two served versions (say `v1` and `v2`) whose schemas differ
by a real, webhook-converted field (e.g. `v1.spec.sizeBytes: integer` ⇄ `v2.spec.size: string`), one
of them the storage version. A `strategy: None` conversion only swaps `apiVersion`; a genuine field
rename requires a **conversion webhook**.

**Why a CRD is the right vehicle (and why it is also easier than M2).** Unlike a Deployment or Pod, a
lab CRD has **no controller**, so there is no status churn or clobber — the watch stream is exactly
the writes the test makes. The determinism work M2 needed (paused deployments, record selection) is
unnecessary here. The cost is purely the install footprint.

**Footprint: one CRD, reusing the existing cert and port.** The conversion webhook needs **no** new
certificate and **no** new server:

- the CRD's `spec.conversion.webhook.clientConfig` points at the existing `gitops-reverser` service
  on the existing admission port at a **new path, `/convert`**, served by the lab binary alongside
  `/validate-admission-webhook` on the same TLS listener;
- the CA bundle is **read from the product's validating-webhook config at runtime** and inlined into
  the CRD's `clientConfig.caBundle`, so the conversion webhook trusts the same admission `Certificate`
  with no new cert (`admissionServiceAndCA` in `m3_scenarios_test.go` reads the service + CA off that
  webhook config — not a cert-manager `inject-ca-from` annotation);
- RBAC needs nothing new: the controller already has `*` get/list/watch (so the lab can watch the
  CR), and the live-cluster driver authenticates as the kubeconfig admin (so it can create both the
  CRD and the CRs).

So the only genuinely new cluster object is the **CRD** itself, created by the driver before the
scenario runs and removed on teardown (`t.Cleanup`, or left for `task clean-cluster`).

**matchPolicy — the reused webhook already observes the submitted version.** An earlier draft said
the lab would *pin* `matchPolicy: Exact` to guarantee the recorder sees the submitted version. With
the swap-image model the lab instead reuses the product's webhook, which is `matchPolicy: Equivalent`
matching `apiVersions: ['*']`. Because the rule matches *every* version, the request's version is
always directly matched and the apiserver performs **no conversion before calling** — so the recorder
already observes the **submitted** version, and recording `request.requestKind`/`requestResource`
makes any conversion visible rather than silent. A dedicated `Exact` webhook for the CRD is therefore
**not required** for Row 14; it stays an optional extra only if a future need for strict per-version
rules appears (and would itself be new webhook-config footprint). This supersedes the "lab pins
`matchPolicy: Exact`" note in [Mechanisms Under Test](#3-validating-admission-webhook) and the Row 14
matrix entry.

**As built.** M3 shipped exactly this, with the install done programmatically by the driver rather
than from a checked-in manifest:

1. `widgetCRDObject` (`m3_scenarios_test.go`) builds the two-version `Widget` CRD in Go — `v1`
   (`spec.sizeBytes: integer`), `v2` (storage, `spec.size: string`), and the conversion-webhook
   `clientConfig` — and the driver creates it with the dynamic client, waits for `Established`, and
   deletes it on teardown.
2. A `/convert` handler in the lab binary implements the `v1`⇄`v2` field rename and **records each
   `ConversionReview`** as the new `conversion` source (so the corpus shows what the apiserver asked
   the webhook to convert, and in which direction) — the conversion path is itself a behavior worth a
   corpus row.
3. The lab watches the CR on its `v2` storage version; the scenario creates in `v1` and captures the
   admission (submitted `v1`), audit (submitted `v1`), watch (stored/served `v2`), and both
   conversion directions side by side.

The earlier "open decision" — whether to add one CRD to the lab's cluster footprint — was approved;
M1+M2 added zero cluster objects, M3 adds exactly one, self-contained and removed on teardown.
