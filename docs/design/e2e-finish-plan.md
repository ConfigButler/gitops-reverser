# E2E Finish Plan

This is the one active plan for the remaining e2e harness work.

## Current Baseline

The harness is already in a much better place than the older plans imply:

- `BeforeSuite` owns shared cluster and install preparation
- repo-using e2e files create file-local repo fixtures through `SetupRepo(...)`
- repo bootstrap is now owned directly by the e2e Go helpers
- signing covers generated keys and BYOK
- signing is verified three ways:
  - `ssh-keygen -Y verify`
  - `git verify-commit`
  - Gitea commit verification after the `verify_ssh` flow

The older follow-up documents are now reference material, not active plans.

## Open Work

### 1. Final harness polish after the direct Go bootstrap shift

The big shell-to-Go seam is now closed. The remaining work is cleanup and
consolidation:

- trim stale docs and comments that still describe the deleted Task and shell
  repo-bootstrap path
- decide whether the current Go bootstrap helper should stay in the main
  `test/e2e` package or be extracted into a smaller e2e support package
- review whether any now-unused historical migration notes should be archived or
  deleted
- keep the persisted artifact set intentionally small unless a new stamp buys
  clear debugging value

Acceptance:

- the active docs describe the direct Go-owned repo bootstrap accurately
- no active e2e path depends on the deleted repo-bootstrap Task targets
- no new file-based stamp is introduced without a concrete consumer

## Current Direction

The preferred direction is now explicit:

- keep repo bootstrap owned in Go
- keep signing keys memory/Secret-backed
- keep transport SSH keys memory-first as well
- persist only the artifacts that are directly useful to tests and humans

In the current shape that means:

- the local checkout remains a stable artifact
- the generated `secrets.yaml` remains a stable artifact
- intermediate values such as transport keys, tokens, and webhook bookkeeping
  stay in memory unless there is a strong reason to persist them

## Why This Simplifies The Harness

- fewer stamp files with overlapping meaning
- less hidden coupling to filenames and shell pipelines
- fewer subprocesses and fewer write-file/read-file-back cycles
- clearer ownership boundaries between typed Gitea calls, Kubernetes lookups,
  and local checkout setup
- easier future parallelism because less mutable setup state is written to disk

## Explicit Non-Goals

This plan does not require:

- rebuilding every remaining shell helper immediately
- immediate `ginkgo -p` safety for the whole package
- redesigning the whole Task-based cluster bootstrap
- restoring deleted repo-bootstrap Task compatibility for manual use

## Validation

Minimum focused checks while working:

```bash
task test-e2e-signing
```

Required wrap-up:

```bash
task lint
task test
docker info
task test-e2e
task test-e2e-quickstart-manifest
task test-e2e-quickstart-helm
```
