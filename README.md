<!-- Badges lead this file on purpose; the H1 follows them. -->
<!-- markdownlint-disable-next-line MD041 -->
[![CI](https://github.com/ConfigButler/gitops-reverser/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ConfigButler/gitops-reverser/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/ConfigButler/gitops-reverser/badge)](https://scorecard.dev/viewer/?uri=github.com/ConfigButler/gitops-reverser)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13468/badge)](https://www.bestpractices.dev/projects/13468)
[![Release](https://img.shields.io/github/v/release/ConfigButler/gitops-reverser?sort=semver)](https://github.com/ConfigButler/gitops-reverser/releases)
[![codecov](https://codecov.io/gh/ConfigButler/gitops-reverser/graph/badge.svg)](https://codecov.io/gh/ConfigButler/gitops-reverser)
[![License](https://img.shields.io/github/license/ConfigButler/gitops-reverser)](https://www.apache.org/licenses/LICENSE-2.0)
![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-2ea44f?logo=docker)
[![Container](https://img.shields.io/badge/container-ghcr.io%2Fconfigbutler%2Fgitops--reverser-2ea44f?logo=docker)](https://github.com/ConfigButler/gitops-reverser/pkgs/container/gitops-reverser)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/gitops-reverser)](https://artifacthub.io/packages/search?repo=gitops-reverser)
[![Open Issues](https://img.shields.io/github/issues/ConfigButler/gitops-reverser)](https://github.com/ConfigButler/gitops-reverser/issues)

# GitOps Reverser

GitOps Reverser watches selected Kubernetes resources and commits a clean YAML representation to Git.
It strips `status`, `managedFields`, and runtime metadata. For supported layouts, it updates existing
manifests in place, preserving comments and document structure.

Use it to capture live changes, bring an existing cluster into Git, or experiment with API-first
workflows alongside Flux or Argo CD.

[Quick start](docs/quickstart.md): install the operator and capture your first ConfigMap in Git.

It is pre-1.0: the CRDs are `v1alpha3` and the configuration surface can still change between
releases. [Before you adopt it](#before-you-adopt-it) covers what to weigh.

<div align="center">
  <img src="docs/demo/demo.gif" width="100%"
       alt="Demo: kubectl apply triggers a sanitized Git commit within seconds">
</div>

Inspect an [example commit](https://github.com/ConfigButler/example-audit/commit/800a51e5a8edcccbc85c94d5fef7ef7cc8381b7b).
Named Kubernetes actors in Git history require optional
[audit attribution](docs/attribution-setup-guide.md); the default uses a configured Git identity.

## How it works

Changes reach the Kubernetes API however your users make them: `kubectl`, a GUI, a CI job, or an
agent over MCP.

![Overview diagram: humans, kubectl, and MCP clients change resources through the Kubernetes API, which GitOps Reverser watches and commits to Git](docs/images/overview.excalidraw.svg)

1. **Watch** the Kubernetes API for the types each `GitTarget` claims, the single source of object
   state. A new target also reconciles all existing resources of those types into Git.
2. **Sanitize** the change and compare it against what Git already holds.
3. **Write** stable YAML to the target folder and push, grouping a burst of changes into one commit.

Custom resources configure it: `GitProvider` for repository and credentials, `GitTarget` for branch
and folder, `WatchRule` or `ClusterWatchRule` for what to capture, and `ClusterProvider` for the
source cluster, created as `default` on install. See [Configuration](docs/configuration.md).

## Principles

- **gitops-reverser creates Git commits out of your Kubernetes resources** Deploying Git back into a cluster is Flux's and
  Argo CD's job. Run one of them alongside if you need the round trip. See
  [bi-directional usage](docs/bi-directional.md).

- **API-first, not API-only.** This operator is designed to reconile API resources into Git. Fast and reliable. The assumption is that gitops-reverser is the most frequent writer. Other actor can push changes to same branch: gitops-reverser can handle a moved branch. The remote is pulled and all unpushed API changes are replayed, until the remote push is succesfull. Any A Git change to a
  files that are not affected by recent API changes are kept. "API-first" decides only the narrow case, where
  both the API and an external Git author changed the same object: in that case the captured API object wins. See
  [API-first publication](docs/api-first-publication.md). for more details.

- **We never commit secret content unencrypted. Ever.** A resource classified as sensitive is
  encrypted before it can reach a commit. If encryption fails, or no encryptor is configured, the
  write is rejected. There is no plaintext fallback.
- **The commit authors field can be trusted** When the facts are weak, late, conflicting or
  missing, the author is set to `unknown (attribution unresolved)` instead of a guess. A person's name in
  the author field means we are sure. See [audit attribution](docs/attribution-setup-guide.md).
- **Fail early** An unsupported layout is refused before anything is
  written, with `Stalled=True` and a reason naming the problem. We will not write the half we
  understood and leave you the rest. See [status conditions](docs/spec/status-conditions-guide.md).

## Features

- **Capture existing resources and future changes.** A new target captures the selected resources
  already in the cluster, then follows changes. With deletion mirroring enabled, removal follows
  the deletion request, even while finalizers keep the object around. Choose which deletions reach
  Git with a [deletion policy](docs/configuration.md#deletion-policy-specprunemode).
- **In-place edits keep the shape of your file.** Updates preserve key order, comments, and
  untouched documents in multi-document files.
- **Duplicate resource identities are rejected.** A folder holding the same resource in two
  documents is refused rather than adopted, so a capture cannot update one copy and leave the other
  stale.
- **Inspect a repository before you point at it.** `manifest-analyzer --mode scan-repo` classifies
  every candidate folder under a repository root, read-only and with no cluster, so you can see what
  a target could adopt before creating one. See [`cmd/manifest-analyzer/`](cmd/manifest-analyzer/).
- **SOPS and age, set up on the fly.** The operator creates the encryption configuration and can
  generate a missing age key in your chosen Kubernetes `Secret`. Private keys stay in the cluster,
  never in Git; back up a generated key before relying on it. See [SOPS and age](docs/sops-age-guide.md).
- **Signed commits.** SSH signing through `GitProvider.spec.commit.signing`, including what it takes
  to earn a verified badge on your Git host. See [commit signing](docs/commit-signing.md).
- **Commit messages you control.** Separate templates for live windows, reconciles, and save
  requests. A bad template holds the target at `Validated=False` instead of surfacing at commit
  time. See [message templates](docs/commit-messages.md).
- **Status you can wait on.** Conditions follow the kstatus convention, so `Ready`, `Reconciling`,
  and `Stalled` mean what `kubectl wait` and GitOps tooling expect, asserted in tests against the
  kstatus library rather than against our own reading of it.
- **Metrics.** A Prometheus surface with copy-pasteable PromQL for the questions operators ask, and
  a named list of what is deliberately not instrumented. See
  [interpreting metrics](docs/interpreting-metrics.md).

## What it can write

Choose the resources to watch and the repository, branch, and folder to write into.

| Your source in Git | What you can do |
|---|---|
| Plain Kubernetes manifests | Capture resources and update their existing YAML documents |
| Supported Kustomize layouts | Update source manifests or image/replica declarations; add and remove resources |
| A Flux `HelmRelease` or Argo CD `Application` | Capture the declaration, including chart versions and inline values |
| Helm templates or standalone `values.yaml` | No writeback from rendered workloads |

Kustomize support includes local bases and overlays, with the base kept read-only when targeting an
overlay. Generators, components, remote bases, and several other transforms are unsupported. See the
[supported subset](docs/configuration.md#kustomize-support-in-the-target-path).

Select the resources that express your intent. For example, watch a `HelmRelease` to capture chart
settings. The operator cannot automatically distinguish authored resources from controller-generated
ones. See [choosing what to capture](docs/installing-apps-as-krm.md#the-design-decision-capture-intent-not-the-rendered-output).

## Quick start

Follow the [installation walkthrough](docs/quickstart.md) to capture ConfigMaps from a demo namespace
into a disposable repository. You need a Kubernetes cluster, `kubectl`, Helm 3, and Git write access.
The walkthrough includes cert-manager setup; Redis and audit delivery are optional.

The demo captures a live change:

```bash
kubectl create configmap test-config --from-literal=key=value -n gitops-reverser-quickstart-demo
```

Inspect the commit under `live-cluster/` in your repository. Then edit the ConfigMap and inspect the
next diff. The walkthrough includes status checks, troubleshooting, and cleanup.

## Batch changes into a commit

Changes for the same target and author share a commit window: each change restarts the timer, and
the commit is made after that much silence. Omitted, the window is `5s`; `0s` opts into a commit per
event. A `CommitRequest` can close an open window early and supply the commit message. This allows the
person making a change to explain **why** the change was necessary.

![Kubernetes resource changes flow through GitOps Reverser's commit window into Git](docs/images/commit-window.excalidraw.svg)

The picture shows the [commit-window example](docs/demo/commit-window.md) running. It carries the
manifests: a `GitTarget` with a `window`, a `WatchRule` selecting the two types, and the save request
that closes the window early. It runs on top of the quickstart above.

## Try it with your existing repo

Start with a [scratch branch](docs/configuration.md#seeing-what-a-target-will-do-before-it-does-it)
containing your existing manifests, with no reconciler deploying that branch. Select a small resource
scope and one destination folder. Inspect the initial commits before making a live edit, then check
which source files changed.

The operator pushes directly to the configured branch. If you later write to a branch that Flux or
Argo CD deploys, read the [bidirectional guide](docs/bi-directional.md) first. Live edits can be
reverted by the reconciler, and [replaying captured state](docs/api-first-publication.md) can overwrite
concurrent Git edits to the same object. Decide per folder which side is authoritative.

## Before you adopt it

The CRDs are `v1alpha3`, so the configuration surface can change between releases;
[`docs/UPGRADING.md`](docs/UPGRADING.md) carries each migration. Keep one `GitProvider` per
repository, so that two of them never write the same paths.

- **Access:** the chart defaults to cluster-wide read access, including Secrets. Review
  [RBAC](docs/rbac.md) to restrict watched types and understand the remaining credential permissions.
- **History:** batching can collapse intermediate edits. Unpublished work is held in memory and can
  be lost on restart; recovery captures current state. Git history is not a complete event log.
- **Deletes:** the default mirrors observed delete events but retains documents absent from a
  reconnect snapshot. Choose a [deletion policy](docs/configuration.md#deletion-policy-specprunemode)
  that fits your repository.
- **Versions:** tested against Kubernetes `1.37` at the API level (envtest) and `1.36` end-to-end
  (k3s, which has no stable `1.37` release yet). Other versions may work but are not in the matrix.
  Running the image needs no Go, but anything importing this repo as a module
  (`pkg/manifestanalyzer`, for example) is bound by the `go` directive in [`go.mod`](go.mod), which
  is the source of truth. That floor can move in any release, including a patch release; when it
  does, the release notes say so.

It runs one replica, with no standby failover: the chart rejects `replicaCount > 1` rather than let
two instances write the same repository. Ownership coordination and a durable worker queue are on
the way to 1.0. The backlog is in [docs/TODO.md](docs/TODO.md), and longer-range directions in
[docs/future/](docs/future/).

## Rather have it managed?

ConfigButler can run a small, secure, public-facing Kubernetes API for you: we operate GitOps
Reverser and authorize your end users, with forward-deployed engineers to get you started. You keep a
clean, self-owned Git repo where your users express their intent.

## Documentation and feedback

- [Configuration](docs/configuration.md): resource selection, Git destinations, and commit behavior
- [Helm chart](charts/gitops-reverser/README.md): installation options and upgrades
- [Documentation index](docs/README.md): setup guides and operational details
- [Reverse GitOps](https://reversegitops.dev/): the manifesto for the broader pattern

Trying it with a real repo? [Open an issue](https://github.com/ConfigButler/gitops-reverser/issues)
with the layout you tried, the diff you expected, and what happened. Install attempts, first-commit
experience, audit delivery, Git output shape, and CRD ergonomics are the most useful reports at this
stage. Contributions are welcome; see [CONTRIBUTING.md](CONTRIBUTING.md).

Or connect on [LinkedIn](https://www.linkedin.com/in/simonkoudijs/): feedback, questions, and ideas
are all welcome.

Licensed under Apache 2.0.
