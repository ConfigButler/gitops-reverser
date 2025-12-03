#!/bin/bash
# Script to create Kind cluster with proper host path substitution for Docker-in-Docker

set -e

CLUSTER_NAME="${KIND_CLUSTER:-gitops-reverser-test-e2e}"
TEMPLATE_FILE="test/e2e/kind/cluster-template.yaml"
CONFIG_FILE="test/e2e/kind/cluster.ignore.yaml"

# Check if HOST_PROJECT_PATH is set
if [ -z "$HOST_PROJECT_PATH" ]; then
    echo "❌ ERROR: HOST_PROJECT_PATH environment variable is not set"
    echo "This should be set in .devcontainer/devcontainer.json"
    exit 1
fi

echo "🔧 Using HOST_PROJECT_PATH: $HOST_PROJECT_PATH"

# Use envsubst to replace ${HOST_PROJECT_PATH} in template
echo "📝 Generating Kind cluster configuration from template..."
envsubst < "$TEMPLATE_FILE" > "$CONFIG_FILE"

echo "✅ Generated configuration:"
cat "$CONFIG_FILE"
echo ""

# Check if cluster already exists
if kind get clusters 2>/dev/null | grep -q "^${CLUSTER_NAME}$"; then
    echo "✅ Cluster '$CLUSTER_NAME' already exists. Skipping creation."
    kind export kubeconfig --name "$CLUSTER_NAME"
else
    echo "🚀 Creating Kind cluster '$CLUSTER_NAME' with audit webhook support..."
    kind create cluster --name "$CLUSTER_NAME" --config "$CONFIG_FILE" --wait 5m
    echo "✅ Kind cluster created successfully"
fi

echo "📋 Configuring kubeconfig for cluster '$CLUSTER_NAME'..."
kind export kubeconfig --name "$CLUSTER_NAME"

echo "✅ Cluster setup complete!"
