# Follow-up: Sensitive Resource Diagnostics

This follow-up builds on
[sensitive-resource-classification-plan.md](sensitive-resource-classification-plan.md).
The first increment only adds explicit startup classification to the Git write
path. This document keeps the diagnostics and policy work out of that small
change.

## Why Defer

Sensitive startup classification can encrypt CozyStack-style Secret-shaped CRDs
without changing WatchRule resolution. Diagnostics add new catalog, readiness,
and controller interactions. They are useful, but they should be designed as a
second behavior change instead of being smuggled into the write-path fix.

## Candidate Work

### Catalog Sensitivity

Add `Sensitive bool` to `APIResourceEntry`, populate it from the process-wide
sensitive-resource policy, and keep it independent from `Allowed`.

`Allowed=false` is a hard resource-policy exclusion. Sensitive resources still
have GitOps value when their GitTarget has encryption.

### Rule Diagnostics

When a WatchRule or ClusterWatchRule selects a classified sensitive resource and
its bound GitTarget has no resolved encryption:

1. report the sensitive selection on rule status, for example with a
   `SensitiveResourcesSkipped` reason
2. decide whether that rule should skip the sensitive resource at runtime while
   leaving non-sensitive resources active

Skipping cannot be status-only. Compiled rules currently feed informer
planning, GitTarget snapshots, and live/audit routing. Any runtime filter needs
one shared resolved-rule or filtering path across all of them, including the
case where another rule keeps an informer for the same GVR alive.

The Git writer's nil-encryptor failure remains the last fail-closed backstop.

### Discovery Validation

Validate configured additional sensitive resource types against API discovery so
a misspelled startup value does not look active.

One design is to wait for the catalog's first complete discovery and fail when a
configured `(group, resource)` has no served match in any version. Degraded
discovery is inconclusive and must not cause a false failure.

## Open Questions

### Validation Failure Mode

Should an unserved configured sensitive resource:

- fail readiness and retry inside the running process, or
- terminate and rely on restart once the CRD exists?

The first option fits later catalog refreshes and can detect CRD removal. The
second is simpler operationally but ties startup order to CRD installation and
needs explicit readiness/startup behavior.

### Rule Re-resolution

If rules are skipped while a GitTarget lacks encryption, what re-enqueues or
re-resolves those rules when encryption later becomes available?

GitTarget spec changes requeue dependent rules today, but encryption readiness
can also move after encryption Secret or status changes.

### Skip or Report Only

Should unencrypted sensitive rules be filtered before runtime work, or should
status report the problem and keep the existing Git writer failure behavior?

Filtering reduces noisy failed writes. Reporting only keeps behavior closest to
the current core Secret path.

### Deployment Scope

Process-level startup flags apply to every GitTarget in the controller. If
operators need target-specific sensitive additions or removals later, revisit a
GitTarget API instead of expanding heuristics.

## Related Deferred Work

The current bootstrapped `.sops.yaml` encrypts Secret-shaped `data` and
`stringData` fields. Sensitive custom resources with other fields still need the
SOPS policy decision tracked in [TODO.md](../TODO.md).
