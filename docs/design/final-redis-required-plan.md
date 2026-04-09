# Final Plan: Redis/Valkey as a Hard Requirement

This document captures the concrete changes needed to treat Valkey/Redis as a first-class external
dependency, require authenticated connections, rename the default install namespace, and clean up
the leftover design-era artifacts that have accumulated now that the consumer is fully implemented
and all e2e tests pass.

---

## 1. Current State

Valkey is already a hard runtime requirement. The controller fails to start without a reachable Redis
endpoint (`audit-redis-addr` is validated at startup and the consumer/producer both call `fatalIfErr`
on connection setup). There is no code path that skips Redis.

What has not caught up to that reality:

- The quick start in `charts/gitops-reverser/README.md` installs Valkey **without auth**, directly
  teaching operators to run an unauthenticated queue that carries audit payloads and secret material.
- The password is threaded through as a CLI argument (`--audit-redis-password`), making it visible in
  `kubectl describe pod` output.
- `values.yaml` exposes `queue.redis.username` and `queue.redis.password` as plain text fields,
  which end up in Helm release secrets and command args rather than in a proper Kubernetes Secret.
- The default install namespace throughout all documentation is `gitops-reverser-system`. The
  `-system` suffix is a convention for Kubernetes infrastructure (e.g. `kube-system`). For an
  operator that users install by name, `gitops-reverser` is shorter, cleaner, and more consistent
  with how cert-manager itself uses `cert-manager` rather than `cert-manager-system`.
- Several design documents written during the "should we adopt Valkey?" and "should the consumer be
  optional?" phases are now fully superseded, but they still sit in `docs/design/` and create
  confusion about what is settled.

---

## 2. Why Auth Is Non-Negotiable

Every audit event that flows through the system — including those for Secrets, all user activity,
and all mutating operations — passes through Valkey before it reaches Git. An unauthenticated Valkey
instance allows any process in the cluster to:

- Read every audit payload (including raw Secret `data` when the audit policy level allows it).
- Inject arbitrary events into the audit stream, causing fake commits.
- Delete or corrupt pending entries, breaking at-least-once delivery.

This is not a theoretical risk. Valkey is explicitly deployed in the controller's namespace in the
standard quick start, meaning any pod in that namespace could attack it without credentials.

Auth is therefore not an optional hardening step — it is the minimum acceptable security posture
for a queue that carries Kubernetes audit data.

---

## 3. Dependency Model: Follow the cert-manager Pattern

Valkey/Redis should be documented and installed the same way as cert-manager: **install it first,
point us at it, we require it to be there**.

What this means in practice:

- The chart does **not** bundle a Valkey subchart (the `valkey-adoption-guide.md` recommended this
  approach, and we already follow it — no change needed here).
- The prerequisites section of the README lists Valkey/Redis alongside cert-manager and Kubernetes.
- The quick start shows a minimal but **auth-enabled** Valkey install before the controller install.
- The configuration reference documents `queue.redis.auth.existingSecret` as the canonical auth
  pattern, not inline plaintext credentials.

There is no need for a `valkey.enabled` toggle or embedded subchart. If a user wants to run a
quick dev environment, they can install Valkey via Helm with one command; they do not need us to
do it for them any more than they need us to install cert-manager.

---

## 4. Changes Required

### 4.1 Default Namespace: `gitops-reverser`

Every occurrence of `gitops-reverser-system` in documentation, README examples, config README, and
any other user-facing text should change to `gitops-reverser`.

The `-system` suffix is a Kubernetes convention for infrastructure namespaces that ship with the
platform (`kube-system`, `kube-public`). Third-party operators typically do not follow that pattern
— cert-manager uses `cert-manager`, not `cert-manager-system`. Using just `gitops-reverser` is
shorter, easier to type, and consistent with the project name.

This is a documentation-only change. The chart itself uses `{{ .Release.Namespace }}` throughout
and has no hardcoded namespace. Users who already installed into `gitops-reverser-system` are
unaffected; the new default is what the quick start and README suggest going forward.

Files to update:

- `charts/gitops-reverser/README.md` — all example commands
- `config/README.md` — install path examples
- Any other docs that use `gitops-reverser-system` as an example namespace

---

### 4.2 Valkey Chart Source: Official valkey-io Chart

**Decision: use the official `valkey-io` Helm chart, not Bitnami.**

The current quick start uses:

```bash
helm install valkey oci://registry-1.docker.io/bitnamicharts/valkey \
  --set auth.enabled=false
```

Replace with the official chart from the Valkey project:

```bash
helm repo add valkey https://valkey.io/valkey-helm/
helm repo update
```

**Why the official chart over Bitnami:**

| | Official (valkey-io) | Bitnami |
|---|---|---|
| Maintained by | The Valkey project itself | Broadcom/Bitnami |
| Auth model | Native ACL users (`usersExistingSecret`) | Wrapped flag (`auth.existingSecret`) |
| Abstraction layer | Minimal — config maps almost directly to `valkey.conf` | Heavy — many init scripts, non-standard entrypoints |
| Dependency on Bitnami | None | Bitnami base image, scripts, helpers |
| User experience | What you configure is what Valkey does | Bitnami layer can surprise users debugging issues |

The Bitnami chart is widely used and works fine, but it wraps Valkey in its own init scripts and
base images. When something goes wrong, users end up debugging Bitnami's layer rather than Valkey
itself. For a project that positions Valkey as a first-class required dependency that operators
need to understand and maintain, pointing to the official chart removes that indirection.

The official chart is actively maintained (0.9.3, January 2025), has 243 GitHub stars, and is
published through the official Valkey project at `https://valkey.io/valkey-helm/`.

**Auth configuration for the official chart:**

The official chart uses ACL-based auth. The `default` user must be defined when auth is enabled.
The secret key name is set per-user via `passwordKey`.

```bash
# 3. Create the namespace and auth secret
kubectl create namespace gitops-reverser
kubectl create secret generic valkey-auth \
  --namespace gitops-reverser \
  --from-literal=password="$(openssl rand -base64 32)"

# 4. Install Valkey using the official chart with auth enabled
helm repo add valkey https://valkey.io/valkey-helm/
helm repo update
helm install valkey valkey/valkey \
  --namespace gitops-reverser \
  --set auth.enabled=true \
  --set auth.usersExistingSecret=valkey-auth \
  --set auth.aclUsers.default.passwordKey=password \
  --set auth.aclUsers.default.permissions="~* &* +@all"

# 5. Install GitOps Reverser, referencing the same auth secret
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --create-namespace \
  --set queue.redis.addr="valkey.gitops-reverser.svc.cluster.local:6379" \
  --set queue.redis.auth.existingSecret=valkey-auth \
  --set queue.redis.auth.existingSecretKey=password
```

The `--create-namespace` on the GitOps Reverser install is a safety net; the namespace already
exists from step 3, so this is idempotent.

> **Service hostname:** The official chart applies standard Helm deduplication: when the release
> name and chart name are both `valkey`, the fullname is just `valkey` (not `valkey-valkey`).
> So `helm install valkey valkey/valkey` produces a service called `valkey`, and the address
> is `valkey.gitops-reverser.svc.cluster.local:6379`. No `fullnameOverride` is needed.

---

### 4.3 Replace Plaintext Password Fields with existingSecret Pattern

**File:** `charts/gitops-reverser/values.yaml`

Remove:

```yaml
queue:
  redis:
    username: ""
    password: ""
```

Add:

```yaml
queue:
  redis:
    auth:
      # Name of a pre-existing Secret in the same namespace that holds the Redis password.
      # The secret must be created before installing this chart.
      # Required for production deployments. Leave empty only for dev/local environments
      # where Valkey runs without authentication.
      existingSecret: ""
      # Key within the Secret that holds the password value.
      existingSecretKey: "password"
      # Optional Redis username (for Redis 6+ ACL-based auth).
      username: ""
```

The `username` field stays because Redis ACL setups use it, and it is not sensitive enough to
require a secret reference. If a project later wants to require username-in-secret too, it can
follow the same `existingSecret` extension.

---

### 4.4 Remove Password from CLI Args, Use Env Var Instead

**File:** `charts/gitops-reverser/templates/deployment.yaml`

Remove:

```yaml
{{- with .Values.queue.redis.username }}
- --audit-redis-username={{ . | quote }}
{{- end }}
{{- with .Values.queue.redis.password }}
- --audit-redis-password={{ . | quote }}
{{- end }}
```

Keep the username arg (it is not sensitive), but source the password through an env var:

```yaml
{{- with .Values.queue.redis.auth.username }}
- --audit-redis-username={{ . | quote }}
{{- end }}
```

Add to the `env:` block:

```yaml
{{- if .Values.queue.redis.auth.existingSecret }}
- name: REDIS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ .Values.queue.redis.auth.existingSecret }}
      key: {{ .Values.queue.redis.auth.existingSecretKey | default "password" }}
{{- end }}
```

**File:** `cmd/main.go`

Change the flag default for `--audit-redis-password` from `""` to `os.Getenv("REDIS_PASSWORD")`:

```go
fs.StringVar(&cfg.auditRedisPassword, "audit-redis-password", os.Getenv("REDIS_PASSWORD"),
    "Redis password for audit event queueing. Can also be set via REDIS_PASSWORD env var.")
```

This means the password never appears in process args, `kubectl describe pod`, or Helm release
history. The env var is read at startup from the mounted secret volume.

The same pattern should be applied to `--audit-redis-username` if ACL usernames are ever
considered sensitive in a given deployment, but that is optional since usernames are generally
not secrets.

---

### 4.5 Single YAML Install (`install.yaml`)

The `install.yaml` generated from the Helm chart is used for quick cluster installs without Helm.
With the `existingSecret` approach, users must create the Secret before applying the manifest.

Document this as a prerequisite in the release page and in the config README:

```bash
# 1. Create the namespace and Valkey auth secret
kubectl create namespace gitops-reverser
kubectl create secret generic valkey-auth \
  --namespace gitops-reverser \
  --from-literal=password="$(openssl rand -base64 32)"

# 2. Install cert-manager (if not already installed)
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.19.1/cert-manager.yaml
kubectl wait --for=condition=ready pod -l app.kubernetes.io/instance=cert-manager -n cert-manager --timeout=300s

# 3. Install Valkey (official chart, auth-enabled)
helm repo add valkey https://valkey.io/valkey-helm/
helm repo update
helm install valkey valkey/valkey \
  --namespace gitops-reverser \
  --set auth.enabled=true \
  --set auth.usersExistingSecret=valkey-auth \
  --set auth.aclUsers.default.passwordKey=password \
  --set auth.aclUsers.default.permissions="~* &* +@all"

# 4. Apply the install manifest
kubectl apply -f https://github.com/ConfigButler/gitops-reverser/releases/latest/download/install.yaml
```

The `install.yaml` workflow does not change structurally. It already requires cert-manager to be
pre-installed; requiring a Valkey auth secret follows the same model.

The `config/deployment.yaml` used for e2e testing currently connects to Valkey without auth. This
is acceptable for CI (the Valkey instance is cluster-internal and short-lived), but a TODO comment
should note that the e2e Valkey install should be updated to run with auth once the e2e environment
Helm values are updated.

---

### 4.6 E2E Test Environment Auth

The e2e cluster already uses the official Valkey chart (via Flux HelmRelease in `valkey-e2e`
namespace). Auth is currently disabled. It should be enabled — no exceptions — but with a fixed
known password committed to the repo. A hardcoded test secret is appropriate here: the e2e cluster
is ephemeral, local, and contains no real data.

**Fixed password constant:** `e2e-valkey-password`

This is committed openly and that is intentional. It is only ever used in CI and local dev clusters.

#### Changes required

**`test/e2e/setup/flux/values/valkey-values.yaml`** — enable auth, reference secret:

```yaml
# Valkey Helm values for e2e testing.
# Standalone mode, no persistence, minimal resources, auth enabled.

architecture: standalone

primary:
  persistence:
    enabled: false
  resources:
    limits:
      memory: "256Mi"
    requests:
      memory: "128Mi"

auth:
  enabled: true
  usersExistingSecret: valkey-auth
  aclUsers:
    default:
      permissions: "~* &* +@all"
      passwordKey: password
```

**`test/e2e/setup/flux/kustomization.yaml`** — add a `secretGenerator` for the valkey-auth secret
in `valkey-e2e`. This ensures the secret exists before the HelmRelease reconciles:

```yaml
secretGenerator:
  - name: valkey-auth
    namespace: valkey-e2e
    literals:
      - password=e2e-valkey-password
    options:
      disableNameSuffixHash: true
```

**`config/valkey-auth.yaml`** — new file: a fixed Secret for the controller namespace. Kustomize
will replace the `namespace` value with `$(NAMESPACE)` at apply time:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: valkey-auth
  namespace: sut
type: Opaque
stringData:
  password: e2e-valkey-password
```

Add `valkey-auth.yaml` to `config/kustomization.yaml` resources.

**`config/deployment.yaml`** — add the `REDIS_PASSWORD` env var from the secret:

```yaml
env:
  - name: POD_NAME
    valueFrom:
      fieldRef:
        fieldPath: metadata.name
  - name: POD_NAMESPACE
    valueFrom:
      fieldRef:
        fieldPath: metadata.namespace
  - name: REDIS_PASSWORD
    valueFrom:
      secretKeyRef:
        name: valkey-auth
        key: password
```

**`Makefile`** — add `E2E_VALKEY_PASSWORD` variable and wire auth into the helm install mode.
The `config-dir` mode picks it up from the committed secret file automatically. The `helm` mode
needs an explicit `kubectl create secret` before the chart install and the auth values passed to
`helm upgrade --install`:

```makefile
E2E_VALKEY_PASSWORD ?= e2e-valkey-password
```

In the `$(CS)/$(NAMESPACE)/helm/install.yaml` target, before the `helm upgrade --install` call:

```makefile
kubectl --context $(CTX) create namespace $(NAMESPACE) --dry-run=client -o yaml \
    | kubectl --context $(CTX) apply -f -
kubectl --context $(CTX) -n $(NAMESPACE) create secret generic valkey-auth \
    --from-literal=password=$(E2E_VALKEY_PASSWORD) \
    --dry-run=client -o yaml | kubectl --context $(CTX) apply -f -
```

And add to the `helm upgrade --install` call:

```makefile
--set queue.redis.auth.existingSecret=valkey-auth \
--set queue.redis.auth.existingSecretKey=password \
```

> **`plain-manifests-file` mode:** The `dist/install.yaml` is generated without `existingSecret`
> set (empty default), so no `REDIS_PASSWORD` env var is rendered. The e2e plain-manifests-file
> path currently does not test auth. This is a known gap; address it in a follow-up by adding a
> `sed` patch alongside the existing redis-addr patch in the Makefile.

---

## 5. Documents to Remove or Archive

Now that the consumer is fully implemented and all e2e tests pass, several design documents are
either fully superseded or describe decisions that are no longer open questions.

### Move to `docs/past/`

| Document | Reason |
|---|---|
| `docs/design/audit-consumer-next-steps.md` | All steps are marked done. The doc's remaining value is tracking the implementation history, which belongs in past/. |
| `docs/design/audit-webhook-redis-queue-plan.md` | "Not implemented yet" section is now fully implemented. "Remaining work" is done. The authoritative current-state doc is `webhook-audit-pipeline-current-state.md`. |
| `docs/design/valkey-adoption-guide.md` | The adoption question is settled. Valkey is required and the architecture described is now live. The Phased Adoption Plan is complete through step 4. Move to past/ and keep as a reference, but remove it from the active design/ surface. |
| `docs/future/AUDIT_WEBHOOK_NATS_ARCHITECTURE_PROPOSAL.md` | NATS was considered and explicitly rejected in favour of Valkey. This is historical context only. |

### Update in Place

| Document | What to Update |
|---|---|
| `docs/design/webhook-audit-pipeline-current-state.md` | Add a note at the top that this is the authoritative current-state document for the audit pipeline. Remove the "still broken" IceCreamOrder section — that was fixed in a subsequent commit. |
| `docs/design/webhook-audit-e2e-remediation-status.md` | The "What Still Fails" section describes a failure that was subsequently fixed. Either remove that section or annotate it as resolved. The doc is still useful for understanding the remediation history. |
| `charts/gitops-reverser/README.md` | Update the quick start (section 4.1), prerequisites list, and the configuration reference table to replace `queue.redis.username`/`password` rows with `queue.redis.auth.*` rows. |

### Keep As-Is

| Document | Reason |
|---|---|
| `docs/design/best-practices-webhook-ingress.md` | Still relevant for ingress hardening decisions. |
| `docs/design/webhook-audit-pipeline-current-state.md` | Current and authoritative once the small updates above are made. |

---

## 6. Summary of Changes

| File | Change |
|---|---|
| `charts/gitops-reverser/values.yaml` | Replace `queue.redis.username`/`password` with `queue.redis.auth.existingSecret`, `existingSecretKey`, `username` |
| `charts/gitops-reverser/templates/deployment.yaml` | Remove `--audit-redis-password` arg; add `REDIS_PASSWORD` env var from secret ref; keep `--audit-redis-username` arg |
| `cmd/main.go` | Change `--audit-redis-password` default to `os.Getenv("REDIS_PASSWORD")` |
| `charts/gitops-reverser/README.md` | Fix quick start: official Valkey chart, auth-enabled, new namespace; update prerequisites and config reference table |
| `config/README.md` | Add Valkey auth secret and official chart install as pre-install steps for the single-YAML path |
| All user-facing docs | Replace every `gitops-reverser-system` namespace example with `gitops-reverser` |
| `test/e2e/setup/flux/values/valkey-values.yaml` | Enable auth, reference `valkey-auth` secret |
| `test/e2e/setup/flux/kustomization.yaml` | Add `secretGenerator` for `valkey-auth` in `valkey-e2e` namespace |
| `config/valkey-auth.yaml` | New: fixed Secret for controller namespace (committed test credential) |
| `config/kustomization.yaml` | Add `valkey-auth.yaml` to resources |
| `config/deployment.yaml` | Add `REDIS_PASSWORD` env var from `valkey-auth` secret |
| `Makefile` | Add `E2E_VALKEY_PASSWORD`, create secret before helm install, pass auth values |

---

## 7. What Not to Change

- The `--audit-redis-addr`, `--audit-redis-db`, `--audit-redis-stream`, `--audit-redis-max-len`,
  and `--audit-redis-tls` flags are not sensitive and stay as CLI args.
- The e2e `config/deployment.yaml` can keep running against an unauthenticated Valkey for now.
  Add a TODO comment pointing at this plan.
- The `internal/queue/` package does not need changes — `RedisAuditQueueConfig.AuthValue` already
  accepts the password value regardless of how it was sourced. The change is purely in how the
  value reaches the process.
- The `AuditConsumerConfig.AuthValue` field follows the same pattern.
