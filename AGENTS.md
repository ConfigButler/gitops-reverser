# Kilo Code Implementation Rules for GitOps Reverser

## COMMUNICATION STYLE

- **Be concise and direct** - Avoid repetitive explanations
- **Focus on actions, not discussions** - Show progress through tool use, not lengthy descriptions
- **Skip conversational phrases** - No "Great!", "Certainly!", "Okay!" - get straight to the point
- **One clear message per concept** - Don't repeat the same information in different ways

## MANDATORY PRE-COMPLETION VALIDATION

**CRITICAL**: These commands MUST pass before any implementation is considered complete:

```bash
make lint      # Must pass golangci-lint checks
make test      # Must pass all unit tests with >90% coverage  
make test-e2e  # Must pass end-to-end tests
```

And before you are really really wrapping up, also run:

```bash
make test-e2e-quickstart-manifest
make test-e2e-quickstart-helm
```

## PRE-IMPLEMENTATION BEHAVIOR

1. **Check Docker availability for e2e tests**: Before running `make test-e2e`, verify Docker is running with `docker info` or ask user to start Docker daemon if needed
2. **Always read project context first**: Use `read_file` to understand existing patterns in target directories
3. **Search for similar implementations**: Use `search_files` to find existing patterns before writing new code
4. **Follow established architecture**: Maintain consistency with `internal/` directory structure

## CODE QUALITY REQUIREMENTS

- Follow Go naming conventions and add godoc comments for exports
- Maintain 120-character line limit (enforced by `.golangci.yml`)
- Use consistent error handling patterns from existing codebase
- Achieve >90% test coverage for all new code
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
- Run `make manifests` to update CRDs
- Test CRD installation and usage

### Git Operations (`internal/git/`)
- Handle Git errors gracefully
- Implement proper conflict resolution
- Add race condition protection
- Use temporary directories for testing

## TESTING REQUIREMENTS

- Write unit tests with >90% coverage
- Add integration tests for complex workflows
- Include both positive and negative test cases
- Follow naming convention: `TestFunctionName_Scenario(t *testing.T)`

## DOCUMENTATION UPDATES

- Update README.md for user-facing changes
- Add/update godoc comments for all exports
- Update WEBHOOK_SETUP.md if touching webhook functionality
- Update API documentation if modifying CRDs

## VALIDATION SEQUENCE

1. `make fmt` - Format code
2. `make generate` - Update generated code (if needed)
3. `make manifests` - Update CRDs (if API changes)
4. `make vet` - Run go vet
5. `make lint` - Run golangci-lint (**MANDATORY**)
6. `make test` - Run unit tests (**MANDATORY**)
7. `make test-e2e` - Run e2e tests (**MANDATORY**)

## FAILURE HANDLING

- If `make lint` fails: Run `make lint-fix` first
- If tests fail: Fix issues and ensure >90% coverage maintained
- If e2e fails: Check Kind cluster setup and Docker availability
- If Docker not available: Ask user to start Docker daemon before running e2e tests

## REFERENCES

- Full guidelines: [`DEVELOPMENT_RULES.md`](../../DEVELOPMENT_RULES.md)
- Implementation checklist: [`IMPLEMENTATION_CHECKLIST.md`](../../IMPLEMENTATION_CHECKLIST.md)
- Contributing guide: [`CONTRIBUTING.md`](../../CONTRIBUTING.md)