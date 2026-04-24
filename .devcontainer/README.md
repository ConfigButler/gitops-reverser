# Development Container

Development environment for `gitops-reverser` with Go, Kubernetes, Docker CLI, and local SSH commit
signing support.

An optional repo-root `.env` can also provide read-only `gh` CLI access for coding agents and
interactive debugging inside the devcontainer.

## Quick Start

### VS Code

1. Install the [Dev Containers extension](https://marketplace.visualstudio.com/items?itemName=ms-vscode-remote.remote-containers)
2. Open the project in VS Code: `code .`
3. Press `F1` and run `Dev Containers: Reopen in Container`
4. Wait for the initial build to finish

### Before Reopening In Container

This repo expects SSH agent forwarding for commit signing.

On your host machine:

```bash
eval "$(ssh-agent -s)"
ssh-add ~/.ssh/id_ed25519
ssh-add -L
git config --global user.name "Your Name"
git config --global user.email "you@example.com"
```

If `ssh-add -L` shows no keys, commit signing inside the devcontainer will fail.

### Verify

```bash
go version
ginkgo version
kubectl version --client
golangci-lint version
task --version
bash -ic 'complete -p task >/dev/null && echo task completion ok'
docker version
gh --version
git config --get gpg.format
git config --get user.signingkey
ssh-add -L
```

Expected Git signing values:

```bash
git config --get gpg.format         # ssh
git config --get commit.gpgsign    # true
git config --get user.signingkey   # /home/vscode/.ssh/devcontainer_signing_key.pub
```

## How SSH Signing Works Here

The devcontainer uses SSH commit signing, not GPG keyring-based signing.

- [`post-create.sh`](./post-create.sh) bootstraps Git identity and calls the signing sync helper
- [`sync-signing-key.sh`](./sync-signing-key.sh) reads the forwarded SSH agent and refreshes:
  - `${HOME}/.ssh/devcontainer_signing_key.pub`
  - `${HOME}/.config/git/allowed_signers`
- [`devcontainer.json`](./devcontainer.json) runs the helper on:
  - `postCreateCommand`
  - `postStartCommand`

This keeps the configured public key file aligned with the currently forwarded agent key.

## Best Practices

## Setup goals

This setup is still evolving.

The goal is to find a development environment that:

- works across Linux, macOS, Windows, Codespaces, and remote dev machines
- keeps personal choices open where possible instead of forcing one editor or one host setup
- still gives contributors a setup that actually works for Go, Kubernetes, Docker, and Git signing

That means some choices here are pragmatic rather than ideal. The current setup favors reliability
and repeatability first, while still trying to leave room for different host platforms and personal
workflows.

### SSH Signing

1. Treat forwarded SSH agent keys as ephemeral
   Key order and loaded keys can change between sessions.

2. Refresh signing config on create and on start
   `postCreate` is not enough when the host agent changes later.

3. Keep signing setup in one small script
   This repo uses [`sync-signing-key.sh`](./sync-signing-key.sh) for that reason.

4. Prefer deterministic key selection
   The helper first looks for an SSH key whose comment matches `git user.email`.
   If none matches, it falls back to the first key from `ssh-add -L`.

5. Keep signing and verification data in sync
   `user.signingkey` and `gpg.ssh.allowedSignersFile` are regenerated together.

6. Store only public key material in the container
   The private key stays on the host and is accessed through the forwarded SSH agent.

7. Fail early with actionable messages
   Missing agent, missing keys, or missing Git identity should stop setup immediately.

### General Devcontainer Maintenance

1. Keep lifecycle hooks small and explicit
   Bootstrap in `post-create.sh`, refresh in `sync-signing-key.sh`.

2. Prefer reusable helpers over duplicated shell snippets
   That makes drift easier to spot and fixes easier to apply.

3. Make runtime state easy to inspect
   Useful checks:

```bash
echo "$SSH_AUTH_SOCK"
ssh-add -L
cat ~/.ssh/devcontainer_signing_key.pub
cat ~/.config/git/allowed_signers
git config --show-origin --get user.signingkey
```

## Optional `.env` for `gh`

A repo-root `.env` file is optional. If present, login shells inside the devcontainer automatically
export its variables from `${PROJECT_PATH}/.env` via `/etc/profile.d/workspace-dotenv.sh`.

This is intended for read-only GitHub access from inside the container, for example:

```bash
echo 'GH_TOKEN=<fine-grained-read-only-token>' > .env
gh auth status
gh run list --limit 5
gh pr view
```

Recommended token scopes:

- repository contents: read
- metadata: read
- pull requests: read
- actions: read

The repo-root `.env` must stay local. It is already gitignored.

## Troubleshooting

### Commit Signing Fails

Check:

```bash
ssh-add -L
cat ~/.ssh/devcontainer_signing_key.pub
git config --get user.signingkey
```

Then refresh the signing setup manually:

```bash
bash .devcontainer/sync-signing-key.sh
```

Common causes:

- no host `ssh-agent` running
- no key loaded into the host agent
- the forwarded agent key changed after the container was created
- `user.name` or `user.email` is missing

Typical signing error when the configured public key is stale:

```text
error: No private key found for public key "/home/vscode/.ssh/devcontainer_signing_key.pub"?
```

### Push Fails But Commit Signing Works

Commit signing and Git push authentication are separate concerns.

This repo currently uses an HTTPS remote, so push failures are often caused by:

- missing upstream branch
- non-fast-forward push rejection
- GitHub HTTPS credential issues

Check:

```bash
git remote -v
git branch -vv
git push --dry-run -u origin HEAD
```

### Container Won't Build

Ensure Docker is running on the host.

### Slow Rebuild

Usually normal. Rebuild time mostly depends on tool installation and cache reuse.

## Files

- [`Dockerfile`](./Dockerfile) - Multi-stage container image
- [`devcontainer.json`](./devcontainer.json) - VS Code devcontainer configuration
- [`post-create.sh`](./post-create.sh) - Initial bootstrap
- [`sync-signing-key.sh`](./sync-signing-key.sh) - SSH signing refresh helper
- [`README.md`](./README.md) - This document

## References

- Why we use Docker-outside-of-Docker:
  [`../docs/ci/dood-vs-dind-reasons.md`](../docs/ci/dood-vs-dind-reasons.md)
- VS Code Dev Containers: adding a non-root user:
  [code.visualstudio.com/remote/advancedcontainers/add-nonroot-user](https://code.visualstudio.com/remote/advancedcontainers/add-nonroot-user)
- Trail of Bits devcontainer setup notes:
  [github.com/trailofbits/skills/.../devcontainer-setup](https://github.com/trailofbits/skills/tree/main/plugins/devcontainer-setup/skills/devcontainer-setup)
- Devcontainer best practices reference:
  [github.com/afonsograca/devcontainers-best-practices/.../vscode-containers.md](https://github.com/afonsograca/devcontainers-best-practices/blob/HEAD/skills/devcontainers-best-practices/references/vscode-containers.md)
