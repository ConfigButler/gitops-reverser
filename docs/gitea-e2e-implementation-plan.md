# Gitea E2E Implementation Plan

## Implementation Roadmap

This document provides the detailed implementation plan for integrating Gitea into the e2e testing pipeline.

## Phase 1: Infrastructure Setup

### 1.1 Helm Integration
**File**: `Makefile` (additions)
```makefile
# Tool Binaries
HELM ?= $(LOCALBIN)/helm

# Tool Versions  
HELM_VERSION ?= v3.12.3

.PHONY: helm
helm: $(HELM) ## Download helm locally if necessary.
$(HELM): $(LOCALBIN)
	@command -v helm >/dev/null 2>&1 || { \
		echo "Installing Helm $(HELM_VERSION)..."; \
		curl -fsSL https://get.helm.sh/helm-$(HELM_VERSION)-linux-amd64.tar.gz | tar -xzO linux-amd64/helm > $(HELM); \
		chmod +x $(HELM); \
	}
```

### 1.2 Gitea Values Configuration
**File**: `test/e2e/gitea-values.yaml`
```yaml
# Memory-optimized Gitea for e2e testing
persistence:
  enabled: false
memcached:
  enabled: false
postgresql:
  enabled: false  
mysql:
  enabled: false
gitea:
  config:
    database:
      DB_TYPE: sqlite3
      PATH: ":memory:"
    server:
      PROTOCOL: http
      HTTP_PORT: 3000
      DOMAIN: gitea.gitea-e2e.svc.cluster.local
    security:
      INSTALL_LOCK: true
    service:
      DISABLE_REGISTRATION: true
  admin:
    username: giteaadmin
    password: giteapassword123
    email: admin@example.com
service:
  http:
    type: ClusterIP
    port: 3000
resources:
  limits:
    memory: "512Mi"
    cpu: "500m"
  requests:
    memory: "256Mi" 
    cpu: "250m"
```

## Phase 2: Makefile Targets

### 2.1 Setup Target
**File**: `Makefile` (additions)
```makefile
GITEA_NAMESPACE ?= gitea-e2e
GITEA_CHART_VERSION ?= 10.4.0

.PHONY: setup-gitea-e2e
setup-gitea-e2e: helm kubectl ## Set up Gitea for e2e testing
	@echo "Setting up Gitea for e2e testing..."
	@$(HELM) repo add gitea-charts https://dl.gitea.com/charts/ 2>/dev/null || true
	@$(HELM) repo update gitea-charts
	@$(KUBECTL) create namespace $(GITEA_NAMESPACE) --dry-run=client -o yaml | $(KUBECTL) apply -f -
	@$(HELM) upgrade --install gitea gitea-charts/gitea \
		--namespace $(GITEA_NAMESPACE) \
		--version $(GITEA_CHART_VERSION) \
		--values test/e2e/gitea-values.yaml \
		--wait --timeout=300s
	@echo "Initializing Gitea test environment..."
	@./test/e2e/scripts/setup-gitea.sh
```

### 2.2 Cleanup Target
**File**: `Makefile` (additions)
```makefile
.PHONY: cleanup-gitea-e2e
cleanup-gitea-e2e: helm kubectl ## Clean up Gitea e2e environment
	@echo "Cleaning up Gitea e2e environment..."
	@$(HELM) uninstall gitea --namespace $(GITEA_NAMESPACE) 2>/dev/null || true
	@$(KUBECTL) delete namespace $(GITEA_NAMESPACE) 2>/dev/null || true
```

## Phase 3: Gitea Initialization Script

### 3.1 Setup Script
**File**: `test/e2e/scripts/setup-gitea.sh`
```bash
#!/bin/bash
set -euo pipefail

GITEA_NAMESPACE=${GITEA_NAMESPACE:-gitea-e2e}
GITEA_SERVICE="gitea"
ADMIN_USER="giteaadmin"
ADMIN_PASS="giteapassword123"
ORG_NAME="testorg"
REPO_NAME="testrepo"

# Wait for Gitea to be ready
echo "Waiting for Gitea to be ready..."
kubectl wait --for=condition=ready pod -l app.kubernetes.io/name=gitea -n $GITEA_NAMESPACE --timeout=300s

# Port-forward for API access
echo "Setting up port-forward..."
kubectl port-forward -n $GITEA_NAMESPACE svc/$GITEA_SERVICE 3000:3000 &
PF_PID=$!
sleep 5

# Function to cleanup port-forward
cleanup() {
    kill $PF_PID 2>/dev/null || true
}
trap cleanup EXIT

# API Base URL
API_URL="http://localhost:3000/api/v1"

# Create organization
echo "Creating test organization..."
curl -X POST "$API_URL/orgs" \
  -H "Content-Type: application/json" \
  -u "$ADMIN_USER:$ADMIN_PASS" \
  -d "{\"username\":\"$ORG_NAME\",\"full_name\":\"Test Organization\",\"description\":\"E2E Test Organization\"}" \
  || echo "Organization may already exist"

# Create repository
echo "Creating test repository..."
curl -X POST "$API_URL/orgs/$ORG_NAME/repos" \
  -H "Content-Type: application/json" \
  -u "$ADMIN_USER:$ADMIN_PASS" \
  -d "{\"name\":\"$REPO_NAME\",\"description\":\"E2E Test Repository\",\"private\":false,\"auto_init\":true}" \
  || echo "Repository may already exist"

# Generate access token
echo "Generating access token..."
TOKEN_RESPONSE=$(curl -X POST "$API_URL/users/$ADMIN_USER/tokens" \
  -H "Content-Type: application/json" \
  -u "$ADMIN_USER:$ADMIN_PASS" \
  -d "{\"name\":\"e2e-test-token\",\"scopes\":[\"write:repository\",\"read:repository\"]}")

TOKEN=$(echo $TOKEN_RESPONSE | grep -o '"sha1":"[^"]*"' | cut -d'"' -f4)

# Create Git credentials secret
echo "Creating Git credentials secret..."
kubectl create secret generic git-creds \
  --namespace=sut \
  --from-literal=ssh-privatekey="$TOKEN" \
  --from-literal=known_hosts="gitea.gitea-e2e.svc.cluster.local" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "Gitea setup completed successfully!"
echo "Repository URL: https://gitea.gitea-e2e.svc.cluster.local:3000/$ORG_NAME/$REPO_NAME.git"
echo "Access Token: $TOKEN"
```

## Phase 4: Sample Configuration Updates

### 4.1 Updated GitRepoConfig Sample
**File**: `config/samples/configbutler.ai_v1alpha1_gitrepoconfig.yaml`
```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  labels:
    app.kubernetes.io/name: gitops-reverser
    app.kubernetes.io/managed-by: kustomize
  name: gitrepoconfig-sample
spec:
  repoUrl: "https://gitea.gitea-e2e.svc.cluster.local:3000/testorg/testrepo.git"
  branch: "main"
  secretName: "git-creds"
  secretNamespace: "sut"
  push:
    interval: "1m"
    maxCommits: 20
```

## Phase 5: Enhanced E2E Tests

### 5.1 New Test Cases
**File**: `test/e2e/e2e_test.go` (additions)
```go
It("should perform real Git operations with Gitea", func() {
    By("creating a GitRepoConfig with real Gitea URL")
    // Use the updated sample that points to Gitea
    
    By("verifying repository connectivity")
    // Check that the controller can clone from Gitea
    
    By("creating a test ConfigMap to trigger Git commit")
    // Create a resource that should trigger a Git operation
    
    By("verifying commit appears in Gitea repository")
    // Check via Gitea API that commit was pushed
    
    By("verifying Git operations metrics")
    metricsOutput := getMetricsOutput()
    Expect(metricsOutput).To(ContainSubstring("git_operations_total"))
})
```

## Phase 6: CI Integration

### 6.1 GitHub Actions Enhancement
**File**: `.github/workflows/ci.yml` (e2e-test job modification)
```yaml
- name: Set up Helm
  uses: azure/setup-helm@v4
  with:
    version: 'v3.12.3'

- name: Run E2E tests with Gitea
  run: |
    make setup-gitea-e2e
    make test-e2e
    make cleanup-gitea-e2e
```

## Implementation Benefits

### Technical Advantages
1. **Real Git Testing**: Actual clone/commit/push operations
2. **CI Optimized**: Memory-based, fast setup/teardown  
3. **Isolated**: Fresh instance per test run
4. **Debuggable**: HTTP-based, easier to troubleshoot
5. **Maintainable**: Standard Helm chart, minimal customization

### Testing Coverage
1. **Authentication**: Real token-based auth
2. **Network**: Service-to-service communication
3. **Git Protocol**: HTTPS Git operations
4. **Error Handling**: Real Git failure scenarios
5. **Performance**: Actual Git operation timing

## Next Steps
1. Switch to code mode for implementation
2. Create all required files and scripts
3. Test the integration step by step
4. Refine based on testing results
5. Update documentation with final implementation