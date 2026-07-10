[![CI](https://github.com/ConfigButler/gitops-reverser/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/ConfigButler/gitops-reverser/actions/workflows/ci.yml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/ConfigButler/gitops-reverser/badge)](https://scorecard.dev/viewer/?uri=github.com/ConfigButler/gitops-reverser)
[![OpenSSF Best Practices](https://www.bestpractices.dev/projects/13468/badge)](https://www.bestpractices.dev/projects/13468)
[![Release](https://img.shields.io/github/v/release/ConfigButler/gitops-reverser?sort=semver)](https://github.com/ConfigButler/gitops-reverser/releases)
[![License](https://img.shields.io/github/license/ConfigButler/gitops-reverser)](https://www.apache.org/licenses/LICENSE-2.0)
[![Go](https://img.shields.io/badge/go-1.26-blue?logo=go)](go.mod)
[![Platforms](https://img.shields.io/badge/platforms-amd64%20%7C%20arm64-2ea44f?logo=docker)](#)
[![codecov](https://codecov.io/gh/ConfigButler/gitops-reverser/graph/badge.svg)](https://codecov.io/gh/ConfigButler/gitops-reverser)
[![Container](https://img.shields.io/badge/container-ghcr.io%2Fconfigbutler%2Fgitops--reverser-2ea44f?logo=docker)](https://github.com/ConfigButler/gitops-reverser/pkgs/container/gitops-reverser)
[![Open Issues](https://img.shields.io/github/issues/ConfigButler/gitops-reverser)](https://github.com/ConfigButler/gitops-reverser/issues)

# GitOps Reverser

GitOps Reverser is a Kubernetes operator that turns live Kubernetes API activity into clean,
versioned YAML in Git: an audit trail, reviewable history, and a GitOps-reconcilable repo, without
giving up API-first workflows. The broader pattern is described at
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

## When it fits

| Good fit | Poor fit |
|---|---|
| Clusters where you can grant watch/RBAC and write to Git (optionally run Valkey/Redis for the full feature set) | Production HA requirements today |
| Teams that want API-to-Git capture first, then named author attribution later | Shared paths with two always-on writers fighting over the same resources |
| API-first or hybrid teams that still want Git history; brownfield discovery, hotfix capture, migration toward GitOps | Workflows that need a guaranteed per-mutation change log rather than a state mirror |

## How it works

1. **Watches** the Kubernetes API for the resource types each `GitTarget` claims. Watch is the
   single source of object state.
2. **Sanitizes** each change (status, `managedFields`, and runtime noise removed) and diffs it
   against the current Git content.
3. **Writes** stable YAML to Git with useful commit metadata, authored according to the mode below.

It can also:

- Encrypt `Secret` values before commit with **SOPS + age** (Secret-shaped custom resources can opt in).
- SSH-sign every commit via `GitProvider.spec.commit.signing`.
- Group changes within a time window into a single commit.
- Take your own commit message and "why" from a `CommitRequest`.

### Operating modes

Every install shares one base: Kubernetes watch/RBAC access, Git credentials, and cert-manager. The
only thing that varies is **who shows up as the commit author**.

| Mode | Git author | Git committer | Also needs |
|---|---|---|---|
| **`configured-author`** *(default)* | the configured identity | the configured identity | None |
| **`attributed-author`** | the authenticated Kubernetes actor | the configured identity | audit delivery + Valkey/Redis |

Every commit carries both a Git *author* and a Git *committer*. In `configured-author` mode they are
the same configured identity (say, `ConfigButler Bot`). Turn on `attribution.enabled` and only the
**author** changes; it becomes whoever actually made the change, while the committer stays put:

| Who made the change (Kubernetes actor) | Git author | Git committer |
|---|---|---|
| Human user (`simon@example.com`) | Simon | `ConfigButler Bot` |
| Service account (`system:serviceaccount:team-a:deployer`) | the `team-a/deployer` service account | `ConfigButler Bot` |
| CI / GitHub App identity | that CI / App identity | `ConfigButler Bot` |

The committer column never moves. That is the point. On a strong audit match the actor becomes the
author; with no match, the commit still lands, authored by the committer. `attributed-author` needs
kube-apiserver audit delivery (which managed control planes like EKS/GKE/AKS generally do not expose)
plus Valkey/Redis. See the [attribution setup guide](docs/attribution-setup-guide.md).

**Valkey/Redis is optional but advised.** The default runs without it in `configured-author` mode.
Add a reachable Valkey/Redis to unlock warm-restart cursors (watches resume instead of cold-replaying),
CommitRequest author capture (the admission webhook is installed by default but only records authors
once Redis is present, a form of author attribution), and attributed-author mode. `configured-author`
needs none of it.

Because object state comes from **watch**, GitOps Reverser is a *state mirror*: it reflects current
object state, not every intermediate mutation, and no delete is silently lost (one missed while no
watch was running is reconciled on reconnect). See [`docs/architecture.md`](docs/architecture.md) for
replay and `410 Gone` details.

## Boundaries

GitOps Reverser reconstructs clean Kubernetes manifests from live cluster state. It does **not**
reconstruct higher-level authoring intent that is no longer in the cluster: it writes back stable
Kubernetes YAML, but it cannot reverse Helm-rendered resources into a clean `values.yaml`, and it
generally cannot infer the original structure of arbitrary templates or overlays.

That boundary is intentional. The goal is deployable cluster intent in Git, not magical recovery of
every upstream abstraction.

## Status

Early-stage software; CRDs and behavior may still change.

- Single controller pod (`replicas=1`); HA is not supported yet.
- Shared-resource bi-directional workflows need explicit coordination.
- Source recovery is limited to Kubernetes manifests, not Helm/Kustomize authoring models.
- Tested against Kubernetes `1.36`; other versions may work but are not in the matrix.
- Runtime behavior is deterministic: no AI or heuristic mutation at runtime.

Good fit for pilots, lab clusters, brownfield discovery, and design partners who can tolerate change.
Production use should follow an environment-specific review. Deferred directions live in
[docs/TODO.md](docs/TODO.md) and [docs/future/](docs/future/).

## Quick start

This brings up the **demo**: a starter `GitProvider`, `GitTarget`, and `WatchRule` in a
`gitops-reverser-quickstart-demo` namespace. It watches ConfigMaps in that namespace and writes them
to `<your-repo>/live-cluster` on the `main` branch. It runs in **`configured-author` mode** (one
committer identity, no Redis) by default.

![Config basics diagram showing the relationship between GitProvider, GitTarget, and WatchRule](docs/images/config-basics.excalidraw.svg)

**Prerequisites:** a Kubernetes cluster with `kubectl`, Helm 3, and cert-manager for TLS.

**1. Install cert-manager**

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.20.3/cert-manager.yaml
kubectl wait --for=condition=ready pod -l app.kubernetes.io/instance=cert-manager -n cert-manager --timeout=300s
```

**2. Create a Git repo and a deploy key**

Create an (empty) repository the operator will write to, then generate a key. Add the public half as a
**deploy key with write access**; the private half becomes the Secret in the next step:

```bash
ssh-keygen -t ed25519 -C "gitops-reverser@cluster" -f /tmp/gitops-reverser-key -N ""
# Add /tmp/gitops-reverser-key.pub to your Git provider as a deploy key (write access)
```

**3. Create the demo namespace and Git credentials**

Do this **before** installing. The starter `GitProvider` is only re-checked about every 5 minutes, so
if the Secret is missing at install time your first commit can be minutes late; having it ready up
front lets the starter resources go Ready on the first reconcile:

```bash
kubectl create namespace gitops-reverser-quickstart-demo \
  --dry-run=client -o yaml | kubectl apply -f -

kubectl create secret generic git-creds \
  --namespace gitops-reverser-quickstart-demo \
  --from-file=ssh-privatekey=/tmp/gitops-reverser-key \
  --from-literal=known_hosts="$(ssh-keyscan github.com 2>/dev/null)" \
  --dry-run=client -o yaml | kubectl apply -f -
```

SSH host-key verification fails closed, so the `known_hosts` line is required. Existing Flux or Argo CD
credentials Secrets are accepted as-is (they must have **write** access). See
[`docs/configuration.md`](docs/configuration.md) for accepted Secret shapes and
[`docs/github-setup-guide.md`](docs/github-setup-guide.md) for the full GitHub guide and HTTPS/PAT
fallback.

**4. Install GitOps Reverser with the demo**

A single install enables the demo and points the starter `GitProvider` at your repo:

```bash
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --create-namespace \
  --set quickstart.enabled=true \
  --set-string quickstart.gitProvider.url=git@github.com:OWNER/REPO.git
```

Replace `OWNER/REPO` with your repository (angle brackets would be parsed by the shell).

This runs without Redis, so warm-restart cursors and author capture (CommitRequest and
attributed-author) stay inactive; the operator is healthy regardless. To enable them, point it at a
reachable Valkey/Redis with `--set queue.redis.addr=HOST:PORT` (add
`--set queue.redis.auth.existingSecret=SECRET` if it requires auth; see the
[chart README](charts/gitops-reverser/README.md)).

The starter `GitTarget` has SOPS encryption enabled, so on first reconcile the controller generates a
`sops-age-key` Secret in the demo namespace and annotates it with a backup reminder. That is expected;
it only matters once you mirror `Secret` resources (the demo watches ConfigMaps). Back up that key if
you keep the demo around.

Check the starter resources become ready:

```bash
kubectl get gitprovider,gittarget,watchrule -n gitops-reverser-quickstart-demo
```

**5. Test it**

```bash
kubectl create configmap test-config --from-literal=key=value -n gitops-reverser-quickstart-demo
```

A new commit should land in your repository within seconds. If none appears:

```bash
kubectl logs -n gitops-reverser deploy/gitops-reverser
kubectl describe gitprovider,gittarget,watchrule -n gitops-reverser-quickstart-demo
```

To tear the demo down: `helm uninstall gitops-reverser -n gitops-reverser` and
`kubectl delete namespace gitops-reverser-quickstart-demo`.

### Want named users on your commits?

Every Git commit has both an author and a committer. Turn on **`attributed-author` mode** and the
real Kubernetes actor (user, service account, or CI identity) becomes the Git author, while the
configured identity stays the committer. It needs kube-apiserver audit delivery and Valkey/Redis; the
[attribution setup guide](docs/attribution-setup-guide.md) walks through the setup.

### Rather have it managed?

ConfigButler can run a small, secure, public-facing Kubernetes API for you: we operate GitOps
Reverser and authorize your end users, with forward-deployed engineers to get you started. You keep a
clean, self-owned Git repo where your users express their intent.

## Docs

Start with the stable docs surface:

- [`docs/README.md`](docs/README.md)
- [`docs/configuration.md`](docs/configuration.md)
- [`docs/attribution-setup-guide.md`](docs/attribution-setup-guide.md)
- [`docs/security-model.md`](docs/security-model.md)
- [`docs/rbac.md`](docs/rbac.md): the two ClusterRoles, and how to stop the reverser enumerating Secrets
- [`docs/commit-signing.md`](docs/commit-signing.md)
- [`docs/github-setup-guide.md`](docs/github-setup-guide.md)
- [`docs/sops-age-guide.md`](docs/sops-age-guide.md)
- [`docs/bi-directional.md`](docs/bi-directional.md)
- [`docs/alternatives.md`](docs/alternatives.md): nearby tools and when another approach fits better

## Looking for early users

If this workflow matches a real problem, feedback is very welcome. The most useful reports are
install attempts, first-commit experience, audit delivery issues, Git output shape, CRD ergonomics,
and security or operational concerns.

## Get in touch

- Read the [Reverse GitOps manifesto](https://reversegitops.dev/) for the broader pattern.
- Connect on [LinkedIn](https://www.linkedin.com/in/simonkoudijs/). Feedback, questions, and ideas
  are welcome.

## Contributing

Issues, docs fixes, and code contributions are welcome. See [`CONTRIBUTING.md`](CONTRIBUTING.md).

## License

Apache 2.0
