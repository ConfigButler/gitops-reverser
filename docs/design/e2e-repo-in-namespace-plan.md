# E2E Repo-Per-File Design

This document keeps the core design decisions behind the repo-per-file harness.

For current status, see:

- [e2e-repo-in-namespace-follow-up-plan.md](./e2e-repo-in-namespace-follow-up-plan.md)

For the remaining active work, see:

- [e2e-finish-plan.md](./e2e-finish-plan.md)

## Intent

The harness should separate:

- shared cluster and install preparation
- mutable repo fixtures used by individual e2e files

That keeps the expensive setup centralized while reducing cross-file coupling.

## Stable Decisions

- `BeforeSuite` owns shared cluster and install preparation
- each repo-using e2e file owns one repo
- repo state is exposed as `RepoArtifacts`, not package-global repo env vars
- repo stamps live under the owning test namespace
- the demo flow stays intentionally special with its fixed repo and namespace

## Why This Design Won

This approach gave the best trade-off between isolation and practicality:

- less hidden coupling between e2e files
- better fit for Ginkgo and VS Code test discovery
- no need to duplicate cluster bootstrap logic in Go
- easier future path toward more parallel execution

## What This Design Does Not Promise

- it does not make the whole e2e package fully `ginkgo -p` safe yet
- it does not remove all shell from the harness
- it does not make every e2e file completely independent of shared services

Those remaining items are tracked in the finish plan, not here.
