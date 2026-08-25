# Development Container

Development environment for `gitops-reverser` with Go, Kubernetes, Docker CLI, and local SSH commit
signing support.

An optional repo-root `.env` can also provide read-only `gh` CLI access for coding agents and
interactive debugging inside the devcontainer.

## Quick Start

### Prerequisites (Before Reopening In Container)

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

If the environment creating the devcontainer has no Git config to hand over, set `GIT_USER_NAME` and
`GIT_USER_EMAIL` in that environment instead; [`devcontainer.json`](./devcontainer.json) forwards
both through `${localEnv:...}` and `post-create.sh` writes them into the container's global Git
config. Configured Git config always wins over these variables.

### VS Code

1. Install the [Dev Containers extension](https://marketplace.visualstudio.com/items?itemName=ms-vscode-remote.remote-containers)
2. Open the project in VS Code: `code .`
3. Press `F1` and run `Dev Containers: Reopen in Container`
4. Wait for the initial build to finish

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
ssh-add -L
```

Expected Git signing values:

```bash
git config --get gpg.format                # ssh
git config --get commit.gpgsign            # true
git config --get gpg.ssh.defaultKeyCommand # ssh-add -L
git config --get gpg.ssh.allowedSignersFile # /home/vscode/.config/git/allowed_signers
```

`user.signingkey` is set only when an agent key comment matches your Git email; otherwise it stays
unset and the key is read from the agent at commit time.

## How SSH Signing Works Here

The devcontainer uses SSH commit signing, not GPG keyring-based signing.

Most of it is plain Git configuration rather than shell logic:

```bash
commit.gpgsign=true                    # every commit is signed
gpg.format=ssh                         # signed with an SSH key
gpg.ssh.defaultKeyCommand="ssh-add -L" # the key comes from the live agent
```

With those three set, Git signs with whatever the agent currently holds, and refuses to commit
when it holds nothing. No key is cached anywhere, so there is nothing to go stale when a
reconnect changes the forwarded socket or the loaded key.

[`sync-signing-key.sh`](./sync-signing-key.sh) applies that baseline and adds the one thing Git
cannot derive on its own: `${HOME}/.config/git/allowed_signers`, which maps the Git email to the
keys currently in the agent so `git log --show-signature` can verify. It runs from
[`post-start.sh`](./post-start.sh) on every start, and can be run by hand at any time.

If one of the agent keys has a comment containing your Git email, that key is pinned as
`user.signingkey` so selection does not depend on agent order. With no such key the pin is left
unset, which is both simpler and immune to going stale. A signing key you configured yourself is
never overwritten.

[`post-create.sh`](./post-create.sh) does not configure signing. Creating the environment,
personalizing Git, and having credentials available are three separate things, and only the first
has to succeed for the container to be usable.

### When No Agent Has Attached Yet

A devcontainer can be created before any interactive session attaches, for example by
`devcontainer up` from the Dev Containers CLI, or by a platform that personalizes the workspace
afterwards. No SSH agent is forwarded at that point, and no Git identity may exist yet. Neither is
treated as a failure to create the environment: the hooks report what is missing and finish.

Signing is not relaxed to achieve that. `commit.gpgsign` is on from the start, so a commit without
a usable key fails:

```text
fatal: either user.signingkey or gpg.ssh.defaultKeyCommand needs to be configured
```

Nothing else in the environment needs a key, so builds, tests and the E2E suite run regardless.

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

4. Prefer standard Git mechanisms over shell logic
   `gpg.ssh.defaultKeyCommand` selects the key whenever no comment matches the Git email, so there
   is usually no pinned key to become stale.

5. Trust every key the agent currently holds
   `allowed_signers` lists them all under the Git email, so verification works whichever one the
   key command picks.

6. Store only public key material in the container
   The private key stays on the host and is accessed through the forwarded SSH agent.

7. Report, do not fail
   A missing identity or agent is diagnosed and setup continues. Git, not a shell script, is what
   refuses an unsigned commit.

8. Use the Git email as the `allowed_signers` principal
   That file takes bare identities, so a `Name <email>` principal makes `ssh-keygen -Y verify`
   report `invalid key` and `No principal matched` for a correctly signed commit.

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
cat ~/.config/git/allowed_signers
git config --show-origin --get-regexp '^(commit\.gpgsign|gpg\.)'
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

Alternatively just run `gh auth login` once. `~/.config/gh` is a named volume (`ghconfig`), so the
login survives container rebuilds; the first rebuild after this mount was added starts from an
empty volume and needs one more login.

## Troubleshooting

### Commit Signing Fails

Check:

```bash
ssh-add -L
git config --get commit.gpgsign
git config --get gpg.ssh.defaultKeyCommand
```

Then refresh the signing setup manually:

```bash
bash .devcontainer/sync-signing-key.sh
```

Common causes:

- no host `ssh-agent` running
- no key loaded into the host agent
- `user.name` or `user.email` is missing

Typical error when no key is reachable:

```text
fatal: either user.signingkey or gpg.ssh.defaultKeyCommand needs to be configured
```

### Commits Sign But Do Not Verify

```text
allowed_signers:1: invalid key
No principal matched.
```

The principal in `~/.config/git/allowed_signers` is malformed. Each line is
`<email> <keytype> <key> [comment]`; re-run `bash .devcontainer/sync-signing-key.sh` to regenerate
it. Verify with `git log -1 --show-signature`, and check the helper itself with
`bash .devcontainer/test-signing.sh`.

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
- [`post-start.sh`](./post-start.sh) - Per-start refresh
- [`sync-signing-key.sh`](./sync-signing-key.sh) - SSH signing refresh helper
- [`test-signing.sh`](./test-signing.sh) - Tests for the signing helper, using a disposable key
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
