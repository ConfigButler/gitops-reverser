# Documentation

GitOps Reverser turns changes made through the Kubernetes API into commits in a Git repository.
These pages are organized by what you want to do. If you are new, read the
[product overview](../README.md) first, then follow **Get started** from the top.

Not sure this is the right tool? [Alternatives](alternatives.md) compares nearby tools and says when
another approach fits better.

## 1. Get started

Pick a setup path with one question: **does Flux or Argo CD apply the same folder GitOps Reverser
writes?** If yes, follow [coordinate changes from Kubernetes and Git](#coordinate-changes-from-kubernetes-and-git),
even when you only want to record cluster changes for now. An applier that keeps deploying that
folder needs coordination either way. If no, follow [mirror cluster state into Git](#mirror-cluster-state-into-git).

### Mirror cluster state into Git

The shortest path to a commit in Git, with no applier and no Git push webhook. It mirrors the
selected cluster state; batching and restarts mean Git history is not a complete record of every
event.

1. **Install GitOps Reverser and see a first commit:** [quick start](quickstart.md).
2. **Connect your Git host:** [GitHub](github-setup-guide.md) or
   [Azure DevOps](azure-devops-getting-started.md).
3. **Start mirroring a team's resources:** [onboard a team](team-onboarding.md): the repository,
   branch, folder, namespaces, and resources it mirrors, and how to adopt an existing repository
   on a scratch branch first.
4. **Optional: record who made each change:** [attribute commits to Kubernetes users](attribution-setup-guide.md)
   through API-server audit delivery. It needs control over the API server's audit settings;
   [the k3s example](audit-setup/cluster/readme.md) shows a complete configuration.

### Coordinate changes from Kubernetes and Git

For a folder that Flux or Argo CD also applies. Start with the path above, then:

1. **Understand why coordination is needed:** [bi-directional GitOps](bi-directional.md).
2. **Configure the applier:** [Flux](bi-directional-flux.md) or [Argo CD](bi-directional-argocd.md).
3. **React promptly to Git pushes:** [point the Git host's push webhook at GitOps Reverser](push-webhook.md),
   then push a test change and watch it apply.
4. **Optional: record who made each change**, as in the path above.

### Supporting guides

Use these where your setup needs them:

- [Encrypt Secrets with SOPS and age](sops-age-guide.md)
- [Sign commits](commit-signing.md)
- [Narrow the controller's permissions](rbac.md)
- [Mirror kcp workspaces](kcp-workspaces.md), including commit authors from kcp's shared audit stream
- [Helm chart values](../charts/gitops-reverser/README.md)

## 2. Make changes

- **Make a change through Kubernetes:** create, update, or delete a selected resource and follow it
  into Git. The [quick start's test step](quickstart.md#4-test-it) is the smallest example.
- **Group changes and explain why:** [the commit window demo](demo/commit-window.md) collects two
  edits and saves them as one commit with a `CommitRequest`. [Commit messages](commit-messages.md)
  covers the templates.
- **Choose what to mirror:** [installing apps as KRM](installing-apps-as-krm.md) shows that a Flux
  `HelmRelease`, an Argo CD `Application`, or a KRO instance mirrors and edits like any other
  resource.
- **Work alongside other people on the same branch:** [API-first publication](api-first-publication.md)
  explains how edits from several authors and targets become commits, and what happens when another
  writer moves the branch.
- **Make a change through Git:** with an applier configured, a push reaches the cluster as described
  in [the life of an apply](bi-directional.md#the-life-of-an-apply).

## 3. Operate

- **Monitor health and resource usage:** [interpreting metrics](interpreting-metrics.md), including
  [suggested alerts](interpreting-metrics.md#suggested-alerts).
- **Investigate a missing or unexpected change:** start from the resource's conditions
  (`kubectl describe`), then the [metrics for the watch-to-push pipeline](interpreting-metrics.md#pipeline-watch-to-push)
  and the [operational boundaries](architecture.md#operational-boundaries).
- **Upgrade an installation:** [UPGRADING.md](UPGRADING.md) lists breaking changes and migration
  steps, newest first.

## 4. Understand and reference

- [Architecture](architecture.md): how a Kubernetes edit reaches Git, how a Git edit reaches the
  cluster, and the boundaries around safety and recovery.
- [Configuration](configuration.md): every object, field, default, and controller setting.
- [Security model](security-model.md): credentials, permissions, trust boundaries, and the Git
  credentials Secret shape.
- [RBAC](rbac.md): the two ClusterRoles, the optional applier grants, and how to stop the controller
  listing Secrets.
- [Definitions](definitions.md): the vocabulary these pages use.
- [Helm chart](../charts/gitops-reverser/README.md): installation options and values.

## Contributing

- [CONTRIBUTING.md](../CONTRIBUTING.md): contributor workflow and validation commands.
- [Style guide](style-guide.md): how these docs are written, including the no-em-dash rule and the
  American-English decision.
- [e2e debugging](../test/e2e/E2E_DEBUGGING.md): e2e troubleshooting, reuse, and `.stamps`.

## Maintainer notes

**Start at [`INDEX.md`](INDEX.md).** It names the ~35 documents that bind, out of the
117 here. Everything else is a user guide (above) or history.

The maintainer folders are organized by **lifecycle**, not by topic. Pick a folder by asking
"what state is this work in?", never "what is this about?":

| Folder | Means | Binds? |
|---|---|---|
| [`spec/`](spec/) | **This is true now, and the code depends on it.** Most are cited by path from Go source. Change the behavior, change the doc. | **yes** |
| [`design/`](design/) | **We are still deciding.** Open questions and unbuilt work. | yes, it is the roadmap |
| [`facts/`](facts/) | Durable reference: how Kubernetes behaves, and what we learned about it. | yes, as reference |
| [`finished/`](finished/) | **This happened.** Shipped plans and closed investigations. | **no** |
| [`future/`](future/) | Deferred ideas we still want. | as intent |
| [`ci/`](ci/) | CI/devcontainer rationale and troubleshooting. | as reference |
| [`audit-setup/`](audit-setup/) | Cluster-specific audit delivery notes. | as reference |

The [deferred three-way comparison investigation](future/git-api-three-way-comparison.md) records
merge semantics, options for concurrent Git and API edits, and why development remains API-first.
The [Flux and Argo CD source review](facts/gitops-apply-and-field-ignore.md) explains how apply,
field ignores, and both reconciliation directions affect that design.

The one rule that keeps this working: **most documents in `spec/` are cited by path from the Go
source.** If you move or rename one, fix the citation in the same commit. Not doing that is what
made the previous tree unreadable: 17 citations were pointing at files that no longer existed.

[`TODO.md`](TODO.md) is a scratch list, not a plan.
