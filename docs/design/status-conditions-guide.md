# KRM Status & Conditions Best Practices

Reference: [Superorbital — "Status and Conditions: Explained!"](https://superorbital.io/blog/status-and-conditions/)

## What status is

Controllers write it, users don't. It's a separate subresource (`/status`) — `kubectl edit` won't
persist changes to it, and RBAC for it is granted separately from the main resource:

```yaml
- resources: ["gitdestinations/status"]
  verbs: ["get", "patch", "update"]
```

## Conditions

A list treated as a map keyed by `type`. Don't append duplicates — update the existing entry.
`kubectl wait --for=condition=Ready=true` works by watching this array until the matching type
hits `"True"`.

Only update `lastTransitionTime` when `status` actually changes — not on every reconcile.

## Best practices

1. **Always have a summary condition.** `Ready` for long-running objects, `Succeeded` for
   bounded ones. This is what operators and scripts will `kubectl wait` on.

2. **Consistent polarity.** Pick positive (`True` = healthy) or negative (`False` = healthy)
   and stick to it across all conditions. Mixing causes confusion when scanning a conditions list.

3. **Names describe states, not transitions.** `ScaledOut` not `Scaling`. This way `True` =
   success, `False` = failed, `Unknown` = in progress — all unambiguous.

4. **Don't duplicate between conditions and status fields.** A string field that mirrors a
   condition is redundant noise. Pick one representation.

## Applied to this project

```go
const (
    TypeReady     = "Ready"      // summary — True when everything is healthy
    TypeAvailable = "Available"  // can we reach the Git repository?
    TypeActive    = "Active"     // is the BranchWorker running? (not "Progressing")
    TypeSynced    = "Synced"     // are all changes pushed to Git?
)
```
