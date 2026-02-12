# Config Kustomize Review: What Is Needed vs. What Can Be Simpler

## Scope reviewed
- `config/default/kustomization.yaml`
- `config/default/manager_webhook_patch.yaml`
- `config/default/cert_metrics_manager_patch.yaml`
- `config/webhook/*`
- `config/certmanager/*`
- `cmd/main.go`
- `test/e2e/*` (especially namespace/cert assumptions)

## Executive summary
- The certs are **already rendered into `sut`**, not `system`, when deploying via `config/default`.
- For webhook TLS + cert-manager CA injection, some kustomize wiring is genuinely required.
- There is also clear scaffolding/legacy complexity that can be reduced (especially commented replacement blocks and currently-unused metrics cert flow).

## What is definitely useful / required

### 1. `manager_webhook_patch.yaml` is required for current runtime behavior
Why:
- Your manager needs `--webhook-cert-path` and `--audit-cert-path`, plus mounted secrets and container ports (`9443`, `9444`).
- Without this patch, the cert secrets are not mounted where `cmd/main.go` expects them.

References:
- `config/default/manager_webhook_patch.yaml:5`
- `config/default/manager_webhook_patch.yaml:27`
- `cmd/main.go:365`
- `cmd/main.go:522`

### 2. Webhook kustomize namespace/name rewriting is required
Why:
- `ValidatingWebhookConfiguration` is cluster-scoped, but it embeds `clientConfig.service.name/namespace` fields.
- Kustomize needs explicit field specs to rewrite those embedded fields with your prefix/namespace.

References:
- `config/webhook/kustomizeconfig.yaml:1`
- `config/webhook/webhook_service_name_patch.yaml:1`

### 3. CA injection annotation wiring for cert-manager is required (if using cert-manager)
Why:
- API server must trust the serving cert for the validating webhook.
- `cert-manager.io/inject-ca-from` annotation on `ValidatingWebhookConfiguration` is the mechanism you currently use.

References:
- `config/default/kustomization.yaml:187`
- Rendered output includes: `cert-manager.io/inject-ca-from: sut/gitops-reverser-webhook-server-cert`

### 4. `certmanager/kustomizeconfig.yaml` is required with `namePrefix`
Why:
- `namePrefix: gitops-reverser-` renames `Issuer` metadata.name.
- `Certificate.spec.issuerRef.name` must be rewritten to match, otherwise cert issuance breaks.

References:
- `config/default/kustomization.yaml:9`
- `config/certmanager/kustomizeconfig.yaml:1`

## What is currently over-complicated / likely removable

### 1. Huge commented replacement blocks in `config/default/kustomization.yaml`
- Most of the metrics/servicemonitor replacement blocks are commented and unused in your current default/e2e flow.
- Keeping them bloats maintenance and confuses intent.

Reference:
- `config/default/kustomization.yaml:53`

### 2. Mutating webhook CA injection replacements appear unused
- You only have `ValidatingWebhookConfiguration` in `config/webhook/manifests.yaml`.
- Replacement entries targeting `MutatingWebhookConfiguration` look like kubebuilder scaffold leftovers.

References:
- `config/default/kustomization.yaml:218`
- `config/webhook/manifests.yaml:3`

### 3. Metrics certificate is created but not mounted by default
- `metrics-server-cert.yaml` is included in resources.
- But `cert_metrics_manager_patch.yaml` is commented out, so manager does not mount/use `metrics-server-cert` by default.
- E2E Prometheus scrape uses `insecure_skip_verify: true` anyway.

References:
- `config/certmanager/kustomization.yaml:5`
- `config/default/kustomization.yaml:40`
- `test/e2e/prometheus/deployment.yaml:24`

## Certificate flow (how certs are used today)

### Admission webhook cert (`webhook-server-cert` secret)
1. `Certificate` resource requests cert for service DNS.
2. cert-manager writes secret `webhook-server-cert`.
3. Deployment mounts that secret and passes `--webhook-cert-path`.
4. webhook server serves TLS on `9443` using cert watcher.
5. cert-manager injects CA into `ValidatingWebhookConfiguration` annotation target.
6. kube-apiserver calls webhook via Service over TLS and trusts injected CA.

Key refs:
- `config/certmanager/webhook-server-cert.yaml:18`
- `config/default/manager_webhook_patch.yaml:52`
- `config/default/kustomization.yaml:187`

### Audit ingress cert (`audit-webhook-server-cert` secret)
1. Separate `Certificate` resource issues audit cert.
2. Secret `audit-webhook-server-cert` is mounted.
3. Manager serves HTTPS audit endpoint on `9444` using `--audit-cert-path`.
4. In e2e, kube-apiserver audit webhook config uses `insecure-skip-tls-verify: true` (so CA pinning is not enforced in test).

Key refs:
- `config/certmanager/audit-server-cert.yaml:17`
- `config/default/manager_webhook_patch.yaml:60`
- `test/e2e/kind/audit/webhook-config.yaml:14`

### Metrics cert (`metrics-server-cert` secret)
- Issued by cert-manager, but only actively used if you also enable metrics cert patch and corresponding monitor TLS config.

Refs:
- `config/certmanager/metrics-server-cert.yaml:20`
- `config/default/cert_metrics_manager_patch.yaml:12`
- `config/prometheus/monitor_tls_patch.yaml:1`

## Your namespace question: `sut` vs `system`

Short answer:
- `system` is **not required** for kube-api webhooks.
- Certs should live in the same namespace as the workload/service that uses them.
- In your current default deployment, that namespace is effectively `sut`.

Important detail:
- Source files still show `namespace: system` in some places, but `config/default/kustomization.yaml` applies `namespace: sut` globally.
- Rendered manifests confirm certs, issuer, service, deployment are in `sut`.

## Recommended simplification plan (test-focused)

### Phase 1 (safe cleanup, behavior unchanged)
1. Remove large commented blocks in `config/default/kustomization.yaml` (keep only active replacements).
2. Remove unused mutating-webhook replacement entries if you do not plan mutating webhooks.
3. Add a short comment block at top: "test profile: single service + validating webhook + audit ingress".

### Phase 2 (decide metrics cert strategy)
Choose one:
1. Keep metrics cert end-to-end: enable `cert_metrics_manager_patch.yaml` and proper monitor TLS usage.
2. Or simplify: remove `metrics-server-cert.yaml` from `config/certmanager/kustomization.yaml` and stop waiting for `metrics-server-cert` in e2e helper.

Given current e2e (`insecure_skip_verify: true`), option 2 is simpler and consistent.

### Phase 3 (optional bigger simplification)
If these manifests are truly test-only and namespace/prefix are fixed:
1. Replace dynamic cert DNS replacements with explicit static DNS names.
2. Replace dynamic `inject-ca-from` replacements with static annotation value.

Tradeoff:
- Less kustomize complexity, but less reusable/generic.

## Extra note on fixed ClusterIP
- The fixed service ClusterIP (`10.96.200.200`) is coupled to Kind audit webhook bootstrap (API server before DNS).
- Keep it if you depend on that startup behavior in e2e.

Refs:
- `config/webhook/service.yaml:10`
- `test/e2e/kind/audit/webhook-config.yaml:12`

## Bold strategy (essence-first): freeze rendered output, delete most kustomize machinery

### What you mean in practice
1. Render today’s desired install profile once (`kustomize build config/default`).
2. Split that output into plain, human-owned files by concern (for example `namespace.yaml`, `crds.yaml`, `rbac.yaml`, `deployment.yaml`, `service.yaml`, `certificates.yaml`, `webhook.yaml`).
3. Remove the current deep transformer/replacement structure from `config/`.
4. Keep either:
   - no kustomize at all (apply a folder of plain YAML in order), or
   - one tiny `kustomization.yaml` that just lists resources with zero patches/replacements.

This is a valid strategy if your goal is readability and low cognitive overhead over portability.

### Why this can be good
1. You get back to essentials: explicit manifests, no hidden transformations.
2. Refactoring confidence improves because object names/refs are visible directly.
3. New contributors can reason about install behavior without learning kustomize tricks.
4. Debugging production/test drift is easier because rendered state is source of truth.

### What you lose
1. Easy rebasing of namespace/namePrefix/env variants.
2. Automatic reference rewriting (`Issuer` name, webhook service references, CA injection path assembly).
3. Scaffold compatibility with future kubebuilder regeneration patterns.

### Where this can hurt later
1. If you later need a second profile (for example non-e2e namespace or no fixed ClusterIP), you will duplicate YAML or re-introduce templating.
2. If cert naming/service naming changes, all references must be manually updated everywhere.
3. Large CRD/regenerated sections can become noisy unless you keep strict ownership boundaries.

### Guardrails to keep this maintainable
1. Declare one supported raw-manifest profile explicitly (for example: `sut` test profile).
2. Keep clear file boundaries:
   - `config/raw/00-namespace.yaml`
   - `config/raw/10-crds.yaml`
   - `config/raw/20-rbac.yaml`
   - `config/raw/30-manager.yaml`
   - `config/raw/40-service.yaml`
   - `config/raw/50-certificates.yaml`
   - `config/raw/60-webhook.yaml`
3. If you keep minimal kustomize, allow only `resources:` entries (no `patches`, no `replacements`, no `configurations`).
4. Add a lightweight validation target (for example `kubectl apply --dry-run=server -f config/raw` in CI).

### Recommendation for your repo
- If these manifests are primarily for e2e and internal testing, this essence-first model is reasonable and likely worth it.
- If you want `config/` to be a broadly reusable install path, keep some kustomize composition and instead prune it aggressively (not fully remove it).
