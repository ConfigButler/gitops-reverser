// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"fmt"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/authz"
)

// An empty repository's HEAD names a branch with no commit yet: its default branch, unborn. When
// spec.parentBranch is omitted and the write branch is another branch, the worker creates that
// default branch first, with one empty commit, and starts the write branch from it. The history
// then starts on the default branch, which stays the default on every forge, and the first PR from
// the write branch into it carries everything the operator wrote. Starting the write branch as the
// repository's first branch instead would make it the default on Gitea and Forgejo, and every later
// write branch without a parent would build on its unreviewed commits.
//
// Creating the default branch is a write to a branch other than spec.branch, so it needs the same
// permission: the GitProvider's spec.allowedGitBranches must allow this GitBranch to write it.
// Without that, nothing is written and the GitBranch reports DefaultBranchCreationNotAllowed until the
// branch is allowed or exists. A server that does not report the unborn name (protocol v0/v1) gives
// nothing to create; the write branch then starts as the first branch, as before.

// ErrDefaultBranchCreationNotAllowed reports an empty repository whose default branch the worker would
// create for the write branch to start from, but the GitProvider does not allow this GitBranch to
// write it. DefaultBranchCreationNotAllowedError names the branch.
var ErrDefaultBranchCreationNotAllowed = errors.New("the empty repository's default branch may not be created")

// DefaultBranchCreationNotAllowedError is ErrDefaultBranchCreationNotAllowed with the branch the server named.
type DefaultBranchCreationNotAllowedError struct {
	// Branch is the short name of the unborn default branch.
	Branch string
}

func (e *DefaultBranchCreationNotAllowedError) Error() string {
	return fmt.Sprintf("the repository is empty and its default branch %q does not exist; "+
		"spec.allowedGitBranches does not allow creating it", e.Branch)
}

// Is makes the error match ErrDefaultBranchCreationNotAllowed.
func (e *DefaultBranchCreationNotAllowedError) Is(target error) bool {
	return target == ErrDefaultBranchCreationNotAllowed
}

// initializeCommitMessage is the message of the empty commit that creates the default branch.
const initializeCommitMessage = "chore: initialize repository\n\n" +
	"Created by GitOps Reverser so that branch '%s' has the default branch to start from.\n"

// unbornDefaultToCreate is the default branch a sync left unborn that the worker must create before
// the write branch can start from it, or empty when there is none: the write branch exists, a parent
// is configured, the repository is not empty, the server did not name its default branch, or the
// write branch is that default branch.
func (w *BranchWorker) unbornDefaultToCreate(report *PullReport, parent parentConfig) string {
	if report == nil || parent.name != "" || report.ExistsOnRemote || !report.HEAD.Unborn ||
		report.ParentBranch == "" || report.ParentBranch == w.Branch {
		return ""
	}
	return report.ParentBranch
}

// mayCreateUnbornDefault reports whether this worker's GitBranch may write branch. A worker that
// serves no GitBranch (the CLI, and tests) is allowed nothing.
func (w *BranchWorker) mayCreateUnbornDefault(
	ctx context.Context, provider *configv1alpha3.GitProvider, branch string,
) (bool, error) {
	if w.GitBranch.Name == "" {
		return false, nil
	}
	return authz.GitBranchMayWrite(ctx, w.Client, w.GitBranch, provider, branch)
}

// createUnbornDefault creates branch on the remote with one empty commit, signed and authored as the
// worker's other commits are, from a checkout that a sync just left unborn (an empty index). It is
// refused with DefaultBranchCreationNotAllowedError when the GitBranch may not write branch.
//
// The push's own failure comes back as an unbornDefaultPushError: a push refused because somebody
// else created branch meanwhile (the advertisement, or the server's compare-and-swap at upload) is
// not a failure, and only the sync that follows can tell the two apart.
func (w *BranchWorker) createUnbornDefault(
	ctx context.Context, repo *gogit.Repository, branch string, auth []gitclient.Option,
) error {
	provider, err := w.getGitProvider(ctx)
	if err != nil {
		return err
	}
	allowed, err := w.mayCreateUnbornDefault(ctx, provider, branch)
	if err != nil {
		return fmt.Errorf("check spec.allowedGitBranches for %q: %w", branch, err)
	}
	if !allowed {
		return &DefaultBranchCreationNotAllowedError{Branch: branch}
	}
	signer, err := getCommitSigner(ctx, w.Client, provider)
	if err != nil {
		return err
	}

	// The first commit has no parent, whatever the checkout holds: a local branch left by an
	// earlier attempt whose push failed, or by history fetched before the remote was emptied,
	// would become its parent and be uploaded with it.
	local := plumbing.NewBranchReferenceName(branch)
	if err := repo.Storer.RemoveReference(local); err != nil && !errors.Is(err, plumbing.ErrReferenceNotFound) {
		return fmt.Errorf("clear the local %q: %w", branch, err)
	}
	if err := setHead(repo, branch); err != nil {
		return fmt.Errorf("point HEAD at %q: %w", branch, err)
	}
	worktree, err := repo.Worktree()
	if err != nil {
		return err
	}
	operator := operatorSignature(ResolveCommitConfig(provider.Spec.Commit), time.Now())
	commit, err := worktree.Commit(fmt.Sprintf(initializeCommitMessage, w.Branch), &gogit.CommitOptions{
		Author:            operator,
		Committer:         operator,
		Signer:            signer,
		AllowEmptyCommits: true,
	})
	if err != nil {
		return fmt.Errorf("commit the first commit of %q: %w", branch, err)
	}

	if _, err := PushAtomic(ctx, repo, plumbing.ZeroHash, plumbing.NewBranchReferenceName(branch), auth); err != nil {
		return &unbornDefaultPushError{branch: branch, err: err}
	}
	w.Log.Info("Created the empty repository's default branch for the write branch to start from",
		"branch", w.Branch, "defaultBranch", branch, "commit", commit.String())
	return nil
}

// unbornDefaultPushError is the push of the default branch's first commit failing, which the sync
// after it judges: see createUnbornDefault.
type unbornDefaultPushError struct {
	branch string
	err    error
}

func (e *unbornDefaultPushError) Error() string {
	return fmt.Sprintf("create the default branch %q: %v", e.branch, e.err)
}

func (e *unbornDefaultPushError) Unwrap() error { return e.err }

// startFromUnbornDefault follows a sync that left the write branch unborn in an empty repository
// whose default branch is another branch: it creates that branch and syncs again, so the write
// branch starts from it. resync is the same sync. Any other report is returned unchanged.
func (w *BranchWorker) startFromUnbornDefault(
	ctx context.Context,
	repo *gogit.Repository,
	report *PullReport,
	parent parentConfig,
	auth []gitclient.Option,
	resync func() (*PullReport, error),
) (*PullReport, error) {
	branch := w.unbornDefaultToCreate(report, parent)
	if branch == "" {
		return report, nil
	}
	var pushErr *unbornDefaultPushError
	if err := w.createUnbornDefault(ctx, repo, branch, auth); err != nil && !errors.As(err, &pushErr) {
		return nil, err
	}
	report, err := resync()
	if err != nil {
		return nil, err
	}
	if w.unbornDefaultToCreate(report, parent) != "" {
		if pushErr != nil {
			return nil, pushErr
		}
		return nil, fmt.Errorf("created the default branch %q, but the remote does not show it", branch)
	}
	if pushErr != nil {
		w.Log.Info("The default branch appeared on the remote before ours; starting from it",
			"branch", w.Branch, "defaultBranch", branch, "pushError", pushErr.Error())
	}
	return report, nil
}
