# Go Development Directory Permissions Solution

## Summary

**Problem:** Permission errors when running `go mod tidy` in devcontainer
**Root Cause:** Go operations run as root during build, creating root-owned files in `/go`
**Solution:** Set group permissions with setgid + ACLs on `/go` **before** any Go operations
**Key Insight:** Both root and vscode users must be in shared `godev` group

## Correct Implementation Order

```dockerfile
# 1. Create shared group
RUN groupadd --gid 2000 godev

# 2. Set permissions BEFORE any Go operations
RUN chgrp -R godev /go && \
    chmod -R 2775 /go && \
    setfacl -d -m g:godev:rwx /go && \
    setfacl -d -m u::rwx /go && \
    setfacl -d -m o::rx /go

# 3. Now run Go operations - they inherit correct permissions
RUN go install <tools>...
RUN go mod download

# 4. In dev stage, add vscode user to godev group
RUN usermod -aG godev vscode
```

**Critical:** Permissions must be set **before** the first Go operation that writes to `/go`.

## Problem

When running `go mod tidy` or other Go module commands in the devcontainer, permission errors occurred:

```
mkdir /go/pkg/mod/github.com/go-openapi/swag/jsonutils: permission denied
unzip /go/pkg/mod/cache/download/github.com/golang/protobuf/@v/v1.5.4.zip: 
  mkdir /go/pkg/mod/github.com/golang/protobuf@v1.5.4: permission denied
```

### Root Cause

1. During Docker image build, `go mod download` runs as **root** user
2. This creates files/directories in `/go/pkg/mod` owned by **root:root**
3. During development, the **vscode** user tries to run `go mod tidy`
4. The vscode user lacks write permissions to root-owned directories

### Why Not Recursive Chown?

The initial approach used `chown -R vscode:vscode /go/pkg/mod/cache`, but this:
- Takes significant time (walks entire directory tree)
- Only fixes existing files, not new ones created by root
- Needs to be repeated whenever root creates new files

## Solution: Group-Based Permissions with Setgid + ACLs

Instead of changing ownership of all files, we use **group permissions** with the **setgid bit** and **POSIX ACLs** for automatic inheritance. We apply this to the entire `/go` directory to cover all Go-related files and tools.

### Implementation

#### 1. Create Shared Group (CI Stage)

```dockerfile
# Create godev group for shared Go development directory access
RUN groupadd --gid 2000 godev
```

#### 2. Set Group Permissions Before Any Go Operations (CI Stage)

**CRITICAL:** Permissions must be set **before** any `go install`, `go mod download`, or other Go operations that write to `/go`.

```dockerfile
# Create godev group first
RUN groupadd --gid 2000 godev

# Set group permissions BEFORE any Go operations
RUN chgrp -R godev /go && \
    chmod -R 2775 /go && \
    setfacl -d -m g:godev:rwx /go && \
    setfacl -d -m u::rwx /go && \
    setfacl -d -m o::rx /go

# Now all Go operations will inherit correct permissions
RUN go install sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0 \
    && go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

# ... other Go operations like go mod download
```

Key flags:
- `chgrp -R godev /go` - Recursively set group ownership to godev for entire /go directory
- `chmod -R 2775 /go` - Recursively set **setgid bit** (2) and permissions (775) on entire /go directory
- `setfacl -d -m g:godev:rwx /go` - Set default ACL for group on /go (automatically inherited by all new subdirectories)
- `setfacl -d -m u::rwx /go` - Set default ACL for owner on /go
- `setfacl -d -m o::rx /go` - Set default ACL for others on /go
- `go mod download` - Downloads modules; all new directories automatically inherit both setgid bit and default ACLs

#### 3. Add Vscode User to Group (Dev Stage)

```dockerfile
# Create vscode user and add to godev group
RUN groupadd --gid 1000 vscode \
    && useradd --uid 1000 --gid vscode --shell /bin/bash --create-home vscode \
    && usermod -aG godev vscode
```

#### 4. Dev Stage Inherits Permissions

When building the dev stage with `FROM ci AS dev`, the filesystem state from the CI stage is preserved, including:
- The `/go` directory structure
- Group ownership (godev)
- The setgid bit (2775 permissions)
- Default ACLs

**No need to re-apply ACLs in the dev stage** - they are automatically inherited from the CI stage. Any `go install` commands in the dev stage will automatically use the ACLs already set on `/go`.

```dockerfile
# Install Delve debugger for Go debugging in VSCode
# ACLs from CI stage are preserved, so no need to re-apply
RUN go install github.com/go-delve/delve/cmd/dlv@latest
```

**Important Note:** The original documentation incorrectly stated that ACLs need to be re-applied in each Docker stage. This is not true - Docker multi-stage builds preserve filesystem metadata including ACLs when using `FROM <stage> AS <newstage>`.

The `2775` permission means:
- `2` - Setgid bit (new files inherit group)
- `7` - Owner has rwx (read, write, execute)
- `7` - Group has rwx
- `5` - Others have rx (read, execute only)

**Important:** The key insight is to apply permissions to the entire `/go` directory tree **BEFORE** running any Go operations (`go install`, `go mod download`, etc.). This way, all subdirectories created automatically inherit both the setgid bit and the default ACLs.

**Common Mistake:** Setting permissions after some Go operations have already run. This leaves some files/directories with incorrect permissions. Always set permissions first, then run all Go operations.

**Why This Works:**
1. `chmod -R 2775 /go` - Recursively sets the setgid bit on all existing directories, which is then inherited by new subdirectories
2. `setfacl -d` - Sets default ACLs on `/go`, which are automatically inherited by all new subdirectories
3. When we apply both to `/go` before running `go mod download`, every subdirectory created (like `pkg/mod/`, `pkg/mod/cache/`, `bin/`, etc.) automatically gets both the setgid bit and the default ACLs
4. No `find` commands needed - everything is automatic, fast, and covers the entire Go directory including binaries, packages, and module cache!

### How Setgid + ACLs Work Together

The **setgid bit** (`g+s` or `2` in numeric mode) on a directory ensures:

1. **New files inherit the directory's group** (godev), not the creator's primary group
2. **New subdirectories inherit the directory's group** (godev)
3. **Both root and vscode can write** because both are in the godev group

**IMPORTANT:** Setgid does **NOT** automatically propagate the setgid bit to new subdirectories. This is a common misconception.

The **default ACLs** (`setfacl -d`) solve this limitation:

1. **Automatically applied to all new files and directories** created within the parent
2. **Persist across directory creation** - no manual intervention needed
3. **Define exact permissions** for user, group, and others
4. **Work alongside setgid** for complete permission inheritance

Example:
```bash
# Directory with setgid
drwxrwsr-x  2 root godev  4096 /go/pkg/mod

# File created by root
-rw-rw-r--  1 root godev  1234 /go/pkg/mod/example.go

# File created by vscode
-rw-rw-r--  1 vscode godev  5678 /go/pkg/mod/another.go
```

Notice both files have **godev** as the group, regardless of who created them.

### Benefits

✅ **Fast** - Only changes directory permissions, not individual files
✅ **Automatic** - New files/directories inherit correct permissions via ACLs
✅ **Complete** - ACLs handle what setgid alone cannot (subdirectory permissions)
✅ **Secure** - Proper group-based access control
✅ **Persistent** - Works for all future Go operations
✅ **No Maintenance** - No need to re-run chown or chmod commands

### Verification

After rebuilding the devcontainer, verify the setup:

```bash
# Check vscode user is in godev group
id vscode
# Output should include: groups=1000(vscode),2000(godev),999(docker)

# Check directory permissions
ls -ld /go
# Output should show: drwxrwsr-x ... godev ... /go
#                              ^-- setgid bit (s instead of x)

# Check ACL configuration
getfacl /go
# Output should show default ACLs:
# default:user::rwx
# default:group:godev:rwx
# default:other::r-x

# Test go mod operations
go mod tidy
# Should complete without permission errors

# Verify new directories get correct permissions
mkdir /go/test-dir
ls -ld /go/test-dir
# Should show: drwxrwsr-x ... godev ... (note the 's' and godev group)
getfacl /go/test-dir
# Should show inherited default ACLs
rm -rf /go/test-dir
```

### Alternative Approaches Considered

1. **Run go mod download as vscode user**
   - Requires restructuring Dockerfile layers
   - May impact build cache efficiency
   - Rejected in favor of group-based solution

2. **Recursive chown after download**
   - Slow (walks entire tree)
   - Doesn't handle future root operations
   - Original problematic approach

3. **Apply permissions only to /go/pkg/mod**
   - Doesn't cover /go/bin or other Go directories
   - Requires separate permission management for different directories
   - More complex than applying to entire /go directory

4. **Use GOMODCACHE environment variable**
   - Could point to vscode-owned directory
   - Breaks Go tooling conventions
   - Complicates CI/CD integration

5. **Setgid alone (without ACLs)**
   - Does not propagate setgid bit to new subdirectories
   - Requires manual `find` commands after each operation
   - Incomplete solution that led to continued permission errors

## References

- [Linux setgid documentation](https://man7.org/linux/man-pages/man2/chmod.2.html)
- [POSIX ACLs documentation](https://man7.org/linux/man-pages/man5/acl.5.html)
- [setfacl command](https://man7.org/linux/man-pages/man1/setfacl.1.html)
- [Go module cache location](https://go.dev/ref/mod#module-cache)
- [Docker multi-stage builds](https://docs.docker.com/build/building/multi-stage/)

## Why Setgid Alone Is Not Enough

A common misconception is that the setgid bit automatically propagates to newly created subdirectories. **This is false.**

### What Actually Happens with Setgid:

```bash
# Parent directory with setgid
drwxrwsr-x  root godev  /go/pkg/mod

# Create a new subdirectory
mkdir /go/pkg/mod/newdir

# Result: Group is inherited, but setgid bit is NOT
drwxrwxr-x  root godev  /go/pkg/mod/newdir
#      ^-- No 's', just regular 'x'
```

The new directory gets the `godev` group (✅) but does **not** get the setgid bit (❌). This means:
- Files created in `/go/pkg/mod/newdir` will NOT inherit the godev group
- They'll use the creator's primary group instead
- This breaks the permission model

### How ACLs Fix This:

Default ACLs (`setfacl -d`) are **automatically applied** to all new files and directories:

```bash
# Parent with default ACLs
drwxrwsr-x+ root godev  /go/pkg/mod  # Note the '+' indicating ACLs

# Create a new subdirectory
mkdir /go/pkg/mod/newdir

# Result: Inherits both group AND default ACLs
drwxrwsr-x+ root godev  /go/pkg/mod/newdir
#      ^-- Has 's' from inherited default ACL
#         ^-- Has '+' indicating it also has default ACLs

# Files created in newdir will now correctly inherit godev group
```

This is why the solution requires **both** setgid and ACLs working together.