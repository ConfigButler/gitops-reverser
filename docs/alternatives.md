# Alternatives

GitOps Reverser is not trying to be the answer to every Kubernetes change-tracking problem.

This document is here so the root README can stay focused, while still being honest about nearby
tools and tradeoffs.

## Quick framing

GitOps Reverser is strongest when you want:

- event-driven write-back from live Kubernetes activity into Git
- sanitized, deployable manifests rather than raw object dumps
- a bridge from API-first operations toward Git-backed workflows

It is weaker when you want:

- broad observability and incident automation
- managed-cluster compatibility without control-plane access
- periodic whole-cluster snapshotting rather than per-event commits

## Nearby tools

| Tool | What it does well | Where GitOps Reverser differs |
|---|---|---|
| [syngit-org/syngit](https://github.com/syngit-org/syngit) | Reverse GitOps with admission webhooks: commit the change to Git first, then let it reach the cluster | GitOps Reverser observes changes after they are admitted, so it never sits in the write path |
| [robusta-dev/robusta](https://github.com/robusta-dev/robusta) | Observability, alert enrichment, automation workflows | GitOps Reverser is much narrower and focused on Git write-back |
| [RichardoC/kube-audit-rest](https://github.com/RichardoC/kube-audit-rest) | Audit-event collection without the full Git write-back model | GitOps Reverser goes further into sanitization, Git shaping, and repo-oriented workflows |
| [bpineau/katafygio](https://github.com/bpineau/katafygio) | Snapshotting cluster resources into Git | GitOps Reverser is event-driven and commit-oriented rather than snapshot-first |

## When another option may be better

### You want Git to gate the change, not record it

Syngit is the closest neighbour to GitOps Reverser: it also turns live cluster changes into commits,
so that people can keep using a UI or `kubectl` without Git falling behind.

The difference is where it sits. Syngit intercepts writes with a validating webhook and commits them
before the change is applied — if the push fails, the resource is not admitted. That keeps Git strictly
in the lead, at the cost of putting your Git provider in the critical path of every matched write.

GitOps Reverser works from audit events, after the API server has already admitted the change. Git
converges a moment later instead of gating the write, so an unreachable Git host slows down write-back
rather than blocking the cluster. Pick Syngit if a change that never reached Git must never reach the
cluster either; pick GitOps Reverser if the write path must stay available regardless of Git.

### You mainly want observability

If the real goal is alerting, enrichment, remediations, or incident workflows, a broader platform
like Robusta may be a better fit.

### You cannot touch kube-apiserver audit configuration

If you are on a managed control plane and cannot configure the audit webhook backend, GitOps
Reverser is usually not the right tool today. A lighter audit collector may be easier to deploy,
though it will often give you less of the full model.

### You prefer periodic exports over event history

If a scheduled snapshot of cluster state is enough, a tool like Katafygio can be simpler to reason
about than an event-driven commit stream.

## Practical recommendation

If you are evaluating the space, ask these questions first:

- Do we control the kube-apiserver audit webhook configuration?
- Should a failed Git push block the change from reaching the cluster, or only delay the commit?
- Do we care about per-change history, or is a periodic snapshot enough?
- Do we need deployable manifests in Git, or only an audit record?
- Are we trying to support API-first workflows, or replace them?

Those answers usually make the tool choice much clearer.
