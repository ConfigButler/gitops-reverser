# Dev Container Migration Guide

## 🎯 Overview

This document explains the dev container setup for GitOps Reverser and how to migrate from the old setup.

## 📊 Before & After Comparison

### Old Setup
```yaml
# Each CI job individually:
- Set up Go → ~1 min
- Install Kustomize → ~30s
- Cache golangci-lint → ~20s
- Download go modules → ~1-2 min
- Install Kind → ~30s
= Total: ~3-5 minutes per job
```

### New Setup
```yaml
# All tools pre-installed in container:
- Pull dev container (cached) → ~10s
- All tools ready → 0s
- Go modules cached in image → 0s
= Total: ~10 seconds per job
```

**Savings: ~3-5 minutes per job × 3 jobs = 9-15 minutes per CI run**

## 🏗️ Architecture Overview

### Three Separate Images

1. **Dev Container** (`.devcontainer/Dockerfile`)
   - Purpose: Development + CI/CD
   - Size: ~2GB
   - Contains: All tools, Kind, kubectl, cached Go modules
   - Registry: `ghcr.io/configbutler/gitops-reverser-devcontainer`

2. **Production Image** (`Dockerfile` in root)
   - Purpose: Running the controller
   - Size: ~20MB
   - Contains: Only the compiled binary
   - Registry: `ghcr.io/configbutler/gitops-reverser`

3. **Why Separate?**
   - Dev needs tools (Kind, linters, kubectl) = bloat
   - Production needs only the binary = minimal
   - Mixing them violates single responsibility principle

## 📁 File Structure

```
.devcontainer/
├── Dockerfile              # Dev container with all tools
├── devcontainer.json       # VS Code configuration
├── post-install.sh         # (Now deprecated, logic moved to Dockerfile)
└── README.md              # Documentation

.github/
├── workflows/
│   ├── devcontainer-build.yml  # Builds and caches dev container
│   └── ci.yml                  # Updated to use dev container
└── actions/
    └── setup-devcontainer/
        └── action.yml          # Reusable action (for future use)

Dockerfile                  # Production image (unchanged)
```

## 🚀 Local Development Migration

### Old Way
```bash
# Manual tool installation on host
brew install kind kubectl kustomize
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
# etc...
```

### New Way
```bash
# Option 1: VS Code (Recommended)
1. Install "Dev Containers" extension
2. Cmd+Shift+P → "Reopen in Container"
3. All tools automatically available

# Option 2: Manual Docker
docker build -f .devcontainer/Dockerfile -t gitops-reverser-dev .
docker run -it --privileged -v $(pwd):/workspace gitops-reverser-dev bash
```

## 🔄 CI/CD Migration

### What Changed

#### New First Step: Build Dev Container

**Added:**
```yaml
build-devcontainer:
  runs-on: ubuntu-latest
  steps:
    - Build dev container for this commit
    - Push with commit SHA tag
    - Uses Docker layer cache (1-2 min rebuild)
```

#### 1. `lint-and-test` Job

**Before:**
```yaml
steps:
  - uses: actions/checkout@v5
  - uses: actions/setup-go@v6  # Downloads Go
  - run: install kustomize      # Downloads kustomize
  - uses: actions/cache@v4      # Sets up golangci-lint cache
  - run: make lint
  - run: make test
```

**After:**
```yaml
needs: build-devcontainer
container:
  image: ${{ needs.build-devcontainer.outputs.image }}
steps:
  - uses: actions/checkout@v5
  - run: make lint  # All tools already installed
  - run: make test  # Go modules already cached
```

#### 2. `e2e-test` Job

**Before:**
```yaml
steps:
  - uses: actions/checkout@v5
  - uses: actions/setup-go@v6        # Downloads Go
  - uses: helm/kind-action@v1.12.0   # Creates Kind cluster
  - run: make test-e2e
```

**After:**
```yaml
needs: [build-devcontainer, docker-build]
container:
  image: ${{ needs.build-devcontainer.outputs.image }}
  options: --privileged  # Required for Kind
steps:
  - uses: actions/checkout@v5
  - run: kind create cluster  # Kind already installed
  - run: make test-e2e
```

### Build Process (Simplified!)

**Every CI Run:**
```
1. build-devcontainer job starts
   ↓
2. Builds dev container for current commit
   ↓
3. Uses Docker layer cache (1-2 min)
   ↓
4. Pushes with commit SHA and 'latest' tags
   ↓
5. lint-and-test uses built container
   ↓
6. e2e-test uses built container
```

**Key Benefits:**
- ✅ Self-contained - no separate build workflow
- ✅ Always sound - exact container for each commit
- ✅ Fast - layer caching makes rebuilds ~1-2 min
- ✅ Simple - no fallback logic needed

## 🎁 Benefits

### For Developers
- ✅ **Zero setup time** - everything pre-installed
- ✅ **Consistency** - same environment for everyone
- ✅ **Cross-platform** - works on Windows/Mac/Linux
- ✅ **Isolation** - doesn't pollute host system

### For CI/CD
- ✅ **Faster builds** - 3-5 minutes saved per job
- ✅ **Reliability** - no flaky package downloads
- ✅ **Cost savings** - less GitHub Actions minutes
- ✅ **Consistency** - exact same env as local dev

### For Maintenance
- ✅ **Centralized versions** - update once in Dockerfile
- ✅ **Automatic propagation** - push triggers rebuild
- ✅ **Layer caching** - rebuilds are fast
- ✅ **Clear separation** - dev vs production images

## 🔧 Maintenance Tasks

### Updating Tool Versions

Edit `.devcontainer/Dockerfile`:
```dockerfile
ENV KIND_VERSION=v0.31.0 \    # Updated
    KUBECTL_VERSION=v1.33.0 \ # Updated
    ...
```

Commit and push → automatic rebuild → CI uses new version

### Updating Go Dependencies

Just update `go.mod` and `go.sum`:
```bash
go get -u ./...
go mod tidy
git commit -am "Update dependencies"
git push
```

The dev container rebuild is triggered automatically.

### Manual Rebuild

```bash
# Trigger via GitHub UI
Actions → Build Dev Container → Run workflow
```

## 🐛 Troubleshooting

### Dev Container Not Found in CI

**Symptom:** CI job fails to pull dev container image

**Solution:** 
- First run on a new branch triggers the build
- Wait for `devcontainer-build.yml` to complete
- Retry the CI job

**Fallback:** 
- The workflow is designed to gracefully handle missing images
- It will warn but continue with standard setup

### Kind Cluster Issues in E2E Tests

**Symptom:** `kind create cluster` fails

**Solution:**
- Ensure `--privileged` flag is set in container options
- Check Docker-in-Docker feature is enabled
- Verify `/var/run/docker.sock` is accessible

### Dev Container Not Building Locally

**Symptom:** VS Code fails to build container

**Solution:**
```bash
# Build manually to see error
docker build -f .devcontainer/Dockerfile -t test .

# Common issues:
# - Network problems → check internet connection
# - Disk space → docker system prune
# - Cache issues → docker build --no-cache
```

## 📚 Best Practices

### DO ✅
- Keep production Dockerfile minimal (distroless)
- Put all dev tools in dev container
- Use layer caching for faster builds
- Pin tool versions for reproducibility
- Document changes in this file

### DON'T ❌
- Mix dev tools into production Dockerfile
- Install tools in CI jobs (use dev container)
- Ignore dev container build failures
- Use `latest` tags for tools (pin versions)
- Modify production Dockerfile for dev needs

## 🔄 Migration Checklist

For team members migrating to the new setup:

- [ ] Pull latest changes
- [ ] Install "Dev Containers" VS Code extension
- [ ] Reopen workspace in container
- [ ] Verify all tools work: `make lint test test-e2e`
- [ ] Remove local tool installations (optional cleanup):
  ```bash
  # Optional: Clean up old local installations
  rm -rf ~/.kube/kind-*
  # Remove other manually installed tools
  ```
- [ ] Update your workflow documentation

## 📖 Additional Resources

- [Dev Container Docs](https://containers.dev/)
- [GitHub Actions Containers](https://docs.github.com/en/actions/using-jobs/running-jobs-in-a-container)
- [Docker BuildKit Cache](https://docs.docker.com/build/cache/)
- [Project Dev Container README](.devcontainer/README.md)

## 🤝 Getting Help

If you encounter issues:
1. Check this guide's troubleshooting section
2. Check [.devcontainer/README.md](.devcontainer/README.md)
3. Open an issue with the "devcontainer" label
4. Ask in the team chat