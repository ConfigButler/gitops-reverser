# Git config interop: rollout plan

Status: **this is the plan we execute in one go.** We are pre-1.0 with no existing users, so there
are no migration shims, compatibility windows, or deprecation steps — we change the schema and the
credentials reader directly and ship them as one change set. The rollout plan section lists the
concrete, ordered changes; the sections before it are the rationale. It supersedes the earlier
"`GitRepository` as a `providerRef`" feasibility note by widening the question from "consume one Flux
object" to "interoperate with the Git credentials a GitOps user already has," whether those come from
Flux **or** Argo CD, without coupling to, advertising, or requiring either.

The framing constraint throughout: **never require Flux or Argo, never advertise a preference, and
never make an Argo CD user read the word "Flux" to use the product.** Internally (this doc) we name
names freely; the user-facing schema and docs stay vendor-neutral.

## The change in one paragraph

We drop the Flux `GitRepository` option from `GitProviderReference` entirely (it is a trap, it is
semantically wrong, and keeping it would couple us *to* Flux). The portable thing across ecosystems
is not the repo object — it is the **credentials Secret** — so we make a `GitProvider`'s referenced
Secret read cleanly whether it was authored in the Kubernetes-native shape, the Flux shape, or the
Argo CD shape. Our own examples stay in the vendor-neutral Kubernetes-native shape; the reader also
accepts the Flux and Argo key names. The reader gains the credential keys it lacks today — the
SSH-key aliases, the passphrase fallback, and the **HTTP bearer token** (`bearerToken`, used by both
Flux and Argo for token auth without a username). HTTP **basic-auth** Secrets and Flux-shaped SSH
Secrets then read directly; Argo-origin SSH Secrets need host-trust data supplied through our neutral
`known_hosts` path because Argo stores host keys outside the repository Secret.

---

## How Flux is set up to track a repo

A Flux user registers each repository as a namespaced **`GitRepository`** custom resource
(source-controller, group `source.toolkit.fluxcd.io`; verified against
`github.com/fluxcd/source-controller/api@v1.8.5`). Its spec is `url` + `ref` (exactly one of
`branch` / `tag` / `semver` / `name` / `commit`) + `secretRef` (a Secret **in the same namespace**
holding the credentials) + `interval` (how often source-controller re-checks) + optional
`verify` / `ignore` / `sparseCheckout` / `proxySecretRef`. source-controller polls on that interval,
clones the repo **read-only**, optionally verifies its signature, and publishes the checked-out tree
as an **Artifact** (a tarball + digest) that other Flux controllers (Kustomization, HelmRelease)
consume and apply. Two properties matter for us: (a) the repo registration is a first-class CRD —
**one object per repository**; (b) credentials live in a **separate, explicitly named Secret** with
no inheritance — every `GitRepository` names its own `secretRef`. Push-based refresh exists via
notification-controller `Receiver`s, but the default is interval polling. Flux never writes through
a `GitRepository`; the one Flux controller that *does* write to Git layers its own spec on top (see
the write-back landscape below).

## How Argo CD is set up to track a repo

Argo CD has **no repository CRD**. A repository is registered as a plain Kubernetes **Secret**
labeled `argocd.argoproj.io/secret-type: repository`, living in the Argo CD namespace; the Secret
itself carries both the connection (`url`, `type`) and the credentials (`sshPrivateKey`,
`username`/`password`, …) **inline**. An `Application` then refers to a repo only by URL
(`spec.source.repoURL`), and Argo resolves the credentials at apply time. Resolution has two layers,
and the second is the "trick" worth remembering:

1. **Exact repository match.** Argo lists the `repository`-labelled Secrets and picks the one whose
   `url` matches the requested URL (normalized), scoped by `project`
   (`secretsRepositoryBackend.getRepositorySecret`).
2. **Credential templates by URL prefix.** If no repository Secret matches, Argo falls back to
   **credential templates** — Secrets labeled `argocd.argoproj.io/secret-type: repo-creds` whose
   `url` is treated as a **prefix**. The template whose URL is the **longest prefix** of the repo URL
   wins (`getRepositoryCredentialIndex`:
   `strings.HasPrefix(NormalizeGitURL(repoURL), NormalizeGitURL(credURL))`, longest match). So you
   declare **one** `repo-creds` Secret with `url: https://github.com/my-org` and every repository
   under that org inherits its credentials without a per-repo Secret. *(This is the "define the first
   part of the URL and it covers the repos beneath it" behaviour.)*

Host keys and CAs are **not** per-repo in Argo: SSH host keys live globally in the
`argocd-ssh-known-hosts-cm` ConfigMap, and custom CA certificates in the `argocd-tls-certs-cm`
ConfigMap (both verified in `common/common.go`). The repository Secret therefore has no
`known_hosts` key at all. Argo's public docs expose three management paths for SSH known hosts:
CLI, UI, and declarative management of that ConfigMap.

The contrast is itself informative: **Flux = an explicit per-repo CRD plus a per-repo named Secret;
Argo = label-discovered Secrets with URL-prefix credential inheritance and global host-key/CA
config.** Our `GitProvider` is closest to Flux's split (a named connection object that references a
Secret), but the *Secret* is the one portable artifact across all three worlds — which is exactly
what the plan leans on.

---

## The trap in our schema today

`GitTarget.spec.providerRef` / the shared `GitProviderReference`
([gittarget_types.go](../../api/v1alpha2/gittarget_types.go)) advertise a Flux `GitRepository`:
the `Group` enum includes `source.toolkit.fluxcd.io`, the `Kind` enum includes `GitRepository`, and
the field comment admits "Support for reading from Flux GitRepository is not yet implemented!"
(The reference is "shared" only in that many `GitTarget`s may point at the same `GitProvider`; it is
not a Flux↔ours indirection.)

But resolution ignores `Kind`/`Group`: `validateProviderAndBranch`
([gittarget_controller.go](../../internal/controller/gittarget_controller.go)) unconditionally
`Get`s a `GitProvider` named `providerRef.Name`. A user who follows the schema and sets
`kind: GitRepository` gets `Referenced GitProvider '<ns>/<name>' not found` — the schema accepts a
shape the controller can never satisfy. A schema must not advertise inputs that always fail.

## Why a `GitRepository` can never be a write target

A Flux `GitRepository` is a **read/source** object: clone read-only at a `ref`, publish an Artifact,
never push. GitOps Reverser **writes** (pushes commits), so there is no artifact for us to consume.
The only reusable part is its connection config (`url`, the branch under `ref`, `secretRef`).
Everything that makes a write target a write target — a writable-branch allowlist, commit identity,
signing, push tuning — has no counterpart on a `GitRepository`.

**Flux's own writer proves the layering.** Flux writes to Git in exactly one place: the
image-automation-controller's `ImageUpdateAutomation` (`api@v1.1.4`, `v1beta2/git.go`). It does
**not** write through a `GitRepository`; it *references* one for connection config and layers its
own write spec on top:

```go
// image-automation-controller/api v1beta2 GitSpec (paraphrased)
type GitSpec struct {
    Checkout *GitCheckoutSpec // ref to clone (defaults to the GitRepository's ref)
    Commit   CommitSpec       // Author{Name,Email}, SigningKey{git.asc GPG}, MessageTemplate
    Push     *PushSpec        // Branch, Refspec, push Options
}
```

That is the same architecture our `GitProvider` already is: connection config (`url`, `secretRef`)
**plus** write identity (`allowedBranches`, `spec.commit` identity/signing, `spec.push` tuning). Even
Flux treats "where to read connection details" and "how to write" as two different objects — so
consuming a `GitRepository` would save us no write configuration. The "one fewer object to declare"
benefit evaporates on inspection.

## The credentials Secret: three-ecosystem comparison

The genuinely portable artifact is the **Git credentials Secret**. Every row below is verified
against source: ours ([helpers.go](../../internal/git/helpers.go),
[ssh/auth.go](../../internal/ssh/auth.go), [security-model.md](../security-model.md)); Flux
(`fluxcd/pkg/runtime@v0.108.0/secrets` constants + `flux2/pkg/manifestgen/sourcesecret`); Argo CD
(`argo-cd` `util/db/repository_secrets.go` + `common/common.go`). The "Ours" column is the **target**
shape this rollout lands, not what the reader accepts today.

| Concept | **Ours — Kubernetes-native** | **Flux** | **Argo CD** |
|---|---|---|---|
| How the Secret is found | typed `secretRef` (name) | typed `secretRef` (name) | label `…/secret-type: repository`; or `repo-creds` template by longest URL prefix |
| SSH private key | `ssh-privatekey` *(`kubernetes.io/ssh-auth`)* | `identity` | `sshPrivateKey` |
| SSH public key | — (derived from private) | `identity.pub` *(optional)* | — |
| SSH key passphrase | `ssh-password` | `password` *(shared with HTTP)* | — *(passphrase keys unsupported)* |
| SSH host keys | `known_hosts` | `known_hosts` *(required)* | **ConfigMap** `argocd-ssh-known-hosts-cm` *(not in the Secret)* |
| Disable host-key check | controller flag, *missing* `known_hosts` only (a present `known_hosts` must parse) | — *(no opt-out; `known_hosts` required)* | `insecureIgnoreHostKey: "true"` |
| HTTP basic user | `username` *(`kubernetes.io/basic-auth`)* | `username` | `username` |
| HTTP basic password | `password` | `password` | `password` |
| HTTP bearer token | `bearerToken` | `bearerToken` | `bearerToken` |
| Client cert (mTLS) | — | `tls.crt` / `tls.key` | `tlsClientCertData` / `tlsClientCertKey` |
| Custom CA | — | `ca.crt` (legacy `caFile`) | **ConfigMap** `argocd-tls-certs-cm` *(not in the Secret)* |
| GitHub App | — | `githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey`, `githubAppBaseURL` | `githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey`, `githubAppEnterpriseBaseUrl` |

Two observations drive everything:

1. **Our keys are not "a third dialect" — they are the Kubernetes built-in ones.** `ssh-privatekey`
   is the field of the core `kubernetes.io/ssh-auth` Secret type; `username`/`password` are the
   fields of `kubernetes.io/basic-auth`. (Honest caveat: the built-in `ssh-auth` type defines *only*
   `ssh-privatekey` — `known_hosts`, the passphrase, and any insecure development opt-out are ours
   to define. For those, `known_hosts` already matches Flux, and the insecure opt-out moves out of
   the ordinary credential Secret to a controller flag.) Neither Flux nor Argo uses the built-in
   Secret *types*; each invented its own key names. So "Kubernetes-native" is genuinely neutral
   ground.
2. **HTTP basic auth is already identical across all three** (`username` + `password`). The only real
   SSH deltas are the key field name and the passphrase field name — both trivial to bridge.

---

## The rollout plan

We ship the following as **one change set**. Validation per AGENTS.md (`task fmt` → `task generate` →
`task manifests` → `task vet` → `task lint` → `task test` → `task test-e2e`) runs once over the whole
set, not per step.

### Step 1 — Drop the `GitRepository` trap from the schema

In [gittarget_types.go](../../api/v1alpha2/gittarget_types.go), remove every trace of Flux from
`GitProviderReference`:

- Remove `source.toolkit.fluxcd.io` from the `Group` enum and `GitRepository` from the `Kind` enum.
- Delete the "Support for reading from Flux GitRepository is not yet implemented!" comment and the
  "the GitProvider or Flux GitRepository" field comments — they become just "the GitProvider".
- With Flux gone, `Group` and `Kind` each have exactly one legal value (`configbutler.ai` /
  `GitProvider`). **Keep the typed `Group`/`Kind` fields with those as defaults** (and `Kind`
  constrained to a single-value enum), matching the project's other local references
  (`LocalTargetReference`, `LocalSecretReference`) rather than collapsing to a name-only reference.
  Many `GitTarget`s may reference the same `GitProvider`; in practice a user only sets `name`, since
  `group`/`kind` default. (Pre-1.0, we change the schema directly; nothing to migrate.)

Then regenerate — `task generate` (deepcopy) and `task manifests` (CRDs under
[config/crd/bases](../../config/crd/bases)) — and update any example manifests/docs that still write
`group:` / `kind:` under `providerRef`.

Rationale (from "The trap" above): the schema today accepts an input the controller can never
honor — `validateProviderAndBranch` unconditionally `Get`s a `GitProvider` and ignores
`Kind`/`Group`, so `kind: GitRepository` always returns `Referenced GitProvider '<ns>/<name>' not
found`. Removing the enum also removes the only place the word "Flux" prints into a user's
`kubectl explain`.

### Step 2 — Read all three credential dialects, and add the bearer token

In the Secret reader ([helpers.go](../../internal/git/helpers.go)), resolve in this order:

- **SSH private key**, in priority order: `ssh-privatekey` → `identity` → `sshPrivateKey`.
- **SSH key passphrase**: `ssh-password`, falling back to `password` **only when an SSH key is
  present** — exactly Flux's own disambiguation (Flux stores the passphrase under `password` and
  tells it apart by the presence of `identity`). With no SSH key present, `password` is the HTTP
  basic password.
- **HTTP basic**: `username` / `password` — already universal, no change.
- **HTTP bearer token** *(new — the gap we are closing)*: read `bearerToken` and authenticate with
  it (go-git `http.TokenAuth`). Bearer tokens are the common HTTPS path in both ecosystems (GitHub
  fine-grained PATs, GitLab project/group access tokens) and our reader has no path for them today —
  a `bearerToken`-only Secret currently falls through to "does not contain valid authentication
  data".

Overall auth precedence stays: SSH key (if present) → HTTP basic (`username`+`password`) → bearer
token (`bearerToken`).

### Step 3 — Make SSH host trust centralizable

`known_hosts` is security-critical trust material, not credential material. Keeping it only inside
each credentials Secret is Flux-compatible and fine for one repo, but repetitive across many
`GitProvider`s on the same host. Resolution order:

1. **Secret-level `known_hosts`** — highest priority; keeps Flux-authored SSH Secrets working
   directly.
2. **`GitProvider.spec.knownHostsRef`** — a namespace-local ConfigMap or Secret holding `known_hosts`
   (and optionally `ssh_known_hosts` for Argo-shaped data copied out of its ConfigMap).
3. **An install-level default known-hosts ConfigMap** in the controller's namespace, for
   cluster-admin-managed Git hosts.
4. If none yields valid host keys, SSH auth **fails closed**.

We do **not** read Argo's `argocd-ssh-known-hosts-cm` directly (that would be Argo coupling), and we
do **not** auto-refresh host keys with `ssh-keyscan` — that only reports "what the network showed me
right now." Host-key rotation is an admin-owned declarative update; admins verify fingerprints
out-of-band (GitHub/GitLab publish them; self-hosted services publish via their platform team).

### Step 4 — Flag-gate the insecure opt-out, and require a present `known_hosts` to be valid

Today the insecure opt-out is a per-Secret key `insecure_ignore_host_key`
([ssh/auth.go](../../internal/ssh/auth.go)) that also swallows an *unparseable* `known_hosts`.
Replace it:

- **Remove the per-Secret `insecure_ignore_host_key` key entirely** (pre-1.0, nothing to migrate).
- Add a controller flag **`--insecure-allow-missing-known-hosts`**, default off, for throwaway/dev
  clusters only. It is deliberately **narrow**: it permits SSH only when **no** host-key source
  produced any `known_hosts` at all.
- A `known_hosts` that **is** present but fails to parse is a **hard error regardless of the flag** —
  if a key is defined it must be valid. (This narrows current behavior on purpose: today the opt-out
  also bypasses an unparseable key; it no longer will.)
- The insecure path stays harder than adding a key to a Secret, and user-facing docs never show it in
  normal setup examples.

### Out of scope for this rollout

Custom CA / client certs (mTLS) and GitHub App keys stay **unread** — they are HTTPS-auth refinements
we can add later without reshaping anything here; the schema just doesn't pretend to accept them.
`identity.pub` is never needed (go-git derives the public key from the private key). Argo's external
`argocd-tls-certs-cm` / `argocd-ssh-known-hosts-cm` ConfigMaps are never read directly.

## Why Kubernetes-native is the canonical shape (and we still read the others)

This is the choice worth settling, because "be like Flux and Argo" and "be Kubernetes-native"
really are different choices — *neither* competitor took the native path (Flux reads opaque Secrets
keyed `identity`; Argo reads opaque Secrets keyed `sshPrivateKey` and discovers them by label). It
helps to split the decision into two independent halves:

- **What we read** (interop) is settled by real demand: early adopters are GitOps-minded and will
  arrive with Flux *or* Argo Secrets, so we accept **all three** dialects. The canonical choice does
  not change this — accepting one more alias costs nothing.
- **What we *show*** (our own examples, and anything we ever generate) is the only real choice, and
  there Kubernetes-native wins:
  1. **It is an actual standard with tooling.** `kubectl create secret generic --type=kubernetes.io/ssh-auth`,
     Sealed Secrets, External Secrets, and SOPS all understand the built-in types; `identity` /
     `sshPrivateKey` are just opaque blobs.
  2. **Neutral by construction.** "Kubernetes-native" names no competitor, so an Argo user reading
     our example never sees a Flux key (or the word Flux), and we can't be accused of adopting a
     house style.
  3. **Lowest churn.** It is already our documented shape (README, [security-model.md](../security-model.md)).

The counter-argument for "just do both dialects, skip native" is honesty about provenance (almost
nobody has a `kubernetes.io/ssh-auth` Secret lying around) and one fewer concept. But that argument
is entirely about *reading*, which we already solve by accepting aliases — so it does not actually
push against a native canonical. **Net rule: read all three; show native; tell Flux/Argo users in
one line that HTTP basic-auth and bearer-token Secrets work directly, Flux SSH keys work directly,
and Argo SSH keys need a neutral `known_hosts` source.**

## On naming our connection object

(We have no `GitRepository` kind of our own — our connection object is **`GitProvider`**, and the
write target is `GitTarget`.) `GitProvider` is already distinct from both ecosystems: Flux's read
object is `GitRepository`, and Argo has no kind at all. The plan keeps `GitProvider`. The one
naming rule worth stating explicitly: **do not rename it to anything containing "Repository"** — that
would invite exactly the Flux confusion we are removing from the schema. ("Provider" leans slightly
toward "the hosting service" rather than "this repo + branch + write identity," but an API-kind
rename churns every doc and example and buys nothing the neutrality goal needs.)

## Git write-back landscape (documented, not consumed)

The only controllers in either ecosystem that *push* to Git — our nearest analogues, and where our
bi-directional hazard meets reality:

- **Flux — `ImageUpdateAutomation`** (image-automation-controller), driven by `ImagePolicy` /
  `ImageRepository` (image-reflector-controller). It clones the repo named by a `GitRepository`,
  rewrites image tags in place, then **commits** (author identity, optional GPG signing via a
  `git.asc` key in `spec.commit.signingKey.secretRef`, `messageTemplate`) and **pushes**
  (`spec.push.branch` / `refspec` / `options`). Scope: image tags only.
- **Argo CD — Argo CD Image Updater** (a separate project; Argo CD core never writes to Git). With
  the **`git` write-back method** it commits the new image to a branch using its own Git credentials;
  with the **`argocd` method** it mutates the `Application` and writes nothing to Git. Scope: image
  tags only.

Relevance: both are narrow single-purpose writers (image tags), whereas GitOps Reverser writes
arbitrary watched resources back to Git — a strictly broader "reverse GitOps" surface, statable
without preference ("like image write-back, but for any watched type"). If a GitTarget writes the
same branch/path an image-updater also writes, that is two competing writers on one branch — the
shared-path hazard in [bi-directional.md](../bi-directional.md), worth a docs warning regardless of
who the other writer is. We consume none of these and import none of their types; they are here for
the map, not the build.

---

### Sources

- Flux secret keys: `github.com/fluxcd/pkg/runtime@v0.108.0/secrets` (`secrets.go`, `factory.go`
  `MakeSSHSecret`), `flux2/pkg/manifestgen/sourcesecret` (`options.go`, `sourcesecret.go`).
- Flux `GitRepository`: `github.com/fluxcd/source-controller/api@v1.8.5/v1/gitrepository_types.go`.
- Flux docs: [GitRepositories](https://fluxcd.io/flux/components/source/gitrepositories/),
  [`flux create secret git`](https://fluxcd.io/flux/cmd/flux_create_secret_git/).
- Flux image write-back: `github.com/fluxcd/image-automation-controller/api@v1.1.4/v1beta2/git.go`.
- Argo CD repository / repo-creds Secrets and URL-prefix matching:
  `argo-cd` `util/db/repository_secrets.go` (`secretToRepository`, `getRepositoryCredentialIndex`),
  `util/db/repository.go` (`RepoURLToSecretName`), `common/common.go`
  (`LabelKeySecretType`, `ArgoCDKnownHostsConfigMapName`, `ArgoCDTLSCertsConfigMapName`).
- Argo CD docs:
  [Declarative Setup](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/),
  [Private Repositories](https://argo-cd.readthedocs.io/en/stable/user-guide/private-repositories/),
  [Argo CD Image Updater](https://argocd-image-updater.readthedocs.io/en/stable/).
- SSH host-key verification sources:
  [`ssh-keyscan(1)`](https://man.openbsd.org/ssh-keyscan.1),
  [GitHub SSH key fingerprints](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints),
  [GitLab SSH key setup and host-key verification](https://docs.gitlab.com/user/ssh/).
