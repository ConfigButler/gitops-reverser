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

2. **Consistent polarity, except for kstatus.** Domain conditions should be positive
   (`True` = healthy). `Reconciling` and `Stalled` are the sanctioned kstatus exceptions:
   they are abnormal-true because generic tooling expects that vocabulary.

3. **Names describe states, not transitions.** `ScaledOut` not `Scaling`. This way `True` =
   success, `False` = failed, `Unknown` = in progress — all unambiguous.

4. **Don't duplicate between conditions and status fields.** A string field that mirrors a
   condition is redundant noise. Pick one representation.

## Applied to this project

GitTarget, WatchRule, and ClusterWatchRule use the kstatus trio as the generic layer:

```go
const (
    TypeReady       = "Ready"       // True when the latest observed generation is fully satisfied
    TypeReconciling = "Reconciling" // True while coarse progress is in flight
    TypeStalled     = "Stalled"     // True when a human-fixable block prevents progress
)
```

GitTarget adds domain conditions that explain the summary:

```go
const (
    TypeValidated            = "Validated"
    TypeEncryptionConfigured = "EncryptionConfigured"
    TypeStreamsRunning       = "StreamsRunning"
    TypeFolderAccepted       = "FolderAccepted"
)
```

Canonical reads:

* fully mirrored: `Ready=True`, `Reconciling=False`, `Stalled=False`
* initial replay or recheck: `Ready=False`, `Reconciling=True`, `Stalled=False`
* refused folder, invalid provider, RBAC denial, or broken encryption: `Ready=False`, `Reconciling=False`,
  `Stalled=True`
* folder refusal details live on `FolderAccepted=False` and `Stalled=True`, reason `UnsupportedContent`
