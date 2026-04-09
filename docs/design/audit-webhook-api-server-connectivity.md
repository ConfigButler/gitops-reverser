# Audit Webhook API Server Connectivity

GitOps Reverser relies on the Kubernetes [audit webhook backend](https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/#webhook-backend): the kube-apiserver POSTs audit events to an HTTPS endpoint using a kubeconfig file written on the control-plane node(s). This works best on clusters you fully control — k3s, k3d, Talos, Kamaji. Managed platforms (EKS, GKE, AKS) restrict access to apiserver configuration; running on them requires switching to a self-managed control plane or a platform that does expose it.

## The problem

This creates a connectivity challenge that is easy to underestimate:

- The audit webhook kubeconfig must be a static file on disk, present before the apiserver starts.
- The endpoint must be reachable from the control-plane node, not just from within the cluster.
- TLS is required for any real deployment; the apiserver must trust the server certificate.

The apiserver will start fine even if the endpoint is unreachable — audit event delivery is non-fatal. Events will be dropped or retried depending on the configured batch mode, but the cluster comes up regardless.

This is not a simple configuration step. It is a network and TLS design decision. The right answer depends on how your cluster is hosted.

## DNS

The kube-apiserver resolves names using the host's DNS, not CoreDNS. This means in-cluster service names (`*.svc.cluster.local`) do not resolve — those only exist inside CoreDNS, which the host node does not use. However, any hostname resolvable from the control-plane node (internal DNS, a LoadBalancer hostname, etc.) works fine. If you have a stable hostname reachable from the node, DNS is a valid approach.

This is why the current e2e setup uses a node-local `127.0.0.1:<nodePort>` address instead.

## Why TLS is non-trivial

The kube-apiserver validates the TLS certificate of the audit webhook backend. Options are:

1. Provide a trusted CA bundle in the webhook kubeconfig via `certificate-authority-data`.
2. Skip verification with `insecure-skip-tls-verify: true` (only acceptable for local development).

The complication: cert-manager, which issues the controller's TLS certificate, is installed *after* the apiserver starts. You cannot reference a cert-manager-issued certificate in the audit webhook kubeconfig at cluster bootstrap time — at least not without a two-phase setup.

## Audit webhook backend best practices

The connectivity model is only half of the design. The other half is how kube-apiserver should behave when the audit
backend is slow, unavailable, or receives unusually large events.

For the full flag reference, see the Kubernetes [`kube-apiserver` command reference](https://kubernetes.io/docs/reference/command-line-tools-reference/kube-apiserver/).

The safest baseline for GitOps Reverser is:

- use `--audit-webhook-mode=batch`
- keep batching throttling enabled
- enable webhook truncation
- keep the default initial backoff unless you have a measured reason to change it
- tune batch latency only after the network path and TLS path are already stable

### Strong recommendation: do not use `blocking` or `blocking-strict`

Kubernetes documents three webhook modes:

- `batch`: buffer events and send them asynchronously
- `blocking`: block API server responses while each audit event is sent
- `blocking-strict`: same as `blocking`, but a failure at `RequestReceived` can fail the API request itself

For GitOps Reverser, `blocking` and especially `blocking-strict` are poor defaults:

- they couple normal API request latency directly to the health of the audit receiver
- they turn a slow or unavailable audit sink into a control-plane performance problem
- `blocking-strict` can reject otherwise valid user requests during audit backend failures

So the practical rule is simple: **never start with `blocking` or `blocking-strict` for this integration**. If someone
ever wants to use them, that should be a conscious, high-trust, explicitly tested choice rather than a default.

### Recommended starting flags

For self-hosted clusters, the most sensible starting point is:

```text
--audit-webhook-mode=batch
--audit-webhook-batch-throttle-enable=true
--audit-webhook-truncate-enabled=true
```

Then leave these at Kubernetes defaults unless you have measurements showing a real need:

- `--audit-webhook-initial-backoff=10s`
- `--audit-webhook-batch-buffer-size=10000`
- `--audit-webhook-batch-max-size=400`
- `--audit-webhook-batch-throttle-qps=10`
- `--audit-webhook-batch-throttle-burst=15`

Those defaults are designed to keep the audit path asynchronous and bounded without making the apiserver overly eager
to retry or overrun the backend.

### What to tune first, and what not to tune first

If operators want faster event delivery, the first knob to consider is `--audit-webhook-batch-max-wait`.

- lower `batch-max-wait` if event freshness matters more than batching efficiency
- leave buffer size, throttle QPS, and burst alone at first
- only increase buffer or throttle after observing real dropped events or backlog pressure

This ordering matters because `batch-max-wait` changes freshness without immediately increasing failure blast radius,
while aggressive throttle or buffer changes can hide a backend sizing problem rather than solving it.

### Truncation should be enabled in real deployments

Kubernetes leaves webhook truncation disabled by default. That is not a great production default for this use case.

Large audit events do happen, especially when:

- request bodies are large
- patch payloads are verbose
- request/response objects include a lot of data

Enabling truncation is safer than letting oversized events destabilize delivery behavior. The tradeoff is that very
large events may lose request/response bodies before export, but that is still usually better than failed delivery or
excessive pressure on the backend.

### The e2e values are intentionally not production advice

Our e2e cluster uses a much more aggressive batch profile so tests observe audit events quickly:

- low `--audit-webhook-batch-max-wait`
- small `--audit-webhook-batch-max-size`

That is a test feedback optimization, not a universal best practice. Production operators should start from `batch`
mode plus truncation, validate the end-to-end path, and then tune for freshness only if needed.

## Options

### Option 1: Pinned ClusterIP

Pin the Kubernetes Service ClusterIP to a known value via `spec.clusterIP`. Write the webhook kubeconfig with that IP address before the cluster starts.

```yaml
# Service with fixed ClusterIP
spec:
  clusterIP: 10.43.200.200

# webhook kubeconfig
server: https://10.43.200.200:9444/audit-webhook
```

**TLS**: Use `insecure-skip-tls-verify: true` for development. For production, pre-generate a certificate with a SAN for this IP and include its CA in `certificate-authority-data`.

**When to use**: Local/dev clusters (k3s, k3d) where you control the CIDR and can reliably pin an IP. Not suitable for clusters where the service CIDR is unpredictable.

**Tradeoffs**:
- Simple and works without extra infrastructure.
- The fixed ClusterIP will conflict if something else claims it.
- `insecure-skip-tls-verify` is not acceptable for production.

---

### Option 2: Node-local endpoint (hostNetwork / hostPort)

Run the audit consumer with `hostNetwork: true` or a `hostPort`, so the apiserver can reach it at a loopback or node IP address.

```yaml
server: https://127.0.0.1:9444/audit-webhook
```

**TLS**: A certificate with `localhost` or the node IP as a SAN is straightforward to provision.

**When to use**: Clusters where you control scheduling on control-plane nodes, and you can tolerate a hostPort or hostNetwork binding.

**Tradeoffs**:
- Avoids the DNS and ClusterIP pinning problems entirely.
- Requires the pod to be scheduled on the control-plane node (toleration + nodeSelector or DaemonSet on control-plane nodes).
- hostNetwork gives the pod broad network access — consider security implications.

---

### Option 3: External / stable URL

Route audit events to a stable external URL: a LoadBalancer service, a NodePort, or a tunnel (e.g. Cloudflare Tunnel, ngrok).

```yaml
server: https://audit.example.com/audit-webhook
```

**TLS**: Standard — use a publicly trusted certificate or any cert your apiserver trusts.

**When to use**: When you want the cleanest separation, or when operating on a managed cluster where you cannot configure the service CIDR or pin ClusterIPs.

**Tradeoffs**:
- Most robust and portable.
- Requires external DNS, a stable IP or tunnel, and certificate management outside the cluster.
- Adds a dependency on external infrastructure.

---

### Option 4: Bootstrapped CA with pre-provisioned cert

Pre-generate a CA and issue a certificate for the service IP or hostname before the cluster starts. Write the CA into the webhook kubeconfig as `certificate-authority-data`. Install the same CA and cert into the cluster as a Secret during bootstrap (before cert-manager runs).

**When to use**: Production k3s/Talos/Kamaji clusters where you want proper TLS without an external endpoint.

**Tradeoffs**:
- Proper TLS without `insecure-skip-tls-verify`.
- Requires a small bootstrap script to generate and distribute the CA and cert.
- Certificate rotation requires updating the kubeconfig on control-plane nodes and restarting the apiserver — operationally heavier than cert-manager rotation.

---

### Option 5: NodePort reachable from the node itself

A NodePort service binds to all node interfaces (`0.0.0.0`). Because kube-apiserver runs on the same node, it can reach the NodePort at `127.0.0.1:<nodeport>` — no ClusterIP pinning, no hostNetwork needed.

```yaml
# Service
spec:
  type: NodePort
  ports:
  - port: 9444
    nodePort: 30444

# webhook kubeconfig
server: https://127.0.0.1:30444/audit-webhook
```

**TLS**: Issue a cert with `localhost` or the node IP as a SAN. Works naturally with cert-manager if you provision the cert before writing the kubeconfig (two-phase setup), or use a pre-generated cert.

**When to use**: Single-node or multi-node clusters where you want to avoid IP pinning and hostNetwork, and the apiserver runs directly on a node (i.e. not a managed control plane).

**Tradeoffs**:
- Clean — uses a standard Kubernetes service primitive.
- NodePort range is typically 30000–32767, which is a slightly unusual port for a webhook.
- On multi-control-plane clusters the NodePort is reachable from every node, so you need to ensure all control-plane nodes can reach it.

---

### Option 6: Static Pod on the control-plane node

Run the audit receiver as a [static Pod](https://kubernetes.io/docs/tasks/configure-pod-container/static-pod/) — a manifest placed in `/etc/kubernetes/manifests/` (or the kubelet's configured staticPodPath). Kubelet starts static pods independently of the API server, before the rest of the cluster is up. With `hostNetwork: true` the pod binds directly to the node's network stack at `127.0.0.1`.

```yaml
server: https://127.0.0.1:9444/audit-webhook
```

**TLS**: The pod can mount a cert from a host path, provisioned during cluster bootstrap alongside the other control-plane certs.

**When to use**: Clusters where you want the audit receiver to be co-located with the control plane as a first-class component — similar to how etcd or the scheduler itself runs. Fits naturally on bare-metal k8s, Talos, or any setup where you already manage control-plane static pods.

**Tradeoffs**:
- Very tight coupling between the audit receiver and the control-plane node — both by design.
- Starts before the API server; no dependency on cluster scheduling.
- Updating the pod requires writing a new manifest to the node, not a `kubectl rollout`.
- Not practical if your control-plane is managed (you cannot write to its manifest directory).

---

### Option 7: Node-local proxy as a systemd unit

Run a minimal reverse proxy on the control-plane node as a systemd service. The proxy forwards traffic from a stable local address to wherever the cluster service happens to be. The audit webhook kubeconfig always points at `127.0.0.1` and never needs to change when the cluster-side service moves.

```bash
# Example using socat (for illustration — use nginx or a purpose-built proxy for production)
socat TCP-LISTEN:9444,fork TCP:10.43.200.200:9444
```

**TLS**: The proxy handles TLS termination with a cert for `localhost`, decoupling the webhook's TLS from the cluster's internal cert infrastructure.

**When to use**: When the cluster-side service IP or port is likely to change, or when you want a clean abstraction between the apiserver config and the cluster topology. Also useful as a temporary bridge while migrating between options.

**Tradeoffs**:
- Adds an extra component to manage on the control-plane node.
- The proxy config must be updated if the backend changes — but the webhook kubeconfig does not.
- `socat` is fine for a quick test; for production use something that handles TLS, connection pooling, and health checks.

---

### Option 8: Kamaji tenant control planes

[Kamaji](https://kamaji.clastix.io/) runs tenant control planes as pods inside a parent cluster. The tenant kube-apiserver is itself a workload, and its audit webhook kubeconfig can point directly at any service in the parent cluster using standard in-cluster DNS — because the parent cluster's CoreDNS *is* available to the tenant apiserver pod.

```yaml
server: https://gitops-reverser.gitops-reverser.svc.cluster.local:9444/audit-webhook
```

**TLS**: Normal in-cluster TLS with cert-manager. The parent cluster's service infrastructure handles it.

**When to use**: If you are already running Kamaji for multi-tenancy. The connectivity and TLS problems described in this document largely disappear because the control plane is just another workload.

**Tradeoffs**:
- Only applicable if you are running Kamaji (or a similar hosted-control-plane system like vcluster).
- Adds a significant operational layer if adopted only to solve this problem.
- Arguably the cleanest long-term architecture if you need multiple clusters.

---

## Which option for which environment

| Environment | Recommended option | Notes |
|---|---|---|
| Local dev / devcontainer | Option 1 (pinned ClusterIP) + `insecure-skip-tls-verify` | Acceptable for local use only |
| k3s / k3d on a real server | Option 5 (NodePort) or Option 4 | NodePort avoids IP pinning; Option 4 for proper TLS |
| Single-node bare metal | Option 6 (static pod) | First-class control-plane component, no scheduler dependency |
| Talos | Option 4 or Option 6 | Talos machine config makes bootstrap certs and static pods feasible |
| Kamaji | Option 8 | Tenant control plane is a pod; DNS and TLS just work |
| Self-hosted with ingress | Option 3 | Cleanest for multi-node or HA setups |
| Managed cluster (EKS, GKE, AKS) | Requires self-managed control plane | No access to apiserver audit webhook config on managed platforms |

## What the current e2e setup does

The e2e test cluster uses **Option 5**:

- kube-apiserver connects to `https://127.0.0.1:30444/...`
- the audit Service is exposed through a `NodePort`
- the tracked bootstrap kubeconfig source lives at `test/e2e/cluster/audit/webhook-config.yaml`
- the mounted runtime copy is generated under `.stamps/cluster/<ctx>/audit/webhook-config.yaml`

At cluster bootstrap, that generated file uses `insecure-skip-tls-verify: true` so kube-apiserver can start before
cert-manager issues the real audit TLS materials. After install, the e2e flow rewrites the mounted `.stamps` copy with
the final CA-trusting mTLS kubeconfig and restarts kube-apiserver once.

## Cluster setup reference

For k3s-specific setup steps (config file location, policy file, restart procedure), see [`docs/audit-setup/cluster/readme.md`](../audit-setup/cluster/readme.md).

## Summary

There is no single correct answer for audit webhook connectivity. Before installing GitOps Reverser, decide:

1. **How will the kube-apiserver reach the webhook?** (IP, hostname, or tunnel)
2. **How will TLS be handled?** (skip verify, pinned CA, or public cert)
3. **Is this a dev cluster or a production cluster?**

Option 1 is the path of least resistance for dev clusters and is what the quick start uses. For anything beyond that, plan the network design before you install.
