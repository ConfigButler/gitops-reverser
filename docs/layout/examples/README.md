# GitTarget layout examples

> **design**: concrete repository and configuration examples for the proposed `GitTarget` layout
> model. The API fields shown here are not available in the current release.
>
> Read this beside [`../model.md`](../model.md) and
> [`../api-wave.md`](../api-wave.md). The current supported Kustomize
> boundary remains in
> [`../support-boundary/kustomize-support-boundary.md`](../../design/support-boundary/kustomize-support-boundary.md).

These folders make a review question concrete: what does a `GitTarget` say about a Git folder, and
what files does that statement cause the operator to create or update?

Every scenario contains:

- `repository/`: the relevant repository subtree. Each scenario states whether it is the starting
  state or the state after the illustrated change.
- `config/`: the `GitTarget` and watcher objects that describe the target. The `GitTarget` files
  deliberately use the proposed `spec.layout` and `spec.mode` fields.
- `input/`: one representative live object or source event, **as the operator receives it from the
  API server** — not as it is written to Git.
- `expected-*.patch`: the exact change proposed for Git. It makes the write boundary reviewable
  without asking the reader to infer a file placement from prose.
- `README.md`: the decision the layout makes, the expected Git change, and the boundary that keeps
  the example safe.

Each target needs a `GitProvider` in the target's namespace. See the
[shared prerequisites](prerequisites/README.md) for the minimal provider specimen and the source
cluster rule. Connection credentials are outside the layout decision. A source `ClusterProvider`
is named only where that choice explains the scenario.

| Scenario | Layout question it exercises |
|---|---|
| [Brownfield Kustomize adoption](brownfield-kustomize/README.md) | How does an existing folder become a target without a first write? |
| [Application configuration as KRM](krm-app-configuration/README.md) | How does an empty repository become a deployable, single-namespace folder? |
| [Homelab cluster tree](homelab-cluster-tree/README.md) | How does a small cluster mirror several namespaces and cluster resources safely? |
| [Homelab Argo CD](homelab-argocd/README.md) | How does an app-of-apps folder remain a narrow, editable Kustomize target? |
| [Homelab Flux](homelab-flux/README.md) | How do Flux declarations stay editable without treating chart output as source? |
| [External base and overlay](external-base-overlay/README.md) | How can an environment overlay change a supported field without claiming ownership of its base? |

## Two fixture conventions

**The inputs are live objects, and the difference is the assertion.** A captured object carries
`uid`, `resourceVersion`, `generation`, `creationTimestamp`, `managedFields`, a populated `status`,
and often a finalizer or a controller's own annotation. None of that reaches Git. So the diff
between an `input/` fixture and its `expected-*.patch` is not decoration: it *is* the sanitization
assertion, and a corpus harness that reads both gets it without a separate test.

[`homelab-argocd`](homelab-argocd/README.md) and [`homelab-flux`](homelab-flux/README.md) keep the
full captured shape, because those are the two a Flux or Argo reviewer will look at hardest — the
first carries `argocd.argoproj.io/tracking-id`, whose leaking into Git hard-fails another
Application's sync, and the second carries `finalizers.fluxcd.io`. The other four scenarios keep
abridged inputs and say so at the top of the file.

**The patches carry no `index` lines.** A hash in a patch header is a promise about blob contents,
and a fabricated one gives `git diff` output's authority without its guarantee — a reader who tries
to `git apply` it gets a confusing failure. `diff --git`, the `---`/`+++` pair, the mode line, and
the hunks are the reviewable content. Every patch here applies cleanly to its scenario's stated
starting state, which is the property the index line was pretending to have.

## How to read the proposed fields

`layout.kind` names the folder's structural rule. `Kustomize` creates files beside one root and
adds each file to `resources:`. `Tree` writes an identity-complete namespace and type tree.
`scope` says whether the folder can receive one namespace or several. For a
`SingleNamespace` layout, `layout.namespace` records the folder's namespace identity. The exact
one-name `allowedSourceNamespaces.names` list separately authorizes that namespace, and admission
requires the two values to match. `writeNamespace` records whether `metadata.namespace` appears in
source files, instead of leaving that result to an unstated convention.

`mode: Observe` is the adoption path: the operator scans and publishes what it resolved without
writing. Change it to `Write` only after the resolved root and prospective paths match the
repository owner's intent.

Every brownfield scenario here opens in `Observe` and shows the observed `status.layout` before it
shows a patch, because that is the path the paragraph above describes and a set of examples that
opened straight into `Write` would be advertising something else.
[`krm-app-configuration`](krm-app-configuration/README.md) is the one honest exception: its
repository is empty, so there is nothing to observe.

The examples do not make Kustomize a general source inverse. They stay inside the support
boundary: one writable expression, a local render root, and a re-rendered proposal. See the
[support contract](../../design/support-boundary/support-contract.md) for the authoritative limits.
