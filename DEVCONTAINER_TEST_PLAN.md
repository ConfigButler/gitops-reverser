# Dev Container Testing & Validation Plan

## 🎯 Testing Strategy

This document outlines how to test and validate the dev container setup both locally and in CI.

## 📋 Pre-Deployment Checklist

Before pushing the dev container changes to production:

### 1. Local Build Test

```bash
# Test dev container builds successfully
docker build -f .devcontainer/Dockerfile -t gitops-reverser-dev-test .

# Verify image size (should be ~1.5-2GB)
docker images gitops-reverser-dev-test

# Test all tools are installed
docker run --rm gitops-reverser-dev-test sh -c "
  go version &&
  kind version &&
  kubectl version --client &&
  kustomize version &&
  golangci-lint version &&
  helm version
"
```

Expected output:
```
✓ go version go1.25.1 linux/amd64
✓ kind v0.30.0 go1.25.1 linux/amd64
✓ Client Version: v1.32.3
✓ v5.4.1
✓ golangci-lint has version v2.4.0
✓ version.BuildInfo{Version:"v3.12.3"}
```

### 2. VS Code Dev Container Test

```bash
# 1. Open project in VS Code
code .

# 2. Reopen in Container
# Cmd+Shift+P → "Dev Containers: Reopen in Container"

# 3. Wait for container build (first time: ~5-10 min)

# 4. Open terminal in container and test
make lint
make test
make test-e2e  # Requires Docker daemon
```

Expected results:
- ✓ Container builds without errors
- ✓ All extensions load correctly
- ✓ `make lint` passes
- ✓ `make test` passes with >90% coverage
- ✓ `make test-e2e` passes (if Docker available)

### 3. Validate File Structure

```bash
# Check all required files exist
ls -la .devcontainer/Dockerfile
ls -la .devcontainer/devcontainer.json
ls -la .devcontainer/README.md
ls -la .github/workflows/devcontainer-build.yml
ls -la .github/actions/setup-devcontainer/action.yml
ls -la DEVCONTAINER_MIGRATION.md
```

### 4. Syntax Validation

```bash
# Validate YAML files
yamllint .github/workflows/devcontainer-build.yml
yamllint .github/workflows/ci.yml
yamllint .github/actions/setup-devcontainer/action.yml

# Validate Dockerfile syntax
docker build --check -f .devcontainer/Dockerfile .

# Validate JSON
jq empty .devcontainer/devcontainer.json
```

## 🚀 Deployment Steps

### Phase 1: Initial Push (No Breaking Changes)

1. **Create feature branch**
   ```bash
   git checkout -b feature/devcontainer-setup
   git add .devcontainer/ .github/ DEVCONTAINER_MIGRATION.md DEVCONTAINER_TEST_PLAN.md
   git commit -m "feat: add dev container setup with CI integration"
   git push origin feature/devcontainer-setup
   ```

2. **Wait for dev container build**
   - Navigate to Actions → "Build Dev Container"
   - Verify workflow runs successfully
   - Check image is pushed to `ghcr.io/configbutler/gitops-reverser-devcontainer:latest`

3. **Test in CI**
   - The CI workflow will use the new dev container
   - Monitor the `lint-and-test` and `e2e-test` jobs
   - Verify they complete faster than before

4. **Expected Timing**
   - First run: Dev container build ~10-15 min (one-time)
   - Subsequent runs: Jobs should be 3-5 min faster

### Phase 2: Validation

1. **Check CI job logs**
   ```
   ✓ "Pulling dev container image..." → ~10s
   ✓ "Verifying pre-installed tools" → all tools present
   ✓ "Run lint" → passes without installing golangci-lint
   ✓ "Run tests" → passes without go mod download
   ```

2. **Compare timing**
   - Before: lint-and-test ~5-7 min
   - After: lint-and-test ~2-3 min
   - Savings: ~3-4 min per job

3. **Verify caching**
   ```bash
   # Check if Docker layer cache is working
   # Re-run devcontainer-build workflow
   # Build should complete in <2 min (vs ~10 min first time)
   ```

### Phase 3: Team Rollout

1. **Merge to main**
   ```bash
   git checkout main
   git merge feature/devcontainer-setup
   git push origin main
   ```

2. **Team notification**
   - Share `DEVCONTAINER_MIGRATION.md`
   - Schedule knowledge sharing session
   - Update team documentation

3. **Monitor adoption**
   - Track dev container usage via GitHub Actions logs
   - Collect feedback from team
   - Address issues in follow-up PRs

## 🧪 Test Scenarios

### Scenario 1: Clean Build

**Goal:** Verify dev container builds from scratch

```bash
# Remove all cached layers
docker builder prune -af

# Build dev container
docker build -f .devcontainer/Dockerfile -t test-clean .

# Verify
docker run --rm test-clean go version
```

**Success Criteria:**
- ✓ Build completes in 5-10 minutes
- ✓ All tools are installed correctly
- ✓ Go modules are cached in image

### Scenario 2: Incremental Build

**Goal:** Verify layer caching works

```bash
# Make a small change to Dockerfile (e.g., add comment)
echo "# Test comment" >> .devcontainer/Dockerfile

# Rebuild
docker build -f .devcontainer/Dockerfile -t test-incremental .
```

**Success Criteria:**
- ✓ Build completes in <1 minute
- ✓ Only changed layers rebuild
- ✓ Base layers are cached

### Scenario 3: CI Integration

**Goal:** Verify CI uses dev container correctly

**Steps:**
1. Push a small code change
2. Observe CI workflow
3. Check job logs

**Success Criteria:**
- ✓ Jobs pull dev container image (<30s)
- ✓ No tool installation steps
- ✓ Tests run immediately
- ✓ Overall job time reduced by 3-5 min

### Scenario 4: Go Module Update

**Goal:** Verify dev container rebuilds when go.mod changes

```bash
# Update a Go dependency
go get -u github.com/some/package
go mod tidy
git commit -am "chore: update dependencies"
git push
```

**Success Criteria:**
- ✓ `devcontainer-build.yml` triggers automatically
- ✓ New dependencies cached in dev container
- ✓ Subsequent CI runs use updated container

### Scenario 5: Tool Version Update

**Goal:** Verify tool updates propagate correctly

**Steps:**
1. Update tool version in `.devcontainer/Dockerfile`
2. Push changes
3. Wait for dev container rebuild
4. Verify CI uses new version

**Success Criteria:**
- ✓ Dev container rebuilds automatically
- ✓ New tool version available in CI
- ✓ Local dev containers can rebuild with new version

### Scenario 6: Fallback Behavior

**Goal:** Verify CI handles missing dev container gracefully

**Steps:**
1. Temporarily make dev container image unavailable (delete tag)
2. Trigger CI workflow
3. Observe behavior

**Expected Behavior:**
- ⚠️  Warning: "Could not pull dev container image"
- ✓ Workflow continues with standard setup
- ✓ Jobs complete successfully (slower)

## 🐛 Known Issues & Workarounds

### Issue 1: First Build Slow

**Symptom:** Initial dev container build takes 10-15 minutes

**Reason:** Downloading all tools and Go modules from scratch

**Workaround:** 
- This is expected for first build
- Subsequent builds use cache (~1-2 min)
- Consider pre-warming cache in separate workflow

### Issue 2: Docker-in-Docker Permissions

**Symptom:** Kind cluster creation fails with permission errors

**Solution:**
```yaml
# Ensure --privileged flag is set
container:
  options: --privileged
```

### Issue 3: Dev Container Not Updating Locally

**Symptom:** Local dev container doesn't have latest tools

**Solution:**
```bash
# Rebuild container without cache
Cmd+Shift+P → "Dev Containers: Rebuild Container Without Cache"
```

### Issue 4: CI Uses Old Dev Container

**Symptom:** CI doesn't pick up dev container changes

**Solution:**
- Wait for `devcontainer-build.yml` to complete
- Check image tag in registry
- Verify CI pulls correct tag (`:latest` or branch-specific)

## 📊 Success Metrics

Track these metrics to validate the improvement:

### Build Time Metrics

| Metric | Before | Target | Measurement |
|--------|--------|--------|-------------|
| lint-and-test job | 5-7 min | 2-3 min | GitHub Actions logs |
| e2e-test job | 8-10 min | 5-6 min | GitHub Actions logs |
| Total CI time | 15-20 min | 10-12 min | Sum of all jobs |
| Dev container build | N/A | 10-15 min (first), <2 min (cached) | devcontainer-build workflow |

### Developer Experience Metrics

| Metric | Before | Target | Measurement |
|--------|--------|--------|-------------|
| Local setup time | 30-60 min | 10-15 min | First-time setup |
| Tool consistency | Variable | 100% | All devs use same versions |
| Environment issues | 2-3/month | 0-1/month | GitHub issues |

### Cost Metrics

| Metric | Before | Target | Measurement |
|--------|--------|--------|-------------|
| CI minutes/month | ~800 | ~600 | GitHub billing |
| Failed CI due to env | 5-10% | <2% | CI statistics |

## ✅ Final Validation Checklist

Before marking the implementation complete:

- [ ] All files created and committed
- [ ] Dev container builds successfully locally
- [ ] Dev container builds successfully in CI
- [ ] Lint job uses dev container and passes
- [ ] Test job uses dev container and passes
- [ ] E2E test job uses dev container and passes
- [ ] Build times improved by 3-5 minutes per job
- [ ] Documentation is complete and accurate
- [ ] Team has been notified of changes
- [ ] Migration guide is available
- [ ] Troubleshooting guide is available
- [ ] No regressions in existing functionality
- [ ] Production Dockerfile remains unchanged
- [ ] Dev container cache is working correctly
- [ ] All tests pass in both environments

## 🔄 Rollback Plan

If issues arise, rollback procedure:

1. **Revert CI changes**
   ```bash
   git revert <commit-hash>  # Revert ci.yml changes
   git push
   ```

2. **CI will use old setup**
   - Jobs install tools individually again
   - Slower but proven to work

3. **Local dev unaffected**
   - Dev containers are optional
   - Developers can continue with local tools

4. **Debug and fix**
   - Investigate root cause
   - Fix in feature branch
   - Re-test thoroughly
   - Re-deploy when ready

## 📞 Support

For issues or questions:
- Check `DEVCONTAINER_MIGRATION.md` troubleshooting section
- Check `.devcontainer/README.md`
- Open GitHub issue with "devcontainer" label
- Contact DevOps team