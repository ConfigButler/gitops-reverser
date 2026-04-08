# Idea: GitOps-backed ArgoCD Application editing

## Context

gitops-reverser is currently positioned as a tool for "intent clusters" — test environments where you
explore changes before promoting them. This document explores one concrete and powerful use case for
that positioning: making the ArgoCD GUI a first-class GitOps authoring tool.

## The idea

When running ArgoCD in a test environment, every change you make through the GUI could automatically
be captured in git. The flow would look like this:

1. A new ArgoCD `Application` is created (through the GUI or otherwise)
   → gitops-reverser creates a new git branch and annotates the `Application` with the branch name.
2. The `Application` spec changes
   → gitops-reverser commits the new state to that branch.
3. The `Application` is deleted
   → gitops-reverser deletes the branch.
4. The branch is merged to main
   → gitops-reverser removes the annotation. From this point on, the configuration lives in main
     and is active on both test and production.

The result: anyone who knows how to use the ArgoCD GUI can author GitOps-managed infrastructure
without ever touching a YAML file or understanding branching strategy. The PR is the handoff point —
the moment where "I'm done experimenting" becomes "ship it."

## Why this is compelling

- **Low barrier to entry.** The target user understands GitOps and Kubernetes but doesn't want to
  manually manage branches and YAML. This pattern does that for them.
- **Clean lifecycle.** The branch mirrors the Application's lifetime exactly. There is no ambiguity
  about what is in progress vs. what is live.
- **Bootstrapping is not a concern here.** Setting up ArgoCD itself is an advanced task, done by
  platform engineers or automation. This pattern targets the users who come *after* that — the people
  who create Applications day-to-day and want their changes tracked without extra friction.

## Open design questions

### Environment-specific configuration
The most significant gap. A test Application might have `replicas: 1`, no resource limits, and point
to a test branch. The production version needs different values for all three. The recommended approach
is a Kustomize overlay per environment that injects the environment-specific values (branch name,
resource limits, replica count) on top of a shared base. gitops-reverser would commit the base; the
overlay handles the rest.

A ConfigMap-merge approach is also possible but adds abstraction that may not pay for itself.

### The "config branch as source of truth" problem
If a user sets the `Application`'s `spec.source.targetRevision` to the gitops-reverser-managed branch
(instead of main), they have made the experiment branch the source of truth. This is probably not what
they intended. Should gitops-reverser detect and warn about this? Should it rewrite the field on merge?
This needs a clear policy.

### Merge trigger: PR or automatic?
Two models:
- **PR-gated**: gitops-reverser opens a pull request when a merge signal is detected. A human approves
  and merges. Safe, auditable, fits most team workflows.
- **Automatic**: gitops-reverser merges the branch directly when some condition is met (e.g. CI passes,
  or the Application is explicitly marked "ready"). Faster, but harder to recover from mistakes.

Both are valid. The PR model is the safer default and maps well to the annotation-removal lifecycle
described above.

### Deletion semantics
"Branch deleted when Application is deleted" is the clean version of the lifecycle, but it is
destructive. Deleting a test Application to clean up cluster resources is routine; silently deleting
the git branch along with it may surprise users. An alternative: deletion closes (but does not delete)
the branch, or opens a "discard this?" PR. The right answer depends on how recoverable the user
expects deletions to be.

### Auto-removing the Application on PR close
If the branch is deleted (or the PR is closed without merging), should gitops-reverser delete the
corresponding `Application` from the cluster? This would make the lifecycle fully bidirectional — git
events drive cluster state just as cluster state drives git. It is a powerful property but also
dangerous: a mistakenly closed PR would take down a running workload.

## Grouping: what belongs on the same branch?

One branch per `Application` is too granular. A user clicking around in the ArgoCD GUI in one session
will touch multiple Applications and their dependent resources. Creating a separate branch per object
produces a fragmented, unmergeable mess.

There are a few strategies for deciding what belongs together, and they are not mutually exclusive:

**Time-based batching.** If changes from the same source arrive within a short window (say 30–60
seconds of each other), group them into one branch. This is low-effort to implement and works well for
interactive sessions. The risk is that two unrelated users happen to make changes in the same window
and their work gets combined.

**Author-based grouping.** All changes from the same user go to the same branch, at least within a
session. This is the most intuitive model. It requires knowing who made the change — which is exactly
what the audit webhook pipeline in gitops-reverser is being built to provide. The audit log gives you
username attribution per event; grouping by author then becomes a natural downstream use of that
signal.

**CRD relationship mapping.** This is the most powerful approach and the most complex. Many Kubernetes
resources reference each other: an `Application` references a source repo and destination namespace; a
`Deployment` owns `ReplicaSets`; a `Service` selects `Pods` by label. By building a lightweight
reference map — walking `ownerReferences`, label selectors, and known cross-resource fields — you can
infer with high probability that a set of changes belongs together, even if they come from different
API calls. You could represent this as a configurable "affinity map" on the WatchRule CRD: something
like `groupWith: [apps/v1/Deployment, v1/Service]` to express that changes to these resource types
should land on the same branch as the Application that owns them.

In practice, a combination of author + time is probably the right starting point. The reference map
is the long-term vision but requires more design work to get right.

## Known limitation: branch divergence and file conflicts

This is a real drawback and should be stated honestly.

Branches created by gitops-reverser start from main at creation time. As time passes and main moves
forward, those branches diverge. The longer a branch lives, the harder it becomes to merge —
especially if the same file was modified on main in the meantime (e.g. a shared `Deployment` that
another team already updated and promoted).

This problem is not unique to gitops-reverser, but the pattern amplifies it because branches are
created automatically and may linger without the user realising. A few partial mitigations:

- **Encourage short-lived branches.** The design should push users toward merge-early behaviour. A
  branch that is open for more than a few days should surface a warning.
- **Detect conflicts early.** Before committing to a branch, gitops-reverser could check whether the
  target file already exists on main and flag a divergence risk in the annotation or in the PR
  description.
- **Rebase rather than merge.** When the PR is created, rebasing onto the current tip of main reduces
  the diff and makes conflicts more obvious earlier.

None of these fully solve the problem. If the same file genuinely exists on both the branch and main
with different content, a human needs to resolve it. This is an inherent limitation of the multi-branch
approach and is worth documenting clearly for users so they are not surprised.
