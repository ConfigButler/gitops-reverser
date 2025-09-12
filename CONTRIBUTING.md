# Contributing to GitOps Reverser

First off, thank you for considering contributing to GitOps Reverser! It's people like you that make open source such a great community.

We welcome any type of contribution, not just code. You can help with:

*   **Reporting a bug:** If you find a bug, please open an issue and provide as much information as possible.
*   **Suggesting a feature:** If you have an idea for a new feature, open an issue to discuss it.
*   **Writing documentation:** We can always use more documentation, whether it's for `godoc`, the project's README, or tutorials.
*   **Submitting a Pull Request:** If you want to contribute code, please follow the steps below.

## Getting Started

### Prerequisites

*   Go (version 1.25 or higher)
*   Docker
*   `kubebuilder`
*   A running Kubernetes cluster (like `kind` or `minikube`) to run tests against.

### Development Environment Setup

1.  **Fork & Clone:** Fork the repository to your own GitHub account and then clone it to your local machine.

    ```sh
    git clone https://github.com/YOUR_USERNAME/gitops-reverser.git
    cd gitops-reverser
    ```

2.  **Install Dependencies:**

    ```sh
    go mod tidy
    ```

### Running Tests

The project has a comprehensive test suite. Before submitting a pull request, please ensure that all tests pass.

*   **Run Unit Tests:**
    ```sh
    make test
    ```

*   **Run Integration Tests (requires a Kubernetes cluster):**
    ```sh
    make test-integration
    ```

### Linting and Formatting

All code must be formatted with `goimports` and pass `golangci-lint` checks.

*   **Format Code:**
    ```sh
    make fmt
    ```

*   **Run Linter:**
    ```sh
    make lint
    ```

## Development Rules

**Mandatory validation before PR submission:**

```bash
make lint      # Must pass golangci-lint checks
make test      # Must pass all unit tests with >90% coverage
make test-e2e  # Must pass end-to-end tests
```

For detailed implementation guidelines, see [`.kilocode/rules/implementation-rules.md`](.kilocode/rules/implementation-rules.md).

## Submitting a Pull Request

1.  Create a new branch for your feature or bug fix.
    ```sh
    git checkout -b my-awesome-feature
    ```
2.  Make your changes and commit them with a clear, descriptive message.
3.  Push your branch to your fork.
    ```sh
    git push origin my-awesome-feature
    ```
4.  Open a pull request against the `main` branch of the `ConfigButler/gitops-reverser` repository.

A GitHub Actions CI pipeline will automatically run on your PR to check for linting errors and run the test suite. All checks must pass before a PR can be merged.

Thank you for your contribution!