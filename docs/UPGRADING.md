# Upgrading

Breaking changes and the steps to adopt them, newest first. The machine-generated per-release
summary lives in [`CHANGELOG.md`](../CHANGELOG.md); this file is the human-written migration
guidance that the changelog's breaking-change entries link to.

We are pre-1.0, so breaking changes bump the **minor** version (release-please is configured with
`bump-minor-pre-major`) rather than the major. Read the relevant entry before upgrading across it.

## Unreleased — Git credentials interop (next minor; breaking)

Two user-visible breaking changes land together. Both come from
[`design/git-credentials-interop.md`](design/git-credentials-interop.md).

### 1. `providerRef` no longer advertises a Flux `GitRepository`

`GitTarget.spec.providerRef` (the shared `GitProviderReference`) previously listed
`source.toolkit.fluxcd.io` in its `group` enum and `GitRepository` in its `kind` enum. That input
never worked — the controller always resolved a `GitProvider`, so a `providerRef` pointing at a
`GitRepository` failed at runtime with `Referenced GitProvider '<ns>/<name>' not found`. Those enum
values are now **removed from the CRD**, so such a manifest is rejected at apply time instead.

`group` and `kind` keep their typed fields but now have a single legal value each, supplied by
CRD defaulting:

- `group` defaults to `configbutler.ai`
- `kind` defaults to `GitProvider` (a single-value enum)

**Migration**

- If your `GitTarget` only sets `providerRef.name` (the common case), **no change is needed.**
- If you set `providerRef.group` or `providerRef.kind` explicitly, drop them or set them to the
  defaults above:

  ```yaml
  spec:
    providerRef:
      name: my-git-provider   # group/kind now default; omit them
  ```

- If any `GitTarget` pointed at `kind: GitRepository`, it was already non-functional. Point it at a
  real `GitProvider` instead.

**Not breaking, but new in the same change:** the credentials-Secret reader now also accepts
Flux- and Argo-CD-authored credential Secrets directly and adds HTTP **bearer-token** auth
(`bearerToken`). Existing Flux/Argo users can reuse their Secret unchanged — see
[`configuration.md`](configuration.md) and [`security-model.md`](security-model.md).

### 2. SSH host-key opt-out moved from a Secret key to a controller flag

The per-Secret `insecure_ignore_host_key` key is **removed**. It is no longer read; a Secret that
still carries it is treated as if it were absent. SSH now **fails closed** unless a valid
`known_hosts` is supplied through one of:

1. the credentials Secret's own `known_hosts` key (unchanged; Flux-shaped Secrets keep working),
2. `GitProvider.spec.knownHostsRef` — a namespace-local ConfigMap or Secret holding `known_hosts`
   (also reads `ssh_known_hosts`, for data copied out of Argo's `argocd-ssh-known-hosts-cm`),
3. an install-level default known-hosts ConfigMap in the controller's namespace.

Two further tightenings:

- A new controller flag **`--insecure-allow-missing-known-hosts`** (default **off**, dev/throwaway
  clusters only) permits SSH **only when no host-key source produced any `known_hosts` at all.** It
  is deliberately narrower than the old key.
- A `known_hosts` that **is** present but fails to parse is now a **hard error regardless of the
  flag.** The old key silently swallowed an unparseable value; it no longer does.

**Migration**

- **Recommended:** add a real `known_hosts` to the credentials Secret, or supply it via
  `GitProvider.spec.knownHostsRef` / an install-level default ConfigMap, then delete the obsolete
  `insecure_ignore_host_key` key.
- **Dev/throwaway clusters only:** set `--insecure-allow-missing-known-hosts` on the controller and
  remove the Secret key. Never set this flag in production.
- If you relied on the old key to mask a malformed `known_hosts`, fix the `known_hosts` content — it
  must now parse.
