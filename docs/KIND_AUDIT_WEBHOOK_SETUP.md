# Kind Audit Webhook Setup Implementation

## Problem Summary

Setting up audit webhook in Kind cluster required solving TWO critical issues:
1. **Docker-in-Docker path mounting** in devcontainer environment
2. **DNS resolution timing** - kube-apiserver starts before CoreDNS

### Root Cause #1: Path Mounting
When running in a devcontainer:
1. Kind runs via Docker on the **host machine**
2. File paths inside the devcontainer (e.g., `/workspaces/gitops-reverser2/`) don't exist on the host
3. When Docker can't find source files, it creates **empty directories** with those names
4. Result: audit configuration files appeared as empty directories, causing kube-apiserver to crash

### Root Cause #2: DNS Resolution Timing (The Real Blocker!)
Even with files correctly mounted:
1. `kube-apiserver` starts and tries to connect to audit webhook via DNS: `gitops-reverser-webhook-service.sut.svc.cluster.local`
2. But **CoreDNS isn't running yet** - it's a pod that needs kube-apiserver to be up first!
3. Result: DNS lookup fails with "server misbehaving" error
4. kube-apiserver gives up and never retries the webhook connection

**This is a classic chicken-and-egg problem with Kubernetes DNS.**

### Evidence
```bash
# Inside devcontainer:
$ ls test/e2e/kind/audit/policy.yaml
-rw-r--r-- 1 vscode vscode 1858 policy.yaml  # ✅ File exists

# From Docker on host:
$ docker exec kind-node ls -la /etc/kubernetes/audit/
drwxr-xr-x 2 root root 4096 policy.yaml  # ❌ Empty directory!
```

## Solution Implemented

The solution requires TWO fixes:

### Fix #1: Host Path Mounting (Docker-in-Docker)
Use environment variable substitution to inject the **host's actual path** into the Kind configuration.

#### 1. Updated `.devcontainer/devcontainer.json`

**File:** [`.devcontainer/devcontainer.json`](../.devcontainer/devcontainer.json)

```json
"containerEnv": {
  "HOST_PROJECT_PATH": "${localWorkspaceFolder}"
}
```

This injects the physical host path (e.g., `/home/user/projects/gitops-reverser2`) that Docker can access.

#### 2. Created Template Configuration

**File:** [`test/e2e/kind/cluster.yaml.template`](../test/e2e/kind/cluster.yaml.template)

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraMounts:
  - hostPath: ${HOST_PROJECT_PATH}/test/e2e/kind/audit  # ← Mount to node
    containerPath: /etc/kubernetes/audit
    readOnly: false
  
  kubeadmConfigPatches:
  - |
    kind: ClusterConfiguration
    apiServer:
      extraArgs:
        audit-policy-file: /etc/kubernetes/audit/policy.yaml
        audit-webhook-config-file: /etc/kubernetes/audit/webhook-config.yaml
        audit-webhook-batch-max-wait: "5s"
        audit-webhook-batch-max-size: "100"
      extraVolumes:  # ← CRITICAL: Mount into kube-apiserver container
      - name: audit-config
        hostPath: /etc/kubernetes/audit
        mountPath: /etc/kubernetes/audit
        readOnly: true
        pathType: DirectoryOrCreate
```

#### 3. Created Cluster Startup Script

**File:** [`test/e2e/kind/start-cluster.sh`](../test/e2e/kind/start-cluster.sh)

The script:
1. Checks if `HOST_PROJECT_PATH` environment variable is set
2. Uses `envsubst` to replace `${HOST_PROJECT_PATH}` in the template
3. Generates the actual `cluster.yaml` with the correct host path
4. Creates the Kind cluster with properly mounted files

#### 4. Updated Makefile

**File:** [`Makefile`](../Makefile)

```makefile
.PHONY: setup-cluster
setup-cluster: ## Set up a Kind cluster for e2e tests if it does not exist
	@if ! command -v $(KIND) >/dev/null 2>&1; then \
		echo "Kind is not installed - skipping..."; \
	else \
		KIND_CLUSTER=$(KIND_CLUSTER) bash test/e2e/kind/start-cluster.sh; \
	fi
```

### Fix #2: DNS Resolution Timing (Critical!)

**The Problem:** kube-apiserver starts before CoreDNS, so DNS names like `*.svc.cluster.local` don't resolve during startup.

**The Solution:** Use a **fixed ClusterIP** instead of DNS name, exactly like in production k3s setup.

#### 1. Created Fixed ClusterIP Patch

**File:** [`config/default/webhook_service_fixed_ip_patch.yaml`](../config/default/webhook_service_fixed_ip_patch.yaml)

```yaml
# Set fixed ClusterIP for webhook service
# Required because kube-apiserver starts before CoreDNS
apiVersion: v1
kind: Service
metadata:
  name: webhook-service
  namespace: system
spec:
  clusterIP: 10.96.200.200  # Fixed IP that doesn't require DNS
```

#### 2. Updated Kustomization

**File:** [`config/default/kustomization.yaml`](../config/default/kustomization.yaml)

Added the patch to apply the fixed ClusterIP:
```yaml
patches:
- path: webhook_service_fixed_ip_patch.yaml
  target:
    kind: Service
    name: webhook-service
```

#### 3. Updated Audit Webhook Config

**File:** [`test/e2e/kind/audit/webhook-config.yaml`](../test/e2e/kind/audit/webhook-config.yaml)

```yaml
clusters:
- name: audit-webhook
  cluster:
    # Use fixed ClusterIP instead of DNS name!
    # DNS doesn't work because CoreDNS isn't running when kube-apiserver starts
    server: https://10.96.200.200:443/audit-webhook
    insecure-skip-tls-verify: true
```

**Why This Works:**
- ClusterIPs are handled by `kube-proxy` which runs as a static pod (starts with kube-apiserver)
- No DNS lookup required - direct IP routing
- Same solution used in production k3s clusters

## Testing Instructions

### Step 1: Rebuild Devcontainer (Required)

The `HOST_PROJECT_PATH` environment variable needs to be available:

1. Press `Ctrl+Shift+P` (or `Cmd+Shift+P` on Mac)
2. Select **"Dev Containers: Rebuild Container"**
3. Wait for rebuild to complete (~2-3 minutes)

### Step 2: Verify Environment Variable

After rebuild, verify the variable is set:

```bash
echo "HOST_PROJECT_PATH=${HOST_PROJECT_PATH}"
# Should output something like: HOST_PROJECT_PATH=/home/user/projects/gitops-reverser2
```

If it shows "HOST_PROJECT_PATH=" (empty), the rebuild didn't work - try again.

### Step 3: Clean Up Old Cluster

```bash
# Delete the old broken cluster
kind delete cluster --name gitops-reverser-test-e2e
```

### Step 4: Create New Cluster

```bash
# Create cluster with proper audit webhook support
make setup-cluster
```

This should:
- Complete in ~30-60 seconds (not hang forever!)
- Show "✅ Kind cluster created successfully"

### Step 5: Verify Audit Files Are Mounted

```bash
# Check if files are properly mounted (not directories!)
docker exec gitops-reverser-test-e2e-control-plane ls -la /etc/kubernetes/audit/

# Should show FILES:
# -rw-r--r-- 1 root root 1858 policy.yaml         ✅
# -rw-r--r-- 1 root root  754 webhook-config.yaml ✅

# NOT directories:
# drwxr-xr-x 2 root root 4096 policy.yaml         ❌


# Verify file content is accessible
docker exec gitops-reverser-test-e2e-control-plane cat /etc/kubernetes/audit/policy.yaml | head -5
# Should show actual YAML content
```

### Step 6: Verify kube-apiserver Started Successfully

```bash
# Check if kube-apiserver is running (not in crash loop)
docker exec gitops-reverser-test-e2e-control-plane crictl ps | grep kube-apiserver

# Should show "Running" state, not "Exited":
# abc123...  Running  kube-apiserver  ✅
```

### Step 7: Verify Cluster is Ready

```bash
# Should return nodes (not connection refused)
kubectl get nodes

# Should show:
# NAME                                    STATUS   READY
# gitops-reverser-test-e2e-control-plane  Ready    <1m
```

### Step 8: Run E2E Test (Optional)

```bash
# Deploy the operator and run full e2e tests
make test-e2e
```

The test **"should receive audit webhook events from kube-apiserver"** should pass.

## Expected Results

### ✅ Success Indicators
- Cluster starts in ~30-60 seconds (not hanging forever)
- Files in `/etc/kubernetes/audit/` are actual files (not directories)
- kube-apiserver is in "Running" state (not crash looping)
- `kubectl get nodes` works (cluster is accessible)
- Audit webhook receives events (visible in metrics)

### ❌ Failure Indicators
- Cluster hangs at "Starting control-plane" for >2 minutes
- Files show as `drwxr-xr-x` (directories) instead of `-rw-r--r--` (files)
- kube-apiserver shows "Exited" or high attempt count
- `kubectl get nodes` shows "connection refused"

## How It Works

```
┌──────────────────────────────────────────────────┐
│ 1. VSCode reads devcontainer.json               │
│    HOST_PROJECT_PATH=/host/path/to/project      │
└────────────────┬─────────────────────────────────┘
                 │
                 ▼
┌──────────────────────────────────────────────────┐
│ 2. start-cluster.sh runs                         │
│    envsubst replaces ${HOST_PROJECT_PATH}        │
│    in cluster.yaml.template                      │
└────────────────┬─────────────────────────────────┘
                 │
                 ▼
┌──────────────────────────────────────────────────┐
│ 3. Generated cluster.yaml has REAL host path:   │
│    hostPath: /host/path/to/project/test/e2e/... │
└────────────────┬─────────────────────────────────┘
                 │
                 ▼
┌──────────────────────────────────────────────────┐
│ 4. Kind tells Docker (on host) to mount:        │
│    Source: /host/path/... (EXISTS! ✅)           │
│    Target: /etc/kubernetes/audit                │
└────────────────┬─────────────────────────────────┘
                 │
                 ▼
┌──────────────────────────────────────────────────┐
│ 5. Files properly mounted in Kind node          │
│    kube-apiserver can read audit config ✅       │
└──────────────────────────────────────────────────┘
```

## Troubleshooting

### Problem: `HOST_PROJECT_PATH` is empty after rebuild

**Solution:** 
1. Check [`.devcontainer/devcontainer.json`](../.devcontainer/devcontainer.json) has the `containerEnv` section
2. Try "Dev Containers: Rebuild Container Without Cache"
3. Restart VS Code completely

### Problem: Files still show as directories

**Solution:**
```bash
# Old directories created by previous failed attempts still exist on host
# Delete them manually:
docker run --rm -v /:/host busybox rm -rf \
  /host/workspaces/gitops-reverser2/test/e2e/kind/audit/policy.yaml \
  /host/workspaces/gitops-reverser2/test/e2e/kind/audit/webhook-config.yaml

# Then try creating cluster again
kind delete cluster --name gitops-reverser-test-e2e
make setup-cluster
```

### Problem: kube-apiserver still crashing

**Solution:**
```bash
# Check API server logs
docker exec gitops-reverser-test-e2e-control-plane crictl logs $(docker exec gitops-reverser-test-e2e-control-plane crictl ps -a | grep kube-apiserver | awk '{print $1}') 2>&1 | tail -20

# Look for audit-related errors
```

## References

- Kind extraMounts: https://kind.sigs.k8s.io/docs/user/configuration/#extra-mounts
- Kubernetes Audit: https://kubernetes.io/docs/tasks/debug/debug-cluster/audit/
- Docker-in-Docker paths: https://code.visualstudio.com/remote/advancedcontainers/develop-remote-host

## Files Modified

- [`.devcontainer/devcontainer.json`](../.devcontainer/devcontainer.json) - Added `HOST_PROJECT_PATH` env var
- [`test/e2e/kind/cluster.yaml.template`](../test/e2e/kind/cluster.yaml.template) - Template with placeholder
- [`test/e2e/kind/start-cluster.sh`](../test/e2e/kind/start-cluster.sh) - Script to substitute and create cluster
- [`Makefile`](../Makefile) - Updated `setup-cluster` target
- [`test/e2e/e2e_test.go`](../test/e2e/e2e_test.go) - Added audit webhook test
- [`test/e2e/kind/README.md`](../test/e2e/kind/README.md) - Documentation

## Verification: It Should Work Now!

After completing all the setup steps above, here's the complete verification sequence to confirm everything works:

### Complete Test Sequence

```bash
# 1. Verify environment variable is available
echo "HOST_PROJECT_PATH=${HOST_PROJECT_PATH}"
# Expected: /home/user/... (actual host path, NOT empty)

# 2. Delete old broken cluster
kind delete cluster --name gitops-reverser-test-e2e

# 3. Create new cluster with audit support
make setup-cluster
# Expected: Completes in ~30-60 seconds with "✅ Kind cluster created successfully"

# 4. Verify files are mounted as FILES (not directories)
docker exec gitops-reverser-test-e2e-control-plane ls -la /etc/kubernetes/audit/
# Expected output:
# -rw-r--r-- 1 root root 1858 Dec  3 10:00 policy.yaml
# -rw-r--r-- 1 root root  754 Dec  3 10:00 webhook-config.yaml

# 5. Verify file content is readable
docker exec gitops-reverser-test-e2e-control-plane head -3 /etc/kubernetes/audit/policy.yaml
# Expected: Shows actual YAML content (apiVersion: audit.k8s.io/v1...)

# 6. Verify kube-apiserver is running
docker exec gitops-reverser-test-e2e-control-plane crictl ps | grep kube-apiserver
# Expected: Shows "Running" (not "Exited")

# 7. Verify cluster is accessible
kubectl get nodes
# Expected:
# NAME                                    STATUS   READY   AGE
# gitops-reverser-test-e2e-control-plane  Ready    master  1m

# 8. Check kube-apiserver has audit flags
docker exec gitops-reverser-test-e2e-control-plane ps aux | grep kube-apiserver | grep audit
# Expected: Shows --audit-policy-file and --audit-webhook-config-file flags

# 9. Success! Cluster is ready with audit webhook configured ✅
```

### Expected Final State

- ✅ Cluster created in <60 seconds
- ✅ Audit policy files mounted as actual files
- ✅ kube-apiserver running and stable
- ✅ Cluster accessible via kubectl
- ✅ Ready for e2e tests with audit webhook

Now you can proceed with deploying the operator and testing that audit events are actually received!

## Related

- [`internal/webhook/audit_handler.go`](../internal/webhook/audit_handler.go) - Audit webhook implementation
- [`cmd/main.go`](../cmd/main.go) - Registers `/audit-webhook` endpoint
- [`internal/metrics/exporter.go`](../internal/metrics/exporter.go) - Defines `AuditEventsReceivedTotal` metric