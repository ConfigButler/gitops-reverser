# GitHub Setup Guide for GitOps Reverser

This guide walks you through setting up GitOps Reverser to stream Kubernetes cluster changes to a GitHub repository.

## Overview

GitOps Reverser will:
- Capture changes in your Kubernetes cluster in real-time
- Automatically commit them to your GitHub repository
- Create organized YAML manifests with detailed commit messages
- Provide complete audit trail of all manual changes

## Prerequisites

- GitOps Reverser installed in your cluster (see [README.md](../README.md))
- A GitHub account
- `kubectl` configured for your cluster
- `ssh-keygen` (usually pre-installed)
- **Optional:** [GitHub CLI (`gh`)](https://cli.github.com/) - makes setup easier (can be installed with `brew install gh` or `apt install gh`)

## Step 1: Create GitHub Repository

Create a new repository on GitHub to store cluster changes:

```bash
# Option A: Via GitHub CLI (if installed)
gh repo create my-k8s-audit --private --description "Kubernetes cluster audit trail"

# Option B: Via web browser
# Go to: https://github.com/new
# Repository name: my-k8s-audit
# Choose: Private
# Click: Create repository
```

**Important:** Note your repository URL. It should look like:
```
git@github.com:YOUR_USERNAME/my-k8s-audit.git
```

## Step 2: Create SSH Deploy Key

Generate an SSH key pair for GitOps Reverser to authenticate with GitHub:

```bash
# Create a directory for keys
mkdir -p ~/.ssh/gitops-reverser

# Generate SSH key (no passphrase for automated use)
ssh-keygen -t ed25519 -C "gitops-reverser" -f ~/.ssh/gitops-reverser/id_ed25519 -N ""

# Display the public key (you'll need this for GitHub)
cat ~/.ssh/gitops-reverser/id_ed25519.pub
```

**Copy the public key output** (starts with `ssh-ed25519`).

## Step 3: Add Deploy Key to GitHub

### Option A: Using GitHub CLI (Recommended)

If you have `gh` CLI installed:

```bash
# Add deploy key with write access
gh repo deploy-key add ~/.ssh/gitops-reverser/id_ed25519.pub \
  --repo YOUR_USERNAME/my-k8s-audit \
  --title "gitops-reverser" \
  --allow-write

# Verify it was added
gh repo deploy-key list --repo YOUR_USERNAME/my-k8s-audit
```

**Replace `YOUR_USERNAME`** with your GitHub username.

Adding deploy keys to individual repos could be disabled on organisations level: an organisation admin can enable this at https://github.com/organizations/{your-org}/settings/deploy_keys.

### Option B: Using GitHub Web UI

If `gh` CLI is not available:

1. **Install GitHub CLI (optional but recommended):**
   ```bash
   # macOS
   brew install gh
   
   # Ubuntu/Debian
   sudo apt install gh
   
   # Other: https://cli.github.com/
   ```

2. Or use the **Web UI manually:**
   - Go to your repository on GitHub
   - Navigate to: **Settings** → **Deploy keys** → **Add deploy key**
   - Fill in:
     - **Title:** `gitops-reverser`
     - **Key:** Paste the public key from Step 2
     - ✅ **Allow write access** (REQUIRED)
   - Click **Add key**

## Step 4: Create Kubernetes Secret

Create a Kubernetes secret containing the SSH private key:

```bash
# Get GitHub's host keys (required for SSH authentication)
ssh-keyscan github.com > /tmp/known_hosts

# Generate secret YAML (doesn't apply to cluster yet)
kubectl create secret generic git-credentials \
  --namespace gitops-reverser-system \
  --from-file=ssh-privatekey=$HOME/.ssh/gitops-reverser/id_ed25519 \
  --from-file=known_hosts=/tmp/known_hosts \
  --dry-run=client -o yaml > git-credentials-secret.yaml

# Review the generated YAML
cat git-credentials-secret.yaml

# Apply the secret to the cluster
kubectl apply -f git-credentials-secret.yaml

# Verify the secret was created
kubectl get secret git-credentials -n gitops-reverser-system
```

**Expected output:**
```
NAME              TYPE     DATA   AGE
git-credentials   Opaque   2      5s
```

**Note:** The `--dry-run=client -o yaml` flags generate YAML without applying to the cluster. This is useful for:
- Reviewing before applying
- Committing to Git for GitOps workflows
- Storing in version control (without the actual secrets!)

## Step 5: Create GitRepoConfig

Create a configuration file to tell GitOps Reverser about your repository.

**Save this as `gitrepoconfig.yaml`** (replace `YOUR_USERNAME` with your GitHub username):

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: github-audit-repo
  namespace: gitops-reverser-system
spec:
  # Replace YOUR_USERNAME with your GitHub username
  repoUrl: "git@github.com:YOUR_USERNAME/my-k8s-audit.git"
  branch: "main"
  
  # Reference to the secret created in Step 4
  secretRef:
    name: git-credentials
  
  # Push settings
  push:
    interval: "2m"      # Push changes every 2 minutes
    maxCommits: 50      # Bundle up to 50 changes per push
```

Apply the configuration:

```bash
kubectl apply -f gitrepoconfig.yaml
```

## Step 6: Create WatchRule

Create rules to define what resources to capture. Here are common examples:

### Option A: Capture Multiple Resource Types

**Save as `watchrule-common-resources.yaml`:**

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: common-resources-audit
  namespace: gitops-reverser-system
spec:
  # Reference to GitRepoConfig from Step 5
  gitRepoConfigRef: github-audit-repo
  
  # Watch multiple resource types
  rules:
    - resources: ["deployments", "services", "configmaps", "secrets"]
    - resources: ["ingresses.*"]  # Wildcard for all ingress versions
  
  # Exclude resources with this label
  excludeLabels:
    matchExpressions:
      - key: "gitops-reverser/ignore"
        operator: Exists
```

Apply it:
```bash
kubectl apply -f watchrule-common-resources.yaml
```

### Option B: Capture Only ConfigMaps and Secrets

**Save as `watchrule-configs.yaml`:**

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: config-audit
  namespace: gitops-reverser-system
spec:
  gitRepoConfigRef: github-audit-repo
  
  # Only watch ConfigMaps and Secrets
  rules:
    - resources: ["configmaps", "secrets"]
  
  # Exclude resources with specific labels
  excludeLabels:
    matchLabels:
      "gitops-reverser/ignore": "true"
```

Apply it:
```bash
kubectl apply -f watchrule-configs.yaml
```

### Option C: Capture All Resources (Wildcard)

**Save as `watchrule-all.yaml`:**

```yaml
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: audit-all
  namespace: gitops-reverser-system
spec:
  gitRepoConfigRef: github-audit-repo
  
  # Watch all resource types (use with caution - generates many events!)
  rules:
    - resources: ["*"]
  
  # Exclude system resources
  excludeLabels:
    matchExpressions:
      - key: "app.kubernetes.io/managed-by"
        operator: In
        values: ["kube-controller-manager", "kube-scheduler"]
```

Apply it:
```bash
kubectl apply -f watchrule-all.yaml
```

**Note:** WatchRule does NOT have `namespaceSelector` or `resourceSelector` fields. To filter by namespace or labels, you need to:
- Use the webhook's namespace selector (configured during installation)
- Use `excludeLabels` to exclude specific resources
- The operator processes ALL resources matching the rules across all namespaces

## Step 7: Test the Setup

Create a test ConfigMap to verify the setup:

```bash
# Create a test ConfigMap
kubectl create configmap test-config \
  --namespace default \
  --from-literal=key1=value1 \
  --from-literal=key2=value2

# Optionally, label it to exclude from capture (testing excludeLabels)
# kubectl label configmap test-config gitops-reverser/ignore=true -n default

# Wait 2-3 minutes for the push interval
sleep 180

# Check if changes were pushed to GitHub
# Go to: https://github.com/YOUR_USERNAME/my-k8s-audit
```

## Step 8: Verify

Check the GitRepoConfig status:

```bash
kubectl get gitrepoconfig github-audit-repo -n gitops-reverser-system -o yaml
```

Check the WatchRule status:

```bash
kubectl get watchrule -n gitops-reverser-system
```

Check GitOps Reverser logs:

```bash
kubectl logs -n gitops-reverser-system deployment/gitops-reverser -f
```

## Expected Repository Structure

After setup, your GitHub repository will contain:

```
my-k8s-audit/
├── namespaces/
│   ├── default/
│   │   ├── ConfigMap/
│   │   │   └── test-config.yaml
│   │   ├── Service/
│   │   └── Deployment/
│   └── production/
│       └── ...
└── cluster-scoped/
    ├── ClusterRole/
    ├── Namespace/
    └── ...
```

## Commit Message Format

Each commit will include detailed metadata:

```
[CREATE] ConfigMap/test-config in ns/default by user/admin@example.com

- Resource: ConfigMap/test-config
- Namespace: default
- Operation: CREATE
- User: admin@example.com
- Timestamp: 2025-01-31T18:30:00Z
- Resource Version: 12345
```

## Troubleshooting

### Issue: "Deploy keys are disabled for this repository" (HTTP 422)

This error occurs when the repository is part of an organization that has **disabled deploy keys as a security policy**.

**Solution 1: Use Personal Access Token (Recommended for Organizations)**

1. **Generate a Personal Access Token (PAT):**
   ```bash
   # Option A: Using gh CLI
   gh auth login --scopes repo
   
   # Option B: Via web browser
   # Go to: https://github.com/settings/tokens/new
   # Scopes needed: repo (full control of private repositories)
   # Generate token and save it securely
   ```

2. **Create secret with PAT instead of SSH:**
   ```bash
   # Generate secret YAML with token (save token securely first!)
   kubectl create secret generic git-credentials \
     --namespace gitops-reverser-system \
     --from-literal=username=git \
     --from-literal=password=YOUR_GITHUB_TOKEN_HERE \
     --dry-run=client -o yaml > git-credentials-secret.yaml
   
   # Apply to cluster
   kubectl apply -f git-credentials-secret.yaml
   ```

3. **Update GitRepoConfig to use HTTPS:**
   ```yaml
   apiVersion: configbutler.ai/v1alpha1
   kind: GitRepoConfig
   metadata:
     name: github-audit-repo
     namespace: gitops-reverser-system
   spec:
     # Use HTTPS URL instead of SSH
     repoUrl: "https://github.com/ConfigButler/my-first-k8s-trail.git"
     branch: "main"
     secretRef:
       name: git-credentials
     push:
       interval: "2m"
       maxCommits: 50
   ```

**Solution 2: Ask Organization Admin to Enable Deploy Keys**

Contact your GitHub organization administrator to enable deploy keys:
- Go to: Organization Settings → Member privileges → Repository administration
- Look for: "Allow members to add deploy keys to repositories"
- Enable this setting
- Then retry Steps 2-3 from the main guide

**Solution 3: Use Machine User with PAT (Most Secure for Organizations)**

Create a dedicated GitHub "machine user" account for GitOps Reverser:
1. Create new GitHub account (e.g., `myorg-gitops-bot`)
2. Add to organization with repository write access
3. Generate PAT from that account
4. Use PAT authentication (Solution 1 above)

This provides better audit trails and doesn't tie automation to personal accounts.

### Issue: "Authentication failed"

**Check SSH key permissions:**
```bash
ls -la ~/.ssh/gitops-reverser/
# Should show: -rw------- (600) for private key

# Fix if needed:
chmod 600 ~/.ssh/gitops-reverser/id_ed25519
```

**Verify GitHub deploy key:**
- Ensure "Allow write access" is checked
- Verify the public key matches

### Issue: "No commits appearing in GitHub"

**Check WatchRule matches resources:**
```bash
# View WatchRule status
kubectl describe watchrule -n gitops-reverser-system

# Check if resources have matching labels
kubectl get configmap test-config -n default --show-labels
```

**Check GitRepoConfig status:**
```bash
kubectl describe gitrepoconfig github-audit-repo -n gitops-reverser-system
```

### Issue: "Repository not found"

**Verify repository URL:**
```bash
# Should be SSH format
# ✅ Correct: git@github.com:username/repo.git
# ❌ Wrong: https://github.com/username/repo.git

# Test SSH connection
ssh -T git@github.com -i ~/.ssh/gitops-reverser/id_ed25519
# Should output: "Hi username! You've successfully authenticated..."
```

### Issue: "Permission denied"

**Verify known_hosts:**
```bash
kubectl get secret git-credentials -n gitops-reverser-system -o yaml | \
  grep known_hosts -A 5
```

**Recreate secret if needed:**
```bash
kubectl delete secret git-credentials -n gitops-reverser-system
ssh-keyscan github.com > /tmp/known_hosts

# Generate new secret YAML
kubectl create secret generic git-credentials \
  --namespace gitops-reverser-system \
  --from-file=ssh-privatekey=$HOME/.ssh/gitops-reverser/id_ed25519 \
  --from-file=known_hosts=/tmp/known_hosts \
  --dry-run=client -o yaml > git-credentials-secret.yaml

# Apply to cluster
kubectl apply -f git-credentials-secret.yaml
```

## Advanced Configuration

### Multiple Repositories

You can configure multiple GitRepoConfig resources for different purposes:

```yaml
# Audit repository
---
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: audit-repo
  namespace: gitops-reverser-system
spec:
  repoUrl: "git@github.com:yourorg/k8s-audit.git"
  branch: "main"
  secretRef:
    name: git-credentials

# Config backup repository
---
apiVersion: configbutler.ai/v1alpha1
kind: GitRepoConfig
metadata:
  name: config-backup-repo
  namespace: gitops-reverser-system
spec:
  repoUrl: "git@github.com:yourorg/k8s-configs.git"
  branch: "main"
  secretRef:
    name: git-credentials
```

### Different SSH Keys per Repository

```bash
# Generate separate key for each repository
ssh-keygen -t ed25519 -C "audit-repo" -f ~/.ssh/gitops-reverser/audit-key -N ""
ssh-keygen -t ed25519 -C "config-repo" -f ~/.ssh/gitops-reverser/config-key -N ""

# Generate secret YAML files
kubectl create secret generic audit-repo-credentials \
  --namespace gitops-reverser-system \
  --from-file=ssh-privatekey=$HOME/.ssh/gitops-reverser/audit-key \
  --from-file=known_hosts=/tmp/known_hosts \
  --dry-run=client -o yaml > audit-repo-secret.yaml

kubectl create secret generic config-repo-credentials \
  --namespace gitops-reverser-system \
  --from-file=ssh-privatekey=$HOME/.ssh/gitops-reverser/config-key \
  --from-file=known_hosts=/tmp/known_hosts \
  --dry-run=client -o yaml > config-repo-secret.yaml

# Apply to cluster
kubectl apply -f audit-repo-secret.yaml
kubectl apply -f config-repo-secret.yaml
```

## Security Best Practices

1. **Use Deploy Keys** (not personal SSH keys)
   - Scoped to single repository
   - Can be rotated independently
   - Easier to audit

2. **Use Private Repositories**
   - Contains sensitive cluster information
   - May include secret metadata

3. **Rotate Keys Regularly**
   ```bash
   # Generate new key
   ssh-keygen -t ed25519 -C "gitops-reverser-$(date +%Y%m)" -f ~/.ssh/gitops-reverser/id_ed25519_new -N ""
   
   # Add to GitHub as new deploy key
   # Update Kubernetes secret
   # Delete old deploy key from GitHub
   ```

4. **Review Commits Regularly**
   - Check for unexpected changes
   - Validate automated commits
   - Investigate anomalies

## Next Steps

- **Configure notifications:** Set up GitHub webhooks for commit notifications
- **Add monitoring:** Enable Prometheus metrics (see [README.md](../README.md))
- **Integrate with tools:** Connect with your incident management system
- **Review documentation:** See [COMPLETE_SOLUTION.md](COMPLETE_SOLUTION.md) for architecture details

## Getting Help

- 📖 [Main Documentation](../README.md)
- 🐛 [Report Issues](https://github.com/ConfigButler/gitops-reverser/issues)
- 💬 [Discussions](https://github.com/ConfigButler/gitops-reverser/discussions)

---

**Quick Reference Commands:**

```bash
# Check status
kubectl get gitrepoconfig -n gitops-reverser-system
kubectl get watchrule -n gitops-reverser-system

# View logs
kubectl logs -n gitops-reverser-system deployment/gitops-reverser -f

# Test SSH connection
ssh -T git@github.com -i ~/.ssh/gitops-reverser/id_ed25519

# Force immediate push (restart pod)
kubectl rollout restart deployment gitops-reverser -n gitops-reverser-system