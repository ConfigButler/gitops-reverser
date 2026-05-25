# `CommitRequest` generated-name audit handling

`CommitRequest` audit handling fails for resources created with
`metadata.generateName`.

## Severity

Medium-high. The watched resource change is still committed eventually through
the normal commit window, but the explicit `CommitRequest` finalize signal is
lost. That means:

- user-provided save messages are ignored;
- "save now" does not save now;
- `CommitRequest.status.phase` stays at `WaitingForAuditEvent`;
- the resulting Git commit may use the fallback grouped message and timing.

This is visible in the voter demo because the auth-service creates
`CommitRequest` objects with `metadata.generateName`.

## Environment

- `gitops-reverser` chart/app: `0.26.1`
- Image observed live: `ghcr.io/configbutler/gitops-reverser:0.26.1`
- Cluster: Kubernetes `v1.36.0` on Talos / Cozystack
- Audit policy: `RequestResponse` for mutating verbs (`create`, `update`,
  `patch`, `delete`, `deletecollection`)
- GitOps Reverser audit endpoint is receiving events successfully
- Affected resource:
  - API group: `configbutler.ai`
  - version: `v1alpha1`
  - resource: `commitrequests`

## Symptom

Creating a `CommitRequest` with `metadata.generateName` leaves the object stuck
in:

```text
WaitingForAuditEvent
```

Live objects:

```text
NAMESPACE          NAME                GITTARGET    PHASE
voter-production   coffee-save-4dnln   voter-demo   WaitingForAuditEvent
voter-production   coffee-save-8tw8m   voter-demo   WaitingForAuditEvent
```

The controller log shows the audit consumer did see a `CommitRequest` create
event, but it tried to read a resource with an empty name:

```text
Failed to read CommitRequest; skipping
commitRequest="voter-production/"
error="resource name may not be empty"
```

The same log stream also confirms impersonated audit events are reaching the
controller:

```text
First impersonated audit event observed
authUser="system:serviceaccount:voter-production:auth-service"
impersonatedUser="Simon2"
```

So the issue is not "no audit event arrived"; the event arrived, but the
`CommitRequest` identity was resolved incorrectly.

## Kubernetes behavior behind this

`generateName` is a Kubernetes server-side name generation mechanism. A client
creates an object with:

```yaml
metadata:
  generateName: coffee-save-
```

and sends a collection `POST` to:

```text
/apis/configbutler.ai/v1alpha1/namespaces/voter-production/commitrequests
```

The API server allocates the final name, for example:

```text
coffee-save-8tw8m
```

For create audit events on collection endpoints, `audit.Event.objectRef` may
identify the collection (`namespace`, `resource`, `apiGroup`, `apiVersion`)
without carrying the final generated `name`. The generated name is available in
the audit body at `responseObject.metadata.name` when the audit level is
`RequestResponse`.

That means code consuming audit events must not assume
`event.ObjectRef.Name != ""` for all successful create events. The audit
`objectRef` is still the first source of identity when it is populated, because
it is the URL-level reference Kubernetes attached to the event. For
server-named resources, any missing identity fields must be backfilled from the
audit bodies:

1. start with `objectRef.{namespace,name,uid}`;
2. for create/update/patch, fill missing fields from
   `responseObject.metadata.{namespace,name,uid}`;
3. for delete, fill missing fields from
   `requestObject.metadata.{namespace,name,uid}`;
4. fall back to the other audit body only if the preferred body is absent or
   incomplete.

## Why this is broader than `CommitRequest`

The normal resource-write path in `gitops-reverser` already has similar
behavior:

- `routeAuditEvent` starts with `ref.Name`;
- `extractObject` selects the audit body;
- `backfillSanitizedIdentity` fills missing object identity from the body or
  from `objectRef`.

There is even a unit test for this generic path:

```text
TestProcessMessage_UsesRequestObjectIdentityWhenObjectRefNameMissing
```

However, `CommitRequest` is handled as a special control-plane signal before
the generic resource extraction path:

```go
if c.isCommitRequestCreate(auditEvent) {
    c.handleCommitRequest(ctx, log, auditEvent)
    c.ackMessage(ctx, msg.ID)
    return
}
```

`handleCommitRequest` currently does this:

```go
ref := event.ObjectRef
log = log.WithValues("commitRequest", ref.Namespace+"/"+ref.Name)

err := c.apiReader.Get(ctx, client.ObjectKey{
    Namespace: ref.Namespace,
    Name:      ref.Name,
}, &commitRequest)
```

For a `generateName` create event where `objectRef.name == ""`, this becomes:

```text
client.ObjectKey{Namespace: "voter-production", Name: ""}
```

and the signal is skipped permanently after the audit message is ACKed.

## Expected behavior

For a successful `CommitRequest` create audit event:

1. Resolve the object key from `objectRef` when available.
2. If `objectRef.name` is empty, parse `responseObject` and use
   `metadata.name`.
3. If namespace is also missing, backfill `metadata.namespace` from the body.
4. Carry `metadata.uid` from the resolved identity into the existing
   stale-object protection.
5. Fetch the resolved `CommitRequest`.
6. Finalize the matching open GitTarget window.
7. Write terminal status (`Committed`, `NoOpenWindow`, or `Failed`).

`metadata.generateName` should work because the public sample for
`CommitRequest` recommends it:

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: CommitRequest
metadata:
  generateName: save-
```

## Actual behavior

`handleCommitRequest` uses `event.ObjectRef.Name` directly. When that value is
empty, it logs `resource name may not be empty`, ACKs the audit message, and
leaves the persisted `CommitRequest` in `WaitingForAuditEvent`.

## Reproduction

1. Configure a `GitProvider`, `GitTarget`, and `WatchRule`.
2. Make a watched write that opens a commit window.
3. Create a `CommitRequest` with `metadata.generateName`:

   ```yaml
   apiVersion: configbutler.ai/v1alpha1
   kind: CommitRequest
   metadata:
     generateName: save-
     namespace: voter-production
   spec:
     gitTargetRef:
       name: voter-demo
     message: "Lower espresso price"
   ```

4. Observe that the created object gets a generated name:

   ```text
   save-abcde
   ```

5. Observe the controller log:

   ```text
   Failed to read CommitRequest; skipping
   commitRequest="voter-production/"
   error="resource name may not be empty"
   ```

6. Observe `.status.phase` remains `WaitingForAuditEvent`.

## Suggested implementation

Add a common audit identity helper and use it consistently for every event path,
including special control resources:

```go
type AuditObjectIdentity struct {
    Namespace string
    Name      string
    UID       types.UID
}

func IdentityFromAuditEvent(event auditv1.Event, op configv1alpha1.OperationType) AuditObjectIdentity {
    // Start from event.ObjectRef.
    // Backfill only missing namespace/name/uid fields from the preferred body.
    // For non-delete operations the preferred body is responseObject.
    // For delete operations the preferred body is requestObject.
    // Fall back to the other body if needed.
}
```

For non-delete operations, prefer `responseObject` before `requestObject`.
For delete operations, prefer `requestObject` before `responseObject`, matching
the existing object extraction semantics.

Then update `handleCommitRequest` to use that helper instead of reading
`event.ObjectRef.Name` directly.

Minimal targeted fix:

```go
key := commitRequestObjectKeyFromAuditEvent(event)
if key.Namespace == "" || key.Name == "" {
    log.Info("CommitRequest audit event did not identify an object; skipping")
    return
}

if err := c.apiReader.Get(ctx, key, &commitRequest); err != nil {
    ...
}
```

where `commitRequestObjectKeyFromAuditEvent` backfills missing namespace, name,
and UID from `event.ResponseObject.Raw` and then `event.RequestObject.Raw`.
`handleCommitRequest` should use the resolved UID when checking whether the
fetched object still matches the audit event; an empty resolved UID remains a
match for compatibility with bodyless or lower-detail audit events.

## Test coverage to add

Add a unit test for the special path:

```text
TestHandleCommitRequest_UsesResponseObjectNameWhenObjectRefNameEmpty
```

Shape:

- create a fake `CommitRequest` named `save-generated`;
- create an audit event with:
  - `verb=create`
  - `stage=ResponseComplete`
  - `objectRef.resource=commitrequests`
  - `objectRef.namespace=team-a`
  - `objectRef.name=""`
  - `responseObject.metadata.name=save-generated`
- assert `FinalizeGitTargetWindow` is called;
- assert the `CommitRequest` reaches `Committed`.

Also add helper-level coverage for identity resolution:

- objectRef namespace/name/uid still wins when present;
- missing `objectRef.name` is backfilled from `responseObject.metadata.name`;
- missing `objectRef.uid` is backfilled from `responseObject.metadata.uid`;
- missing response identity falls back to `requestObject.metadata`;
- delete operations prefer request identity before response identity;
- an event with no resolvable namespace or name is skipped without finalizing;
- a resolved UID mismatch is treated as a stale event and does not finalize.

Also consider an integration/e2e variant that creates the `CommitRequest` with
`generateName`, not explicit `metadata.name`. The current e2e test uses a fixed
name and therefore misses this bug.

## Proposed acceptance criteria

- `CommitRequest` created with explicit `metadata.name` still works.
- `CommitRequest` created with `metadata.generateName` reaches a terminal
  phase after its create audit event.
- The Git commit uses `spec.message` from the generated-name `CommitRequest`.
- The audit consumer does not log `resource name may not be empty` for
  successful generated-name creates.
- The shared audit identity helper is used by all paths that need object
  identity, or there is a documented reason for any exception.

## Appendix: related hardening item

During the same investigation, Valkey was also unstable:

```text
restartCount: 9
lastState.terminated.exitCode: 137
RDB memory usage when created: 6124.68 Mb
```

The chart default `queue.redis.maxLen=0` leaves the hot audit stream unbounded.
That is not the root cause of the empty-name bug, but it can cause missed audit
events and long queue outages during restarts. Production/demo installs should
set a bounded stream length, for example:

```yaml
queue:
  redis:
    maxLen: 10000
```

This is out of scope for the generated-name fix, but should be tracked as a
separate production hardening item.
