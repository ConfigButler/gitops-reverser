# E2E Pipeline Analysis and First-Time User Improvement Plan

## Scope

This document summarizes the current end-to-end (e2e) CI coverage and proposes improvements focused on first-time user success.

## Current CI e2e setup

The current CI workflow runs three e2e executions:

1. Full e2e behavior suite (`e2e-test`, runs `make test-e2e`)
2. Install smoke via Helm (`e2e-install-smoke` matrix: `helm`)
3. Install smoke via manifest (`e2e-install-smoke` matrix: `manifest`)

References:

- `.github/workflows/ci.yml:249`
- `.github/workflows/ci.yml:304`

## What each existing e2e flow validates

### 1) Full e2e (`make test-e2e`)

Validates broad runtime behavior including:

- Controller/webhook readiness
- Metrics and audit webhook behavior
- GitProvider/GitTarget/WatchRule behavior
- Commit/write/update/delete paths to Git
- Secret encryption flows
- CRD/ClusterWatchRule scenarios

Reference:

- `test/e2e/e2e_test.go`

Important note:

- This suite installs via `make install` + `make deploy` inside tests, not via the README quickstart `install.yaml` path.
- Reference: `test/e2e/e2e_test.go:98`

### 2/3) Install smoke matrix (`helm`, `manifest`)

Validates installation and rollout health only:

- Deployment rollout and readiness
- Pod readiness
- CRDs present
- ValidatingWebhookConfiguration present

Reference:

- `test/e2e/scripts/install-smoke.sh:103`

Important note:

- It does not validate the first-time functional journey:
  - create credentials
  - apply minimal GitProvider/GitTarget/WatchRule
  - create resource
  - verify commit in Git

## Gaps for first-time users

### Gap 1: Quickstart parity gap

- README quickstart install path is release manifest:
  - `kubectl apply -f .../releases/latest/download/install.yaml`
  - `README.md:69`
- Full behavior e2e primarily validates kustomize deploy path:
  - `test/e2e/e2e_test.go:98`

Risk:

- A user can follow quickstart install successfully, but still hit an untested first functional path.

### Gap 2: First-success path not covered by install smoke

- Install smoke stops at installation health checks.
- No assertion that a brand-new user can get first commit into Git with minimal objects.

Reference:

- `test/e2e/scripts/install-smoke.sh:103`

### Gap 3: Documentation/API drift risk (`baseFolder` vs `path`)

- README quickstart example uses `baseFolder`:
  - `README.md:126`
- API currently defines `spec.path`:
  - `api/v1alpha1/gittarget_types.go:55`
- e2e templates use `path`:
  - `test/e2e/templates/gittarget.tmpl:11`

Risk:

- First-time users copying README may fail or get confusing validation behavior depending on compatibility handling.

### Gap 4: README points to missing samples path

- README references `config/samples/`:
  - `README.md:151`
- Repository currently has no `config/samples` directory.

Risk:

- New users hit dead links and lose confidence early.

### Gap 5: Existing strategy doc is only partially implemented

- Existing design already recommends a 3-layer approach:
  - Layer 1 install smoke
  - Layer 2 quickstart flow with Gitea
  - Layer 3 scheduled external provider check
- Current CI implements Layer 1 and full e2e, but no dedicated quickstart-flow job.

Reference:

- `docs/design/quickstart-flow-e2e-strategy.md`

## Recommendation

Replace the current install-only smoke model with an install-plus-first-success model:

- Rename and evolve `install-smoke` into `install-smoke-quickstart`.
- Keep `helm` and `manifest` matrix modes.
- Keep existing rollout checks.
- Add minimal quickstart functional assertions in the same job.

This keeps CI cost close to current while directly validating the first-time user path.

## Target state

After migration, e2e CI has:

1. `e2e-install-quickstart` matrix (`helm`, `manifest`) as required PR gate.
2. `e2e-test` full suite as broad behavior coverage.
3. Optional scheduled external-provider quickstart check.

## `install-smoke-quickstart` test contract

Each matrix scenario (`helm`, `manifest`) should validate:

1. Install and rollout:
   - deployment rollout success
   - pod ready
   - required CRDs present
   - validating webhook present
2. First-time quickstart functionality:
   - create git credentials secret
   - apply minimal `GitProvider`
   - wait for `GitProvider` Ready=True
   - apply minimal `GitTarget` (`spec.path`, not `baseFolder`) with encryption enabled by default
   - set `spec.encryption.provider: sops`
   - set `spec.encryption.generateWhenMissing: true` by default for quickstart-first UX
   - wait for `GitTarget` Ready=True
   - verify encryption secret is generated automatically when missing
   - verify generated secret includes backup warning annotation
   - apply minimal `WatchRule`
   - wait for `WatchRule` Ready=True
   - create ConfigMap
   - verify corresponding YAML file exists in repo path
   - verify commit count increased by at least one
3. Basic regression path:
   - update ConfigMap and verify file/commit changed
   - delete ConfigMap and verify file removed
4. Failure UX path:
   - apply invalid git credentials scenario
   - verify status condition reason/message is actionable
5. Safety messaging path:
   - assert quickstart output and docs mention key backup is mandatory
   - assert warning text explains consequence: encrypted files are unrecoverable without the private key

## Detailed implementation plan

## Phase 0: Docs and schema parity (fast, low risk)

1. Update README quickstart example from `baseFolder` to `path`.
2. Fix `config/samples` references or add valid samples.
3. Update quickstart examples to enable encryption by default:
   - include `spec.encryption.provider: sops`
   - include `spec.encryption.generateWhenMissing: true`
4. Add explicit backup guidance in quickstart docs:
   - backup generated `SOPS_AGE_KEY` immediately and securely
   - remove backup-warning annotation only after verified backup
5. Add a docs-contract check job that validates quickstart snippets with server-side dry-run in Kind.

Deliverables:

- Updated `README.md`.
- Valid sample links or sample files.
- CI check for quickstart snippets.

Exit criteria:

- New user can copy quickstart YAML without field-name mismatch.
- Quickstart defaults to encrypted secret handling.
- Backup requirement is clearly visible in docs and quickstart guidance.

## Phase 1: Refactor install smoke into quickstart smoke

1. Replace `test/e2e/scripts/install-smoke.sh` with `install-smoke-quickstart.sh`.
2. Keep existing install verification logic intact.
3. Add a minimal quickstart scenario runner.
4. Reuse existing e2e templates where possible, but ensure quickstart parity.
5. Add deterministic assertions with explicit timeouts and retries.
6. Add assertion that encryption default is active in the quickstart path.
7. Add assertion that backup warning annotation appears on generated key secret.

Deliverables:

- New script and helper functions.
- Updated Make targets:
  - `test-e2e-install-quickstart`
  - `test-e2e-install-quickstart-helm`
  - `test-e2e-install-quickstart-manifest`
- Backward-compatible alias targets for one release cycle (optional).

Exit criteria:

- Both matrix scenarios pass with install and first commit assertions.

## Phase 2: CI migration and gating

1. Rename workflow job from `e2e-install-smoke` to `e2e-install-quickstart`.
2. Keep matrix on `helm` and `manifest`.
3. Update downstream `needs` references in release jobs.
4. Publish artifacts/logs for fast triage:
   - controller logs
   - namespace events
   - GitProvider/GitTarget/WatchRule YAML status dumps

Deliverables:

- Updated `.github/workflows/ci.yml`.
- Updated release job dependencies.

Exit criteria:

- Required PR gate covers install + first success path.

## Phase 3: Stability hardening

1. Run as required on PRs for 1-2 weeks.
2. Track flake rate and median duration.
3. Harden waits/assertions where failures are timing-related.
4. Keep full e2e unchanged until quickstart lane is stable.

Exit criteria:

- Flake rate below agreed threshold.
- No increase in false negatives.

## Phase 4: External reality check (optional but recommended)

1. Add nightly `quickstart-github` job against dedicated repo.
2. Use restricted bot credentials.
3. Create temporary branch per run and clean up.

Exit criteria:

- Periodic hosted-provider validation without blocking PRs.

## CI/runtime impact estimate

- Runtime increase versus current install smoke: moderate.
- Maintenance impact: low-to-moderate if scenario stays minimal.
- Confidence gain: high for first-time user success.

## Risk register and mitigations

1. Risk: Flakiness from async controller readiness.
   Mitigation: explicit condition polling and bounded retries.
2. Risk: Drift between quickstart docs and test fixtures.
   Mitigation: derive smoke fixtures from quickstart snippets/templates.
3. Risk: Increased CI time.
   Mitigation: keep scenario minimal, avoid duplicate heavy validations.
4. Risk: Secret-handling issues in CI.
   Mitigation: scope secrets per job and avoid exposing in logs.

## Acceptance criteria

1. `helm` quickstart smoke passes install + first commit + update + delete checks.
2. `manifest` quickstart smoke passes the same checks.
3. README quickstart YAML is API-valid against current CRDs.
4. Quickstart examples enable encryption by default.
5. Backup warning and key-backup requirement are explicitly documented.
6. Clear status messaging is asserted for at least one invalid-credentials path.
7. Release workflow dependencies point to new quickstart-smoke job name.

## Suggested execution order

1. Land docs/schema parity fixes (Phase 0).
2. Implement script/Make target refactor (Phase 1).
3. Update CI job and release dependencies (Phase 2).
4. Observe stability window and harden (Phase 3).
5. Add optional nightly hosted-provider check (Phase 4).

## Summary

Replacing install-only smoke with `install-smoke-quickstart` is the right direction. It preserves the strong install signal already in CI and adds the missing first-time-user success proof: a clean install that produces a real first commit with minimal configuration.
