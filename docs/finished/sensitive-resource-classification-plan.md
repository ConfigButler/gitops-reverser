# Implementation Plan: Startup Sensitive Resource Classification

Tracks [#147](https://github.com/ConfigButler/gitops-reverser/issues/147) —
support CozyStack's `tenantsecrets.core.cozystack.io` so Secret-shaped custom
resources are encrypted before they land in Git.

## Goal

GitOps Reverser currently encrypts only core Kubernetes Secrets. The decision is
hardcoded in the Git write path:

```go
if !types.IsSecretResource(event.Identifier) {
    return content, nil
}
```

CozyStack `tenantsecrets.core.cozystack.io` carry the same `.data` payload as a
core Secret. The first increment adds one explicit process-level way to send
that resource type through the existing Secret encryption path.

## Design

Add one comma-separated startup flag:

```text
--additional-sensitive-resources=core.cozystack.io/tenantsecrets,example.io/credentials
```

Core `secrets` is always built in, so current Secret behavior never depends on a
new flag or Helm value. The flag only carries additional resource types:

- core resources use a bare resource name, for example `secrets`
- grouped resources use `group/resource`, for example
  `core.cozystack.io/tenantsecrets`

The sensitive-resource policy matches **group + resource**, not API version.
Sensitivity is a resource-type property; a CRD version bump must not silently
send the same kind back to the plaintext path.

The parser splits each entry on one optional `/`, rejects empty segments and
entries with extra `/`, normalizes and deduplicates entries, and logs the final
resource set at startup.

## Scope

The sensitive-resource policy affects the Git layer only in this increment:

1. `content_writer.buildContentForWrite` chooses encryption from the policy.
2. `generateFilePath` chooses `.sops.yaml` from the policy so writes and deletes
   use the same path contract as core Secrets.
3. Git write-path encryption-failure logging uses the policy instead of a
   core-Secret-only check.

`IsSecretResource` stays for code that specifically means core Secrets. New Git
write code that means "must be encrypted" should use the policy.

Do not change the bootstrapped `.sops.yaml` here. It encrypts `data` and
`stringData`, which fits core Secrets and CozyStack `tenantsecrets`. Sensitive
custom resources with other field shapes are tracked in [TODO.md](../TODO.md).

## Helm

Expose the additions through Helm values:

```yaml
controllerManager:
  additionalSensitiveResources:
    - core.cozystack.io/tenantsecrets
```

The chart renders the list into the manager flag. The default list is empty
because core Secrets stay built in.

The operator trade-off is explicit: a custom secret-bearing resource omitted
from the startup flag still follows the ordinary plaintext path. This increment
avoids a GitTarget API, name heuristics, and denylist carve-outs.

## Implementation

1. Add a small group/resource `SensitiveResourcePolicy` with parser and tests.
2. Parse `--additional-sensitive-resources` in `cmd/main.go`, keep built-in
   core `secrets`, and log the resolved set.
3. Thread the policy from startup into branch workers and content writers.
4. Switch content encryption, `.sops.yaml` path generation, and encryption
   failure logging from `IsSecretResource` to the policy.
5. Add Helm values and user docs for the CozyStack-style opt-in.
6. Add focused tests for parser validation, group/resource matching, non-core
   sensitive encryption, nil-encryptor fail-closed behavior, and non-core
   `.sops.yaml` paths.

## Testing

- `task fmt`
- `task vet`
- `task lint`
- `task test`
- `task test-e2e` after confirming Docker is running

Run the required validation sequence sequentially.

## Follow-up

Catalog validation, rule-status diagnostics, runtime skipping for unencrypted
targets, and the open questions around those behaviors are split into
[sensitive-resource-diagnostics-follow-up.md](../design/sensitive-resource-diagnostics-follow-up.md).
