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

## Commit Message Format

This project uses [Conventional Commits](https://www.conventionalcommits.org/) for automated versioning and changelog generation. Please format your commit messages as follows:

```
<type>(<optional scope>): <description>

[optional body]

[optional footer(s)]
```

### Commit Types

- **feat:** A new feature (triggers minor version bump: 0.1.0 → 0.2.0)
- **fix:** A bug fix (triggers patch version bump: 0.1.0 → 0.1.1)
- **docs:** Documentation changes (no version bump)
- **style:** Code style changes (formatting, etc., no version bump)
- **refactor:** Code refactoring (no version bump)
- **perf:** Performance improvements (triggers patch version bump)
- **test:** Adding or updating tests (no version bump)
- **build:** Changes to build system or dependencies (no version bump)
- **ci:** CI/CD configuration changes (no version bump)
- **chore:** Other changes that don't modify src or test files (no version bump)
- **revert:** Reverts a previous commit (triggers patch version bump)

### Breaking Changes

To trigger a major version bump (0.1.0 → 1.0.0), include `BREAKING CHANGE:` in the commit footer or add `!` after the type:

```
feat!: redesign API structure

BREAKING CHANGE: The API now uses a different authentication method
```

### Examples

**Feature commit:**
```
feat(controller): add support for multi-repository configurations

This allows users to configure multiple Git repositories for different
namespaces, improving flexibility in audit trail organization.
```

**Bug fix commit:**
```
fix(webhook): prevent race condition in event queue

Fixes #123
```

**Documentation update:**
```
docs: update README with Helm installation instructions
```

## Automated Releases

When commits are pushed to the `main` branch:

1. **CI runs automatically** - All tests must pass (lint, unit tests, e2e)
2. **Release PR created** - If CI passes, [release-please](https://github.com/googleapis/release-please) analyzes commits and creates/updates a Release PR
3. **Changelog generated** - The Release PR includes an auto-generated changelog
4. **Version bumped** - Chart.yaml versions are updated automatically based on commit types
5. **Docker images built** - When the Release PR is merged, Docker images are built and published

**Note:** Only conventional commit messages will trigger version bumps. Non-conventional commits are still recorded in the changelog but won't affect versioning.

## Submitting a Pull Request

1.  Create a new branch for your feature or bug fix.
    ```sh
    git checkout -b my-awesome-feature
    ```
2.  Make your changes and commit them with a clear, descriptive message following the Conventional Commits format.
3.  Push your branch to your fork.
    ```sh
    git push origin my-awesome-feature
    ```
4.  Open a pull request against the `main` branch of the `ConfigButler/gitops-reverser` repository.

A GitHub Actions CI pipeline will automatically run on your PR to check for linting errors and run the test suite. All checks must pass before a PR can be merged.

Thank you for your contribution!