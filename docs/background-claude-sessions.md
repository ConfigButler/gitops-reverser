# Running Claude Code in the background of the devcontainer

How to start a long Claude Code task inside the devcontainer, close VS Code, and come back to it
later. The usual case is a prompt file under `docs/` (`*.ignore.md`, git-ignored) that takes hours
because it runs the model sweep and the e2e legs.

## Why it needs setup

Two things stop a session that VS Code started from outliving VS Code:

- **The container stops with the window.** By default VS Code stops a Dockerfile-based
  devcontainer when its last window closes, which kills every process inside it.
  [`devcontainer.json`](../.devcontainer/devcontainer.json) sets `"shutdownAction": "none"` so the
  container keeps running. Stop it yourself with `docker stop` on the host when you are done; until
  then it holds its memory (capped at 12 GB by `runArgs`).
- **A chat in the VS Code extension belongs to the extension.** Closing VS Code ends it. A session
  started with `claude --bg` runs as its own process in the container instead.

The k3d e2e clusters are sibling containers on the host daemon (docker-outside-of-docker), so they
are unaffected either way.

## One-time setup

1. **Pick up `shutdownAction`.** After the line is in `devcontainer.json`, run
   **Developer: Reload Window**. If the container still stops when you close VS Code, run
   **Dev Containers: Rebuild Container**, which is the sure way to apply it.
2. **Put the CLI on `PATH`.** The VS Code extension ships a `claude` binary but does not put it on
   `PATH`. Link the newest one:

   ```bash
   mkdir -p ~/.local/bin
   ln -sf "$(ls -d ~/.vscode-server/extensions/anthropic.claude-code-*/resources/native-binary/claude | sort -V | tail -1)" ~/.local/bin/claude
   ```

   Redo this after an extension update (the path contains the version) and after a rebuild
   (`~/.local/bin` is not on a volume).
3. **Trust the workspace once.** `claude --bg` refuses to start in an untrusted folder. Run
   `claude` in `/workspaces/gitops-reverser2`, accept the trust prompt, and `/exit`. The answer is
   stored in `~/.claude`, which is a volume, so it survives a rebuild.

## Start a session

```bash
cd /workspaces/gitops-reverser2
claude --bg --permission-mode auto -n save-asks \
  "Read docs/<task>-prompt.ignore.md and carry it out end to end, following AGENTS.md. \
The user is away: do not wait for answers. When a decision is theirs, take the conservative \
option, note it in the PR body, and carry on. If you are truly blocked, stop and write down where."
```

`-n` names the session, so the commands below take the name instead of the id. `--bg` prints the
id and returns at once.

## Check on it

| Command | Does |
|---|---|
| `claude agents` | Lists sessions and whether each is working. Add `--json` outside a terminal. |
| `claude logs save-asks` | Prints the session's recent output. |
| `claude attach save-asks` | Opens the session in this terminal, so you can answer or redirect it. |
| `claude stop save-asks` | Stops it. |

If the container stopped anyway, the work on disk is kept (the workspace is a bind mount). Reopen
the container, find the session id with `claude agents`, and continue it with
`claude --bg --resume <id>`.

## What does not work while VS Code is closed: signing and push

Both go through VS Code, so a session can edit, build, test and run e2e with VS Code closed, but it
cannot commit or push until VS Code is back:

- **Commit signing** uses the SSH agent that VS Code forwards
  (`SSH_AUTH_SOCK=/tmp/vscode-ssh-auth-<uuid>.sock`, see
  [`sync-signing-key.sh`](../.devcontainer/sync-signing-key.sh)). Every commit must be signed, so
  `git commit` fails without it. Never work around this with `--no-gpg-sign`.
- **`git push`** uses VS Code's credential helper, which `/etc/gitconfig` and `~/.gitconfig` point
  at a script that talks to the VS Code window.

Reopening VS Code restores both, but under a new socket path. A session that was already running
still has the old `SSH_AUTH_SOCK`; tell it (through `claude attach`) to use the newest socket:

```bash
export SSH_AUTH_SOCK="$(ls -t /tmp/vscode-ssh-auth-*.sock | head -1)"
```

To let a session commit and push with VS Code closed, give the container credentials of its own:

1. **A signing key file.** Git signs with a key file directly, without an agent (checked: a
   commit made with `SSH_AUTH_SOCK` unset carries a good signature). Store the key on the
   `persisted-home` volume so it survives a rebuild, and add the `.pub` file to GitHub as a
   **signing** key:

   ```bash
   mkdir -p ~/persisted-home/.ssh
   ssh-keygen -t ed25519 -N '' -C 'devcontainer signing' -f ~/persisted-home/.ssh/devcontainer-signing
   git config --global user.signingkey ~/persisted-home/.ssh/devcontainer-signing
   ```

   The trade-off: the key has no passphrase and lives in the container, so anything running in the
   container can sign as you. Remove it from GitHub if the container is ever compromised.
   `sync-signing-key.sh` keeps a `user.signingkey` you configured. `~/.gitconfig` is not on a
   volume, so set it again after a rebuild.
2. **`gh` for push.** `gh` is already logged in (`~/.config/gh` is a volume).
   `gh auth setup-git` makes Git use it for `github.com` instead of the VS Code helper.
