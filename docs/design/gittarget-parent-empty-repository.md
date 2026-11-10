# An empty repository's first commit: behavior analysis

> **design**: open. Analysis dated 2026-11-10, written after #480 made the evidence available.
> Nothing here is built beyond #480's status field.
>
> Related: [`gittarget-parent-branch.md`](gittarget-parent-branch.md), the parent contract as
> built; [`gittarget-parent-hardening.md`](gittarget-parent-hardening.md), which defines "empty"
> (§4.2) and `Unborn` (§4.3).

## 1. The situation

An empty repository has no refs, but its `HEAD` already names a branch: `main` on Gitea, Forgejo
and GitHub by default, or whatever `init.defaultBranch` or an organization setting chose. That
branch is unborn: it has a name and no commit.

A `GitBranch` writes `spec.branch`, created from `spec.parentBranch`, or from the remote's default
branch when that is omitted. In an empty repository the omitted parent is the unborn branch. Two
cases:

- **`spec.branch` is the unborn branch** (the quickstart: `branch: main`). The first commit creates
  the default branch. Every option below agrees on this, and nothing about it is weird.
- **`spec.branch` is another branch** (a review-branch workflow: `branch: reverser/edits`). This
  page is about this case.

What we know about the unborn branch, since #480:

| Server | Reports the unborn name | How we know |
|---|---|---|
| GitHub (HTTPS) | yes, `main` | live test `TestCheckRepo_PublicConnectivityEmpty` |
| Gitea, Forgejo (HTTPS) | yes | both forward `Git-Protocol` to git (`routers/web/repo/githttp.go`); the #480 e2e spec checks Gitea |
| canonical `git http-backend` | yes, from git 2.31 | `unborn_head_test.go` |
| any server over protocol v0/v1 | no | `unborn_head_test.go` |
| Azure DevOps | unknown | its v2 support is unverified; the test harness assumes v0 |
| any server over SSH | unknown | needs `GIT_PROTOCOL` passed through the SSH session; unverified |

## 2. What happens today

The first write creates `reverser/edits` as a root commit. `main` does not exist afterwards. What
happens next depends on the server, because the server, not us, decides what `HEAD` becomes.

**Gitea and Forgejo adopt the first pushed branch as the default.** On the first push into an empty
repository, `services/repository/push.go` sets the repository's default branch to the pushed ref
and moves `HEAD` to it (verified in both sources). So after our first push:

1. The repository's default branch is `reverser/edits`, a review branch nobody reviewed.
2. A second `GitBranch` on the repository, `branch: reverser/other` with no parent, resolves its
   parent to the default branch, so its first commit **builds on the first GitBranch's
   unreviewed commits**. Its PR into anything carries them. This is the weird case.
3. A PR `reverser/edits` → `main` cannot be opened: `main` does not exist. A human who creates
   `main` from the forge's UI (from `reverser/edits`) gets a `main` that already holds the edits,
   so the first review never happens. A human who pushes their own first commit to `main` creates
   an unrelated history, and the PR needs `--allow-unrelated-histories`.

**GitHub, GitLab and Azure DevOps reportedly do the same** (the first pushed branch becomes the
default). Unverified; verifying needs a throwaway repository on each.

**A plain git server keeps `HEAD` at `main`, which now dangles.** The repository is no longer empty
and its default branch resolves to nothing, so the second `GitBranch` with no parent is refused
with `ParentBranchNotFound`: "the remote's default branch (HEAD -> 'main') does not exist". Over v2
the message names `main` (#480); over v0 it says there is no default branch. Nothing is written
until a human fixes `HEAD` or sets `parentBranch`. Safe, but the human is now repairing a
repository we shaped.

**Two GitBranches whose first writes race** both create root commits, on two unrelated branches.
On Gitea whichever lands first becomes the default.

So today's behavior is not wrong for one `GitBranch`, but it makes the repository's history
start on a review branch, and on the common forges it promotes that branch to default.

## 3. Options

Each option changes only the case in §1 where `spec.branch` is not the unborn branch and
`spec.parentBranch` is omitted. A set `parentBranch` already behaves like B: it must exist,
or the target refuses and recovers when it appears.

### A. Keep today's bootstrap

The write branch is the repository's first branch, with the consequences in §2. No evidence
needed, so it behaves the same on every server.

### B. Refuse until the default branch exists

`Ready=False`, `Stalled=True`, reason `ParentBranchNotFound`, with a message that names the branch
the server reported: "the repository is empty and its default branch is 'main': push a first commit
to 'main', or set spec.branch: main". Writes are held, as for a missing configured parent. When
`main` appears, the existing parent probe finds it, and the held writes land on `main`'s tip
(`TestBranchWorker_AnEmptyRepositoryThatGainsABranchIsNotOrphaned` shows that replay works).

- Same rule as a set `parentBranch`: the omitted parent must exist too, unless it is the write
  branch itself. Easy to explain.
- Nothing is written that was not asked for, and the history starts on `main`.
- A human has to create the first commit. On GitHub and Gitea that is the "initialize with a
  README" checkbox, so many empty repositories never reach this case.
- **Needs the evidence.** Over v0 we cannot tell whether `spec.branch` is the unborn branch, so we
  cannot refuse only the review-branch case. Over v0 B has to fall back to A, or refuse every
  empty repository, including the quickstart's `branch: main`.

### C. The operator seeds the default branch

One atomic push creates two refs: `main` with an empty root commit ("Initialize repository",
authored as the provider's commit identity), and `reverser/edits` with the content on top of it.

- `main` stays the default. On Gitea the default branch follows the first ref in the push
  (`pushUpdates` handles the updates in order), so `main` goes first. Unverified on the other
  forges.
- The first PR, `reverser/edits` → `main`, shows the whole initial snapshot as one reviewable
  change. This is the outcome a review workflow wants.
- A second `GitBranch` builds on `main`, the empty commit, not on the first one's edits.
- Racing first writes converge: one seed wins, the other push is refused (it expected `main` to be
  absent) and replays onto the winner's `main`.
- **It writes a branch nobody named in `spec.branch`.** That needs authorization:
  `main` must be in the provider's `allowedGitBranches`. If it is not, C cannot apply.
- **Needs the evidence**, like B: without the name, there is nothing to seed.
- Cost: today's push sends one ref update (`git_atomic_push.go`); this needs two, with
  compare-and-swap on both. The protocol and the `atomic` capability we already request
  support that.

### D. Seed when allowed, otherwise refuse

C when `allowedGitBranches` covers the unborn branch, B otherwise, and A when the server did not
report the name. No new field: the existing allowlist decides whether we may write `main`.

- Best outcome where it applies, honest refusal where it does not.
- Implicit: whether we create a commit on `main` depends on an allowlist entry, which a reader of
  the `GitBranch` cannot see. Status and an event must say "seeded 'main'" when it happens.

## 4. How the options compare

| | A (today) | B (refuse) | C (seed) | D (seed or refuse) |
|---|---|---|---|---|
| History starts on | the review branch | `main`, by a human | `main`, by the operator | `main` |
| Gitea default branch afterwards | the review branch | `main` | `main` | `main` |
| A second no-parent `GitBranch` builds on | the first one's edits (Gitea), or is refused (plain git) | `main` | `main` | `main` |
| First PR into `main` possible | no | yes | yes | yes |
| Writes a branch not in `spec.branch` | no | no | yes | only if allowed |
| Human action needed | later, to repair | before the first write | none | none, or before the first write |
| Without the unborn name (v0) | same | falls back to A, or refuses all | falls back to A | falls back to A |

## 5. Recommendation

**D, built as B first.**

1. **B now**: refuse the review-branch case when the server reported the name, keep A when it did
   not. It is small (`fallbackBranch` already refuses a missing configured parent, and the unborn
   name reaches it through `smartFetch`). It uses the existing parent probe and replay. It ends
   the Gitea stacking.
2. **C behind the allowlist, if the human step in B turns out to be friction**: the two-ref atomic
   push is the real work. Both steps keep the quickstart (`branch: main`) unchanged.

A loses because, on the forges people use, it promotes an unreviewed branch to default and stacks
later branches on it. C alone loses because it writes `main` without permission. B alone is
acceptable but leaves the human step.

## 6. To verify before building

- What GitHub, GitLab and Azure DevOps do on the first push into an empty repository: does the
  pushed branch become the default? Use a throwaway repository, never `ConfigButler/empty.git`.
- Whether Azure DevOps speaks protocol v2, and whether our SSH transport carries it. Where it does
  not, B and C fall back to A.
- For C: that Gitea takes the first ref of an atomic two-ref push as the default branch, and what
  the other forges do with it.
