# Upgrading

Breaking changes and the steps to adopt them, newest first. The machine-generated per-release
summary lives in [`CHANGELOG.md`](../CHANGELOG.md); this file is the human-written migration
guidance that the changelog's breaking-change entries link to.

We are pre-1.0, so breaking changes bump the **minor** version (release-please is configured with
`bump-minor-pre-major`) rather than the major. Read the relevant entry before upgrading across it.

## Safe upgrade order for the GitTarget API changes

Five fields are **removed outright** in this release, across three kinds. There is no shim, no
conversion webhook and no refusal to catch you: a removed field is pruned from the schema, so the
next two entries are the whole migration and they are yours to apply by hand.

**Take the inventory FIRST, before you upgrade anything.** This is not a stylistic preference. Once
the new CRDs are applied, the old values are no longer returned by the API server at all — not
after a write, but immediately, because a field outside the structural schema is not served. Your
own manifests in Git are then the only record of what you had. If you upgrade before taking the
inventory, recover the values from your repository or from an etcd backup.

```bash
# 1. INVENTORY — run this against the CURRENT cluster, before applying the new CRDs.
kubectl get gittargets -A -o json |
  jq -r '.items[] | select(.spec.allowedSourceNamespaces)
    | "gittarget \(.metadata.namespace)/\(.metadata.name): \(.spec.allowedSourceNamespaces)"'

kubectl get clusterproviders -o json |
  jq -r '.items[] | select(.spec.allowedNamespaces or .spec.allowSourceNamespaceOverride == true)
    | "clusterprovider \(.metadata.name): accessFrom<-\(.spec.allowedNamespaces)
       allowAny<-\(.spec.allowSourceNamespaceOverride)"'

kubectl get gitproviders -A -o json |
  jq -r '.items[] | select(.spec.push or .spec.commit.message)
    | "gitprovider \(.metadata.namespace)/\(.metadata.name): window<-\(.spec.push.commitWindow)
       message<-\(.spec.commit.message)"'

# The rules whose MEANING changes even though their YAML does not:
kubectl get watchrules -A -o json |
  jq -r '.items[] | select(.spec.rules[]?.sourceNamespace == "*")
    | "watchrule \(.metadata.namespace)/\(.metadata.name) -> target \(.spec.targetRef.name)"'
```

Keep that output. It is the input to every step below.

**2. Decide what each `sourceNamespace: "*"` rule should become.** This is the only step that needs
judgement rather than transcription, and the only one where getting it wrong is not obvious
afterwards. `"*"` keeps its spelling and changes its meaning — see
[its own section below](#sourcenamespace--keeps-its-spelling-and-changes-its-meaning). A rule whose
target had an `allowedSourceNamespaces` list will start mirroring **every namespace its credential
can read** unless you either enumerate those namespaces in `rules[].sourceNamespace` or tighten the
credential's RBAC to match the list you had.

**3. Upgrade the controller and CRDs.** What happens next depends on which field a given object was
carrying, and the two outcomes are opposites:

- A target whose **`ClusterProvider`** policy was pruned **stops writing**. An absent `accessFrom` is
  deny-by-default, so the provider admits no namespace and every `GitTarget` through it goes
  `Validated=False`. Same for a pruned `allowSourceNamespaceOverride: true`, which stalls the
  cross-namespace `WatchRule`s it used to permit. Loud, and it clears in step 4.
- A target carrying a pruned **`GitTarget`** or **`GitProvider`** field **keeps writing**, under the
  new defaults — and for a `"*"` rule, under the new meaning. Nothing stalls and nothing warns.

The second is the one to hurry for, because it is the one that is not telling you anything. Go
straight to step 4.

**4. Re-apply every migrated object in one sync.** There is no ordering requirement between them,
so do not stage it: a single apply makes the stall in step 3 one reconcile long.

Ordering is unnecessary because nothing here reads its migrated value from another object. Each
`GitTarget` now carries its own `spec.commit`, read from the target at write time rather than from
the `GitProvider` — that is the whole point of the move — so a provider applied first buys a target
nothing. What a `ClusterProvider` *does* control is whether its targets are admitted at all, which
is why applying it alongside them is what lifts the stall.

Your manifests should already carry the new spellings from steps 1 and 2.

**5. Confirm the new fields took effect**, which for half the cases matters more than a green
condition. A pruned `ClusterProvider` policy shows up in the status, so `Ready` catches it. A pruned
`GitTarget` or `GitProvider` field does not: the object is `Ready=True` either way, and only reading
the value back distinguishes migrated from silently defaulted.

```bash
kubectl get clusterproviders -o custom-columns=\
NAME:.metadata.name,ACCESS_FROM:.spec.accessFrom,ALLOW_ANY:.spec.allowAnySourceNamespace

# Read back the whole spec.commit, not just the window: a pruned message template is otherwise
# invisible, since a target with no templates commits perfectly happily under the built-in ones.
kubectl get gittargets -A -o custom-columns=\
NS:.metadata.namespace,NAME:.metadata.name,COMMIT:.spec.commit
```

`<none>` in `ACCESS_FROM` or `COMMIT` on an object you meant to migrate means the value was pruned:
the manifest still carries an old spelling. Check `COMMIT` renders **both** halves you migrated —
a `map[window:...]` with no `message:` key means the templates did not come across.

**What you will NOT get is a warning.** Because the fields are removed rather than retained, an
object still carrying an old spelling is accepted with the value silently pruned — the mirror keeps
running under the new defaults. That is a deliberate trade for a much smaller code surface, priced
in [`facts/crd-upgrade-strategies.md`](facts/crd-upgrade-strategies.md), and it is why the inventory
is step 1 rather than a footnote.

## Commit batching and message templates are GitTarget fields

`GitProvider.spec.push.commitWindow` and `GitProvider.spec.commit.message` are now
`GitTarget.spec.commit.window` and `GitTarget.spec.commit.message`. The message shape is unchanged:
the same `eventTemplate`, `reconcileTemplate` and `groupTemplate`, with the same variables.

`GitProvider` is the connection — a URL, a credential, the branches it will accept. How a folder's
writes are batched and how those commits are phrased describe the folder, and two `GitTarget`s
sharing one `GitProvider` had no way to disagree about either. They can now: an RBAC folder that
wants a commit per change and an app folder that wants a burst coalesced no longer have to be two
connections.

`commit.committer` and `commit.signing` stay on `GitProvider`. Both describe the identity that talks
to the remote — the signing key is a Secret in the provider's namespace, and the committer is the bot
the platform sees.

**Both old fields are removed, not retained.** A `GitProvider` that still sets either is accepted
and the value is pruned, so the folder carries on committing at the default `5s` cadence under the
default message templates — with nothing anywhere reporting it. That is why the
[inventory](#safe-upgrade-order-for-the-gittarget-api-changes) comes first.

Move them per target:

```yaml
# GitProvider: delete spec.push, and spec.commit.message if you set one.
apiVersion: configbutler.ai/v1alpha3
kind: GitProvider
spec:
  commit:
    committer:
      name: GitOps Reverser
---
# GitTarget: the values land here, once per folder that needs them.
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
spec:
  commit:
    window: "5s"
    message:
      groupTemplate: "{{ .Author }} on {{ .GitTarget }}: {{ .Count }} resource(s)"
```

A `GitTarget` that sets no `spec.commit` batches over a 5s rolling silence window and uses the
built-in templates, which is what an omitted `spec.push` gave you before. So a `GitProvider` that
never set either field needs one edit only if it set `spec.commit.message`; otherwise nothing to do.

The chart moves the value with the field: `quickstart.gitProvider.push.commitWindow` is
`quickstart.gitTarget.commit.window`.

## Source-namespace scope is the ClusterProvider's, and `sourceNamespace: "*"` is cluster-wide

Four changes to how a target's source namespaces are bounded, and they ship together because the
last one is defined in terms of the first.

| Was | Is |
|---|---|
| `GitTarget.spec.allowedSourceNamespaces` | **removed** |
| `ClusterProvider.spec.allowSourceNamespaceOverride` | `ClusterProvider.spec.allowAnySourceNamespace` |
| `ClusterProvider.spec.allowedNamespaces` | `ClusterProvider.spec.accessFrom` |
| `sourceNamespace: "*"` = every namespace the `GitTarget` admits | every namespace the source credential can read, as one cluster-wide watch |

All three are **removed from the schema**, so an object still setting one is accepted with the
value pruned. Nothing warns you, which is the whole reason the
[inventory](#safe-upgrade-order-for-the-gittarget-api-changes) is step 1.

The consequences differ, and only one of them is quiet:

| Old field, left in place | What happens |
|---|---|
| `GitTarget.spec.allowedSourceNamespaces` | pruned. A `"*"` rule under that target **widens** to every namespace the credential can read |
| `ClusterProvider.spec.allowedNamespaces` | pruned, so `accessFrom` is absent. That is deny-by-default: **no `GitTarget` is admitted** and every one of them stalls |
| `ClusterProvider.spec.allowSourceNamespaceOverride: true` | pruned, so the delegation is **revoked** and every cross-namespace `WatchRule` through it stalls |

The two `ClusterProvider` rows fail closed and are loud — stalled objects with conditions you can
read — so they are an outage rather than a hazard. The `GitTarget` row is the one to be careful
about: it fails **open**, widening what a `"*"` rule mirrors into your repository, with no condition
to notice. Treat it as the reason to do step 1 properly.

### The two renames

Mechanical. Same type, same default, same semantics; rename the key.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: ClusterProvider
metadata:
  name: prod-eu-1
spec:
  accessFrom:                    # was allowedNamespaces
    names: [team-a]
  allowAnySourceNamespace: true  # was allowSourceNamespaceOverride
```

`accessFrom` keeps doing exactly what it did: it is the deny-by-default policy for which
**control-cluster** namespaces may reference this provider from a `GitTarget`, matched against
control-cluster `Namespace` labels. It is the one namespace policy that survived, because the
boundary it draws is available nowhere else — source-cluster RBAC bounds what a credential may
*read*, and cannot express which control-plane tenant may *wield* it.

`allowAnySourceNamespace` keeps `Source` in its name on purpose: this object carries two namespace
planes, and an `allowAnyNamespace` sitting directly beneath `accessFrom` would read as a modifier on
it.

### Removing `allowedSourceNamespaces`

Delete the field. What it bounded is bounded by the source credential's own Kubernetes RBAC: a
namespace the credential cannot read fails with a clean 403 instead of being refused by a policy
field that restated the credential in the one place that could not revoke it.

```yaml
apiVersion: configbutler.ai/v1alpha3
kind: GitTarget
spec:
  # allowedSourceNamespaces: {...}   <- delete this
```

After the edit, a `WatchRule` item naming a namespace other than its own needs two things instead of
three: the `GitTarget`'s namespace admitted by its `ClusterProvider`'s `accessFrom`, and that
provider setting `allowAnySourceNamespace: true`.

**`allowAnySourceNamespace: false` is not exactly your previous posture**, and it is worth saying
plainly rather than papering over. A declared `allowedSourceNamespaces` could deny a rule's **own**
namespace: it was exhaustive once declared, with no self-namespace exception. The new default
matches the *no-policy* path — every rule keeps its own namespace, and nothing else — which is what
a default install ran. If you declared a policy that deliberately excluded a co-resident rule's own
namespace, that exclusion is gone and that rule now watches its own namespace again.

**Source-side label selectors are lost, and there is no replacement.** Admitting every namespace
carrying a label, and following namespaces as they appear, has no RBAC equivalent short of a
`RoleBinding` per namespace. This is the real capability cost of the change and it is accepted
rather than overlooked: an N-way restriction costs N objects wherever it is expressed. If you ran
`allowedSourceNamespaces: {selector: {...}}`, enumerate the namespaces in `rules[].sourceNamespace`,
or use `"*"` and bind the credential to exactly the namespaces you mean.

The operator now needs **no `Namespace` access at all** in a source cluster. If you granted a remote
`ClusterProvider`'s identity `namespaces` `get`/`list`/`watch` only for a selector policy, you can
take it back.

### `sourceNamespace: "*"` keeps its spelling and changes its meaning

This one has no shim, because there is nothing to rename: the value is still `"*"` and it still
parses. Read this paragraph even if you change no YAML.

`"*"` used to mean *every namespace this `GitTarget` admits* — resolved live through
`allowedSourceNamespaces` into a concrete set, then planned as one watch stream and one list per
namespace. That field is gone, so the definition had to move. `"*"` is now **one cluster-wide list
and one cluster-wide watch**, bounded by the source credential's RBAC and by nothing else, and
**refused outright while `allowAnySourceNamespace` is false**.

For a target that declared no `allowedSourceNamespaces`, `"*"` already resolved to whatever the
credential could see, so the widening is narrower in practice than it reads. For a target that
declared one, it is real: **a `"*"` item now mirrors namespaces that policy excluded.** Before you
upgrade, find them:

```bash
kubectl get watchrules -A -o json |
  jq -r '.items[]
    | select(.spec.rules[]?.sourceNamespace == "*")
    | "\(.metadata.namespace)/\(.metadata.name) -> \(.spec.targetRef.name)"'
```

For each one, either name the namespaces explicitly in `rules[].sourceNamespace`, or keep `"*"` and
make the credential's RBAC the fence you meant the policy to be.

Two things get better. A `"*"` rule over a type in a hundred-namespace cluster was a hundred watch
connections and a hundred list calls at warm-up, each with its own cursor and its own share of the
apiserver watch cache; it is one of each now, and the saving grows with the cluster. And its failure
mode is a clean 403 rather than a silently empty set.

A `"*"` item and a named-namespace item for the same type are **peers**, not duplicates. Each rule
carries its own `operations` filter, so a target holding both runs two streams over overlapping
objects. That is correct, not something to tune away.

## A GitTarget must cover exactly one kustomize render root

A `GitTarget` whose `spec.path` covers more than one kustomize render root — an app root above a
`base/` and several `overlays/`, rather than one leaf overlay — no longer places new documents. It
reports `LayoutResolved=False` with reason `Ambiguous`, naming the roots it covers, and refuses the
write with `GitPathAccepted=False`, reason `AmbiguousLayout`.

Before this, such a target wrote the new document to the built-in canonical path inside the folder
it covered, where it belonged to no render root and no deployer would ever apply it.

**Who is affected.** Only a target pointed at a folder with several kustomizations under it. A
folder with one kustomization, or none at all, is untouched. **Existing documents are untouched
either way**: a resource that already has a document in Git is edited where it lives, whatever the
folder covers, so nothing stops being mirrored and nothing moves.

**Find them before you upgrade.** For each `GitTarget`, count the `kustomization.yaml` files under
its `spec.path` in the branch it writes to:

```bash
kubectl get gittargets -A \
  -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name,PATH:.spec.path
# then, in a checkout of each target's branch:
find <spec.path> -name kustomization.yaml
```

Two or more, and that target is affected — unless all but one of them are referenced by another
(a `base/` that every overlay lists is not itself a root).

**What to do about it.** Point the target at one leaf, and declare the other environments as their
own `GitTarget` objects:

```yaml
spec:
  path: apps/checkout/overlays/prod    # not apps/checkout
```

One target is one environment is one write partition, which is what makes authorization, audit and
review line up with the environment boundary. The reasoning is in
[`layout/shapes/README.md`](layout/shapes/README.md#why-only-a-leaf-can-be-a-kustomize-target).

`status.placement.mode` and `status.placement.renderRoot` report what the scan resolved, and both
are published before a target has written anything — so a target you have just declared already
says whether it found one root, none, or several.

## New resources land where you declare, not where the folder's other documents live

Sibling inference is gone. A resource with no document in Git yet is placed by the first of three
things that applies, and nothing else:

1. the GitTarget's `spec.placement.byType` entry for its type, or `spec.placement.default`;
2. the folder's one supported `kustomization.yaml`, when the whole folder has exactly one — the file
   lands beside it and joins its `resources:` list;
3. the built-in canonical path, `{namespaceOrCluster}/{groupPath}/{resource}/{name}{sensitiveSuffix}`:
   a cluster-scoped resource uses the literal `_cluster/` in place of the namespace, a core resource
   omits the group segment, there is no version segment, and a sensitive resource gets `.sops.yaml`
   instead of `.yaml`.

Before this, a folder with no declared placement was read for its layout: a new ConfigMap was appended
to the bundle the other ConfigMaps shared, or written beside them one-per-file. That is what changes.

**Who is affected.** A target whose repository this operator created is **unaffected** — such a folder
already used canonical paths, which inference also produced. A target pointed at a **hand-authored**
folder with a layout of its own is affected: a resource of a type that folder already holds, in a
namespace or with a name it has never held, now gets the canonical path instead of joining the
existing file or directory. Nothing already in Git moves — an existing document is still edited in
place at its current location, forever.

**What to do about it.** Declare the layout the folder means:

```yaml
spec:
  placement:
    byType:
      v1/configmaps: "all.yaml"                      # keep bundling ConfigMaps into one file
      v1/secrets: "team-a/secrets/{name}.sops.yaml"  # one encrypted file per Secret
```

A declared template does everything inference did and says so on the page. A **kustomize** folder needs
no declaration: step 2 already places new files where that folder builds them.

**One shape worth expecting.** In a folder that kustomize builds, a bundle file is no longer extended
by default. A new resource gets a file of its own beside the `kustomization.yaml` and an entry added to
its `resources:` list, so the build file changes where it previously did not (the bundle was already
listed, so extending it needed no entry). Both outcomes mirror the resource and both render; the new
one keeps a resource the operator placed out of a file a human curated, and it is undone by declaring
the bundle in `byType`.

**How to tell whether it affects you**, before or after upgrading — every placement is counted, by
GitTarget and by type:

```promql
sum by (gittarget_namespace, gittarget_name, group, version, resource) (
  increase(gitopsreverser_placements_total{source="canonical"}[24h]))
```

Each series is a type that took the built-in path. For a canonical-layout folder that is simply the
layout. For a folder with a convention of its own it is the `byType` line to add. `source="declared"`
and `source="kustomize_root"` need no attention. Two companions ship with it:
`gitopsreverser_placement_refusals_total{reason}` (resources the writer declined to place — each one
is absent from the mirror) and `gitopsreverser_placement_kustomization_entries_total{outcome}`, whose
`failed` value is a new file committed outside every render. See
[interpreting-metrics.md](interpreting-metrics.md).

**Why the feature was removed rather than made switchable.** It let an edit to the *repository* change
where the operator writes, with no Kubernetes object changing and nothing in status recording the move
— delete enough of one namespace's documents from a shared bundle and the next new one takes a
different path. Its namespace-safety guard had also failed once by cascading: one wrong append made a
per-namespace file look namespace-agnostic, which legitimized it for every later resource, which
collapsed a whole type into one file. There is deliberately **no** `spec.placement.mode` flag to turn
inference back on: an off-switch for a removed feature is a permanent API field bought to solve a
temporary problem. The full argument is in
[`open-asks-priority.md`](design/open-asks-priority.md).

**One related fix.** A new document in a directory whose kustomization sets `namespace:` omits
`metadata.namespace` only when the transformer names the resource's **own** namespace. When it names a
different one the namespace is now written explicitly — omitting it would have handed the namespace to
kustomize and rendered a different object than the one being mirrored. This also now applies to a path
you declared, which previously wrote a `namespace:` line the rest of that folder omits.

## 0.41.0 — attribution facts travel on a selectable transport, and Redis is no longer implied

Attribution stopped meaning Redis. The audit receiver appends its facts to a per-type **stream**,
the watch side follows the streams for the types it watches into a bounded in-process index, and
which stream implementation carries them is a choice:

| `attribution.transport` (`--author-attribution-transport`) | Store | When |
|---|---|---|
| `redis` — the default, and the previous behaviour | Redis streams, per type | any install; the only one that survives a restart |
| `memory` | an in-process ring | a single-replica install with no Valkey; every fact is lost on restart, by design |

**A default upgrade needs no action.** `redis` is the default, `queue.redis.addr` keeps its meaning,
and the same events resolve to the same authors.

Two things to know if you look closely:

- **Facts in flight across the upgrade lose their author.** The v1 fact keyspace is gone, and the
  streams are a new keyspace rather than a migration of it, so a fact written by the old version is
  not read by the new one. A watch event whose fact was written moments before the restart resolves
  `absent` and its commit is authored `unknown (attribution unresolved)`. The window is one fact TTL
  at most, and mirroring is unaffected — attribution changes the author, never the state.
- **`attribution.transport=memory` requires `queue.redis.addr` to be empty-able, and a single
  replica.** The chart refuses `redis` without an address at render time, and the binary refuses
  `memory` with `--replica-count` above 1 at startup, because the audit receiver and the resolver
  must be one process for in-process facts to be visible at all. Redis is still required for the
  admission webhook's command-author capture, which has no in-process counterpart.

New tuning knobs, all with behaviour-preserving defaults: `attribution.maxFactsPerType` (4096),
`attribution.maxFacts` (65536), `attribution.collectionWindow` (30s), `attribution.collectionUIDCap`
(10000). They bound the in-process index and the collection join; see
[configuration.md](configuration.md).

## 0.41.0 — the attribution metrics are relabelled and partly renamed (breaking for queries)

The `result` label is **gone** from `gitopsreverser_attribution_resolutions_total` and
`gitopsreverser_attribution_resolution_wait_seconds`. It crammed two orthogonal questions into one
value — which evidence answered, and who it named — and hid a third, so it is replaced by two labels
on the counter and a different second label on the histogram. Four metric families are renamed in the
same change, while the surface is moving.

Nothing else in the pipeline changes: the same events resolve to the same authors, and the same
commits are written. Only the names a query selects on change.

| Old | New |
| --- | --- |
| `attribution_resolutions_total{result}` | `attribution_resolutions_total{tier, actor_kind}` |
| `attribution_resolution_wait_seconds{result}` | `attribution_resolution_wait_seconds{tier, event_kind}` |
| `attribution_fact_events_total{op}` | `attribution_facts_total{op}` |
| `attribution_fact_index_size` | `attribution_fact_index_entries` |
| `attribution_collection_degraded_total{reason}` | `attribution_collection_without_uidset_total{reason}` |

`attribution_fact_index_evictions_total{reason}` and `attribution_fact_stream_gaps_total{stream}`
are unchanged.

The values move too. `tier` carries what `result` said about evidence, and `weak` splits in two,
because it covered two different kinds of it:

| Old `result` | New |
| --- | --- |
| `exact_user` | `tier="exact"`, `actor_kind="user"` |
| `exact_serviceaccount` | `tier="exact"`, `actor_kind="serviceaccount"` |
| `weak` (a UID-latest match) | `tier="latest"` |
| `weak` (the RV-only escape hatch) | `tier="resource_version"` |
| `collection_uid` | `tier="deletecollection_body_uid"` |
| `collection_scope` | `tier="deletecollection_scope"` |
| `name`, `absent` | `tier=` the same value |

The two collection values are renamed for the **verb that produced the fact** and how it matched, the
convention the three removal-only tiers now share: `delete_sticky`, `deletecollection_body_uid`,
`deletecollection_scope`. `body` is in the second because the uid set comes from the response body,
which is the part the API server may not send; when it does not, the same fact resolves at
`deletecollection_scope` instead. `latest` and `name` keep their names, because either can hold a
delete fact or a write and so cannot claim a verb.

`delete_sticky` is a value `result` never had at all, and it comes with a behaviour change. A
finalized deletion — a human deletes, a controller clears the finalizer — is attributed to the human
and resolves at `tier="delete_sticky"`. Before this release it named the controller and was counted
on the exact path (`result="exact_user"` or `result="exact_serviceaccount"`), because the finalizer
patch's fact carries the resourceVersion the *deletion* stamped and overwrote the deleter's under
the same key. A dashboard therefore sees the exact path shift toward `delete_sticky` for types that
carry finalizers, and `commits_total{author_kind}` shift from `serviceaccount` toward `user`. A query
that enumerates tiers explicitly needs the three new values; `tier!="absent"` covers them already.

`actor_kind` is `user`, `serviceaccount`, or `none`, the vocabulary
`gitopsreverser_commits_total{author_kind}` already uses, and it is available on **every** tier
rather than on the exact one alone. `event_kind` on the wait histogram is `write` or `removal`: a
removal holds a fallback and keeps waiting for evidence about the deletion where a write does not,
so `{event_kind="removal"}` is the distribution `--author-attribution-grace` is tuned from.

**Rewrite match coverage as `tier!="absent"`.** A query kept as `result=~"exact_.*"` — or ported to
`tier=~"exact.*"` — reads the collection and name tiers as misses, and those tiers named an actor.

Four signals are new in the same release, so a query is worth writing against them at the same time:

- `attribution_fact_stream_decode_errors_total{transport}` — an entry the follower refuses is
  skipped and its facts lost, and this is the one loss path with no other symptom. It covers both a
  payload that is not JSON and one that breaks the fact contract by naming nobody (`author` is
  required and non-empty).
- `attribution_fact_follower_errors_total{transport}` and
  `attribution_fact_follower_last_success_timestamp_seconds`. Alert on **both** arms of the
  timestamp: it is not published until the follower's first successful read, so
  `time() - <gauge>` returns no series for a follower wedged since startup rather than a large
  number. The expression that covers that case is in the field guide.
- `attribution_transport_info{transport}` — an info gauge, always 1, naming the transport in force.
  It is a legend rather than a threshold: a burst of unresolved commits after a restart is expected
  under `memory` and a bug under `redis`.

`gitopsreverser_audit_events_total` also gains a `no_attribution_fact` outcome in the `dropped`
category, for an accepted event that produces no fact — a population previously counted `queued`.
The `category="error"` invariant is unaffected.

Every one of these is documented in
[interpreting-metrics.md](interpreting-metrics.md#audit-attribution-optional).

## `summary.fleetRoot` is gone from the analyzer report (breaking JSON change)

`manifest-analyzer --mode scan-repo` no longer emits `status.summary.fleetRoot`, the text
report no longer prints `fleet-root=true`, and `pkg/manifestanalyzer`'s `RepoSummary` no
longer has a `FleetRoot` field. A reader that indexed the key gets nothing back; there is no
replacement key to switch to, and none is needed — the field never changed which folders the
scan offers.

It was a guess, and not one a better heuristic could rescue. It required top-level
`clusters/` + `apps/` + `infra/` directories, so it read `false` on the layout Flux's own
documentation and reference repository use (`infrastructure/`) and `true` on any repository
that happens to use those three names. The deeper problem is that fleet-rootness is not a
property of a repository at all: a directory is a cluster entry point because some cluster is
*pointed at* it — a `FluxInstance.spec.sync.path`, a `flux bootstrap` somebody ran once — and
a read-only scan of the tree cannot see that. The same repository is zero cluster entry
points, one, or twelve, depending on who syncs it.

Nothing branched on it, here or downstream, and the repository root is not offered as a
candidate with or without it. When the question needs answering — *is this folder somebody's
bootstrap directory rather than their app?* — the answer will come from the documents a
candidate folder holds (a `FluxInstance`, a `flux-system` bootstrap `Kustomization`), which is
decidable rather than guessed. See
[orchestrator-knowledge-boundary.md](design/support-boundary/orchestrator-knowledge-boundary.md).

## The analyzer report is a KRM document (breaking JSON change)

The two published scan modes (`manifest-analyzer --mode scan-folder|scan-repo --format json`)
and the `pkg/manifestanalyzer` report types emit a KRM envelope. (`--mode analyze` is unaffected:
it renders the engine's own structural report, which is not part of the published contract.)
`schemaVersion` is gone; `apiVersion` and `kind` replace it, the scan request lives in `spec`, and
everything the scan found lives in `status`:

```json
{
  "apiVersion": "manifestanalyzer.configbutler.ai/v1alpha1",
  "kind": "RepoReport",
  "spec": { "root": "/repo", "mode": "scan-repo" },
  "status": {
    "generator": { "name": "manifest-analyzer", "version": "v0.39.1" },
    "candidates": [],
    "summary": {}
  }
}
```

A bespoke `schemaVersion` left three questions open: what a bump asserts, whether a reader should
refuse a version it does not know, and what bumps it at all. The Kubernetes API conventions already
answer all three, in a document every consumer of a GitOps tool has read, so the report cites that
contract instead of writing its own: `v1alpha1` may change incompatibly in any release, a reader
that does not know the version it is handed should refuse it rather than best-effort parse, and
adding a field is still not a bump. The report is never served, never registered, and not
applyable, which is why it carries no `metadata`.

Three things come with the envelope:

- **`status.generator`** names the build that produced the report (`{name, version}`), so a
  document that outlives the process that made it still says which release decided its contents.
  It is never empty: ldflags when the release build sets them, otherwise the module version the Go
  toolchain records for `go install ...@vX.Y.Z`, otherwise the literal `"dev"`.
- **`manifest-analyzer --version`** prints that release and the report `apiVersion` together, so
  one exec answers both questions.
- **Every refusal says whether it can be solved.** `solvable` is a boolean, and `actor`
  (`repository-author` or `platform-operator`) names who can solve it. See below.

### Refusals say whether they can be solved, and three more codes are published

`Issue` and `RefusalReason` carry `solvable` (always present) and `actor` (set only when
`solvable`). They are decided by the check that raised the refusal, because only that check knows:
`unsupported-kustomize` is solvable for a build file the author broke and not for a generator, and
no per-code table can express that. `solvable` describes the release you are running and makes no
promise about the future, so read it on every scan rather than caching a mapping from it.

`pkg/manifestanalyzer` also publishes `IssueRenderRefused`, `IssueRenderDoesNotMatchLive` and
`IssueUnplaceableEdit`. The engine could already raise all three, but only a live write does, so
they reached a consumer through GitTarget status with no exported constant to match on.
`RefusalReason.Code` is typed `IssueKind` rather than `string`, which makes "a candidate's refusal
is the acceptance gate's own issue" compile-checked rather than merely stated.

### Migration

- Read `apiVersion` where you read `schemaVersion`, and refuse a version you do not know.
- Move field accesses under `spec`/`status`: `report.Accepted` becomes `report.Status.Accepted`,
  `report.Root` becomes `report.Spec.Root`, `report.Candidates` becomes `report.Status.Candidates`,
  and so on. Golden fixtures generated from the old shape need regenerating.
- If you inferred from the code whether a refusal can be solved (for example, treating
  `refused-structural` as hopeless and everything else as "not supported yet"), delete that mapping
  and read `solvable`. A report from a build that predates the field carries no `solvable` key at
  all; treat that as "nobody said", not as `false`.
- Record the `status.generator.version` you consumed a report with, if you pin our release
  elsewhere. That is the whole point of the field.

## Unreleased: status vocabulary and two removed status fields (next minor; status-only change)

Nothing in `spec` changed and no manifest you wrote needs editing. Three things a **reader of
status** may notice:

**1. The generic condition reason is now `Succeeded`, not `OK` or `Ready`.** A reason that restates
the condition type (`Ready=True, reason=Ready`) answers nothing. The generic reasons are now aliases
of [`github.com/fluxcd/pkg/apis/meta`](https://pkg.go.dev/github.com/fluxcd/pkg/apis/meta)
(`Succeeded`, `Failed`, `Progressing`, `DependencyNotReady`), so one alerting rule works across every
kind here and across every Flux kind in the same cluster. Domain reasons (`UnsupportedContent`,
`WriteBoundaryRefused`, `NoAdmittedSourceNamespaces`, …) are unchanged.

If you match on a reason string, update:

| Was | Now |
|---|---|
| `Ready=True`, reason `OK` (GitTarget, `Validated` too) | reason `Succeeded` |
| `Ready=True`, reason `Ready` (GitProvider, ClusterProvider, WatchRule, ClusterWatchRule) | reason `Succeeded` |

Matching on `status` rather than `reason` (the usual case, and what
`kubectl wait --for=condition=Ready` does) needs no change.

**2. `GitTarget.status.lastReconcileTime` and `status.streams.observedTime` were removed.** Both were
stamped with the current time on every pass, which made every reconcile a status write, and every
status write a fresh watch event that re-queued the object. A condition's `lastTransitionTime` plus
the `controller_runtime_reconcile_total` metric answer the same question without making every object
mutable on read. `status.lastPushTime` (a real event) and `status.retention.observedTime` (the
data plane's own observation time, which you need to tell a stale zero from a live one) both stay.

**3. A GitTarget with no WatchRules now reports `Ready=True`.** It used to sit at
`Ready=False`/`Reconciling=True`/`NoResolvedTypes` forever, a state the documented setup flow passes
through, since you create the GitTarget before its rules. Nothing was pending and nothing ever would
be, so `kubectl wait --for=condition=Ready` never returned and the object re-reconciled every 10
seconds for its whole life. "Nothing to mirror" is a converged state. `status.streams.summary` still
reads `0/0` and `StreamsRunning` still carries reason `NoResolvedTypes`, so the zero stays visible.

If you have automation that treats "GitTarget Ready" as "GitTarget is mirroring something", assert
`status.streams.total > 0` as well.

**Also in this change, with no action needed:** GitTarget's `kubectl get` output drops to four
columns (`Ready`, `Reason`, `Streams`, `Age`); `Provider`, `Branch` and `Path` moved to
`-o wide`. And every controller now emits a Kubernetes Event on each persisted `Ready` transition,
visible in `kubectl describe` and routable by anything that consumes Events. The operator's
ServiceAccount gained `create`/`patch` on `events` for it, which `helm upgrade` applies.

## Unreleased — a resync no longer deletes Git documents by default (next minor; behavior change)

**`GitTarget` gained `spec.prune.mode`, and its effective default changes what a resync does.**
Previously every resync mark-and-swept: a managed document whose resource was absent from the
desired snapshot was deleted. That is now opt-in.

| Mode | Explicit source DELETE | Resync mark-and-sweep |
|---|---|---|
| `Never` | kept | kept |
| `OnEvent` — the effective default | mirrored | kept |
| `Always` — the previous behavior | mirrored | swept |

**Deleting a resource in the cluster still deletes its file.** Only the *inferred* deletion changes:
the operator no longer concludes "Git has a document, the snapshot does not list it, therefore delete
it". That inference is only as good as the snapshot's scope — a watch rule narrower than you
intended, or version skew against a controller that does not understand a newer scope field, both
produce a complete-looking snapshot that is smaller than reality. (A snapshot the operator could not
*finish* is already handled: a failed list or watch enqueues no resync, so an outage stops a sweep
rather than shrinking one.)

### What you have to do

**Nothing, to be safe.** An existing `GitTarget` has no `spec.prune` and resolves to `OnEvent`
without being edited — Kubernetes does not retro-fill defaults into stored objects, so the operator
applies the default itself.

**To keep the old behavior**, declare it on each target that needs full convergence:

```yaml
spec:
  prune:
    mode: Always
```

The field is mutable, so you can switch a target to `Always` after confirming its watch scope
without recreating it. The switch re-lists that target's watched scopes, so the documents a resync
had been keeping are swept on the edit rather than at some later replay.

### How to tell whether it affects you

```console
$ kubectl get gittarget acme -o jsonpath='{.status.retention}'
{"mode":"OnEvent","retainedDocuments":3,"observedTime":"2026-07-21T13:20:00Z"}
```

A non-zero `retainedDocuments` means the mirror holds documents a converged one would not — the
configured outcome, not a fault, so no condition goes `False` for it. `0` means a resync ran and
found nothing to retain; an absent `retention` block means none has reported yet. The same event
logs a throttled line naming the target and increments
`gitopsreverser_prune_retained_documents_total`, labelled by GitTarget and mode. See
[configuration.md](configuration.md#seeing-what-was-kept).

This ships in the **same release** as the rule-kind scope change below, and is what makes that
migration non-destructive: a converted `WatchRule` that resolves to a narrower set of namespaces than
you intended leaves the affected documents in Git instead of deleting them.

## Unreleased — scope is now carried by the rule kind (next minor; breaking)

**Scope moved from a per-rule field onto the rule KIND.** `WatchRule` is the namespaced surface and
gained `spec.rules[].sourceNamespace`; `ClusterWatchRule` is cluster-scoped only and lost its scope
choice. There is deliberately **no migration tool**: the conversion is cross-kind, so a conversion
webhook cannot perform it.

```yaml
# WatchRule — each rule item names the source namespace it watches.
spec:
  rules:
    - resources: [configmaps]              # omitted → this WatchRule's own namespace
    - resources: [secrets]
      sourceNamespace: repo-config         # one admitted source namespace
    - resources: [deployments]
      sourceNamespace: "*"                 # every namespace the GitTarget admits, live
```

### Two capabilities are removed on purpose

- **Platform-authored namespaced mirroring from outside the tenant namespace.** A `ClusterWatchRule`'s
  cross-namespace `targetRef` let a platform team mirror a tenant's namespaced resources with no
  object in that tenant's namespace. A platform administrator may still own the manifest, but it must
  now live in the tenant namespace.
- **Rule-author-declared all-namespace watching.** `scope: Namespaced` let the rule author reach every
  namespace. The replacement is destination-declared:
  `GitTarget.spec.allowedSourceNamespaces: {selector: {}}` plus `sourceNamespace: "*"` — same reach,
  declared by the `GitTarget` owner rather than by the rule author, and legible on the target.

### `ClusterWatchRule.spec.rules[].scope` is retained for one release as a loud rejection

The field was not deleted, because deleting it is the **silent** option: CRD pruning happens on
write, so a re-applied legacy manifest would be accepted with the value dropped — no error anywhere —
and the rule would quietly stop mirroring namespaced objects.

It is now optional, defaults to `Cluster`, and its enum accepts only `Cluster`. Applying
`scope: Namespaced` is **rejected at apply time**, and a stored one is refused at compile with
`ClusterScopeOnly`. The field is removed entirely one release from now, or at `v1beta1`.

No such shim exists for `WatchRule.spec.sourceNamespace`, and none is needed: that field never
reached a release, so no stored object can carry it and no manifest in the wild sets it. It is simply
absent — the source namespace lives on `spec.rules[].sourceNamespace`.

### `ClusterProvider.spec.allowWatchRuleSourceNamespaceOverride` → `allowSourceNamespaceOverride`

A plain rename with no deprecation shim: the field never reached a release, so no stored object can
carry the old key. Semantics, and the `false` default, are unchanged. It is required for **every**
cross-source-namespace request, including `sourceNamespace: "*"`.

### Migration

List every affected object and its target:

```bash
kubectl get clusterwatchrules -o json | jq -r '
  .items[]
  | select(any(.spec.rules[]; .scope == "Namespaced"))
  | "\(.metadata.name)\ttarget=\(.spec.targetRef.namespace)/\(.spec.targetRef.name)\tnamespaced-rules=\(
      [.spec.rules[] | select(.scope == "Namespaced") | (.resources | join(","))] | join(" | ")
    )"'
```

For each one:

1. **Split it.** Any cluster-scoped items stay behind in a revised `ClusterWatchRule` (drop their
   `scope:` line — it defaults to `Cluster`). If none remain, delete the object.
2. **Create a `WatchRule` in the TENANT namespace** — the namespace of the `GitTarget` the rule
   targets — carrying the namespaced items. Each becomes `sourceNamespace: "*"` to keep cluster-wide
   reach, or an explicit name where you know the one namespace you meant.
3. **Declare the target's policy.** This is the step to get right:
   **a `GitTarget` that declares no `allowedSourceNamespaces` admits NOTHING to a `"*"` item, and a
   declared policy is exhaustive with no self-namespace exception.** Converting without also
   declaring the policy therefore *narrows production data*. Use `selector: {}` for "every source
   namespace", `names: [...]` for an explicit set, and remember to include any co-resident legacy
   `WatchRule`'s own namespace.
4. **Delegate on the provider.** Set `allowSourceNamespaceOverride: true` on the `ClusterProvider`
   the target mirrors through; without it every non-own-namespace item, `"*"` included, is refused.

A narrowing that slips through is visible rather than silent — `SourceNamespaceAuthorized=False`,
`Stalled=True`, streams stopped, and a message naming the failing item — but the documents already in
Git are governed by `GitTarget.spec.prune.mode`, which ships in the same release and defaults to
`OnEvent`: prior documents are **left in place** rather than swept. (The two changes are never
released apart.) Verify with `kubectl get watchrules -o wide`, whose `SourceAuthorized` column carries
the verdict.

### Rolling back

**Rolling the controller back past this release is unsupported while migrated manifests exist.** The
previous controller neither understands `rules[].sourceNamespace` (so a rule resolves to its own
namespace — a *narrower* desired set) nor has `prune.mode` (so a resync sweeps). Together that
deletes the mirrored namespaces' documents. If a rollback is unavoidable, remove or narrow the
affected `WatchRule`s **first**.

The same skew exists inside a rolling upgrade: CRDs are cluster-wide, so an old leader can observe a
new `WatchRule`, ignore `rules[].sourceNamespace`, and mirror the wrong namespace's content into Git.
**Complete the controller rollout before applying migrated manifests.**

## Unreleased — unresolved attribution is visible in Git (next minor; author identity changes)

When `attribution.enabled=true` and the operator cannot match a live watch event to an audit fact within
`attribution.grace`, the Git author is now:

```text
unknown (attribution unresolved) <attribution-unresolved@gitops-reverser.invalid>
```

Previously, this case used the configured committer identity, making an attribution miss
indistinguishable from configured-author mode. The Git **committer** remains the configured operator
identity in both cases.

Configured-author mode is unchanged: when attribution is disabled, the configured committer is still both
the Git author and committer. A real Kubernetes actor is unchanged too. Only a live event for which
attribution ran but did not resolve now has the explicit author above.

Update any automation that treats `GitOps Reverser` as meaning "attribution was not configured". Monitor
`gitopsreverser_commits_total{author_kind="unresolved"}` after the upgrade. For a live mutation that should
be attributable, this author normally means the audit policy, webhook route, source identity, Redis
connectivity, or grace window is not configured correctly. It can also represent a change with no usable
audit actor, so use the audit metrics before assigning a cause.

## Unreleased — a `patches:` block no longer refuses the folder (next minor; more folders accepted)

A kustomization declaring `patches:` used to refuse the whole `GitTarget`. Not the edit — the
**target**. A folder whose patch touched a replica count also lost `images:`/`replicas:`
edit-through, which the patch had nothing to do with.

A patch is now **tolerated as read-only build context**:

- the folder is **accepted**, and what it renders is mirrored;
- the patch file is **retained, never managed** — it is a build input, not a manifest. (It is a KRM
  document, so without this the operator would index it as one: match a live object to it, mirror a
  whole Deployment over the sparse patch, or sweep it away as an orphan.)
- `images:` / `replicas:` edit-through works in a patched folder exactly as it does anywhere else;
- an edit to a field **the patch owns** is refused *per object* — `WriteBoundaryRefused`, naming the
  file and the object — because authoring a patch is still not supported.

**Tolerating a patch is not authoring one.** Nothing is ever written into a patch file.

Exactly one shape is tolerated. The rest are refused **by name**, so the message says what to fix:

| Shape | Verdict |
|---|---|
| `patches: [{path: patch.yaml}]` — a sparse KRM document inside the tree | **tolerated** |
| `patches: [{patch: "..."}]` — inline (including an inline JSON6902 op list) | refused: `patches-inline` |
| `patches: [{path: json-patch.yaml}]` where the file is an `op`/`path`/`value` list | refused: `patches-json6902` |
| a `path:` naming no file in the tree, or escaping it | refused: `patches-outside-tree` |
| `patchesStrategicMerge:`, `patchesJson6902:` (deprecated spellings) | refused under their own names |

## Unreleased — the build's output stops leaking into the build's input (next minor; bug fix + behavior change)

**If your kustomization declares `labels:`, `commonLabels:`, `commonAnnotations:` or `namespace:`,
the operator has been writing those injected values into your source manifests.** Measured, on a
folder we accept today, with nothing changed in the cluster and nothing changed in the render:

```yaml
# kustomization.yaml (yours)          # deployment.yaml, after one reconcile (ours)
labels:                               metadata:
  - pairs:                              labels:
      env: prod                           env: prod      # <- the OVERLAY's, absorbed into the BASE
commonAnnotations:                      annotations:
  owner: platform                         owner: platform
```

The writer mirrors a live object into the file that produced it — but under kustomize that file is
not what the cluster runs, and mirroring the live object straight back writes the build's own
output into the build's input. Every reconcile of an unchanged folder produced a commit, and the
file was left wrong: delete the kustomization later and the injected values are now yours forever.
In a base shared by two overlays, the value baked in is **one environment's**.

The fix needs no model of any transformer, and it is now the rule the writer follows:

> **Where the live object and the render agree, the source keeps its bytes. Where they disagree,
> the user changed something, and that is what we write.**

**Nothing needs migration** — this is a fix, and it makes the operator stop rewriting files it
should have left alone. If a past reconcile has already baked injected metadata into a manifest,
the operator will not remove it for you; remove it by hand and it will not come back.

**Two behavior changes go with it.**

*The re-render now runs for any document a kustomization produces*, not only for one an
`images:`/`replicas:` entry governs. A change to a field the build supplies (relabelling a live
object whose label a `labels:` block sets, say) cannot be expressed in the repository: the source
file cannot hold it, because the build would stamp its own value straight back. That write now
**refuses the flush** — `GitPathAccepted=False` / `WriteBoundaryRefused`, naming the file and the
object — where before it was committed and silently never converged.

*A live change the projection cannot place is refused* (`unplaceable-edit`). It fires when the
build and the live object have **both** rewritten one list whose elements carry no unique `name:`
to pair them by — the source's `args:` rewritten by a patch, for example. There is no honest way
to say which of the source's bytes you meant to keep, and pairing the lists by position is
measurably wrong (kustomize *prepends* a container a patch adds), so the operator refuses rather
than guesses.

## Unreleased — kustomize decides what it renders, and what it touched (next minor; bug fixes + behavior change)

The write path no longer contains a re-implementation of kustomize's image and replica
transformers. It asks kustomize what a folder renders to, and — by rendering a second time
with a unique nonce written into every override entry — which entry supplied each value.
`renderImage`, `imageSuppliers`, `simulateImageRender` and `isReplicaKind` are deleted.

**Three shipped bugs go with them.** Each was a case where we believed a folder rendered
one thing while kustomize rendered another, and the projection then wrote the difference
into your source manifest as though you had typed it there.

| | If your repo has… | What was happening |
|---|---|---|
| **B1** | an `images:` entry whose `name:` is not a literal — `- name: "ap."`, `- name: ".*"`, `- name: app:v1` | A kustomization `name:` is a **regular expression**, and kustomize matches on it as one. Our matcher was string equality, so we thought the entry matched nothing while kustomize rewrote the image. We read the difference as a user edit and wrote the *rendered* value into the source manifest — which then no longer matched the entry, silently killing the override. |
| **B2** | a `replicas:` entry naming a **ReplicationController** | kustomize's replica fieldspec is `[Deployment, StatefulSet, ReplicaSet, ReplicationController]` — it says so in its own error message. Ours listed three of the four. A scale on an RC was written into the source document, where the transformer overrode it right back: non-converging drift, on every reconcile, forever. |
| **B3** | an OCI **`volumes[].image.reference`**, or an **`ephemeralContainers`** entry | kustomize rewrites volume image references (measured) and does **not** rewrite ephemeral containers (measured). We had it backwards on both: we never looked at volume images, so the rendered value was mirrored back into your source file; and we treated ephemeral containers as override-governed when the source file owns them. |

**Nothing here needs migration** — these are fixes, and they make the operator stop
rewriting files it should have left alone. Check `git log` on your manifests if you want to
see whether a past reconcile touched an image or a replica count you did not change.

**Deleting a resource now also removes its `resources:` entry.** Previously the manifest was
deleted and the entry was left behind, pointing at a file that no longer exists — which
kustomize refuses to build over (*"accumulating resources … doesn't exist"*), so the folder
became undeployable and the `GitTarget` was refused on the next reconcile. Registering the
entry when a resource is created was only half the job.

The entry is removed **only when the file itself is actually gone**. A file holding several
documents survives the deletion of one of them, and its entry stays — pulling it would
un-deploy every other resource in that file.

**One more behavior change.** A write routed through a kustomization is now
re-rendered with kustomize before it is committed, and must reproduce the live object
exactly while leaving every other rendered object untouched. A write that fails that check
**refuses the flush** — `GitPathAccepted=False` / `WriteBoundaryRefused`, naming the file
and the object — rather than being committed.

This is deliberate, and it is the safe direction: a write that does not survive the
re-render is one the override entry overrides straight back on the next render, so
committing it would leave the resource permanently un-mirrored while looking like it
worked. If you see this refusal, the live state cannot be expressed in the repository as it
stands — most often because something we do not model (a `patches:` block, a
`replacements:` entry) owns the field. The refusal names it.

## Unreleased — a folder kustomize cannot build is now refused (next minor; behavior change)

The analyzer now **builds** every render root with kustomize, instead of only parsing the
kustomization structurally. A root that fails to build refuses the `GitTarget`, quoting
kustomize's own error.

This refuses folders that were previously accepted, and all of them were folders no GitOps
controller could deploy:

- a `resources:` entry that does not resolve (a manifest that was moved or renamed);
- a **diamond** — one render root reaching a shared base through two overlays — which
  kustomize rejects outright with *"may not add resource with an already registered id"*;
- a **cycle** — `a` referencing `b` referencing `a`. A cycle has no render root at all
  (every directory in it is referenced by another), so it used to be invisible rather than
  refused: nothing was built, nothing failed, and the folder passed. It is now built, and
  kustomize says *"cycle detected"*;
- an `images:` entry whose `name:` is not a valid **regular expression** — `- name: "ngin["`.
  A kustomization `name:` is a regex, not a literal string, and kustomize compiles it without
  checking the compile error, so such an entry does not fail the build, it **panics** inside
  it. We refuse the folder before the build rather than hand it over. (Note the corollary,
  which is not new but is easy to miss: `- name: "ngin."` **matches** `nginx`, because it is
  a regex.)

**Why this is a safety fix, not just strictness.** The override chain, and therefore the
write-fan-in guard, is derived from the render. A root that does not build yields no chain,
so no ambiguity is recorded, so the fan-in precondition never fires — and the operator would
write straight through into a base shared by two render paths, which is the one edit
`fan-in = 1` exists to forbid. Silently accepting an unbuildable folder disarmed the guard
protecting it.

### Migration

- Run `kustomize build` on the folder your `GitTarget` points at. If it fails, the operator
  now fails the target with `GitPathAccepted=False` / `UnsupportedContent` instead of writing
  into a folder whose render it cannot know. Fix the build.

## Unreleased — a `digest:` override no longer strips the tag out of your source file (next patch; bug fix)

**If any of your kustomizations use `images:` with `digest:`, or `newTag:` on an image
that carries a digest, the operator has been rewriting your source manifests. This stops.**

kustomize's image transformer treats tag and digest as mutually exclusive — its own code
says *"overriding tag or digest will replace both original tag and digest values"*. Our
re-implementation set the two components independently, so:

| Source image | `images:` entry | kustomize renders | We believed |
|---|---|---|---|
| `app:1.0.0` | `digest: sha256:bbb` | `app@sha256:bbb` | `app:1.0.0@sha256:bbb` |
| `app@sha256:old` | `newTag: "2.0"` | `app:2.0` | `app:2.0@sha256:old` |

Believing the wrong render, the projection compared the real live object against it,
concluded the user had *removed* the tag, and wrote the tag out of the source document —
`app:1.0.0` became `app`. On every reconcile, silently, with no refusal and no diagnostic.

### Migration

- **Check the affected files.** Any manifest referenced by a kustomization whose `images:`
  entry sets `digest:` may have lost its tag in Git. The fix stops the rewrite but does not
  restore what was already written; recover the tag from history if you need it.
- Nothing to configure. The behaviour is simply correct now, and pinned against a real
  `kustomize build` so it cannot regress.

## Unreleased — `kustomization.yaml` is now read by kustomize itself (next minor; behavior change)

The analyzer used to decode `kustomization.yaml` with a hand-written walk over a generic
YAML map, checked against a hand-maintained list of 17 unsupported keys. It now decodes with
kustomize's own type (`sigs.k8s.io/kustomize/api/types.Kustomization`) and runs the same
`Unmarshal` + `FixKustomization` sequence kustomize's builder runs, and it derives the
unsupported set by reflecting over that type: **anything not explicitly modelled refuses the
folder.**

Five verdicts change as a result. Each was verified against a real `kustomize build`, and in
every case the new behaviour is the one that agrees with the renderer:

| Kustomization contains | Before | After |
|---|---|---|
| `vars:` | **accepted** — and `$(VAR)` in a source file was silently overwritten with its substituted value | **refused** (`vars`) |
| `validators:` (plugin code) | **accepted**, unmodelled | **refused** (`validators`) |
| a `kustomization.yaml` kustomize cannot decode (e.g. `resources:` is a string, or the file is really a Flux `Kustomization` CR) | **accepted**, and written into | **refused** (`unparseable`, quoting kustomize's own error) |
| `imageTags:` / `bases:` (deprecated spellings) | `imageTags` ignored; `bases` read | both **normalised** into `images`/`resources`, as the builder does |
| a case-variant key (`newtag:`), or a blank optional component (`newName: ""`) | **refused** | **accepted** — kustomize honours both, so folders that render fine are no longer refused |

### Migration

- **The first three refuse folders that previously worked.** All three were unsafe: two let
  the operator write into a folder whose render it had misunderstood, and the third let it
  write into a folder no GitOps controller can build at all. If a `GitTarget` starts failing
  with `GitPathAccepted=False` / `UnsupportedContent`, the refusal detail now names the
  feature — and for `unparseable`, quotes kustomize's decode error verbatim.
- **`vars` has no supported replacement.** A value derived at render time has no single home
  in Git; edit the source field instead.
- Nothing to do for the last two rows: they only ever accept more.

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

### Migration

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

### Migration

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

## Unreleased — `manifest-analyzer` scan modes renamed, and `--format json` now emits a

versioned contract (next minor; breaking)

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

The JSON documents also gained a `schemaVersion` field, and one field was dropped.
(`schemaVersion` is itself gone in a later release — see "The analyzer report is a KRM
document" above, which replaces it with `apiVersion`. Read this entry as the history of a
release you are upgrading *across*, not as current advice.)

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

### Migration

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
  webhook no longer requires Redis. Without `queue.redis.addr` it runs as a no-op: CommitRequests
  claim no actor (`AuthorAttributed=False`), while the Redis-free installation's matching windows are
  configured-author. It captures submitters once Redis is configured. Previously enabling admission
  without Redis failed startup.
- The chart still rejects one invalid combination at render time: `attribution.enabled=true` without
  `queue.redis.addr` fails `helm install`/`upgrade` with an actionable message (attributed-author mode
  cannot run without Redis) instead of crash-looping the pod.
- `quickstart.namespace` now defaults to `gitops-reverser-quickstart-demo` (was `default`), and a new
  `quickstart.createNamespace` (default `false`) controls whether the chart creates it.

### Migration

- To keep the previous behavior, set the values explicitly: `--set queue.redis.addr=valkey:6379
  --set queue.redis.auth.existingSecret=valkey-auth --set servers.admission.enabled=true`.
- `helm upgrade --reuse-values` preserves your existing settings, so reused-value upgrades are
  unaffected; only fresh installs (or upgrades that re-specify values) pick up the new defaults.

## Unreleased — API group version bumped `v1alpha2` → `v1alpha3` (next minor; breaking)

The served API version moved from `configbutler.ai/v1alpha2` to `configbutler.ai/v1alpha3` to
reflect the accumulated schema and status changes on this branch. `v1alpha2` is **removed**, not
co-served — there is no conversion webhook, so the old version stops being recognized once the new
CRDs are applied.

### Migration

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

#### Migration

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

#### Migration

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
webhook captured the submitter. `AuthorAttributed=False` with reason `CommitterFallback` means capture
ran but found no record; `AuthorCaptureDisabled` means capture is not configured. Neither is a failed
request.

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

#### Migration

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

#### Migration

- **Recommended:** add a real `known_hosts` to the credentials Secret, or supply it via
  `GitProvider.spec.knownHostsRef` / an install-level default ConfigMap, then delete the obsolete
  `insecure_ignore_host_key` key.
- **Dev/throwaway clusters only:** set `--insecure-allow-missing-known-hosts` on the controller and
  remove the Secret key. Never set this flag in production.
- If you relied on the old key to mask a malformed `known_hosts`, fix the `known_hosts` content — it
  must now parse.
