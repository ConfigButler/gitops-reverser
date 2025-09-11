#!/bin/bash
set -euo pipefail

# Configuration
GITEA_NAMESPACE=${GITEA_NAMESPACE:-gitea-e2e}
GITEA_SERVICE="gitea-http"
ADMIN_USER="giteaadmin"
ADMIN_PASS="giteapassword123"
ORG_NAME="testorg"
REPO_NAME="${2:-testrepo}"
TARGET_NAMESPACE="sut"
SECRET_NAME="git-creds"
SSH_SECRET_NAME="git-creds-ssh"
ACTION="${1:-setup}"
SSH_KEY_PATH="/tmp/e2e-ssh-key"
SSH_PUB_KEY_PATH="/tmp/e2e-ssh-key.pub"
API_URL="http://localhost:3000/api/v1"
if [ "$ACTION" = "create-repo" ]; then
    echo "🚀 Creating unique test repository: $REPO_NAME"
else
    echo "🚀 Setting up Gitea test environment..."
fi

# Function to setup Gitea installation
wait_gitea() {
    echo "⏳ Waiting for Gitea to be ready..."
    kubectl wait --for=condition=ready pod \
        -l app.kubernetes.io/name=gitea \
        -n "$GITEA_NAMESPACE" \
        --timeout=300s

    echo "✅ Gitea is ready"
}

# Function to setup API connectivity
setup_persistant_port_forward() {
    # Kill any existing port-forwards on port 3000
    echo "🔧 Cleaning up any existing port-forwards..."
    pkill -f "kubectl.*port-forward.*3000" 2>/dev/null || true
    sleep 2

    # Setup port-forward for API access (persistent for e2e testing)
    echo "🔗 Setting up persistent port-forward to Gitea on localhost:3000..."
    echo "💡 Note: Port-forward will remain active after script completion. Use 'pkill -f \"kubectl.*port-forward.*3000\"' to stop."
    
    # Start port-forward as a fully detached background process using nohup and disown
    nohup kubectl port-forward -n "$GITEA_NAMESPACE" "svc/$GITEA_SERVICE" 3000:3000 >/dev/null 2>&1 &
    PF_PID=$!
    
    # Detach the process from the current shell session
    disown $PF_PID 2>/dev/null || true
    
    # Wait for port-forward to be established
    sleep 5
}

test_api_connectivity() {
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
}

# Function to create organization and get token
setup_organization_and_token() {
    echo "🏢 Creating test organization '$ORG_NAME'..."
    ORG_RESPONSE=$(curl -s -w "%{http_code}" -o /tmp/org_response.json \
        -X POST "$API_URL/orgs" \
        -H "Content-Type: application/json" \
        -u "$ADMIN_USER:$ADMIN_PASS" \
        -d "{\"username\":\"$ORG_NAME\",\"full_name\":\"Test Organization\",\"description\":\"E2E Test Organization\"}")

    if [[ "$ORG_RESPONSE" == "201" ]]; then
        echo "✅ Organization created successfully"
    elif [[ "$ORG_RESPONSE" == "409" ]] || [[ "$ORG_RESPONSE" == "422" ]]; then
        echo "ℹ️  Organization already exists"
    else
        echo "⚠️  Unexpected response creating organization: $ORG_RESPONSE"
        cat /tmp/org_response.json 2>/dev/null || true
        echo "ℹ️  Continuing setup despite organization creation issue..."
    fi

    # Generate or reuse access token
    echo "🔑 Getting access token..."
    TOKEN_RESPONSE=$(curl -s -w "%{http_code}" -o /tmp/token_response.json \
        -X POST "$API_URL/users/$ADMIN_USER/tokens" \
        -H "Content-Type: application/json" \
        -u "$ADMIN_USER:$ADMIN_PASS" \
        -d "{\"name\":\"e2e-test-token-persistent\",\"scopes\":[\"write:repository\",\"read:repository\",\"write:organization\",\"read:organization\"]}")

    if [[ "$TOKEN_RESPONSE" == "201" ]]; then
        echo "✅ Access token created successfully"
        TOKEN=$(cat /tmp/token_response.json | grep -o '"sha1":"[^"]*"' | cut -d'"' -f4)
    elif [[ "$TOKEN_RESPONSE" == "422" ]]; then
        echo "ℹ️  Token already exists, retrieving existing tokens..."
        # Get existing tokens and use the first one
        if curl -s "$API_URL/users/$ADMIN_USER/tokens" \
            -u "$ADMIN_USER:$ADMIN_PASS" > /tmp/token_list.json 2>/dev/null; then
            TOKEN=$(cat /tmp/token_list.json 2>/dev/null | grep -o '"sha1":"[^"]*"' | head -1 | cut -d'"' -f4 || echo "")
        fi
        if [[ -z "$TOKEN" ]]; then
            echo "⚠️  Using admin credentials directly"
            TOKEN="$ADMIN_PASS"
        fi
    else
        echo "⚠️  Using admin credentials as fallback"
        TOKEN="$ADMIN_PASS"
    fi

    if [[ -z "$TOKEN" ]]; then
        echo "❌ Failed to get token"
        exit 1
    fi
}

# Function to create a specific repository
create_repository() {
    echo "📁 Creating test repository '$REPO_NAME'..."
    REPO_RESPONSE=$(curl -s -w "%{http_code}" -o /tmp/repo_response.json \
        -X POST "$API_URL/orgs/$ORG_NAME/repos" \
        -H "Content-Type: application/json" \
        -u "$ADMIN_USER:$ADMIN_PASS" \
        -d "{\"name\":\"$REPO_NAME\",\"description\":\"E2E Test Repository\",\"private\":false,\"auto_init\":true}")

    if [[ "$REPO_RESPONSE" == "201" ]]; then
        echo "✅ Repository '$REPO_NAME' created successfully"
    elif [[ "$REPO_RESPONSE" == "409" ]]; then
        echo "ℹ️  Repository '$REPO_NAME' already exists"
    else
        echo "⚠️  Unexpected response creating repository: $REPO_RESPONSE"
        cat /tmp/repo_response.json || true
        # Don't exit on repo creation failure for individual repos
    fi
}

# Function to generate SSH key pair
generate_ssh_keys() {
    echo "🔑 Generating SSH key pair for testing..."
    
    # Remove existing keys
    rm -f "$SSH_KEY_PATH" "$SSH_PUB_KEY_PATH"
    
    # Generate new SSH key pair without passphrase for e2e testing
    # Use 4096 bits to meet Gitea's security requirements (needs at least 3071)
    ssh-keygen -t rsa -b 4096 -f "$SSH_KEY_PATH" -N "" -C "e2e-test@gitops-reverser" >/dev/null 2>&1
    
    if [[ ! -f "$SSH_KEY_PATH" ]] || [[ ! -f "$SSH_PUB_KEY_PATH" ]]; then
        echo "❌ Failed to generate SSH key pair"
        exit 1
    fi
    
    echo "✅ SSH key pair generated successfully"
}

# Function to configure SSH key in Gitea
configure_ssh_key_in_gitea() {
    echo "🔐 Configuring SSH key in Gitea..."
    
    if [[ ! -f "$SSH_PUB_KEY_PATH" ]]; then
        echo "❌ SSH public key not found"
        exit 1
    fi
    
    SSH_PUB_KEY_CONTENT=$(cat "$SSH_PUB_KEY_PATH")
    
    # First, delete existing SSH keys to ensure we use the current key
    echo "🧹 Removing existing SSH keys..."
    EXISTING_KEYS=$(curl -s -X GET "$API_URL/user/keys" \
        -H "Content-Type: application/json" \
        -u "$ADMIN_USER:$ADMIN_PASS" || echo "[]")
    
    # Only process if we have keys to delete
    if echo "$EXISTING_KEYS" | grep -q '"id":'; then
        echo "$EXISTING_KEYS" | grep -o '"id":[0-9]*' | sed 's/"id"://' | \
        while read -r key_id; do
            echo "🗑️  Deleting SSH key ID: $key_id"
            curl -s -X DELETE "$API_URL/user/keys/$key_id" \
                -H "Content-Type: application/json" \
                -u "$ADMIN_USER:$ADMIN_PASS" >/dev/null 2>&1 || true
        done
    else
        echo "ℹ️  No existing SSH keys to remove"
    fi
    
    # Add SSH key to admin user
    SSH_KEY_RESPONSE=$(curl -s -w "%{http_code}" -o /tmp/ssh_key_response.json \
        -X POST "$API_URL/user/keys" \
        -H "Content-Type: application/json" \
        -u "$ADMIN_USER:$ADMIN_PASS" \
        -d "{\"title\":\"E2E Test Key\",\"key\":\"$SSH_PUB_KEY_CONTENT\"}")
    
    if [[ "$SSH_KEY_RESPONSE" == "201" ]]; then
        echo "✅ SSH key configured successfully in Gitea"
    elif [[ "$SSH_KEY_RESPONSE" == "422" ]]; then
        echo "⚠️  SSH key rejected by Gitea: $(cat /tmp/ssh_key_response.json 2>/dev/null || echo 'unknown error')"
        echo "ℹ️  SSH authentication tests will be skipped, but HTTP tests will continue"
        # Don't fail the setup - HTTP authentication should still work
        return 0
    else
        echo "⚠️  Unexpected response configuring SSH key: $SSH_KEY_RESPONSE"
        cat /tmp/ssh_key_response.json 2>/dev/null || true
        echo "ℹ️  SSH authentication may not work, but HTTP tests will continue"
        # Don't fail the setup for SSH key issues
    fi
}

setup_credentials() {
    # Create target namespace if it doesn't exist
    echo "🏗️  Ensuring target namespace '$TARGET_NAMESPACE' exists..."
    kubectl create namespace "$TARGET_NAMESPACE" --dry-run=client -o yaml | kubectl apply -f -

    # Create Git credentials secret for HTTP authentication (username/password)
    echo "🔐 Creating HTTP Git credentials secret..."
    kubectl create secret generic "$SECRET_NAME" \
        --namespace="$TARGET_NAMESPACE" \
        --from-literal=username="$ADMIN_USER" \
        --from-literal=password="$TOKEN" \
        --dry-run=client -o yaml | kubectl apply -f -

    echo "✅ HTTP Git credentials secret created successfully"

    # Create SSH-based credentials secret
    if [[ -f "$SSH_KEY_PATH" ]]; then
        echo "🔐 Creating SSH Git credentials secret..."
        
        # Generate known_hosts entry for the Gitea SSH service
        echo "🔑 Generating known_hosts entry for Gitea SSH..."
        SSH_HOST="gitea-ssh.$GITEA_NAMESPACE.svc.cluster.local"
        
        # Get the actual SSH host key from the Gitea SSH service
        echo "🔍 Retrieving SSH host key from Gitea..."
        TEMP_KNOWN_HOSTS="/tmp/temp_known_hosts"
        
        # Try to get the SSH host key by connecting to the SSH service
        if timeout 10 ssh-keyscan -p 2222 "$SSH_HOST" > "$TEMP_KNOWN_HOSTS" 2>/dev/null && [[ -s "$TEMP_KNOWN_HOSTS" ]]; then
            echo "✅ Retrieved SSH host key successfully"
        else
            echo "⚠️  Could not retrieve SSH host key, using permissive configuration"
            # Create a permissive known_hosts that accepts any key for this host
            echo "$SSH_HOST,gitea-ssh *" > "$TEMP_KNOWN_HOSTS"
        fi
        
        # Use the generated known_hosts
        cp "$TEMP_KNOWN_HOSTS" /tmp/known_hosts_ssh
        
        kubectl create secret generic "$SSH_SECRET_NAME" \
            --namespace="$TARGET_NAMESPACE" \
            --from-file=ssh-privatekey="$SSH_KEY_PATH" \
            --from-file=known_hosts="/tmp/known_hosts_ssh" \
            --dry-run=client -o yaml | kubectl apply -f -
        
        # Cleanup
        rm -f /tmp/known_hosts_ssh "$TEMP_KNOWN_HOSTS"
        
        echo "✅ SSH Git credentials secret created successfully"
    else
        echo "⚠️  SSH private key not found, skipping SSH secret creation"
    fi

    # Create an invalid secret for failure testing
    echo "🔐 Creating invalid credentials secret for failure testing..."
    kubectl create secret generic "${SECRET_NAME}-invalid" \
        --namespace="$TARGET_NAMESPACE" \
        --from-literal=username="invaliduser" \
        --from-literal=password="invalidpassword" \
        --dry-run=client -o yaml | kubectl apply -f -

    echo "✅ Invalid credentials secret created for testing"
}

# Main execution logic
if [ "$ACTION" = "create-repo" ]; then
    # Only create individual repository - assume Gitea is already running
    if ! kubectl get namespace "$GITEA_NAMESPACE" >/dev/null 2>&1; then
        echo "❌ Gitea namespace not found. Please run full setup first."
        exit 1
    fi
    
    setup_persistant_port_forward
    setup_organization_and_token
    create_repository
    
    echo "✅ Repository '$REPO_NAME' setup completed!"
    echo "💡 Port-forward to Gitea is active on localhost:3000 (if started previously)"
else
    # Full setup - install Gitea if needed
    wait_gitea
    setup_persistant_port_forward
    test_api_connectivity
    setup_organization_and_token
    generate_ssh_keys
    configure_ssh_key_in_gitea
    create_repository
    setup_credentials
    
    # Repository information
    REPO_URL="http://gitea-http.$GITEA_NAMESPACE.svc.cluster.local:3000/$ORG_NAME/$REPO_NAME.git"

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

🌐 Access Gitea:
   • Visit http://localhost:3000 in your browser
   • Login: $ADMIN_USER / $ADMIN_PASS
   • Stop port-forward: pkill -f 'kubectl.*port-forward.*3000'

✨ Ready for e2e testing! Port-forward will stay active.
"
fi

# Cleanup temporary files
rm -f /tmp/org_response.json /tmp/repo_response.json /tmp/token_response.json /tmp/token_list.json /tmp/ssh_key_response.json