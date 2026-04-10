# Git safe.directory in CI

Git 2.35.2 (CVE-2022-24765) refuses to operate in repos where `.git` is owned by a different
user. In GitHub Actions, `actions/checkout` runs on the host as `runner` (UID 1001), but our
CI container runs as root — so every git command fails with "dubious ownership".

Fix: add one step before any git/make/lint work:

```yaml
- name: Configure Git safe directory
  run: git config --global --add safe.directory ${{ github.workspace }}
```

This is safe because CI containers are ephemeral and isolated. Don't use `safe.directory = *`
(disables the check globally).

The same issue does not usually affect the devcontainer — the `vscode` user and the mounted
workspace typically match. It's a CI-specific quirk.
