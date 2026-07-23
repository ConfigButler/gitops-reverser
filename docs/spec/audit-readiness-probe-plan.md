# Audit-pipeline readiness probe: improvement plan

> **Status (2026-06-12): implemented** — `/readyz` now gates on the audit listener serving, the
> TLS cert being loaded, and a startup-only Redis connection; `/healthz` stays a bare `Ping`. The
> e2e harness's live-pipeline gate (`a133137`) is intentionally **kept** for now: it proves the
> apiserver→manager path, which `/readyz` cannot assert (readiness only checks local
> preconditions). §7's e2e simplification remains a future follow-up.

## Summary

Today both Kubernetes probes are wired to `healthz.Ping` — a bare "the process is alive"
check that knows nothing about whether the audit ingress path can actually receive events
([cmd/main.go:893-894](../../cmd/main.go#L893-L894)). The pod reports `Ready` the instant its
probe server binds, even though the audit TLS cert may not be loaded and the audit listener may
not yet be accepting connections. On a fresh deploy or a control-plane restart this opens a
window where the kube-apiserver delivers audit events to a pod that silently can't process them —
the "first events don't enter the system" failure that the e2e suite currently works around
externally (`a133137`: drive a throwaway audited write and tail the manager log until receipt).

This plan makes `/readyz` reflect the **locally-checkable audit-serving preconditions** so that,
in the wild, the kube-apiserver stops routing audit traffic to a pod that isn't ready to receive
it — without the e2e's external probing. It is the production implementation of recommendation
**A1** in
[startup-robustness-cert-and-crd-wobble.md](../finished/startup-robustness-cert-and-crd-wobble.md#L163-L167).

---

## 1. Why readiness is the right lever (and liveness is not)

The kube-apiserver reaches the manager **through a Service** — the chart and the e2e kubeconfig
both target `gitops-reverser-audit.<ns>.svc.cluster.local:9444`
(prepare-sample-apiserver-proxy-webhook-kubeconfig.sh:17),
and the pod already carries a `readinessProbe → /readyz`
([charts/.../deployment.yaml:124-132](../../charts/gitops-reverser/templates/deployment.yaml#L124-L132),
[config/deployment.yaml:50-66](../../config/deployment.yaml#L50-L66)).

That means **readiness gates Service endpoints.** A not-ready pod is removed from the Service's
endpoint set, so kube-proxy / EndpointSlices stop routing to it. Combined with the recommended
`--audit-webhook-mode=batch` (the apiserver buffers and retries on delivery failure), an accurate
`/readyz` produces the correct behaviour during rollout and cert rotation:

> pod not yet serving → out of endpoints → apiserver gets no-endpoint / connection-refused →
> buffers and retries → delivers once the pod is ready.

instead of today's:

> pod `Ready` but not serving → apiserver `200`s (or `500`s) into a pod that drops → events lost.

| Probe | Today | Plan | Rationale |
|---|---|---|---|
| `/healthz` (liveness) | `Ping` | **keep `Ping`** | A cert/Redis problem is **not** fixed by restarting the pod; gating liveness on it just crashloops while an external dependency is down. |
| `/readyz` (readiness) | `Ping` | **gate on audit-serving preconditions** | Readiness controls endpoint membership; this is exactly where "am I able to receive audit events" belongs. |

---

## 2. The trap to design around

**Readiness must never depend on having _received_ an audit event.** Because readiness gates
endpoints, a "ready only after I've seen traffic" check deadlocks:

> no endpoint → apiserver sends no traffic → pod never sees an event → never ready → never an
> endpoint.

So the e2e's "drive a write and watch the log until receipt" is correct **as an external test
probe**, but it must never become the in-process `/readyz` criterion. `/readyz` may only assert
preconditions the pod can verify **on its own**, with no inbound dependency:

1. the audit TLS cert is loaded and parseable, and
2. the audit ingress listener is bound and accepting.

(Plus the startup-only Redis gate — see §4.)

---

## 3. What `/readyz` checks

### 3.1 Audit cert loaded

The audit ingress runs with a `certwatcher.CertWatcher`
([cmd/main.go:708-743](../../cmd/main.go#L708-L743)). `certwatcher.New` reads the cert once at
construction and errors if it can't, so a non-nil watcher already implies an initial successful
read; the readiness value is asserting the watcher **still** holds a usable cert after rotation.
Check via `watcher.GetCertificate(nil)` returning a non-nil cert / nil error. In `--audit-insecure`
mode there is no watcher — this sub-check is skipped (trivially ready).

### 3.2 Audit listener serving

Today [`auditServerRunnable.Start`](../../cmd/main.go#L681-L706) calls `ListenAndServeTLS`
internally and exposes **no** "I'm listening" signal, so `/readyz` cannot currently tell whether
the listener opened. Add a tiny readiness signal to the runnable:

- bind the listener explicitly up front (`net.Listen`) and set an `atomic.Bool serving` to true
  immediately before `Serve`, clearing it on shutdown; **or**
- keep `ListenAndServeTLS` and flip the flag in a `BaseContext`/`ConnState` no-op once the server
  loop is entered.

`/readyz` reads that flag. This is the load-bearing change — it closes the "pod up but listener
not yet accepting" window.

### 3.3 Wiring

Replace the stub readiness registration
([cmd/main.go:891-895](../../cmd/main.go#L891-L895)) with a composite `healthz.Checker` closure:

```go
mgr.AddReadyzCheck("readyz", func(_ *http.Request) error {
    if !auditRunnable.Serving() {
        return errors.New("audit ingress not yet serving")
    }
    if auditCertWatcher != nil {
        if _, err := auditCertWatcher.GetCertificate(nil); err != nil {
            return fmt.Errorf("audit cert not loaded: %w", err)
        }
    }
    return redisStartupGate.Err() // see §4 — startup-only, nil once first-connected
})
```

`/healthz` stays `healthz.Ping`.

---

## 4. The Redis question: should `/readyz` report a healthy Redis connection?

**Answer: gate _initial_ readiness on the first successful Redis connection; do _not_ flip
steady-state readiness on transient Redis health; always expose Redis health as an observable.**

The reasoning hinges on a fact already in the code: **the audit handler already returns HTTP 500
when an enqueue/process fails** ([audit_handler.go:185-201](../../internal/webhook/audit_handler.go#L185-L201)).
So request-level backpressure already exists — when Redis is unreachable, the audit POST `500`s
and the apiserver (batch mode) **retries that batch**. Events are not silently lost on a transient
Redis blip; they are retried at the request layer, which is targeted and self-healing.

That reshapes the trade-off into three distinct surfaces:

| Surface | Redis in it? | Why |
|---|---|---|
| **Startup gate** (first-Ready) | **Yes** | A pod that has _never_ reached Redis genuinely cannot do its job. Going `Ready` before the first connection means it joins endpoints and `500`s every batch until Redis first connects — under HA that needlessly routes real traffic to a dead-on-arrival replica. A one-shot "have I ever connected" gate is naturally satisfied once and **cannot flap**. |
| **Steady-state probe flip** | **No** (or heavily debounced) | Redis is a **shared** dependency: a blip flips _every_ replica not-ready **simultaneously** → the Service drops to **zero** endpoints → the apiserver has nowhere to send at all. That converts a 1–2 s Redis hiccup into an audit-delivery gap plus noisy endpoint churn — strictly worse than the existing per-request 500+retry, which keeps the pods in endpoints and lets the apiserver retry. Fate-sharing makes blunt readiness the wrong tool here. |
| **Observable** (metric / log / condition) | **Yes, always** | Operators need to _see_ Redis health regardless. Surface it as a low-cardinality gauge and/or a `GitTargetStatus`/manager condition, never as a readiness flip. |

**Recommendation:** include Redis only as a **startup gate** (`redisStartupGate`): on boot, attempt
a `PING` with a short timeout and a bounded retry; `/readyz` stays not-ready until the first PING
succeeds, then the gate is permanently satisfied and contributes `nil` forever after. Leave ongoing
Redis failures to the existing request-level 500 + apiserver retry, and report ongoing health as a
metric. If a steady-state readiness component is ever wanted, it must be debounced behind a
sustained-failure window (e.g. unreachable for ≥ N s) and a generous probe `failureThreshold`, so a
brief blip never depopulates endpoints — but given the existing 500-retry path, that is **not
recommended** as a first step.

Implementation note: the audit queues wrap `*redis.Client`
(redis_bytype_queue.go:130,
redis_objects_snapshot.go:65) but expose no
health method. Add a small `Ping(ctx) error` to the by-type queue (the canonical audit producer)
and call it once at boot to satisfy the gate; no new client or pool is needed.

---

## 5. Implementation steps

1. **Audit runnable "serving" signal** — add `atomic.Bool` + `Serving() bool` to
   `auditServerRunnable`; set true once the listener is accepting, false on shutdown
   ([cmd/main.go:670-706](../../cmd/main.go#L670-L706)).
2. **Redis startup gate** — add `Ping(ctx) error` to `RedisByTypeStreamQueue`; build a one-shot
   `redisStartupGate` that PINGs with timeout+retry at boot and latches satisfied on first success.
3. **Composite `/readyz`** — replace the `healthz.Ping` readyz registration with the closure in
   §3.3; keep `/healthz` as `Ping` ([cmd/main.go:891-895](../../cmd/main.go#L891-L895)).
4. **Insecure mode** — when `--audit-insecure`, skip the cert sub-check (no watcher).
5. **Redis observability** — add a `redis_up`-style gauge updated by the producer; optionally a
   manager status condition (out of scope for the first slice).

## 6. Helm / config / docs

- The probes already exist in both
  [charts/.../deployment.yaml:124-132](../../charts/gitops-reverser/templates/deployment.yaml#L124-L132)
  and [config/deployment.yaml:50-66](../../config/deployment.yaml#L50-L66) — no manifest change
  needed beyond confirming `failureThreshold`/`periodSeconds` give the listener+cert a sane
  startup budget (consider a `startupProbe` so a slow cert mount doesn't trip liveness).
- **NOTES.txt / README caveats:**
  - **Service-target only.** Readiness endpoint-gating helps clusters whose audit webhook targets
    the Service (the chart default). Installs that aim the apiserver at a node URL / external LB
    bypass endpoint health and still rely on `batch` mode + retry buffering.
  - **`batch` mode matters.** The endpoint-gating benefit assumes `--audit-webhook-mode=batch`
    (already recommended in NOTES.txt) so the apiserver buffers during the not-ready window.

## 7. Testing

- **Unit:** the readyz closure returns error when (a) not serving, (b) cert watcher errors,
  (c) Redis gate unsatisfied; returns nil once all hold. Cover insecure-mode cert skip.
- **Unit:** audit runnable `Serving()` is false before bind, true after, false after shutdown.
- **e2e:** once `/readyz` is authoritative, the bespoke "drive a throwaway write + tail the log"
  gate in `inject-webhook-tls.sh` (`a133137`) can be **simplified** to wait on pod-Ready — the
  whole point of A1. Keep the cert-existence waits; drop the log-scraping liveness drive.

## 8. Relationship to existing work

- Implements **A1** from
  [startup-robustness-cert-and-crd-wobble.md](../finished/startup-robustness-cert-and-crd-wobble.md#L163-L167);
  complementary to **A2** (apply certs before the Deployment) — A2 shrinks the not-ready window,
  this makes the window safe.
- Orthogonal to the CRD-wobble hazard (B-series) in the same doc.

## 9. Out of scope

- Steady-state Redis readiness flipping (rejected in §4 in favour of the existing 500+retry).
- HA leader/standby readiness semantics (see `docs/design/stream/ha-improvements.md`).
- Manager-level status condition for Redis health (a fast follow, not the first slice).
