# Automated Release Guide

Complete guide to GitOps Reverser's automated release system with semantic versioning.

## Quick Start

### Prerequisites Setup

**Required:** Enable GitHub Actions to create pull requests.

#### If the Setting is Greyed Out

Your organization has disabled this at the org level. Enable it in this order:

1. **Organization Settings** (requires admin/owner permissions):
   - Go to: `https://github.com/organizations/YOUR_ORG/settings/actions`
   - Under "Workflow permissions": ✅ **"Allow GitHub Actions to create and approve pull requests"**
   - Click **Save**

2. **Repository Settings** (should now be available):
   - Go to: `https://github.com/YOUR_ORG/gitops-reverser/settings/actions`
   - Under "Workflow permissions": ✅ **"Allow GitHub Actions to create and approve pull requests"**
   - Click **Save**

### Test the Setup

```bash
# Create a test commit
git commit --allow-empty -m "feat: test automated releases"
git push origin main
```

Expected outcome:
1. CI runs (lint, unit tests, e2e)
2. If tests pass → Release PR created for v0.2.0
3. Merge PR → GitHub Release + Docker images published

---

## How It Works

### The Release Flow

```
Push to main
    ↓
[CI: Build & Test + E2E]
    ↓ (tests pass)
[release-please analyzes commits]
    ↓ (version bump needed)
[Create/Update Release PR]
    ↓ (human reviews & merges)
[Create GitHub Release + Tag]
    ↓
[Build & Push Docker Images]
```

### What Happens When You Push

**On every push to `main`:**

1. **CI Tests Run** (`.github/workflows/ci.yml`):
   - Lint: `make lint` with golangci-lint
   - Unit tests: `make test` (>90% coverage required)
   - E2E tests: `make test-e2e` in Kind cluster

2. **Release Analysis** (if tests pass):
   - release-please analyzes commits since last release
   - Determines version bump based on commit types
   - Creates/updates Release PR if needed

3. **Release PR Contents**:
   - Auto-generated CHANGELOG.md updates
   - Updated `charts/gitops-reverser/Chart.yaml` versions
   - Summary of all changes

4. **When Release PR is Merged**:
   - GitHub Release created with tag (e.g., `v0.2.0`)
   - Docker images built for linux/amd64 and linux/arm64
   - Images pushed to `ghcr.io` with tags: `0.2.0`, `0.2`, `0`, `latest`

---

## Conventional Commits

### Format

```
<type>(<optional scope>): <description>

[optional body]

[optional footer(s)]
```

### Commit Types & Version Bumps

| Type | Version Bump | Example |
|------|--------------|---------|
| `feat` | **Minor** (0.1.0 → 0.2.0) | `feat(controller): add multi-repo support` |
| `fix` | **Patch** (0.1.0 → 0.1.1) | `fix(webhook): handle timeout` |
| `feat!` or `BREAKING CHANGE:` | **Major** (0.1.0 → 1.0.0) | `feat!: redesign API` |
| `docs` | No bump | `docs: update README` |
| `style` | No bump | `style: format code` |
| `refactor` | No bump | `refactor: simplify logic` |
| `perf` | **Patch** (0.1.0 → 0.1.1) | `perf: optimize loop` |
| `test` | No bump | `test: add unit tests` |
| `build` | No bump | `build: update deps` |
| `ci` | No bump | `ci: improve workflow` |
| `chore` | No bump | `chore: update .gitignore` |
| `revert` | **Patch** (0.1.0 → 0.1.1) | `revert: undo feature X` |

### Examples

**Feature (Minor Bump):**
```bash
git commit -m "feat(controller): add multi-repository support

Allows configuring different Git repos for different namespaces,
improving flexibility in audit trail organization.

Closes #42"
```

**Bug Fix (Patch Bump):**
```bash
git commit -m "fix(webhook): prevent race condition in event queue

The event queue could process events out of order. This adds proper locking.

Fixes #123"
```

**Breaking Change (Major Bump):**
```bash
git commit -m "feat!: redesign GitRepoConfig API

BREAKING CHANGE: The GitRepoConfig CRD uses a different schema.
Users must migrate using the provided script.

Migration guide: docs/migration-v1.md"
```

---

## Troubleshooting

### Error: "GitHub Actions is not permitted to create or approve pull requests"

**Solution:** Enable at org level first (if greyed out), then repo level. See [Prerequisites Setup](#prerequisites-setup) above.

### No Release PR Created

**Check:**
- ✅ CI tests passed (green checkmark in Actions tab)
- ✅ Commit used conventional format (`feat:`, `fix:`, etc.)
- ✅ Pushed to `main` branch
- ✅ GitHub Actions has PR creation permissions

**Retry:**
```bash
# Check workflow status
gh run list --branch main --limit 5

# Re-run failed workflow
gh run rerun <run-id>

# Or push new commit
git commit --allow-empty -m "feat: trigger release"
git push origin main
```

### Tests Failed

Release process won't proceed if tests fail. Fix tests first:

```bash
# Run tests locally
make lint
make test
make test-e2e

# Fix issues, then push
git add .
git commit -m "fix: resolve test failures"
git push origin main
```

### Wrong Version in Release PR

**Causes:**
- Multiple commit types (most significant wins)
- `!` or `BREAKING CHANGE:` triggers major bump
- `.release-please-manifest.json` out of sync

**Solution:** Edit Release PR version or close it and retrigger.

### Docker Build Failed

**Check:**
- Build logs in Actions tab
- Multi-arch build issues (amd64/arm64)
- Registry authentication (GITHUB_TOKEN permissions)

```bash
# View logs
gh run view --log

# Manually retrigger
gh run rerun <run-id>
```

---

## Best Practices

### 1. Write Clear Commit Messages

✅ **Good:**
```
feat(controller): add support for custom branch names

Users can now specify different branch names for different GitRepoConfigs,
allowing more flexible repository organization.
```

❌ **Bad:**
```
add stuff
fix bug
wip
```

### 2. Use Scopes

Indicate which component is affected:
- `feat(controller): ...`
- `fix(webhook): ...`
- `docs(readme): ...`
- `test(integration): ...`

### 3. One Concern Per Commit

```bash
git commit -m "feat(controller): add multi-repo support"
git commit -m "docs: document multi-repo config"
git commit -m "test: add multi-repo tests"
```

### 4. Review Release PRs Carefully

Before merging:
- ✅ Verify changelog accuracy
- ✅ Check version bump is appropriate
- ✅ Ensure all commits are included
- ✅ Confirm breaking changes are documented

### 5. Coordinate Breaking Changes

For major version bumps:
1. Discuss in an issue first
2. Add migration guide to `docs/`
3. Update relevant documentation
4. Announce in discussions/Slack

---

## Files & Configuration

### Auto-Updated Files

These are automatically updated by release-please:
- `.release-please-manifest.json` - Current version
- `charts/gitops-reverser/Chart.yaml` - Helm chart versions
- `CHANGELOG.md` - Auto-generated changelog

### Configuration Files

Don't modify these without understanding the impact:
- `release-please-config.json` - Semantic versioning rules
- `.github/workflows/ci.yml` - Unified CI and release workflow

---

## Manual Release (Emergency Only)

If automation fails completely:

```bash
# 1. Tag the commit
git tag -a v0.2.1 -m "Release v0.2.1"
git push origin v0.2.1

# 2. CI will build and push Docker images

# 3. Create GitHub Release
gh release create v0.2.1 \
  --title "v0.2.1" \
  --notes "Emergency release: fix critical bug"
```

**Note:** Manual releases should be rare. Fix the automation instead.

---

## References

- [Conventional Commits Specification](https://www.conventionalcommits.org/)
- [Semantic Versioning](https://semver.org/)
- [Release Please Documentation](https://github.com/googleapis/release-please)
- [`CONTRIBUTING.md`](../CONTRIBUTING.md) - Full commit guidelines
- [`README.md`](../README.md#automated-releases) - Quick reference

---

## Questions?

1. Check this document first
2. Review existing Release PRs for examples
3. Check GitHub Actions logs for errors
4. Open an issue or discussion if stuck