[![CI](https://github.com/ConfigButler/gitops-reverser/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ConfigButler/gitops-reverser/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/ConfigButler/gitops-reverser?sort=semver)](https://github.com/ConfigButler/gitops-reverser/releases)
[![License](https://img.shields.io/github/license/ConfigButler/gitops-reverser)](https://www.apache.org/licenses/LICENSE-2.0)
[![Go](https://img.shields.io/badge/go-1.25-blue?logo=go)](go.mod)
[![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-2ea44f?logo=docker)](#)
[![Go Report Card](https://goreportcard.com/badge/github.com/ConfigButler/gitops-reverser)](https://goreportcard.com/report/github.com/ConfigButler/gitops-reverser)
[![Container](https://img.shields.io/badge/container-ghcr.io%2Fconfigbutler%2Fgitops--reverser-2ea44f?logo=docker)](https://github.com/ConfigButler/gitops-reverser/pkgs/container/gitops-reverser)
[![Open Issues](https://img.shields.io/github/issues/ConfigButler/gitops-reverser)](https://github.com/ConfigButler/gitops-reverser/issues)

# GitOps Reverser

GitOps Reverser is a Kubernetes operator that turns live Kubernetes API activity into clean, versioned YAML in Git.
It gives API-first teams a Git audit trail and a deployable repo without forcing every change through Git first.

> [!IMPORTANT]
> Speaking at KubeCon: the Reverse GitOps pattern and this project will be presented on
> [March 23, 2026 at 15:55 in Amsterdam](https://sched.co/2DY82).

<div align="center"><img src="docs/demo/demo.gif" alt="GitOps Reverser Demo" width="100%"></div>

Want proof? See this [example commit](https://github.com/ConfigButler/example-audit/commit/800a51e5a8edcccbc85c94d5fef7ef7cc8381b7b)
in [ConfigButler/example-audit](https://github.com/ConfigButler/example-audit).

The broader pattern behind this project is described at [reversegitops.dev](https://reversegitops.dev).
This repo contains a concrete operator that is implementing this.

## Kubecon Adam -> I would love your feedback!

I am at Kubecon Amsterdam 2026:

* Feel free to [book a meeting](https://calendar.app.google/FgYaoZog1dFpRG9Z7) to share your thoughts.
* Take a look at the [manifesto](https://reversegitops.dev/) ⭐ or the [concrete open source implemention](https://github.com/ConfigButler/gitops-reverser) ⭐.
* Let me know what you think, I'm on [LinkedIn](https://www.linkedin.com/in/simonkoudijs/).

## Why

Today teams often choose between workflows:

- Pure GitOps: safe and auditable, but less friendly for users and tools that work through the Kubernetes API.
- API-first: fast and interactive, but it usually does not come with strong review, rollout safety, or change history out of the box.

GitOps Reverser bridges that gap: write to the Kubernetes API, and let the operator write the result to Git.

![](docs/images/overview.excalidraw.svg)

## How it works

- Capture Kubernetes API activity.
- Sanitize objects into stable YAML (intent only).
- Queue and batch writes safely.
- Commit and push the result to Git with useful metadata (username and e-mail that made the change).

## Status

🚨 Early stage software. CRDs and behavior may change, and it is not recommended for production yet.

- Single pod only today (`replicas=1`); multi-pod/HA is not (yet!) supported yet.
- Runtime behavior is deterministic. The operator does not use AI or heuristics at runtime.
- Current roadmap includes signed commits, HA support, and more edge-case hardening.

## Recommended usage

| Mode | Good fit | Notes |
|---|---|---|
| Audit only | Capture live changes into Git | Safest starting point |
| Human in the loop | Hotfix in cluster, review later | Good migration path |
| Split ownership | API-first app resources, Git-first infra | Avoids shared write ownership |
| Controlled bi-directional | Shared paths that truly need both sides | Requires explicit coordination |

For shared resources, the recommended model is:

- write through the Kubernetes API
- let GitOps Reverser publish to Git
- let GitOps controllers acknowledge those commits in a controlled way

Do not let GitOps Reverser and Flux or Argo CD continuously reconcile the same resources in two always-on
loops. That creates stale replays, extra commits, and possible loops.

For Flux on shared paths, the current direction is manual coordination:

- keep the source object
- suspend the `Kustomization` for the shared path
- refresh and reconcile on demand
- wait until Flux reports the expected revision

See [`docs/bi-directional.md`](docs/bi-directional.md) for the user guide and
[`docs/design/bi-directional-flux-manual-handshake-plan.md`](docs/design/bi-directional-flux-manual-handshake-plan.md)
for the implementation direction.

## Quick start

Prerequisites:

- A Kubernetes cluster with `kubectl` configured
- `cert-manager` for webhook certificates
- A Git repository with write access

Install `cert-manager` if needed:

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.19.1/cert-manager.yaml
```

**1. Install GitOps Reverser**

```bash
kubectl apply -f https://github.com/ConfigButler/gitops-reverser/releases/latest/download/install.yaml
```

**2. Create Git credentials**

Generate an SSH key and add the public key to your Git provider:

```bash
ssh-keygen -t ed25519 -C "gitops-reverser@cluster" -f /tmp/gitops-reverser-key -N ""
# Add /tmp/gitops-reverser-key.pub to your Git provider as a deploy key
```

Create a Secret with the private key:

```bash
kubectl create secret generic git-creds \
  --from-file=ssh-privatekey=/tmp/gitops-reverser-key \
  --from-literal=known_hosts="$(ssh-keyscan github.com)" \
  --dry-run=client -o yaml | kubectl apply -f -
```

See [`docs/GITHUB_SETUP_GUIDE.md`](docs/GITHUB_SETUP_GUIDE.md) for a full setup guide.

**3. Create a `GitProvider`, `GitTarget`, and `WatchRule`**

![](docs/images/config-basics.excalidraw.svg)

```bash
# NOTE: Edit the line with YOUR_USERNAME to match your repository
cat <<EOF | kubectl apply -f -
apiVersion: configbutler.ai/v1alpha1
kind: GitProvider
metadata:
  name: your-repo
spec:
  url: "git@github.com:YOUR_USERNAME/my-k8s-audit.git"
  allowedBranches: ["*"]
  secretRef:
    name: git-creds
  push:
    interval: "5s"
    maxCommits: 10
---
apiVersion: configbutler.ai/v1alpha1
kind: GitTarget
metadata:
  name: to-folder-live-cluster
spec:
  providerRef:
    name: your-repo
  branch: test-gitops-reverser
  path: live-cluster
  encryption:
    provider: sops
    age:
      enabled: true
      recipients:
        extractFromSecret: true
        generateWhenMissing: true
    secretRef:
      name: sops-age-key
---
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: only-configmaps
spec:
  targetRef:
    name: to-folder-live-cluster
  rules:
    - operations: [CREATE, UPDATE, DELETE]
      apiGroups: [""]
      apiVersions: ["v1"]
      resources: [configmaps]
EOF
```

Check connectivity:

```bash
kubectl get gitprovider your-repo
```

If `age.recipients.generateWhenMissing: true` is enabled, GitOps Reverser can create the age key Secret
automatically. Back up the generated `*.agekey` immediately. If you lose that private key, existing encrypted
`*.sops.yaml` files are unrecoverable. After backup, remove the warning annotation:

```bash
kubectl annotate secret sops-age-key -n default configbutler.ai/backup-warning-
```

**4. Test it**

```bash
kubectl create configmap test-config --from-literal=key=value -n default
```

You should now see a new commit in your Git repository.

For cluster-wide resources or watching multiple namespaces, use
[`ClusterWatchRule`](config/samples/clusterwatchrule.yaml). More examples live in [`config/samples/`](config/samples/).

## Secrets and encryption

`Secret` resources go through the same pipeline, but sensitive values are encrypted before
commit. Secret manifests are written as `*.sops.yaml`.

See [`docs/SOPS_AGE_GUIDE.md`](docs/SOPS_AGE_GUIDE.md) for working with Secrets, SOPS, age keys, bootstrap, and rotation.

## Known limitations

- GitOps Reverser currently supports only a single controller pod.
- Shared-resource bi-directional sync requires explicit coordination; it is not "set both sides to automatic and walk away."
- Avoid multiple GitProvider configurations pointing at the same repository.

## Other tools to consider

| Tool | What it does | How GitOps Reverser differs |
|---|---|---|
| [RichardoC/kube-audit-rest](https://github.com/RichardoC/kube-audit-rest) | Receives audit events and exposes them over a REST API | GitOps Reverser consumes the change and writes it to Git |
| [robusta-dev/robusta](https://github.com/robusta-dev/robusta) | Broad observability and automation platform | GitOps Reverser is narrower and focused on Git write-back |
| [bpineau/katafygio](https://github.com/bpineau/katafygio) | Periodically snapshots cluster resources into Git | GitOps Reverser is event-driven and commit-oriented |

## Development

### DevContainer

This project includes a DevContainer for consistent development environments.

[![Open in GitHub Codespaces](https://github.com/codespaces/badge.svg)](https://codespaces.new/ConfigButler/gitops-reverser)

- Linux/macOS: works with Docker Desktop or Docker Engine.
- Windows: see [`docs/ci/WINDOWS_DEVCONTAINER_SETUP.md`](docs/ci/WINDOWS_DEVCONTAINER_SETUP.md).

### Running tests

```bash
make test
make test-e2e
make lint
```

## Contributing

Contributions, issues, and discussions are welcome.

## License

Apache 2.0
