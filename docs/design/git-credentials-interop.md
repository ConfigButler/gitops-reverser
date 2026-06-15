# Git config interop: drop the `GitRepository` providerRef, align the credentials Secret

Status: investigation + recommendation. No implementation yet. Supersedes the earlier
"`GitRepository` as a `providerRef`" feasibility note: it broadens the question from "consume one
Flux object" to "interoperate with the Git credentials people already have" — whether those come
from Flux **or** Argo CD — without coupling to, advertising, or requiring either.

## TL;DR

- **Drop** `GitRepository` / `source.toolkit.fluxcd.io` from `GitProviderReference` (the `Kind` and
  `Group` enums). It is a trap today (the controller cannot honor it), it is semantically wrong (a
  read object used as a write target), and — counter-intuitively — keeping it would make us *more*
  coupled to Flux, not less. Removing it also removes Flux from every user's CRD schema, which is
  what makes Argo CD users feel at home.
- **Align the credentials Secret instead.** Our Secret keys are already the **Kubernetes-native**
  ones (`ssh-privatekey`, `username`, `password` — the field names of the built-in
  `kubernetes.io/ssh-auth` and `kubernetes.io/basic-auth` Secret types). Accept the small set of
  Flux/Argo alias keys as fallbacks so a Secret authored for either ecosystem works unchanged. This
  is the real interop win, it serves *both* audiences, and it names no vendor in the user-facing
  surface.
- **Document the Git write-back landscape** (Flux's `ImageUpdateAutomation`, Argo CD Image Updater)
  separately — these are the only controllers in either ecosystem that *write* to Git, they are our
  nearest analogues, and they share our bi-directional hazard. We neither consume nor depend on
  them; we document them for positioning and for a guardrail note.

The framing requirement throughout: **never require Flux, never advertise a preference, never make
an Argo CD user read the word "Flux" to use the product.** Internally (this doc) we name names
freely; the user-facing schema and docs stay vendor-neutral.

---

## 1. Current affairs — the trap in the schema today

`GitTarget.spec.providerRef` and the shared `GitProviderReference`
([gittarget_types.go](../../api/v1alpha1/gittarget_types.go)) already advertise a Flux
`GitRepository`:

- `Group` enum includes `source.toolkit.fluxcd.io`
- `Kind` enum includes `GitRepository`
- the field comment says: "Support for reading from Flux GitRepository is not yet implemented!"

But resolution ignores `Kind` and `Group`. `validateProviderAndBranch`
([gittarget_controller.go](../../internal/controller/gittarget_controller.go)) unconditionally does
a `Get` for a `GitProvider` named `providerRef.Name`. So a user who follows the schema and sets
`kind: GitRepository` gets `Referenced GitProvider '<ns>/<name>' not found` — the schema accepts a
shape the controller can never satisfy. A schema must not advertise inputs that always fail. This
alone is worth fixing, and it is already flagged as a maturity-plan item
([maturity-and-adoption-plan.md](../maturity-and-adoption-plan.md)) and in
[configuration.md](../configuration.md) ("support … is not implemented yet").

## 2. The read/write truth — why a `GitRepository` can never be a write target

A Flux `GitRepository`
(`source.toolkit.fluxcd.io`, source-controller `api@v1.8.5`,
[`gitrepository_types.go`](https://github.com/fluxcd/source-controller)) is a **read/source**
object. source-controller clones the repo **read-only** at a `ref`, optionally verifies it, and
publishes an **Artifact** (a tarball + checksum) for downstream Flux controllers to consume. Its
spec is `url` + `secretRef` + `ref` (`branch`/`tag`/`semver`/`name`/`commit`) + `interval` +
`verify`/`ignore`/`sparseCheckout`/… It never pushes and grants no write path.

GitOps Reverser **writes** (pushes commits). So a `GitRepository` cannot be "consumed" the way Flux
consumes it — there is no artifact we want. The only reusable part is its **connection
configuration**: `spec.url`, the branch under `spec.ref`, and `spec.secretRef`. Everything that
makes a write target a write target — a writable-branch allowlist, commit identity, signing, push
tuning — has no counterpart on a `GitRepository`.

**Flux's own writer proves the layering.** Flux *does* write to Git in exactly one place: the
image-automation-controller's `ImageUpdateAutomation` (`api@v1.1.4`,
[`git.go`](https://github.com/fluxcd/image-automation-controller) `v1beta2`). Crucially, it does
**not** write *through* a `GitRepository`. It *references* a `GitRepository` purely for the
connection config (url + secretRef + ref) and then layers its **own** write spec on top:

```go
// image-automation-controller/api v1beta2 GitSpec (paraphrased)
type GitSpec struct {
    Checkout *GitCheckoutSpec // ref to clone (defaults to the GitRepository's ref)
    Commit   CommitSpec       // Author{Name,Email}, SigningKey{git.asc GPG}, MessageTemplate
    Push     *PushSpec        // Branch, Refspec, push Options
}
```

That is the same architecture our `GitProvider` already is: connection config (`url`, `secretRef`)
**plus** write identity (`allowedBranches`, `spec.commit` identity/signing, `spec.push` tuning).
Even Flux treats "where to read connection details" and "how to write" as two different objects. So
consuming a `GitRepository` would not save us a single piece of write configuration — we would still
need a companion object for commit/signing/push/branch-allowlist, i.e. a `GitProvider` in all but
name. The "one fewer object to declare" benefit evaporates on inspection.

## 3. What is actually reusable: the credentials Secret (three-ecosystem comparison)

The genuinely portable artifact is the **Git credentials Secret**. Here is where the three
ecosystems land. Verified against source where available: ours
([helpers.go](../../internal/git/helpers.go), [ssh/auth.go](../../internal/ssh/auth.go),
[security-model.md](../security-model.md)); Flux (`fluxcd/pkg/runtime@v0.108.0/secrets` constants +
`flux2` `pkg/manifestgen/sourcesecret`); Argo CD (declarative-setup / private-repositories docs,
see Sources).

| Concept | **Ours — Kubernetes-native** | **Flux** | **Argo CD** |
|---|---|---|---|
| How the Secret is found | typed `secretRef` (name) | typed `secretRef` (name) | label `argocd.argoproj.io/secret-type: repository`, `url` key inside |
| SSH private key | `ssh-privatekey` *(`kubernetes.io/ssh-auth`)* | `identity` | `sshPrivateKey` |
| SSH public key | — (derived from private) | `identity.pub` *(optional)* | — |
| SSH key passphrase | `ssh-password` | `password` *(shared with HTTP)* | — *(passphrase keys unsupported)* |
| Host keys | `known_hosts` | `known_hosts` *(required)* | **ConfigMap** `argocd-ssh-known-hosts-cm` (not in the Secret) |
| Disable host-key check | `insecure_ignore_host_key: "true"` | — (host key via spec) | `insecureIgnoreHostKey` |
| HTTP basic user | `username` *(`kubernetes.io/basic-auth`)* | `username` | `username` |
| HTTP basic password | `password` | `password` | `password` |
| HTTP bearer token | — | `bearerToken` | — (cluster secrets only) |
| Custom CA | — | `ca.crt` (legacy `caFile`) | via TLS config / mTLS |
| Client cert (mTLS) | — | `tls.crt` / `tls.key` | `tlsClientCertData` / `tlsClientCertKey` |
| GitHub App | — | `githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey`, `githubAppBaseURL` | `githubAppID`, `githubAppInstallationID`, `githubAppPrivateKey`, `githubAppEnterpriseBaseUrl` |

Two observations drive the whole recommendation:

1. **Our keys are not "a third dialect" — they are the Kubernetes built-in ones.** `ssh-privatekey`
   is the field of the core `kubernetes.io/ssh-auth` Secret type; `username`/`password` are the
   fields of `kubernetes.io/basic-auth`. That is the perfect neutral ground: we can document "we use
   Kubernetes-native Secret types" and never name Flux or Argo. Flux and Argo each invented their
   own key names; we did not.
2. **HTTP basic auth is already identical across all three** (`username` + `password`). The only real
   deltas are the **SSH key field name** and the **passphrase field name** — both trivial to bridge.

## 4. Decision: drop the providerRef enum, align the Secret

### 4.1 Dropping `GitRepository` *reduces* Flux coupling

The earlier note framed the choice as "implement a thin config-source adapter later." On reflection,
keeping the enum is the more Flux-coupled path, because honoring it would require:

- importing/RBAC for `gitrepositories.source.toolkit.fluxcd.io` (a hard dependency on
  source-controller's CRD being installed — i.e. **requiring Flux**, which is explicitly off the
  table), and
- a `ref` → writable-branch translation that rejects tag/semver/commit/name (only `ref.branch` is
  writable), and
- inheriting the bi-directional footgun: source-controller re-clones on its interval and a
  downstream Kustomization/HelmRelease may apply the branch, so pointing a *writer* at a
  Flux-tracked branch invites a write→pull→apply→audit→write loop (see
  [bi-directional.md](../bi-directional.md)).

Dropping the enum deletes all three problems at once and removes Flux from the user-facing schema.

### 4.2 Neutrality requirements

- **No Flux in the CRD.** A `providerRef.kind: GitRepository` enum literally prints "Flux" into every
  user's `kubectl explain`. Removing it means an Argo CD user never encounters Flux to use the
  product. This is the single highest-leverage neutrality fix.
- **No vendor in the docs.** The Secret-key docs present alternate accepted key names in a neutral
  table ("also accepted: …"), not "Flux keys" / "Argo keys." We are happy to interoperate; we do not
  advertise a house style. (Internally, in this doc, we are candid about provenance.)
- **No required install of either tool.** Alias support is pure string matching on Secret data — it
  works whether or not source-controller or Argo is present in the cluster.

## 5. The Secret-alignment plan

Keep **Kubernetes-native keys canonical**; accept a minimal alias set as fallbacks. Today the auth
method is picked by the keys present in the Secret
([helpers.go](../../internal/git/helpers.go) `getAuthFromSecret`): SSH if `ssh-privatekey` is
present, else HTTP basic if `username`/`password` are present. Generalize that:

- **SSH private key**, read in priority order:
  `ssh-privatekey` → `identity` → `sshPrivateKey`.
- **SSH key passphrase**: `ssh-password` → `password`, but the `password` fallback is consulted
  **only when an SSH key is present** (this matches Flux exactly — Flux stores the passphrase under
  `password` and disambiguates by the presence of `identity`). When no SSH key is present,
  `password` is the HTTP basic password. Transport is otherwise unambiguous: it follows the repo
  URL scheme, and SSH vs HTTP key presence never overlaps for one provider.
- **Host keys**: keep `known_hosts`. This is already shared with Flux. The one genuine gap is **Argo
  CD**, which keeps host keys in the `argocd-ssh-known-hosts-cm` ConfigMap rather than the Secret —
  an Argo-origin Secret will have no `known_hosts`. Document that those users copy their
  `known_hosts` entry into the Secret (or use `insecure_ignore_host_key` for throwaway envs). We do
  **not** read Argo's ConfigMap (that would be Argo coupling).
- **HTTP basic**: `username`/`password` — already universal, zero work.
- **SSH host-key verification** is already hardened to fail closed on this branch
  ([ssh/auth.go](../../internal/ssh/auth.go): `known_hosts` required unless
  `insecure_ignore_host_key: "true"`). No change needed; the alignment work is purely the key-name
  fallbacks above.

Out of scope for the first cut, documented as "not yet read" so nobody is trapped a second time:
`bearerToken`, `ca.crt`/`caFile`, `tls.crt`/`tls.key`/`tlsClientCert*`, GitHub App keys. These are
HTTPS-auth refinements; add them on demand. (`identity.pub` is never needed — go-git derives the
public key from the private key.)

Net effect: a Secret a user already wrote for **either** Flux **or** Argo CD's SSH/HTTPS-basic auth
works against a `GitProvider` unchanged, with the single documented exception of Argo's externalized
`known_hosts`. No vendor named in the schema; no vendor required in the cluster.

## 6. Git write-back landscape (documented, not consumed)

The user asked that Flux's image write-back be written down somewhere; for symmetry here is the Argo
analogue too. These are the **only** controllers in either ecosystem that push to Git, which makes
them our nearest cousins and the place our bi-directional hazard overlaps reality.

- **Flux — `ImageUpdateAutomation`** (image-automation-controller), driven by `ImagePolicy` /
  `ImageRepository` (image-reflector-controller). It clones the repo named by a `GitRepository`,
  rewrites image tags in-place in the checked-out manifests, then **commits** (author identity,
  optional GPG signing via a `git.asc` key in `spec.commit.signingKey.secretRef`, `messageTemplate`)
  and **pushes** (`spec.push.branch` / `refspec` / `options`). Scope: image tags only.
- **Argo CD — Argo CD Image Updater** (a separate project; Argo CD core never writes to Git). With
  the **`git` write-back method** it commits the new image to a branch using a Git credentials Secret
  (its own `argocd-image-updater` config / SSH creds); with the **`argocd` method** it mutates the
  `Application` instead and writes nothing to Git. Scope: image tags only.

Relevance to us:

- **Positioning.** Both are narrow, single-purpose writers (image tags). GitOps Reverser writes
  arbitrary watched resources back to Git — a strictly broader "reverse GitOps" surface. Useful
  framing for README/positioning, again statable without preference ("like image write-back, but for
  any watched type").
- **Guardrail.** If a GitTarget writes the same branch/path that an image-updater also writes, you
  have two competing writers on one branch — the shared-path hazard in
  [bi-directional.md](../bi-directional.md). Worth a docs warning, not code: the conflict is the same
  whether the other writer is Flux, Argo, or a human.
- **Non-dependency.** We consume none of these and import none of their types. They are documented
  for the map, not the build.

## 7. Staged plan / action items

1. **Remove the trap (schema).** Drop `GitRepository` from the `Kind` enum and
   `source.toolkit.fluxcd.io` from the `Group` enum in `GitProviderReference`
   ([gittarget_types.go](../../api/v1alpha1/gittarget_types.go)); regenerate CRDs (`task manifests`).
   Update the now-stale references in [configuration.md](../configuration.md) (the "`GitRepository` …
   not implemented yet" lines) and the open item in
   [maturity-and-adoption-plan.md](../maturity-and-adoption-plan.md). Net: Flux disappears from the
   user-facing schema.
2. **Secret-key fallbacks (code).** Generalize `getAuthFromSecret`
   ([helpers.go](../../internal/git/helpers.go)) to the priority order in §5: SSH key
   `ssh-privatekey`→`identity`→`sshPrivateKey`; passphrase `ssh-password`→`password` (SSH-present
   only). Table-driven tests for each origin (k8s-native / Flux-style / Argo-style) plus the
   passphrase-disambiguation case. No new RBAC, no new imports.
3. **Document the Secret neutrally.** Extend the Git-credentials-Secret table in
   [security-model.md](../security-model.md) and [configuration.md](../configuration.md) with an
   "also accepted" column for the alias keys, and a one-line note that Argo CD's externalized
   `known_hosts` must be copied into the Secret. No vendor named as the "right" one.
4. **Write down the write-back landscape.** Land §6 (Flux `ImageUpdateAutomation` + Argo CD Image
   Updater) into a positioning/landscape doc and cross-link the bi-directional guardrail.
5. **Defer on demand:** `bearerToken`, CA/TLS, GitHub App keys — add when a real user needs HTTPS
   push to a custom-CA/GitHub-App-auth server; track as a follow-up, advertised as "not yet read."

## 8. Open questions

- **Canonical vs alias surface in docs.** Do we present k8s-native as "the" shape with aliases in a
  footnote, or a flat "any of these keys" table? Leaning k8s-native-canonical so our own examples
  stay vendor-neutral and copy-pasteable.
- **Should step 1 ship before step 2?** Removing the enum is a clean, independent maturity fix and
  can land first; the secret fallbacks are additive and can follow without re-touching the schema.
- **Argo `known_hosts` ergonomics.** Copy-into-Secret is the simple answer. If Argo interop becomes a
  real ask, revisit whether a documented `known_hosts`-from-ConfigMap convenience is worth the Argo
  coupling (current answer: no).

---

### Sources

- Flux secret keys: `github.com/fluxcd/pkg/runtime@v0.108.0/secrets` (`secrets.go`, `factory.go`
  `MakeSSHSecret`), `flux2/pkg/manifestgen/sourcesecret` (`options.go`, `sourcesecret.go`).
- Flux `GitRepository`: `github.com/fluxcd/source-controller/api@v1.8.5/v1/gitrepository_types.go`.
- Flux image write-back: `github.com/fluxcd/image-automation-controller/api@v1.1.4/v1beta2/git.go`.
- Argo CD repository Secret / SSH known-hosts:
  [Declarative Setup](https://argo-cd.readthedocs.io/en/stable/operator-manual/declarative-setup/),
  [Private Repositories](https://argo-cd.readthedocs.io/en/stable/user-guide/private-repositories/),
  [`argocd-ssh-known-hosts-cm`](https://argo-cd.readthedocs.io/en/stable/operator-manual/argocd-ssh-known-hosts-cm-yaml/).
- Argo CD Image Updater (Git write-back):
  [Image Updater docs](https://argocd-image-updater.readthedocs.io/en/stable/).
