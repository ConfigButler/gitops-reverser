# Idea: Burst commit batching (rolling-window coalescing)

## The idea in one sentence

Events that arrive within a short rolling time window get folded into a single commit with a
summary message, instead of producing one commit per resource change.

---

## Context: what happens today

The `BranchWorker` already buffers events and controls push frequency via `push.interval` and
`push.maxCommits`. But the current batching is about **when to push**, not about **commit
granularity**. When the push ticker fires, every event in the buffer becomes its own individual
commit:

```
[CREATE] apps/v1/deployments/default/api-server
[CREATE] apps/v1/deployments/default/worker
[CREATE] v1/configmaps/default/api-config
[CREATE] v1/services/default/api-server
[CREATE] v1/serviceaccounts/default/api-sa
```

A `kubectl apply -k ./base/` on a modest kustomize overlay produces exactly this — five or ten
commits in a row, all within the same second, that read as noise rather than as a meaningful
operation. The user did one logical thing; the history shows five.

---

## The proposal

Introduce a **commit window** (separate from the push interval): a short rolling duration during
which arriving events are held and then written to the repository as one atomic commit with a
single summary message.

```yaml
spec:
  push:
    interval: "1m"       # unchanged — controls how often to push to remote
    maxCommits: 20       # unchanged — hard cap, forces a flush
    commitWindow: "10s"  # new — how long to wait for more events before committing
```

A `kubectl apply -k ./base/` that touches five resources within two seconds would produce one
commit:

```
apply: 5 resources (deployments ×2, configmaps, services, serviceaccounts)
```

instead of five. The push interval then controls when that one commit reaches the remote, same as
today.

---

## How it would work internally

The `processEvents` loop in `BranchWorker` currently has a single ticker (the push interval) that
drains the buffer. With a commit window, two timers coexist:

- **commit timer** (short, e.g. 10s): restarted each time a new event arrives. When it fires
  without new events, the buffer is committed as one atomic batch.
- **push ticker** (existing, e.g. 1m): forces a commit-and-push of anything still buffered,
  regardless of whether the commit timer has fired. Unchanged from today.
- **maxCommits cap** (existing): forces an immediate flush if the buffer grows too large.

The commit produces a batch message summarising the events: operation counts, resource kinds, and
optionally the first few resource names. The template system from
[gitprovider-commits-api.md](../design/gitprovider-commits-api.md) would use the existing
`BatchTemplate` field for this, reusing `BatchCommitMessageData`.

---

## Pros

**One logical operation → one commit.**
`kubectl apply -f ./namespace/`, `helm upgrade`, and ArgoCD sync waves all touch multiple resources
in rapid succession. Today's history is noise; with burst batching it becomes a clean operation
record.

**Directly improves the ArgoCD use case.**
The [application-editing idea](idea-application-editing.md) already identifies author + time as
the natural grouping signal for GUI sessions. Burst batching is the commit-level implementation of
the time dimension of that idea. A user clicking through the ArgoCD GUI for 30 seconds produces
one branch commit summarising the session, not 12.

**No new information is lost.**
Per-event detail is still in the commit's body (or recoverable from the files changed). The
summary is the subject line; each resource's YAML is still in the tree. `git log --stat` shows all
files touched.

**Fits naturally into the existing API.**
`commitWindow` is a sibling of `interval` in `PushStrategy`. It's orthogonal to the message
template, signing, and committer identity — no cross-cutting concerns.

**Makes the message template more useful.**
The `BatchTemplate` field in the `commits.message` block becomes the primary message for a wider
class of real-world commits, not just reconcile snapshots.

---

## Cons and open questions

**The window length is a guess.**
10 seconds feels right for interactive `kubectl apply` sessions. But a slow Helm chart with
templating overhead, or an ArgoCD sync that pauses between resource groups, might spread events
over 30–60 seconds. Too short a window and you get multiple commits anyway; too long and latency
to first commit is annoying. The right value is workload-dependent. Mitigation: make it
configurable and default to something conservative (15–30s).

**Mixed sources within one window.**
If Alice runs `kubectl apply` while Bob deletes a resource in the same 10-second window, on the
same branch, their changes land in one commit attributed to... whom? The author field collapses.
Options:
- Use the committer identity only (bot), drop per-event author attribution for batches. Clean but
  loses the audit signal.
- List all contributors in the commit body. Preserves the information but messier.
- Break on author change: treat a different username as a reason to flush and start a new batch.
  This is the cleanest model but requires tracking the "current batch owner" in the worker.

The author-break model aligns well with the ArgoCD use case and is probably the right default.

**The summary message is an approximation.**
"5 resources" is less informative than five individual `[CREATE]` lines. Users who rely on `git
log --oneline` for their change history will see less detail. The template system partially
mitigates this — a good `BatchTemplate` can include resource kinds and counts — but the per-event
subject precision is genuinely gone.

**Relationship to `CommitModeAtomic`.**
The existing `CommitModeAtomic` (used for reconcile snapshots) already creates one commit for
many events. Burst batching is conceptually similar but event-driven rather than caller-driven.
The implementation should reuse the atomic commit path rather than adding a third mode.

**Not a substitute for structured apply metadata.**
The ideal solution would be for the Kubernetes API server to tag batched operations (e.g. server-
side apply's field manager is already a step in this direction). If a future Kubernetes version
exposes apply-group metadata in the audit log, that would be a better grouping signal than time
proximity. For now, time is what we have.

---

## Relationship to other ideas

| Idea | Connection |
|---|---|
| [ArgoCD application editing](idea-application-editing.md) | Burst batching is the commit-level mechanism for the "time-based grouping" strategy already described there. The author-break flush model maps directly onto the author-based grouping idea in that doc. |
| [gitprovider-commits-api.md](../design/gitprovider-commits-api.md) | `BatchTemplate` and `CommitMessageData` are the message-layer for burst batch commits. The `commitWindow` field lives in `PushStrategy` alongside the existing `interval`. |

---

## Suggested default behaviour

If not configured, commit window is disabled — every event still produces its own commit, same as
today. Opt-in keeps the change non-breaking and avoids surprising existing users whose tooling
parses per-event commit messages.

```yaml
# Today's default — no change
spec:
  push:
    interval: "1m"
    maxCommits: 20

# Opt in to burst batching
spec:
  push:
    interval: "1m"
    maxCommits: 20
    commitWindow: "15s"
  commit:
    message:
      batchTemplate: "apply: {{.Count}} resources ({{.Kinds}})"
```

Where `{{.Kinds}}` would be a new convenience field on `BatchCommitMessageData` — a compact
summary of the distinct resource kinds touched, e.g. `"deployments ×2, configmaps, services"`.
