# GitTarget parent branch: an empty repository's default branch

> **Plan**. The experiment in §2 is done; nothing else is implemented. Dated 2026-10-01.
>
> Siblings:
>
> - [`gittarget-parent-branch.md`](gittarget-parent-branch.md), the original plan;
> - [`gittarget-parent-hardening.md`](gittarget-parent-hardening.md), the hardening pass on #407.
>
> Run it **after** the hardening pass. Its §4.2 and §4.3 define "empty" and `Unborn`, and this
> plan refines that `Unborn` case.

## 1. The problem

Take an empty repository whose default branch is `main`, but which has no commits yet. Add a
`GitTarget` with `branch: reverser/edits` and no `parentBranch`.

Today the first write creates **`reverser/edits` as the repository's first branch**:

- The commit has no parent, and no `main` exists afterwards.
- What we pushed does not match the branch the repository says is its default.
- On GitHub, the first branch pushed to an empty repository probably becomes the default branch
  (verify this, §5 step 0). If so, the operator's review branch silently becomes the repository's
  default branch. Even if not, the eventual PR `reverser/edits` → `main` cannot be made: the two
  branches have no common history.

This is not a status detail. It is the wrong branch name on the first commit we make.

`branch: main`, which is what the quickstart uses, is unaffected: the first commit creates `main`,
which is correct.

## 2. What we know (experiment, 2026-10-01)

**The server already tells us the default branch.** Over protocol v2, `ls-refs` with the `unborn`
argument reports the target of an unborn HEAD:

```text
git -c protocol.version=2 ls-remote --symref https://github.com/ConfigButler/empty.git
  (GIT_TRACE_PACKET) ls-remote< unborn HEAD symref-target:refs/heads/main
```

- Canonical git 2.39 does the same: a bare repository made with `git init --bare -b trunk`
  answers `unborn HEAD symref-target:refs/heads/trunk`.
- Protocol v0 reports nothing.

**go-git asks the question, then throws the answer away.** At v6.0.0-alpha.5, and unchanged on
upstream main at `fc18716c`:

- It speaks protocol v2 by default, and sends `unborn` when the server advertises
  `ls-refs=unborn` (`internal/transport/v2.go`, `lsRefsSupportsUnborn`).
- But when there are no hash refs, `GetRemoteRefs` returns plain `ErrEmptyRemoteRepository` for an
  upload-pack session, and the reply is discarded:
  - `plumbing/transport/http/handshake.go` ~252;
  - `plumbing/transport/pack_stream.go` ~119.
- `RemoteRefs.Unborn` exists, but it is unreachable for an empty repository.
- So our `CheckRepo` has to answer "no way to know" (`internal/git/git.go:59`).

**Why our live test passes.** `TestCheckRepo_PublicConnectivityEmpty`
(`internal/git/git_operations_test.go:175`, against `ConfigButler/empty.git`) runs, and is not
disabled. It skips only without network. It asserts that `DefaultBranch` is **empty** and that
the local HEAD is unborn on the *write* branch (`cool-test`). It pins today's behavior rather than
the server's knowledge.

**A small, backward-compatible go-git patch recovers the answer.** I tested this patch through a
temporary `-modfile` `replace`; nothing in the repository changed. The patch:

- adds `transport.EmptyRemoteRepositoryError{Unborn plumbing.ReferenceName}`, whose `Is(ErrEmptyRemoteRepository)`
  is true;
- returns it from both empty-advertisement paths above.

Results:

- `Remote.List` against `ConfigButler/empty.git` returns an error that still matches
  `errors.Is(err, ErrEmptyRemoteRepository)`, and `errors.As` yields `Unborn = refs/heads/main`.
- The existing live test still passes unchanged.
- The diff is in §7.

**Our hermetic harness cannot show this today.** `startGitHTTPServer`
(`internal/git/ado_multiack_test.go` ~125) deliberately strips `Git-Protocol` to force protocol
v0, so that it simulates Azure DevOps. Against it, the patched go-git returns the plain error with
no `Unborn`.

**Not reported:** any server or path that negotiates v0/v1. Whether Azure DevOps supports v2 is
unknown; the harness assumes v0. SSH to GitHub needs `GIT_PROTOCOL` sent over the session; that is
not verified.

## 3. Proposed behavior

Everything below turns on one name, the **default branch D** of an empty repository. There are two
ways to know D, and they ship in two stages.

### 3.1 Stage 1, which this plan leads with: assume `main`

**D is `main`**, hardcoded. This needs no go-git change, so it can ship now, alongside or right
after the hardening pass.

- `main` is what GitHub, GitLab and Azure DevOps create for a new repository, and what `git init`
  suggests. `master` is not an acceptable default in 2026, and nothing here falls back to it.
- Nothing is guessed silently. The assumption is visible in `status.remote.parent.branch`, it is
  named in the refusal message, and it is documented in `configuration.md`.

### 3.2 Stage 2: ask the server

When the server reports its unborn HEAD (§2: protocol v2 `ls-refs` with `unborn`, once go-git
passes it through, §4), **D is what the server said**. This only refines stage 1: the rules below
are the same, and only where D comes from changes.

### 3.3 The rules, given D

| Empty repository, and… | Behavior |
|---|---|
| `branch == D`, parent omitted | as today: the first commit creates D as the root. `status.remote.parent = {state: Unborn, branch: D}` |
| `branch != D`, parent omitted | **refuse.** Ready=False/Stalled, reason `ParentBranchUnborn`: "the repository is empty; its default branch 'D' has no commits yet. Push a first commit to 'D', or set `spec.branch: D`". Nothing is written; the work is retained, or remembered per hardening §4.4. When D appears, the normal standby flow creates the write branch from it |
| `parentBranch: P` | the same refusal and reason, naming P. Today this is `Missing`, a less precise message. See the escape hatch below for `P == branch` |

**The cost of stage 1.** A repository whose real default branch is not `main` (say `trunk`), on a
server that does not report its unborn HEAD (v0/v1; Azure DevOps is unverified), with
`branch: trunk`:

- Today that bootstraps `trunk` correctly.
- Under stage 1 it is refused, because we believe D is `main`. The message says exactly what to
  do: push a first commit to `trunk`, after which everything works.
- Stage 2 removes the refusal for every server that reports its unborn HEAD, which includes
  GitHub.

**Escape hatch, decision for the user:** allow `parentBranch == branch` on an empty repository to
mean "start this branch with no history".

- This is explicit intent, needs no knowledge of D, and fixes the cost above without a manual
  push.
- It reverses one rule from #408. Today, `parentBranch == branch` on an absent branch is refused
  even in an empty repository.

**Why refuse rather than bootstrap D ourselves.** The parent-branch contract is that the parent is
*only read*, and it is not in `allowedBranches`. Creating D ourselves would break that, and would
mean inventing an "initialize repository" commit on a branch we don't own. Refusing is the "clear
failure" this design asks for, and recovery is the existing standby flow.

**Second decision for the user:** an opt-in that bootstraps instead.

- The mechanism would be one atomic push of two refs: an empty-tree commit on D, and the content
  commit on the write branch on top of it, with D required to be in `allowedBranches`.
- This plan does not include it. If you want it, it is a follow-up.

**`RemoteObservation` and the API.** For `Unborn`, `ParentBranch` and `status.remote.parent.branch`
now always carry D. Update the doc comments that today say "empty when Unborn":

- `internal/git/remote_observation.go`;
- `api/v1alpha3/gittarget_types.go` ~498–512;
- `configuration.md`.

## 4. Getting the go-git change (stage 2 only)

1. **Upstream first.** Open a go-git issue and a PR with §7: the error type, both paths, and tests
   against a v2 `git http-backend` and over `file://`.
   - The change is additive, and backward-compatible for every `errors.Is` caller.
   - For prior art on our upstream write-ups, see the `file://` CAS write-up in
     `external-sources/go-git/` (gitignored).
2. **Our side waits for a go-git release that carries it.** Until then stage 1 applies, so nothing
   waits on upstream.
3. **Fallback, if upstream stalls:** a temporary `replace` to a fork tag.
   - Only with the user's agreement. We already pin an alpha.
   - Do not hand-roll a v2 `ls-refs` client in this repository.

## 5. Steps

### 5.1 Stage 1 (now)

0. **Verify the GitHub claim** in §1 on a *throwaway* repository the user creates, never on
   `ConfigButler/empty.git`. The question: does pushing a non-default branch first make it the
   default? Record the answer in §1. It sharpens the motivation but does not change the design.
1. **Discovery.** The empty-repository result carries D. For now that is the constant
   `DefaultBranchForEmptyRepository = "main"`, with one comment explaining why `main` and why
   never `master`. `CheckRepo` returns `DefaultBranch{ShortName: "main", Unborn: true}`.
2. **Worker.**
   - The empty-repository path applies the §3.3 table before `makeHeadUnborn`.
   - The refusal is a new sentinel, `ErrParentBranchUnborn`. Handle it like
     `ErrParentBranchNotFound` on fetch, refresh, publication and the hardening probe, so the same
     recovery obligation applies.
   - The observation becomes `ParentState=Unborn` with `ParentBranch=D`.
3. **Controller.** `parentBranchReadiness` maps that observation (with a `ParentRequested` match)
   to `ParentBranchUnborn`, Stalled, using the §3.3 message.
4. **Tests**, red first:
   - empty repository with `branch: main`: a root commit on `main`, as today. This covers the
     quickstart path;
   - `branch: feature`: refused, nothing pushed, the work retained. Then push `main` externally,
     and assert that `feature` is created from it without another cluster edit;
   - `parentBranch: release` on an empty repository: the `ParentBranchUnborn` message;
   - the escape hatch, if chosen;
   - ledger: rows unchanged. A constant costs no connection.
5. **Live test.** `TestCheckRepo_PublicConnectivityEmpty` expects `main` / `Unborn`, and its
   `PrepareBranch("cool-test")` half now expects the refusal.
6. **Docs.**
   - In `configuration.md`'s parent-branch section: add the empty-repository rows, the `main`
     assumption, and the escape hatch, if chosen.
   - In UPGRADING: a target writing anything other than `main` into an empty repository now waits
     for `main`, or for the escape hatch.

### 5.2 Stage 2 (once go-git carries the change)

1. **Harness.** Add a `startRealGitServerV2`, or an option, that keeps `Git-Protocol`, so v2
   reaches `git http-backend`. Leave the ADO/v0 server and the ledger rows untouched.
2. **Discovery.** `listRemoteRefs`/`SmartFetchFrom` and `CheckRepo` use `errors.As` to read the
   unborn target. When present it replaces the constant; when absent, the constant stays.
3. **Tests**, on the v2 harness, with a bare repository made by `init -b trunk`:
   - `branch: trunk` bootstraps `trunk`;
   - `branch: feature` is refused, naming `trunk`.

   On the v0 harness, the stage 1 behavior holds and is pinned. The ledger is unchanged: the
   unborn target rides on the `ls-refs` call discovery already makes.

## 6. Out of scope

- The opt-in bootstrap (§3.3, a decision for the user).
- Repositories that are non-empty but whose default branch is unresolved. That is hardening §4.3.

## 7. The go-git patch used in the experiment (against v6.0.0-alpha.5)

```text
--- a/plumbing/transport/errors.go
+++ b/plumbing/transport/errors.go
@@ import (
     "errors"
 
+    "github.com/go-git/go-git/v6/plumbing"
+
     internal "github.com/go-git/go-git/v6/internal/transport"
 )
@@
+// EmptyRemoteRepositoryError is ErrEmptyRemoteRepository with what the server
+// said about its unborn HEAD (protocol v2 ls-refs "unborn"), when it said it.
+type EmptyRemoteRepositoryError struct {
+    // Unborn is the branch HEAD points at, e.g. refs/heads/main; empty when
+    // the server did not report it (v0/v1, or no ls-refs=unborn support).
+    Unborn plumbing.ReferenceName
+}
+
+func (e *EmptyRemoteRepositoryError) Error() string { return ErrEmptyRemoteRepository.Error() }
+
+func (e *EmptyRemoteRepositoryError) Is(target error) bool { return target == ErrEmptyRemoteRepository }
+
+func NewEmptyRemoteRepositoryError(refs []*plumbing.Reference) error {
+    return &EmptyRemoteRepositoryError{Unborn: NewRemoteRefs(refs).Unborn}
+}
--- a/plumbing/transport/http/handshake.go   (smartPackSession.GetRemoteRefs, v2 branch)
-            return nil, transport.ErrEmptyRemoteRepository
+            return nil, transport.NewEmptyRemoteRepositoryError(refs)
--- a/plumbing/transport/pack_stream.go      (StreamSession.GetRemoteRefs, v2 branch)
-            return nil, ErrEmptyRemoteRepository
+            return nil, NewEmptyRemoteRepositoryError(refs)
```

To reproduce the experiment:

1. Copy the module from `/go/pkg/mod/github.com/go-git/go-git/v6@v6.0.0-alpha.5` into a scratch
   directory and apply the patch.
2. Copy `go.mod` and `go.sum` to `exp.mod` and `exp.sum`, and append
   `replace github.com/go-git/go-git/v6 => <scratch>/go-git`.
3. Run `go test -modfile=exp.mod ./internal/git -run <a temporary test calling Remote.List>`.

An upstream PR should probably also expose the unborn target on `Remote.List`'s caller-facing API
rather than only through the error. Discuss the shape with the maintainers.
