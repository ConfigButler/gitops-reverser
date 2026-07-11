# Audit Webhook TLS Design

## The two operator questions

This design needs to answer two practical questions before anything else:

1. What has to happen first?
2. How often do we need to touch kube-apiserver after the initial setup?

The short answer is:

- **Yes, audit webhooks always require one control-plane step** because kube-apiserver reads a static
  `--audit-webhook-config-file` from the node filesystem.
- **No, we should not require admins to reconfigure kube-apiserver every time cert-manager rotates a leaf cert.**
  That would be too operationally expensive.
- **The design goal is one-time enablement, then rare planned maintenance only.**

The rest of this document is organized around those answers.

## Why admission and audit are different

GitOps Reverser has two webhook servers that need TLS:

1. **Admission webhook** on port `9443`
2. **Audit webhook** on port `9444`

They look similar, but the trust distribution path is completely different.

### Admission webhook: zero-touch after install

The admission webhook is configured through a `ValidatingWebhookConfiguration`, which is a Kubernetes API object.
That object has a `caBundle` field. cert-manager's CA injector can patch that field automatically when the serving
certificate is issued or renewed.

That is why admission webhook TLS is operationally easy:

- cert-manager issues or renews the serving cert
- cert-manager injects the CA bundle into the webhook object
- kube-apiserver uses the updated `caBundle`
- no node-level file changes are needed

### Audit webhook: node-level bootstrap is unavoidable

The audit webhook is configured with `--audit-webhook-config-file`, which points to a kubeconfig-like file on the
control-plane node filesystem. kube-apiserver reads:

- the webhook URL from `clusters[].cluster.server`
- the server trust bundle from `certificate-authority-data`
- the expected TLS name from `tls-server-name`
- optional client credentials from `users[]`

That file is **not** a Kubernetes API object, so there is no audit equivalent of admission webhook CA injection.

This means there is an unavoidable control-plane bootstrap step:

- generate the audit webhook kubeconfig
- copy it onto every control-plane node
- ensure kube-apiserver is started with `--audit-webhook-config-file=...`

That is the first important ordering rule: **the API server integration point is static, even if the webhook
implementation inside the cluster is dynamic.**

## What must happen first, and why

There are two separate kinds of kube-apiserver work here:

1. **Enable audit webhook delivery at all**
2. **Teach kube-apiserver what server certificate and client credential to trust**

Those have different frequencies.

### One-time enablement

A cluster that does not already use the audit webhook backend needs:

- an audit policy file
- an audit webhook kubeconfig file on the node
- kube-apiserver flags pointing at those files

This is a control-plane configuration task. GitOps Reverser cannot remove that requirement, because Kubernetes exposes
audit webhooks as a kube-apiserver feature, not as an in-cluster API object.

### Post-install trust bootstrap

Once the cluster can send audit events to a webhook, we still need proper TLS. The key point is that we do **not** need
the final TLS assets before kube-apiserver first starts. Audit webhook delivery is non-fatal. The apiserver can start,
the operator can install cert-manager-managed certificates, and then the final kubeconfig can be generated and copied
onto the node.

That leads to the correct ordering:

1. Pick a connectivity model that kube-apiserver can actually reach.
2. Install GitOps Reverser and cert-manager resources.
3. Wait for the audit trust root and certificates to exist.
4. Generate the final audit webhook kubeconfig from those artifacts.
5. Copy that file to the control-plane node(s).
6. Restart or otherwise reload kube-apiserver once for the final configuration.

This order matters because the audit webhook kubeconfig depends on artifacts that do not exist until after the
in-cluster deployment is running.

## Recommended operating model

The design should optimize for this operator experience:

- one initial control-plane enablement step
- one TLS bootstrap step after install
- no kube-apiserver changes for normal serving cert rotation
- rare planned maintenance only when the root CA or the apiserver client credential is intentionally rotated

In other words, **the API server should trust a stable identity, not a rotating leaf certificate.**

## Why the current self-signed leaf design is not enough

Today the audit serving certificate is issued directly from a `selfSigned` issuer. In that setup, the `ca.crt` bundled
in the Secret is effectively the same object as the serving cert.

That creates the bad lifecycle:

1. cert-manager renews the serving cert
2. `ca.crt` changes with it
3. kube-apiserver still trusts the old `certificate-authority-data`
4. audit delivery breaks until the node-side kubeconfig is rewritten
5. kube-apiserver must be restarted or reloaded to pick up the change

That is exactly the admin burden we want to avoid.

This is not an audit-webhook problem by itself. It is a trust-anchor problem. The fix is to separate:

- a long-lived root CA that kube-apiserver trusts
- short-lived serving certs signed by that CA

## The design decision

Use a small CA hierarchy for the audit webhook:

1. A long-lived audit root CA
2. A CA issuer backed by that root
3. A short-lived audit serving cert signed by that CA
4. A distinct apiserver client credential for mTLS

That gives us the lifecycle we actually want:

- serving cert rotation is automatic and invisible to kube-apiserver
- only root CA rotation requires updating `certificate-authority-data`

## Connectivity choice

This document focuses on TLS, but the TLS design still depends on a reachable endpoint.

For the current k3d/k3s e2e path, the design assumes:

- audit Service exposed as `NodePort`
- kube-apiserver connects to `https://127.0.0.1:30444/...`
- kubeconfig sets `tls-server-name` to the Service DNS name

This works in our e2e environment, but should be treated as a **k3d/k3s-specific convenience**, not a universal
Kubernetes guarantee. The broader connectivity tradeoffs stay in
`docs/design/audit-webhook-api-server-connectivity.md`.

The important TLS point is that `tls-server-name` lets us decouple transport address from certificate identity:

- kube-apiserver can connect to `127.0.0.1:30444`
- the certificate can still be issued only for
  `gitops-reverser-audit.gitops-reverser.svc`

That means the serving cert does not need IP SANs for `127.0.0.1`.

## Recommended certificate layout

### 1. Long-lived audit root CA

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: gitops-reverser-audit-root-ca
  namespace: gitops-reverser
spec:
  isCA: true
  commonName: gitops-reverser-audit-root-ca
  secretName: gitops-reverser-audit-root-ca
  duration: 87600h # 10 years
  privateKey:
    algorithm: ECDSA
    size: 256
  issuerRef:
    name: gitops-reverser-selfsigned-issuer
    kind: Issuer
```

### 2. CA issuer backed by that root

```yaml
apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: gitops-reverser-audit-ca-issuer
  namespace: gitops-reverser
spec:
  ca:
    secretName: gitops-reverser-audit-root-ca
```

### 3. Short-lived serving cert

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: gitops-reverser-audit-server-cert
  namespace: gitops-reverser
spec:
  secretName: audit-server-cert
  dnsNames:
  - gitops-reverser-audit.gitops-reverser.svc
  - gitops-reverser-audit.gitops-reverser.svc.cluster.local
  usages:
  - digital signature
  - key encipherment
  - server auth
  issuerRef:
    name: gitops-reverser-audit-ca-issuer
    kind: Issuer
  duration: 2160h # 90 days
  renewBefore: 360h # 15 days
  privateKey:
    rotationPolicy: Always
```

### 4. Long-lived apiserver client cert

The audit endpoint should require client authentication, but this certificate has a different operational lifecycle than
the serving cert. Because kube-apiserver is the client and its credentials live on the control-plane node, rotating this
credential is a control-plane maintenance event.

So the client cert should be long-lived as well. It should **not** rotate on the same 90-day cadence as the serving
cert.

```yaml
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: gitops-reverser-audit-client-cert
  namespace: gitops-reverser
spec:
  secretName: audit-client-cert
  commonName: kube-apiserver
  usages:
  - digital signature
  - client auth
  issuerRef:
    name: gitops-reverser-audit-ca-issuer
    kind: Issuer
  duration: 87600h # 10 years
  renewBefore: 720h # 30 days
  privateKey:
    rotationPolicy: Always
```

The intent is to protect sysadmins from repeated manual kube-apiserver audit-webhook kubeconfig updates and control-plane
reconfiguration. The key design point is that **serving cert rotation should be frequent and automatic; apiserver client
credential rotation should be infrequent and explicit.**

If an operator wants a shorter lifetime, that should remain configurable, but it should come with a clear warning:
shorter client-cert rotation increases the frequency of node-side kubeconfig redistribution and kube-apiserver
maintenance.

## The kubeconfig kube-apiserver should trust

With the stable root CA in place, the node-side file should look like this:

```yaml
apiVersion: v1
kind: Config
clusters:
- name: audit-webhook
  cluster:
    server: https://127.0.0.1:30444/audit-webhook/my-cluster
    certificate-authority-data: <base64 of gitops-reverser-audit-root-ca ca.crt>
    tls-server-name: gitops-reverser-audit.gitops-reverser.svc
contexts:
- name: audit-webhook
  context:
    cluster: audit-webhook
    user: apiserver
current-context: audit-webhook
users:
- name: apiserver
  user:
    client-certificate-data: <base64 of audit-client-cert tls.crt>
    client-key-data: <base64 of audit-client-cert tls.key>
```

The important lifecycle property is:

- `certificate-authority-data` comes from the root CA Secret, so it stays stable across serving cert renewal

## How often kube-apiserver must be changed

This is the most important operational section.

| Event | Node-side update needed? | Control-plane maintenance needed? |
|---|---|---|
| Initial audit webhook enablement | Yes | Yes |
| First TLS bootstrap after install | Yes | Yes |
| Normal serving cert rotation | No | No |
| Root CA rotation | Yes | Yes |
| Client certificate rotation | Yes | Yes |

### Always required

These are unavoidable control-plane tasks:

- enabling the audit webhook backend in kube-apiserver
- placing an audit policy file on the node
- placing an audit webhook kubeconfig on the node

### Required once at install time

After GitOps Reverser and cert-manager issue the initial root CA and certificates, the generated kubeconfig must be
copied to the control-plane node(s) and kube-apiserver must pick it up.

For this design, that should happen once after installation.

### Not required for normal serving cert rotation

If kube-apiserver trusts the long-lived root CA rather than the serving leaf cert:

- cert-manager can renew the serving cert every 90 days
- the webhook pod can reload the new cert
- kube-apiserver keeps working without any kubeconfig edit

This is the main design win.

### Required for root CA rotation

If the root CA is intentionally rotated, then the node-side `certificate-authority-data` must be updated on every
control-plane node. This should be rare and planned.

### Required for client certificate rotation

If the client certificate embedded in the kubeconfig is rotated, then the kubeconfig must be regenerated and
redistributed. That is why the client credential needs a much slower rotation cadence than the serving cert.

If we later prove that a specific Kubernetes distribution safely reloads file-based client credentials without a full
apiserver restart, we can optimize this. For now, the conservative design assumption should be:

- changes to the audit webhook kubeconfig or its embedded credentials are control-plane maintenance

## Why mTLS still matters

Without client authentication, anything that can reach the audit endpoint can POST forged audit events.

That risk is lower in local e2e, but it is not acceptable as a production design. So the audit server should require and
verify client certificates against the same root CA:

- server presents a cert signed by `gitops-reverser-audit-root-ca`
- kube-apiserver presents a client cert signed by that same CA
- audit server uses `tls.RequireAndVerifyClientCert`

That keeps the endpoint closed to unauthenticated callers even if the NodePort is reachable more broadly than intended.

## Current e2e state

The current e2e path is intentionally a bridge state, not the end-state design.

Today it does this:

1. Start with `insecure-skip-tls-verify: true`
2. Wait for `audit-server-cert`
3. Extract `ca.crt` from that Secret
4. Rewrite `test/e2e/cluster/audit/webhook-config.yaml`
5. Restart the k3d server node

That proves the transport works, but it still has two production problems:

- trust is tied to the rotating serving cert
- no client authentication is configured

So the e2e mechanism is useful as proof of connectivity, but it should not be the final operator story.

## Proposed operator story

The installation flow should be documented like this:

1. Install GitOps Reverser and cert-manager resources.
2. Wait for `gitops-reverser-audit-root-ca`, `audit-server-cert`, and `audit-client-cert`.
3. Run a helper that prints the final webhook kubeconfig.
4. Copy that kubeconfig to the control-plane node(s).
5. Restart or reload kube-apiserver once.
6. Leave serving cert rotation entirely to cert-manager.

That answers the original ordering question cleanly:

- **what goes first?**
  The root CA must exist before we generate the final kubeconfig, because kube-apiserver needs a stable trust anchor.
- **why?**
  Because the on-disk kubeconfig is where apiserver trust lives, and we want that trust to outlive normal serving cert
  rotation.

## Service split recommendation

The current single `NodePort` Service exposes admission, audit, and metrics together. Only the audit path needs
node-level reachability.

The cleaner topology is:

| Service | Type | Ports | Purpose |
|---|---|---|---|
| `gitops-reverser` | `ClusterIP` | admission `9443`, metrics `8443` | In-cluster only |
| `gitops-reverser-audit` | `NodePort` | audit `9444` -> `30444` | Reachable by kube-apiserver |

This keeps the node-reachable surface area as small as possible.

## Implementation checklist

- [ ] Split the audit Service from the in-cluster admission and metrics Service
- [ ] Add a long-lived audit root CA Certificate
- [ ] Add a CA Issuer backed by that root
- [ ] Issue the audit serving cert from the CA Issuer
- [ ] Issue a distinct audit client cert for kube-apiserver
- [ ] Remove `127.0.0.1` IP SAN dependency and rely on `tls-server-name`
- [ ] Update the audit server to require and verify client certificates
- [ ] Add tooling that prints the final audit webhook kubeconfig after install
- [ ] Update e2e TLS injection to read the stable root CA instead of the serving cert Secret
- [ ] Document that serving cert rotation is automatic, but root CA and client credential rotation are planned
      control-plane maintenance events
