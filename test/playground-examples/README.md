# Example ConfigMaps

A bundle of 10 plain `ConfigMap` manifests used to smoke-test the watch/commit
pipeline with multiple objects at once.

The manifests intentionally **omit a namespace** so it can be supplied at apply
time:

```bash
kubectl -n my-namespace apply  -f test/playground-examples
kubectl -n my-namespace delete -f test/playground-examples --ignore-not-found
```

These live **outside** `test/playground/`, so they are not part of the playground
`kustomization.yaml` and `tilt up` does not apply them automatically. From Tilt
they are applied/removed on demand via the `apply-10-cms` and `delete-10-cms`
resources (both default to the `tilt-playground` namespace, overridable with the
`TILT_CONFIGMAP_NAMESPACE` env var).
