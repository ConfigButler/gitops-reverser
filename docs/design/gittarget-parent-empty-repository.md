# An empty repository's first commit

> **design**: built in #480 (2026-11-10). Records the behavior, why it was chosen over the
> alternatives, and what was deliberately not verified.
>
> Related: [`gittarget-parent-branch.md`](gittarget-parent-branch.md), the parent contract as
> built; [`gittarget-parent-hardening.md`](gittarget-parent-hardening.md), which defines "empty"
> (§4.2) and `Unborn` (§4.3).

## 1. The situation

An empty repository has no refs, but its `HEAD` already names a branch: `main` on Gitea, Forgejo
and GitHub by default, or whatever `init.defaultBranch` or an organization setting chose. That
branch is unborn: it has a name and no commit. Protocol v2 servers report the name (`ls-refs`
`unborn`); #480 reads it through go-git's public `transport.Commander` (`unbornHead` in
`internal/git/git_smart_fetch.go`), and status shows it as `status.remote.parent: {state: Unborn,
branch: main}`.

A `GitBranch` writes `spec.branch`, created from `spec.parentBranch`, or from the remote's default
branch when that is omitted. In an empty repository the omitted parent is the unborn branch.

## 2. Behavior

With `spec.parentBranch` omitted, in an empty repository:

| The server names its default branch | `spec.branch` is it | The provider allows this `GitBranch` to write it | What happens |
|---|---|---|---|
| yes | yes | (it is `spec.branch`, so yes) | the first commit starts `spec.branch` with no history |
| yes | no | yes | the default branch is created first with one empty commit; `spec.branch` starts from it |
| yes | no | no | nothing is written: `GitBranch` `Ready=False`, `Stalled=True`, reason `DefaultBranchCreationNotAllowed`; its `GitTarget`s `GitBranchNotReady` |
| no (protocol v0/v1) | unknown | | the first commit starts `spec.branch` as the repository's first branch |

- **The empty commit** is authored and signed like every other commit the provider makes
  (`spec.commit`), with the message `chore: initialize repository`. It is pushed alone, with
  compare-and-swap on the branch being absent, before anything is written to `spec.branch`. After
  it, the repository is no longer empty, and `spec.branch` is an ordinary absent branch whose parent
  is `Found`.
- **When:** as soon as the worker picks up work for the branch, even work that changes nothing; a
  refresh only looks. It prepares the base the work builds on, and a bad credential, signing key or
  branch protection fails at setup instead of at the first change. `spec.branch` still waits for a
  change. A default branch somebody creates by hand meanwhile is used as it is
  (`TestBranchWorker_TheDefaultBranchIsCreatedWhenWorkIsPickedUp`,
  `TestBranchWorker_ADefaultBranchCreatedByHandIsPickedUp`).
- **Permission is the existing allowlist.** Creating the default branch is a write to a branch
  other than `spec.branch`, so the provider's `spec.allowedGitBranches` must allow this `GitBranch`'s
  namespace to write it, as it would for a `GitBranch` on that branch. There is no setting to opt in
  or out.
- **Without permission, writes are held**, the same way as for a missing configured parent: the
  worker latches parent recovery and probes on its retry schedule. A probe counts the parent as
  found once the default branch exists (somebody pushed a first commit) or once the provider allows
  creating it; either way the held writes land on it.
- **A default branch that appears during our push** (another writer, or another `GitBranch`) makes
  the server refuse our empty commit. The sync that follows finds theirs and `spec.branch` starts
  from it; the refusal is not reported as an error.
- `ParentUnborn` stays the state of an empty repository, and `ErrDefaultBranchUnresolved` stays the
  refusal for a repository with commits whose `HEAD` names a branch it does not carry. Over v2 that
  refusal now names the branch.

Code: `internal/git/unborn_default_branch.go` (create or refuse),
`internal/git/parent_recovery.go` (`probeParent`), `internal/controller/gitbranch_controller.go`
(`gateUnbornDefault`), `internal/authz` (`GitBranchMayWrite`, shared by both).

## 3. Why

Starting `spec.branch` as the repository's first branch looked harmless, but on the common forges
it is not. Gitea and Forgejo make the first branch pushed into an empty repository the default
(`services/repository/push.go`, verified in both sources). So a review branch such as
`reverser/edits` became the repository's default branch without anyone reviewing it, and every
later `GitBranch` without a parent built on its unreviewed commits. A PR into `main` was impossible,
because `main` did not exist. On a plain git server `HEAD` kept naming `main`, which then dangled,
and later `GitBranch`es were refused until someone repaired the repository.

The alternatives considered:

- **Keep the bootstrap**: the outcome above.
- **Refuse until the default branch exists**: clean, but every review-branch user has to push a
  first commit by hand before anything works.
- **Always create the default branch**: writes a branch nobody allowed.
- **Create it when the allowlist allows it, refuse otherwise** (built): the history starts on the
  default branch, which stays the default; later write branches build on an empty commit, not on
  someone's edits; and the first PR into the default branch carries everything written, so the
  initial snapshot gets reviewed. Creating a commit nobody explicitly asked for still feels odd, but
  leaving users of these platforms with a review branch as their default branch is worse.

## 4. Deliberately not verified

What other platforms (GitHub, GitLab, Azure DevOps, Bitbucket) do on a first push into an empty
repository is not pinned down, and does not need to be for now. When the server names its default
branch, the default branch is pushed first, alone, so whatever a forge does with "the first pushed
branch" lands on the right branch. It matters only on servers that do not name it (v0/v1), where
`spec.branch` is still created as the first branch.

Also unverified: whether Azure DevOps speaks protocol v2, and whether our SSH transport carries it.
Where they do not, the server does not name the default branch, and the last row of §2 applies.
