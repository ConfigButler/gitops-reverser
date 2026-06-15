# Security Model

What GitOps Reverser can access, why, and which pieces are sensitive. Read this before installing.

## Why the controller has broad access

GitOps Reverser writes the live state of watched resource types into Git. To do that it must:

- **Read watched resources cluster-wide.** WatchRule and ClusterWatchRule decide which types are
  followed, but the controller needs read (get/list/watch) access to those types to materialize
  them. Broad WatchRules imply broad read access.
- **Read referenced Secrets.** It reads the Git credentials Secret (and, when encryption is
  configured, the SOPS/age key Secret) named by a GitProvider/GitTarget.
- **Receive audit events.** The kube-apiserver audit webhook posts events to the controller's
  audit ingress. Those events carry object metadata and, for some resources, request/response
  bodies.

The controller does not need write access to watched resources. Its only write target is Git.

## Sensitive trust boundaries

| Boundary | Why it matters |
|---|---|
| Git credentials Secret | Grants push access to your repository. |
| SOPS/age key material | Decrypts (and the public key encrypts) Secret data written to Git. |
| Redis/Valkey queue | Buffers decoded audit events in transit; not an audit archive. |
| Audit ingress (`/audit-webhook`) | Accepts audit traffic; protected by mutual TLS via cert-manager. |
| Generated Secret material | Signing keys and generated age keys live in cluster Secrets. |

## Secret data the controller writes to Git

Without encryption, a watched `Secret` is committed as-is (its data is plain in the repository). With
SOPS + age (`GitTarget.spec.encryption`), Secret values are encrypted before commit using the age
recipients, and the private key never leaves the cluster. So: only watch `Secret` types you intend
to commit, and prefer encryption. Secret-shaped custom resource types can opt into the same
encryption path at controller startup. See [`sops-age-guide.md`](sops-age-guide.md).

## Git credentials Secret shape

`GitProvider.spec.secretRef` points to a namespace-local Secret. The controller picks the auth
method from the keys present, preferring an SSH key, then HTTP basic auth, then a bearer token. The
examples below use the Kubernetes-native key names; the reader also accepts the Flux and Argo CD key
names so an existing GitOps Secret works unchanged (see
[`design/git-credentials-interop.md`](design/git-credentials-interop.md)).

**HTTPS (basic auth)**

| Key | Required | Notes |
|---|---|---|
| `username` | yes | Git username. |
| `password` | yes | Token or password. |

**HTTPS (bearer token)**

| Key | Required | Notes |
|---|---|---|
| `bearerToken` | yes | OAuth/PAT bearer token; sent without a username (GitHub fine-grained PAT, GitLab access token). |

**SSH**

| Key | Required | Notes |
|---|---|---|
| `ssh-privatekey` | yes | PEM-encoded private key (also read from Flux `identity` / Argo `sshPrivateKey`). |
| `ssh-password` | no | Passphrase for the private key, if any (also read from `password`). |
| `known_hosts` | conditional | Host key(s) for the Git server. SSH fails closed unless host keys are supplied by some source. |

Host key verification is enforced by default and fails closed. Host keys are resolved in priority
order: the Secret's own `known_hosts`, then `GitProvider.spec.knownHostsRef` (a namespace-local
ConfigMap/Secret), then an install-level default known-hosts ConfigMap in the controller's namespace
(`--default-known-hosts-configmap`). A `known_hosts` that is present but unparseable is always a hard
error. The controller flag `--insecure-allow-missing-known-hosts` disables verification **only** when
no source provided any host keys at all, and is for throwaway/development clusters only.
