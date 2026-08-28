# Brownfield Kustomize adoption

This scenario adopts an existing application folder before it writes. The folder already has one
supported Kustomize root and one namespace convention, so `Auto` can resolve to `Kustomize` during
an observation pass.

## Starting repository

[`repository/`](repository/) is an existing `demo` application folder. Its
[`kustomization.yaml`](repository/kustomization.yaml) supplies `namespace: demo` and lists the
Deployment and Service that it owns.

```text
apps/demo/
  kustomization.yaml
  deployment-web.yaml
  service-web.yaml
```

## Proposed configuration

[`config/gittarget.yaml`](config/gittarget.yaml) opens suspended and declares no
`placement` at all, so the built-in ladder applies: the folder's one kustomize root places new
files beside it. `serializeNamespace: Auto` and `kustomizeRoot: Adopt` are both defaults, spelled
out here because this scenario is about what adoption resolves to.
[`config/watchrule.yaml`](config/watchrule.yaml) supplies the namespaced resource subscription.

The first observation records a result equivalent to:

```yaml
status:
  conditions:
    - type: LayoutResolved
      status: "True"
      reason: SingleKustomization
      message: "render root '.' governs new files"
  placement:
    renderRoot: .
    serializeNamespace: Auto
```

No commit is made while the target is suspended. After a reviewer confirms that result, clearing
`suspend` lets a new ConfigMap named `cache` produce this commit:

```text
apps/demo/
  kustomization.yaml             # adds cache.yaml to resources
  cache.yaml           # omits metadata.namespace
  deployment-web.yaml
  service-web.yaml
```

The Kustomize root supplies the omitted namespace, so the new document still represents
`demo/cache`. The first write has a home that the existing root reaches.

## Scenario contract

- Starting repository: [`repository/`](repository/), already recognized while suspended.
- Live input: [`input/cache.yaml`](input/cache.yaml).
- Expected Git change: [`expected-cache.patch`](expected-cache.patch), after a
  reviewer changes `mode` to `Write`.
- Expected status: `Ready=True` with `LayoutResolved=True` and `renderRoot: .` during observation.
- Boundary: a second Kustomize root or an unsupported expression makes the target refuse the write.

## What this example rules out

This target does not infer a custom file convention from its siblings, and the new file's name is
where that shows. The folder holds `deployment-web.yaml` and `service-web.yaml`, so a reader might
expect `configmap-cache.yaml`. The operator writes **`cache.yaml`**: the rung names a new sibling
`{name}.yaml` and does not learn a naming convention by looking at the neighbours. That inference
was deliberately deleted, and declaring `placement.default: "{kindLower}-{name}.yaml"` is how you
ask for the other convention on purpose.

A second kustomize root in the target path is a misconfiguration of the GitTarget rather than a
placement puzzle; it does not cause the operator to pick an arbitrary root. A folder with no
kustomize root falls through to the canonical identity path.

The example stays within the
[Kustomize support boundary](../../../design/support-boundary/kustomize-support-boundary.md): it edits local
declarations and verifies the resulting render. It does not invert generators, plugins, remote
bases, or chart output.
