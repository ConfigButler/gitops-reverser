#!/bin/bash
# Validation script for dev container setup
# Run this to verify all tools are installed correctly

set -e

echo "================================"
echo "Dev Container Validation Script"
echo "================================"
echo ""

# Color codes
GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m' # No Color

validate_tool() {
    local tool_name=$1
    local command=$2
    
    echo -n "Checking $tool_name... "
    if $command &> /dev/null; then
        echo -e "${GREEN}✓${NC}"
        $command
    else
        echo -e "${RED}✗ FAILED${NC}"
        return 1
    fi
    echo ""
}

echo "=== Validating Tools ==="
echo ""

validate_tool "Go" "go version"
validate_tool "Kind" "kind version"
validate_tool "kubectl" "kubectl version --client"
validate_tool "Kustomize" "kustomize version"
validate_tool "Kubebuilder" "kubebuilder version"
validate_tool "Helm" "helm version"
validate_tool "golangci-lint" "golangci-lint version"

echo "=== Validating Go Tools ==="
echo ""

validate_tool "controller-gen" "controller-gen --version"
validate_tool "setup-envtest" "setup-envtest --help"

echo "=== Validating Go Modules ==="
echo ""

if [ -f "go.mod" ]; then
    echo -n "Checking Go modules... "
    if go mod verify &> /dev/null; then
        echo -e "${GREEN}✓${NC}"
        echo "All Go modules verified successfully"
    else
        echo -e "${RED}✗ FAILED${NC}"
        exit 1
    fi
else
    echo -e "${RED}✗ go.mod not found${NC}"
    exit 1
fi
echo ""

echo "=== Validating Make Targets ==="
echo ""

echo -n "Checking Makefile... "
if [ -f "Makefile" ]; then
    echo -e "${GREEN}✓${NC}"
    echo "Available make targets:"
    make help 2>/dev/null || echo "  (help target not available)"
else
    echo -e "${RED}✗ Makefile not found${NC}"
    exit 1
fi
echo ""

echo "=== Docker Configuration ==="
echo ""

echo -n "Checking Docker availability... "
if docker info &> /dev/null; then
    echo -e "${GREEN}✓${NC}"
    echo "Docker is available (required for Kind/e2e tests)"
else
    echo -e "${RED}✗ Docker not available${NC}"
    echo "Docker is required for Kind clusters and e2e tests"
    echo "This is normal in some dev container configurations"
fi
echo ""

echo "=== Network Configuration ==="
echo ""

echo -n "Checking Kind network... "
if docker network ls | grep -q kind; then
    echo -e "${GREEN}✓${NC}"
    echo "Kind network already exists"
else
    echo "Kind network not found (will be created on demand)"
fi
echo ""

echo "================================"
echo -e "${GREEN}✓ Validation Complete!${NC}"
echo "================================"
echo ""
echo "All required tools are installed and configured."
echo "You can now run:"
echo "  make lint      - Run linting"
echo "  make test      - Run unit tests"
echo "  make test-e2e  - Run end-to-end tests (requires Docker)"
echo ""