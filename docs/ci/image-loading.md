# E2E image loading

## How it works

`PROJECT_IMAGE` is the single control variable:

- **Not set** (local dev): the Task-driven e2e flow builds `gitops-reverser:e2e-local` from source and loads
  it into the k3d cluster.
- **Set to local default** (`E2E_LOCAL_IMAGE`): same as above.
- **Set to something else** (CI / prebuilt): treated as a provided image. The Task-driven flow skips the
  local build, pulls if not already present locally, then loads or lets k3d pull it depending
  on `IMAGE_DELIVERY_MODE`.

`IMAGE_DELIVERY_MODE`:
- `load` (default): import into k3d from the local Docker daemon
- `pull`: let Kubernetes pull from a registry at rollout time (CI with published images)

## Common invocations

| Context | Command |
|---|---|
| Local full e2e | `task test-e2e` |
| Local install smoke (helm) | `task test-e2e-quickstart-helm` |
| Local install smoke (manifest) | `task test-e2e-quickstart-manifest` |
| CI e2e | `PROJECT_IMAGE=<prebuilt> task test-e2e` |
| CI smoke | `PROJECT_IMAGE=<prebuilt> task test-e2e-quickstart-helm` |
| IDE direct | `go test ./test/e2e/...` (BeforeSuite handles prep) |

## IDE fallback

Running `go test ./test/e2e/...` directly (no `task` entrypoint) works — `BeforeSuite` detects
the missing `PROJECT_IMAGE` and calls the Task targets to prepare the cluster and image.
