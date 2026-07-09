# Identity-based write exclusion

> Status: implemented
> Related: [README.md](README.md), [../../configuration.md](../../configuration.md)

## Problem

A reverser that mirrors a branch a GitOps forward leg (Flux, Argo CD) also
applies will commit that forward leg's own applies. Nothing in the rule model
could express *"this write is not interesting"*: a `ResourceRule` filtered on
type and operation, never on **who wrote**.

## Shape

Two optional fields on `ResourceRule` (`WatchRule`) and `ClusterResourceRule`
(`ClusterWatchRule`):

```yaml
rules:
  - resources: ["configmaps"]
    excludeFieldManagers: ["kustomize-controller"]   # from watch state alone
    excludeUsers: ["system:serviceaccount:flux-system:kustomize-controller"]
```

`excludeFieldManagers` is the stronger form and the one to reach for. It reads
`metadata.managedFields` off the live object, so it needs no audit fact, cannot
race the attribution grace window, and works in configured-author mode.

`excludeUsers` matches the identity the audit webhook attributed the write to
(the impersonated user when impersonation is in play, otherwise the
authenticated user). It therefore requires `--author-attribution` and a working
audit webhook.

## Semantics

### Rules OR, exclusions veto within a rule

Rules are a logical OR: a resource matching **any** rule is watched. An
exclusion is a negative clause **within** one rule, not a global filter. Given

```yaml
rules:
  - resources: ["configmaps"]
    excludeFieldManagers: ["kustomize-controller"]
  - resources: ["configmaps"]
```

a write by `kustomize-controller` is still mirrored, because the second rule
admits it. Formally, for an event *e* with operation *op*, last writer *fm*,
and attributed user *u*, over the set *S* of compiled resource rules that select
the event's type and namespace:

> route *e* ⟺ ∃ *r* ∈ *S* : *r*.operations matches *op* ∧ *fm* ∉ *r*.excludeFieldManagers ∧ *u* ∉ *r*.excludeUsers

This is the only composition that keeps `rules` an OR.

### "The last writer" is the newest managedFields entry

`excludeFieldManagers` compares against the managers of the `managedFields`
entries carrying the **newest** `time`. If several entries tie on that
timestamp, the event is excluded only when **every** tied manager is excluded —
when in doubt, commit. An object with no `managedFields` at all is never
excluded.

### DELETE is never excluded by field manager

`managedFields` names who last *wrote* an object, not who deleted it. A human
deleting a Flux-managed ConfigMap would otherwise be silently ignored, which is
exactly the failure a label selector has. So `excludeFieldManagers` is not
evaluated for `DELETE`; `excludeUsers`, which reads the audit fact for the
delete itself, is.

### Unresolved identity fails open

If `excludeUsers` is set but the author cannot be attributed — attribution
disabled, or the grace window expired with no matching fact — the event is
**routed**, not dropped. Dropping a change because we failed to identify its
author would silently lose a human's edit. This is the reason to prefer
`excludeFieldManagers`.

### Exclusion suppresses *writes*, not *state*

An exclusion drops a live watch event. It does **not** remove the object from
the mark-and-sweep desired set that a replay or resync computes: an object a
GitOps tool manages is still an object the GitTarget mirrors, and dropping it
from the desired set would make the sweep delete its file from Git.

The practical consequence: the labels a GitOps tool stamps onto an object do
not reach Git on the live-event path, but they do land the next time that type
replays (a reconnect, a restart, a rule change). That write is idempotent and
does not re-trigger the forward leg, because the content it applies is what the
forward leg already applied. The loop is broken; the reconciliation is not.

## Where it is enforced

`internal/watch/target_watch.go`, in `routeLiveTargetWatchEvent`, alongside the
existing operation filter and before the content dedup. The raw watch object is
still un-sanitized there, so `managedFields` is available (`sanitize.Sanitize`
strips it when building the Git event).

Author attribution normally runs *after* the dedup, so a status-only update does
not pay the 3s grace window. When — and only when — some selecting rule declares
`excludeUsers`, attribution is resolved early so the exclusion can see the
identity, and the result is reused rather than looked up twice.

## Observability

`gitopsreverser_watch_events_excluded_total{gittarget_namespace, gittarget_name,
group, resource, reason}` counts dropped events, with `reason` one of
`field_manager` or `user`.
