# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.1.0] - 2025-01-31

### Features

* **Initial Release** - GitOps Reverser operator with core functionality
* **Admission Webhooks** - Capture manual cluster changes in real-time
* **Git Integration** - Automatic commit and push to Git repositories
* **WatchRule CRD** - Flexible rule-based resource monitoring
* **GitRepoConfig CRD** - Git repository configuration management
* **Race Condition Handling** - Intelligent conflict resolution with "last writer wins" strategy
* **Sanitization Engine** - Clean and format Kubernetes manifests before commit
* **Event Queue** - Buffer and batch changes for efficient processing
* **OpenTelemetry Metrics** - Comprehensive observability and monitoring
* **Multi-platform Support** - Docker images for linux/amd64 and linux/arm64
* **Helm Chart** - Easy deployment with configurable values
* **Comprehensive Testing** - Unit tests (>90% coverage), integration tests, and e2e tests

### Documentation

* Complete README with usage examples and architecture diagrams
* Contributing guidelines with development setup
* Testing documentation covering all test types
* Webhook setup guide for production deployments

---

**Note:** This changelog will be automatically updated by [release-please](https://github.com/googleapis/release-please) based on [Conventional Commits](https://www.conventionalcommits.org/). Future releases will have their changes automatically documented here.