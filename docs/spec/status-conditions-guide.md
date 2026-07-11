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
    TypeGitPathAccepted       = "GitPathAccepted"
    TypeGitTargetReady       = "GitTargetReady"
)
```

Canonical reads:

* fully mirrored: `Ready=True`, `Reconciling=False`, `Stalled=False`
* initial replay or recheck: `Ready=False`, `Reconciling=True`, `Stalled=False`
* refused Git path, invalid provider, RBAC denial, or broken encryption: `Ready=False`, `Reconciling=False`,
  `Stalled=True`
* Git path refusal details live on `GitPathAccepted=False` and `Stalled=True`, reason `UnsupportedContent`
* WatchRule and ClusterWatchRule carry target dependency health in `GitTargetReady`

### CommitRequest (one-shot)

CommitRequest runs once to a terminal outcome. Best-practice 1 above would suggest a `Succeeded` summary,
but it deliberately keeps `Ready` so every CRD in this project shares one summary type and the kstatus
trio; the bounded "did the work actually happen" signal lives on the `Pushed` domain condition instead.

```go
const (
    TypeReady       = "Ready"       // True at a non-error terminal outcome (committed, or a benign no-commit)
    TypeReconciling = "Reconciling" // True through both progress waits (reason names which)
    TypeStalled     = "Stalled"     // True when the finalize failed (kstatus Failed)
    TypeAttributed  = "Attributed"  // True once the author is settled (immediately True when not required)
    TypePushed      = "Pushed"      // True once the commit is in the remote repository
)
```

A request passes through two distinct, observable progress waits, both `Reconciling=True` but told apart
by the `Reconciling`/`Ready` reason and the `Attributed` condition.

Canonical reads:

* waiting for the create audit event (attributed-author mode only): `Reconciling=True` reason
  `WaitingForAuditEvent`, `Attributed=Unknown` → kstatus InProgress
* waiting for the close delay (author settled, attached — the `closeDelaySeconds` collect window, then
  commit and push): `Reconciling=True` reason `WaitingForCloseDelay`, `Attributed` settled, `Pushed=False`
  → kstatus InProgress
* committed: `Ready=True`, `Pushed=True`, `Stalled=False`, reason `Committed` → kstatus Current
* benign no-commit (nothing to save / already present / foreign open window): `Ready=True`, `Pushed=False`,
  `Stalled=False`, with the specific reason on `Ready` → kstatus Current (a correct, non-error outcome)
* failed finalize: `Ready=False`, `Stalled=True`, reason `FinalizeFailed` → kstatus Failed
* configured-author mode sets `Attributed=True` (`AttributionNotRequired`) immediately; attributed-author mode leaves
  it `Unknown` until the create audit event names the author, or `False` (`AuditEventNotObserved`) if it
  never arrives and the commit is authored by the configured committer
