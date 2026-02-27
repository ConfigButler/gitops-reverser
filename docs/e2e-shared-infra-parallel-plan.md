# Shared-Infra Parallel E2E Plan

## Objective

Run three E2E flows in parallel in one Kind cluster, sharing one Gitea and one Prometheus instance, without cross-run contamination.

Target flows:

- `full`
- `quickstart-helm`
- `quickstart-manifest`

## Boundary Contract (Non-Negotiable)

This is the key design boundary for this phase:

- Run context contract is only in Go E2E code.
- Cluster creation and Makefile `CTX` context are not run-context-aware yet.
- E2E Go code owns run-scoped lifecycle.
- Makefile owns shared cluster prerequisites and deployment actions that Go invokes.

Concretely, three E2E flows share one suite-level `BeforeAll` where Go will:

1. decide `runID`
2. create run namespace
3. connect to shared Gitea
4. perform Gitea setup currently in `test/e2e/scripts/setup-gitea.sh` (migrated to Go)

## Current Baseline

- Shared infra is already provisioned by Make stamp targets (`ready`, `cert-manager`, `gitea`, `prometheus`, `portforward`).
- Go E2E (`test/e2e/e2e_test.go`) currently shells into `setup-gitea.sh`.
- `setup-gitea.sh` currently does both API setup and Kubernetes secret/bootstrap work.
- The script still contains global/shared-risk behavior (`git config --global`, fixed `/tmp/e2e-ssh-key`, fixed secret names).

## Target Topology

Shared components (single instance per cluster):

- Gitea in `gitea-e2e`
- Prometheus in `prometheus-operator`
- Fixed port-forwards: `localhost:13000`, `localhost:19090`

Run-scoped components (one set per flow/run):

- Namespace: `run-<flow>-<id>`
- Repo: unique per run
- Secrets: unique names per run namespace
- Local checkout/temp paths: unique per run
- Labels: `configbutler.ai/e2e-run-id=<id>`

## Execution Model

### 1. Shared bootstrap via Make (once)

Go starts by invoking Make for shared prerequisites only:

- `$(CS)/ready`
- `$(CS)/cert-manager.installed`
- `$(CS)/gitea.installed`
- `$(CS)/prometheus.installed`
- `$(CS)/portforward.running`
- `$(CS)/image.loaded` (only when local image is required)

This keeps infra setup in Make and avoids reimplementing cluster dependency logic in Go.

### 2. Shared `BeforeAll` in Go (run context starts here)

Define and propagate a Go-only run context:

- `E2E_RUN_ID`
- `E2E_TEST_NAMESPACE`
- `E2E_GITEA_API_URL` (default `http://localhost:13000/api/v1`)

`BeforeAll` responsibilities:

- Generate stable unique `runID`.
- Create run namespace.
- Initialize Gitea artifacts for this run.
- Dispatch flow-specific deploy/install actions by calling Make targets.

### 3. Flow execution (parallel)

Run three flows in parallel using the same shared infra but distinct run contexts:

- full flow
- quickstart-helm flow
- quickstart-manifest flow

### 4. Cleanup model

- Cleanup by `runID` (namespace + run-scoped assets only).
- Never remove shared infra or shared port-forwards during per-run cleanup.
- Keep per-run artifacts/logs when a flow fails.

## Make Targets to Call from Go

Go should call Make for concrete actions, not reimplement them.

Shared prerequisites:

- Existing stamp targets listed above.

Deploy/install actions (to add/refine):

- `e2e-deploy-full` (run-scoped install/deploy for full flow)
- `$(CS)/$(NAMESPACE)/deploy-helm` (run-scoped Helm quickstart deploy)
- `deploy-installer` (run-scoped manifest quickstart deploy)

Expected inputs for these targets:

- `E2E_TEST_NAMESPACE`
- `E2E_RUN_ID`
- flow-specific names (release/resource prefixes)
- repo URL / secret refs created by Go setup

Important:

- These targets must be non-destructive to shared infra.
- They can consume run parameters, but run-context ownership remains in Go.

## Gitea Migration Plan (setup-gitea.sh -> Go)

Use `https://gitea.com/gitea/go-sdk` for API operations. Keep parity-first behavior, then harden.

### What the current script does

`setup-gitea.sh` currently performs:

1. API readiness check (`/version` retry loop)
2. org create-or-exists (`testorg`)
3. token create/reuse
4. run repo create-or-exists
5. SSH keypair generation (`/tmp/e2e-ssh-key*`)
6. delete existing Gitea user SSH keys, then add key
7. create Kubernetes secrets in target namespace:
   - HTTP secret: `git-creds`
   - SSH secret: `git-creds-ssh`
   - invalid secret: `git-creds-invalid`
8. clone repo to checkout dir and set git config (currently uses global git URL rewrite)

### Port slice 1 (get started, minimum viable)

Implement first in Go helper:

1. `WaitForGiteaAPI(ctx, apiURL)`
2. `EnsureOrg(ctx, orgName)`
3. `CreateRunToken(ctx, tokenName=e2e-<runID>)`
4. `EnsureRepo(ctx, orgName, repoName)`
5. `EnsureNamespace(ctx, runNamespace)`
6. `CreateHTTPSecret(ctx, runNamespace, secretName, username, token)`
7. `CreateInvalidSecret(ctx, runNamespace, invalidSecretName)`

This unblocks HTTP-based GitProvider flows with run-scoped credentials.

### Port slice 2 (required for SSH test parity)

Add SSH parity next:

1. Generate run-scoped SSH key material (no fixed `/tmp` names).
2. Register key in Gitea without deleting unrelated keys globally.
3. Build `known_hosts` data for `gitea-ssh.gitea-e2e.svc.cluster.local:2222`.
4. Create run-scoped SSH secret (`git-creds-ssh-<runID>` or namespace-local fixed name per run namespace).

### Port slice 3 (checkout and git config hardening)

Replace global git rewrite behavior:

- Do not use `git config --global url.*.insteadOf`.
- Use repo-local auth or per-command credential injection.
- Keep checkout dir run-scoped and disposable.

## Suite Structure in Go

Target shape in `test/e2e`:

- one shared suite `BeforeAll` for run initialization
- three flow specs (`full`, `quickstart-helm`, `quickstart-manifest`)
- shared helper package for run context + Gitea setup + Make invocation

## Metrics and Assertions Isolation

- Attach run label(s) to run-scoped resources.
- Update PromQL assertions to filter by run label/namespace.
- Avoid assertions tied to fixed shared values when they can collide across runs.

## Rollout Plan

1. Finalize boundary ownership in code structure (Go vs Make).
2. Add Go run-context helper and shared `BeforeAll` wiring.
3. Implement Gitea port slice 1.
4. Add run-scoped Make deploy targets and call them from Go.
5. Implement Gitea port slice 2 for SSH parity.
6. Move quickstart shell assertions into Go tests.
7. Add/finish parallel orchestration target (`test-e2e-all-parallel`).

## Definition of Done

- Three flows run concurrently in one cluster.
- Shared infra is initialized once and remains stable.
- Run context exists only in Go E2E code.
- Go creates namespace and triggers deploy/install through Make targets.
- Gitea setup is migrated from `setup-gitea.sh` into Go helper with at least slice 1 and slice 2 complete.
- No cross-run contamination from secrets, repo names, temp files, or cleanup.
