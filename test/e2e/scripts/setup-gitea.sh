#!/bin/bash
set -euo pipefail

# Configuration
GITEA_NAMESPACE=${GITEA_NAMESPACE:-gitea-e2e}
ADMIN_USER="giteaadmin"
ADMIN_PASS="giteapassword123"
ORG_NAME="testorg"
REPO_NAME="${1:-}"
CHECKOUT_DIR="${2:-}"
TARGET_NAMESPACE="${TARGET_NAMESPACE:-${SUT_NAMESPACE:-${QUICKSTART_NAMESPACE:-${NAMESPACE:-sut}}}}"
SECRET_NAME="git-creds"
SSH_SECRET_NAME="git-creds-ssh"
SSH_KEY_PATH="/tmp/e2e-ssh-key"
SSH_PUB_KEY_PATH="/tmp/e2e-ssh-key.pub"
API_URL="http://localhost:13000/api/v1"

if [ -z "$REPO_NAME" ]; then
    echo "❌ Error: Repository name must be provided as first argument"
    echo "Usage: $0 <repo-name> <checkout-dir>"
    exit 1
fi

if [ -z "$CHECKOUT_DIR" ]; then
    echo "❌ Error: Full checkout dir (including repo name if you wish) must be provided as second argument"
    echo "Usage: $0 <repo-name> <checkout-dir-including-name>"
    exit 1
fi

echo "🚀 Setting up Gitea test environment with repository: $REPO_NAME"

test_api_connectivity() {
    # Installation and port-forward setup are handled by Makefile stamp targets.
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
    organization_exists() {
        local status
        status=$(curl -s -w "%{http_code}" -o /tmp/org_get_response.json \
            -X GET "$API_URL/orgs/$ORG_NAME" \
            -H "Content-Type: application/json" \
            -u "$ADMIN_USER:$ADMIN_PASS" || true)
        [[ "$status" == "200" ]]
    }

    echo "🏢 Ensuring test organization '$ORG_NAME' exists..."
    if organization_exists; then
        echo "ℹ️  Organization already exists"
    else
        local attempt
        for attempt in {1..5}; do
            ORG_RESPONSE=$(curl -s -w "%{http_code}" -o /tmp/org_response.json \
                -X POST "$API_URL/orgs" \
                -H "Content-Type: application/json" \
                -u "$ADMIN_USER:$ADMIN_PASS" \
                -d "{\"username\":\"$ORG_NAME\",\"full_name\":\"Test Organization\",\"description\":\"E2E Test Organization\"}" || true)

            if [[ "$ORG_RESPONSE" == "201" ]]; then
                echo "✅ Organization created successfully"
                break
            fi
            if [[ "$ORG_RESPONSE" == "409" ]] || [[ "$ORG_RESPONSE" == "422" ]]; then
                echo "ℹ️  Organization already exists"
                break
            fi
            if organization_exists; then
                echo "ℹ️  Organization already exists"
                break
            fi

            echo "⚠️  Attempt $attempt/5: failed to create organization (status=${ORG_RESPONSE:-none})"
            cat /tmp/org_response.json 2>/dev/null || true
            if [[ "$attempt" -lt 5 ]]; then
                sleep 1
            fi
        done
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
        -d "{\"name\":\"$REPO_NAME\",\"description\":\"E2E Test Repository\",\"private\":false,\"auto_init\":false}")

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

    echo "✅ HTTP Git credentials secret ($TARGET_NAMESPACE/$SECRET_NAME) created successfully"

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
            # Verify the known_hosts format is valid
            if ! grep -q "ssh-" "$TEMP_KNOWN_HOSTS"; then
                echo "⚠️  Retrieved SSH host key format is invalid, generating fallback"
                echo "[$SSH_HOST]:2222 ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7vbqajaaAAAEBgW5TTHlNUG..." > "$TEMP_KNOWN_HOSTS"
            fi
        else
            echo "⚠️  Could not retrieve SSH host key, generating fallback known_hosts entry"
            # Create a valid known_hosts entry with proper format: [host]:port key-type key-data
            # Using a generic RSA key format that Git will accept
            echo "[$SSH_HOST]:2222 ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABAQC7vbqajaaAAAEBgW5TTHlNUGzvTTHlNUGzvTTHlNUGzvTTHlNUGzvTTHlNUGzvTTHlNUGzvTTHlNUGzvTTHlNUGzvTTHlNUGzvTTHlNUGzvTTHlNUGzvTTHlNUGzvTTHlNUGzvTTHlNUGzvTTHlNUGzvTTHlNUGzv" > "$TEMP_KNOWN_HOSTS"
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
        
        echo "✅ SSH Git credentials secret ($TARGET_NAMESPACE/$SSH_SECRET_NAME) created successfully"
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

    echo "✅ Invalid credentials secret ($TARGET_NAMESPACE/${SECRET_NAME}-invalid) created for testing purposes"

}

# Function to checkout repository with authentication
checkout_repository() {
    echo "📂 Setting up repository checkout in $CHECKOUT_DIR..."
    
    # Remove existing checkout directory if it exists
    rm -rf "$CHECKOUT_DIR"
    
    # Create parent directory
    mkdir -p "$(dirname "$CHECKOUT_DIR")"
    
    # Configure git for localhost:13000 authentication using credentials
    # This creates a global git config that maps the localhost URL to use our credentials
    REPO_URL_WITH_AUTH="http://$ADMIN_USER:$TOKEN@localhost:13000/$ORG_NAME/$REPO_NAME.git"
    REPO_URL_LOCALHOST="http://localhost:13000/$ORG_NAME/$REPO_NAME.git"
    
    echo "🔐 Configuring git authentication for localhost:13000..."
    # Set up URL rewriting so git will use our credentials automatically
    git config --global "url.$REPO_URL_WITH_AUTH.insteadOf" "$REPO_URL_LOCALHOST"
    
    echo "📥 Cloning repository to $CHECKOUT_DIR..."
    if git clone "$REPO_URL_LOCALHOST" "$CHECKOUT_DIR"; then
        echo "✅ Repository cloned successfully"
        
        # Configure git settings in the checkout directory for future operations
        cd "$CHECKOUT_DIR" || exit 1
        git config user.name "E2E Test"
        git config user.email "e2e-test@gitops-reverser.local"
        
        # Set up the remote URL to use localhost:13000 (authentication is handled by global config)
        git remote set-url origin "$REPO_URL_LOCALHOST"
        
        echo "🔧 Git configuration completed in checkout directory"
        echo "   • Directory: $CHECKOUT_DIR"
        echo "   • Remote URL: $REPO_URL_LOCALHOST"
        echo "   • Authentication: Configured via global git config"
        
        cd - > /dev/null || true
    else
        echo "❌ Failed to clone repository"
        # Clean up git config on failure
        git config --global --unset "url.$REPO_URL_WITH_AUTH.insteadOf" 2>/dev/null || true
        exit 1
    fi
}

# Main execution logic - full setup with specified repository
test_api_connectivity
setup_organization_and_token
generate_ssh_keys
configure_ssh_key_in_gitea
create_repository
setup_credentials
checkout_repository

# Repository information
REPO_URL="http://gitea-http.$GITEA_NAMESPACE.svc.cluster.local:13000/$ORG_NAME/$REPO_NAME.git"

echo "
🎉 Gitea setup completed successfully!

📋 Configuration Details:
   • Namespace: $GITEA_NAMESPACE
   • Organization: $ORG_NAME
   • Repository: $REPO_NAME
   • Secret: $SECRET_NAME (in $TARGET_NAMESPACE namespace)
   • Repository URL: $REPO_URL
   • Checkout Directory: $CHECKOUT_DIR
    
🔧 For debugging:
   • Admin User: $ADMIN_USER
   • Admin Pass: $ADMIN_PASS
   • Access Token: ${TOKEN:0:8}...

🌐 Access Gitea:
   • Visit http://localhost:13000 in your browser
   • Login: $ADMIN_USER / $ADMIN_PASS
   • Stop port-forward: pkill -f 'kubectl.*port-forward.*13000'

📂 Git Repository:
   • Local checkout: $CHECKOUT_DIR
   • Git operations configured for localhost:13000
   • Ready for git pull/fetch operations during tests

✨ Ready for e2e testing! Port-forward will stay active.
"

# Cleanup temporary files
rm -f /tmp/org_response.json /tmp/repo_response.json /tmp/token_response.json /tmp/token_list.json \
    /tmp/ssh_key_response.json /tmp/org_get_response.json
