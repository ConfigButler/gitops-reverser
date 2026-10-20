# Idea: name the Argo CD user behind a UI edit

Status: investigation only, nothing built. Read against Argo CD `master` at
[`4e682980`](https://github.com/argoproj/argo-cd/tree/4e682980460d2ebbaac20cf78f4795ce44c3cf34)
(2026-10-18).

## The question

For an ArgoCon demo: someone clicks around in the Argo CD UI, and the change shows up in a GitOps
folder. Can the commit name the person who was logged in, rather than Argo CD?

## Short answer

Not from the Kubernetes audit log. Partly from Argo CD's own audit Events, today. Exactly, after a
small upstream change.

| Source | Names the person? | Joins to the write exactly? |
|---|---|---|
| Kubernetes audit (what we use now) | No: `system:serviceaccount:argocd:argocd-server` | Yes |
| Argo CD audit Event | Yes, as `annotations.user` and in `message` | No, see below |
| Argo CD audit Event, with the upstream change | Yes | Yes, by uid + resourceVersion |

## Why the audit log cannot name the person

Every UI action that writes to the cluster is executed by `argocd-server` with its own credentials.
The audit event's `user` is the server's service account, and we take the author from `user`, or
from `impersonatedUser` when it is set
([audit_event_parsing.go](../../internal/queue/audit_event_parsing.go)).

Argo CD's impersonation feature does not change this. It impersonates the service account an
AppProject sets per destination in `destinationServiceAccounts`
([`DeriveServiceAccountToImpersonate`](https://github.com/argoproj/argo-cd/blob/4e682980460d2ebbaac20cf78f4795ce44c3cf34/util/settings/impersonation.go#L19)),
never the logged-in user. At best the author becomes a better-named service account.

## What Argo CD writes to its audit Event

`argocd-server` records each user action as a `core/v1` Event through
[`AuditLogger.logEvent`](https://github.com/argoproj/argo-cd/blob/4e682980460d2ebbaac20cf78f4795ce44c3cf34/util/argo/audit_logger.go#L49).
These Events are on by default: `--enable-k8s-event` / `ARGOCD_ENABLE_K8S_EVENT` defaults to `all`.
The user name sits on the **Event**, never on the changed resource.

These are all the fields it sets. Every other field of the Event is left empty.

| Field | Value |
|---|---|
| `metadata.name` | `<involvedObject.name>.<unix nanoseconds, hex>`. The only place the time is kept to the nanosecond. |
| `metadata.namespace` | The Application's namespace for app Events; Argo CD's own namespace for resource Events. |
| `metadata.labels` | App labels plus AppProject labels, filtered by `resource.includeEventLabelKeys` / `resource.excludeEventLabelKeys` in `argocd-cm`. Empty unless configured. Resource Events never have labels. |
| `metadata.annotations.user` | The logged-in user, see [Who "user" is](#who-user-is). Present only when there is one. |
| `metadata.annotations.dest-server` | `app.spec.destination.server` (app Events only). |
| `metadata.annotations.dest-namespace` | `app.spec.destination.namespace` (app Events only). |
| `metadata.annotations.resource-namespace` | The resource's namespace (resource Events only). |
| `involvedObject.{apiVersion,kind,name,namespace,uid,resourceVersion}` | The object the Event is about: the **Application** for app Events, the **resource** for resource Events. |
| `reason` | `ResourceCreated`, `ResourceUpdated`, `ResourceDeleted`, `ResourceActionRan` or `OperationStarted`. |
| `type` | `Normal` |
| `message` | `"<user> <action>"`, for example `alice@example.com patched resource apps/Deployment 'guestbook-ui'`. `"Unknown user"` when there is none. |
| `source.component` | `argocd-server` |
| `firstTimestamp`, `lastTimestamp` | `time.Now()` when the Event is written. These serialize to whole seconds. |
| `count` | `1` |

### Which UI action writes which Event

Line numbers refer to
[`server/application/application.go`](https://github.com/argoproj/argo-cd/blob/4e682980460d2ebbaac20cf78f4795ce44c3cf34/server/application/application.go).

| UI action | `reason` | `message` after the user name | `involvedObject` | Its `resourceVersion` |
|---|---|---|---|---|
| Edit live manifest ([L1574](https://github.com/argoproj/argo-cd/blob/4e682980460d2ebbaac20cf78f4795ce44c3cf34/server/application/application.go#L1574)) | `ResourceUpdated` | `patched resource <group>/<Kind> '<name>'` | Application | The app's, unrelated to the patch |
| Delete resource ([L1617](https://github.com/argoproj/argo-cd/blob/4e682980460d2ebbaac20cf78f4795ce44c3cf34/server/application/application.go#L1617)) | `ResourceDeleted` | `deleted resource <group>/<Kind> '<name>'` | Application | The app's |
| Resource action, such as restart ([L2686](https://github.com/argoproj/argo-cd/blob/4e682980460d2ebbaac20cf78f4795ce44c3cf34/server/application/application.go#L2686)) | `ResourceActionRan` | `ran action <action> on resource <group>/<Kind>/<name>` | Application | The app's |
| … and a second Event for the same action | `ResourceActionRan` | `ran action <action>` | **the resource** (has its uid) | **Wrong: the API version, such as `v1`** |
| Create app ([L395](https://github.com/argoproj/argo-cd/blob/4e682980460d2ebbaac20cf78f4795ce44c3cf34/server/application/application.go#L395)) | `ResourceCreated` | `created application` | Application | The new object's, so it matches |
| Edit app: `Update`, `UpdateSpec`, `Patch` ([L1045](https://github.com/argoproj/argo-cd/blob/4e682980460d2ebbaac20cf78f4795ce44c3cf34/server/application/application.go#L1045)) | `ResourceUpdated` | `updated application spec` | Application | The **old** one, from before the update |
| Delete app ([L1233](https://github.com/argoproj/argo-cd/blob/4e682980460d2ebbaac20cf78f4795ce44c3cf34/server/application/application.go#L1233)) | `ResourceDeleted` | `deleted application` | Application | The old one, which is fine for a delete |
| Sync ([L2212](https://github.com/argoproj/argo-cd/blob/4e682980460d2ebbaac20cf78f4795ce44c3cf34/server/application/application.go#L2212)) | `OperationStarted` | `initiated sync to <revision>` | Application | The new one, after the operation was set |
| Terminate operation ([L2548](https://github.com/argoproj/argo-cd/blob/4e682980460d2ebbaac20cf78f4795ce44c3cf34/server/application/application.go#L2548)) | `ResourceUpdated` | `terminated running operation` | Application | The old one |

All of these are written **after** the write to the cluster succeeded. A write that fails produces no
Event.

A Lua action can also **create** resources. Those creates are named in no Event at all.

For contrast, the application-controller writes Events through the same logger, but always without a
user, so they never carry `annotations.user`.

An Edit-live-manifest Event looks like this:

```yaml
apiVersion: v1
kind: Event
metadata:
  name: guestbook.18a2f3c4d5e6f7a8
  namespace: argocd
  annotations:
    user: alice@example.com
    dest-server: https://kubernetes.default.svc
    dest-namespace: guestbook
involvedObject:
  apiVersion: argoproj.io/v1alpha1
  kind: Application
  name: guestbook
  namespace: argocd
  uid: 0b6c…            # the Application's uid, not the Deployment's
  resourceVersion: "48211"
reason: ResourceUpdated
type: Normal
message: alice@example.com patched resource apps/Deployment 'guestbook-ui'
source:
  component: argocd-server
firstTimestamp: "2026-10-18T09:14:03Z"
lastTimestamp: "2026-10-18T09:14:03Z"
count: 1
```

### Who "user" is

[`session.Username`](https://github.com/argoproj/argo-cd/blob/4e682980460d2ebbaac20cf78f4795ce44c3cf34/util/session/sessionmanager.go#L693)
returns:

- for a local Argo CD account, its name, such as `admin`
- for SSO through Dex or another OIDC provider, the `email` claim when present, otherwise Dex's
  `federated_claims.user_id`, otherwise `sub`

For a demo, an SSO login gives a real email address.

## What we could build today: an approximate match

To match, we would read `argocd-server`'s Events and pair each one with an audit fact for the same
write:

- The audit fact says `argocd-server` wrote `apps/Deployment guestbook-ui` in `guestbook` at time T.
- The Event says `alice patched resource apps/Deployment 'guestbook-ui'` shortly after T, on an
  Application whose `dest-namespace` is `guestbook`.

The match is weaker than an audit match, for three reasons:

- **No resource key** on the patch and delete Events. The resource exists only as group, kind and
  name inside free text, and its namespace has to be taken from the Application's destination.
- **Time is the only order.** Two people editing the same object within the window cannot be told
  apart. The nanosecond time hidden in `metadata.name` helps, but it records when the Event was
  written, not when the write happened.
- **An Event proves nothing about who wrote it.** Anyone allowed to create Events in the Argo CD
  namespace can write `user: alice`. To trust one, we would check through audit that
  `argocd-server` created it, or present the result as Argo CD's claim and not as a fact.

That makes it a separate attribution tier, "Argo CD reports alice", alongside the audit match, never
mixed into it silently. It is good enough for a demo, but not for an audit trail.

## The upstream change that would make the match exact

`argocd-server` already holds the identity of what it wrote at the moment it logs the Event. It just
doesn't record it:

1. **Edit live manifest.** `kubectl.PatchResource` returns the patched object (`manifest`), with its
   uid and **new** resourceVersion, and the Event is logged on the very next lines. Writing a
   resource Event from `manifest` gives us uid + resourceVersion.
2. **Delete resource.** The tree node `res` already has the resource's uid. A resource Event with
   that uid is enough, since a delete is matched by uid.
3. **Resource actions.** `patchResource` and `createResource` throw away the API server's response.
   Returning it would give the new resourceVersion for patches and a uid for creates, which currently
   have no Event at all.
4. **Bug fix, worth sending on its own.** `LogResourceEvent` sets
   `involvedObject.resourceVersion` to `res.Version`, which is the **API version**
   ([audit_logger.go#L164](https://github.com/argoproj/argo-cd/blob/4e682980460d2ebbaac20cf78f4795ce44c3cf34/util/argo/audit_logger.go#L164)).
   `ResourceNode` does have a `ResourceVersion` field, but it comes from the cache, from before the
   action, so the real fix is to pass in the object the write returned.
5. **App edits and terminate** log the object from before the update (`app`, `a`) instead of the
   result (`res`, `updated`). Swapping them gives the new resourceVersion.

With uid + resourceVersion on the Event, it carries the same key as one of our audit facts
([author_fact.go](../../internal/queue/author_fact.go)). We could join it exactly and drop the
time-window guesswork. For writes made through the Argo CD UI, the user name would then be as exact
as the audit match.

### The alternative upstream change: forward the identity itself

If `argocd-server` impersonated the logged-in user (and their groups) on resource writes, the
Kubernetes audit event would carry `impersonatedUser: alice`. We already read that field, so this
would need no change on our side. But Argo CD would need `impersonate` rights on users, which is a
security-model change. Its current impersonation design deliberately stops at service accounts.
It's a strong message for a talk, and a long road to a merge.

## For the ArgoCon proposal

- **Works today:** click in the UI and the change lands in Git, authored as `argocd-server`.
- **Demo-able with modest work:** the real user name, from the approximate match on Argo CD's audit
  Events, with the caveats above.
- **The ask to the Argo CD community:** put the uid and resourceVersion of what you wrote on your
  audit Event. Items 1 to 3 are small, item 4 is a bug, and together they make UI attribution exact.
