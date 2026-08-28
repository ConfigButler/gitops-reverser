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

| Scenario | Question it exercises |
|---|---|
| [Brownfield kustomize adoption](brownfield-kustomize/README.md) | How does an existing folder become a target without a first write? |
| [Empty-repo bootstrap](empty-repo-bootstrap/README.md) | How does an empty repository become a deployable, single-namespace folder? |
| [Tree, multi-namespace](tree-multi-namespace/README.md) | How does a small cluster mirror several namespaces and cluster resources safely? |
| [Homelab Argo CD](homelab-argocd/README.md) | How does an app-of-apps folder remain a narrow, editable target? |
| [Homelab Flux](homelab-flux/README.md) | How do Flux declarations stay editable without treating chart output as source? |
| [Overlay-scoped target](overlay-scoped-target/README.md) | How can an environment overlay change a supported field without claiming ownership of its base? **Write path, not placement** |

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

Two fields are proposed on top of the `spec.placement` the current release already has. Everything
else in these scenarios ships today.

`serializeNamespace` (`Auto`, `Always`, `Never`) decides whether `metadata.namespace` appears inside
the committed document. A path cannot express it: kustomize takes the namespace from either the
document or a governing root, and where the file sits decides neither. `Never` is honest only when
something guarantees the namespace, which is why two scenarios pair it with `Require`.

`kustomizeRoot` (`Adopt`, `Create`, `Require`) decides what happens when **no** kustomization governs
the path. When one does, every value registers the new file in its `resources:` — that is an
invariant, not a setting. `Adopt` writes the file anyway, `Create` writes the missing
`kustomization.yaml` first, and `Require` refuses the write.

Where a new file goes is the template's job, and most scenarios here declare no template at all: the
built-in ladder places a new document beside the folder's one kustomize root, or at the canonical
identity path when there is no root. The file is named `{name}.yaml`. The operator does not copy a
naming convention from the neighbouring files — that inference was deliberately deleted — so a folder
of `deployment-web.yaml` and `service-web.yaml` receives `cache.yaml`, and
`placement.default: "{kindLower}-{name}.yaml"` is how to ask for the other convention on purpose.

`suspend: true` is the adoption path: a suspended target still scans and publishes what it
resolved, and writes nothing. Clear it only after the resolved root and prospective paths match the
repository owner's intent.

Every brownfield scenario here opens suspended and shows the observed status before it shows a
patch, because that is the path the paragraph above describes and a set of examples that opened
writing would be advertising something else.
[`empty-repo-bootstrap`](empty-repo-bootstrap/README.md) is the one honest exception: its repository
is empty, so there is nothing to observe.

## One product, three names

A reader meets all three inside a single scenario, so it is worth defusing once: `configbutler.ai`
is the API group, **GitOps Reverser** is the product, and `gitops-reverser` is the repository and
binary name. Flux is uniformly `*.toolkit.fluxcd.io` and pays nothing for it; we have a split, and
these examples are where it is most visible.

The examples do not make Kustomize a general source inverse. They stay inside the support
boundary: one writable expression, a local render root, and a re-rendered proposal. See the
[support contract](../../design/support-boundary/support-contract.md) for the authoritative limits.
