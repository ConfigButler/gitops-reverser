# Development container with all tools pre-installed including Docker and Kind
# Extends the CI base image with Docker support for local development
# This is NOT for production - see root Dockerfile for that
# For local dev: builds CI base from local Dockerfile.ci
# For CI: uses pre-built image from GHCR
ARG CI_BASE_IMAGE=ci-base-local
FROM ${CI_BASE_IMAGE} AS ci-base

# Switch to noninteractive for Docker installation
ENV DEBIAN_FRONTEND=noninteractive

# Install Docker (for local development and Kind cluster management)
RUN apt-get update \
    && apt-get -y install --no-install-recommends \
        apt-transport-https \
        gnupg \
        lsb-release \
    && install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && chmod a+r /etc/apt/keyrings/docker.asc \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian bookworm stable" | tee /etc/apt/sources.list.d/docker.list > /dev/null \
    && apt-get update \
    && apt-get -y install --no-install-recommends \
        docker-ce \
        docker-ce-cli \
        containerd.io \
        docker-buildx-plugin \
        docker-compose-plugin \
    && apt-get autoremove -y \
    && apt-get clean -y \
    && rm -rf /var/lib/apt/lists/*

# Install Kind (for local Kubernetes clusters)
ENV KIND_VERSION=v0.30.0
RUN curl -Lo /usr/local/bin/kind https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-amd64 \
    && chmod +x /usr/local/bin/kind

# Verify Docker and Kind installations
RUN echo "=== Additional Dev Tools ===" \
    && kind version \
    && docker --version

# Switch back to dialog for any ad-hoc use of apt-get
ENV DEBIAN_FRONTEND=dialog

# Default command
CMD ["/bin/bash"]