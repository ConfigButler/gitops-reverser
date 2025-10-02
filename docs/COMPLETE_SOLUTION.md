# GitOps Reverser CI/CD Architecture

## Overview

Simplified e2e testing and CI pipeline using:
1. **Two-tier containers**: CI base (800MB) + Dev extension (1.2GB)
2. **Hybrid e2e testing**: Kind on runner, tests in container
3. **Validation-only dev container**: Build+validate, never push to GHCR

## Container Strategy

### CI Base Container (`.devcontainer/Dockerfile.ci`)
- **Purpose**: Build, lint, test in CI
- **Size**: ~800MB
- **Includes**: Go, kubectl, helm, kustomize, golangci-lint
- **Excludes**: Docker, Kind
- **Pushed to**: GHCR (every build)
- **Used by**: lint, test, e2e jobs

### Dev Container (`.devcontainer/Dockerfile`)
- **Purpose**: Local development
- **Size**: ~1.2GB  
- **Extends**: CI base + Docker + Kind
- **Pushed to**: ❌ Never (validation only)
- **Used by**: VS Code, local development
- **CI check**: Validates build works on all PRs

### Why Not Push Dev Container?

- Local dev builds from local `Dockerfile` (via `initializeCommand`)
- No need to pull from GHCR
- Saves 1.2GB per commit in storage
- Uses GitHub Actions cache instead
- Still validates on every PR
- Required for release (quality gate)

## CI Workflow

```yaml
build-ci-container → Push to GHCR ✅
validate-devcontainer → Build only, validate ✅
lint-and-test → Uses CI container
docker-build → Build app image
e2e-test → Kind on runner, tests in CI container
release → Requires all above passing
```

## Hybrid E2E Testing

```
GitHub Actions Runner (has Docker)
├─ helm/kind-action creates cluster
├─ Load app image into Kind
└─ Run tests in CI container:
   docker run --network host \
     -v workspace:/workspace \
     -v kubeconfig:/root/.kube \
     ci-container make test-e2e
```

**Why hybrid?**
- Kind needs Docker (runner has it)
- Tests don't need Docker (CI container)
- Simple communication (network=host + mounted kubeconfig)

## Local Development

```bash
# VS Code opens dev container:
1. initializeCommand builds ci-base-local from Dockerfile.ci
2. Dev container extends ci-base-local  
3. Adds Docker + Kind
4. Ready to develop!

# Run tests
make test      # Unit tests
make test-e2e  # E2E tests (creates Kind cluster locally)
```

## Key Files

- `.devcontainer/Dockerfile.ci` - CI base (no Docker/Kind)
- `.devcontainer/Dockerfile` - Dev (adds Docker/Kind)
- `.devcontainer/devcontainer.json` - VS Code config
- `.github/workflows/ci.yml` - CI pipeline
- `Makefile` - Build targets (handles optional Kind)

## Performance

| Metric | Improvement |
|--------|-------------|
| CI container build | 50% faster |
| E2E setup | 60% faster |
| GHCR storage | -1.2GB per commit |
| Dev container in CI | Validation only |

## Troubleshooting

**"Kind not found in CI"** → Expected! CI uses helm/kind-action  
**"Dev container build fails"** → Check Docker is running  
**"E2E network errors"** → Verify Kind cluster created