# GitOps Reverser Implementation Plan

This document outlines the tasks required to build the GitOps Reverser tool as specified in the PRD.

## Phase 1: Project Scaffolding & Core Types

- [x] Initialize Go module and project structure.
- [x] Define API types for `GitRepoConfig` CRD (`api/v1alpha1/gitrepoconfig_types.go`).
- [x] Define API types for `WatchRule` CRD (`api/v1alpha1/watchrule_types.go`).
- [x] Generate CRD manifests (`config/crd/bases/`).
- [x] Create a `CONTRIBUTING.md` guide.

## Phase 2: Controller Implementation

- [x] Implement `GitRepoConfig` controller (`internal/controller/gitrepoconfig_controller.go`).
- [x] Implement `WatchRule` controller (`internal/controller/watchrule_controller.go`).
- [x] Implement in-memory rule model for efficient webhook lookups (`internal/rulestore/store.go`).
- [x] Implement leader election for High Availability (`internal/leader/leader.go`).

## Phase 3: Webhook Implementation

- [x] Implement the `ValidatingAdmissionWebhook` handler (`internal/webhook/event_handler.go`).
- [x] Add logic to filter incoming requests based on the in-memory rule model.
- [x] Implement manifest sanitization logic (`internal/sanitize/sanitize.go`).
- [x] Implement the in-memory queue for decoupling webhook and Git operations (`internal/eventqueue/queue.go`).

## Phase 4: Git Operations & Commit Worker

- [x] Implement the asynchronous Git worker (`internal/git/worker.go`).
- [x] Implement Git logic for cloning, committing, and pushing (`internal/git/git.go`).
- [x] Implement batching logic based on `push.interval` and `push.maxCommits`.
- [x] Implement structured commit messages.
- [x] Implement logic for handling Git credentials via secrets.

## Phase 5: Observability & Metrics

- [x] Set up OpenTelemetry (OTLP) exporter (`internal/metrics/exporter.go`).
- [x] Instrument code to expose key metrics (`events_received`, `events_processed`, `git_operations`, etc.).

## Phase 6: Testing

- [x] Write unit tests for core logic (manifest sanitization, rule matching).
- [x] Write integration tests for controllers and webhook using `envtest` (`test/e2e/`).

## Phase 7: Build & Deployment

- [x] Create a `Dockerfile` for building the application image.
- [x] Create a basic Helm chart for deployment (`charts/gitops-reverser/`).
- [x] Implement a GitHub Actions CI/CD pipeline for linting, testing, and building.

[ ] Combine edits of the same person in the same minute (make that configurable): it doesnt make sense to have lot's of commits for one action. This is a hard one to get right, when does this stop? After x actions or x seconds of inactivity. Or if two persons change something in the same resource, that shouls also be immediatly be comitted. Can you check that effeciently on every incomming event?
[ ] Do we really need to pull before each commit? That's not what was in my head before we started the whole conversation -> it should do a push/pull once a minute. Or perhaps a pull the first time an event is created? I would like to have a timeline, please let's be carefull with pushes and pulls
[ ] See if we can get more out of: https://github.com/RichardoC/kube-audit-rest?tab=readme-ov-file#known-limitations-and-warnings (since it's maintained and gives some exampels on how to maintain such an open tool).
[ ] Should we also do a full reconicile on the folders? As in: check if all the yaml files are still usefull?
    -> This last line is where it gets interesting: who wins? I guess we just push a new commit and throw away the files that don't exist in the cluster. Should we do a full reconcile every x minutes? How many resources can we handle before it gets tricky?
[ ] Should the repo config be namespaced or clustered? All that duplication is also ugly, how does flux do that part?
