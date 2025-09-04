#!/bin/bash
set -euo pipefail

# Configuration
GITEA_NAMESPACE=${GITEA_NAMESPACE:-gitea-e2e}
GITEA_SERVICE="gitea-http"
ADMIN_USER="giteaadmin"
ADMIN_PASS="giteapassword123"
ORG_NAME="testorg"
REPO_NAME="testrepo"
TARGET_NAMESPACE="sut"
SECRET_NAME="git-ssh-key"

echo "🚀 Setting up Gitea test environment..."

# Wait for Gitea to be ready
echo "⏳ Waiting for Gitea to be ready..."
kubectl wait --for=condition=ready pod \
    -l app.kubernetes.io/name=gitea \
    -n "$GITEA_NAMESPACE" \
    --timeout=300s

echo "✅ Gitea pod is ready"

# Kill any existing port-forwards on port 3000
echo "🔧 Cleaning up any existing port-forwards..."
pkill -f "kubectl.*port-forward.*3000" 2>/dev/null || true
sleep 2

# Setup port-forward for API access
echo "🔗 Setting up port-forward..."
kubectl port-forward -n "$GITEA_NAMESPACE" "svc/$GITEA_SERVICE" 3000:3000 &
PF_PID=$!

# Function to cleanup port-forward
cleanup() {
    echo "🧹 Cleaning up port-forward..."
    kill $PF_PID 2>/dev/null || true
    wait $PF_PID 2>/dev/null || true
    pkill -f "kubectl.*port-forward.*3000" 2>/dev/null || true
}
trap cleanup EXIT

# Wait for port-forward to be established
sleep 5

# API Base URL
API_URL="http://localhost:3000/api/v1"

# Test API connectivity
echo "🔍 Testing API connectivity..."
for i in {1..30}; do
    if curl -s -f "$API_URL/version" >/dev/null 2>&1; then
        echo "✅ API is accessible"
        break
    fi
    if [ $i -eq 30 ]; then
        echo "❌ Failed to connect to Gitea API after 30 attempts"
        exit 1
    fi
    echo "⏳ Waiting for API... (attempt $i/30)"
    sleep 2
done

# Create organization
echo "🏢 Creating test organization '$ORG_NAME'..."
ORG_RESPONSE=$(curl -s -w "%{http_code}" -o /tmp/org_response.json \
    -X POST "$API_URL/orgs" \
    -H "Content-Type: application/json" \
    -u "$ADMIN_USER:$ADMIN_PASS" \
    -d "{\"username\":\"$ORG_NAME\",\"full_name\":\"Test Organization\",\"description\":\"E2E Test Organization\"}")

if [[ "$ORG_RESPONSE" == "201" ]]; then
    echo "✅ Organization created successfully"
elif [[ "$ORG_RESPONSE" == "409" ]]; then
    echo "ℹ️  Organization already exists"
else
    echo "⚠️  Unexpected response creating organization: $ORG_RESPONSE"
    cat /tmp/org_response.json
fi

# Create repository
echo "📁 Creating test repository '$REPO_NAME'..."
REPO_RESPONSE=$(curl -s -w "%{http_code}" -o /tmp/repo_response.json \
    -X POST "$API_URL/orgs/$ORG_NAME/repos" \
    -H "Content-Type: application/json" \
    -u "$ADMIN_USER:$ADMIN_PASS" \
    -d "{\"name\":\"$REPO_NAME\",\"description\":\"E2E Test Repository\",\"private\":false,\"auto_init\":true}")

if [[ "$REPO_RESPONSE" == "201" ]]; then
    echo "✅ Repository created successfully"
elif [[ "$REPO_RESPONSE" == "409" ]]; then
    echo "ℹ️  Repository already exists"
else
    echo "⚠️  Unexpected response creating repository: $REPO_RESPONSE"
    cat /tmp/repo_response.json
fi

# Generate access token
echo "🔑 Generating access token..."
TOKEN_RESPONSE=$(curl -s -w "%{http_code}" -o /tmp/token_response.json \
    -X POST "$API_URL/users/$ADMIN_USER/tokens" \
    -H "Content-Type: application/json" \
    -u "$ADMIN_USER:$ADMIN_PASS" \
    -d "{\"name\":\"e2e-test-token-$(date +%s)\",\"scopes\":[\"write:repository\",\"read:repository\",\"write:organization\",\"read:organization\"]}")

if [[ "$TOKEN_RESPONSE" == "201" ]]; then
    echo "✅ Access token created successfully"
    # Extract token from response
    TOKEN=$(cat /tmp/token_response.json | grep -o '"sha1":"[^"]*"' | cut -d'"' -f4)
    if [[ -z "$TOKEN" ]]; then
        echo "❌ Failed to extract token from response"
        cat /tmp/token_response.json
        exit 1
    fi
else
    echo "❌ Failed to create access token: $TOKEN_RESPONSE"
    cat /tmp/token_response.json
    exit 1
fi

# Create target namespace if it doesn't exist
echo "🏗️  Ensuring target namespace '$TARGET_NAMESPACE' exists..."
kubectl create namespace "$TARGET_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

# Create Git credentials secret for HTTP authentication (username/password)
echo "🔐 Creating Git credentials secret for HTTP authentication..."
kubectl create secret generic "$SECRET_NAME" \
    --namespace="$TARGET_NAMESPACE" \
    --from-literal=username="$ADMIN_USER" \
    --from-literal=password="$TOKEN" \
    --dry-run=client -o yaml | kubectl apply -f -

echo "✅ Git credentials secret created successfully"

# Also create an invalid secret for failure testing
echo "🔐 Creating invalid credentials secret for failure testing..."
kubectl create secret generic "${SECRET_NAME}-invalid" \
    --namespace="$TARGET_NAMESPACE" \
    --from-literal=username="invaliduser" \
    --from-literal=password="invalidpassword" \
    --dry-run=client -o yaml | kubectl apply -f -

echo "✅ Invalid credentials secret created for testing"

# Repository information
REPO_URL="https://gitea-http.$GITEA_NAMESPACE.svc.cluster.local:3000/$ORG_NAME/$REPO_NAME.git"

echo "
🎉 Gitea setup completed successfully!

📋 Configuration Details:
   • Namespace: $GITEA_NAMESPACE
   • Organization: $ORG_NAME  
   • Repository: $REPO_NAME
   • Secret: $SECRET_NAME (in $TARGET_NAMESPACE namespace)
   • Repository URL: $REPO_URL
   
🔧 For debugging:
   • Admin User: $ADMIN_USER
   • Admin Pass: $ADMIN_PASS
   • Access Token: ${TOKEN:0:8}...
   
✨ Ready for e2e testing!
"

# Cleanup temporary files
rm -f /tmp/org_response.json /tmp/repo_response.json /tmp/token_response.json