# E2E image loading

## How it works

`PROJECT_IMAGE` is the single control variable:

- **Not set** (local dev): Makefile builds `gitops-reverser:e2e-local` from source and loads
  it into the k3d cluster.
- **Set to local default** (`E2E_LOCAL_IMAGE`): same as above.
- **Set to something else** (CI / prebuilt): treated as a provided image. Makefile skips the
  local build, pulls if not already present locally, then loads or lets k3d pull it depending
  on `IMAGE_DELIVERY_MODE`.

`IMAGE_DELIVERY_MODE`:
- `load` (default): import into k3d from the local Docker daemon
- `pull`: let Kubernetes pull from a registry at rollout time (CI with published images)

## Common invocations

| Context | Command |
|---|---|
| Local full e2e | `make test-e2e` |
| Local install smoke (helm) | `make test-e2e-install-helm` |
| Local install smoke (manifest) | `make test-e2e-install-manifest` |
| CI e2e | `PROJECT_IMAGE=<prebuilt> make test-e2e` |
| CI smoke | `PROJECT_IMAGE=<prebuilt> make test-e2e-install-helm` |
| IDE direct | `go test ./test/e2e/...` (BeforeSuite handles prep) |

## IDE fallback

Running `go test ./test/e2e/...` directly (no Make entrypoint) works — `BeforeSuite` detects
the missing `PROJECT_IMAGE` and calls the Make targets to prepare the cluster and image.
