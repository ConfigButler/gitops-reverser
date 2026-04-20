# E2E Aggregated APIServer Proxy Hookup Plan

This document records the plan for hooking the audit pass-through APIServer prototype into the
main project's e2e environment.

## Branch Context

This work is happening on a branch, so it is acceptable to adjust the existing
`test/e2e/setup/manifests/sample-apiserver/` path directly for the first integration pass.

That means we do **not** need to preserve the current sample-apiserver manifest shape as a stable
baseline during this spike. If the cleanest path is to repoint the existing `APIService`,
`Deployment`, and `Service` wiring in place, that is fine.

Even so, keep the old direct-wiring manifests recoverable in git history rather than deleting them
without a clear replacement. This is still a spike.

## Goal

Replace the current e2e aggregated API request path:

`kube-apiserver -> sample-apiserver`

with:

`kube-apiserver -> audit pass-through proxy -> sample-apiserver`

while continuing to send the resulting synthetic `audit.k8s.io/v1` `EventList` payloads into the
existing GitOps Reverser audit receiver.

## Non-Goals

- Do not solve duplicate suppression in this integration pass.
- Do not solve delegated header trust in this integration pass.
- Do not require production-grade security posture before the first e2e spike works.

## Recommended Hookup Shape

### 1. Split the current Wardle backend into proxy front door and real backend

Keep the existing sample-apiserver container as the real backend, but stop exposing it as the
direct `APIService` target.

Target shape:

- `APIService v1alpha1.wardle.example.com` points to a new proxy `Service`
- proxy `Deployment` forwards to the real sample-apiserver `Service`
- sample-apiserver remains responsible only for serving the Wardle API itself
- the colocated etcd sidecar stays with the real backend `Deployment`; do not split it away from
  the sample-apiserver pod in the first pass

Concrete impact:

- update `test/e2e/setup/manifests/sample-apiserver/apiservice.yaml`
- update `test/e2e/setup/manifests/sample-apiserver/service.yaml`
- update `test/e2e/setup/manifests/sample-apiserver/deployment.yaml`
- likely add one new proxy `Deployment`, one new proxy `Service`, and one Secret/ConfigMap mount

Name the services explicitly up front so edits stay unambiguous:

- current `api` Service becomes the proxy-facing `APIService` target
- add a distinct backend Service such as `api-backend`

If different names are chosen, pin them consistently in the manifests and readiness flow instead of
leaving "proxy Service" and "real backend Service" abstract.

### 2. Build and load the prototype image into the k3d e2e cluster

Add a narrow e2e helper target that:

- builds `external-prototype/audit-pass-through-apiserver`
- tags it with a local e2e image reference
- imports that image into the k3d cluster

This should reuse the same image loading style already used by the main project where possible.

### 3. Use the existing sample-apiserver-ready flow, but retarget it

The current readiness flow already waits for:

- the Wardle deployment rollout
- `apiservice/v1alpha1.wardle.example.com`
- `kubectl api-resources --api-group=wardle.example.com`

For the proxy hookup, keep the same high-level readiness checks, but ensure they now validate the
proxy-backed path rather than the old direct path.

## Kubeconfig Bootstrap Strategy

### Short answer

Yes, reusing the Kubernetes audit webhook kubeconfig model will help a lot for the first spike.

### Practical recommendation

For the **first usable e2e integration**, the simplest path is:

- reuse the **same webhook endpoint**
- reuse the **same cluster ID path**
- reuse the **same trust model and certificate materials**
- provide the proxy pod its **own mounted kubeconfig Secret**, even if that kubeconfig initially
  contains the same endpoint and client credentials as the kube-apiserver webhook config

That gives the proxy a working outbound path quickly, using infrastructure the e2e environment
already knows how to provision.

### Important nuance

The most useful bootstrap is to reuse the **contents/model**, not necessarily the exact
control-plane-mounted file path.

The kube-apiserver bootstrap kubeconfig currently exists for the host/control-plane path. The proxy
pod should instead get a Kubernetes Secret or projected file inside the pod, but the data inside it
can initially mirror the same endpoint, CA, and client credentials.

### Why this is acceptable for the spike

- it minimizes new TLS plumbing
- it reduces the number of moving parts in the first end-to-end test
- it validates the real question: does the richer synthetic `EventList` reach the existing audit
  ingestion pipeline and produce useful downstream state?

### What not to overcomplicate yet

Do not block the first e2e hookup on:

- a distinct proxy client identity
- a separate CA hierarchy
- a separate audit receiver endpoint
- a full production-grade separation of responsibilities

Those can be follow-up improvements once the basic proxy-backed e2e scenario works.

## Prerequisites Before Phase 1 Starts

### Inbound TLS on the proxy is now available

The prototype now supports inbound HTTPS directly via:

- `--tls-cert-file`
- `--tls-private-key-file`

That clears the biggest runtime prerequisite for putting it behind `APIService`.

Why this still matters in the e2e wiring:

- kube-apiserver will dial the aggregated backend over HTTPS
- `spec.insecureSkipTLSVerify: true` on `APIService` only skips certificate verification
- it does **not** let the backend speak plain HTTP on port 443

The e2e plan should keep using the native in-process TLS path unless a sidecar is explicitly
chosen later.

### Backend TLS is explicit too

The real sample-apiserver is also reached over HTTPS, and the prototype now makes that trust mode
explicit with:

- `--backend-insecure-skip-verify`
- `--backend-ca-file`
- `--backend-client-cert-file`
- `--backend-client-key-file`
- `--backend-server-name`

For the first spike, `--backend-insecure-skip-verify=true` is still the expected bootstrap mode.

### Backend caller identity matters too

The real sample-apiserver may still need the immediate caller to authenticate successfully before it
will serve the proxied request. In the proxy topology, that immediate caller is the proxy pod, not
the kube-apiserver.

So the first workable e2e path needs one explicit backend-caller story:

- give the proxy its own client certificate for the backend hop
- configure the sample-apiserver to trust that client certificate chain
- use that backend client identity only to get the request accepted by the backend

The synthetic audit event is still built from the inbound delegated `X-Remote-*` headers captured
at the proxy boundary.

### Aggregator auth is network-trust-first in the spike

If the proxy is deployed without `--client-ca-file` style verification, it will accept and use
delegated `X-Remote-*` headers without authenticating the aggregator client certificate first.

That is acceptable for the first spike only if it is stated plainly:

- the first spike effectively trusts the cluster network path and service topology

This is still aligned with the non-goal that delegated header trust is not being solved here.
The prototype currently derives trust from deployment topology / network path, not from verified
aggregator client identity.

## First Usable Runtime Wiring

The first usable e2e runtime wiring needs two categories of flags:

### Implemented now

- `--listen-address`
- `--backend-url`
- `--backend-insecure-skip-verify`
- `--backend-ca-file`
- `--backend-client-cert-file`
- `--backend-client-key-file`
- `--backend-server-name`
- `--webhook-kubeconfig`
- `--webhook-timeout`
- `--max-audit-body-bytes`
- `--capture-temp-dir`
- `--tls-cert-file`
- `--tls-private-key-file`

### Still optional for the first spike

- `--client-ca-file`

Suggested first-pass values in e2e after those prerequisites land:

- `--listen-address=:9445`
- `--backend-url=https://<real-sample-apiserver-service>.<namespace>.svc:443`
- `--backend-insecure-skip-verify=true` for the first spike, unless backend CA wiring lands first
- `--webhook-kubeconfig=/etc/audit-pass-through/webhook/kubeconfig`
- `--webhook-timeout=5s`
- `--max-audit-body-bytes=1048576`
- `--capture-temp-dir=/tmp`
- `--tls-cert-file=/etc/audit-pass-through/tls/tls.crt`
- `--tls-private-key-file=/etc/audit-pass-through/tls/tls.key`

## Concrete Implementation Steps

### Phase 1: Plumbing

1. Add an image build/load step for the prototype under `test/e2e/Taskfile.yml`.
2. Add proxy manifests under `test/e2e/setup/manifests/sample-apiserver/` or a nearby sibling path.
3. Land the prototype prerequisites from [`external-prototype/audit-pass-through-apiserver/TODO.md`](../../external-prototype/audit-pass-through-apiserver/TODO.md):
   - inbound TLS
   - backend TLS behavior
4. Add a Secret or projected volume containing the proxy outbound webhook kubeconfig.
5. Add a Secret or projected volume containing the proxy inbound serving certificate and key.
6. Point the existing `APIService` at the proxy `Service`.
7. Retain the real sample-apiserver plus its etcd sidecar as an internal backend `Service`.

### Phase 2: Readiness

1. Extend `_sample-apiserver-ready` so it waits for the proxy deployment too.
   In practice, the existing task already keys off the manifest glob under
   `test/e2e/setup/manifests/sample-apiserver/**/*.yaml`, so adding the proxy manifests there is
   enough to make the timestamp trigger rerun.
2. Continue waiting for `apiservice/v1alpha1.wardle.example.com` to become `Available`.
3. Continue asserting that `flunders` appear in API discovery.

### Phase 3: Verification

Add one focused e2e scenario that:

1. creates a `Flunder`
2. waits for the audit pipeline to ingest the resulting event
3. verifies that the downstream event includes the fields this prototype is intended to recover:
   - `objectRef.name`
   - `requestObject`
   - `responseObject`

For the first pass, `create` is enough. `update` and `delete` can follow after that path is stable.

## Suggested First Assertion Strategy

Do not start by asserting the full Git write-back path.

The tighter first proof is:

- the proxy-backed aggregated request succeeds
- the existing audit receiver accepts the proxy's `EventList`
- the queued or dumped event contains the richer fields absent from native kube aggregated audit

That narrows failures to the load-bearing integration point.

## Main Risks

- inbound TLS on the proxy is the one prerequisite that must be resolved before Phase 1 is treated
  as ready to start
- proxy outbound kubeconfig/Secret mounting may be the fiddliest part
- backend TLS behavior must be explicit rather than assumed
- the `APIService` must still become `Available` once the proxy is inserted
- backend service naming must stay unambiguous after the split
- the first integration should avoid conflating proxy correctness with unrelated Git write-back
  issues

## Recommended First Cut

The fastest credible first cut is:

1. directly modify the existing sample-apiserver manifest path on this branch
2. insert the proxy as the new `APIService` backend
3. give the proxy a kubeconfig Secret that mirrors the existing audit webhook receiver setup
4. add one focused e2e case for aggregated `create`

That is enough to validate the integration premise before investing in cleanup or stronger
separation.
