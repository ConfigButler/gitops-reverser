[![CI](https://github.com/ConfigButler/gitops-reverser/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ConfigButler/gitops-reverser/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/ConfigButler/gitops-reverser?sort=semver)](https://github.com/ConfigButler/gitops-reverser/releases)
[![License](https://img.shields.io/github/license/ConfigButler/gitops-reverser)](https://www.apache.org/licenses/LICENSE-2.0)
[![Go](https://img.shields.io/badge/go-1.26-blue?logo=go)](go.mod)
[![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-2ea44f?logo=docker)](#)
[![Go Report Card](https://goreportcard.com/badge/github.com/ConfigButler/gitops-reverser)](https://goreportcard.com/report/github.com/ConfigButler/gitops-reverser)
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
| Self-managed clusters where you can configure kube-apiserver audit delivery | Managed control planes that do not expose audit webhook configuration |
| API-first or hybrid teams that still want Git history | Shared paths with two always-on writers fighting over the same resources |
| Brownfield discovery, hotfix capture, migration toward GitOps | Production HA requirements today |

## How it works

1. kube-apiserver sends audit events to the operator's audit webhook.
2. GitOps Reverser loads the live object and removes runtime-only noise.
3. Events are queued safely through Valkey/Redis.
4. The operator writes stable YAML to Git with useful commit metadata.

`Secret` resources can be encrypted before commit with SOPS + age, and Git commits can be SSH-signed
through `GitProvider.spec.commit.signing`.

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
- Tests run against Kubernetes `1.35`. Other versions may work but are not part of the current matrix.
- Runtime behavior is deterministic; there is no AI or heuristic mutation at runtime.

## Ideas

Interesting, but not promised:

- simpler setup flows, for example automating more Git provider bootstrap steps such as creating
  GitHub SSH keys
- a mode that commits changes without end-user author attribution, using the watch/reconcile path
  without Kubernetes audit integration; simpler to set up, but at the cost of not knowing who made
  the change
- constrained reverse actions for simple, known Kustomize-style mutations
- branching and promotion strategy beyond a single straight write-back flow

## Quick start

This quick start assumes you already know how to operate a Kubernetes control plane and are willing
to change kube-apiserver audit settings.

**Prerequisites**

- Kubernetes cluster with `kubectl` configured
- cert-manager for TLS certificate management
- Admin access to kube-apiserver configuration so you can enable the
  [audit webhook backend](https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/#webhook-backend)

Managed platforms such as EKS, GKE, and AKS generally do not expose that control-plane
configuration.

For networking and TLS tradeoffs around audit delivery, see
[`docs/design/audit-webhook-api-server-connectivity.md`](docs/design/audit-webhook-api-server-connectivity.md).

**1. Install cert-manager**

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.19.1/cert-manager.yaml
kubectl wait --for=condition=ready pod -l app.kubernetes.io/instance=cert-manager -n cert-manager --timeout=300s
```

**2. Install Valkey with auth**

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

**4. Configure kube-apiserver audit delivery**

Read the Helm post-install notes:

```bash
helm get notes gitops-reverser -n gitops-reverser
```

Those notes include the audit webhook URL, the client certificate Secret names, and the kubeconfig
shape that kube-apiserver needs.

**5. Create Git credentials**

SSH deploy key example:

```bash
ssh-keygen -t ed25519 -C "gitops-reverser@cluster" -f /tmp/gitops-reverser-key -N ""
# Add /tmp/gitops-reverser-key.pub to your Git provider as a deploy key

kubectl create secret generic git-creds \
  --from-file=ssh-privatekey=/tmp/gitops-reverser-key \
  --from-literal=known_hosts="$(ssh-keyscan github.com 2>/dev/null)" \
  --dry-run=client -o yaml | kubectl apply -f -
```

See [`docs/github-setup-guide.md`](docs/github-setup-guide.md) for a focused GitHub setup path and
the HTTPS/PAT fallback.

**6. Enable the starter configuration**

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

Check that the starter resources become ready:

```bash
kubectl get gitprovider,gittarget,watchrule -n default
```

See [`docs/configuration.md`](docs/configuration.md) for the starter values and follow-up
customization.

**7. Test it**

```bash
kubectl create configmap test-config --from-literal=key=value -n default
```

You should see a new commit land in your Git repository within seconds.

If no commit appears, start with:

```bash
kubectl logs -n gitops-reverser deploy/gitops-reverser
kubectl describe gitprovider,gittarget,watchrule -n default
```

## Docs

Start here for the stable docs surface:

- [`docs/README.md`](docs/README.md)
- [`docs/configuration.md`](docs/configuration.md)
- [`docs/github-setup-guide.md`](docs/github-setup-guide.md)
- [`docs/sops-age-guide.md`](docs/sops-age-guide.md)
- [`docs/bi-directional.md`](docs/bi-directional.md)
- [`docs/alternatives.md`](docs/alternatives.md)

If you are evaluating alternatives or deciding when another approach is a better fit, start with
[`docs/alternatives.md`](docs/alternatives.md).

## Get in touch

- Read the [Reverse GitOps manifesto](https://reversegitops.dev/) for the broader pattern behind
  this work.
- Connect on [LinkedIn](https://www.linkedin.com/in/simonkoudijs/). Feedback, questions, and ideas
  are welcome.

## Contributing

Issues, docs fixes, and code contributions are welcome. See [`CONTRIBUTING.md`](CONTRIBUTING.md).

## License

Apache 2.0
