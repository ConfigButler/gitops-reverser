# Plan: Remove GitTarget Admission Webhook

## Context

The GitTarget admission webhook (`internal/webhook/gittarget_validator.go`) is a validating webhook
that rejects invalid GitTarget resources at `kubectl apply` time. It checks two things:

1. **Encryption spec shape** — `secretRef.name` is required when encryption is configured
2. **Uniqueness** — no two GitTargets may share the same `(repo URL, branch, path)` combination

The GitTarget controller independently enforces both of these constraints:

- Encryption configuration is validated in the `EncryptionConfigured` gate
  ([internal/controller/gittarget_controller.go:319](../internal/controller/gittarget_controller.go#L319))
- Path uniqueness is checked in `evaluateValidatedGate` → `checkForConflicts`
  ([internal/controller/gittarget_controller.go:304](../internal/controller/gittarget_controller.go#L304))

This makes the admission webhook a UX convenience, not a correctness requirement. Removing it
simplifies the operator's runtime topology, deployment configuration, and certificate management.

## Pros

- **Drop the controller-runtime webhook server entirely.** The GitTarget validator is the only
  admission webhook in the project. Removing it means the operator no longer needs to run an HTTPS
  server on port 9443 for admission reviews.

- **No more webhook TLS certificate management.** Today every deployment needs either cert-manager
  or a manually provisioned TLS certificate for the admission endpoint. This is consistently one of
  the most common sources of setup friction for Kubernetes operators.

- **Simpler Helm chart.** The `servers.admission` values block, the `admission-webhook.yaml`
  template, the admission Certificate resource, the admission Service port, the caBundle injection,
  and the associated README sections all go away.

- **Simpler kustomize config.** Three files deleted (`webhook/manifests.yaml`, `webhook/patch.yaml`,
  `webhook-service.yaml`), plus the replacement stanzas and the admission Certificate in
  `config/certs/`.

- **Fewer CLI flags.** `--webhook-cert-path`, `--webhook-cert-name`, `--webhook-cert-key`, and
  `--webhook-insecure` can all be removed. The fallback logic that copies webhook cert paths to
  audit cert paths also goes away, making the audit TLS configuration self-contained.

- **Removes a failure mode.** If the webhook is misconfigured or its certificate expires, GitTarget
  creates/updates are rejected cluster-wide. This is a high blast radius for a UX convenience. The
  `failurePolicy: Fail` setting means a broken webhook blocks all GitTarget operations.

- **Less code.** Approximately 700 lines of Go code (validator + tests) plus significant Helm/kustomize
  configuration.

## Cons

- **Slower feedback on invalid GitTargets.** Without the webhook, a user who creates a duplicate
  GitTarget will see it accepted by `kubectl apply`, then find `Validated=False` in the status
  conditions. The error is still surfaced, but after a reconcile cycle rather than immediately.

- **Encryption spec errors are deferred.** A GitTarget with `encryption.enabled: true` but no
  `secretRef.name` will be created and sit in `EncryptionConfigured=False` instead of being
  rejected at apply time. Again, the error is clear in status, just not immediate.

Both of these are standard Kubernetes operator behavior. Most operators do not use admission
webhooks for their CRDs and rely entirely on status conditions for validation feedback.

## What changes

### Files to delete

| File | What it is |
|---|---|
| `internal/webhook/gittarget_validator.go` | Validator implementation |
| `internal/webhook/gittarget_validator_test.go` | Validator tests (~669 lines) |
| `config/webhook/manifests.yaml` | Generated ValidatingWebhookConfiguration |
| `config/webhook/patch.yaml` | cert-manager CA injection patch |
| `config/webhook-service.yaml` | Admission Service (port 9443) |
| `charts/gitops-reverser/templates/admission-webhook.yaml` | Helm ValidatingWebhookConfiguration |

### Files to edit

#### `cmd/main.go`

- Remove `SetupGitTargetValidatorWebhook` call (lines 181-184)
- Remove `initWebhookServer` call and function definition — this is the only admission webhook, so
  the entire controller-runtime webhook server can go
- Remove `webhookCertPath`, `webhookCertName`, `webhookCertKey`, `webhookInsecure` from `appConfig`
  and their flag bindings
- Remove the fallback logic that copies webhook cert paths into audit cert paths — make the audit
  TLS flags self-contained with their own defaults
- Remove `WebhookServer` from `newManager` call

#### `cmd/main_audit_server_test.go`

- Remove `webhookInsecure` default assertion
- Remove or rewrite `TestParseFlagsWithArgs_FallsBackToWebhookCertPath` — the fallback concept no
  longer exists
- Update any test that passes `--webhook-cert-path` as an argument

#### `config/deployment.yaml`

- Remove `--webhook-cert-path=...` CLI argument
- Remove `containerPort: 9443 / name: admission`
- Remove `volumeMount` and `volume` for `webhook-certs`

#### `config/kustomization.yaml`

- Remove `webhook-service.yaml` from resources
- Remove `webhook/manifests.yaml` from resources
- Remove `webhook/patch.yaml` from patches
- Remove the `replacements` stanza for `ValidatingWebhookConfiguration`

#### `config/certs/certificates.yaml`

- Remove the `gitops-reverser-admission-server-cert` Certificate resource
- Remove its corresponding replacement entry in `config/certs/kustomization.yaml`

#### Helm chart

| File | Changes |
|---|---|
| `templates/certificates.yaml` | Remove admission Certificate resource (lines 14-35) |
| `templates/deployment.yaml` | Remove admission TLS args, container port, volumeMount, volume |
| `templates/services.yaml` | Remove `admission` port from Service |
| `templates/configmap.yaml` | Remove `webhook.port` entry |
| `values.yaml` | Remove `servers.admission` block, `webhook.caBundle`, `service.ports.admission` |
| `README.md` | Remove cert-manager prerequisite, webhook config section, troubleshooting section |

#### Tests

| File | Changes |
|---|---|
| `test/e2e/e2e_test.go:134` | Update smoke test — the admission port is gone from the Service |

#### Documentation

| File | Changes |
|---|---|
| `docs/architecture.md` | Remove admission webhook paragraph, startup diagram node, package map entry |

### Not affected

- **Audit webhook** (`internal/webhook/audit_handler.go`) — this is a plain HTTP server, not a
  Kubernetes admission webhook. It has its own TLS configuration and is completely independent.
- **Audit TLS setup in e2e** (`inject-webhook-tls.sh`, `_webhook-tls-ready` task) — despite the
  name, this is for the audit webhook, not the admission webhook.
- **CI workflows** — no webhook-specific CI steps exist.

## Suggested order of implementation

1. Remove the Go code first (`gittarget_validator.go`, its tests, `cmd/main.go` changes)
2. Verify the operator builds and unit tests pass without the webhook server
3. Remove kustomize configuration
4. Remove Helm chart configuration
5. Update e2e smoke test
6. Update `docs/architecture.md`
7. Run full e2e suite to confirm nothing depends on the webhook
