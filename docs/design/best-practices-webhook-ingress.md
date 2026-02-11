1) Mutating webhook from a Kubernetes Service: minimal settings to support
A. Listener + routing

listenAddress / port (default 8443)

path (e.g. /mutate), and optionally multiple paths if you’ll have multiple webhooks

readTimeout / writeTimeout / idleTimeout

maxRequestBodyBytes (defensive; AdmissionReview can be big with certain objects)

B. TLS (this is non-negotiable in real clusters)

Kubernetes expects HTTPS for webhooks (service or URL). Minimally support:

Provide TLS cert + key

Either via: tls.secretName (mounted secret)

Or direct file paths (less “Kubernetes-y”, but useful for dev)

Provide CA bundle for the webhook configuration

In practice you’ll set caBundle on the MutatingWebhookConfiguration (or let cert-manager inject it)

Best practice: integrate with cert-manager and expose:

certManager.enabled (bool)

certManager.issuerRef (name/kind/group)

dnsNames (at least service.namespace.svc and service.namespace.svc.cluster.local)

rotation: rely on cert-manager renewal; your pod must reload certs (or restart on secret change)

C. Webhook registration (what you control via config/helm values)

Even if you generate the MutatingWebhookConfiguration from code/helm, you want these as configurable knobs:

Per webhook:

failurePolicy: Fail vs Ignore

Default recommendation: Fail for security/consistency webhooks; Ignore only if mutation is “nice to have”

timeoutSeconds: keep low (1–5s). Default 2–3s.

sideEffects: usually None (and mean it)

admissionReviewVersions: support v1 (and accept v1beta1 only if you must)

matchPolicy: typically Equivalent

reinvocationPolicy: consider IfNeeded if you mutate fields other mutators might touch

Selectors

namespaceSelector (exclude system namespaces by default)

objectSelector (optional but great for opt-in via label)

Rules

resources + operations you mutate (keep tight)

scope: cluster vs namespaced where relevant

D. Runtime safety knobs

Expose:

concurrency (max in-flight)

rateLimit (optional but helpful under thundering herd)

metrics (Prometheus) + request duration histogram

pprof optional (dev only)

logLevel with request IDs and admission UID

E. Leader election (only if you have shared mutable state)

For pure stateless mutation, you can run multiple replicas with no leader election.
If you rely on a single writer (e.g., CRD-backed shared cache warmup, or you do coordinated external writes), support:

leaderElection.enabled

lease namespace/name

2) Mutating webhook best practices (the stuff that prevents outages)

Correctness & determinism

Make patches deterministic (same input → same output).

Be idempotent (if called twice, you don’t double-apply).

Respect dryRun (don’t create external side effects).

Don’t depend on “live GET” calls in the hot path unless cached; API calls add latency and can deadlock during API stress.

Performance

Keep p99 latency low; webhooks are on the API request path.

Prefer fast local validation/mutation + cached lookups.

Set tight timeoutSeconds and tune server timeouts accordingly.

Safety

Default namespaceSelector to exclude kube-system, kube-public, kube-node-lease, and your own operator namespace until you explicitly need them.

Use objectSelector to allow opt-in (label) for risky mutations.

Use failurePolicy=Fail only when you’re confident in HA + readiness + rollout strategy.

Rollout strategy

Run at least 2 replicas (or more, depending on API QPS).

Use a PodDisruptionBudget.

Ensure readinessProbe only goes ready when:

certs are loaded

any required caches are warm (if you depend on them)

Prefer “versioned” webhook names/paths when doing breaking changes.

Observability

Log: admission UID, kind, namespace/name, userInfo, decision, latency

Metrics: requests, rejections, patch size, errors, timeouts

3) Should you “support the same settings” for audit webhook handling?

Some overlap, yes (TLS/HA/observability), but don’t treat them as the same product surface. Audit has very different operational requirements.

What overlaps (you should support in both)

HTTPS listener, cert management, rotation

AuthN (ideally mTLS) and authorization/allowlisting

Timeouts + max body size

Concurrency limits and metrics

What’s different (audit needs extra settings)

Audit webhook backends can get a lot of traffic and the API server will retry under some failure modes, but you still need to assume:

bursts

duplicates

out-of-order delivery

occasional loss depending on audit config and backpressure

So minimally for audit ingestion, add:

queue.enabled + queue.size

batching (optional, but very useful downstream)

durability choice:

memory queue (simple, lossy on restart)

persistent queue (disk/DB/Kafka/etc.)

Backpressure behavior

what happens when full: drop / block / shed by priority

Deduplication keying (best-effort): use audit event IDs if present

Separate endpoint / separate Deployment strongly recommended

Auth for audit

For the audit webhook backend, the API server can be configured with a kubeconfig to talk to your endpoint, which makes mTLS client cert auth a clean approach. If you already have a public wildcard cert, that helps with server identity, but client auth is what prevents random in-cluster callers from spamming your audit ingest.

Recommendation:

Admission webhook: rely on in-cluster service + TLS + CA bundle (standard)

Audit webhook: mTLS (client certs) and strict allowlisting/rate limits

Practical recommendation on architecture

Keep admission and audit as separate handlers, ideally separate deployments.

Admission: optimized for latency + correctness

Audit: optimized for throughput + buffering + durability

Share libraries (TLS, metrics, logging), but do not share the same scaling knobs or failure modes.

If you want a simple “minimal config surface” that still scales, expose two top-level blocks:

admissionWebhooks: (tls, selectors, failurePolicy, timeouts, concurrency)

auditIngest: (tls, authn, queue/durability, backpressure, concurrency)

That’s the line where you stay sane when traffic grows.

If you want, paste your current helm values / flags structure and I’ll suggest a clean config schema (what should be values vs generated defaults) without blowing up the number of knobs.