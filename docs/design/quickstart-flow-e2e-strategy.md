# Quickstart Flow E2E Strategy

## Why this document exists

We currently validate core behavior with:

- unit/integration tests (`make test`)
- full e2e tests (`make test-e2e`) with Kind + Gitea + Prometheus

But we do **not** have a dedicated test focused on "new user install paths":

- install from the raw/basic Helm chart
- install from generated `dist/install.yaml` (the path shown in quickstart)

This document defines how to test those flows in CI and whether Gitea should be part of that validation.

## Goals

- Ensure first-time installation paths do not regress.
- Catch packaging/rendering/rollout failures before release.
- Keep runtime and maintenance cost reasonable.

## Non-goals

- Replacing existing full behavior e2e coverage.
- Re-testing every reconciliation scenario in this new flow.

## Current gaps

- Existing e2e deploys using `make install` + `make deploy` (kustomize path), not Helm chart install.
- `dist/install.yaml` is generated in release pipeline, but not validated as an install/rollout path in e2e.
- Quickstart user journey is not directly tested end-to-end.

## Should we add Gitea here?

Short answer: **yes, but as a second phase**.

### Option A: Install smoke only (no Gitea)

What it tests:

- Kind cluster bootstrap
- cert-manager dependency
- Helm install from `charts/gitops-reverser`
- `kubectl apply -f dist/install.yaml`
- controller rollout readiness
- CRDs and webhook objects present

Pros:

- Fast, stable, low maintenance.
- Directly validates packaging and install UX.
- Best signal per minute for quickstart regressions.

Cons:

- Does not prove end-to-end "create resource -> commit appears in git" in this specific path.

### Option B: Full quickstart flow with Gitea

What it adds:

- Create credentials secret
- Apply minimal `GitProvider` + `GitTarget` + `WatchRule`
- Create a ConfigMap
- Verify resulting commit/file in Git repo (Gitea)

Pros:

- Closest possible validation of "new user success" narrative.
- Strong confidence that install path + runtime behavior work together.

Cons:

- Higher runtime and flakiness surface.
- More setup/teardown complexity.
- Duplicates part of existing heavy e2e coverage.

### Option C: Commit to a dedicated GitHub repository

What it adds:

- Use a purpose-built GitHub repository for e2e output validation.
- Create short-lived branch per run (for example `e2e/<run-id>`).
- Configure `GitProvider` credentials for GitHub.
- Apply minimal quickstart CRs and assert commit/file appears in that branch.

Pros:

- Highest fidelity to real user setup from quickstart perspective.
- Validates network/auth/provider behavior against actual GitHub.
- Catches provider-specific issues that local Gitea cannot.

Cons:

- More operational overhead (token/key rotation, branch cleanup, rate limits).
- Higher flakiness due to external service dependency and internet variability.
- Secret handling is stricter in CI (especially for PRs from forks).

Security/ops considerations:

- Use a dedicated low-privilege bot account and repo.
- Scope credentials to one repo and minimal permissions.
- Never run secret-bearing jobs for untrusted fork PRs.
- Auto-clean old e2e branches with retention policy.

## Recommendation

Adopt a **three-layer strategy**:

1. **Layer 1 (required in CI): install smoke tests without Gitea**
2. **Layer 2 (targeted quickstart journey with Gitea): one focused scenario**
3. **Layer 3 (external reality check): periodic quickstart run against dedicated GitHub repo**

This balances reliability and confidence:

- Layer 1 catches most breakages early (chart, manifest, certs, webhook, rollout).
- Layer 2 ensures we do not disappoint new users on the full "it commits to git" story.
- Layer 3 validates real hosted-provider behavior without making every PR depend on external systems.

## Proposed test matrix

### Layer 1: `install-smoke`

Run on every PR:

- Scenario 1: Helm chart install (raw/basic values)
- Scenario 2: Generated `dist/install.yaml` install

Assertions:

- Namespace/resources created
- Deployment available and pod ready
- CRDs installed
- Validating webhook configuration exists

### Layer 2: `quickstart-e2e`

Run on main and/or nightly at first (can be promoted to PR later):

- Start fresh Kind cluster
- Install via `dist/install.yaml` (quickstart parity)
- Bring up lightweight local Git endpoint (Gitea as today)
- Apply minimal quickstart CRs
- Create test ConfigMap
- Assert git repo contains expected YAML/commit

### Layer 3: `quickstart-e2e-github`

Run on schedule (nightly) and on protected branches only:

- Start fresh Kind cluster
- Install via `dist/install.yaml`
- Configure GitHub credentials from CI secrets
- Apply minimal quickstart CRs against dedicated e2e repo
- Create test ConfigMap
- Assert commit/file appears in dedicated branch
- Optionally delete branch at end (or rely on periodic cleanup job)

## CI integration proposal

- Add dedicated Make targets:
  - `test-e2e-install-helm`
  - `test-e2e-install-manifest`
  - `test-e2e-quickstart` (includes Gitea)
  - `test-e2e-quickstart-github` (external provider validation)
- Add a new workflow job for Layer 1 and keep it mandatory.
- Add Layer 2 as non-blocking initially; promote to required once stable.
- Add Layer 3 as scheduled/protected-branch only (non-blocking for PRs).

## Success criteria

- PRs fail if Helm/basic install or `install.yaml` install cannot roll out cleanly.
- Quickstart flow test validates an actual commit path at least on main/nightly.
- External GitHub quickstart path is exercised regularly and alerts on failures.
- Runtime overhead remains acceptable and failures are actionable.

## Rollout plan

1. Implement Layer 1 install smoke tests first.
2. Land CI wiring and make it required.
3. Implement Layer 2 quickstart-with-Gitea scenario.
4. Observe flakiness for 1-2 weeks; then decide if Layer 2 should be required on PRs.
5. Add Layer 3 scheduled GitHub-repo validation with strict secret handling.
