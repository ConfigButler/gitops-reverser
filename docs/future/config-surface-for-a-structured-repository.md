# The configuration surface, now that a folder has structure

> Status: proposal — review findings and an ordered plan. Nothing here binds until scheduled.
> Date: 2026-07-24
> Absorbs and extends the API-surface block left unbuilt by the status and configuration-model
> review — `spec.suspend`, `spec.interval` and the reconcile-request annotation, the
> `CommitRequest` lifecycle, and the `ClusterWatchRule` scope question — which is now sequenced in
> [`../layout/api-wave.md`](../layout/api-wave.md).

## The one-sentence finding

The product's premise — *the kube-apiserver writes back into Git* — is sound and worth
building on. What has drifted is that **`spec.path` is still one string**, while the folder
it names has quietly acquired structure: a layout, render roots, read-only context, build
directives, an inferred placement convention, and a support verdict. The operator now knows
a great deal about that folder, and the API offers the user **no way to say what they expect,
no way to see what was understood, and no way to look before writing.**

Everything below follows from that one gap.

---

## Part A — Review of `docs/configuration.md` and `docs/architecture.md`

### A1. Factual errors (fixed on this branch)

| Where | Was | Now |
|---|---|---|
| `architecture.md:533` | cluster-scoped resources use the literal `` `cluster/` `` | `` `_cluster/` `` — verified against `internal/manifestanalyzer/placement.go:418`, `internal/types/identifier.go:66`, and `configuration.md:700`, which all say `_cluster` |
| `configuration.md:664-665` | two links, both labelled `design/manifest/…` (a path layout that no longer exists), both resolving to the *same* file, one of them claiming to be a different document ("the vision behind it") | one correctly-labelled link |
| `architecture.md:862` | link label `design/manifest/version2/type-followability.md` | correct label |
| `architecture.md:1281` | "GitTarget lifecycle and repo architecture" linked to `architecture.md` — i.e. to itself | links to the support contract, which is what that list was missing |

### A2. `configuration.md` is stale against the CRD it documents

The GitTarget status list (`configuration.md:515-524`) names `Ready`, `Reconciling`, `Stalled`,
`Validated`, `EncryptionConfigured`, `StreamsRunning`, `GitPathAccepted`. The CRD's own printer
columns (`gittarget_types.go:283-297`) also ship **`RenderMatchesLive`**, **`GitProviderReady`**,
and **`ClusterProviderReady`**. `RenderMatchesLive` is the condition that carries the entire
kustomize-verification story — the newest and least obvious part of the product — and the
configuration guide does not mention it once. A user who hits it has nowhere to read what it means.

### A3. The structural problem: three documents are wearing one trench coat

`configuration.md` is ~1150 lines and is simultaneously

- a **tutorial** (the setup flow, the quickstart pointers),
- a **field reference** (every spec field of six kinds), and
- a **design-rationale record** (why the provider scopes differ, why omitted ≠ empty, why the
  sweep fails closed, why `default` is never auto-created).

The rationale is genuinely good — better than what Flux has written down, as the maintainer
review says — but it is interleaved with the reference at a 1:1 ratio, so the reference is
unusable at speed and the rationale is unfindable on purpose. Concretely, the reader who wants
to answer *"which folder do I point this at?"* has to get through ~460 lines of Git credential
dialects, known-hosts precedence, commit templates and signing before reaching `GitTarget` at all.

Recommendation (not urgent, but do it before there are users):

- `configuration.md` → **reference only**: one section per kind, fields, defaults, a table, an
  example. Rationale moves out to `docs/spec/`, linked inline as "why:".
- A new `docs/onboarding.md` → **the tutorial**: pick a repo, scan it, pick a folder, apply, verify.
  This is where Part B's missing product step lives.
- Credentials interop (`configuration.md:95-136`) is a *guide*, not configuration — it belongs
  beside `github-setup-guide.md`.

### A4. Three surfaces are described as if they were one

`configuration.md`'s "Audit ingestion settings" section (line 1027 onward) freely mixes:

- CRD fields (`ClusterProvider.spec.attribution.auditRoute`),
- Helm values (`attribution.enabled`, `queue.redis.addr`),
- controller flags (`--author-attribution`, `--author-attribution-grace`, `--redis-key-prefix`),

with no visual marker for which is which. `attribution.enabled` and `--author-attribution` are
the *same knob* under two names in the same paragraph. There is a `docs/config-flag-conventions.md`
already; this section should adopt its vocabulary and label every knob with its surface.

### A5. `architecture.md` is in good shape

It is long but it is *one* document with one job, and the ground-rules → mental-model → detail
progression works. Two notes:

- The "Design documents" list at the end (`:1263`) omits the entire
  `docs/design/support-boundary/` tree, which is now where the product's most consequential
  reasoning lives. `support-contract.md` should be linked from the top of the file, not the bottom.
- "Operational boundaries" (`:1210`) is the most honest section in the repo and should be
  linked from the README. It is currently only reachable by reading 1200 lines.

---

## Part B — What the config surface is missing, and why

### B0. The direction is right; the API did not keep up

The doubt in the original question — *"analysing git folders, deciding where to place files,
understanding Kustomize … it's all very cool, but it requires more understanding to pick which
folder to reverse"* — is correct, and it is not an argument against the work. It is an argument
that the work **finished in the engine and never reached the API**.

The engine now answers, per folder: what layout is this, which directories are render roots,
which files are read-only build context, which are build directives, which documents are
ambiguous, is this folder supportable at all, and where would a new resource go. Every one of
those answers exists in `internal/manifestanalyzer` and in the `manifest-analyzer` CLI. The
GitTarget API exposes exactly **one bit** of it: `GitPathAccepted`.

That asymmetry produces four concrete problems.

### B1. There is no "look before you write"

For a product whose job is *writing to your GitOps repository*, first contact is: apply a
GitTarget, and it starts committing. `spec.prune.mode` guards deletions and is well designed —
but nothing guards the *initial* write, which for a populated folder is the scary one.

**Proposal: `GitTarget.spec.mode`.**

```yaml
spec:
  mode: Observe   # Observe | Write   (default Write, or Observe — see below)
```

`Observe` runs the entire pipeline — watches, acceptance gate, layout analysis, placement
resolution, plan construction — and reports what it *would* do, without touching the remote.
It is the field the current direction most obviously demands, because the folder analysis is
now the interesting part and it is invisible until after the first commit.

This is **not** `spec.suspend` (F6 in the maintainer review). Suspend means *stop reconciling
this object*; Observe means *keep reconciling and keep telling me, just don't write*. Ship both;
they answer different questions and Flux users expect `suspend` by name.

Whether `Observe` should be the **default** is the interesting call. Arguments for: a
reverse-GitOps tool defaulting to "writes to your repo on apply" is a startling default, and
there are no users yet to break. Arguments against: it adds a step to the quickstart, which is
currently the product's best asset. Recommendation: **default `Write`, but make the chart's
`quickstart` render `Observe`** — the fast path stays fast, and the deliberate path is safe.

### B2. A refusal is a boolean, not a diagnosis

`GitPathAccepted=False` + reason `UnsupportedContent` + a message string is the whole
report. What the operator actually computed — layout kind, render roots, external bases,
tolerated context files, ambiguous documents — reaches the user only through the message,
or through `-v1` logs, or by running a separate CLI against a separate checkout.

**Proposal: `GitTarget.status.layout`**, bounded and count-based, exactly like the existing
`status.streams` and `status.retention` precedents:

```yaml
status:
  layout:
    kind: KustomizeOverlay        # PlainManifests | KustomizeRoot | KustomizeOverlay | Mixed | Empty
    renderRoots: 1
    externalBases: 1              # rendered as read-only context, outside spec.path
    contextFiles: 3               # rendered, never written
    buildDirectives: 1            # kustomization.yaml / .sops.yaml — retained, never swept
    ambiguousDocuments: 0         # reachable from >1 render root; degrade to plain in-place edit
    observedTime: "2026-07-24T…"
```

This is the highest-value, lowest-risk item in this document. The analyzer computes all of it
already; this is a projection, not new logic. It turns "the operator did something clever with
my repo" into "the operator says my repo is an overlay with one external base and three context
files", which is the difference between trusting the product and not.

`ambiguousDocuments` deserves special mention. Per `configuration.md:779-784`, a document
reachable from more than one render root with differing override chains **silently degrades**
to plain in-place editing, recorded only as a store diagnostic at debug verbosity. That is a
correctness-relevant fallback that is currently invisible in the API. It is exactly the class
of surprise the original question is worried about.

### B3. Inference cannot be turned off

Sibling inference (`configuration.md:626-661`) is good, careful work — the namespace-agnosticism
proof, the sensitive/plaintext firewall, the deterministic tie-break. But it is **unconditional**.
A user who wants determinism — "if I didn't declare it, use the canonical path, do not read my
repo's mind" — has no field to say so.

**Proposal: `GitTarget.spec.placement.mode`.**

| Value | Ladder that runs |
|---|---|
| `Infer` (default, today's behaviour) | `byType` → `default` → sibling inference → canonical |
| `Declared` | `byType` → `default` → canonical. Inference off. |
| `Strict` | `byType` → `default` only; a type with no declared route is **refused**, not guessed. |

One enum, no new machinery — the ladder already exists, this just truncates it. `Strict` is what
a platform team standardising a repo layout across many clusters will want, and it converts a
silent surprise into a loud condition.

### B4. `commitWindow` is on the wrong object

Flagged in the maintainer review (§3) and worth acting on now. The split today:

| `GitProvider` (namespaced) holds | Which is really about |
|---|---|
| `url`, `secretRef`, `knownHostsRef`, `allowedBranches` | **the connection** ✅ |
| `commit.committer`, `commit.signing` | **the repo's identity on the platform** ✅ |
| `push.commitWindow` | **a workload's write cadence** ❌ |
| `commit.message.*Template` | **a workload's commit style** ❌ |

Consequence: two GitTargets sharing one repository — a fast-moving app folder and a
cluster-RBAC mirror — cannot have different batching or different subjects. That is not
hypothetical; it is the normal shape as soon as a second GitTarget exists.

**Proposal:** move `push.commitWindow` and `commit.message` to `GitTarget`. Keep `committer`
and `signing` on `GitProvider` (they are properties of the key registered with the Git host).

Implementation note, so this is not underestimated: the commit window is owned by the
`BranchWorker`, which is keyed per `(provider namespace, provider, branch)` and **shared across
GitTargets**. But the open window is already keyed by `(author, GitTarget)`
(`architecture.md:974-980`) — so the duration becomes a property of the open window rather than
of the worker. The worker keeps serializing the branch; only the timer moves. That is a real
change but a contained one, and it is materially easier now than after a release.

Do it in the same breaking wave as F6/F12, not separately.

### B5. Onboarding is a product step the docs pretend does not exist

`docs/design/support-boundary/repo-discovery-and-onboarding-scan.md` is explicit and correct:
**the operator never discovers.** A GitTarget is told exactly one subtree. Argo and Flux scan;
we deliberately do not.

That architectural call is right — and it makes onboarding a **step the product owes the user**,
performed by `manifest-analyzer --mode scan-repo`, which answers precisely the question the
original doubt raises: *which folders can become GitTargets, what layout is each, which are
supported and why not, and what GitTarget/WatchRule would express each one.*

That CLI is mentioned in `docs/style-guide.md` and `docs/UPGRADING.md`. It appears in
`configuration.md` **zero times** and in `architecture.md` **zero times**. The tool that closes
the gap exists and is undocumented where the gap is felt.

**Proposal:** `docs/onboarding.md`, step 0 of every non-quickstart install, with the scan output
of a real repo and the GitTargets it suggests. Link it from the README, from `configuration.md`'s
`spec.path` section, and from `architecture.md`'s configuration model.

### B6. Absorb the outstanding maintainer-review items into this wave

Still open from the companion review, all API-surface, all cheaper now than after release:

- **F6** — `spec.suspend` on GitTarget / WatchRule / ClusterWatchRule / GitProvider;
  `spec.interval` on GitProvider at minimum; jitter the requeue;
  `reconcile.configbutler.ai/requestedAt` + `status.lastHandledReconcileAt`.
- **F10** — CommitRequest lifecycle: `ttlSecondsAfterFinished` or an ownerRef, plus the
  `delete` verb. Today they accumulate in etcd forever and the controller cannot reap them.
- **F9** — verify the stored `scope: Namespaced` status-write path on the minimum supported
  Kubernetes version (one envtest).
- **F12 remainder** — unify the six near-identical reference shapes onto `fluxcd/pkg/apis/meta`
  where they match.

Plus one this review adds: **the `ProviderNotFound` message for the literal name `default`**
should name the fix (`clusterProvider.createDefault` in the chart, or commit the object). It is
the most likely first-run support ticket and it is a one-line message change.

---

## Part C — Worked examples

Three repositories, run through today's configuration and through the proposal. These are the
shapes the layout corpus (`test/fixtures/gitops-layouts/`) already covers.

### C1. Per-environment folders — the `commitWindow` problem

```text
repo/
  clusters/prod/…          # ~40 objects, changes hourly
  clusters/staging/…       # ~40 objects, changes constantly during the day
  platform/rbac/…          # ClusterRoles, changes monthly
```

Three GitTargets, one GitProvider, one branch. Today they **must** share
`push.commitWindow: 5s` and one set of message templates.

What you actually want: `staging` batching at `60s` (a `kubectl apply -k` storm should be one
commit, not twelve), `platform/rbac` at `0s` (every RBAC change is individually meaningful and
should be individually reviewable in `git log`), `prod` at the `5s` default.

Today: impossible without three GitProviders pointing at the same URL — which means three
credential Secrets, three signing configurations, and three `ls-remote` probes per interval
against the same host, for a batching preference. **This is the clearest argument for B4.**

With B4:

```yaml
kind: GitTarget
metadata: {name: platform-rbac}
spec:
  providerRef: {name: platform-repo}
  branch: main
  path: platform/rbac
  push:
    commitWindow: "0s"                      # every RBAC change is its own commit
  commit:
    message:
      groupTemplate: "rbac: {{.Author}} changed {{.Count}} object(s)"
```

### C2. `base` + `overlays` — the folder-choice problem

```text
apps/podinfo/
  base/                  kustomization.yaml + deployment.yaml
  overlays/prod/         kustomization.yaml (resources: [../../base], images:, replicas:)
  overlays/test/         kustomization.yaml (resources: [../../base], images:)
```

**Which folder is the GitTarget?** The API gives no help, and the two plausible answers behave
very differently:

- `path: apps/podinfo/overlays/prod` — the supported, designed case. The base is read as
  read-only context; writes stay in the overlay; a `kubectl set image` lands on the overlay's
  own `images:` entry. This is what render-root scoping shipped for.
- `path: apps/podinfo` — **also accepted.** Two render roots inside one target. `base/deployment.yaml`
  is now reachable from both, with differing override chains, so per `configuration.md:779-784`
  it is **ambiguous** and the writer silently degrades to plain in-place editing of the base —
  which means a `kubectl scale` in *prod* rewrites the **base**, changing test too.

Both report `GitPathAccepted=True` and `Ready=True`. Nothing in `kubectl get gittarget`
distinguishes them. The degradation is a debug log line.

That is the single most important example in this document, and it is the concrete form of the
original doubt. The proposal addresses it on three sides:

1. **B5 / scan-repo** tells you `apps/podinfo/overlays/prod` and `…/test` are the GitTargets,
   before you apply anything.
2. **B2 / `status.layout`** makes the wrong choice visible after you apply:
   `kind: Mixed, renderRoots: 2, ambiguousDocuments: 1`.
3. **B1 / `mode: Observe`** means the wrong choice costs you a status read, not a commit that
   changed the wrong environment.

Optionally a fourth: a `spec.expect.layout: KustomizeOverlay` that refuses when the observed
layout differs. I would **not** ship that yet — `status.layout` first, and see whether anyone
wants to gate on it. Publish the observation before inventing the assertion.

### C3. A bundle file and a new namespace — the inference problem

```text
clusters/prod/
  all.yaml                     # 9 ConfigMaps in one multi-document file,
                               #   spanning namespaces team-a and team-b
  team-a/secrets/db.sops.yaml
```

The two namespaces are load-bearing, not decoration. Per `configuration.md:652-655`, a resource
in a namespace the target has never written joins an existing cohort **only** when that cohort
has proven itself namespace-agnostic by already holding more than one — one directory holding one
namespace is indistinguishable from a per-namespace layout whose second namespace has not arrived
yet. So with a single-namespace `all.yaml` the answer below inverts, and the new ConfigMap takes
the canonical path.

Today (`configuration.md:638-646`): a new ConfigMap is **appended to `all.yaml`**, including one
in a brand-new `billing` namespace, because the bundle has cleared that bar. A new Secret goes to
`team-a/secrets/`. Both are the right calls, and both are *inferred from mutable repo state*.

The failure mode is not the logic; it is that a **human editing the repo changes the operator's
behaviour without touching any Kubernetes object.** Delete every `team-b` ConfigMap from
`all.yaml` and the bundle stops being namespace-agnostic, so the next ConfigMap in a new namespace
takes the canonical path instead. Nothing in the GitTarget changed. Nothing in its status says the
placement basis moved — and this is a sharper version of the same point, because the file the
inference reads is still there and still full of ConfigMaps.

With B3, a team that finds this unacceptable writes one field:

```yaml
spec:
  placement:
    mode: Declared                                   # never read the repo's mind
    byType:
      v1/configmaps: "{namespace}/configmaps.yaml"
      v1/secrets: "{namespace}/secrets/{name}.yaml"
```

…and gets the same practical layout, permanently, from a declaration rather than an inference.
Inference stays the default and stays excellent for "point me at an existing repo and it just
works" — which remains the product's best demo.

---

## Part D — Ordered plan

### Wave 0 — docs, no code (this branch has started it)

1. ✅ Fix the four factual/link errors (A1).
2. Add `RenderMatchesLive`, `GitProviderReady`, `ClusterProviderReady` to `configuration.md`'s
   status list, with one line each (A2).
3. Write `docs/onboarding.md` around `manifest-analyzer --mode scan-repo`, with the C2 repo as
   the worked example. Link it from README, `configuration.md`, `architecture.md` (B5).
4. Label every knob in the attribution section with its surface: CRD field / Helm value /
   controller flag (A4).
5. Link `support-contract.md` from the top of `architecture.md` and add the support-boundary
   tree to its design-document list (A5).

### Wave 1 — the breaking API wave, before any release

All of these change or add a spec field. Do them together, in one `feat(api)!` sequence, while
still `v1alpha3` and while there are no users.

1. **B4** — `push.commitWindow` and `commit.message` move `GitProvider` → `GitTarget`.
2. **B1** — `GitTarget.spec.mode: Observe|Write`; chart quickstart renders `Observe`.
3. **F6** — `spec.suspend` on the four reconciled kinds; `spec.interval` on GitProvider;
   requeue jitter; `reconcile.configbutler.ai/requestedAt` + `status.lastHandledReconcileAt`.
4. **B3** — `spec.placement.mode: Infer|Declared|Strict`.
5. **F10** — CommitRequest TTL/ownerRef + the `delete` verb.

### Wave 2 — legibility, non-breaking, ships any time after

1. **B2** — `GitTarget.status.layout`, including `ambiguousDocuments`. Highest value per line
    of code in this document.
2. The `default` ClusterProvider not-found message names the fix (B6).
3. **F9** — the one envtest.
4. `configuration.md` splits into reference + rationale + guides (A3).

### Explicitly not now

- `spec.expect.layout` — publish the observation (B2) before inventing the assertion.
- Making `Observe` the global default — revisit once there is one real user's onboarding to
  watch.
- Ordered placement rules (Option A of the placement design) — `byType` + `mode` covers the
  demand we can actually see.
