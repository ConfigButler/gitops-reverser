# Patching kustomize: there is no seam, and there are thirty lines that would be one

> **design** — direction-setting; ships no code. Nothing it describes is supported today.
> Captured: 2026-07-14
> Related:
> [render-attribution.md](render-attribution.md),
> [generated-repo-map.md](generated-repo-map.md),
> [render-root-scoping.md](render-root-scoping.md),
> [kustomize-support-boundary.md](kustomize-support-boundary.md),
> [support-contract.md](support-contract.md)

> **This doc revises [render-attribution.md](render-attribution.md) §4.** That section rules
> out "get the DAG out of kustomize" on the grounds that it means a fork, and that a fork
> means re-implementation. The first half is right. **The second half is wrong**, and the
> measurement below is why: the change is ~30 lines in one file, it cannot alter rendered
> output, and it yields the field-level attribution the dye was invented to approximate.

Three routes to "make kustomize tell us more": **(a)** upstream it, **(b)** carry a patch,
**(c)** stay outside and be clever. This doc measures all three against the checkout in
`external-sources/kustomize`.

## 1. Route (c) is dead. There is no seam.

Not "awkward" — absent. Three independent walls, any one of which is fatal:

| Hoped-for seam | What the code says |
|---|---|
| A hook on `krusty.Options` | The struct has **exactly four fields**: `Reorder`, `AddManagedbyLabel`, `LoadRestrictions`, `PluginConfig` (`api/krusty/options.go:22-47`). No callback, observer, or listener. `MakeKustomizer` hardcodes its `DepProvider`. |
| Register a Go transformer that wraps the builtins | The builtin list is a **hardcoded slice literal** (`api/internal/target/kusttarget_configplugin.go:69-91`), resolved against factory maps in `api/internal/plugins/builtinhelpers`. `api/internal/...` is unreachable from our module by Go's internal rule. There is no `RegisterTransformer` of any kind; `PluginConfig` has four fields and none is a registry. |
| A custom plugin that *observes* the build | Custom transformers are appended **after** every builtin, as peers in one flat slice (`kusttarget.go:330-345`). A plugin never sees a pre-builtin state and cannot intercept one. |

So the choice is genuinely binary: **patch kustomize, or accept resource-level "ran over"
semantics forever.** Everything clever we could do from outside — the dye
([render-attribution.md](render-attribution.md) §3), leave-one-out probing, N+1 builds — is
a workaround for this absence, not an alternative to it.

## 2. The one seam that would work is already half-built

Every builtin transformer runs through one loop, `multiTransformer.Transform`
(`api/internal/target/multitransformer.go:27-41`):

```go
for _, t := range o.transformers {
    if err := t.Transform(m); err != nil { return err }
    if t.Origin != nil {
        if err := m.AddTransformerAnnotation(t.Origin); err != nil { return err }
    }
    m.DropEmpties()
}
```

`AddTransformerAnnotation` then walks **every resource in the map, unconditionally**
(`api/resmap/reswrangler.go:526-550`). That single unconditional loop is precisely where
"ran over" gets baked in — and it has **exactly one production call site**, the line above.

Everything needed to do better already exists and is already used this way elsewhere:

- `ResMap.DeepCopy()` (`api/resmap/reswrangler.go:377`) — and `kusttarget.go:385-390`
  **already deep-copies the ResMap before and after running a validator and compares it.**
  The pattern we want is upstream's own pattern, applied one loop over.
- `resource.AsYAML()` (`api/resource/resource.go:382`) for content equality; `ResMap.GetById()`
  (`reswrangler.go:214`) matches across renames via `PrevIds()`.
- The whole cost is gated on `t.Origin != nil`, and origins are only constructed when
  `len(BuildMetadata) != 0` (`kusttarget.go:131-135`). **A default `kustomize build` pays
  literally zero.**

Snapshot before, diff after, annotate only what changed: ~30 added lines, one file, no
interface change. And `multitransformer.go` has been touched **seven times in its life,
most recently in January 2022** — a four-year-stable file.

## 3. The decisive fact: one transformer instance per entry

This is the finding that changes the shape of the argument, and it is easy to miss.

Kustomize does **not** build one `ImageTagTransformer` holding all your images. It builds
**one per `images:` entry**, in file order (`kusttarget_configplugin.go:412-429`):

```go
for _, args := range kt.kustomization.Images {
    c.ImageTag = args
    p := f()
    ...
    result = append(result, p)   // one transformer instance per entry
}
```

And it is the rule, not an images-only quirk:

| Stanza | Instances | Cite |
|---|---|---|
| `images:` | **one per entry** | `kusttarget_configplugin.go:419` |
| `replicas:` | **one per entry** | `:455` |
| `patches:` | **one per entry** | `:270-278` |
| `labels:` | **one per entry** | `:294` |
| `replacements:` | *one instance for all* — the exception | `:439-441` |

Therefore the before/after diff in §2 is **not merely "did this transformer change
anything."** Because the loop iterates *entries*, a structural field-path diff inside it
yields, exactly:

> `overlays/prod/kustomization.yaml` → `images[1]` → set
> `Deployment/web` `spec.template.spec.containers[0].image` → `web:v2`

That is **tier 4** — the field-level attribution that
[generated-repo-map.md](generated-repo-map.md) §2 records as *"does not exist"* and that
[render-attribution.md](render-attribution.md) is entirely about approximating. It does not
exist in kustomize's **output**. It is one structural diff away from existing in kustomize's
**execution**. Upstream never exposed it because it never needed it: `transformerOrigin` is
built once per transformer *type* and shared across every instance
(`kusttarget_configplugin.go:88-101`), so the annotation is structurally incapable of
naming an entry even in principle. The information is there at runtime; only the reporting
throws it away.

## 4. What this does to the dye

The dye is a good idea born of a false constraint. Compare honestly:

| | The dye (render-attribution §3) | The patched loop |
|---|---|---|
| Attribution | inferred from a nonce surviving into the output | **observed directly** |
| `newTag`, `digest`, `replicas` | works | works |
| `newName` | **cannot work** — it is the join key, not a sink | works |
| `patches:` | no | **works** (one instance per patch) |
| Charset constraints | **a correctness requirement** — the nonce must survive a regex over the whole image string | none |
| Builds per render | 2 | 1 |
| Failure mode | silent mis-attribution if a nonce collides or is rewritten | none of that class |

The dye's central caveat — *sound only for pure sinks* — is a consequence of standing
outside the process and inferring. From inside the loop there are no pure-sink
restrictions, because nothing is being inferred.

**But be precise about what this buys, because it is easy to overclaim: attribution is
necessary for reversal, not sufficient.** Knowing that `patches[0]` set `spec.replicas: 3`
tells us where the value came from. It does not tell us how to *edit the patch* so it
produces `5` — for a scalar set that is mechanical, for a strategic merge with list
semantics it is not. So this does not make `patches:` supported. It removes the reason we
cannot even *see* what a patch did, which is the first of several locks on that door. The
oracle in [render-root-scoping.md](render-root-scoping.md) §3 — re-render and require the
proposal to reproduce the live object exactly — remains the gate, and remains necessary.

## 5. Fork risk: this is an observability fork, not a semantics fork

The standing objection to forking kustomize is exactly right in general and does not apply
here, and the distinction is worth naming precisely, because it is the whole argument.

**Our correctness contract is: render what the user's controller renders.** A fork that
drifts from that is not merely a maintenance cost, it is a correctness hazard — we would
propose writes against a render nobody actually deploys.

The measured skew today is **zero**:

| | kustomize `api` | how |
|---|---|---|
| Us | **v0.21.1** | `go.mod:33-34` |
| Flux | **v0.21.1** | `fluxcd/pkg/kustomize@v1.32.0` pins *and `replace`s* it — the controller links `krusty` with `LoadRestrictionsNone` + `DisabledPluginConfig()`, **byte-identical options to ours** |
| Argo (default) | **v0.21.1** | execs the `kustomize` **binary**, shipped at 5.8.1, which pins api v0.21.1 |

Now the key property: **the patch is structurally incapable of changing rendered content.**

- It only writes **annotations** — and only `config.kubernetes.io/origin` /
  `alpha.config.kubernetes.io/transformations`, which our renderer already **strips** before
  anything reaches Git ([`kustomize_render.go`](../../../internal/manifestanalyzer/kustomize_render.go),
  `collectRendered`).
- It only runs when `BuildMetadata` is non-empty — a path **upstream users never take** and
  **we always take** (we inject it into our in-memory copy of the root). Flux does not set
  it; Argo does not set it.
- It touches no transformer, no fieldspec, no merge logic. The object graph is untouched.

A fork is dangerous when it changes the thing you must match. This one cannot reach it.
Those are different risk classes and should not be priced the same.

*(Aside, and not caused by this: Argo is the real fidelity problem, and it is unrelated to
forking. It execs a user-swappable binary, at a user-chosen version, and defaults to
`LoadRestrictionsRootOnly` where we and Flux use `LoadRestrictionsNone` — so there exist
repos we render and Argo refuses. That belongs in its own doc.)*

## 6. The cost that is actually real: `replace` does not compose

The maintenance cost of the patch is small (one four-year-stable file; rebase when Flux
bumps). The cost that will bite is subtler:

**A `replace` directive is honoured only in the main module.** It is ignored when our code
is consumed as a library. We *have* a public library — `pkg/manifestanalyzer` — so a third
party importing it would silently link **upstream** kustomize, the patched loop would not
exist, the trace would come back empty, and attribution would **degrade silently to
nothing** rather than fail.

That failure mode is unacceptable and it is cheap to close: the analyzer must **probe for
the patched build at startup and fail loudly** if the trace hook is absent, rather than
quietly falling back to guesswork. Any design that carries this patch must carry that probe
with it. (This also argues for keeping the dye's *verification* half —
re-render-and-compare — regardless: it is the check that catches exactly this.)

## 7. Upstream: not hostile, but slow, and the goldens are against us

The resource-level half of this is genuinely framable as a **bug fix**, not a feature —
upstream's own documentation already describes the semantics we want while the code
implements the other one:

- `site/content/en/docs/Tasks/build_metadata.md:259` — *"the transformer that **updated** the
  resource"* (what we want).
- Same file, `:214` and the proposal — *"transformers that have **acted on** them"* / *"**touched**
  each resource"* (what the code does).
- And `:209-212` states the annotation is **alpha**: *"We are not guaranteeing that the
  annotation content will be stable during alpha, and reserve the right to make changes."*

That is a strong opening. The countervailing facts are equally concrete:

- **The existing goldens assert the current semantics.** `api/krusty/transformerannotation_test.go`
  has a `Namespace` object carrying two `PrefixTransformer` entries despite the prefix
  transformer excluding namespaces by fieldspec. Our patch deletes those annotations — i.e.
  it rewrites tests written by the feature's own author.
- **kustomize is vendored into `kubectl`.** Changing `buildMetadata` output changes
  `kubectl kustomize` output, which per `proposals/README.md` pushes toward a full KEP.
- **`CONTRIBUTING.md:216-218`**: a feature PR is not reviewable without a triaged/accepted
  issue first.
- **Staffing.** `ROADMAP.md` is titled *"Kustomize roadmap 2023-2024"*, is largely about
  understaffing, and says of this very feature area that *"due to limited staffing, we have
  been unable to drive this feature out of alpha."* The annotation has been alpha for four
  years and nobody has promoted it.
- **The precedent's price tag.** `buildMetadata` itself shipped as a **469-line in-repo
  proposal** followed by 3–5 PRs of 500–800 lines each, authored by the project owner.

Field-level attribution (§3) is a larger ask than the resource-level fix, with **zero
in-repo precedent** — the original proposal scoped itself to resource granularity
deliberately, and nothing about field-level provenance exists in the tree.

So: propose it, but do not plan around it landing.

## 8. Recommendation

1. **Carry the patch** (`replace` directive, ~30–50 lines in `multitransformer.go`),
   emitting a per-(entry, resource) changed-field-path trace. It cannot alter rendered
   content (§5), and it is the only route to tier 4 (§3).
2. **Ship the loud probe** (§6) in the same change. Silent degradation to no-attribution is
   worse than not having the feature.
3. **Keep the oracle regardless** — re-render and require byte-identical reproduction
   ([render-root-scoping.md](render-root-scoping.md) §3). Attribution may be observed;
   verification must still be independent. A patched loop that we ourselves wrote is not
   permitted to be its own witness.
4. **File the upstream issue in parallel**, framed as a bug fix against
   `build_metadata.md:259`, with a regression test as a separate first commit per
   `CONTRIBUTING.md:226-235`. Treat acceptance as upside. If it lands, our `replace`
   evaporates — which is the quiet virtue of an observability-only patch: it is
   forward-compatible with its own obsolescence.
5. **Demote the dye** from *the* attribution mechanism to the fallback for an unpatched
   build — and keep its verification half permanently.

## Still open

- **Does the trace escape the process, or stay a side-channel?** Annotating per-field
  provenance onto the resources themselves would bloat output and change what
  `RemoveBuildAnnotations` has to strip. A side-channel (`Kustomizer` returning a trace
  alongside the `ResMap`) is cleaner for us but is a public API change, which makes the
  upstream story harder. These two goals pull in opposite directions and the doc does not
  resolve it.
- **`replacements:` stays coarse** (one instance for all — §3 table). We refuse them today,
  so it costs nothing now; it would need an index inside the transformer to fix later.
- **Which version do we fork from, and what happens when Flux bumps?** Today the answer is
  trivially v0.21.1 because all three of us are there. The first divergence between Flux's
  pin and Argo's shipped binary is the moment this question gets a real answer, and we
  should decide *then* whether we track Flux or track the user.
