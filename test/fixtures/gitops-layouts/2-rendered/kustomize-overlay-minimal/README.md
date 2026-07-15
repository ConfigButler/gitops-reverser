# kustomize-overlay-minimal

The **minimal** kustomize overlay: an overlay whose kustomization declares nothing but a
`namespace`, a `resources:` pointing at a base **outside its own subtree** (`../../base`), and
an `images:` entry. No `namePrefix`/`nameSuffix`, no patches, no generators — the constructs
that make every other overlay in this corpus refuse *structurally* before its overlay nature
is ever visible.

It exists to make the `kustomize-overlay` layout observable on its own. Every other overlay
here (`rendered-manifests`, `kustomize-overlays`, `flux-monorepo`) also uses `namePrefix` or
`patches`, so `refused-structural` fires first and hides the overlay verdict. Delete those
keys and you get this: `overlay-fan-out-unsupported` — a base read from outside the subtree,
shared by two render roots.

```
base/                       # the shared base (a Deployment + a Service)
  kustomization.yaml
  deployment.yaml
  service.yaml
overlays/
  production/               # namespace + images over ../../base, nothing else
    kustomization.yaml
  staging/                  # same shape, different namespace + tag
    kustomization.yaml
```

## What it forces

- **Renderability by reading past the subtree.** An operator pointed at `overlays/production`
  must read `../../base` to render it. That is render-root scoping's read half: reads reach
  shared context; writes stay in `spec.path`. See
  [`docs/design/support-boundary/render-root-scoping.md`](../../../../../docs/design/support-boundary/render-root-scoping.md).
- **Fan-in.** `base/` is reached by both overlays, so it is shared context no single overlay
  may edit in place — the write-fan-in = 1 invariant.
- **Edit-through.** An image tag bump on the rendered Deployment belongs on the overlay's
  `images:` entry, never in the read-only base.

This fixture records no verdict; the current analyzer report for it lives in
[`../../support-today.md`](../../support-today.md).
