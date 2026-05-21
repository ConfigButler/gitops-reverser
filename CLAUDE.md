# Claude Code instructions

The contribution, code-quality, and validation rules for this repository live in
**[AGENTS.md](./AGENTS.md)**.

**Read [AGENTS.md](./AGENTS.md) at the start of every task and follow it.** It is
the source of truth — if it ever conflicts with this file, AGENTS.md wins.

Key points it enforces:

- **Mandatory validation before a change is complete:** `task lint`, `task test`,
  and `task test-e2e` must all pass. Run the e2e commands **sequentially, not in
  parallel**.
- **e2e tests need Docker** — verify with `docker info` before running
  `task test-e2e`; ask the user to start the Docker daemon if it is not running.
- **Docs-only exception:** a pure markdown/docs change that touches no Go code,
  manifests, charts, Taskfiles, CI, or scripts may skip the full suite — see
  AGENTS.md for the exact wording.
- Validation sequence: `task fmt` → `task generate` → `task manifests` →
  `task vet` → `task lint` → `task test` → `task test-e2e`.
