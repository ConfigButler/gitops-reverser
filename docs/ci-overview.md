# CI/CD Overview

The pipeline is split by **trust level**, not by topic. One rule drives the whole design:

> Untrusted (pull-request) code may be built and tested, but it never meets a write
> token, a secret, or publishing credentials.

Everything else follows from that rule. The checks themselves come from the same
[Task graph](tasks-overview.md) you run locally — CI is just `task lint`, `task test`,
and the e2e suite executed inside the same CI container the devcontainer is built from,
so local and CI results cannot drift apart.

## The four trust zones

| Zone | Workflow | Trigger | Secrets | Writes | Purpose |
| --- | --- | --- | --- | --- | --- |
| Untrusted validation | [ci.yml](../.github/workflows/ci.yml) | `pull_request` | none | none | Full contributor validation: containers, lint, unit, e2e, image scan |
| Trusted validation | [release.yml](../.github/workflows/release.yml) → calls `ci.yml` | `push` to `main` | `GITHUB_TOKEN` | `packages` (CI base container + release-grade image digests) | The *same* jobs, plus building the release-grade multi-arch digests (per-arch, by digest) the release tail retags. The instrumented test image stays artifact-only. |
| Release & publish | [release.yml](../.github/workflows/release.yml) tail jobs | after trusted validation is green | `GITHUB_TOKEN` + OIDC | packages, releases, attestations | Version (release-please), **retag** the CI-built multi-arch digests to semver + `latest` (zero rebuilds), publish chart, sign, attest |
| Hygiene | [scorecard.yml](../.github/workflows/scorecard.yml) | weekly + `main` | none | security-events | OpenSSF Scorecard supply-chain checks |

Two properties are worth calling out:

- **One copy of the validation pipeline.** `ci.yml` runs directly for PRs and is invoked
  by `release.yml` as a [reusable workflow](https://docs.github.com/en/actions/using-workflows/reusing-workflows)
  for pushes to `main`. PR runs and main runs execute the *same jobs from the same file* —
  there is no `pr.yml`/`main.yml` pair that can drift apart.
- **Release only after everything passed.** `release.yml` chains
  `ci → release-please → publish-manifest` with `needs:`. This is deliberate: tags created
  by release-please with `GITHUB_TOKEN` never trigger other workflows (GitHub's recursion
  guard), so a separate tag-triggered release workflow would either silently not run or
  need a PAT. Chaining in one run keeps the guarantee *structural*: nothing can be
  published from a commit that did not pass the full pipeline first. The release tail
  builds nothing — the multi-arch image digests it publishes were built and scanned by the
  `ci` run of the same commit; the release only retags, signs, and attests them.

## How fork PRs work

GitHub gives fork PRs a read-only `GITHUB_TOKEN` and no secrets. The pipeline adapts by
changing the *delivery* of images, never the *checks*. The instrumented project (test)
image now travels the fork-safe artifact path on **every** run — PR and `main` alike — so
the battle-tested path is the only path. Only the CI base container and the release-grade
digests are pushed, and only on `main`:

```mermaid
flowchart LR
  classDef pr fill:#e8ecff,stroke:#5566aa,color:#000;
  classDef trusted fill:#e6f7e6,stroke:#33aa33,color:#000;

  subgraph BOTH["instrumented test image — PR and main alike"]
    B1["build locally<br/>GOCOVER=1, push: false"]:::pr --> A1["docker save → artifact"]:::pr
    A1 --> C1["later jobs docker load<br/>e2e imports into k3d"]:::pr
  end

  subgraph MAIN["push to main only (trusted)"]
    B2["build-release amd64 + arm64<br/>clean, semver, push by digest"]:::trusted --> R2["ghcr.io candidate digests"]:::trusted
    R2 --> S2["Trivy-scanned;<br/>retagged at release"]:::trusted
  end
```

- The CI base container and the project image are built from the PR's own code, so a PR
  that changes the toolchain is validated against *its own* toolchain.
- The instrumented image travels between jobs as an **artifact** (`docker save`/`docker
  load`) on every run, and e2e imports it into k3d with `IMAGE_DELIVERY_MODE=load` instead
  of pulling. It is never pushed, so no instrumented digest can be mistaken for a
  promotable release candidate.
- Nothing is published from a PR, regardless of origin. Same-repo PRs follow the exact
  same path so the two flavors can't diverge.
- The `image-refresh` e2e lane validates the local build → k3d load → rollout chain with
  a locally built image (`PROJECT_IMAGE` unset), so it works identically on fork PRs.

First-time contributors additionally need a maintainer to approve the workflow run —
that is a GitHub Actions repository setting, the last line of defense for CI-minute
abuse, not something the workflow files control.

## Release artifacts and how to verify them

On a release (release-please PR merged to `main`), `release.yml` publishes. The image
bytes are not rebuilt at release time: `build-release-amd64`/`-arm64` in the `ci` run built
and pushed the per-arch digests (and `image-scan-release` scanned them), and the release
step merges those exact digests into a multi-arch manifest tagged with the semver:

| Artifact | Where | Integrity |
| --- | --- | --- |
| Multi-arch image (`linux/amd64`, `linux/arm64`) | `ghcr.io/configbutler/gitops-reverser` | cosign keyless signature, SLSA build provenance attestation, SPDX SBOM attestation |
| Helm chart | `oci://ghcr.io/configbutler/charts/gitops-reverser` | cosign keyless signature |
| `install.yaml` + `sbom.spdx.json` | GitHub release assets | part of the signed release |

Verify an image (also embedded in every release's notes):

```bash
cosign verify \
  --certificate-identity-regexp '^https://github.com/ConfigButler/gitops-reverser/\.github/workflows/release\.yml@refs/heads/main$' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  ghcr.io/configbutler/gitops-reverser:<version>

gh attestation verify oci://ghcr.io/configbutler/gitops-reverser:<version> \
  --repo ConfigButler/gitops-reverser
```

Signing is *keyless* (Sigstore): there is no private key to store or leak. The signature
certifies "built by the `release.yml` workflow on `main` of this repository", issued via
GitHub's OIDC identity and logged in the public Rekor transparency log.

## Supply-chain hygiene

- **Every GitHub Action is pinned to a full commit SHA** (with a `# vX.Y.Z` comment);
  Dependabot's `github-actions` ecosystem bumps pin + comment together.
- **Every base image is pinned by digest** (`golang`, `alpine`, `distroless` in
  [Dockerfile](../Dockerfile), `golang-bookworm` in
  [.devcontainer/Dockerfile](../.devcontainer/Dockerfile)); Dependabot's `docker`
  ecosystem keeps the digests moving.
- **Trivy scans images** in every run: on PRs, `image-scan` scans the built project image
  (report to the job log); on `main`, `image-scan-release` scans **both** shipped arches
  (`amd64` + `arm64`) by digest and uploads SARIF to code scanning — so the exact bytes a
  release retags are scanned *before* the release. Either way the job fails on CRITICAL
  vulnerabilities that have a fix available.
- **Minimal token permissions** per job; the workflow default is `contents: read`.
- **OpenSSF Scorecard** runs weekly and on every push to `main`.

## Where things are defined

| Concern | Lives in |
| --- | --- |
| What gets checked (lint, unit, e2e, packaging) | [Taskfile-build.yml](../Taskfile-build.yml), [test/e2e/Taskfile.yml](../test/e2e/Taskfile.yml) — see [tasks-overview.md](tasks-overview.md) |
| Tool versions | [.devcontainer/Dockerfile](../.devcontainer/Dockerfile) (single source for devcontainer *and* CI) |
| Validation pipeline | [.github/workflows/ci.yml](../.github/workflows/ci.yml) |
| Release pipeline | [.github/workflows/release.yml](../.github/workflows/release.yml) |
| Hygiene | [.github/workflows/scorecard.yml](../.github/workflows/scorecard.yml), [.github/dependabot.yml](../.github/dependabot.yml) |
| Release process details | [.github/RELEASES.md](../.github/RELEASES.md) |
