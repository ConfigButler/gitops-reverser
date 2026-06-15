# Kilo Code Implementation Rules for GitOps Reverser

## COMMUNICATION STYLE

- **Be concise and direct** - Avoid repetitive explanations
- **Focus on actions, not discussions** - Show progress through tool use, not lengthy descriptions
- **Skip conversational phrases** - No "Great!", "Certainly!", "Okay!" - get straight to the point
- **One clear message per concept** - Don't repeat the same information in different ways

## MANDATORY PRE-COMPLETION VALIDATION

**CRITICAL**: These commands MUST pass before any implementation is considered complete:

Exception: if the change is **markdown/docs-only** and does not modify Go code, generated manifests,
Helm/chart behavior, Taskfiles, CI, shell scripts, or any executable/test configuration, you do
**not** need to run the full validation suite below. In that case, limit validation to a quick
sanity check of the edited markdown and links.

Run the e2e commands sequentially, not in parallel!

```bash
task lint      # Must pass golangci-lint checks
task test      # Must pass all unit tests + the coverage ratchet (see TESTING REQUIREMENTS)
task test-e2e  # Must pass end-to-end tests
```

If you change a GitHub Actions workflow, also run `task lint-actions`, which lints
`.github/workflows/ci.yml` with `actionlint`. Both `actionlint` and `golangci-lint`
ship in the devcontainer image.

## PRE-IMPLEMENTATION BEHAVIOR

1. **Check Docker availability for e2e tests**: Before running `task test-e2e`, verify Docker is running with `docker info` or ask user to start Docker daemon if needed
2. **Always read project context first**: Use `read_file` to understand existing patterns in target directories
3. **Search for similar implementations**: Use `search_files` to find existing patterns before writing new code
4. **Follow established architecture**: Maintain consistency with `internal/` directory structure

## CODE QUALITY REQUIREMENTS

- Follow Go naming conventions and add godoc comments for exports
- Maintain 120-character line limit (enforced by `.golangci.yml`)
- Use consistent error handling patterns from existing codebase
- Cover new code with tests; total coverage must not regress (see TESTING REQUIREMENTS)
- Write table-driven tests where appropriate

## COMPONENT-SPECIFIC RULES

### Controller Code (`internal/controller/`)
- Follow kubebuilder patterns and annotations
- Implement idempotent reconciliation logic
- Add appropriate RBAC markers
- Handle finalizers for cleanup

### Webhook Code (`internal/webhook/`)
- Implement admission webhook interface correctly
- Add proper validation/mutation logic
- Update webhook configuration in `config/webhook/`

### API Changes (`api/v1alpha1/`)
- Add kubebuilder validation tags
- Include JSON tags and field descriptions
- Run `task manifests` to update CRDs
- Test CRD installation and usage

### Git Operations (`internal/git/`)
- Handle Git errors gracefully
- Implement proper conflict resolution
- Add race condition protection
- Use temporary directories for testing

### CI Workflows (`.github/workflows/`)
- After editing any workflow, run `task lint-actions` to catch errors with
  `actionlint` before pushing
- A workflow-only change still counts as a CI/config change, so it is **not**
  covered by the markdown/docs-only validation exception

## TESTING REQUIREMENTS

- Cover new code with tests (both positive and negative cases)
- Add integration tests for complex workflows
- Follow naming convention: `TestFunctionName_Scenario(t *testing.T)`

### Coverage

- `task test` runs `cover-check`, a self-ratcheting gate: it fails if total unit coverage drops
  more than a small tolerance below `.coverage-baseline` (a committed high-water mark).
- When coverage improves, `cover-check` auto-raises `.coverage-baseline`. **Commit the bumped file**
  so the floor advances; otherwise the change is just discarded.
- e2e coverage of the deployed controller: `E2E_COVERAGE=1 task test-e2e` then
  `task e2e-coverage-collect` (writes `e2e-cover.out`).
- On PRs, Codecov reports the merged `unit` + `e2e` coverage (`codecov.yml`); its project status is
  a non-regression ratchet (compared to the base commit).

## DOCUMENTATION UPDATES

- Update README.md for user-facing changes
- Add/update godoc comments for all exports
- Update API documentation if modifying webhook behavior
- Update API documentation if modifying CRDs

## VALIDATION SEQUENCE

For markdown/docs-only edits, skip this full sequence unless the documentation change depends on or
describes behavior you also changed in code/config during the same task.

1. `task fmt` - Format code
2. `task generate` - Update generated code (if needed)
3. `task manifests` - Update CRDs (if API changes)
4. `task vet` - Run go vet
5. `task lint` - Run golangci-lint (**MANDATORY**)
6. `task test` - Run unit tests (**MANDATORY**)
7. `task test-e2e` - Run e2e tests (**MANDATORY**)

## FAILURE HANDLING

- If `task lint` fails: Run `task lint-fix` first
- If tests fail: Fix issues; keep total coverage at or above `.coverage-baseline` (commit the bump
  when it auto-raises)
- If e2e fails: Check k3d cluster setup and Docker availability
- If Docker not available: Ask user to start Docker daemon before running e2e tests

## REFERENCES

- Contributing guide: [`CONTRIBUTING.md`](./CONTRIBUTING.md)
- In the devcontainer, agents may use `gh` in read-only mode when the repo-root `.env` sets
  `GH_TOKEN`. That `.env` file is optional, local-only, and must never be committed.
