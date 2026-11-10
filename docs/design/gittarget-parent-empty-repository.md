# GitTarget parent branch: an empty repository's default branch

> **design**: evidence built (§4, 2026-11-10); the policy in §3 is deferred. Not part of #407.
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

**go-git asks the question, then throws the answer away.** At v6.0.0-alpha.5, still at
v6.0.0-beta.1 and on upstream main at `12b788e4` (line numbers below are alpha.5's; §4 has the
workaround):

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

**Built without a go-git change, on v6.0.0-beta.1** (the latest release as of 2026-11-10). beta.1
still discards the answer: `GetRemoteRefs` returns `ErrEmptyRemoteRepository` for an empty
repository before it builds the `RemoteRefs` whose `Unborn` field would carry it, so `Remote.List`
cannot expose it. Upstream main (`d6fa0275`) adds `LsRefsOutput.UnbornHead` but returns the same
error. What beta.1 does make public is enough:

- `listRemoteRefs` opens the upload-pack session itself (`gitclient.New(...).Handshake`) instead
  of calling `Remote.List`, and returns the same refs (peeled ones left out, as `Remote.List`
  does by default).
- On `ErrEmptyRemoteRepository`, `unbornHead` asks `ls-refs` again on that session through go-git's
  public `transport.Commander`, with `packp.LsRefsArgs{Symrefs, Unborn}`, and only when the server
  offers `ls-refs=unborn`. It is not a hand-rolled client: go-git encodes the request and decodes the
  reply. A v0/v1 session is not asked, and a reply that carries any hash ref is discarded (the
  repository stopped being empty in between).
- Cost: one more upload-pack POST, only for an empty repository over v2. The v0 harness and the
  request ledger are unchanged.

Delete `unbornHead` once a go-git release returns the name for an empty repository; the §7 patch
is still the shape to propose upstream.

## 5. Steps

1. **Verify the GitHub claim** in §1 on a *throwaway* repository, never on `ConfigButler/empty.git`:
   does pushing a non-default branch first make it the default? Not done; it matters only to §3.
2. **Harness.** Done: `adoSimulator.protocolV2` keeps `Git-Protocol`, off by default, so the
   ADO/v0 server and the ledger rows are untouched.
3. **Discovery.** Done: `CheckRepo` returns `DefaultBranch{ShortName: X, Unborn: true}`;
   `PullReport.ParentBranch` and the refresh advertisement carry X, and the observation is
   `{state: Unborn, branch: X}`. A push cannot see `HEAD`, so its observation keeps the name the
   last discovery read. `recordAbsentBranch` now decides `Unborn` by the absent parent commit, not
   the absent name.
4. **Tests.** Done (`internal/git/unborn_head_test.go`): a bare repository made by `init -b trunk`
   reports `trunk` over v2 and no name over v0; behavior is unchanged in both. Over v2 a dangling
   `HEAD` in a repository with commits is named too.
5. **Live test.** Done: `TestCheckRepo_PublicConnectivityEmpty` expects `main` / `Unborn`.
6. **Then the policy decision in §3**, as its own change with an UPGRADING note. Whatever is chosen,
   `ParentUnborn` and `ErrDefaultBranchUnresolved` stay: the first is the state an empty repository
   is in, the second a repository with commits whose `HEAD` names no branch it carries.

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
