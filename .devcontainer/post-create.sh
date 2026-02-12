#!/usr/bin/env bash

set -euo pipefail

# Keep docker socket usable in the devcontainer (best-effort)
sudo chmod 666 /var/run/docker.sock || true

# Ensure kind network exists (best-effort)
docker network create -d=bridge --subnet=172.19.0.0/24 kind || true

# Ensure Go-related caches exist and are writable by vscode
sudo mkdir -p \
  /home/vscode/.cache/go-build \
  /home/vscode/.cache/goimports \
  /home/vscode/.cache/golangci-lint

# Fix ownership for workspace and cache roots used by tooling
sudo chown -R vscode:vscode "${containerWorkspaceFolder}" /home/vscode/.cache || true

