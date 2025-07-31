# GitOps Reverser Implementation Plan

This document outlines the tasks required to build the GitOps Reverser tool as specified in the PRD.

## Phase 1: Project Scaffolding & Core Types

- [ ] Initialize Go module and project structure.
- [ ] Define API types for `GitRepoConfig` CRD (`pkg/apis/configbutler.ai/v1alpha1/gitrepoconfig_types.go`).
- [ ] Define API types for `WatchRule` CRD (`pkg/apis/configbutler.ai/v1alpha1/watchrule_types.go`).
- [ ] Generate CRD manifests (`crd.yaml`).
- [ ] Create a `CONTRIBUTING.md` guide.

## Phase 2: Controller Implementation

- [ ] Implement `GitRepoConfig` controller (`internal/controller/gitrepoconfig_controller.go`).
- [ ] Implement `WatchRule` controller (`internal/controller/watchrule_controller.go`).
- [ ] Implement in-memory rule model for efficient webhook lookups.
- [ ] Implement leader election for High Availability.

## Phase 3: Webhook Implementation

- [ ] Implement the `ValidatingAdmissionWebhook` handler (`internal/webhook/webhook.go`).
- [ ] Add logic to filter incoming requests based on the in-memory rule model.
- [ ] Implement manifest sanitization logic.
- [ ] Implement the in-memory queue for decoupling webhook and Git operations.

## Phase 4: Git Operations & Commit Worker

- [ ] Implement the asynchronous Git worker.
- [ ] Implement Git logic for cloning, committing, and pushing.
- [ ] Implement batching logic based on `push.interval` and `push.maxCommits`.
- [ ] Implement structured commit messages.
- [ ] Implement logic for handling Git credentials via secrets.

## Phase 5: Observability & Metrics

- [ ] Set up OpenTelemetry (OTLP) exporter.
- [ ] Instrument code to expose key metrics (`events_received`, `events_processed`, `git_operations`, etc.).

## Phase 6: Testing

- [ ] Write unit tests for core logic (manifest sanitization, rule matching).
- [ ] Write integration tests for controllers and webhook using `envtest`.

## Phase 7: Build & Deployment

- [ ] Create a `Dockerfile` for building the application image.
- [ ] Create a basic Helm chart for deployment.
- [ ] Implement a GitHub Actions CI/CD pipeline for linting, testing, and building.