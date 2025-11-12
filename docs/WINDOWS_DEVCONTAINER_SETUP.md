# Windows DevContainer Setup Guide

## Problem

On Windows, the devcontainer works differently than on Linux due to how Docker Desktop handles volume mounts:

1. **Container filesystem (`/go`)**: Full Linux filesystem with ACL support ✅
2. **Mounted workspace (`/workspace`)**: Windows filesystem mounted via Docker, limited Unix permission support ❌

## Symptoms

- Cannot write files in `/workspace` directory
- Permission denied errors when running `go mod tidy` or other commands
- ACL commands fail on mounted volumes

## Root Cause

Windows uses NTFS/ReFS filesystems which don't support Linux ACLs. When Docker Desktop mounts a Windows directory into a Linux container, it uses a compatibility layer that:
- Simulates Unix permissions
- Cannot support `setfacl` or advanced ACLs
- May have permission mapping issues between Windows and Linux users

## Solution

The solution is to ensure the workspace directory has proper ownership and permissions for the `vscode` user, without relying on ACLs.

### Updated Dockerfile Approach

The Dockerfile already handles this correctly:

```dockerfile
# In dev stage - ensure vscode user can write to workspace
RUN chown -R vscode:vscode /workspace && \
    chmod -R 755 /workspace
```

However, this only affects the **empty** `/workspace` directory in the image. When you mount your actual Windows workspace, it **overrides** this with the Windows filesystem.

### Windows-Specific Configuration

For Windows users, add this to your `.devcontainer/devcontainer.json`:

```json
{
  "remoteUser": "vscode",
  "containerEnv": {
    "WORKSPACE_OWNER": "vscode"
  },
  "postCreateCommand": "sudo chown -R vscode:vscode /workspace || true",
  "mounts": [
    "source=/var/run/docker.sock,target=/var/run/docker.sock,type=bind"
  ]
}
```

The `postCreateCommand` runs **after** the workspace is mounted, ensuring proper ownership.

### Alternative: Use WSL2 Backend

For the best experience on Windows, use WSL2:

1. **Install WSL2** with Ubuntu or Debian
2. **Clone the repository inside WSL2** (not in Windows filesystem)
3. **Open in VSCode** using the WSL extension
4. **Use the devcontainer** - it will work exactly like on Linux

This approach:
- ✅ Full Linux filesystem support
- ✅ ACLs work properly
- ✅ Better performance
- ✅ No permission mapping issues

### Quick Fix for Existing Setup

If you're already in the devcontainer and experiencing permission issues:

```bash
# Run this inside the devcontainer
sudo chown -R vscode:vscode /workspace
sudo chmod -R 755 /workspace

# Verify
ls -la /workspace
# Should show vscode:vscode ownership
```

## Why `/go` Works But `/workspace` Doesn't

| Directory | Location | ACL Support | Why |
|-----------|----------|-------------|-----|
| `/go` | Container filesystem | ✅ Yes | Part of the Linux container image |
| `/workspace` | Mounted from Windows | ❌ No | Windows filesystem mounted via Docker |

The `/go` directory (where Go modules are cached) uses the container's Linux filesystem, so ACLs work perfectly. The `/workspace` directory is mounted from your Windows filesystem, so it doesn't support Linux ACLs.

## Recommended Setup for Windows Users

### Option 1: WSL2 (Recommended)

```bash
# In WSL2 terminal
cd ~
git clone <your-repo-url>
cd gitops-reverser
code .  # Opens in VSCode with WSL extension
# Then reopen in container
```

### Option 2: Windows with Post-Create Fix

Update `.devcontainer/devcontainer.json`:

```json
{
  "postCreateCommand": "sudo chown -R vscode:vscode /workspace && sudo chmod -R 755 /workspace || true"
}
```

### Option 3: Run as Root (Not Recommended)

Change `remoteUser` to `root` in `devcontainer.json`, but this is not recommended for security reasons.

## Verification

After setup, verify permissions:

```bash
# Check workspace ownership
ls -la /workspace
# Should show: drwxr-xr-x vscode vscode

# Check you can write files
touch /workspace/test.txt
# Should succeed without errors

# Check Go operations work
go mod tidy
# Should complete without permission errors

# Clean up test file
rm /workspace/test.txt
```

## References

- [Docker Desktop WSL2 Backend](https://docs.docker.com/desktop/wsl/)
- [VSCode Remote - WSL](https://code.visualstudio.com/docs/remote/wsl)
- [Docker Volume Permissions](https://docs.docker.com/storage/bind-mounts/#configure-bind-propagation)