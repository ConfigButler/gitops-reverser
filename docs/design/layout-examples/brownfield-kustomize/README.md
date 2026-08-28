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

[`config/gittarget.yaml`](config/gittarget.yaml) chooses `mode: Observe`. It constrains the source
to `demo`, declares that the target is single-namespace, and leaves the structural kind as `Auto`.
[`config/watchrule.yaml`](config/watchrule.yaml) supplies the namespaced resource subscription.

The first observation records a result equivalent to:

```yaml
status:
  layout:
    declaredKind: Auto
    kind: Kustomize
    renderRoot: .
    renderRootReason: SingleKustomization
```

No commit is made in `Observe` mode. After a reviewer confirms that result, changing the target to
`mode: Write` lets a new ConfigMap named `cache` produce this commit:

```text
apps/demo/
  kustomization.yaml             # adds configmap-cache.yaml to resources
  configmap-cache.yaml           # omits metadata.namespace
  deployment-web.yaml
  service-web.yaml
```

The Kustomize root supplies the omitted namespace, so the new document still represents
`demo/cache`. The first write has a home that the existing root reaches.

## Scenario contract

- Starting repository: [`repository/`](repository/), already recognized in `Observe` mode.
- Live input: [`input/configmap-cache.yaml`](input/configmap-cache.yaml).
- Expected Git change: [`expected-configmap-cache.patch`](expected-configmap-cache.patch), after a
  reviewer changes `mode` to `Write`.
- Expected status: `Ready=True` with `kind: Kustomize` and `renderRoot: .` during observation.
- Boundary: a second Kustomize root or an unsupported expression makes the target refuse the write.

## What this example rules out

This target does not infer a custom file convention from its siblings. A second Kustomize root in
the target path makes a declared `Kustomize` target invalid; it does not cause the operator to pick
an arbitrary root. A folder with no Kustomize root instead follows the `Tree` or `Flat` rule that
its declared layout permits.

The example stays within the
[Kustomize support boundary](../../support-boundary/kustomize-support-boundary.md): it edits local
declarations and verifies the resulting render. It does not invert generators, plugins, remote
bases, or chart output.
