# Attribute commits to Kubernetes users

Attribution uses kube-apiserver audit events to name the Kubernetes actor as the Git author.
The configured committer stays unchanged. Without attribution, both identities use the configured
committer.

Start with a working [Git mirror](../README.md#quick-start). You need control over kube-apiserver
flags and files; managed control planes that hide these settings cannot use this webhook.

## 1. Enable the receiver

Add these chart values to your release and reconcile it:

```yaml
replicaCount: 1
attribution:
  enabled: true
  transport: redis
queue:
  redis:
    addr: valkey.example.internal:6379
    # For authenticated Valkey, supply a Secret in the release namespace:
    # auth:
    #   existingSecret: valkey-auth
```

Redis/Valkey holds attribution facts and watch cursors. If its data is lost, recent facts are lost
and watches cold-replay. For a disposable single-pod setup, `attribution.transport: memory` needs
no Redis but loses facts whenever the reverser restarts. It also leaves Redis-backed features such
as `CommitRequest` author capture inactive unless you configure Redis separately.

The chart's default `ClusterProvider` admits targets from every namespace. To restrict it, set
`clusterProvider.default.accessFrom.names` and `selector: null`; Helm otherwise retains the
permissive `selector: {}`. See [RBAC](rbac.md) for the controller's read permissions.

The chart creates the audit Service and cert-manager certificates. Wait for the release to be
ready, then read its Secret names and kubeconfig generator:

```bash
helm get notes gitops-reverser -n gitops-reverser
```

Use the Helm **storage namespace** here. For Flux this may be `flux-system`, even when the pods
and certificate Secrets live in `gitops-reverser`.

## 2. Configure delivery on every API server

Generate the webhook kubeconfig from the audit CA and client certificate Secrets. Keep CA
verification and `tls-server-name`; the audit listener requires mTLS. The kubeconfig embeds a
private key: encrypt it before committing it to Git.

Set its `server` to an address reachable from each control-plane host, with a **named route**:

```yaml
server: https://REACHABLE_ADDRESS:9444/audit-webhook/default
tls-server-name: gitops-reverser-audit.gitops-reverser.svc
```

The route is `ClusterProvider.spec.attribution.auditRoute`, defaulting to the provider's name.
The chart creates a provider named `default`. If Helm notes show a bare `/audit-webhook`, append
`/default` (or your route). The bare endpoint requires `attribution.auditRouteAnnotationKey` and
an annotation on every event; it is not the default endpoint.

A ClusterIP works only when the host can route to the service CIDR. Otherwise use a reachable
NodePort or load balancer. Do not assume cluster DNS or a loopback NodePort works from the host.
Keep `tls-server-name` when connecting by IP so the service certificate's DNS SAN is verified.

Place the kubeconfig and the policy below on every control-plane node, mount them into
kube-apiserver, and set:

```text
--audit-policy-file=/path/to/audit-policy.yaml
--audit-webhook-config-file=/path/to/audit-webhook.kubeconfig
--audit-webhook-mode=batch
--audit-webhook-version=audit.k8s.io/v1
--audit-webhook-batch-max-wait=1s
--audit-webhook-batch-max-size=100
```

The files must exist and be readable by the API server's process user before it starts. Follow
your distribution's file lifecycle: Talos 1.14 writes `machine.files` during boot, so stage the
configuration and reboot each node. A successful live apply does not prove the files exist.

Restart one API server at a time and check its direct `/readyz` endpoint before continuing.
Use `batch`: blocking modes couple API writes to receiver availability. Batching buffers and
retries during short outages, but its finite buffer does not guarantee delivery through an outage.
See [k3s file placement](audit-setup/cluster/readme.md) for an example.

### Choose the audit policy

Start with the [tuned policy](../test/e2e/cluster/audit/policy.yaml). It captures configuration
writes at `RequestResponse` level, including CRDs and custom resources, and drops reads,
heartbeats, core runtime noise, selected status updates, and HPA-driven scale changes.
Keep the write catch-all so newly installed APIs are covered.

For broader noise filtering, add these resources to the first `level: None` rule:

```yaml
resources:
  - group: ""
    resources: ["pods/*", "nodes/*"]
  - group: events.k8s.io
    resources: [events]
  - group: discovery.k8s.io
    resources: [endpointslices]
  - group: "*"
    resources: ["*/status"]
```

Merge these with the existing exclusions. A `pods` entry alone does not exclude `pods/exec` or
other subresources. Keep manual scale writes if you want their authors recorded.

The example includes Secret writes at `RequestResponse` level. To keep Secret bodies out of the
webhook, exclude core `secrets` too. Those writes then have no audit evidence for attribution.
The audit policy controls evidence collection; `WatchRule` and `ClusterWatchRule` control what
gets written to Git.

## 3. Verify a live change

Check receiver readiness and the route:

```bash
kubectl -n gitops-reverser rollout status deployment/gitops-reverser
kubectl get clusterprovider default
```

`FACTS=True` means the provider has received an attribution fact at least once. It is historical
evidence, not a continuous health check. Receiver readiness alone does not prove API server delivery.

Using your normal user credentials, mutate a ConfigMap covered by an existing ready `GitTarget`
and `WatchRule`:

```bash
kubectl -n YOUR_WATCHED_NAMESPACE create configmap attribution-check \
  --from-literal=check=first
kubectl -n YOUR_WATCHED_NAMESPACE patch configmap attribution-check \
  --type=merge -p '{"data":{"check":"second"}}'
```

After the target's commit window closes, fetch its branch and inspect the commits affecting the
ConfigMap. The author should identify your Kubernetes user; the committer should be unchanged:

```bash
git show --format=fuller COMMIT
```

Repeat through each direct API server endpoint, using a unique ConfigMap name per test. Delete
the test objects afterward. A baseline snapshot is not a substitute for this live-write check.

An unmatched live change is authored as
`unknown (attribution unresolved) <attribution-unresolved@gitops-reverser.invalid>`.
Check the route, policy coverage, API server webhook errors, and transport connectivity first.
`attribution.grace` defaults to `3s`; increase it only if facts arrive after that window.
`attribution.ttl` defaults to `10m`.

## Rotate or disable

When the audit client certificate changes, regenerate the embedded kubeconfig and roll it out to
all API servers. A server certificate renewal under the same CA needs no node update. Plan CA
rotation with trust overlap; replacing the CA while nodes still trust the old one breaks delivery.

To disable attribution, remove webhook delivery from every API server first. Then set
`attribution.enabled: false` in the release. This avoids sending events to a removed Service.

For remote clusters and shared routes, see [configuration](configuration.md) and the
[shared audit trust boundary](../SECURITY.md#shared-audit-ingress-trust-model).
