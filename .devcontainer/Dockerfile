# Development container with all tools pre-installed
# This is NOT for production - see root Dockerfile for that
FROM golang:1.25.1

# Avoid warnings by switching to noninteractive
ENV DEBIAN_FRONTEND=noninteractive

# Configure apt and install packages including Docker
RUN apt-get update \
    && apt-get -y install --no-install-recommends \
        git \
        curl \
        ca-certificates \
        vim \
        less \
        jq \
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

# Tool versions - centralized for easy updates
ENV KIND_VERSION=v0.30.0 \
    KUBECTL_VERSION=v1.32.3 \
    KUSTOMIZE_VERSION=5.4.1 \
    KUBEBUILDER_VERSION=4.4.0 \
    GOLANGCI_LINT_VERSION=v2.4.0 \
    HELM_VERSION=v3.12.3

# Install Kind
RUN curl -Lo /usr/local/bin/kind https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-amd64 \
    && chmod +x /usr/local/bin/kind

# Install kubectl
RUN curl -LO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    && chmod +x kubectl \
    && mv kubectl /usr/local/bin/

# Install Kustomize
RUN curl -s "https://raw.githubusercontent.com/kubernetes-sigs/kustomize/master/hack/install_kustomize.sh" | bash -s ${KUSTOMIZE_VERSION} /usr/local/bin/

# Install Kubebuilder
RUN curl -L -o /usr/local/bin/kubebuilder "https://github.com/kubernetes-sigs/kubebuilder/releases/download/v${KUBEBUILDER_VERSION}/kubebuilder_linux_amd64" \
    && chmod +x /usr/local/bin/kubebuilder

# Install Helm
RUN curl -fsSL https://get.helm.sh/helm-${HELM_VERSION}-linux-amd64.tar.gz \
    | tar -xzO linux-amd64/helm > /usr/local/bin/helm \
    && chmod +x /usr/local/bin/helm

# Install golangci-lint
RUN curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
    | sh -s -- -b /usr/local/bin ${GOLANGCI_LINT_VERSION}

# Set working directory
WORKDIR /workspace

# Pre-download Go modules for caching (if go.mod exists)
# This layer will be cached and only rebuilt when go.mod/go.sum changes
COPY go.mod go.sum ./
RUN go mod download

# Install Go tools used by the project
# These are cached in a separate layer
RUN go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0 \
    && go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

# Copy golangci config to populate its cache
COPY .golangci.yml ./

# Copy source code to pre-populate golangci-lint cache
# This downloads all ~100 linter dependencies during build
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
COPY hack/ hack/

# Run golangci-lint to populate cache (will fail on code issues, that's ok)
# The important part is downloading all linter dependencies
RUN golangci-lint run --timeout=10m || true

# Clean up source files (not needed in final image, only cache matters)
RUN rm -rf api cmd internal hack .golangci.yml

# Verify installations
RUN echo "=== Tool Versions ===" \
    && go version \
    && kind version \
    && kubectl version --client \
    && kustomize version \
    && (kubebuilder version || echo "Kubebuilder: not available (optional)") \
    && helm version \
    && golangci-lint version

# Create Kind network for Docker-in-Docker
RUN mkdir -p /usr/local/bin/devcontainer-init.d

# Switch back to dialog for any ad-hoc use of apt-get
ENV DEBIAN_FRONTEND=dialog

# Default command
CMD ["/bin/bash"]