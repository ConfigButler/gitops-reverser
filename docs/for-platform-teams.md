# Let teams change the cluster through Kubernetes, with Git as the record

This page is for a platform team that runs Kubernetes with a GitOps repository, usually applied by
Flux or Argo CD, and serves application teams who change what runs there. It shows what GitOps
Reverser changes for them, how one installation serves many teams, and which guides to follow in
which order.

## The problem

GitOps keeps the desired state in Git, so every change has to arrive as a commit. That works for the
people who know the repository, and creates friction for everyone else:

- A developer who needs a ConfigMap changed has to know the folder layout, the overlays, and the
  review rules, or file a ticket and wait.
- A portal, a CI job, or an AI agent that changes the cluster needs a Git client, credentials, and
  knowledge of where each file lives.
- A `kubectl` fix during an incident either gets reverted at the next apply or lives on as drift.
- An existing cluster holds state that was never committed anywhere.

## What changes

People and tools change the cluster through the Kubernetes API: `kubectl`, a portal, a CI job, or an
agent. Kubernetes RBAC and admission decide what each of them may do. GitOps Reverser writes each
change into the right folder as a commit within seconds, and Flux or Argo CD keeps deploying from
the repository.

- **Files keep their shape.** An update edits the existing YAML document in place, and in a
  supported Kustomize layout it edits the source manifest. See
  [what it can write](../README.md#what-it-can-write).
- **Each commit can name its author.** With [author attribution](attribution-setup-guide.md), the
  commit names the identity the API server authenticated.
- **A commit can say why.** A [`CommitRequest`](demo/commit-window.md) groups a set of edits into one
  commit with the author's own message.
- **The applier waits for Git.** When Flux or Argo CD applies a folder that GitOps Reverser writes,
  an applier claim lets GitOps Reverser decide when the applier applies, so a live edit reaches Git
  before the folder is applied again. See [bi-directional GitOps](bi-directional.md).

## One installation, many teams

One GitOps Reverser installation serves every team. The platform keeps the objects that hold
credentials in its own namespace, and lends them to teams through **grants**:

- A `GitProvider` holds the repository credential, and grants which namespaces may hold which
  branches.
- A `GitBranch` holds one branch, and grants which namespaces may write through it.
- A `KubeProvider` connects a cluster, and grants which namespaces may mirror which source
  namespaces.

Each team then creates its own `GitTarget`, which names its folder, and `WatchRule`s, which select
what to mirror, in its own namespace.

![A platform namespace holds the shared GitProvider with its credentials Secret, a KubeProvider and
a GitReceiver; each team namespace has its own WatchRule and GitTarget writing its own branch of that
repository, and a push event is routed by repository and branch to the GitTarget that writes
it](images/config-platform-shared.excalidraw.svg)

A team cannot reach a credential, branch, or cluster that the platform has not granted it, and it
decides everything else about its own folder. [The grants](configuration.md#the-grants) explains
every shape.

## Adopt it in three stages

Each stage stands on its own, so you can stop after any of them.

**1. Mirror.** Start with folders that nothing applies: an audit trail of a namespace, or the first
commit of an existing cluster's state. Nothing about your current GitOps setup changes.

- [Quick start](quickstart.md): install, and see a first commit.
- Connect your Git host: [GitHub](github-setup-guide.md) or
  [Azure DevOps](azure-devops-getting-started.md).
- [Start mirroring a team's resources](team-onboarding.md): the grants, the branch, the team's
  folder, and how to adopt an existing repository on a scratch branch first.

**2. Coordinate.** Let GitOps Reverser write a folder that Flux or Argo CD applies, so a change can
come from either side.

- [Bi-directional GitOps](bi-directional.md), then the [Flux](bi-directional-flux.md) or
  [Argo CD](bi-directional-argocd.md) setup.
- [React promptly to Git pushes](push-webhook.md), so a push to the repository is applied within
  seconds.

**3. Explain.** Name who made each change, and why.

- [Attribute commits to Kubernetes users](attribution-setup-guide.md), through the API server's
  audit settings.
- [Group changes into one commit](demo/commit-window.md) with a `CommitRequest`.

Alongside the stages: [encrypt Secrets with SOPS](sops-age-guide.md),
[sign commits](commit-signing.md), [narrow the controller's permissions](rbac.md), and
[monitor it](interpreting-metrics.md).

## Before you choose this

- **Git history is a record of the selected state, not of every event.** Commit windows group a
  burst of edits into one commit, and a restart resyncs instead of replaying every change.
- **Author attribution needs the API server's audit settings.** Managed control planes that hide
  them cannot name authors; commits then carry the configured committer.
- **Mirror what people author, and leave out what controllers generate.** A `Deployment` belongs
  in Git; its `ReplicaSet`s and `Pod`s do not. See
  [installing apps as KRM](installing-apps-as-krm.md).
- **Some layouts are refused.** GitOps Reverser edits plain manifests and a supported Kustomize
  subset. It refuses a folder of Helm templates before writing anything.

If your team offers an API to users or customers rather than running clusters for other teams,
[offer your users an API, with every change in Git](for-application-teams.md) is the better
starting point.
