# E2E Design: The Bi-Directional Corner (Flux + Argo CD)

## Status

Implemented. This document is the design for:

1. a new opt-in e2e **category** that is the only place Argo CD is installed,
2. moving the existing Flux bi-directional spec into it,
3. a new Argo CD bi-directional spec that pins Argo's *exact* observed behaviour
   when combined with GitOps Reverser,
4. a resulting change to `internal/sanitize`.

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
[`bi-directional.md`](../bi-directional.md#why-shared-automatic-ownership-breaks-down),
except Argo's numbers make it a near-certainty rather than a race.

### Install shape

`manifests/install.yaml` deploys 7 workloads: `argocd-application-controller`
(StatefulSet), `argocd-repo-server`, `argocd-server`, `argocd-redis`,
`argocd-applicationset-controller`, `argocd-dex-server`,
`argocd-notifications-controller`.

`manifests/core-install.yaml` deploys 4 — but **omits `argocd-server`**, i.e. no
web UI. Since a browsable GUI is a requirement here, core-install is out.

Minimal set that syncs `Application`s *and* serves the UI:
`application-controller` + `repo-server` + `redis` + `server`. Drop `dex`
(SSO only; local admin login works without it), `notifications`, and
`applicationset`.

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

*(Line numbers above describe the code as it stood before this change.)*

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
an intent-cluster hydration step. Those tools apply the file *verbatim*, tracking-id
and all. The live object now claims to belong to `app-a`.

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
| Sub-labels | `flux`, `argocd` |
| Env gate | `E2E_ENABLE_BI_DIRECTIONAL` |
| Task | `task test-e2e-bi-directional` |
| CI leg | `bi-directional` |

Filter changes so the default suite stops paying for it:

- `test/e2e/Taskfile.yml`: `E2E_LABEL_FILTER` default `!image-refresh`
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

New directory `test/e2e/setup/argocd/`, applied by a new Taskfile node
`_argocd-installed`. It mirrors `_flux-installed` exactly, including the
`envsubst` version-pinning trick:

```
test/e2e/setup/argocd/
  kustomization.yaml      # remote base pinned to v${ARGOCD_VERSION}
  namespace.yaml
  cmd-params-cm.yaml      # server.insecure: "true"
  argocd-cm.yaml          # timeout.reconciliation: 30s
  remove-dex.yaml         # $patch: delete
  remove-notifications.yaml
  remove-applicationset.yaml
```

- `ARGOCD_VERSION=3.5.0` joins `FLUX_VERSION` / `FLUX_OPERATOR_VERSION` in
  [`.devcontainer/Dockerfile`](../../.devcontainer/Dockerfile). Per the comment
  already in `test/e2e/Taskfile.yml`, the container env is the single source of
  truth for in-cluster versions.
- `_argocd-installed` renders `*.yaml` through `envsubst '${ARGOCD_VERSION}'`
  into `.stamps/cluster/<ctx>/argocd/`, runs `kubectl apply -k`, then waits on
  the four workloads' rollouts. Stamped at `.stamps/cluster/<ctx>/argocd.installed`,
  so a warm cluster re-runs it for free.
- `deps: [_services-ready]` — the Argo `Application` sources from Gitea, which
  Flux installs.
- **Only** `test-e2e-bi-directional` depends on `_argocd-installed`. `prepare-e2e`
  does not. The suite's own in-process `task prepare-e2e` call
  (`e2e_suite_test.go:197`) stays untouched and no-ops on the warm stamp.

Deliberately **not** a Flux `HelmRelease` alongside gitea/valkey, even though that
is the local idiom: `hack/e2e/wait-flux-services.sh` waits on every `HelmRelease`
in every namespace, which would drag Argo CD into `_flux-setup-ready` — i.e. back
into every cluster. A standalone kustomize dir keeps the blast radius at one node
of the graph.

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

```
Argo CD UI:  http://localhost:18080
username:    admin
password:    <kubectl -n argocd get secret argocd-initial-admin-secret \
                -o jsonpath='{.data.password}' | base64 -d>
```

Password stays **generated**, never committed — `argocd-server` writes it to
`argocd-initial-admin-secret` on first start. `server.insecure: "true"` means no
TLS, so the port-forward works without `--insecure` gymnastics in the browser.

`argocd-ui` is *not* wired into `prepare-e2e` or the CI leg. The specs drive Argo
entirely through `kubectl`, so **no `argocd` CLI is added to the devcontainer**:

| Operation | kubectl equivalent |
| --- | --- |
| refresh | annotate `argocd.argoproj.io/refresh=hard` |
| sync | patch `.operation.sync` on the `Application` |
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

## Test design: `argocd_bi_directional_e2e_test.go`

Four scenarios, sharing one Gitea repo and one `IceCreamOrder` CRD applied via
`applyIceCreamCRD(crdGroupArgoBiDirectional)`.

**They are implemented as three phases of a single `Ordered` `It`, driving one
Application whose `syncPolicy` is re-applied between phases — not as separate
specs with an Application each.** Two Applications pointed at the same live path
would each stamp their own tracking-id on the same objects, and by the very
mechanism this spec documents, each would then see the other's id as foreign and
raise `SharedResourceWarning`. The specs would fight. One Application, one order
per phase.

The Argo `Application` lives in the `argocd` namespace, targets
`spec.destination.namespace = <testNs>`, `spec.source.path = <livePath>`,
`targetRevision: main`, `directory.recurse: true`.

Ordering matters: `SetupRepo` leaves the Gitea repo **empty**, so `main` does not
exist on the remote until something pushes it, and `git pull` cannot fetch a ref
that is not there. The spec therefore seeds the branch first and only then stands
up the reverser pipeline — which also sidesteps the question of whether a
`GitTarget` aimed at an empty repository bootstraps a commit of its own. The Flux
spec depends on the same ordering.

### Spec A — Argo CD's stamps land in Git

`syncPolicy: {}` (manual). This is the characterization test: it records exactly
what Argo writes and exactly what survives sanitization.

1. Commit a clean `order-1` (no Argo metadata). Refresh + sync via `kubectl`.
2. Assert the **live** object has annotation `argocd.argoproj.io/tracking-id`
   equal to `<app>:<group>/IceCreamOrder:<ns>/order-1` — proves default
   `annotation` tracking, exact value format.
3. Assert the **live** object has `kubectl.kubernetes.io/last-applied-configuration`
   — proves client-side apply is the default.
4. Assert the **committed file** contains none of it: no
   `argocd.argoproj.io/tracking-id`, no `kubectl.kubernetes.io/last-applied-configuration`,
   no `managedFields`, no `resourceVersion`.
5. Assert, with `Consistently`, that the Argo sync produced **zero commits**.

Step 5 is the strongest form of the assertion, and it only holds *because* of the
sanitize fix. If sanitization is complete, the sanitized live object is byte-equal
to the file already in Git, so the Reverser has nothing to write. Before the fix
the tracking-id leaked, the sanitized object differed from Git, and the count grew
by one. The commit count is therefore a sharper detector of a metadata leak than
any substring assertion — it catches keys nobody thought to name.

(The same shape is what lets the Flux spec assert exact counts: Flux's stamps are
stripped too, so its applies are commit-neutral.)

### Spec B — `selfHeal: true` destroys the API-side change

`syncPolicy.automated: {prune: true, selfHeal: true}`.

1. Git holds `order-2` with `container: Cone`. Sync. Live = `Cone`.
2. Patch the live object through the Kubernetes API: `container: WaffleBowl`.
3. `Eventually` assert live is back to **`Cone`**, then `Consistently` assert it
   stays `Cone`.
4. `Eventually` assert the committed file is back to `Cone`; then `Consistently`
   assert the commit count is stable.

Expected timeline, from the verified constants: Reverser commits `WaffleBowl` in
well under a second; self-heal fires at ~2s and replays the stale cached revision;
Reverser then observes the revert and commits `Cone`; at the 120s refresh Argo
finds `Cone` in Git and is already Synced. **The user's change is lost, and Git
history flaps.**

Assert only the terminal invariant, never commit counts. Whether the Reverser
catches the transient `WaffleBowl` before self-heal overwrites it is a genuine
race, and the commit window may coalesce the two events. The terminal state is
deterministic; the intermediate count is not. This is the difference between the
Flux spec (manual applier, exact counts are safe) and this one.

### Spec C — the safe Argo CD recipe

`syncPolicy.automated: {selfHeal: true}` **plus**:

```yaml
ignoreDifferences:
  - group: argo-bi-directional.e2e.example.com
    kind: IceCreamOrder
    jsonPointers: ["/spec/scoops"]
syncOptions: ["RespectIgnoreDifferences=true"]
```

1. Git holds `order-3` with `scoops: [{Vanilla, 2}]`. Sync.
2. API-patch `scoops: [{MintChip, 4}]`.
3. `Consistently` assert live **stays** `MintChip` despite `selfHeal: true` —
   the field is ignored, so no drift, so no heal.
4. Assert the `Application` remains `Synced`.
5. Assert the Reverser commits `MintChip`.
6. After an Argo refresh, assert still `Synced`, still `MintChip`, commit count stable.

This is the Argo CD analogue of split ownership (mode 3 in
[`bi-directional.md`](../bi-directional.md#3-split-ownership)) and the concrete
answer to the "equivalent alignment patterns for Argo CD" gap. Contrast with
Spec B — *the only difference is `ignoreDifferences`, and it is the difference
between losing the change and keeping it.*

`RespectIgnoreDifferences=true` is belt-and-braces rather than load-bearing here:
because the Reverser keeps Git current, target and live agree anyway. It closes
the window in which an unrelated Git change triggers a sync *between* the API
patch and the Reverser's commit, which would otherwise reset the field
(`controller/sync.go:250-261`). Documented, not separately asserted.

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

Per [`e2e-sharding`](e2e-ci-runner-sharding-plan.md), e2e is **not** a required
check on `main` (only *Project image*, *Unit tests*, *Lint* are), so adding a leg
carries no merge-gating risk. The two specs are pulled out of `full-core`, so that
leg gets faster.

`--procs=2`: the two specs own separate Gitea repos and separate CRD groups, and
neither is `Serial`.

## Risks

| Risk | Mitigation |
| --- | --- |
| `_argocd-installed` fetches `manifests/install.yaml` from `raw.githubusercontent.com` at install time — a network dependency | `curl --retry 3` plus an explicit error message. If CI flakes, mirror the `_ghcr-preflight` node as an `_argocd-preflight`, or vendor the pinned manifest |
| ~~`install.yaml` ships `NetworkPolicy` objects, and k3s enforces them~~ | **Resolved.** Every Argo NetworkPolicy declares `policyTypes: [Ingress]` only, so egress is unrestricted and repo-server reaches Gitea. Port-forward goes kubelet→pod and bypasses NP anyway |
| Argo's app-controller watches cluster-wide and will observe the Flux spec's objects | Only objects carrying a tracking-id are compared. Harmless, but confirm no `SharedResourceWarning` noise across specs |
| Spec B is timing-derived | Asserts terminal invariants only, never commit counts. See Spec B rationale |
| Argo CD 3.5.0 is recent; a bump may move the tracking defaults | The defaults are asserted directly in Spec A. A behaviour change breaks the spec loudly, which is the point |

## Decisions

1. **Category name: `bi-directional`.** Reuses the label already on the Flux spec
   and matches `docs/bi-directional.md`. If the corner later hosts
   non-bi-directional Argo work (layout discovery, `Application` mirroring),
   `interop` would age better — rename then.
2. **Fetch, don't vendor.** `_argocd-installed` `curl`s the pinned release into
   the cluster stamp dir. Vendoring 34k lines would bloat every diff, and
   kustomize cannot consume a bare remote file as a resource (it resolves
   `raw.githubusercontent.com` as a git repo). Fetching the *released*
   `install.yaml` also pins the image tag, which the kustomize-native remote base
   would not — upstream's `manifests/base` carries `newTag: latest`.
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
  the trap can no longer be armed through this product. It remains the strongest
  empirical argument for the fix, and is worth adding if the strip list is ever
  questioned.

## References

- [`docs/bi-directional.md`](../bi-directional.md) — the product-level guidance this closes gaps in
- [`docs/design/e2e-serial-registry.md`](e2e-serial-registry.md) — parallelism and shared cluster state
- [`docs/design/e2e-ci-runner-sharding-plan.md`](e2e-ci-runner-sharding-plan.md) — leg membership and rebalancing
- [`test/e2e/bi_directional_e2e_test.go`](../../test/e2e/bi_directional_e2e_test.go) — the Flux spec being moved
- [`internal/sanitize/types.go`](../../internal/sanitize/types.go) — the strip lists
