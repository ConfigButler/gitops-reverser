# GitTarget parent branch: an empty repository's default branch

> **design**: deferred; not part of #407. The experiment in §2 is done, nothing else is built.
> Dated 2026-10-01.
>
> Related: [`gittarget-parent-branch.md`](gittarget-parent-branch.md), the feature as built;
> [`gittarget-parent-hardening.md`](gittarget-parent-hardening.md), which keeps today's
> empty-repository behavior and defines "empty" (§4.2) and `Unborn` (§4.3).

## 1. The problem

Take an empty repository whose default branch is `main`, but which has no commits yet. Add a
`GitTarget` with `branch: reverser/edits` and no `parentBranch`.

Today the first write creates **`reverser/edits` as the repository's first branch**:

- The commit has no parent, and no `main` exists afterwards.
- What we pushed does not match the branch the repository says is its default.
- On GitHub, the first branch pushed to an empty repository probably becomes the default branch
  (unverified, §5). If so, the operator's review branch silently becomes the repository's default
  branch. Even if not, the eventual PR `reverser/edits` → `main` cannot be made: the two branches
  have no common history.

This is more than a status detail: for a review-branch workflow it is the wrong branch on the first
commit we make. It is also what the bootstrap contract says today ("only an omitted parent
bootstraps", and the first commit is a root on the write branch), so changing it is a policy and
compatibility decision, not a bug fix.

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

**A small, backward-compatible go-git patch recovers the answer.** It was tested through a
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

## 3. Direction

Two things, in this order:

1. **Get the evidence.** Recover the server's unborn `HEAD` through go-git (§4). With it, an empty
   repository can report what it actually is: `status.remote.parent = {state: Unborn, branch: X}`
   when the server said X, and an empty `branch` when it did not. Behavior does not change.
2. **Then decide the policy**, separately, with an explicit compatibility note. The question: when
   the server says the default branch is X and the write branch is not X, may the first commit
   still create the write branch as the repository's first branch?

   | Option | Effect |
   |---|---|
   | keep today's bootstrap | the write branch becomes the first branch (as now) |
   | refuse until X exists | `Ready=False`, `Stalled=True`, a reason naming X: "push a first commit to 'X', or set `spec.branch: X`"; recovery is the normal standby flow |
   | opt-in bootstrap of X | one atomic push: an empty-tree commit on X and the content on the write branch; X must be in `allowedBranches`. This writes to a branch the parent contract says is only read, so it needs its own field |

   Whichever is chosen applies only when the server reported X. Without that evidence, today's
   behavior stays.

### Considered and rejected: assume `main`

Assuming the default branch is `main` when the server does not say (never `master`) was considered
as a stage that needs no go-git change. It is rejected:

- It turns working configurations into refusals. A repository whose default is `trunk`, with
  `branch: trunk`, bootstraps correctly today and would be refused.
- The default is configurable in practice: `git init` takes `init.defaultBranch`, and GitHub
  organizations set the default name for new repositories. A guess is wrong exactly for those
  users.
- It would publish a guessed name inside a remote observation. Making the guess visible does not
  make it evidence; `status.remote` reports what the remote said.

Also not introduced here: reading `parentBranch == branch` on an empty repository as "start this
branch with no history". That would reverse a rule of the built contract as a side effect; if an
explicit bootstrap form is wanted, it gets its own design.

## 4. Getting the go-git change

1. **Upstream first.** Open a go-git issue and a PR with §7: the error type, both paths, and tests
   against a v2 `git http-backend` and over `file://`. It is additive and backward-compatible for
   every `errors.Is` caller. An upstream PR may prefer to expose the unborn target on the
   `Remote.List` result rather than only through the error; settle the shape with the
   maintainers.
2. **Our side waits for a go-git release that carries it.** Until then nothing changes.
3. **Fallback, if upstream stalls:** a temporary `replace` to a fork tag, only with the user's
   agreement. Do not hand-roll a v2 `ls-refs` client in this repository.

## 5. Steps once go-git carries it

1. **Verify the GitHub claim** in §1 on a *throwaway* repository, never on `ConfigButler/empty.git`:
   does pushing a non-default branch first make it the default? Record the answer in §1.
2. **Harness.** Add a v2 variant of `startRealGitServer` that keeps `Git-Protocol`. Leave the
   ADO/v0 server and the ledger rows untouched.
3. **Discovery.** `CheckRepo`, `listRemoteRefs` and `SmartFetchFrom` read the unborn target with
   `errors.As`. `CheckRepo` returns `DefaultBranch{ShortName: X, Unborn: true}`; the observation
   carries `ParentBranch=X` for `Unborn`. Update the doc comments that say "empty when Unborn"
   (`remote_observation.go`, `api/v1alpha3/gittarget_types.go`, `configuration.md`).
4. **Tests.** On the v2 harness with a bare repository made by `init -b trunk`, status reports
   `trunk`; on the v0 harness it reports an empty branch. Behavior is unchanged in both. The
   ledger is unchanged: the target rides on the `ls-refs` call discovery already makes.
5. **Live test.** `TestCheckRepo_PublicConnectivityEmpty` expects `main` / `Unborn`.
6. **Then the policy decision in §3**, as its own change with an UPGRADING note.

## 6. Out of scope

- The policy decision in §3, until the evidence exists.
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
