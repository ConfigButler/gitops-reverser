# Multi-stage Dockerfile for GitOps Reverser
# Stage 1: CI base image with essential build tools
FROM golang:1.25.3-bookworm AS ci

# Avoid warnings by switching to noninteractive
ENV DEBIAN_FRONTEND=noninteractive

# Install essential packages (no Docker)
RUN apt-get update \
    && apt-get -y install --no-install-recommends \
    git \
    curl \
    ca-certificates \
    vim \
    less \
    jq \
    sudo \
    && apt-get autoremove -y \
    && apt-get clean -y \
    && rm -rf /var/lib/apt/lists/*

# Tool versions - centralized for easy updates
ENV KUBECTL_VERSION=v1.34.1 \
    KUSTOMIZE_VERSION=5.7.1 \
    KUBEBUILDER_VERSION=4.9.0 \
    GOLANGCI_LINT_VERSION=v2.5.0 \
    HELM_VERSION=v3.19.0

# Install kubectl
RUN curl -LO "https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl" \
    && chmod +x kubectl \
    && mv kubectl /usr/local/bin/

# Install Kustomize
RUN curl -Lo kustomize.tar.gz "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv${KUSTOMIZE_VERSION}/kustomize_v${KUSTOMIZE_VERSION}_linux_amd64.tar.gz" && \
    tar -zxvf kustomize.tar.gz && \
    mv kustomize /usr/local/bin/ && \
    rm kustomize.tar.gz

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

# Install Go tools used by the project (using @version doesn't need go.mod)
RUN go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0 \
    && go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

# Initialize golangci-lint cache by running it once on an empty directory
# This downloads linter dependencies without needing source code
RUN mkdir -p /tmp/golangci-init && cd /tmp/golangci-init \
    && go mod init example.com/init \
    && echo 'package main\n\nfunc main() {}' > main.go \
    && golangci-lint run --timeout=5m || true \
    && cd / && rm -rf /tmp/golangci-init

# Pre-download Go modules for caching - placed AFTER tool installation
# This layer will be cached and only rebuilt when go.mod/go.sum changes
# Moving this down prevents tool reinstallation when dependencies change
COPY go.mod go.sum ./
RUN go mod download

# Verify installations
RUN echo "=== Tool Versions ===" \
    && go version \
    && kubectl version --client \
    && kustomize version \
    && kubebuilder version \
    && helm version \
    && golangci-lint version

# Switch back to dialog for any ad-hoc use of apt-get
ENV DEBIAN_FRONTEND=dialog

# Default command
CMD ["/bin/bash"]

# Stage 2: Development container with all tools including Docker and Kind
FROM ci AS dev

USER root

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
    bash-completion \
    && apt-get autoremove -y \
    && apt-get clean -y \
    && rm -rf /var/lib/apt/lists/*

# Install Kind (for local Kubernetes clusters)
ENV KIND_VERSION=v0.30.0
RUN curl -Lo /usr/local/bin/kind https://kind.sigs.k8s.io/dl/${KIND_VERSION}/kind-linux-amd64 \
    && chmod +x /usr/local/bin/kind

# Install Delve debugger for Go debugging in VSCode
RUN go install github.com/go-delve/delve/cmd/dlv@latest

# Install VSCode Go extension tools (gopls and staticcheck)
RUN go install golang.org/x/tools/gopls@latest \
    && go install honnef.co/go/tools/cmd/staticcheck@latest

RUN echo ". /usr/share/bash-completion/bash_completion" >> /home/vscode/.bashrc

# Verify Docker and Kind installations
RUN echo "=== Additional Dev Tools ===" \
    && kind version \
    && docker --version \
    && dlv version \
    && gopls version \
    && staticcheck -version

# Create vscode user for non-root development
RUN groupadd --gid 1000 vscode \
    && useradd --uid 1000 --gid vscode --shell /bin/bash --create-home vscode \
    && echo 'vscode ALL=(ALL) NOPASSWD:ALL' >> /etc/sudoers.d/vscode \
    && chmod 0440 /etc/sudoers.d/vscode

# Ensure vscode user owns Go directories and has proper permissions
RUN mkdir -p /go && chown -R vscode:vscode /go

# Switch back to vscode user for development
USER vscode

# Switch back to dialog for any ad-hoc use of apt-get
ENV DEBIAN_FRONTEND=dialog

# Default command
CMD ["/bin/bash"]