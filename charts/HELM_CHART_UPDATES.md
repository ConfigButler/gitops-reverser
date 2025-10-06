# Helm Chart Updates Summary

This document summarizes the updates made to align the GitOps Reverser Helm chart with the `config/` folder structure and make it production-ready with HA configuration.

## Changes Made

### 1. High Availability Configuration

**File: `charts/gitops-reverser/values.yaml`**

- Changed default `replicaCount` from 1 to **2** for HA deployment
- Enabled `leaderElection: true` by default (required for HA)
- Enabled `podDisruptionBudget` with `minAvailable: 1`
- Added pod anti-affinity to spread replicas across nodes:
  ```yaml
  affinity:
    podAntiAffinity:
      preferredDuringSchedulingIgnoredDuringExecution:
        - weight: 100
          podAffinityTerm:
            labelSelector:
              matchLabels:
                app.kubernetes.io/name: gitops-reverser
            topologyKey: kubernetes.io/hostname
  ```
- Updated image repository to `ghcr.io/configbutler/gitops-reverser`
- Added health probe bind address configuration

### 2. RBAC Alignment with config/rbac

**File: `charts/gitops-reverser/templates/rbac.yaml`**

- Aligned ClusterRole permissions with `config/rbac/role.yaml`:
  - Simplified to only include necessary permissions for pods and secrets
  - Removed redundant configmaps and webhook configuration permissions
  - Core CRD permissions (gitrepoconfigs, watchrules) maintained
- Cleaned up values.yaml to not include redundant RBAC rules by default

### 3. ValidatingWebhookConfiguration Alignment

**File: `charts/gitops-reverser/templates/validating-webhook.yaml`**

- Reordered fields to match `config/webhook/manifests.yaml` structure:
  - `admissionReviewVersions` first
  - `clientConfig` second
  - `failurePolicy` third
  - `name` fourth
  - `rules` fifth
  - `sideEffects` last
- Fixed cert-manager annotation to reference correct certificate name
- Maintained flexibility for namespace and object selectors

### 4. Deployment Template Updates

**File: `charts/gitops-reverser/templates/deployment.yaml`**

- Aligned with `config/manager/manager.yaml`:
  - Simplified args to use `--leader-elect` and `--health-probe-bind-address=:8081`
  - Added pod identity environment variables:
    ```yaml
    - name: POD_NAME
      valueFrom:
        fieldRef:
          fieldPath: metadata.name
    - name: POD_NAMESPACE
      valueFrom:
        fieldRef:
          fieldPath: metadata.namespace
    ```
  - Fixed health probe ports to use 8081
  - Added tmp-dir volume mount for writable filesystem
  - Simplified certificate secret reference

### 5. Certificate Management

**File: `charts/gitops-reverser/templates/certificates.yaml`**

- Updated DNS names to use webhook service name
- Simplified secret name generation using template functions
- Removed hardcoded values, now generated dynamically

**File: `charts/gitops-reverser/values.yaml`**

- Simplified certificates configuration
- Removed nested webhook.secretName, now generated automatically

### 6. Helm Chart Publishing in CI Pipeline

**File: `.github/workflows/ci.yml`** (MODIFIED)

- Added `publish-helm` job to existing CI pipeline
- Runs after `release-please` creates a release
- Integrated with existing Docker image publishing workflow
- Features:
  - Automatically copies CRDs from `config/crd/bases/` before packaging
  - Packages chart using `helm package`
  - Pushes to `oci://ghcr.io/configbutler/charts/gitops-reverser`
  - Updates the same release with Helm chart installation instructions
  - Runs in parallel with Docker image publishing

### 7. Documentation Improvements

**File: `charts/gitops-reverser/Chart.yaml`**

- Enhanced description for better discoverability
- Added keywords: gitops, kubernetes, controller, synchronization, git, reverse-sync
- Added maintainer information
- Added OCI and Artifact Hub annotations for metadata
- Included changelog in annotations

**File: `charts/gitops-reverser/README.md`**

- Comprehensive installation guide with OCI registry instructions
- Configuration examples for different scenarios:
  - Minimal (single replica for testing)
  - Production (3 replicas with resource limits)
  - Custom webhook configuration
- Troubleshooting section
- Advanced configuration options
- Usage examples and verification steps
- CRD installation and management documentation

### 7. Custom Resource Definitions (CRDs)

**Directory: `charts/gitops-reverser/crds/`** (NEW)

- Created `crds/` directory with `.gitkeep`
- CRDs are **not checked into git** (added to [`.gitignore`](../.gitignore:34))
- CRDs are automatically copied from `config/crd/bases/` during chart packaging
- Helm automatically installs CRDs from the `crds/` directory before other resources
- **Important**: CRDs are **not deleted** on `helm uninstall` (Helm best practice to prevent data loss)
- CRDs are automatically upgraded during `helm upgrade`

**Simple Automation:**

The `publish-helm` job in CI automatically copies CRDs before packaging:

```yaml
- name: Copy CRDs to Helm chart
  run: |
    mkdir -p charts/gitops-reverser/crds
    cp config/crd/bases/*.yaml charts/gitops-reverser/crds/
```

**Benefits:**
- ✅ No manual copying needed
- ✅ CRDs always fresh from source
- ✅ No git conflicts from generated files
- ✅ Single source of truth: `config/crd/bases/`
- ✅ Integrated with existing release pipeline

## Installation Instructions

### Prerequisites

```bash
# Install cert-manager (required for webhook certificates)
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.16.2/cert-manager.yaml

# Wait for cert-manager to be ready
kubectl wait --for=condition=ready pod -l app.kubernetes.io/instance=cert-manager -n cert-manager --timeout=300s
```

### Install from OCI Registry

```bash
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --namespace gitops-reverser-system \
  --create-namespace
```

### Verify Installation

```bash
# Check pods (should see 2 replicas)
kubectl get pods -n gitops-reverser-system

# Check webhook configuration
kubectl get validatingwebhookconfiguration -l app.kubernetes.io/name=gitops-reverser

# Check PodDisruptionBudget
kubectl get pdb -n gitops-reverser-system
```

## Testing Performed

1. **Helm Lint**: ✅ Passed
   ```bash
   helm lint charts/gitops-reverser
   # Output: 1 chart(s) linted, 0 chart(s) failed
   ```

2. **Template Rendering**: ✅ Verified
   - All templates render correctly
   - 2 replicas configured in Deployment
   - ValidatingWebhookConfiguration properly structured
   - Certificate and RBAC resources generated correctly
   - CRDs included in chart package
   
   3. **Configuration Validation**: ✅ Aligned
      - RBAC matches `config/rbac/role.yaml`
      - Webhook matches `config/webhook/manifests.yaml`
      - Deployment structure matches `config/manager/manager.yaml`
      - CRDs match `config/crd/bases/`

## Release Strategy

The Helm chart is automatically released to ghcr.io as part of the main CI pipeline:

1. **release-please** creates a release (when changes are pushed to main)
2. **publish** jobs build and push Docker images (multi-platform)
3. **publish-helm** job packages and pushes Helm chart (in parallel with Docker)
4. Both Docker and Helm chart information are added to the same GitHub release

Users can install using:

```bash
# Install specific version
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser \
  --version 0.3.0

# Install latest
helm install gitops-reverser \
  oci://ghcr.io/configbutler/charts/gitops-reverser
```

## Benefits

1. **Production-Ready**: HA configuration with 2 replicas by default
2. **Easy Setup**: One-command installation with sensible defaults
3. **Automatic CRD Installation**: CRDs installed automatically with the chart
4. **Proper Pod Identity**: POD_NAME and POD_NAMESPACE environment variables
5. **Aligned with Config**: Matches the structure tested in e2e tests
6. **Modern Distribution**: OCI artifacts via ghcr.io (no separate chart repository needed)
7. **Comprehensive Documentation**: Clear examples and troubleshooting guides
8. **Automated Releases**: CI/CD pipeline for chart distribution

## Migration Notes

For existing users upgrading from previous chart versions:

1. The chart now defaults to 2 replicas - adjust `replicaCount` if needed
2. Leader election is enabled by default - required for HA
3. Certificate secret names are now auto-generated
4. Health probe port changed to 8081 (was using metrics port before)
5. CRDs are now installed automatically with the chart

## Next Steps

1. Push changes to trigger the first automated release
2. Update main project README.md to reference the Helm chart installation method
3. Consider adding to Artifact Hub for better discoverability
4. Monitor the GitHub Actions workflow on first run

## Files Modified/Created

- `charts/gitops-reverser/Chart.yaml` - Enhanced metadata
- `charts/gitops-reverser/values.yaml` - HA configuration and fixes
- `charts/gitops-reverser/README.md` - Comprehensive documentation with CRD info
- `charts/gitops-reverser/templates/deployment.yaml` - Aligned with config/manager
- `charts/gitops-reverser/templates/rbac.yaml` - Aligned with config/rbac
- `charts/gitops-reverser/templates/validating-webhook.yaml` - Aligned with config/webhook
- `charts/gitops-reverser/templates/certificates.yaml` - Simplified and fixed
- `charts/gitops-reverser/crds/.gitkeep` - NEW: CRD directory placeholder
- `.gitignore` - MODIFIED: Ignore `charts/gitops-reverser/crds/*.yaml`
- `Makefile` - MODIFIED: Added `helm-sync-crds` target (optional, for local testing)
- `.github/workflows/ci.yml` - MODIFIED: Added `publish-helm` job
- `charts/HELM_CHART_UPDATES.md` - NEW: This summary document