# Task Migration Plan

## Summary

- Replace the current `Makefile` with Taskfiles and update all repo callers in the same migration so `make` is no longer required.
- Use the newly initialized [`Taskfile.yml`](/workspaces/gitops-reverser/Taskfile.yml:1) as the starting point, but expand it into a real orchestration layer instead of a single-file monolith.
- Keep the current `.stamps` tree as-is; Task's own `.task` directory is gitignored and left entirely to Task.
- Preserve the current stamp behavior, including stable mtimes and content-bearing readiness files, because that behavior is important to the e2e flow and image refresh logic.
- Use Task for orchestration, but keep explicit file-based state for cluster/e2e/runtime steps rather than trying to express everything through Task's internal checksum cache.

## Why The Makefile Is Complex Today

The current `Makefile` is doing three different jobs at once:

1. Local artifact generation and developer commands
   Examples: `manifests`, `generate`, `helm-sync`, `fmt`, `vet`, `lint`, `build`, `run`.
2. Stateful e2e orchestration
   Examples: cluster setup, Flux install, CRD apply, image load, controller deploy, webhook TLS injection, SOPS setup, Gitea bootstrap.
3. Incremental caching and invalidation
   This is implemented through explicit stamp files under `.stamps`, content-aware stamp contents, compare-and-replace logic, and Make dependency ordering.

That third part is the important one to preserve. The repo is not using stamps as a cosmetic optimization. They are part of the execution model.

## State Layout

The `.stamps` tree is kept exactly as-is. The Task project has no official guidance on where to put user-managed state files — `TASK_TEMP_DIR` only covers Task's own internal checksum cache. Migrating `.stamps` would be pure churn with no functional benefit and would require updating every Go helper, shell script, and `.gitignore` entry.

Task's default `.task` directory is left entirely to Task. It is gitignored and not referenced anywhere in the repo. No `TASK_TEMP_DIR` override is needed.

## Public Command Shape

Keep command names close to the current Make targets so the cutover is easy:

**Build / local**
- `task manifests`
- `task generate`
- `task helm-sync`
- `task fmt`
- `task vet`
- `task lint`
- `task lint-fix`
- `task lint-config`
- `task build`
- `task run`
- `task test`
- `task setup-envtest`
- `task docker-build`
- `task docker-push`
- `task docker-buildx`
- `task dist-install` (replaces the file-target `make dist/install.yaml`)
- `task clean`

**E2E cluster and install**
- `task prepare-e2e`
- `task prepare-e2e-demo`
- `task e2e-gitea-bootstrap`
- `task e2e-gitea-run-setup`
- `task portforward-ensure`
- `task install`
- `task install-helm`
- `task install-plain-manifests-file`
- `task install-config-dir`

**E2E test suites**
- `task test-e2e`
- `task test-e2e-quickstart-manifest`
- `task test-e2e-quickstart-helm`
- `task test-e2e-bi`
- `task test-e2e-audit-redis`
- `task test-e2e-demo`
- `task test-image-refresh`

**Demo / loadtest**
- `task loadtest`

**Cleanup**
- `task clean-cluster`
- `task clean-installs`
- `task clean-port-forwards`

Keep the current env contract unchanged:

- `CTX`
- `INSTALL_MODE`
- `INSTALL_NAME`
- `NAMESPACE`
- `PROJECT_IMAGE`
- `IMAGE_DELIVERY_MODE`
- `REPO_NAME`
- current port, demo, image pull, and e2e helper vars

`test-e2e-quickstart-helm` and `test-e2e-quickstart-manifest` bake their `INSTALL_MODE` overrides directly as `vars:` inside the task definition rather than relying on the caller to pass them. The env contract for `NAMESPACE`, `CTX`, and `E2E_AGE_KEY_FILE` is still inherited from the root vars.

## Taskfile Structure

Create:

- `Taskfile.yml`
- `taskfiles/build.yml`
- `test/e2e/Taskfile.yml`

### Root `Taskfile.yml`

Responsibilities:

- shared vars/defaults
- includes for sub-Taskfiles with no `namespace:` set, so all tasks are flattened into the root namespace — `task test-e2e`, not `task e2e:test-e2e`
- aliases/help
- `dotenv` or env wiring if later needed

### `taskfiles/build.yml`

All build/local artifact tasks. Task names match the Make targets directly; the only non-obvious one is `dist-install` (was the file-target `dist/install.yaml`).

### `test/e2e/Taskfile.yml`

Lives next to the e2e code it orchestrates. Included from the root with no namespace.

Owns everything in the **E2E cluster and install**, **E2E test suites**, **Demo / loadtest**, and **Cleanup** groups from the public command list above. Internal stamp-driving tasks (e.g. `flux.installed`, `image.loaded`) are defined here too but are not listed in `tasks:` `desc:` fields so they stay out of `task --list`.

## How To Translate The Current Make Behavior

### 1. Pure local artifact tasks

Use Task `sources` and `generates` for:

- manifests generation
- helm sync copies
- deepcopy generation
- consolidated install manifest generation
- envtest setup readiness file

These are the easiest tasks to convert because they are file-driven and mostly local.

### 2. Stateful runtime/e2e tasks

Do not rely on Task's internal checksum cache for cluster/runtime steps.

Instead:

- keep explicit readiness files under `.stamps/...` as today
- gate task execution with `status` checks against those files
- continue to use content-bearing files where the file contents are part of the contract

This applies to:

- cluster readiness
- services readiness
- image loaded state
- controller deployed state
- install outputs
- webhook TLS readiness
- SOPS readiness
- Gitea bootstrap/setup outputs

### 3. Ordered execution

Task runs `deps` in parallel by default. That does not match the current e2e orchestration model.

So for ordered workflows like `prepare-e2e`:

- use serial nested task calls inside `cmds`
- do not model dependent steps with parallel `deps`

This is important for:

- `prepare-e2e`
- install mode flows
- demo prep
- full validation sequences

### 4. Always-run tasks

A task with no `sources`, `generates`, or `status` fields always runs when invoked — Task has no basis to skip it. Use this for tasks that must check live state every time, regardless of stamp freshness. `portforward-ensure` is the primary example: it must always run to verify port-forwards are healthy, even when no stamps changed.

## Stamp Behavior To Preserve

These behaviors should remain exactly true after the migration.

### No-op fast path

A second `prepare-e2e` run with no relevant changes should skip expensive rebuild/reapply work and only hit the always-run port-forward health fast path.

### Content-aware image chain

The current image chain is good and should stay conceptually identical:

```text
Go/Docker inputs
  -> controller.id
  -> project-image.ready
  -> image.loaded
  -> controller.deployed
```

Requirements:

- Go or Dockerfile changes must change the local image identity.
- Changed image identity must update loaded-image state.
- Changed loaded-image state must trigger controller rollout/deploy refresh.
- No-op runs must not restart the controller.

### Stable mtimes on no-op writes

Keep compare-and-replace behavior so unchanged outputs do not get rewritten just because the task ran.

Important places:

- install manifests
- SOPS secret manifest
- any rendered artifact whose mtime drives downstream work

### Explicit generated outputs

If a generating tool may omit one declared output, the task must still ensure the declared file exists after the task completes.

This matters for the current webhook-manifest case and any future grouped-output conversions.

### Runtime updates must not poison upstream readiness

Files rewritten during later runtime steps must not accidentally invalidate earlier bootstrap tasks unless that is intentionally part of the dependency model.

## Repo Callers That Must Move Off `make`

Update these to `task` in the same migration:

- `.github/workflows/ci.yml`
- `README.md`
- `CONTRIBUTING.md`
- e2e Go helpers that shell out to `make`

Also remove:

- `checkmake` from CI
- `checkmake` from devcontainer validation/tool install

### Tiltfile

The current `Tiltfile` appears stale relative to the repo's current k3d-based e2e flow and points at older Make-driven commands. The cleanest move is to remove it rather than port it as part of this migration.

## Suggested Migration Sequence

1. Expand `Taskfile.yml` from the fresh `task --init` scaffold into the root orchestrator.
2. Add `taskfiles/build.yml` and move pure local artifact tasks first.
3. Add `test/e2e/Taskfile.yml` and port the e2e/stateful orchestration tasks while preserving current helper scripts and the `.stamps` layout unchanged.
4. Update Go e2e helpers to invoke `task` instead of `make`.
5. Update CI, docs, and contributor instructions.
6. Remove `Makefile`.
7. Remove `checkmake` references.
8. Run the full validation sequence.

## Validation Plan

### Smoke checks

- `task --list-all`
- `task manifests`
- `task generate`
- `task helm-sync`
- `task dist-install`

### Mandatory validation sequence

Run sequentially:

1. `task lint`
2. `task test`
3. `docker info`
4. `task test-e2e`
5. `task test-e2e-quickstart-manifest`
6. `task test-e2e-quickstart-helm`
7. `task test-image-refresh` — directly validates the stamp/invalidation chain; most sensitive to the Make→Task translation
8. `task test-e2e-bi` — covers the bi-directional Flux scenario not exercised by the main suite

### Stamp parity checks

- run `task prepare-e2e` twice with no changes
- confirm `.stamps/cluster/<ctx>/image.loaded` is unchanged on the second run
- confirm `.stamps/cluster/<ctx>/<namespace>/controller.deployed` is unchanged on the second run
- confirm no-op runs do not restart the controller
- confirm Go source changes still update the image/deploy state and trigger rollout

### Caller parity checks

- no remaining repo file shells out to `make`
- CI invokes `task` only
- contributor-facing docs reference `task` only

## Assumptions

- The newly created `Taskfile.yml` is only a starter scaffold and can be replaced freely.
- Existing bash scripts remain the implementation boundary for detailed cluster/install logic; this migration changes orchestration wiring, not script internals.
- State layout is decided in the **State Layout** section above; no further changes to `.stamps` or `.task` handling.

## References

- `Makefile`
- `docs/make-caching-analysis.md`
- `docs/design/e2e-test-design.md`
- `docs/design/e2e-image-refresh-makefile-analysis.md`
- `docs/design/e2e-runtime-state-vs-stamps.md`
- Task guide: <https://taskfile.dev/docs/guide>
- Task env reference: <https://taskfile.dev/docs/reference/environment>
