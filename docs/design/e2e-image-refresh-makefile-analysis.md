# E2E Image Refresh and Makefile Dependency Analysis

## Goal

This note documents how `make test-e2e` currently reaches the controller image, what was
validated live, where the dependency chain loses image identity, and which Makefile-shaped
solutions are the cleanest.

The framing here is intentionally strict:

- prefer dependency-chain semantics over imperative repair logic
- prefer explicit image identity over side effects
- prefer behavior that is easy to reason about from the Make graph alone

## Executive Summary

The current setup does rebuild the local controller image when Go source files change.

However, the path from "new local image content exists" to "the running controller pod uses
that new content" is not represented cleanly in one dependency chain:

- the image build and image load path are content-aware
- the deployment step is only tag-aware
- the install steps currently contain unconditional cleanup logic, which can tear down and
  recreate the namespace and deployment

In practice this means a Go change may still be picked up, but often because the install path
recreated the deployment, not because the deployment target itself understood that the image
content had changed under the same tag.

If the goal is a simple and defensible Makefile model, the cleanest direction is:

- give each local e2e image build a content-derived tag
- inject that full tag into the rendered install manifest
- let the install target become the single declarative place where image identity changes

That makes "Go changed" naturally flow into "install manifest changed" and therefore into
"deployment spec changed" without any special-case rollout logic.

Two important scope notes:

- this problem is primarily about the local developer loop, where we rebuild from source and
  reuse a local cluster
- the GitHub build path appears to be different: it uses a prebuilt image and effectively
  pulls from `ghcr.io` immediately, so it is much closer to "new image reference -> pull ->
  rollout"

That difference matters. The local loop is where same-tag image replacement and cluster-local
image reuse become subtle. CI with a published image reference is already closer to the
desired declarative model.

## Historical Context

This area has evolved over time.

Earlier versions used `k3d image import`, and that worked fine for the original local loop.
The current [hack/e2e/load-image.sh](/workspaces/gitops-reverser/hack/e2e/load-image.sh)
exists because several real-world issues accumulated around that simpler flow, including:

- Docker-outside-of-Docker environments where `k3d image import` was unreliable
- the need to track whether the cluster nodes still had the imported image
- containerd GC behavior on reused local clusters
- the need to pin imported images after load so the stamp remains trustworthy

So this note is not arguing that `load-image.sh` was a mistake. It solved real problems.
The issue is narrower:

- `load-image.sh` became increasingly good at preserving image availability
- but the Deployment refresh path still only compares the image tag string

Those two parts are now out of alignment.

## What Was Validated

### Validation 1: image identity change under the same tag

I forced a new local image ID under the same `gitops-reverser:e2e-local` tag and reran the
same `prepare-e2e` path that `make test-e2e` uses.

Observed:

- local image ID changed
- `.stamps/cluster/<ctx>/image.loaded` changed
- the cluster eventually ran a new pod

But that successful refresh was not proof that the deploy step was correct, because the install
path had already torn down and recreated the namespace during the run.

### Validation 2: Go-only source change

I then made a temporary, harmless Go-only change in `cmd/main.go` and reran:

```bash
make CTX=k3d-gitops-reverser-test-e2e INSTALL_MODE=config-dir NAMESPACE=gitops-reverser prepare-e2e
```

Observed:

- the controller image rebuilt to a new image ID
- `hack/e2e/load-image.sh` imported the new image into k3d
- `hack/e2e/deploy-controller.sh` still printed:

```text
deployment/gitops-reverser already running gitops-reverser:e2e-local (1/1 available); skipping
```

That message is the key finding: the deploy target does not treat a content change under the
same tag as a reason to update the Deployment.

The pod still changed in that run, but only because the install recipe had already recreated the
namespace and Deployment earlier in the same invocation. That means the same-tag deployment bug
is real, but it is currently masked by broader reinstall behavior.

## Current Target Chain

### Top-level test entrypoint

`make test-e2e` does not itself prepare the cluster. It runs Go tests, and `BeforeSuite` calls
`make prepare-e2e`.

High-level chain:

```text
make test-e2e
  -> go test ./test/e2e/
    -> BeforeSuite
      -> make prepare-e2e
      -> make e2e-gitea-run-setup
```

Relevant code:

- `test/e2e/e2e_suite_test.go`
- `Makefile` target: `test-e2e`

### `prepare-e2e` aggregate

`prepare-e2e` is a small aggregate:

```text
prepare-e2e
  -> $(CS)/$(NAMESPACE)/prepare-e2e.ready
  -> portforward-ensure
```

And `prepare-e2e.ready` depends on:

```text
$(CS)/$(NAMESPACE)/prepare-e2e.ready
  -> $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/install.yaml
  -> $(CS)/image.loaded
  -> $(CS)/$(NAMESPACE)/controller.deployed
  -> $(CS)/$(NAMESPACE)/webhook-tls.ready
  -> $(CS)/$(NAMESPACE)/sops-secret.applied
```

### Shared infrastructure chain

The shared cluster and service prerequisites flow like this:

```text
$(CS)/ready
  -> start-cluster.sh

$(CS)/flux.installed
  -> $(CS)/ready

$(CS)/flux-setup.ready
  -> $(CS)/flux.installed
  -> test/e2e/setup/flux/**

$(CS)/services.ready
  -> $(CS)/flux-setup.ready
  -> test/e2e/setup/manifests/**
```

This is the long-lived base layer used by all install modes.

### Image chain

The image path is the part that is already close to what we want:

```text
GO_SOURCES
  -> .stamps/image/controller.id
  -> .stamps/image/project-image.ready
  -> .stamps/cluster/<ctx>/image.loaded
```

Important details:

- `GO_SOURCES` includes `cmd/`, `internal/`, `api/`, `go.mod`, and `go.sum`
- `.stamps/image/controller.id` rebuilds `gitops-reverser:e2e-local`
- `hack/e2e/load-image.sh` records the Docker image ID in `image.loaded`
- `load-image.sh` is content-aware: if the image ID changed, it re-imports the image

This is good Makefile behavior.

For CI / GitHub-style runs, this chain also supports a different mode:

- if `PROJECT_IMAGE` is provided and differs from the local default, the Makefile treats it as
  an external image
- in that case the image can be pulled rather than locally rebuilt
- `IMAGE_DELIVERY_MODE=pull` lets the cluster pull it directly from the registry

That means the "published image from `ghcr.io`" flow is already structurally better aligned
with content-addressed deployment than the local stable-tag flow.

### Install chain

There are three install targets:

- `$(CS)/$(NAMESPACE)/config-dir/install.yaml`
- `$(CS)/$(NAMESPACE)/helm/install.yaml`
- `$(CS)/$(NAMESPACE)/plain-manifests-file/install.yaml`

Each one starts with:

```make
$(DO_CLEANUP_INSTALLS)
```

And `DO_CLEANUP_INSTALLS` runs `hack/cleanup-installs.sh`, which:

- deletes previously rendered `install.yaml` resources
- deletes the namespace
- removes the namespace stamp directory
- deletes the project CRDs

So the install targets are not pure "render/apply if inputs changed" targets. They currently
bundle a teardown phase into the recipe itself.

### Deploy chain

The deploy target is:

```text
$(CS)/$(NAMESPACE)/controller.deployed
  -> $(CS)/$(NAMESPACE)/$(INSTALL_MODE)/install.yaml
  -> $(CS)/image.loaded
```

This looks good from the Makefile, but the shell implementation loses image identity:

- `hack/e2e/deploy-controller.sh` reads the Deployment's current image string
- if the string equals `PROJECT_IMAGE` and the Deployment is available, it skips

That means:

- `gitops-reverser:e2e-local` -> new image content under same tag
- `image.loaded` changes
- `controller.deployed` reruns
- deploy script still says "same image string, skip"

This is the precise place where the dependency chain stops being content-aware.

## Why Go Changes Are Still Often Picked Up Today

There are two overlapping mechanisms:

### Mechanism 1: the correct one

The image chain sees Go file changes:

```text
Go file change
  -> controller.id changes
  -> image.loaded changes
```

This part is correct.

### Mechanism 2: the masking side effect

The install path can tear down and recreate the namespace and Deployment in the same run.

When that happens:

- the new pod may start with the latest imported image
- it can appear as though the deploy target handled the refresh
- but in reality the deployment refresh came from reinstall side effects

This makes the system harder to reason about:

- the image path says one thing
- the deploy script says another
- the observed result may be caused by the install recipe instead

## Where the Current Model Is Weak

### 1. Image identity is tracked in stamps, but not in the Deployment spec

The current local image reference is stable:

```text
gitops-reverser:e2e-local
```

But the real identity of the built artifact is the Docker image ID recorded in:

- `.stamps/image/controller.id`
- `.stamps/cluster/<ctx>/image.loaded`

The Deployment spec does not consume that identity. It only consumes the stable tag.

That is why tags are still useful and should be preserved deliberately.

Even if the local loop is simplified, an explicit image tag remains valuable because it:

- makes logs and rollout events much easier to interpret
- makes it obvious which build a manifest intended to deploy
- aligns local and CI behavior if both carry a meaningful image reference
- avoids overloading opaque container image IDs as the only user-visible identity

### 2. Install recipes are not pure state constructors

The install targets are not just:

- render manifest
- apply manifest
- write stamp

They are:

- cleanup prior installs
- delete namespace
- delete CRDs
- recreate namespace
- reapply everything
- write stamp

That behavior can be useful operationally, but it blurs dependency semantics.

### 3. The deploy step is logically weaker than its prerequisites

`controller.deployed` depends on `image.loaded`, which is image-ID-aware.

But `deploy-controller.sh` only compares the image string:

```text
current_image == PROJECT_IMAGE
```

So a stronger upstream signal is collapsed into a weaker downstream decision.

## Recommendation: Use a Content-Derived Local Image Tag

If the goal is "simple and based on good Makefile practices", the best fit is to make the
image reference itself content-aware.

### Shape

Instead of always using:

```text
gitops-reverser:e2e-local
```

use something like:

```text
gitops-reverser:e2e-<short-sha>
```

where `<short-sha>` is derived from the built image identity or from a deterministic hash of
the relevant inputs.

Examples:

- image ID based: `gitops-reverser:e2e-2b7d3ce89bca`
- source hash based: `gitops-reverser:e2e-<hash of GO_SOURCES>`

This keeps the useful "place these tags" property:

- developers can see which build is supposed to be running
- cluster events remain readable
- local e2e and GitHub/registry-based flows can share the same mental model of "the image
  reference is the build identity"

### Why this is the cleanest option

With a content-derived tag:

- Go source changes produce a new image reference
- the rendered install manifest changes because `PROJECT_IMAGE` changed
- the Deployment spec changes declaratively
- `kubectl apply` or `helm upgrade` naturally rolls the Deployment
- `deploy-controller.sh` no longer needs to guess whether same-tag content changed

That turns the image refresh problem back into ordinary Make dependencies.

### Dependency chain after this change

```text
Go file change
  -> image identity changes
  -> PROJECT_IMAGE changes
  -> install.yaml changes
  -> Deployment spec image changes
  -> rollout happens
```

That is the simplest chain in this codebase that matches the desired mental model.

## Alternative: Keep the Stable Tag and Inject Image Identity Elsewhere

Another valid option is:

- keep `gitops-reverser:e2e-local`
- inject `controller.id` into a pod-template annotation

For example:

```yaml
metadata:
  annotations:
    e2e.configbutler.ai/controller-image-id: sha256:...
```

That would also make the Deployment content-aware.

### Pros

- keeps the familiar local tag
- no proliferation of local tags

### Cons

- the manifest says one thing in `image:`
- the real rollout trigger lives somewhere else
- it is less obvious than just using the full image reference as the identity

This is still a reasonable design, but it is slightly less direct than a content-derived tag.

## Chosen Direction

For this work, the selected direction is:

1. content-derived local image tag
2. inject that exact tag into install rendering
3. let install rendering own Deployment spec changes
4. remove the special same-tag logic from `deploy-controller.sh`

This is intentionally the simpler path.

The key consequence is:

- `deploy-controller.sh` should stop trying to infer freshness from the existing image string
- freshness should instead be expressed by the rendered image reference itself
- once the install manifest contains the new content-derived tag, normal apply / upgrade
  behavior is enough to drive rollout

That is more declarative than relying on:

- runtime checks
- imperative rollout nudges
- same-tag image replacement
- side effects from cleanup/reinstall

It also lines up better with what CI already appears to do with published images from
`ghcr.io`: a concrete image reference is the deployment identity, and Kubernetes can pull it
without depending on local image replacement semantics.

## Follow-Up Work I Would Suggest

### High priority

- Make local `PROJECT_IMAGE` content-derived instead of constant `gitops-reverser:e2e-local`
- Ensure all install modes consume that exact image reference
- Remove the deploy script's same-tag skip behavior entirely
- Keep `deploy-controller.sh` minimal, with no special-case logic for "same tag but maybe new content"

### Medium priority

- Revisit whether install targets should embed unconditional cleanup
- Separate "clean previous install" from "render/apply current install" if the goal is a more
  conventional Make graph

### Nice to have

- Add a small build-info endpoint or startup log field so tests can assert which build is
  actually running

## Bottom Line

The current system does usually pick up Go changes, but it does not express that refresh
cleanly through the Make dependency graph.

The chosen fix is to make the local image reference itself content-aware and to remove the
special same-tag logic from `deploy-controller.sh`.

Once the image tag changes when Go code changes, the rest of the Makefile can work the way
Make is best at:

- inputs change
- rendered output changes
- apply step changes cluster state
- rollout follows naturally
