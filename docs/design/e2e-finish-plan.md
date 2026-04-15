# E2E Finish Plan

This is the one active plan for the remaining e2e harness work.

## Current Baseline

The harness is already in a much better place than the older plans imply:

- `BeforeSuite` owns shared cluster and install preparation
- repo-using e2e files create file-local repo fixtures through `SetupRepo(...)`
- signing covers generated keys and BYOK
- signing is verified three ways:
  - `ssh-keygen -Y verify`
  - `git verify-commit`
  - Gitea commit verification after the `verify_ssh` flow

The older follow-up documents are now reference material, not active plans.

## Open Work

### 1. Move the remaining Gitea API setup out of shell

The main remaining harness debt is in
[hack/e2e/gitea-run-setup.sh](/workspaces/gitops-reverser/hack/e2e/gitea-run-setup.sh).

The first pass should move these operations to Go:

- `create_token`
- `configure_ssh_key_in_gitea`
- `ensure_repo`
- `ensure_repo_webhook`

Recommended shape:

- add a reusable Go helper under `test/e2e/gitea/` or a similarly small
  test-support package
- keep `task e2e-gitea-run-setup` usable through a thin Go CLI wrapper
- leave these shell-native pieces in bash for now:
  - `known_hosts` generation
  - `secrets.yaml` rendering
  - local checkout/bootstrap plumbing

Acceptance:

- repo setup no longer uses `curl`/`jq` for the Gitea HTTP surface
- the Task entry point still works for manual debugging
- e2e behavior stays unchanged

### 2. Final harness polish after the shell-to-Go seam

Only after the shell-to-Go seam is closed:

- remove any dead shell paths or duplicate helpers left behind by the migration
- trim stale comments and docs that still describe pre-refactor behavior
- decide whether a higher-level reusable Gitea bootstrap helper is still worth
  adding, or whether the lower-level pieces are already clean enough

## Ideal End-State: Mostly In-Memory Repo Bootstrap

The narrow migration above is the safest next step, but the cleaner long-term
shape is bolder: keep far less repo bootstrap state on disk.

### Why This Is Feasible

The signing refactor already proved the main point: once the SSH and Gitea HTTP
logic lives in Go, we are no longer forced to bounce through temp files and
subprocesses just because OpenSSH traditionally does.

The same idea can be extended to the transport setup:

- create the SSH transport keypair in Go
- register the public key in Gitea through typed helpers
- discover or construct `known_hosts` content in Go
- build the Secret manifests in Go instead of through shell pipes
- keep only the artifacts that are genuinely useful for humans and tests

### What Would Still Need To Exist

Even in the ideal shape, not everything disappears from disk.

Some artifacts are still valuable as explicit run outputs:

- the local checkout path
- the generated `secrets.yaml`, if we still want an inspectable apply artifact
- stamp files that record the active repo and readiness markers

The bigger question is the SSH material itself:

- `id_rsa`
- `id_rsa.pub`
- `known_hosts`
- `token.txt`

Those do not need to remain first-class stamp artifacts if the setup path is
owned in Go.

### Preferred Direction

The preferred direction is:

- keep signing keys memory/Secret-backed
- move transport SSH keys to memory-first handling as well
- treat `known_hosts` as generated content, not as a manually managed artifact
- write secrets and stamps as the stable outputs, not the raw intermediate
  files that happened to produce them

In that shape, the repo bootstrap helper would return structured values such as:

- token
- transport private key bytes
- transport public key bytes
- known-hosts content
- generated Secret YAML
- checkout path

Then the code can decide what must be persisted, instead of the shell script
forcing every intermediate artifact onto disk.

### Why This Simplifies The Harness

- fewer stamp files with overlapping meaning
- less hidden coupling to filenames like `id_rsa` and `known_hosts`
- fewer shell subprocesses and fewer "write file, read file back" cycles
- easier future test parallelism because mutable credentials can stay scoped to
  one setup call
- clearer ownership boundaries between transport auth, signing, and test
  artifacts

### Caution

This is a good target, but it should be reached in stages.

The risky part is not the crypto; it is preserving all the small operational
behaviors the current shell flow provides:

- stamp compatibility
- inspectability during debugging
- `task e2e-gitea-run-setup` usability outside the test binary
- compatibility with the current Secret contract and checkout flow

So the plan should remain:

1. move the Gitea HTTP surface to Go
2. keep the current outputs stable
3. then decide which intermediate SSH files can disappear without harming
   debuggability

## Explicit Non-Goals

This plan does not require:

- a full bash-to-Go rewrite of every setup step
- immediate `ginkgo -p` safety for the whole package
- redesigning the whole Task-based cluster bootstrap

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
