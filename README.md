[![CI](https://github.com/ConfigButler/gitops-reverser/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ConfigButler/gitops-reverser/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/ConfigButler/gitops-reverser?sort=semver)](https://github.com/ConfigButler/gitops-reverser/releases)
[![License](https://img.shields.io/github/license/ConfigButler/gitops-reverser)](https://www.apache.org/licenses/LICENSE-2.0)
[![Go](https://img.shields.io/badge/go-1.25-blue?logo=go)](go.mod)
[![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-2ea44f?logo=docker)](#)
[![Go Report Card](https://goreportcard.com/badge/github.com/ConfigButler/gitops-reverser)](https://goreportcard.com/report/github.com/ConfigButler/gitops-reverser)
[![Container](https://img.shields.io/badge/container-ghcr.io%2Fconfigbutler%2Fgitops--reverser-2ea44f?logo=docker)](https://github.com/ConfigButler/gitops-reverser/pkgs/container/gitops-reverser)
[![Open Issues](https://img.shields.io/github/issues/ConfigButler/gitops-reverser)](https://github.com/ConfigButler/gitops-reverser/issues)

# GitOps Reverser

GitOps Reverser is a Kubernetes operator that turns live API activity into clean, versioned YAML in Git. It results in a folder with YAML files that can be deployed to any cluster. The commit history is your perfect audit trail.

<div align="center"> <img src="docs/demo/demo.gif" alt="GitOps Reverser Demo" width="100%"> </div>
<p>

Want to see the evidence? You can find the [commit](https://github.com/ConfigButler/example-audit/commit/800a51e5a8edcccbc85c94d5fef7ef7cc8381b7b) in [ConfigButler/example-audit](https://github.com/ConfigButler/example-audit).

## Why

Today teams have to choose between workflows:
- Pure GitOps: safe and auditable, but unfriendly to non‑git users.
- API‑first: fast and interactive, but databases don't come (by default) with a way to deploy or test high-risk changesets.

Reverse GitOps gives you both: the interactivity of the Kubernetes API with Git's safety and traceability. Users, CLIs, and automations talk to a lightweight control plane; the operator immediately reflects desired state to Git.

### Converting API actions into Git commits

![](docs/images/overview.excalidraw.svg)

## How it works

- Capture: Admission webhook receives Kubernetes API requests.
- Sanitize: Remove status and server‑managed fields; format as clean YAML.
- Queue: Buffer events to handle spikes reliably.
- Commit: Annotate with user, operation, namespace, timestamp; commit to Git.
- Push: It's now in your git repo.

## Status

🚨 This is early stage software. CRDs and behavior may change; not recommended for production yet. Feedback and contributions are very welcome!

Current limitation: GitOps Reverser must run as a single pod (`replicas=1`). Multi-pod/HA operation is not supported yet.

### Use of AI

I have been thinking about the idea behind GitOps Reverser for several years (I've given up my fulltime job to work on it). Some of the hardest parts, especially writing to Git efficiently and safely under load, were designed and implemented manually. The rest is vibe coded, and needs more refinement before I would run it in production.

**The operator itself is fully deterministic and does not use AI or heuristics at runtime. Given the same inputs, it produces the same Git output.**

Feedback, issues, and pull requests are very welcome!

### Planned Improvements

- Signed git commits
- Full HA support by introducing [Valkey](https://valkey.io/)
- More refined behavior in edge cases (especially with newly added CRDs)
- Migrate to the [Kubernetes audit webhook](https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/#webhook-backend) to replace the watcher/webhook combination; see [`docs/past/audit-webhook-experimental-design.md`](docs/past/audit-webhook-experimental-design.md).

## Quick start

Prerequisites:
- A Kubernetes cluster with kubectl configured
- cert-manager to create the webhook certificates (run `kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.19.1/cert-manager.yaml`)
- A test git repository with write access

**1. Install GitOps Reverser:**
```bash
kubectl apply -f https://github.com/ConfigButler/gitops-reverser/releases/latest/download/install.yaml
```

**2. Set up Git credentials:**

Create an SSH key and add it to your Git provider (GitHub, GitLab, Gitea):
```bash
ssh-keygen -t ed25519 -C "gitops-reverser@cluster" -f /tmp/gitops-reverser-key -N ""
# Add /tmp/gitops-reverser-key.pub to your Git provider as a deploy key
```

Create a Kubernetes Secret with the private key:
```bash
kubectl create secret generic git-creds \
  --from-file=ssh-privatekey=/tmp/gitops-reverser-key \
  --from-literal=known_hosts="$(ssh-keyscan github.com)" \
  --dry-run=client -o yaml | kubectl apply -f -
```

See [`docs/GITHUB_SETUP_GUIDE.md`](docs/GITHUB_SETUP_GUIDE.md) for detailed setup instructions.

**3. Configure what to reconcile:**

Reconciliation sources and targets are configured by three types of custom resources. Create these to start reconciling ConfigMaps:

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
EOF
```

Check the status to see if it's able to connect: `kubectl get gitprovider your-repo`

```bash
cat <<EOF | kubectl apply -f -
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

When `age.recipients.generateWhenMissing: true` is enabled, GitOps Reverser can create the encryption key Secret
automatically.
Back up the generated `*.agekey` entry immediately and securely.
If you lose that private key, existing encrypted `*.sops.yaml` files are unrecoverable.
After confirming backup, remove the warning annotation:
`kubectl annotate secret sops-age-key -n default configbutler.ai/backup-warning-`

**4. Test it:**
```bash
# Create a test ConfigMap
kubectl create configmap test-config --from-literal=key=value -n default

# Check your Git repository - you should see a new commit with the ConfigMap YAML
```

For cluster-wide resources (nodes, CRDs, etc.) or watching multiple namespaces, use
[`ClusterWatchRule`](config/samples/clusterwatchrule.yaml). More examples in [`config/samples/`](config/samples/).

## Usage guidance

Avoid infinite loops: Do not point GitOps (Argo CD/Flux) and GitOps Reverser at the same resources in fully automated mode. Recommended patterns:
- Audit (capture changes, no enforcement)
- Human‑in‑the‑loop (hotfix in cluster, capture to Git, review/merge)
- Drift detection (use commits as alert inputs)
- Hybrid (traditional GitOps for infra; Reverser for app/config changes)

## Known limitations / design choices

- GitOps Reverser currently supports only a single controller pod (no multi-pod/HA yet).
- `Secret` resources (`core/v1`, `secrets`) are written via the same pipeline, but sensitive values are expected to be encrypted before commit.
  - Configure encryption via `--sops-binary-path` and optional `--sops-config-path`.
  - The container image ships with `/usr/local/bin/sops`.
  - Per-path `.sops.yaml` files are bootstrapped in the target repo for SOPS-based secret encryption.
  - If Secret encryption fails, Secret writes are rejected (no plaintext fallback).
  - `GitTarget.spec.encryption.age.recipients.generateWhenMissing: true` can auto-generate a date-based `*.agekey`
    in the referenced encryption Secret when no `*.agekey` entry exists.
    - Generated Secret data contains one `<date>.agekey` (`AGE-SECRET-KEY-...`).
    - Generated Secret annotation `configbutler.ai/age-recipient` stores the public age recipient.
    - Generated Secret annotation `configbutler.ai/backup-warning: REMOVE_AFTER_BACKUP` is set by default.
    - While `configbutler.ai/backup-warning` remains, gitops-reverser logs a recurring high-visibility backup warning during periodic reconciliation.
  - WARNING: backup generated private keys immediately and securely. Losing the key means existing encrypted `*.sops.yaml` files cannot be decrypted.
  - After backup, remove the warning annotation:
    - `kubectl annotate secret <your-encryption-secret> -n <namespace> configbutler.ai/backup-warning-`
- Avoid multiple GitProvider configurations pointing at the same repo to prevent queue collisions.
- Queue collisions are possible when multiple configs target the same repository (so don't do that).


## Other applications to consider

| **Tool** | **How it Works** | **Key Differences** | 
|---|---|---|
| [RichardoC/kube-audit-rest](https://github.com/RichardoC/kube-audit-rest) | An admission webhook that receives audit events and exposes them over a REST API. | **Action vs. Transport:** `kube-audit-rest` is a transport layer. GitOps Reverser is an *action* layer that consumes the event and commits it to Git. | 
| [robusta-dev/robusta](https://github.com/robusta-dev/robusta) | A broad observability and automation platform. | **Focused Tool vs. Broad Platform:** Robusta is a large platform. GitOps Reverser is a small, single-purpose utility focused on simplicity and low overhead. | 
| [bpineau/katafygio](https://github.com/bpineau/katafygio) | Periodically scans the cluster and dumps all resources to a Git repository. | **Event-Driven vs. Snapshot:** Katafygio is a backup tool. GitOps Reverser is event-driven, providing a real-time audit trail. | 

## Development

### DevContainer Setup

This project includes a DevContainer for consistent development environments.

[![Open in GitHub Codespaces](https://github.com/codespaces/badge.svg)](https://codespaces.new/ConfigButler/gitops-reverser)

**Linux/macOS:** Works out of the box with Docker Desktop or Docker Engine.

**Windows:** See [`docs/ci/WINDOWS_DEVCONTAINER_SETUP.md`](docs/ci/WINDOWS_DEVCONTAINER_SETUP.md) for Windows-specific setup instructions. TL;DR: Use WSL2 for the best experience, or the devcontainer will automatically fix workspace permissions on startup.

### Running Tests

```bash
make test        # Unit tests
make test-e2e    # End-to-end tests (requires Docker)
make lint        # Linting
```

## Contributing

Contributions, issues, and discussions are welcome.

## License

Apache 2.0
