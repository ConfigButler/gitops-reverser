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

GitOps Reverser is a Kubernetes operator that turns Kubernetes API resources into clean YAML in Git.
It is configurable, and can be used as:

* a live audit trail, or
* a "reverse" GitOps-reconcilable repo — without giving up API-first workflows.

The broader pattern is described at [reversegitops.dev](https://reversegitops.dev).

<div align="center"><img src="docs/demo/demo.gif" alt="Demo: kubectl apply triggers a sanitized Git commit within seconds" width="100%"></div>

Want proof? See this
[example commit](https://github.com/ConfigButler/example-audit/commit/800a51e5a8edcccbc85c94d5fef7ef7cc8381b7b)
in [ConfigButler/example-audit](https://github.com/ConfigButler/example-audit).

## What it does

- Reconciles existing Kubernetes API state into Git: the repo reflects the exact current state.
- Captures live changes through watches.
- Includes real actors for every change if you configure [kube-api server audit webhooks](https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/#webhook-backend).

![Overview diagram showing how API events flow through the operator into Git](docs/images/overview.excalidraw.svg)

## What it can't

It edits the **intent layer** — the documents a human authored — never the **expansion layer** a
controller derived from them. So it cannot reverse Helm-rendered resources into a clean `values.yaml`,
and it will not invent structure for templating it does not model. Simple Kustomize layouts *are*
supported ([see below](#simple-kustomize-support)); the full verdict table is in
[`support-contract.md`](docs/design/support-boundary/support-contract.md).

## When it fits

Good fit if you want API-first and Git at the same time.

| Good fit | Poor fit |
|---|---|
| Clusters where you can grant watch/RBAC and write to Git (optionally run Valkey/Redis for the full feature set) | Production HA requirements today |
| Teams that want API-to-Git capture first, then named author attribution later | Shared paths with two always-on writers fighting over the same resources |
| API-first or hybrid teams that still want Git history; brownfield discovery, hotfix capture, migration toward GitOps | Teams who want Git to stay the write path, with humans editing manifests first |

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

### Who authors the commits

Every commit carries a Git *author* and a Git *committer*. By default both are one configured
identity (`configured-author`). Turn on attribution and the **author** becomes the real Kubernetes
actor — user, service account, or CI identity — while the committer never moves. That needs
kube-apiserver audit delivery (managed control planes like EKS/GKE/AKS generally do not expose it)
plus Valkey/Redis: see the [attribution setup guide](docs/attribution-setup-guide.md).

**Valkey/Redis is optional but advised.** Without it the default mode works fine; adding it unlocks
warm-restart cursors, `CommitRequest` author capture, and attribution.

### Delivery guarantees

While the watch is connected the operator sees each individual update and commits it, so Git tracks
changes as they happen. Across a *gap* — pod restart, disconnect, `410 Gone` — it reconciles to
current state instead of replaying versions it never saw, so edits made during the gap collapse into
one commit. Nothing is lost or left stale; deletes are reconciled on reconnect. See
[`docs/architecture.md`](docs/architecture.md) for replay and `410 Gone` details.

## Simple Kustomize support

The write path runs **kustomize itself** (`sigs.k8s.io/kustomize/api`) in memory — no plugins, no
exec, no network, no remote bases — and uses the render to decide where a change belongs and to check
the result before committing. What it can do:

- Edit `resources:` (and `bases:`), `namespace:`, `images:`, and `replicas:` as real declarations.
- Write a change where the value actually lives — the source document, or the governing
  `images:`/`replicas:` entry — rather than mirroring rendered output over your source files.
- Add a new file to the right `resources:` list in the same commit, and remove the entry when that
  file's last document is deleted, so the repo never stops building.
- Support `base/` + `overlays/{env}/`: the base is read-only context, never written through an overlay.
- Create a missing `images:`/`replicas:` entry in an overlay, so one environment changes without
  touching a shared base, and author a `$patch: delete` for an object an overlay inherits.
- Read `patches:` (local strategic-merge files), `commonLabels`, `labels`, and `commonAnnotations` as
  read-only build context.
- Verify every commit by re-rendering before and after, refusing it unless your change lands exactly
  and nothing else moves.

It refuses by name — before writing anything, reported as `Stalled=True` — generators, `components`,
`namePrefix`/`nameSuffix`, `replacements`, `vars`, `helmCharts`, plugins, inline and JSON6902 patches,
remote bases, and any field it does not model. One known gap: a strategic-merge patch that *edits a
field* of a base-owned object is not authored yet. Reasoning in
[`kustomize-support-boundary.md`](docs/design/support-boundary/kustomize-support-boundary.md).

## Status

Early-stage software; CRDs and behavior may still change.

- Runs as a single controller pod (`replicas=1`).
- Shared-resource bi-directional workflows need explicit coordination.
- Source recovery covers Kubernetes manifests and simple Kustomize layouts, not Helm authoring models.
- Tested against Kubernetes `1.36`; other versions may work but are not in the matrix.
- Runtime behavior is deterministic: no AI or heuristic mutation at runtime.

Good fit for pilots, lab clusters, brownfield discovery, and design partners who can tolerate change.
Production use should follow an environment-specific review.

### On the road to 1.0

Roughly in priority order:

- **High availability** — `replicaCount > 1` is rejected today; needs leader/ownership coordination so
  two replicas never write the same `GitTarget`. Redis becomes required rather than advised.
- **A stabilized configuration surface** — all six CRDs are `v1alpha3` with no conversion path yet.
- **More documentation** — day-2 operations, troubleshooting, and worked examples per layout.
- **A durable worker queue** — a crash between advancing the watch cursor and landing the write can
  currently skip work on restart.
- **Write-collision safety** across `GitProvider` objects sharing a repository. Until then, keep one
  `GitProvider` per repository.
- **Better queue and worker observability** — enough metrics to run it without reading logs.
- **More control over output layout**, plus filtering cluster-generated noise out of the Git view.

Backlog in [docs/TODO.md](docs/TODO.md); longer-range directions in [docs/future/](docs/future/).

## Quick start

This brings up the **demo**: a starter `GitProvider`, `GitTarget`, and `WatchRule` in a
`gitops-reverser-quickstart-demo` namespace, watching ConfigMaps there and writing them to
`<your-repo>/live-cluster` on `main`. It runs in `configured-author` mode (no Redis) by default. The
chart also renders the cluster-scoped `default` `ClusterProvider` the starter target resolves against.

![Config basics diagram showing the relationship between GitProvider, GitTarget, and WatchRule](docs/images/config-basics.excalidraw.svg)

**Prerequisites:** a Kubernetes cluster with `kubectl`, Helm 3, and cert-manager for TLS.

**1. Install cert-manager**

The controller mounts an admission certificate at startup, so cert-manager must be healthy *before*
you install the chart. (`--set servers.admission.enabled=false` drops the dependency; the
[chart README](charts/gitops-reverser/README.md) covers bring-your-own certificates.)

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.21.0/cert-manager.yaml
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

**4. Install GitOps Reverser with the demo enabled**

Point the starter `GitProvider` at your repo and install:

```bash
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser \
  --create-namespace \
  --set quickstart.enabled=true \
  --set-string quickstart.gitProvider.url=git@github.com:OWNER/REPO.git
```

Replace `OWNER/REPO` with your repository (angle brackets would be parsed by the shell).

Three things worth knowing about this install:

- **No Redis**, so warm-restart cursors and author capture stay inactive. Add one with
  `--set queue.redis.addr=HOST:PORT` (plus `queue.redis.auth.existingSecret` if it needs auth).
- **Cluster-wide read on every watchable type, including Secrets** (`rbac.watchTypes.mode=any`) — fine
  for a demo, probably not for a real cluster. [`docs/rbac.md`](docs/rbac.md) narrows it.
- **SOPS is on for the starter target**, so a `sops-age-key` Secret is generated in the demo namespace
  with a backup reminder. Only matters once you mirror Secrets; back it up if you keep the demo.

Wait for the controller — on a fresh install this waits on cert-manager issuing the certificate:

```bash
kubectl rollout status deployment/gitops-reverser -n gitops-reverser --timeout=300s
```

Then check the starter resources:

```bash
kubectl get gitprovider,gittarget,watchrule -n gitops-reverser-quickstart-demo
```

The `GitProvider` and `WatchRule` report `Ready=True`; the `GitTarget` reports **`Validated=True`** —
its aggregate `Ready` stays `Unknown` until first source discovery, which is expected, not a failure.

**5. Test it**

```bash
kubectl create configmap test-config --from-literal=key=value -n gitops-reverser-quickstart-demo
```

A new commit should land in your repository within seconds. If none appears:

```bash
kubectl logs -n gitops-reverser deploy/gitops-reverser
kubectl describe gitprovider,gittarget,watchrule -n gitops-reverser-quickstart-demo
```

Two `GitTarget` conditions stop the data plane and are worth recognising: `ClusterProviderNotFound`
(the `default` `ClusterProvider` is missing) and `NamespaceNotAuthorized` (its `allowedNamespaces`
selector does not cover the demo namespace).

To tear the demo down: `helm uninstall gitops-reverser -n gitops-reverser` and
`kubectl delete namespace gitops-reverser-quickstart-demo`.

> **Note:** the `default` `ClusterProvider` is cluster-scoped and chart-owned, so uninstalling takes
> it with it — holding *every* other `GitTarget` in the cluster unready. Mind that if you run the demo
> alongside a real deployment.

### Want named users on your commits?

Turn on `attributed-author` mode so the real Kubernetes actor becomes the Git author. Needs audit
delivery and Valkey/Redis — the [attribution setup guide](docs/attribution-setup-guide.md) walks it
through.

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
