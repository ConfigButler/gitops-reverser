#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Demo script: wire up gitops-reverser to sync PodInfoApp changes from
# podinfos-preview to the "monster-deployment" git branch.
#
# What this script does:
#   1. Finds the first git-repo-* secret (searches gitops-reverser ns first,
#      then default; override with SECRET_SEARCH_NS)
#   2. Copies it into the podinfos-preview namespace
#   3. Creates a GitProvider, GitTarget and WatchRule in podinfos-preview
#      to sync PodInfoApp instances to the "monster-deployment" branch
#   4. Generates a Kustomize overlay (monster-deployment-production/) that
#      deploys those same instances to the podinfos-production namespace
#
# Prerequisites:
#   - kubectl configured against the demo cluster
#   - jq installed
#   - gitops-reverser controller running in the cluster
#   - At least one git-repo-* secret and a GitProvider referencing it
#
# Optional env vars:
#   SECRET_SEARCH_NS   Space-separated list of namespaces to search for the
#                      git-repo-* secret (default: "gitops-reverser default")
#   GIT_PATH           Base path inside the git repo (default: "gitops-reverser-demo")
#   PROVIDER_NAME      Name for the GitProvider  (default: "monster-deployment-provider")
#   TARGET_NAME        Name for the GitTarget    (default: "monster-deployment-target")
#   WATCHRULE_NAME     Name for the WatchRule    (default: "monster-deployment-watchrule")

set -euo pipefail

# ──────────────────────────────────────────────────────────────────────────────
# Configuration (override via env vars)
# ──────────────────────────────────────────────────────────────────────────────
PREVIEW_NS="podinfos-preview"
PRODUCTION_NS="podinfos-production"
BRANCH="monster-deployment"

PROVIDER_NAME="${PROVIDER_NAME:-monster-deployment-provider}"
TARGET_NAME="${TARGET_NAME:-monster-deployment-target}"
WATCHRULE_NAME="${WATCHRULE_NAME:-monster-deployment-watchrule}"

# Base path inside the git repo where gitops-reverser writes resources.
# PodInfoApp files land at: $GIT_PATH/kro.run/v1alpha1/podinfoapps/$PREVIEW_NS/<name>.yaml
GIT_PATH="${GIT_PATH:-gitops-reverser-demo}"

# Name given to the secret after it is copied into podinfos-preview
COPIED_SECRET_NAME="git-repo-preview"

# Namespaces to search for the git-repo-* secret
read -ra SECRET_SEARCH_NS <<< "${SECRET_SEARCH_NS:-gitops-reverser default}"

# Where the production kustomization is written
OUTPUT_DIR="./monster-deployment-production"

# ──────────────────────────────────────────────────────────────────────────────
# Helpers
# ──────────────────────────────────────────────────────────────────────────────
die()  { echo "ERROR: $*" >&2; exit 1; }
info() { echo "==> $*"; }
ok()   { echo "    ✓ $*"; }

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || die "Required command not found: $1 — please install it first."
}

require_cmd kubectl
require_cmd jq

# ──────────────────────────────────────────────────────────────────────────────
# Step 1: Discover the git-repo-* secret and its associated GitProvider URL
# ──────────────────────────────────────────────────────────────────────────────
info "Searching for git-repo-* secret in namespaces: ${SECRET_SEARCH_NS[*]} ..."

SECRET_NAME=""
SECRET_NS=""

for ns in "${SECRET_SEARCH_NS[@]}"; do
  found=$(kubectl get secrets -n "$ns" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null \
    | tr ' ' '\n' | grep -E '^git-repo-' | head -1 || true)
  if [[ -n "$found" ]]; then
    SECRET_NAME="$found"
    SECRET_NS="$ns"
    break
  fi
done

[[ -n "$SECRET_NAME" ]] \
  || die "No 'git-repo-*' secret found in namespaces: ${SECRET_SEARCH_NS[*]}."

ok "Found secret '$SECRET_NAME' in namespace '$SECRET_NS'"

# Look up the repo URL from the GitProvider that references this secret.
info "Resolving repo URL from GitProvider referencing '$SECRET_NAME'..."

REPO_URL=$(kubectl get gitproviders --all-namespaces \
  -o jsonpath='{range .items[*]}{.spec.secretRef.name}{"\t"}{.spec.url}{"\n"}{end}' \
  2>/dev/null \
  | awk -v s="$SECRET_NAME" '$1 == s { print $2; exit }' || true)

[[ -n "$REPO_URL" ]] \
  || die "Could not determine repo URL. Make sure a GitProvider exists that has spec.secretRef.name='$SECRET_NAME'."

ok "Repo URL: $REPO_URL"

# ──────────────────────────────────────────────────────────────────────────────
# Step 2: Copy the secret into podinfos-preview
# ──────────────────────────────────────────────────────────────────────────────
info "Copying secret '$SECRET_NAME' → '$COPIED_SECRET_NAME' in namespace '$PREVIEW_NS'..."

kubectl get secret "$SECRET_NAME" -n "$SECRET_NS" -o json \
  | jq --arg name "$COPIED_SECRET_NAME" --arg ns "$PREVIEW_NS" '
      .metadata = {
        name:      $name,
        namespace: $ns
      }
      | del(.status)
    ' \
  | kubectl apply -f -

ok "Secret copied."

# ──────────────────────────────────────────────────────────────────────────────
# Step 3: Create GitProvider in podinfos-preview
# ──────────────────────────────────────────────────────────────────────────────
info "Creating GitProvider '$PROVIDER_NAME' in '$PREVIEW_NS'..."

kubectl apply -f - <<EOF
apiVersion: configbutler.ai/v1alpha1
kind: GitProvider
metadata:
  name: $PROVIDER_NAME
  namespace: $PREVIEW_NS
spec:
  url: "$REPO_URL"
  allowedBranches:
    - "$BRANCH"
  push:
    interval: "5s"
    maxCommits: 10
  secretRef:
    name: $COPIED_SECRET_NAME
EOF

ok "GitProvider created."

# ──────────────────────────────────────────────────────────────────────────────
# Step 4: Create GitTarget in podinfos-preview
# ──────────────────────────────────────────────────────────────────────────────
info "Creating GitTarget '$TARGET_NAME' in '$PREVIEW_NS'..."

kubectl apply -f - <<EOF
apiVersion: configbutler.ai/v1alpha1
kind: GitTarget
metadata:
  name: $TARGET_NAME
  namespace: $PREVIEW_NS
spec:
  providerRef:
    kind: GitProvider
    name: $PROVIDER_NAME
  branch: "$BRANCH"
  path: "$GIT_PATH"
EOF

ok "GitTarget created (branch: $BRANCH, path: $GIT_PATH)."

# ──────────────────────────────────────────────────────────────────────────────
# Step 5: Create WatchRule in podinfos-preview (PodInfoApp instances only)
# ──────────────────────────────────────────────────────────────────────────────
info "Creating WatchRule '$WATCHRULE_NAME' in '$PREVIEW_NS'..."

kubectl apply -f - <<EOF
apiVersion: configbutler.ai/v1alpha1
kind: WatchRule
metadata:
  name: $WATCHRULE_NAME
  namespace: $PREVIEW_NS
spec:
  targetRef:
    kind: GitTarget
    name: $TARGET_NAME
  rules:
    - operations: [CREATE, UPDATE, DELETE]
      apiGroups: ["kro.run"]
      apiVersions: ["v1alpha1"]
      resources: ["podinfoapps"]
EOF

ok "WatchRule created (watches: PodInfoApp)."

# ──────────────────────────────────────────────────────────────────────────────
# Step 6: Generate the production kustomization.yaml
#
# gitops-reverser writes synced resources to the branch at:
#   $GIT_PATH/kro.run/v1alpha1/podinfoapps/$PREVIEW_NS/<name>.yaml
#
# The kustomization overrides the namespace to $PRODUCTION_NS so the same
# resource manifests deploy into production without modification.
# ──────────────────────────────────────────────────────────────────────────────
info "Discovering PodInfoApp instances in '$PREVIEW_NS'..."

PODINFO_NAMES=$(kubectl get podinfoapps -n "$PREVIEW_NS" \
  -o jsonpath='{.items[*].metadata.name}' 2>/dev/null \
  | tr ' ' '\n' \
  | grep -v '^$' \
  || true)

mkdir -p "$OUTPUT_DIR"

info "Generating kustomization at '$OUTPUT_DIR/kustomization.yaml'..."

{
  cat <<YAML
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization

# Deploys PodInfoApp instances (captured from $PREVIEW_NS by gitops-reverser)
# into the $PRODUCTION_NS namespace.
#
# This file is meant to be committed to the '$BRANCH' branch of the repo.
# gitops-reverser writes each PodInfoApp manifest to:
#   $GIT_PATH/kro.run/v1alpha1/podinfoapps/$PREVIEW_NS/<name>.yaml
#
# To apply:
#   git checkout $BRANCH
#   kubectl apply -k $OUTPUT_DIR/

namespace: $PRODUCTION_NS

resources:
YAML

  if [[ -n "$PODINFO_NAMES" ]]; then
    while IFS= read -r name; do
      echo "  - ../$GIT_PATH/kro.run/v1alpha1/podinfoapps/$PREVIEW_NS/$name.yaml"
    done <<< "$PODINFO_NAMES"
  else
    cat <<YAML
  # No PodInfoApp instances found yet in '$PREVIEW_NS'.
  # gitops-reverser will sync them here once they are created.
  # Add entries for each instance, e.g.:
  #   - ../$GIT_PATH/kro.run/v1alpha1/podinfoapps/$PREVIEW_NS/<name>.yaml
YAML
  fi
} > "$OUTPUT_DIR/kustomization.yaml"

ok "Kustomization written with $(echo "$PODINFO_NAMES" | grep -c . || echo 0) resource(s)."

# ──────────────────────────────────────────────────────────────────────────────
# Done
# ──────────────────────────────────────────────────────────────────────────────
echo ""
echo "All done!"
echo ""
echo "  Resources created in namespace '$PREVIEW_NS':"
echo "    Secret:      $COPIED_SECRET_NAME  (copied from $SECRET_NS/$SECRET_NAME)"
echo "    GitProvider: $PROVIDER_NAME"
echo "    GitTarget:   $TARGET_NAME  (branch: $BRANCH, path: $GIT_PATH)"
echo "    WatchRule:   $WATCHRULE_NAME  (watches: PodInfoApp)"
echo ""
echo "  Production kustomization:"
echo "    $OUTPUT_DIR/kustomization.yaml"
echo ""
echo "Next steps:"
echo "  1. Verify resources are Ready:"
echo "       kubectl get gitproviders,gittargets,watchrules -n $PREVIEW_NS"
echo ""
echo "  2. Create or update PodInfoApp instances in '$PREVIEW_NS' —"
echo "     gitops-reverser pushes every change to branch '$BRANCH'."
echo ""
echo "  3. Commit $OUTPUT_DIR/kustomization.yaml to the '$BRANCH' branch,"
echo "     then apply it to the cluster to promote instances to production:"
echo "       kubectl apply -k $OUTPUT_DIR/"
