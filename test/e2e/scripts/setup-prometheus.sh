#!/bin/bash
set -e

echo "Setting up Prometheus for e2e testing..."

# Create namespace
echo "Creating prometheus-e2e namespace..."
kubectl create namespace prometheus-e2e --dry-run=client -o yaml | kubectl apply -f -

# Apply RBAC
echo "Applying Prometheus RBAC..."
kubectl apply -f test/e2e/prometheus/rbac.yaml

# Deploy Prometheus
echo "Deploying Prometheus..."
kubectl apply -f test/e2e/prometheus/deployment.yaml

echo "Restarting Prometheus deployment to pick up ConfigMap changes..."
kubectl rollout restart deployment/prometheus -n prometheus-e2e
kubectl rollout status deployment/prometheus -n prometheus-e2e --timeout=120s

echo "✅ Prometheus manifests deployed"
