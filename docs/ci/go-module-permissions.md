# Go module permissions in the devcontainer

## The problem

`go mod download` and `go install` run as root during the Docker image build, creating
root-owned files in `/go`. When the `vscode` user then runs `go mod tidy`, it hits
permission errors on those directories.

## The solution: setgid + ACLs, applied before any Go operations

```dockerfile
# 1. Create shared group
RUN groupadd --gid 2000 godev

# 2. Set permissions BEFORE any go install / go mod download
RUN chgrp -R godev /go && \
    chmod -R 2775 /go && \
    setfacl -d -m g:godev:rwx /go && \
    setfacl -d -m u::rwx /go && \
    setfacl -d -m o::rx /go

# 3. Now run Go operations — new dirs inherit the ACLs automatically
RUN go install ... && go mod download

# 4. In the dev stage, add vscode to godev
RUN usermod -aG godev vscode
```

**Order matters.** If any Go operation runs before step 2, those directories get
root-only permissions and the ACL inheritance doesn't apply retroactively.

## Why both setgid and ACLs

Setgid alone makes new files inherit the `godev` group, but does **not** propagate the
setgid bit to new subdirectories. Default ACLs (`setfacl -d`) fill that gap — they are
automatically inherited by every new file and directory created under `/go`.

## Docker multi-stage note

ACLs are preserved when you do `FROM ci AS dev`. No need to re-apply them in the dev stage.
