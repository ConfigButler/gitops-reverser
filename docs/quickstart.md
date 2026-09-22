# Quick start

Point this at a disposable repository: the demo commits real cluster state, and the fastest way to
throw it away afterwards is to delete the repo. It creates a starter `GitProvider`,
`GitTarget`, and `WatchRule` in the `gitops-reverser-quickstart-demo` namespace, watching ConfigMaps
there and writing them to
`<your-repo>/live-cluster` on `main`. It runs in `configured-author` mode (no Redis) by default. The
chart also renders the cluster-scoped `default` `ClusterProvider` the starter target resolves against.

![Config basics diagram showing the relationship between GitProvider, GitTarget, and WatchRule](images/config-basics.excalidraw.svg)

**Prerequisites:** a Kubernetes cluster with `kubectl`, Helm 3, and cert-manager for TLS.

To write the objects yourself instead of letting the chart generate them, start from
[`config/samples/`](../config/samples/): one manifest per object, consistent enough to apply
together. [`configuration.md`](configuration.md) explains every field.

## 1. Install cert-manager

Skip this step if cert-manager is already installed and healthy.

The controller mounts an admission certificate at startup, so cert-manager must be healthy *before*
you install the chart. (`--set servers.admission.enabled=false` drops the dependency; the
[chart README](../charts/gitops-reverser/README.md) covers bring-your-own certificates.)

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.21.0/cert-manager.yaml
kubectl wait --for=condition=ready pod -l app.kubernetes.io/instance=cert-manager -n cert-manager --timeout=300s
```

## 2. Create a Git repo and a deploy key

Create an (empty) repository the operator will write to, then generate a key. Add the public half as a
**deploy key with write access**; the private half becomes the Secret in the next step:

```bash
ssh-keygen -t ed25519 -C "gitops-reverser@cluster" -f /tmp/gitops-reverser-key -N ""
# Add /tmp/gitops-reverser-key.pub to your Git provider as a deploy key (write access)
```

## 3. Create the demo namespace and Git credentials

Do this **before** installing. The starter `GitProvider` is only re-checked about every 5 minutes, so
if the Secret is missing at install time your first commit can be minutes late; having it ready up
front lets the starter resources go Ready on the first reconcile:

Scan the host keys once, check them, then use that same file, so what you verified is what the
Secret gets:

```bash
kubectl create namespace gitops-reverser-quickstart-demo \
  --dry-run=client -o yaml | kubectl apply -f -

ssh-keyscan github.com > /tmp/gitops-reverser-known_hosts 2>/dev/null
ssh-keygen -lf /tmp/gitops-reverser-known_hosts

kubectl create secret generic git-creds \
  --namespace gitops-reverser-quickstart-demo \
  --from-file=ssh-privatekey=/tmp/gitops-reverser-key \
  --from-file=known_hosts=/tmp/gitops-reverser-known_hosts \
  --dry-run=client -o yaml | kubectl apply -f -
```

SSH host-key verification fails closed, so `known_hosts` is required. `ssh-keyscan` pins whichever
key answers on the network, so compare the fingerprints it printed against
[GitHub's published list](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints)
before you rely on the Secret. They are linked rather than copied in here because GitHub has rotated
a host key before, and a fingerprint pasted into a guide goes stale silently. Existing Flux or Argo CD
credentials Secrets are accepted as-is (they must have **write** access). See
[`configuration.md`](configuration.md) for accepted Secret shapes and
[`github-setup-guide.md`](github-setup-guide.md) for the full GitHub guide and HTTPS/PAT
fallback.

## 4. Install GitOps Reverser with the demo enabled

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

This install has these defaults:

- **No Redis**, so warm-restart cursors and author capture stay inactive. Add one with
  `--set queue.redis.addr=HOST:PORT` (plus `queue.redis.auth.existingSecret` if it needs auth).
- **Cluster-wide read on every watchable type, including Secrets** (`rbac.watchTypes.mode=any`).
  [`rbac.md`](rbac.md) explains how to narrow it.
- **SOPS is on for the starter target**, so a `sops-age-key` Secret is generated in the demo namespace
  with a backup reminder. Only matters once you mirror Secrets; back it up if you keep the demo.

Wait for the controller. On a fresh install this waits on cert-manager issuing the certificate:

```bash
kubectl rollout status deployment/gitops-reverser -n gitops-reverser --timeout=300s
```

Then check the starter resources:

```bash
kubectl get gitprovider,gittarget,watchrule -n gitops-reverser-quickstart-demo
```

The `GitProvider` and `WatchRule` should report `Ready=True`. The `GitTarget`'s aggregate `Ready`
stays `Unknown` until its first source discovery, which is the expected state and not a fault, so
the condition to check on it is `Validated`. It is not a printed column, so ask for it directly:

```bash
kubectl wait --for=condition=Validated gittarget/example-target \
  -n gitops-reverser-quickstart-demo --timeout=60s
```

`condition met` means the configuration is accepted and the target is waiting for work. That is the
point to go and create a ConfigMap.

If it times out instead, the `Reason` column tells you which of the three states you are in:

```bash
kubectl get gittarget example-target -n gitops-reverser-quickstart-demo
kubectl describe gittarget example-target -n gitops-reverser-quickstart-demo
```

A `Reason` that names a missing or unauthorized dependency (`ClusterProviderNotFound`,
`NamespaceNotAuthorized`, `InvalidConfig`) is a misconfiguration to fix. One that names discovery or
streams still settling is initialization, and resolves on its own. `describe` prints every condition
with its message, which is where the specific cause is written.

## 5. Test it

```bash
kubectl create configmap test-config --from-literal=key=value -n gitops-reverser-quickstart-demo
```

A new commit should land in your repository within seconds. If none appears:

```bash
kubectl logs -n gitops-reverser deploy/gitops-reverser
kubectl describe gitprovider,gittarget,watchrule -n gitops-reverser-quickstart-demo
```

Two `GitTarget` conditions stop the data plane and are worth recognizing: `ClusterProviderNotFound`
(the `default` `ClusterProvider` is missing) and `NamespaceNotAuthorized` (its `accessFrom`
selector does not cover the demo namespace).

## Clean up

To tear the demo down: `helm uninstall gitops-reverser -n gitops-reverser` and
`kubectl delete namespace gitops-reverser-quickstart-demo`.

> **Note:** the `default` `ClusterProvider` is cluster-scoped and chart-owned, so uninstalling takes
> it with it, holding *every* other `GitTarget` in the cluster unready. Mind that if you run the demo
> alongside a real deployment.

## Want named users on your commits?

Enable audit attribution to use the Kubernetes actor as the Git author while keeping the
committer unchanged. The [attribution setup guide](attribution-setup-guide.md) covers receiver
configuration, API server delivery, and verification. It requires control over kube-apiserver settings.
