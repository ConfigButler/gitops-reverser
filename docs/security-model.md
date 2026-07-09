# Security Model

What GitOps Reverser can access, why, and which pieces are sensitive. Read this before installing.

## Why the controller has broad access

GitOps Reverser writes the live state of watched resource types into Git. To do that it must:

- **Read watched resources cluster-wide.** WatchRule and ClusterWatchRule decide which types are
  followed, but the controller needs read (get/list/watch) access to those types to materialize
  them. Broad WatchRules imply broad read access.
- **Read referenced Secrets.** It reads the Git credentials Secret (and, when encryption is
  configured, the SOPS/age key Secret) named by a GitProvider/GitTarget. At **runtime** the
  control-plane code paths read these controller-owned input Secrets **directly by name** (`get`):
  they no longer `list` or `watch` Secrets and never cache Secret values in memory. Out-of-band
  credential or age-key rotations are picked up on the direct read the next time work happens, and
  at the latest by the 5-minute periodic reconcile. **This is a runtime-behavior change, not (yet)
  an RBAC change:** the default install still *grants* `secrets get;list;watch` (and the
  dynamic-watch wildcard), so the ServiceAccount's effective Secret permission is unchanged until
  the separate RBAC-narrowing track lands. Mirrored Secrets selected by a WatchRule are a separate
  concern and still use the watched-resource read path above. See
  [`future/secret-value-retention-plan.md`](future/secret-value-retention-plan.md) and
  [`future/scoped-rbac-least-privilege-plan.md`](future/scoped-rbac-least-privilege-plan.md).
- **Receive audit events.** The kube-apiserver audit webhook posts events to the controller's
  audit ingress. Those events carry object metadata and, for some resources, request/response
  bodies.

The controller does not need write access to watched resources. Its only write target is Git.

## Sensitive trust boundaries

| Boundary | Why it matters |
|---|---|
| Git credentials Secret | Grants push access to your repository. |
| SOPS/age key material | Decrypts (and the public key encrypts) Secret data written to Git. |
| Source-cluster kubeconfig Secret | Grants the operator's read access to a remote cluster. |
| Redis/Valkey queue | Buffers decoded audit events in transit; not an audit archive. |
| Audit ingress (`/audit-webhook`) | Accepts audit traffic; protected by mutual TLS via cert-manager. |
| Generated Secret material | Signing keys and generated age keys live in cluster Secrets. |
| `assert-author` RBAC verb | Lets a holder write any author into a repository's Git history. |

## Keeping Git credentials off the cluster you watch

Historically the operator built one client and used it for both jobs: reading its own CRs and the Git
credentials Secret, and watching the resources it mirrors. Nothing chose that; it fell out of having
one kubeconfig. The consequence is worth stating plainly: because `GitProvider.spec.secretRef` is a
same-namespace reference, the Git write credential — often scoped far more broadly than the one
repository a `GitTarget` names — had to live **on the cluster being watched**, one RBAC rule away
from whoever can read Secrets in that namespace.

`GitTarget.spec.sourceCluster` separates them. The operator reads its own CRs and credentials from the
cluster it runs in (**the config plane**) and watches whichever cluster each `GitTarget` names. The
watched cluster then holds only the watched resources — no Git credential, and not even the
`configbutler.ai` CRDs. See
[`configuration.md`](configuration.md#mirroring-a-remote-cluster-specsourcecluster).

The source-cluster kubeconfig Secret is read on demand from the config plane, parsed into a
`rest.Config`, and its bytes are dropped; only the Secret's `resourceVersion` is retained, so a
rotation rebuilds the clients exactly once. No Secret informer is started.

## Asserting a commit author is a privilege

`CommitRequest.spec.author` lets a trusted client name the human a commit is for, instead of deriving
them from an apiserver audit fact. It is gated by the `assert-author` verb on the named `GitTarget`
(checked by a `SubjectAccessReview` at admission, and re-verified by the controller against the
recorded verdict, so the check does not depend on the webhook's `failurePolicy`).

**Treat `assert-author` exactly like `impersonate`.** The asserted `name` and `email` are free text
and are not verified against any real identity: they are what the trusted control plane says they are.
Granting the verb grants the ability to write any author into that repository's history. Scope grants
with `resourceNames` to the specific `GitTarget`s a caller owns.

The commit's **committer** is always the operator's configured identity, and commit signing (when
configured) signs as the committer — so a reader can always tell a commit was made by the reverser on
someone's behalf, whoever the author header names.

## Secret data the controller writes to Git

Without encryption, a watched `Secret` is committed as-is (its data is plain in the repository). With
SOPS + age (`GitTarget.spec.encryption`), Secret values are encrypted before commit using the age
recipients, and the private key never leaves the cluster. Because the write path only encrypts, it
uses **public age recipients only**: the private age identity is never written to disk or passed to
the `sops` process, even when the recipient is derived from a cluster age-key Secret. So: only watch
`Secret` types you intend to commit, and prefer encryption. Secret-shaped custom resource types can opt into the same
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
