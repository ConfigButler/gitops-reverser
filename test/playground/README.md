# Playground Manifests

This kustomize package holds the starter cluster objects for the local playground
loop.

- Tilt loads it with `k8s_yaml(kustomize('test/playground'))`, so edits here are
  applied live after bootstrap.
- `task playground-bootstrap` applies the same package from e2e after it has
  created the namespace, Git repo, Git credentials, and `sops-age-key`.

Keep bootstrap-owned setup out of this folder:

- do not add the `tilt-playground` namespace manifest here
- do not add the Git credential Secrets here
- do not add the `sops-age-key` Secret here

To toggle starter objects on or off, edit [kustomization.yaml](kustomization.yaml).
