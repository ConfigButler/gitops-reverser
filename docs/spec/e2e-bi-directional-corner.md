# E2E Design: The Bi-Directional Corner (Flux + Argo CD)

> **spec** — current behaviour. The code depends on this document; change one, change the other. Index: [`../INDEX.md`](../INDEX.md)

## Status

Implemented. This document is the design for:

1. a new opt-in e2e **category** that is the only place Argo CD is installed,
2. moving the existing Flux bi-directional spec into it,
3. a new Argo CD bi-directional spec that pins Argo's *exact* observed behaviour
   when combined with GitOps Reverser,
4. a resulting change to `internal/sanitize`,
5. a Gitea → Argo CD **webhook** so pushes reconcile without waiting for the poll
   (see [Push-triggered reconciliation](#5-push-triggered-reconciliation-the-webhook)).

## Motivation

Two problems, one shape.

**Cost.** Argo CD is a 4-to-7-workload control plane. Today every e2e cluster
already carries Flux, because the shared e2e dependencies (Gitea, Valkey,
Prometheus, cert-manager) are delivered as Flux `HelmRelease`s from
[`test/e2e/setup/flux/`](../../test/e2e/setup/flux/) — Flux is load-bearing and
cannot be removed. Argo CD is *not* load-bearing: only bi-directional tests need
it. Installing it in all four CI legs would tax every leg to serve two specs.

**Cohesion.** [`docs/bi-directional.md`](../bi-directional.md) currently says, under
"What is not complete yet":

> equivalent alignment patterns for Argo CD or other GitOps operators

That gap is not closed by prose. It is closed by an e2e that drives a real Argo CD
and records what actually happens. That test needs a home.

So: one new category, one Argo CD install, both bi-directional specs inside it.

## Verified upstream facts

Everything below was read out of the local upstream checkout at
`external-sources/argo-cd` (untracked; `master`, `VERSION` = **3.5.0-dev**,
`git describe` = `v0.8.0-10466-g3cbae653d`). Cited as `argo-cd <path>:<line>`.
These are *not* assumptions — the test design depends on them, so they were
checked in source.

**The install is pinned to `v3.4.5`**, the latest actual release (`v3.5.0` is
master's next version and has no tag). Every fact in the tables below was
re-verified at `v3.4.5` before pinning; line numbers cited are master's, and the
constants are unchanged between the two.

### Resource tracking

| Fact | Evidence |
| --- | --- |
| Default `application.resourceTrackingMethod` is **`annotation`** (not `label`) | `util/settings/settings.go:894-904` |
| Annotation key is `argocd.argoproj.io/tracking-id` | `common/common.go:215` |
| Value format is `<app>:<group>/<Kind>:<namespace>/<name>` | `util/argo/resource_tracking.go:271-273` |
| The **repo-server** stamps this onto **every rendered non-CRD manifest**, unconditionally | `reposerver/repository/repository.go:2144-2153` |
| `GetAppName()` parses the annotation and returns the embedded app name **without ever checking** that the embedded group/kind/namespace/name match the object it was read from | `util/argo/resource_tracking.go:88-116`, `69-86` |
| A live object whose tracked app name differs from the comparing app raises `SharedResourceWarning` | `controller/state.go:1043-1064` |
| `SharedResourceWarning` **fails the sync** — `"Shared resource found: …"` | `controller/sync.go:169-173` |

The last three lines together are the interesting part. See
[The tracking-id landmine](#the-tracking-id-landmine).

### Apply, drift, and reconciliation

| Fact | Evidence |
| --- | --- |
| Default apply is **client-side** (writes `kubectl.kubernetes.io/last-applied-configuration`); server-side apply is opt-in per app or per resource | `gitops-engine/pkg/sync/sync_context.go:1434-1435`, `gitops-engine/pkg/sync/common/types.go:34` |
| `syncPolicy.automated.selfHeal` defaults to **false** | `pkg/apis/application/v1alpha1/types.go:1615-1616` |
| Self-heal backoff: initial **2s**, factor 3, cap 300s. The fixed `--self-heal-timeout-seconds` defaults to 0 and is unused when backoff is active | `cmd/argocd-application-controller/commands/argocd_application_controller.go:311-317` |
| Default reconciliation interval is **120s** (+60s jitter), *not* 180s | same file, `:45`, `:47`; `docs/operator-manual/argocd-cm.yaml:341,348` |
| Sync comparison is a **three-way merge**, so a live-only field absent from Git does **not** cause `OutOfSync` | `gitops-engine/pkg/diff/diff.go:764-797`; decision at `controller/state.go:1285-1305` |
| `RespectIgnoreDifferences=true` copies live values of ignored fields into the target *before* apply, so a sync will not reset them | `controller/sync.go:250-261` |
| Refresh is requestable via annotation `argocd.argoproj.io/refresh: normal\|hard`; the controller deletes the annotation when done | `pkg/apis/application/v1alpha1/types.go:545-546`; `controller/appcontroller.go:2329,2454` |

**Self-heal is ~60× faster than refresh.** Drift is healed on a 2s backoff; a new
Git revision is noticed on a 120s poll. Whatever GitOps Reverser writes to Git,
self-heal will overwrite the cluster from the *stale cached revision* long before
Argo looks at the new commit. This is the causality failure from
[`bi-directional.md`](../bi-directional.md#there-are-no-timing-guarantees),
except Argo's numbers make it a near-certainty rather than a race.

### Install shape

The full Argo CD control plane is 7 workloads: `argocd-application-controller`
(StatefulSet), `argocd-repo-server`, `argocd-server`, `argocd-redis`,
`argocd-applicationset-controller`, `argocd-dex-server`,
`argocd-notifications-controller`.

The corner runs six of them — everything except `dex` (SSO only; local admin login
works without it) and `notifications` (nothing subscribes), both switched off in
`values.yaml`. The **ApplicationSet controller stays on** (the chart's default),
and every running component exports Prometheus metrics via a ServiceMonitor.

Other keys, verified:

- Plain-HTTP UI: `argocd-cmd-params-cm` key **`server.insecure: "true"`** →
  env `ARGOCD_SERVER_INSECURE` (`manifests/base/server/argocd-server-deployment.yaml:37-42`).
- Initial admin password: `argocd-server` generates a random 16-char password
  into Secret **`argocd-initial-admin-secret`**, key **`password`**
  (`util/settings/settings.go:529-531`, `2447-2494`).
- Repository credentials: Secret in the Argo namespace labelled
  **`argocd.argoproj.io/secret-type: repository`** (`common/common.go:198`).

### GitOps Reverser side

| Fact | Evidence |
| --- | --- |
| `Sanitize()` preserves `metadata.labels` and `metadata.annotations` (filtered) | [`internal/sanitize/sanitize.go:37-45`](../../internal/sanitize/sanitize.go#L37-L45) |
| Stripped **label** prefixes: `kustomize.toolkit.fluxcd.io/`, `kro.run/`, `applyset.kubernetes.io/` | [`internal/sanitize/types.go:69-73`](../../internal/sanitize/types.go#L69-L73) |
| Stripped **annotation** prefixes: `kubectl.kubernetes.io/`, `control-plane.alpha.kubernetes.io/`, `deployment.kubernetes.io/`, `autoscaling.alpha.kubernetes.io/`, `kustomize.toolkit.fluxcd.io/`, `applyset.kubernetes.io/` | [`internal/sanitize/types.go:75-82`](../../internal/sanitize/types.go#L75-L82) |
| **No Argo CD key was stripped, at all.** The strip lists were hardcoded prefix matches; there is no runtime config | same |

Line numbers above describe the code as it stood before this change.

So `kubectl.kubernetes.io/last-applied-configuration` (which Argo's client-side
apply writes) *was* stripped — good. And `argocd.argoproj.io/tracking-id` (which
Argo's repo-server writes) *was not* — it was committed to Git as if it were user
intent.

Note the asymmetry: Flux's equivalent stamp, `kustomize.toolkit.fluxcd.io/*`, was
already stripped. Argo's was not. That is the bug this corner exists to surface,
and [the sanitize change below](#consequent-code-change-internalsanitize) fixes it.

## The tracking-id landmine

Chain of events, all steps individually verified above:

1. A clean manifest sits in Git — no Argo annotations.
2. Argo's repo-server renders it and stamps
   `argocd.argoproj.io/tracking-id: app-a:example.com/IceCreamOrder:ns-a/order-1`.
3. Argo applies it. The **live** object now carries the annotation.
4. GitOps Reverser observes the live object, sanitizes it (`last-applied-configuration`
   removed, tracking-id **kept**), and commits it.
5. Git now contains a manifest carrying a provenance string naming app `app-a`,
   namespace `ns-a`, resource `order-1`.

Within a single Argo Application this **converges** and is merely noisy: the
repo-server re-stamps the same value on every render, so target and live agree and
no further commits occur. Git is polluted, but nothing breaks.

It breaks the moment that committed manifest reaches a cluster through **anything
other than Argo's repo-server** — `kubectl apply`, Flux, a promotion pipeline, or
any tool that applies the committed file into a second cluster. Those tools apply
the file *verbatim*, tracking-id and all. The live object now claims to belong to
`app-a`.

When an Argo Application `app-b` later manages that object:

- `GetAppName(liveObj)` returns `app-a`, because the embedded `ns-a/order-1`
  identity is **never checked** against the object (`resource_tracking.go:88-116`).
- `app-a != app-b` → `SharedResourceWarning` (`controller/state.go:1043-1064`).
- Sync fails: `"Shared resource found: IceCreamOrder/order-1 is part of
  applications … and app-a"` (`controller/sync.go:169-173`).

Promoting a manifest between environments is precisely this repository's headline
workflow. Committing the tracking-id arms a mine under it.

## Design

### 1. The category

The cost problem is a *task-graph and CI-leg* problem, not a directory problem.
Argo CD lands only on the cluster of the leg that asks for it. Nothing needs to
move between Go packages.

Follow the established opt-in pattern already used by `demo`, `quickstart-framework`,
and `playground` — Ginkgo label + env skip-gate + dedicated Taskfile target +
dedicated CI leg:

| Piece | Value |
| --- | --- |
| Category label | `bi-directional` (already exists on the Flux spec) |
| Sub-labels | `flux`, `argocd`, `provenance` |
| Env gate | `E2E_ENABLE_BI_DIRECTIONAL` |
| Task | `task test-e2e-bi-directional` |
| CI leg | `bi-directional` |

### The third spec: `provenance_markers_e2e_test.go`

The corner hosts a third spec, and it is the reason the corner is worth more than the
two bi-directional ones. Because this is the only cluster carrying **Flux,
flux-operator, and Argo CD at once**, it is the only place that can ask what each
GitOps producer stamps on the objects it creates — and therefore the only place that
can answer *"does this live object have a home in Git?"* empirically rather than by
reading upstream source.

[`provenance_markers_e2e_test.go`](../../test/e2e/provenance_markers_e2e_test.go)
drives five producers off **one Gitea repo and one commit**, and asserts the marker,
the (absent) `ownerReference`, and the applying field manager on each result:

| Producer | Marker | Source | Home in Git? |
| --- | --- | --- | --- |
| Flux `Kustomization` | `kustomize.toolkit.fluxcd.io/*` | a folder | **yes** |
| Argo CD `Application` | `argocd.argoproj.io/tracking-id` | a folder | **yes** |
| Flux `HelmRelease` | `helm.toolkit.fluxcd.io/*` | a chart | no |
| flux-operator `ResourceSet` | `resourceset.fluxcd.controlplane.io/*` | a template in the CR | no |
| Argo CD `ApplicationSet` | **`ownerReference`** | a generator | no |

It found that the `derived`-object gate the design docs specify — *"evidence: a
controller `ownerReference`"* — catches **one of those five**. It drives no
`GitTarget` and no `WatchRule`: the reverser is not under test, the ecosystem is. The
results are written up in
[`../facts/expansion-provenance-markers.md`](../facts/expansion-provenance-markers.md).

It shares the `argocd` namespace with the Argo CD spec but never patches
`argocd-secret` and owns its own repo, namespace, and resource names, so it runs in
parallel with both. `BI_DIRECTIONAL_GINKGO_PROCS` is therefore `3`.

Filter changes so the default suite stops paying for it:

- `test/e2e/Taskfile-e2e.yml`: `E2E_LABEL_FILTER` default `!image-refresh`
  → `!image-refresh && !bi-directional`
- `.github/workflows/ci.yml`, `full-core` leg: `!manager && !image-refresh`
  → `!manager && !image-refresh && !bi-directional`

`full-manager` (`manager`) is unaffected — the bi-directional specs carry no
`manager` label.

> **Deliberately not doing:** moving the specs to `test/e2e/bidirectional/` as a
> separate Go package. Every helper they use (`SetupRepo`, `kubectlRunInNamespace`,
> `verifyResourceStatus`, `applyFromTemplate`, …) is unexported in `package e2e`.
> A package split means extracting ~30 helpers into `test/e2e/framework` and
> touching every existing spec, for zero effect on the Argo CD install cost that
> motivated this. If the corner grows a third GitOps engine, revisit.

### 2. Argo CD installation

Directory `test/e2e/setup/argocd/` (just `values.yaml` + `README.md`), installed by
the Taskfile node `_argocd-installed` using the official **`argo-helm` `argo-cd`
chart**. `_argocd-installed` `helm pull`s the pinned chart into
`.stamps/cluster/<ctx>/argocd/` (retryable network step) and `helm upgrade
--install`s it with `values.yaml` and `--wait`, then gates on CRD establishment.

- `ARGOCD_CHART_VERSION` (e.g. `10.1.3`) lives in `test/e2e/Taskfile-e2e.yml` and pins
  the chart, which carries the Argo CD release the specs pin behaviour against (its
  appVersion, e.g. `v3.4.5`). Unlike `FLUX_VERSION` it is not a devcontainer env
  var: nothing outside the cluster needs an `argocd` binary — the specs drive Argo
  through `kubectl` alone — so pinning it in the Taskfile avoids a container rebuild.
- **Why `helm`, not a flat `kubectl apply`.** The chart's `redis-secret-init` runs
  as a pre-install **hook** that must complete before redis and the controllers
  start (they read `REDIS_PASSWORD` from the secret it writes); helm's hook ordering
  handles that. Helm also sees the live cluster's API surface, so the chart emits
  the ServiceMonitors (it gates them on `monitoring.coreos.com` being present).
- **Components.** `values.yaml` disables `dex` (SSO; local admin works without it)
  and `notifications` (nothing subscribes); the **ApplicationSet controller stays
  on** (chart default). Prometheus metrics + ServiceMonitors are enabled for the
  four Argo components, labelled `monitoring.configbutler.ai/instance: gitops-reverser`
  so the corner's shared Prometheus selects them.
- **Defaults are load-bearing.** `values.yaml` deliberately does not set
  `application.resourceTrackingMethod` (stays `annotation`) nor enable server-side
  apply globally — the specs assert those defaults.
- The stamp `.stamps/cluster/<ctx>/argocd.installed` records the chart version, so
  a bump re-installs; a warm cluster otherwise re-runs it for free.
- `deps: [_services-ready]` — the Argo `Application` sources from Gitea, which Flux
  installs; `_services-ready` also guarantees the `monitoring.coreos.com` CRDs that
  the ServiceMonitors need.
- **Only** `test-e2e-bi-directional` depends on `_argocd-installed`. `prepare-e2e`
  does not. The suite's own in-process `task prepare-e2e` call
  (`e2e_suite_test.go:197`) stays untouched and no-ops on the warm stamp.

Deliberately **not** a Flux `HelmRelease` alongside gitea/valkey, even though that
is the local idiom: `hack/e2e/wait-flux-services.sh` waits on every `HelmRelease`
in every namespace, which would drag Argo CD into `_flux-setup-ready` — i.e. back
into every cluster. A standalone `helm` install off one Taskfile node keeps the
blast radius at one node of the graph.

Repository credentials are created by the spec at runtime (not committed): a
Secret in the `argocd` namespace labelled
`argocd.argoproj.io/secret-type: repository`, with `type/url/username/password`
read from the Gitea credential Secret the existing `SetupRepo` helper already
produces.

### 3. The GUI

`task argocd-ui`:

1. waits for `argocd-server` to be Ready,
2. starts a detached port-forward on **`ARGOCD_PORT=18080`** → `svc/argocd-server:80`,
   reusing the `setsid` + ready-pod-wait + TCP-probe pattern from
   [`hack/e2e/setup-port-forwards.sh`](../../hack/e2e/setup-port-forwards.sh)
   (ports 13000/19090/16379/19080/18081 are taken; 18080 is free),
3. prints the URL and the admin password:

```text
Argo CD UI:  http://localhost:18080
username:    admin
password:    <kubectl -n argocd get secret argocd-initial-admin-secret \
                -o jsonpath='{.data.password}' | base64 -d>
```

Password stays **generated**, never committed — `argocd-server` writes it to
`argocd-initial-admin-secret` on first start. `server.insecure: "true"` means no
TLS, so the port-forward works without `--insecure` gymnastics in the browser.

`argocd-ui` is *not* wired into `prepare-e2e` or the CI leg. The specs drive Argo
entirely through `kubectl` (and the webhook), so **no `argocd` CLI is added to the
devcontainer**:

| Operation | kubectl equivalent |
| --- | --- |
| refresh | a push fires the webhook (§5); no manual refresh needed |
| sync | GitOps Reverser sets `.operation` itself; the specs patch it by hand only to deploy the refusal spec's overlay before any `GitTarget` names it |
| read state | `.status.sync.status`, `.status.sync.revision`, `.status.operationState.phase`, `.status.conditions` |

### 4. What moves

| From | To |
| --- | --- |
| `test/e2e/bi_directional_e2e_test.go` | `test/e2e/flux_bi_directional_e2e_test.go`, `Label("bi-directional", "flux")`, plus the `E2E_ENABLE_BI_DIRECTIONAL` skip gate |
| — | `test/e2e/argocd_bi_directional_e2e_test.go`, `Label("bi-directional", "argocd")` |
| — | `test/e2e/templates/bi-directional/argocd-application.tmpl`, `argocd-repo-secret.tmpl` |

Per the existing per-file CRD-group convention, add
`crdGroupArgoBiDirectional = "argo-bi-directional.e2e.example.com"` to
[`test/e2e/icecream.go`](../../test/e2e/icecream.go) and register it in the
suite's CRD pre-clean loop (`e2e_suite_test.go:225`).

The Flux spec's body does not change. It keeps its exact-commit-count discipline,
which works because it drives Flux as a manually triggered applier.

### 5. Push-triggered reconciliation (the webhook)

Mirrors the Flux receiver webhook (`ensureRepoWebhook` in `repo_setup.go`) so
Argo notices a push immediately instead of waiting for its timed reconciliation.
Argo's model is simpler than Flux's — one endpoint, no per-repo `Receiver` CR:

- `argocd-server` exposes `POST /api/webhook`
  (`server/server.go`, `util/webhook/webhook.go`).
- The corner sets `webhook.gogs.secret` on the cluster-global `argocd-secret`
  (a merge patch; argocd-server watches the Secret and reloads), and creates a
  Gitea webhook on the repo pointing at
  `http://argocd-server.argocd.svc.cluster.local/api/webhook` with the same
  secret and the `push` event. Both live in the spec (`configureArgoWebhookSecret`,
  `ensureArgoWebhook`), not the Taskfile, so no install-stamp staleness.
- Gitea's `type: "gitea"` hook emits `X-Gogs-*` headers (including an
  HMAC-SHA256 `X-Gogs-Signature`) alongside its native `X-Gitea-*` ones, so
  Argo's **Gogs** parser accepts the payload and validates the signature against
  `webhook.gogs.secret`.

**The one load-bearing prerequisite — host match.** Argo decides which
Applications to refresh with an anchored **host + path** regex, compiled from the
payload's `repository.html_url` (derived from Gitea's `ROOT_URL`) and tested
against `app.spec.source.repoURL` (`util/webhook/webhook.go`, `GetWebURLRegex` /
`sourceUsesURL`). The regex tolerates the `:13000` port and `.git` suffix but the
**host must be identical**. The e2e Gitea sets
`ROOT_URL: http://gitea-http.gitea-e2e.svc.cluster.local:13000/`
(`test/e2e/setup/flux/values/gitea-values.yaml`), which is exactly the app repo
host — so it matches. A `localhost` ROOT_URL would silently fail to match and no
app would refresh; that is why this is called out.

A matched webhook triggers a **`normal`** refresh (`argo.RefreshApp` with
`RefreshTypeNormal`). The specs run the standard (automated sync off), so the
webhook only refreshes: it makes Argo CD compare sooner, and the specs prove it
never syncs. Every sync is one GitOps Reverser starts, learning of a push through
its own receiver, which the corner registers as a second Gitea webhook. Only Spec E
turns automated sync on, and there the webhook's refresh does auto-sync.

## Test design: `argocd_bi_directional_e2e_test.go`

Every spec runs the standard of [`docs/argocd.md`](../argocd.md): the `Application`
has automated sync off (`automated: {enabled: false, prune: true}`) and is named by
the `GitTarget`'s `spec.applier.applicationRef`, so every sync it runs is one GitOps
Reverser started, pinned to a commit it checked. The earlier phases that ran
automated sync (`selfHeal: true` losing the edit, `ignoreDifferences`, and
`selfHeal: false` with the webhook) are gone with it: they characterized an Argo CD
that synced on its own, which the standard no longer allows.

Specs A, B and E are three `It`s of one `Ordered` describe, sharing one Gitea repo,
one `IceCreamOrder` CRD (`applyIceCreamCRD(crdGroupArgoBiDirectional)`) and one
`Application`, with one order per spec. Two Applications pointed at the same live
path would each stamp their own tracking-id on the same objects and raise
`SharedResourceWarning`. Spec C has its own describe, repo, namespace and
`Application`.

The `Application` lives in the `argocd` namespace, targets
`spec.destination.namespace = <testNs>`, `spec.source.path = <livePath>`,
`targetRevision: main`, `directory.recurse: true`. The `GitProvider` has a
`gitea` receiver, registered as a Gitea webhook next to Argo CD's own.

Ordering matters: `SetupRepo` leaves the Gitea repo **empty**, so the spec seeds
`main` first, creates the `Application` (nothing to sync yet), and only then stands
up the reverser pipeline, whose `GitTarget` needs the `Application` to report
`ApplierConfigured=True`.

### Spec A — a Git-side push is synced through a pinned `ForeignCommit`

1. Assert Argo CD has run **no operation**: GitOps Reverser's own bootstrap push,
   and its start, ask for nothing.
2. Push a clean `order-1` from the Git side, and nothing else.
3. Assert the `Application`'s operation state is GitOps Reverser's: initiated by
   `gitops-reverser` with `automated: true`, reason `ForeignCommit` in its info,
   `sync.revision` and `syncResult.revision` both the pushed commit, `Succeeded`,
   with resources. The `GitTarget`'s `status.applier` names the same commit.
4. Assert the **live** object carries `argocd.argoproj.io/tracking-id` (exact
   value format) and `kubectl.kubernetes.io/last-applied-configuration`, and the
   **committed file** carries neither, nor `managedFields` or `resourceVersion`.
5. Assert, with `Consistently`, that the sync produced **zero commits**.

Step 5 is the strongest form of the bookkeeping assertion, and it only holds
*because* of the sanitize fix: if sanitization is complete, the sanitized live
object is byte-equal to the file already in Git, so the Reverser has nothing to
write. The commit count is a sharper detector of a metadata leak than any
substring assertion — it catches keys nobody thought to name.

### Spec B — two quick API edits keep the later value

The race the apply trigger exists for.

1. Push `order-4` from the Git side; it is synced by a `ForeignCommit`.
2. Patch `container: Cup` through the API, wait for its commit, then patch
   `container: WaffleBowl` and wait for that commit.
3. `Consistently` assert live stays `WaffleBowl`, the commit count is exactly
   baseline + 2, and the `Application`'s last `finishedAt` has not moved: GitOps
   Reverser's own commits owe Argo CD no sync.
4. Assert Argo CD, refreshed by its webhook, reports the `Application` `Synced`:
   there was nothing to catch up.

### Spec C — a refused edit is reverted by an Argo CD sync

Its own describe, repo and namespace. Corpus shape 8, as in the Flux refusal
spec: an overlay whose `Deployment` comes from `../../base`, and a live edit to an
env var no overlay declaration can express.

1. Seed the base and overlay, create the `Application` on the overlay
   (kustomize), and deploy it with one manual sync, before any `GitTarget` names it.
2. Drift `LOG_LEVEL` to `debug`.
3. Create the `GitTarget` on the overlay with `applicationRef` and
   `onRefusal: Reapply`, and a `WatchRule` on `deployments`.
4. Assert the write is refused at the boundary (`WriteBoundaryRefused`), and that
   GitOps Reverser's `Refusal` sync, pinned to the tip, reverts `LOG_LEVEL` to
   `info`, with no commit for it.

### Spec E — an `Application` with automated sync on is never synced

1. Re-apply the `Application` with automated sync on (an `automated` block with no
   `enabled`), and request a `GitTarget` reconcile, since the applier is read on
   reconcile rather than watched.
2. Assert `ApplierConfigured=False` with `AutomatedSyncEnabled`, and `Stalled`.
3. Push a Git-side change. Assert an `ApplyRequestFailed` Event: the `ForeignCommit`
   was decided and refused at the `Application`.
4. `Consistently` assert no operation initiated by `gitops-reverser` names the
   pushed commit, and `status.applier` is unchanged. Argo CD's own automated sync
   applies the push; that is not GitOps Reverser's.

### Spec D — arming the landmine (phase 2)

Proves the tracking-id leak is a correctness bug, not cosmetics.

1. Take the file Spec A committed — it carries `app-a`'s tracking-id.
2. `kubectl apply` it into a second namespace, standing in for "promoted by
   anything that is not Argo's repo-server".
3. Point a second `Application` `app-b` at a path rendering that object.
4. Assert `app-b.status.conditions` gains `SharedResourceWarning` naming `app-a`.
5. Assert a sync of `app-b` fails with `"Shared resource found"`.

After the sanitize fix, Spec D inverts: the committed file has no tracking-id,
step 2 cannot arm anything, and `app-b` syncs clean. It becomes the regression
test for the fix.

Marked phase 2: steps 4-5 are derived from source reading
(`state.go:1043-1064` → `sync.go:169-173`), and an e2e exists precisely to confirm
that derivation empirically. Land Specs A-C first.

### Out of scope: SOPS

The Flux spec round-trips a SOPS-encrypted `Secret` because Flux `Kustomization`
has a native `spec.decryption.provider: sops`. Argo CD has no built-in SOPS
decryption — it needs a Config Management Plugin (ksops, argocd-vault-plugin).
The Argo spec therefore covers `IceCreamOrder` CRs only, and
[`bi-directional.md`](../bi-directional.md) should state this asymmetry plainly:
**encrypted-Secret round-trip is a Flux-only capability today.**

## Consequent code change: `internal/sanitize`

*Implemented.*

Argo's tracking annotation is controller bookkeeping and must not reach Git.

The fix is **not** a `argocd.argoproj.io/` prefix strip. Several annotations under
that prefix are legitimate user intent that belongs in Git:
`argocd.argoproj.io/sync-wave`, `sync-options`, `compare-options`, `hook`.
Stripping the prefix would silently delete a user's sync ordering.

`isOperationalAnnotation` is prefix-only today
([`types.go:75-82`](../../internal/sanitize/types.go#L75-L82)); it gains an
exact-key set:

```go
var operationalAnnotationKeys = map[string]struct{}{
 "argocd.argoproj.io/tracking-id":     {},
 "argocd.argoproj.io/installation-id": {},
}

func isOperationalAnnotation(key string) bool {
 if _, ok := operationalAnnotationKeys[key]; ok {
  return true
 }
 return strings.HasPrefix(key, "kubectl.kubernetes.io/") ||
  // … existing prefixes unchanged
}
```

### Label-based tracking is unsupported, and must be documented as such

If Argo is configured with `resourceTrackingMethod: label` or `annotation+label`,
it stamps the label `app.kubernetes.io/instance` (`common/common.go:190`). That key
is indistinguishable from the standard recommended label that Helm and Kustomize
set for entirely legitimate reasons. The Reverser cannot tell bookkeeping from
intent, so it must not strip it.

Recommendation for `bi-directional.md`: **keep Argo CD on the default `annotation`
tracking method for any Reverser-managed path.** Under `label` tracking the
instance label will be committed to Git, and that is not fixable in the sanitizer.

### Adjacent gap found while auditing

No test covers the `kro.run/` label prefix or the `applyset.kubernetes.io/`
prefixes, though both are in the strip list. Also `kro.run/` is stripped from
labels but **not** from annotations, while `applyset.kubernetes.io/` is stripped
from both — an inconsistency that may or may not be intentional. Worth a
table-driven case in `internal/sanitize/types_test.go` while the file is open.

## CI wiring

New matrix entry in `.github/workflows/ci.yml`:

```yaml
- name: bi-directional
  script: "task test-e2e-bi-directional"
  needs_artifact: false
  coverage: "1"
  e2e_ginkgo_procs: "2"
  k3d_agent_count: "0"
```

`coverage: "1"` — the leg uses the default `config-dir` install, which carries the
`GOCOVERDIR` overlay, so Codecov unions its `e2e` upload with the other legs and
the non-regression ratchet still holds.

Per [`e2e-sharding`](../finished/e2e-ci-runner-sharding-plan.md), e2e is **not** a required
check on `main` (only *Project image*, *Unit tests*, *Lint* are), so adding a leg
carries no merge-gating risk. The two specs are pulled out of `full-core`, so that
leg gets faster.

`--procs=2`: the two specs own separate Gitea repos and separate CRD groups, and
neither is `Serial`.

## Risks

| Risk | Mitigation |
| --- | --- |
| `_argocd-installed` `helm pull`s the chart from the argo-helm repo at install time — a network dependency | Retried 3× with an explicit error message. If CI flakes, mirror the `_ghcr-preflight` node as an `_argocd-preflight`, or vendor the chart tarball |
| The chart could ship `NetworkPolicy` objects that k3s enforces | `global.networkPolicy.create` is off by default (we do not enable it), so no NPs are created. Port-forward goes kubelet→pod and bypasses NP anyway |
| Argo's app-controller watches cluster-wide and will observe the Flux spec's objects | Only objects carrying a tracking-id are compared. Harmless, but confirm no `SharedResourceWarning` noise across specs |
| Spec B asserts an exact `+2` commit delta | Deterministic, not timing-derived: the Kubernetes watch delivers every edit, `commitWindow=0` makes each its own commit, and nothing syncs GitOps Reverser's own commits |
| A chart bump may move the tracking defaults | The defaults are asserted directly in Spec A. A behaviour change breaks the spec loudly, which is the point. README lists the re-verify checklist |

## Decisions

1. **Category name: `bi-directional`.** Reuses the label already on the Flux spec
   and matches `docs/bi-directional.md`. If the corner later hosts
   non-bi-directional Argo work (layout discovery, `Application` mirroring),
   `interop` would age better — rename then.
2. **Official Helm chart, don't vendor.** `_argocd-installed` `helm pull`s the
   pinned `argo-helm`/`argo-cd` chart into the cluster stamp dir and `helm upgrade
   --install`s it with `values.yaml`. Vendoring the chart would bloat every diff.
   Helm is required rather than a flat `kubectl apply`: the `redis-secret-init`
   pre-install hook must run before redis. It also makes
   `dex`/`notifications`/ApplicationSet/metrics one-line toggles.
   See `test/e2e/setup/argocd/README.md`.
3. **Specs and fix ship together.** The two-commit story (specs assert the leak,
   then the fix flips them) was collapsed: the e2e now asserts the fixed
   invariant, and `internal/sanitize/types_test.go` carries the deterministic
   unit-level proof. That the test genuinely fails without the fix was verified by
   removing the exact-key set and re-running.

## Not done

- **Spec D**, the SharedResourceWarning demonstration, is not implemented. Its
  premise is verified in source (`resource_tracking.go:88-116` →
  `state.go:1043-1064` → `sync.go:169-173`), and with the sanitize fix in place
  the trap can no longer be armed through this operator. It remains the strongest
  empirical argument for the fix, and is worth adding if the strip list is ever
  questioned.

## References

- [`docs/bi-directional.md`](../bi-directional.md) — the user-facing guidance this closes gaps in
- [`docs/design/e2e-serial-registry.md`](e2e-serial-registry.md) — parallelism and shared cluster state
- [`docs/design/e2e-ci-runner-sharding-plan.md`](../finished/e2e-ci-runner-sharding-plan.md) — leg membership and rebalancing
- [`test/e2e/flux_bi_directional_e2e_test.go`](../../test/e2e/flux_bi_directional_e2e_test.go) and
  [`test/e2e/argocd_bi_directional_e2e_test.go`](../../test/e2e/argocd_bi_directional_e2e_test.go) —
  the two specs in this corner
- [`internal/sanitize/types.go`](../../internal/sanitize/types.go) — the strip lists
