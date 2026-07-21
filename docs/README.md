# Documentation

This repository has two kinds of markdown:

- stable user/operator guides
- maintainer notes, design docs, and working plans

If you only want the supported product docs, start with the files below.

## Start here

- [`../README.md`](../README.md): product overview and end-to-end quick start
- [`configuration.md`](configuration.md): core configuration objects and how they fit together
- [`installing-apps-as-krm.md`](installing-apps-as-krm.md): installing an app is adding a KRM
  document — Flux `HelmRelease`, Argo CD `Application`, KRO, and core resources all mirror and edit alike
- [`commit-signing.md`](commit-signing.md): how valid Git signatures map to platform verification
- [`github-setup-guide.md`](github-setup-guide.md): GitHub repository and credential setup
- [`attribution-setup-guide.md`](attribution-setup-guide.md): naming real Kubernetes users as commit
  authors via kube-apiserver audit delivery
- [`sops-age-guide.md`](sops-age-guide.md): Secret encryption with SOPS + age
- [`security-model.md`](security-model.md): controller access, trust boundaries, and the Git
  credentials Secret shape
- [`rbac.md`](rbac.md): the two ClusterRoles, and how to stop the reverser enumerating Secrets
- [`bi-directional.md`](bi-directional.md): safe shared-path and handoff patterns
- [`alternatives.md`](alternatives.md): nearby tools and when another approach fits better
- [`UPGRADING.md`](UPGRADING.md): breaking changes and migration steps, newest first
- [`../CONTRIBUTING.md`](../CONTRIBUTING.md): contributor workflow and validation commands
- [`style-guide.md`](style-guide.md): how docs here are written, including the no-em-dash rule and
  the American-English decision
- [`../test/e2e/E2E_DEBUGGING.md`](../test/e2e/E2E_DEBUGGING.md): e2e troubleshooting, reuse,
  and `.stamps`

## Maintainer notes

**Start at [`INDEX.md`](INDEX.md)** — it names the ~35 documents that actually bind, out of the
117 here. Everything else is a user guide (above) or history.

The maintainer folders are organised by **lifecycle**, not by topic. Pick a folder by asking
"what state is this work in?", never "what is this about?":

| Folder | Means | Binds? |
|---|---|---|
| [`spec/`](spec/) | **This is true now, and the code depends on it.** Most are cited by path from Go source. Change the behavior, change the doc. | **yes** |
| [`design/`](design/) | **We are still deciding.** Open questions and unbuilt work. | yes — it is the roadmap |
| [`facts/`](facts/) | Durable reference: how Kubernetes behaves, and what we learned about it. | yes, as reference |
| [`finished/`](finished/) | **This happened.** Shipped plans and closed investigations. | **no** |
| [`future/`](future/) | Deferred ideas we still want. | as intent |
| [`ci/`](ci/) | CI/devcontainer rationale and troubleshooting. | as reference |
| [`audit-setup/`](audit-setup/) | Cluster-specific audit delivery notes. | as reference |

The one rule that keeps this working: **most documents in `spec/` are cited by path from the Go
source.** If you move or rename one, fix the citation in the same commit. Not doing that is what
made the previous tree unreadable — 17 citations were pointing at files that no longer existed.

[`TODO.md`](TODO.md) is a scratch list, not a plan.
