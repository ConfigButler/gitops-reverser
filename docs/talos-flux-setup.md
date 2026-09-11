# Install with Flux on Talos

Flux owns the reverser release; your Talos configuration owns audit delivery. Install the receiver
first, then stage and reboot one control-plane node at a time. This procedure was exercised on
Talos 1.14 with Cilium routing ClusterIPs from the host.

## 1. Reconcile the release

Prerequisites: cert-manager, a reachable Redis/Valkey service, and Flux Helm and source controllers
supporting `OCIRepository` chart references. Add these manifests to a path Flux reconciles:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: gitops-reverser
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: OCIRepository
metadata:
  name: gitops-reverser
  namespace: flux-system
spec:
  interval: 1h
  url: oci://ghcr.io/configbutler/charts/gitops-reverser
  ref:
    tag: "0.44.1" # Pin the release you reviewed; a digest also works.
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: gitops-reverser
  namespace: flux-system
spec:
  interval: 10m
  releaseName: gitops-reverser
  targetNamespace: gitops-reverser
  chartRef:
    kind: OCIRepository
    name: gitops-reverser
  install:
    crds: CreateReplace
  upgrade:
    crds: CreateReplace
  values:
    replicaCount: 1
    attribution:
      enabled: true
      transport: redis
    queue:
      redis:
        addr: valkey.YOUR_NAMESPACE.svc.cluster.local:6379
    auditService:
      type: ClusterIP
      clusterIP: YOUR_RESERVED_SERVICE_IP
    clusterProvider:
      default:
        accessFrom:
          selector: null
          names: [gitops-reverser]
```

Replace the Valkey address and reserve an unused IP in your service CIDR. If hosts cannot route
ClusterIPs, choose NodePort or a load balancer and use that reachable address instead.
Order the Flux Kustomization after the one providing cert-manager. Valkey deployment, storage,
and authentication remain part of your platform configuration.

`selector: null` removes the chart's default `selector: {}`, which admits every namespace.
`names` and `selector` are ORed. List the namespaces where you will create `GitTarget` objects.
The [RBAC guide](rbac.md) covers restricting the controller's default cluster-wide read access.

Wait for the HelmRelease to become ready. Generate the webhook kubeconfig using the release's
Secret names from `helm get notes gitops-reverser -n flux-system`. Set its URL to
`https://YOUR_RESERVED_SERVICE_IP:9444/audit-webhook/default` and retain mTLS verification.
The [attribution guide](attribution-setup-guide.md) covers the route and audit policy.

This installs the controller and default `ClusterProvider`. Add your own
[GitProvider, GitTarget, and WatchRule](configuration.md) to start mirroring.

## 2. Stage the Talos configuration

Add the following to your canonical Talos configuration, preserving existing files, API server
arguments, mounts, and additional configuration documents. Replace the content placeholders with
the complete audit policy and generated webhook kubeconfig before applying.

```yaml
machine:
  files:
    - path: /var/gitops-reverser/audit-policy.yaml
      permissions: 0o444
      op: create
      content: |
        # Insert the complete audit Policy here.
    - path: /var/gitops-reverser/audit-webhook.yaml
      permissions: 0o444
      op: create
      content: |
        # Insert the complete mTLS webhook kubeconfig here.
cluster:
  apiServer:
    extraArgs:
      audit-policy-file: /var/gitops-reverser/audit-policy.yaml
      audit-webhook-config-file: /var/gitops-reverser/audit-webhook.yaml
      audit-webhook-mode: batch
      audit-webhook-version: audit.k8s.io/v1
      audit-webhook-batch-max-wait: 1s
      audit-webhook-batch-max-size: "100"
    extraVolumes:
      - hostPath: /var/gitops-reverser/audit-policy.yaml
        mountPath: /var/gitops-reverser/audit-policy.yaml
        readonly: true
      - hostPath: /var/gitops-reverser/audit-webhook.yaml
        mountPath: /var/gitops-reverser/audit-webhook.yaml
        readonly: true
```

Talos writes `machine.files` during boot. A live apply can restart kube-apiserver before the new
files exist, even when `no-reboot` mode accepts the change. Stage the complete configuration and
reboot so the files exist before kube-apiserver starts. `op: create` also refreshes existing files
on Talos 1.14; `overwrite` requires a file to exist already.

Mode `0444` lets the non-root Talos API server read the kubeconfig. Restrict host access and keep
these files out of application pods. Encrypt the private key in Git; render decrypted configs
locally with mode `0600` and exclude them from version control.

Repeated strategic patches can append duplicate `extraVolumes` entries. Render each mount once
in the final configuration rather than appending the same patch on each run.

## 3. Roll out and verify

For each control-plane node, use its direct IP and its fully rendered configuration:

```bash
# Apply output can contain private keys. Keep the log local and private.
umask 077
talosctl --talosconfig talosconfig -e NODE_IP -n NODE_IP \
  apply-config --mode=staged --file node.yaml > audit-apply.log 2>&1
# Continue only if apply-config succeeded.
talosctl --talosconfig talosconfig -e NODE_IP -n NODE_IP reboot --wait=false
kubectl --server=https://NODE_IP:6443 get --raw=/readyz
```

Wait for the node and its API server to become ready before proceeding to the next node. A VIP
may briefly refuse connections while its owning API server restarts; use another node directly.

Run the [live attribution check](attribution-setup-guide.md#3-verify-a-live-change) through each
API server. Also test a CRD or custom resource if those are part of your mirror. Record both
receiver delivery and Git authorship; they establish different parts of the path.

For certificate rotation, update the encrypted source, render the renewed kubeconfig, and repeat
this serial rollout. Keep the receiver available until every node uses the new credentials.
