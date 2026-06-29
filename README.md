[![CI](https://github.com/ConfigButler/gitops-reverser/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ConfigButler/gitops-reverser/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/ConfigButler/gitops-reverser?sort=semver)](https://github.com/ConfigButler/gitops-reverser/releases)
[![License](https://img.shields.io/github/license/ConfigButler/gitops-reverser)](https://www.apache.org/licenses/LICENSE-2.0)
[![Go](https://img.shields.io/badge/go-1.26-blue?logo=go)](go.mod)
[![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-2ea44f?logo=docker)](#)
[![Go Report Card](https://goreportcard.com/badge/github.com/ConfigButler/gitops-reverser)](https://goreportcard.com/report/github.com/ConfigButler/gitops-reverser)
[![codecov](https://codecov.io/gh/ConfigButler/gitops-reverser/graph/badge.svg)](https://codecov.io/gh/ConfigButler/gitops-reverser)
[![Container](https://img.shields.io/badge/container-ghcr.io%2Fconfigbutler%2Fgitops--reverser-2ea44f?logo=docker)](https://github.com/ConfigButler/gitops-reverser/pkgs/container/gitops-reverser)
[![Open Issues](https://img.shields.io/github/issues/ConfigButler/gitops-reverser)](https://github.com/ConfigButler/gitops-reverser/issues)

# GitOps Reverser

GitOps Reverser is a Kubernetes operator that turns live Kubernetes API activity into clean,
versioned YAML in Git.

It is for teams that want API-first workflows without giving up an audit trail, reviewable history,
or a repo that can later be reconciled by GitOps tooling.

The broader pattern behind this project is described at
[reversegitops.dev](https://reversegitops.dev).

<div align="center"><img src="docs/demo/demo.gif" alt="Demo: kubectl apply triggers a sanitized Git commit within seconds" width="100%"></div>

Want proof? See this
[example commit](https://github.com/ConfigButler/example-audit/commit/800a51e5a8edcccbc85c94d5fef7ef7cc8381b7b)
in [ConfigButler/example-audit](https://github.com/ConfigButler/example-audit).

## What it does

- Keep using the Kubernetes API as the write path.
- Capture those live changes as stable manifests in Git.
- Keep configuration file-backed, reviewable, and reusable.

![Overview diagram showing how API events flow through the operator into Git](docs/images/overview.excalidraw.svg)

## Audience

This is advanced operator software to install, even if the downstream workflow is meant to become
simpler for other teams.

## When it fits

| Good fit | Poor fit |
|---|---|
| Clusters where you can grant watch/RBAC, run Valkey/Redis in-cluster, and write to Git | Production HA requirements today |
| Teams that want to try API-to-Git capture first, then add named author attribution later | Shared paths with two always-on writers fighting over the same resources |
| API-first or hybrid teams that still want Git history; brownfield discovery, hotfix capture, migration toward GitOps | Workflows that need a guaranteed per-mutation change log rather than a state mirror |

> **Author attribution** is the only optional capability: it needs kube-apiserver audit delivery, which
> managed control planes (EKS/GKE/AKS) generally do not expose. Without it the operator still mirrors
> state, with commits authored by the configured committer. **Valkey/Redis is required either way** — it
> holds each GitTarget's watch resume state so work is re-picked up after a restart or reconnect.
>
> If exact per-user authorship matters but you do not want to own kube-apiserver audit delivery yourself,
> I welcome conversations about a managed ConfigButler path.

## How it works

1. GitOps Reverser **watches** the Kubernetes API for the resource types each `GitTarget` claims —
   watch is the single source of object state.
2. Each change is sanitized (status, `managedFields`, and runtime noise removed) and diffed against the
   current Git content.
3. **Valkey/Redis** tracks each GitTarget's watch resume position, so the operator re-picks up exactly
   where it left off after a restart or reconnect (and, when configured, holds audit attribution facts).
4. The operator writes stable YAML to Git with useful commit metadata. Commits are authored by the
   configured committer, or by the actual user / service account when an audit fact matches
   (**attribution** — the one part that is optional).

`Secret` resources can be encrypted before commit with SOPS + age, Secret-shaped custom resource
types can opt into the same path at controller startup, and Git commits can be SSH-signed
through `GitProvider.spec.commit.signing`.

Capturing objects served by an **aggregated API server** is supported through the same watch path
(with a LIST fallback for servers that do not implement streaming lists); see
[`docs/architecture.md`](docs/architecture.md).

### Operating modes

Every install needs the same base: Kubernetes watch/RBAC access, **Valkey/Redis** (watch resume state),
Git credentials, and cert-manager. The only thing that varies is **author attribution**:

| Mode | Attribution | Additionally needs | Commit author |
|---|---|---|---|
| **Committer-only** (default) | off | — | configured committer identity |
| **Attributed** | on | kube-apiserver audit delivery | named user / service account on a strong match, committer otherwise |

Because object state comes from **watch**, GitOps Reverser is a *state mirror with opportunistic
per-mutation history*: it records every change it observes while watching, and collapses intermediate
versions to current state across restarts, reconnects, or `410 Gone` replays. It is not a guaranteed
per-mutation change log. No path silently loses a delete — a delete missed while no watch was running is
reconciled by the replay mark-and-sweep on reconnect.

## Boundaries

GitOps Reverser reconstructs clean Kubernetes manifests from live cluster state. It does not
reconstruct higher-level authoring intent that is no longer present in the cluster.

That means it can write back stable Kubernetes YAML, but it cannot reverse Helm-rendered resources
back into a clean `values.yaml`, and it generally cannot infer the original authoring structure of
arbitrary templates or overlays.

That boundary is intentional. The goal is deployable cluster intent in Git, not magical recovery of
every upstream abstraction.

## Status

Early-stage software. CRDs and behavior may still change.

- Single controller pod only (`replicas=1`); HA is not supported yet.
- Shared-resource bi-directional workflows require explicit coordination.
- Reverse-GitOps source recovery is limited to Kubernetes manifests, not Helm/Kustomize authoring models.
- Tests run against Kubernetes `1.36`. Other versions may work but are not part of the current matrix.
- Runtime behavior is deterministic; there is no AI or heuristic mutation at runtime.

GitOps Reverser is a good fit for pilots, lab clusters, brownfield discovery, and design-partner
users who can tolerate API and behavior changes. Production use should happen only after an
environment-specific review.

Directions we may revisit later live in [docs/TODO.md](docs/TODO.md) and [docs/future/](docs/future/).

## Quick start

This quick start sets up **committer-only mode**: the operator mirrors watched Kubernetes state into Git,
and commits are authored by the configured committer identity. That is the easiest way to prove the
workflow works.

Valkey/Redis is still required: it holds each GitTarget's watch resume state so the operator re-picks up
where it left off after a restart or reconnect. Named commit authors can be added later with
kube-apiserver audit delivery.

**Prerequisites**

- Kubernetes cluster with `kubectl` configured
- cert-manager for TLS certificate management

Managed platforms such as EKS, GKE, and AKS generally work for this committer-only flow because it does
not require control-plane audit webhook configuration.

**1. Install cert-manager**

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.19.1/cert-manager.yaml
kubectl wait --for=condition=ready pod -l app.kubernetes.io/instance=cert-manager -n cert-manager --timeout=300s
```

**2. Install Valkey with auth** *(required — both modes)*

```bash
kubectl create namespace gitops-reverser
kubectl create secret generic valkey-auth \
  --namespace gitops-reverser \
  --from-literal=password="$(openssl rand -base64 32)"

helm repo add valkey https://valkey.io/valkey-helm/ && helm repo update
helm install valkey valkey/valkey --version 0.9.3 --namespace gitops-reverser \
  --set auth.enabled=true \
  --set auth.usersExistingSecret=valkey-auth \
  --set auth.aclUsers.default.passwordKey=password \
  --set "auth.aclUsers.default.permissions=~* &* +@all"
```

**3. Install GitOps Reverser**

```bash
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --create-namespace
```

**4. Create Git credentials**

SSH deploy key example:

```bash
ssh-keygen -t ed25519 -C "gitops-reverser@cluster" -f /tmp/gitops-reverser-key -N ""
# Add /tmp/gitops-reverser-key.pub to your Git provider as a deploy key

kubectl create secret generic git-creds \
  --from-file=ssh-privatekey=/tmp/gitops-reverser-key \
  --from-literal=known_hosts="$(ssh-keyscan github.com 2>/dev/null)" \
  --dry-run=client -o yaml | kubectl apply -f -
```

SSH host-key verification fails closed, so the `known_hosts` line is required. Existing Flux or Argo
CD Git credentials Secrets are accepted as-is (they just need **write** access). See
[`docs/configuration.md`](docs/configuration.md) for accepted Secret shapes and
[`docs/github-setup-guide.md`](docs/github-setup-guide.md) for the GitHub path and HTTPS/PAT fallback.

**5. Enable the starter configuration**

![Config basics diagram showing the relationship between GitProvider, GitTarget, and WatchRule](docs/images/config-basics.excalidraw.svg)

Use the chart's `quickstart` values so Helm creates a starter `GitProvider`, `GitTarget`, and
`WatchRule`:

```bash
helm upgrade gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --reuse-values \
  --set quickstart.enabled=true \
  --set quickstart.gitProvider.url=git@github.com:<org>/<repo>.git
```

The default quickstart namespace is `default`, so the `git-creds` Secret above should exist there
unless you explicitly set `quickstart.namespace` to something else.

The starter `GitTarget` writes under `live-cluster` by default. That keeps the first run away from
the repository root. To deliberately target the root instead, add
`--set quickstart.gitTarget.path=.` to the Helm command.

Check that the starter resources become ready:

```bash
kubectl get gitprovider,gittarget,watchrule -n default
```

See [`docs/configuration.md`](docs/configuration.md) for how `GitProvider`, `GitTarget`, and
`WatchRule` fit together after the starter install.

**6. Test it**

```bash
kubectl create configmap test-config --from-literal=key=value -n default
```

You should see a new commit land in your Git repository within seconds.

If no commit appears, start with:

```bash
kubectl logs -n gitops-reverser deploy/gitops-reverser
kubectl describe gitprovider,gittarget,watchrule -n default
```

### Add named authors later

The quick start deliberately avoids kube-apiserver audit delivery. To make commits use the actual
Kubernetes user or service account when a strong audit match exists, enable attribution and then follow
the Helm notes:

```bash
helm upgrade gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --reuse-values \
  --set attribution.enabled=true

helm get notes gitops-reverser -n gitops-reverser
```

Those notes include the audit webhook URL, the client certificate Secret names, and the kubeconfig shape
that kube-apiserver needs. For networking and TLS tradeoffs around audit delivery, see
[`docs/design/audit-webhook-api-server-connectivity.md`](docs/design/audit-webhook-api-server-connectivity.md).

If you need exact edit authorship but do not want to operate this audit path yourself, I welcome
conversations about a managed ConfigButler solution.

## Docs

Start here for the stable docs surface:

- [`docs/README.md`](docs/README.md)
- [`docs/configuration.md`](docs/configuration.md)
- [`docs/security-model.md`](docs/security-model.md)
- [`docs/commit-signing.md`](docs/commit-signing.md)
- [`docs/github-setup-guide.md`](docs/github-setup-guide.md)
- [`docs/sops-age-guide.md`](docs/sops-age-guide.md)
- [`docs/bi-directional.md`](docs/bi-directional.md)
- [`docs/alternatives.md`](docs/alternatives.md)

If you are evaluating alternatives or deciding when another approach is a better fit, start with
[`docs/alternatives.md`](docs/alternatives.md).

## Looking for early users

If this workflow matches a real problem, feedback is very welcome. The most useful reports are install
attempts, first-commit experience, audit delivery issues, Git output shape, CRD ergonomics, and security
or operational concerns.

## Get in touch

- Read the [Reverse GitOps manifesto](https://reversegitops.dev/) for the broader pattern behind
  this work.
- Connect on [LinkedIn](https://www.linkedin.com/in/simonkoudijs/). Feedback, questions, and ideas
  are welcome.

## Contributing

Issues, docs fixes, and code contributions are welcome. See [`CONTRIBUTING.md`](CONTRIBUTING.md).

## License

Apache 2.0
