# Upgrading

Breaking changes and the steps to adopt them, newest first. The machine-generated per-release
summary lives in [`CHANGELOG.md`](../CHANGELOG.md); this file is the human-written migration
guidance that the changelog's breaking-change entries link to.

We are pre-1.0, so breaking changes bump the **minor** version (release-please is configured with
`bump-minor-pre-major`) rather than the major. Read the relevant entry before upgrading across it.

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

### Migration

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

### Migration

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

### Migration

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

### Migration

- **Recommended:** add a real `known_hosts` to the credentials Secret, or supply it via
  `GitProvider.spec.knownHostsRef` / an install-level default ConfigMap, then delete the obsolete
  `insecure_ignore_host_key` key.
- **Dev/throwaway clusters only:** set `--insecure-allow-missing-known-hosts` on the controller and
  remove the Secret key. Never set this flag in production.
- If you relied on the old key to mask a malformed `known_hosts`, fix the `known_hosts` content — it
  must now parse.
