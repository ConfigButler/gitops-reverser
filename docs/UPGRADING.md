# Upgrading

Breaking changes and the steps to adopt them, newest first. The machine-generated per-release
summary lives in [`CHANGELOG.md`](../CHANGELOG.md); this file is the human-written migration
guidance that the changelog's breaking-change entries link to.

We are pre-1.0, so breaking changes bump the **minor** version (release-please is configured with
`bump-minor-pre-major`) rather than the major. Read the relevant entry before upgrading across it.

## Unreleased — `pkg/manifestanalyzer`: the overlay fan-out refusal code was renamed (next minor; breaking for consumers)

One refusal reason changed its name, in both the Go constant and the machine-readable
value it carries:

| | Before | After |
|---|---|---|
| Go constant | `ReasonOverlayFanOutNeedsF2` | `ReasonOverlayFanOutUnsupported` |
| `RefusalReason.Code` (JSON) | `overlay-fan-out-needs-f2` | `overlay-fan-out-unsupported` |

The meaning is unchanged: a kustomize overlay whose base is shared by more than one
render root is refused, and the refusal is a *forward-looking* one — it flips to accepted
when render-root scoping ships, unlike `refused-structural`, which is the permanent
boundary. The old name encoded an internal roadmap label (`F2`) that meant nothing outside
our planning docs; the new one says what it means.

**Migration**

- **Go consumers** get a compile error naming the constant. Rename and rebuild.
- **Consumers matching the JSON `code` string get no error.** This is the one to look for:
  a `switch` or `if` on `"overlay-fan-out-needs-f2"` simply stops matching, and the refusal
  falls through to whatever your default branch does. Grep for the old string.

## Unreleased — the wildcard cluster read moved to its own, droppable ClusterRole (next minor; behavior change)

The shipped RBAC now says what the binary actually does. Nothing is taken away from a default
install; what changes is that the parts can be separated.

**The manager ClusterRole no longer contains `apiGroups: ["*"], resources: ["*"]`.** The types a
`WatchRule` may read now come from a ClusterRole of their own, rendered from the new
`rbac.watchTypes` and bound to the same ServiceAccount. The default (`mode: any`) reproduces the old
wildcard exactly, so the default install keeps the same effective permissions. The split exists
because RBAC is additive: while the wildcard sat in the manager role, no chart value could remove the
cluster-wide Secret read it implied.

**The manager role's `secrets` rule narrowed from `get,list,watch,create,update,patch` to
`get,create,update`.** The operator has held no Secret informer since `v0.31.0` — Secrets are
excluded from the manager cache, so every read is a direct `get` of a Secret a `GitProvider` or
`GitTarget` names — but the marker was never updated to match. It never used `list`, `watch` or
`patch`.

**The manager role gained explicit `get,list,watch` on `customresourcedefinitions` and
`apiservices`.** They were previously reachable only through the wildcard. The API-resource catalog
and its trigger informers read both.

**Migration**

- Default installs (`rbac.watchTypes` unset): no action. Helm creates `<release>-watch-any` and its
  binding; `kubectl apply` of `dist/install.yaml` includes the same pair.
- To run least-privilege, set `rbac.watchTypes.mode: selected` and list the types your `WatchRule`s
  name. The chart renders the ClusterRole; verbs are always `get,list,watch`. See [`rbac.md`](rbac.md).

  ```yaml
  rbac:
    watchTypes:
      mode: selected
      selected:
        - apiGroups: [""]
          resources: ["configmaps"]
        - apiGroups: ["apps"]
          resources: ["deployments"]
  ```

- If you hand-wrote a role because the shipped one was too broad, you can now drop it along with the
  parts that duplicate `<release>-manager-role`.

**Related behavior change.** A trigger informer (`customresourcedefinitions`, `apiservices`) that
the API server serves but RBAC denies is now **stopped** after the first `403`, logged once, and
re-armed automatically on a later catalog refresh if the permission is granted. Previously the
reflector retried the denial forever. Discovery reports what the server serves, not what the caller
may read, so a narrowed role reached this path — which is exactly the path this release makes easy
to enter.

## Unreleased — `manifest-analyzer` scan modes renamed, and `--format json` now emits a versioned contract (next minor; breaking)

The analyzer's machine-readable output moved to the new public
[`pkg/manifestanalyzer`](../pkg/manifestanalyzer), which is a supported Go API and the single
definition of the JSON documents the CLI prints. Freezing that contract is also the moment to name
the CLI modes after the question each answers, so the CLI, the Go API and the docs use one pair of
nouns — **folder** and **repo**.

**Modes renamed. There are no back-compat aliases: the old names now exit 2 (usage error).**

| Before | After | Answers |
|---|---|---|
| `--mode scan` | `--mode scan-folder` | May **this folder** become a `GitTarget`? (`ScanFolder`) |
| `--mode repo-walker` | `--mode scan-repo` | Which folders under **this repo root** could? (`ScanRepo`) |

`repo-walker` named an internal traversal phase rather than a contract, and a bare `scan` was
asymmetric once a repo-level scan existed. `--mode analyze` and `--mode discovery` are unchanged.

The JSON documents also gained a `schemaVersion` field, and one field was dropped:

- `--mode scan-folder --format json` no longer carries `plan`. In folder-scan mode the analyzer has
  no cluster state and no desired resources, so `plan` was structurally always
  `{"counts":{},"actions":null}` — it never carried information. The meaningful fields (`accepted`,
  `issues`, `retained`) are unchanged, and `issues` now marshals as `[]` rather than `null` when
  there are none.
- `--mode scan-repo --format json` is otherwise unchanged.
- Retained entries now omit `identity` for a whole-file retention (an ordinary
  `kustomization.yaml`, which names no resource) instead of emitting a zero-valued object. It is
  still present for the refused mixed-file case, where a named resource hides inside a build
  directive.
- `--mode analyze` and every `--format text` output are unchanged.

**Migration**

- Replace `--mode scan` with `--mode scan-folder`, and `--mode repo-walker` with `--mode scan-repo`.
  A stale invocation fails loudly rather than falling back to the default `analyze` mode.
- Read `schemaVersion` and ignore fields you do not know; new fields get added. The report is
  pre-1.0 and carries no compatibility guarantee — pin a version.
- If you exec'd the binary only to reach the acceptance verdict, prefer importing
  `pkg/manifestanalyzer` and calling `ScanFolder` / `ScanRepo`. They run the same acceptance gate the
  operator's writer enforces, so a tool built on them cannot drift from the operator that will later
  adopt (or refuse) the folder.
- If you parsed `plan` from folder-scan mode, you were reading an empty object; drop the field.

## Unreleased — chart defaults now run Redis-free (next minor; behavior change)

The Helm chart now defaults to the simple, Redis-free `configured-author` path, so a bare
`helm install` comes up healthy without external infrastructure:

- `queue.redis.addr` now defaults to `""` (was `valkey:6379`) and `queue.redis.auth.existingSecret`
  to `""` (was `valkey-auth`). Without a Redis endpoint the operator runs `configured-author` and
  watches cold-replay on restart.
- `servers.admission.enabled` stays `true` by default, but the validate-operator-types admission
  webhook no longer requires Redis. Without `queue.redis.addr` it runs as a no-op (CommitRequests
  commit as the configured committer, `AuthorAttributed=False`); it captures authors once Redis is
  configured. Previously enabling admission without Redis failed startup.
- The chart still rejects one invalid combination at render time: `attribution.enabled=true` without
  `queue.redis.addr` fails `helm install`/`upgrade` with an actionable message (attributed-author mode
  cannot run without Redis) instead of crash-looping the pod.
- `quickstart.namespace` now defaults to `gitops-reverser-quickstart-demo` (was `default`), and a new
  `quickstart.createNamespace` (default `false`) controls whether the chart creates it.

**Migration**

- To keep the previous behavior, set the values explicitly: `--set queue.redis.addr=valkey:6379
  --set queue.redis.auth.existingSecret=valkey-auth --set servers.admission.enabled=true`.
- `helm upgrade --reuse-values` preserves your existing settings, so reused-value upgrades are
  unaffected; only fresh installs (or upgrades that re-specify values) pick up the new defaults.

## Unreleased — API group version bumped `v1alpha2` → `v1alpha3` (next minor; breaking)

The served API version moved from `configbutler.ai/v1alpha2` to `configbutler.ai/v1alpha3` to
reflect the accumulated schema and status changes on this branch. `v1alpha2` is **removed**, not
co-served — there is no conversion webhook, so the old version stops being recognized once the new
CRDs are applied.

**Migration**

- Update every manifest, GitOps source, and client to `apiVersion: configbutler.ai/v1alpha3`
  (`GitProvider`, `GitTarget`, `WatchRule`, `ClusterWatchRule`, `CommitRequest`). The kinds, field
  names, and semantics are otherwise unchanged from `v1alpha2` except where noted in the entries
  below.
- Re-apply the CRDs (or upgrade the Helm chart), then re-apply your objects under the new
  `apiVersion`. Because the group version changed, existing `v1alpha2` objects are not converted in
  place; recreate them as `v1alpha3`.
- `kubectl` commands that pin the version (`kubectl get gittargets.v1alpha2.configbutler.ai`) must
  switch to `v1alpha3`. Unqualified short names (`kubectl get gittargets`) need no change.

## Unreleased — first-run and status surface cleanup (next minor; breaking)

This branch changes the default install to be easier to try, and it tightens the v1alpha3 status
surface around conditions. Existing installs should check the items below before upgrading.

### 1. Helm installs now start configured-author by default

The chart default for `attribution.enabled` changed from `true` to `false`. A default install no longer
renders the audit receiver Service or audit TLS Secrets, and mirrored-resource commits are authored by
the configured committer identity.

Redis/Valkey is optional in configured-author mode. Set `--redis-addr` to store watch resume cursors (warm
restart); leave it empty to cold-replay from scratch on restart. Attributed-author mode still requires a
non-empty `--redis-addr`.

**Migration**

- If you want the easier configured-author install, no chart value is needed.
- If you currently rely on kube-apiserver audit delivery for named commit authors, set:

  ```yaml
  attribution:
    enabled: true
  ```

  Then re-run `helm get notes <release> -n <namespace>` and verify your kube-apiserver audit webhook
  kubeconfig still points at the rendered audit Service.

### 2. `CommitRequest.spec.delaySeconds` became `closeDelaySeconds`

`CommitRequest.spec.delaySeconds` was renamed to `spec.closeDelaySeconds` to describe what the field
does: after the request author is known, the worker waits this long before closing the matching open
commit window.

**Migration**

Before:

```yaml
spec:
  targetRef:
    name: example-target
  delaySeconds: 2
```

After:

```yaml
spec:
  targetRef:
    name: example-target
  closeDelaySeconds: 2
```

Because the old field is no longer in the CRD schema, server-side validation rejects it when strict
field validation is enabled. Update manifests, UI payloads, and tests that create `CommitRequest`
objects.

### 3. `CommitRequest.status.phase` moved to conditions

`CommitRequest.status.phase`, `reason`, `message`, and `observedTime` were removed. Automation should
read conditions instead.

The common replacements are:

| Old check | New check |
| --- | --- |
| `.status.phase == "Committed"` | `Ready=True` with reason `Committed`; `Pushed=True`; read `status.sha` |
| `.status.phase` benign no-commit values | `Ready=True` with reason `NoWindowInGrace`, `WindowMismatch`, or `AlreadyPresent` |
| failed finalize phase/reason | `Ready=False` with reason `FinalizeFailed`; `Stalled=True` |
| old `Attributed` condition | `AuthorAttributed` condition |

Use:

```bash
kubectl wait --for=condition=Ready commitrequest/<name> -n <namespace> --timeout=120s
kubectl get commitrequest/<name> -n <namespace> -o jsonpath='{.status.sha}'
```

`AuthorAttributed=True` with reason `AttributedFromAdmission` means the internal commands admission
webhook captured the submitter. `AuthorAttributed=False` with reason `CommitterFallback` is a valid
fallback, not a failed request.

### 4. `GitTarget.status.phase` and materialization rollups moved to stream conditions

`GitTarget.status.phase` and the old materialization status fields were replaced by condition-first
status plus a bounded `status.streams` summary.

The main automation replacements are:

| Old check | New check |
| --- | --- |
| target phase/current-style checks | `Ready=True` |
| materialization or source-liveness checks | `StreamsRunning=True` and `status.streams` |
| human-fixable blocks | `Stalled=True`, with domain conditions such as `GitPathAccepted=False` |

For workflows that must wait until live watch events are flowing, use:

```bash
kubectl wait --for=condition=StreamsRunning=true gittarget/<name> -n <namespace> --timeout=120s
```

`WatchRule` and `ClusterWatchRule` use the same condition vocabulary for source readiness
(`StreamsRunning`) and referenced target readiness (`GitTargetReady`).

## Unreleased — Config flag naming pass (next minor; breaking)

Controller command-line flags were renamed to follow
[`config-flag-conventions.md`](config-flag-conventions.md). The Helm chart and the
bundled `config/` manifests were updated in lockstep, so **chart/manifest users
who don't override these flags need no action.** Direct-binary users and anyone
templating their own manifests must adopt the new names:

| Old flag | New flag |
| --- | --- |
| `--admission-webhook-enabled` | `--admission-webhook` |
| `--admission-webhook-port=N` | `--admission-webhook-bind-address=:N` |
| `--audit-listen-address=H` + `--audit-port=N` | `--audit-bind-address=H:N` |
| `--branch-buffer-max-bytes` (env `BRANCH_BUFFER_MAX_BYTES`) | `--branch-buffer-max-size` (env `BRANCH_BUFFER_MAX_SIZE`) |
| `--redis-tls` | `--redis-insecure` (see below) |

**Behavioural change — Redis now defaults to TLS.** `--redis-tls` (opt *in* to
TLS) became `--redis-insecure` (opt *out* of TLS), so the binary now connects to
Redis/Valkey over TLS unless told otherwise. The Helm chart
(`queue.redis.tls.enabled: false`) and the `config/` manifests pass
`--redis-insecure` automatically, so default installs keep talking plaintext to an
in-cluster Valkey. **If you run the controller directly against a plaintext Redis,
add `--redis-insecure`** — otherwise startup fails on a TLS handshake.

## Unreleased — Git credentials interop (next minor; breaking)

Two user-visible breaking changes land together. Both come from
[`design/git-credentials-interop.md`](finished/git-credentials-interop.md).

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
