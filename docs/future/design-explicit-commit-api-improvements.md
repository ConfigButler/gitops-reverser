# Follow-up: `ExplicitCommit` implementation improvements

> Status: implemented (2026-05-18). Follow-up to
> [design-explicit-commit-api.md](design-explicit-commit-api.md).
> Date: 2026-05-18

## Summary

The first `ExplicitCommit` implementation matches the broad shape of the design: a namespaced CRD
is created after user edits, its own audit event triggers a finalize signal, and status reports
`Committed` or `NoOpenWindow`.

The review found a few correctness gaps where the implementation should become stricter before the
API is treated as stable. The most important theme is binding the finalize signal to the exact
intent represented by the audit event: the effective user, the referenced `GitTarget`, and the
specific `ExplicitCommit` object instance.

All findings below have since been implemented. One decision diverged from the original proposal:
finding #2 recommended leaving failed finalizes in `WaitingForAuditEvent` with no `Failed` phase,
but a terminal `Failed` phase was added instead so finalize failures are user-visible in status.
The relevant sections record the decision inline.

## Findings

### 1. Bind finalization to the audit-event author

The design says:

> finalize the open commit window for the authenticated author on the referenced `GitTarget`

The current implementation reads the author from the audit event, but uses it only for logging.
`FinalizeGitTargetWindow` receives the target and message, and the branch worker finalizes whichever
open window is present on that provider/branch.

Why this matters:

- Branch workers are keyed by provider and branch, not by author or `GitTarget`.
- The open window does track author, target name, and target namespace.
- If the currently-open window belongs to a different user or target, the explicit save could commit
  the wrong edits with the caller's message.

Implemented:

- `git.FinalizeSignal` gained `Author`, `GitTargetName`, and `GitTargetNamespace` fields, with a
  `matchesWindow` helper.
- `FinalizeGitTargetWindow` takes an `author` argument; `handleExplicitCommit` passes
  `resolveUserInfo(event).Username`.
- `handleFinalizeSignal` returns `FinalizeNoOpenWindow` and leaves the window open unless the
  current `openWindow` matches all three fields.
- Tests cover: matching author and target commits the window; a different author returns
  `NoOpenWindow` and leaves the window open; a different target returns `NoOpenWindow` and leaves
  the window open.

This preserves the existing queue-ordering argument while making "the open window" mean "the
author's open window for this target."

### 2. Propagate finalize errors instead of mapping them to `NoOpenWindow`

Some finalize paths return `git.FinalizeResult{Err: ...}` without returning a Go error from
`FinalizeGitTargetWindow`. The status mapper treats unknown or empty outcomes as `NoOpenWindow`.
That can turn real failures, such as a full queue or a failed local commit, into a terminal
non-error phase.

Implemented:

- `FinalizeGitTargetWindow` now returns the result's `Err` as a Go error instead of `(result, nil)`,
  so a finalize failure can no longer be mistaken for a benign outcome.
- A terminal `Failed` phase was added (this diverges from the original proposal, which suggested
  leaving the object in `WaitingForAuditEvent`). The decision was made because the audit message is
  ACKed exactly once with no redelivery: leaving the object in `WaitingForAuditEvent` would strand
  it silently, with the reason only in logs. `Failed` surfaces the failure in status instead.
- `ExplicitCommitStatus` gained a `message` field carrying the failure detail. An unknown or empty
  finalize outcome with no error is also recorded as `Failed` rather than `NoOpenWindow`.
- Trade-off: a transient `ErrFinalizeQueueFull` now also becomes a terminal `Failed`. This is
  intentional — without redelivery there is no retry, so a terminal phase is more honest than a
  stuck `WaitingForAuditEvent`. The rolling silence timer still commits the pending edits later
  with the generated message; only the caller's custom message is lost.

### 3. Compare the audit object UID before acting

The audit event carries `objectRef.uid`, but the consumer currently reads the `ExplicitCommit` by
namespace/name only.

Why this matters:

- An `ExplicitCommit` could be deleted and recreated with the same name before a delayed audit event
  is processed.
- The stale audit event could then act on the new object's spec and write its status.

Implemented:

- `handleExplicitCommit` compares `event.ObjectRef.UID` (when set) to the fetched object's UID via
  `auditEventMatchesObject`, and logs and skips the event when they differ.
- `writeExplicitCommitStatus` re-checks the UID in the status-write retry loop.
- An empty event UID (e.g. a `Metadata`-level audit policy that omits it) is treated as a match.

Using `generateName` makes name reuse unlikely, but the consumer still honors the identity the
audit event gave it.

### 4. Finish chart and operator-facing docs

The implementation adds the CRD and RBAC, but install guidance should also reflect the new resource.

Implemented:

- `explicitcommits.configbutler.ai` is listed in the Helm README CRD list, the feature bullet, and
  both `kubectl delete crd` cleanup commands.
- The chart's `crds/` directory is synced from `config/crd/bases` by the `helm-sync` task, so the
  new CRD is packaged automatically.
- The e2e audit policy already captures all `create` events with a catch-all rule; a comment was
  added noting that, if the catch-all is ever narrowed, `explicitcommits` create events must keep at
  least `Metadata`-level capture (the consumer reads the object body from the apiserver).

## Commit message validation

The open question is whether `spec.message` should be rejected, sanitized, or accepted and used
verbatim. For a first version, the cleanest contract is:

- Prefer API rejection for clearly invalid input.
- Avoid silent rewriting when the message is valid enough to accept.
- Keep a small defensive cleanup in the consumer only as a guardrail, not as the main contract.

### Recommended first-version contract

`spec.message` should be optional. When present:

- limit it to 1-1024 Unicode characters, not bytes;
- allow normal printable Unicode;
- allow newline (`\n`) so users can create a subject plus body;
- reject ASCII control characters other than newline;
- preserve the accepted message exactly when creating the commit.

This is simple, explainable, and Kubernetes-native. Kubernetes API requests are JSON, so strings are
already valid Unicode by the time they reach CRD validation; the API does not need a separate UTF-8
checker. Exact byte-length limits are not worth the complexity for the first version. Git can handle
messages larger than 1024 bytes, and a 1024-character cap is already small enough to prevent abuse.

### CRD validation shape

Keep:

```go
// +kubebuilder:validation:MinLength=1
// +kubebuilder:validation:MaxLength=1024
```

Add a pattern that rejects C0 controls and DEL while allowing newline:

```go
// +kubebuilder:validation:Pattern=`^[^\x00-\x09\x0B-\x1F\x7F]*$`
```

That pattern intentionally disallows tab. If tab support is desirable, use this variant instead:

```go
// +kubebuilder:validation:Pattern=`^[^\x00-\x08\x0B-\x1F\x7F]*$`
```

The design currently says "no control characters except `\n`", so the no-tab version is the better
match.

### Runtime handling

Once CRD validation rejects bad messages, the consumer should not normally sanitize. It should use
the accepted value as-is.

A small helper can remain as defense in depth, but it should be boring:

- normalize CRLF and CR line endings to LF, or reject carriage return via the pattern and leave this
  out;
- cap extreme length defensively if an object somehow bypasses validation;
- do not trim leading/trailing whitespace before committing.

One implementation choice to make explicit: an all-whitespace message is probably not useful, but it
is not dangerous. For the first version, allow it and let Git record exactly what the user supplied.
If product feedback says this is confusing, add a later CEL validation rule or webhook.

### Status behavior for invalid existing objects

As implemented, the consumer does not re-validate message content: CRD validation owns that
contract, and the accepted message is used verbatim (only a defensive byte-length cap is applied).
The `ExplicitCommit` API is `v1alpha1` and the pattern shipped with it, so there are no pre-pattern
objects to migrate. A finalize that genuinely fails now lands in the terminal `Failed` phase (see
finding #2) rather than silently staying in `WaitingForAuditEvent`.

## Implementation order (completed)

1. ✅ Fixed error propagation so failed finalize signals no longer become `NoOpenWindow`; added the
   terminal `Failed` phase.
2. ✅ Added author and target matching to `FinalizeSignal`.
3. ✅ Added UID checks around object reads and status writes.
4. ✅ Tightened message validation (CRD pattern) and removed normal-path sanitization.
5. ✅ Updated Helm/docs packaging and added focused tests for each behavior.
