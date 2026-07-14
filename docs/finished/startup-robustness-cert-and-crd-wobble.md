# Startup robustness: the audit-cert mount race & the CRD-discovery wobble

> **finished** — shipped or closed. Kept for context only; **nothing here binds**. For current behaviour see [`../spec/`](../spec/). Index: [`../INDEX.md`](../INDEX.md)

This document describes two distinct, pre-existing reliability hazards that surface as a single
intermittent e2e failure, explains the mechanisms precisely, and sketches candidate fixes with
trade-offs. Both are **independent of the api-source-of-truth / R3 work** — they predate it and
live in the startup path and the discovery→catalog→registry pipeline.

The two hazards:

1. **The audit-cert startup mount race** — on a fresh deploy the controller pod cannot start
   until cert-manager has issued its audit TLS secrets; until it starts, the audit webhook is
   not serving, so audit events the apiserver tries to deliver in that window are buffered or
   dropped.
2. **The CRD-discovery wobble** — a just-installed (or transiently-undiscovered) CRD can blink
   out of API discovery; if the blink outlives the registry's grace, the type's checkpoint is
   released and its tail stopped, so a resource created in that window is not mirrored.

---

## 1. The observed failure

The e2e spec `Manager CRD Lifecycle › should create Git commit when IceCreamOrder is added via
WatchRule` ([crd_lifecycle_e2e_test.go:182](../../test/e2e/crd_lifecycle_e2e_test.go#L182))
installs an `IceCreamOrder` CRD, creates a WatchRule + GitTarget, creates one custom resource,
and expects it committed within ~45 s ([:297](../../test/e2e/crd_lifecycle_e2e_test.go#L297)).

It is a **load-dependent flake**:

| Context | Result |
|---|---|
| Full suite, procs=4 (run10) | **failed** — CR not committed in 45 s |
| Same spec, focused, procs=1 | **passed** (5/5) |
| Full suite procs=1/procs=4, runs 5–9 | passed, on identical controller code |

Evidence in the controller logs around the failure:

- `MountVolume.SetUp failed for volume "audit-webhook-certs" : secret "audit-server-cert" not found`
  and `… "audit-client-ca" : secret "gitops-reverser-audit-root-ca" not found` at pod startup.
- `objects-mirror: snapshot cleared` for `icecreamorders` (the type's checkpoint was **Released**).

Those two lines are the fingerprints of the two mechanisms below.

---

## 2. Mechanism A — the audit-cert startup mount race

### Wiring

The audit ingress server is mutual-TLS. cert-manager builds the material in a **multi-step
chain** ([config/certs/certificates.yaml](../../config/certs/certificates.yaml),
[config/certs/issuer.yaml](../../config/certs/issuer.yaml)):

```
selfSigned Issuer
   └── Certificate gitops-reverser-audit-root-ca   (isCA, secret: gitops-reverser-audit-root-ca)
          └── CA Issuer gitops-reverser-audit-ca-issuer
                 ├── Certificate audit-server-cert  (server auth, secret: audit-server-cert)
                 └── Certificate audit-client-cert  (client auth, secret: audit-client-cert)
```

The Deployment mounts two of those secrets as **non-optional** secret volumes
([config/deployment.yaml](../../config/deployment.yaml#L100-L107)):

- `audit-webhook-certs` ← `audit-server-cert` → `/tmp/k8s-audit-server/audit-server-certs`
- `audit-client-ca` ← `gitops-reverser-audit-root-ca` → `/tmp/k8s-audit-server/audit-client-ca`

### The race

On a fresh apply, the Deployment and all the Certificates land together. The pod is scheduled
immediately, but the cert chain has to reconcile step-by-step (self-signed issuer → root CA
secret → CA issuer ready → server/client certs). Because the secret volumes are **not
`optional`**, the kubelet **blocks the pod** until both secrets exist, emitting `FailedMount`
and retrying with exponential backoff. On a cold cluster this delays the container start by
tens of seconds up to ~2 minutes.

### Consequence

The controller process only starts once the secrets are mounted, so the in-process cert load
is fine — and rotation is handled too: the audit server is created with a reload **watcher**
([cmd/main.go:407](../../cmd/main.go#L407)), so a cert-manager renewal (`renewBefore` on the
server cert) is picked up without a restart.

The damage is purely the **startup delay**: while the pod is stuck mounting, the audit webhook
is not serving TLS, so the kube-apiserver's audit backend cannot deliver events. The apiserver
buffers them and, if the buffer fills or the per-event delivery times out, **drops** them. An
early spec that creates a resource during this window can lose the very event it is waiting on.

> Note: this is a *fresh-cluster* hazard. In a steady cluster the certs already exist, so a pod
> reschedule mounts instantly. It bites e2e because every run starts cold.

---

## 3. Mechanism B — the CRD-discovery wobble

### The pipeline

Followability flows: **kube discovery → API-resource catalog → typeset Registry → Materializer**.

- **Catalog** ([internal/watch/api_resource_catalog.go](../../internal/watch/api_resource_catalog.go))
  refreshes from `disco.ServerGroupsAndResources()`. It is already resilient to *errored*
  discovery: a group reported via `IsGroupDiscoveryFailedError` is kept (marked `degraded`, its
  prior entries retained), and undiscovered group-versions are pruned **only when the scan is
  `complete` (`err == nil`)** ([Refresh, :174](../../internal/watch/api_resource_catalog.go#L174)).
- **Registry** ([internal/typeset/registry.go](../../internal/typeset/registry.go)) turns
  catalog observations into a verdict per type and applies the **live-set grace**: *additions
  fast, removals slow*. A type that stops being observed is held `Retained` for
  `RemovalGrace = 60 s` ([:33](../../internal/typeset/registry.go#L33)) before it becomes
  `Refused`.
- The verdict transitions emit lifecycle events
  ([internal/typeset/lifecycle.go](../../internal/typeset/lifecycle.go)):
  `Followable → Retained` ⇒ **`TypeWobbling`**; `Retained → Followable` ⇒ `TypeRecovered`;
  a full drop ⇒ `TypeRemoved` / `TypeRefused`.
- **Materializer** ([internal/typeset/materializer.go](../../internal/typeset/materializer.go))
  reacts: `TypeWobbling` **freezes** the type (suspends re-anchor/sweep but **keeps the
  checkpoint served and the tail running**); `TypeRemoved`/`TypeRefused` **force-releases** the
  checkpoint, and the watch layer then stops the tail and clears `:objects`.

So short blinks are *designed for*: a `Followable → Retained → Followable` round-trip inside
60 s is absorbed as a freeze that never disrupts mirroring.

### Where it still breaks

Two gaps remain:

1. **A "complete" scan that simply omits a group.** The catalog only retains a group when
   discovery *errors* on it. If a transient hiccup makes `ServerGroupsAndResources()` return a
   scan with **no error** that just doesn't list the CRD's group, that group is "undiscovered in
   a complete scan" → pruned ([removeUndiscoveredGroupVersions](../../internal/watch/api_resource_catalog.go#L251)).
   The registry then starts the 60 s grace; if the group stays missing past 60 s (or flaps
   repeatedly), the type is `Refused` → checkpoint released → tail stopped. A CR created in that
   window is not mirrored.
2. **A freshly-installed CRD that has not stabilised.** The spec installs the CRD and almost
   immediately creates the CR. For the CR to commit within 45 s the whole chain must complete:
   *CRD Established → discovery lists it → Registry `Followable` → Materializer claim → checkpoint
   LIST → `TypeSynced` → tail started → CR audit event flows → commit*. A just-Established CRD's
   discovery endpoint can blink during this settling, and the demand-driven checkpoint LIST adds
   latency. Under procs=4 the discovery client is contended by many CRDs at once, so the blink is
   more likely — and the 45 s budget is tight relative to the chain plus any blink.

The `snapshot cleared` log line is this path: the type's checkpoint was released (either by a
>60 s refusal, or by the suite's end-of-spec CRD teardown).

---

## 4. How A and B combine

They are independent but compounding, and both are worse under load:

- A delays the **whole pipeline** (no webhook → no events) early in the run.
- B can **release an established type** mid-run, or prevent a fresh CRD from stabilising in time.
- procs=4 raises the probability of each (cold start contends with N parallel specs; discovery
  is contended by N CRDs). That is exactly why the spec passes focused/procs=1 and flakes at
  procs=4.

---

## 5. Potential solutions

Ordered roughly by leverage. None is required for R3; they harden a pre-existing seam.

### For the cert race (A)

- **A1 — Make the audit pipeline a readiness signal, and gate the e2e suite on it.** The pod
  becoming `Ready` is not the same as the audit webhook being reachable from the apiserver. Add
  a readiness probe / status condition that is only true once the audit server is serving TLS,
  and have `task prepare-e2e` wait for it before the first event-producing spec. *Lowest-risk,
  test-deterministic; does not change production wiring.*
- **A2 — Order the apply: certs (and their secrets) before the Deployment.** A kustomize/helm
  ordering or a wait-for-secret step removes the kubelet mount-retry entirely. Clean for fresh
  installs.
- **A3 — initContainer that blocks on the cert secrets.** Equivalent to A2 without changing
  apply order; the main container starts only once the certs exist (and the `FailedMount` churn
  moves to a quiet init wait).
- **A4 — Tune the apiserver audit-webhook buffer/retry** (`--audit-webhook-batch-*`) so events
  are not dropped during a brief webhook outage. This is kube-apiserver (k3s) config, not ours,
  so it is a deployment-environment mitigation, not a code fix.

**Recommendation:** A1 + A2 — make audit-pipeline readiness explicit and ensure the certs exist
before the Deployment. The kubelet retry already self-heals; the goal is to make startup
*deterministic* so events are never produced before the webhook can receive them.

### For the CRD wobble (B)

- **B1 — Confirm removals over multiple complete scans (extend "removals slow" into the
  catalog).** Today a single complete scan that omits a previously-served group prunes it. Treat
  a disappearance the way an error is treated: keep the group (degraded) and require *N
  consecutive* complete scans without it — or a confirmation window — before removal. This is the
  root fix and matches the existing retain-on-failure philosophy. *Risk: genuine removals are
  delayed by the confirmation window (acceptable — removals are already slow by design).*
- **B2 — Don't force-release a recently-served checkpoint on a brief refusal.** Give
  `TypeRemoved`/`TypeRefused` the same "keep serving for a grace" treatment that `TypeWobbling`
  gets, so a flap that briefly exceeds 60 s does not drop the mirror; release only after a longer
  confirmed-gone window. Aligns with the "trust the order, keep replaying" model. *Risk: serves a
  stale checkpoint slightly longer for a genuinely-removed type.*
- **B3 — Lengthen / stage the grace for freshly-Established CRDs.** The fixed 60 s
  `RemovalGrace` is generous for steady types but the *settling* of a brand-new CRD is the real
  problem; a startup-stabilisation window (or a longer grace while a CRD is freshly Established)
  absorbs the blink. *Risk: tuning knob; least principled.*
- **B4 — Stabilise discovery at the source.** Investigate why k3s `ServerGroupsAndResources()`
  transiently omits a just-Established CRD; wait for the CRD's discovery to be stable before
  declaring it Followable, or use a discovery client with short-retry/caching so one bad scan
  does not propagate.
- **B5 — De-flake the test.** Have the spec wait for the type to be *stably followable* (e.g.
  poll the GitTarget materialization status until the type is `Synced`) before creating the CR.
  This masks rather than fixes the hazard, but removes the e2e noise while B1/B2 are designed.

**Recommendation:** **B1 (root fix) + B2 (defence in depth).** Both extend the project's
"additions fast, removals slow" principle the one place it currently isn't applied — the
catalog's prune decision and the materializer's release — so a discovery blink can never drop a
live mirror. B5 is a reasonable interim to keep CI green.

> **Steering update (2026-06-11):** B1 will NOT be implemented inside the catalog — the
> catalog stays a thin discovery wrapper with no time-sensitive state. The omission-grace
> mechanism moves into the typeset registry instead; see
> [typeset-owns-discovery-grace.md](../spec/typeset-owns-discovery-grace.md) for the revised
> plan (B2 lives on there as stage S4).
>
> **Implemented (same day):** the relocation landed (S1–S3) — the catalog is now a
> per-scan normalizer and `Registry.UpdateFromScan` owns retain-on-error + the omission
> grace, with the instant prune gone. Mechanism-B gap 1 above is therefore CLOSED; gap 2
> (fresh-CRD settling under a tight test budget) and the B2 force-release confirmation
> remain open as S4.

---

## 6. Scope

- Both hazards are **pre-existing** and orthogonal to the api-source-of-truth / R3 changes.
- A is a fresh-deploy startup ordering issue; B is a discovery→catalog→registry confirmation gap.
- The current code already has real resilience (cert reload watcher; catalog retain-on-error;
  registry 60 s grace + `TypeWobbling` freeze) — the fixes above close the two specific gaps
  where that resilience does not yet reach.
