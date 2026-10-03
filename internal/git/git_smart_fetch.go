// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	gitclient "github.com/go-git/go-git/v6/plumbing/client"
	"github.com/go-git/go-git/v6/plumbing/transport"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// SmartFetch performs a network sync and returns the best available LOCAL branch reference.
// It prioritizes the target branch but always fetches the default branch as a safety net.
//
// Return values (example with target="refs/heads/feature"):
// - "refs/heads/feature", nil: Target found on remote, fetched, ready to checkout.
// - "refs/heads/main", nil:    Target missing on remote, fell back to default branch.
// - "", nil:                   The repository is empty (see advertisesNoRefs).
// - "", ErrDefaultBranchUnresolved: Not empty, and no default branch to fall back to.
func SmartFetch(
	ctx context.Context,
	repo *git.Repository,
	target plumbing.ReferenceName, // e.g. "refs/heads/feature" or "HEAD"
	auth []gitclient.Option,
) (plumbing.ReferenceName, error) {
	return SmartFetchFrom(ctx, repo, target, "", auth)
}

// ErrParentBranchNotFound reports that a configured parent branch is absent on the remote while
// the target branch is absent too, so there is nothing to create the target branch from. It is
// never reported for an omitted parent: that falls back to the remote's default branch, and to
// an unborn branch in an empty repository.
var ErrParentBranchNotFound = errors.New("parent branch not found on the remote")

// ErrDefaultBranchUnresolved reports a repository that is not empty but whose default branch does
// not resolve to a branch it carries, while no parent is configured and the target branch is
// absent. Starting the target branch there would make an orphan, so it is refused exactly like a
// missing configured parent. DefaultBranchUnresolvedError names what HEAD points at.
var ErrDefaultBranchUnresolved = errors.New("the remote's default branch does not resolve to a branch")

// DefaultBranchUnresolvedError is ErrDefaultBranchUnresolved with the evidence: the branch the
// remote's HEAD names, or empty when the advertisement carries no symbolic HEAD at all.
//
// The resolution itself follows go-git. Remote.List rewrites a hash-only HEAD (a server without
// the symref capability, or a detached HEAD) into a symbolic one before returning: to master when
// master has that hash, otherwise to the alphabetically first branch that has it. So a HEAD this
// error reports is one even that heuristic could not place.
type DefaultBranchUnresolvedError struct {
	// Head is the short name of the branch HEAD points at; empty when there is no symbolic HEAD.
	Head string
}

func (e *DefaultBranchUnresolvedError) Error() string {
	if e.Head == "" {
		return "the remote has no default branch"
	}
	return fmt.Sprintf("the remote's default branch (HEAD -> %s) does not exist", e.Head)
}

// Is makes the error match ErrDefaultBranchUnresolved.
func (e *DefaultBranchUnresolvedError) Is(target error) bool {
	return target == ErrDefaultBranchUnresolved
}

// advertisesNoRefs reports an empty repository: an advertisement with no hash refs at all, no
// branch, no tag, nothing else. A symbolic HEAD pointing at an unborn branch does not count, and a
// repository with only tags is not empty. This is the one definition discovery, publication and
// the observation share.
func advertisesNoRefs(refs []*plumbing.Reference) bool {
	for _, ref := range refs {
		if ref.Type() == plumbing.HashReference && ref.Name() != plumbing.HEAD {
			return false
		}
	}
	return true
}

// symbolicHeadTarget is the short name of the branch the advertisement's HEAD points at, or empty.
func symbolicHeadTarget(refs []*plumbing.Reference) string {
	for _, ref := range refs {
		if ref.Name() == plumbing.HEAD && ref.Type() == plumbing.SymbolicReference {
			return ref.Target().Short()
		}
	}
	return ""
}

// SmartFetchFrom is SmartFetch with an explicit parent: the branch to fall back to when the target
// branch is absent. Empty means the remote's default branch. A configured parent that is absent is
// an error rather than a fallback, so a typo can never start an orphan branch; it does not matter
// while the target branch exists.
func SmartFetchFrom(
	ctx context.Context,
	repo *git.Repository,
	target plumbing.ReferenceName,
	parent string,
	auth []gitclient.Option,
) (plumbing.ReferenceName, error) {
	remoteName := "origin"
	remote, err := repo.Remote(remoteName)
	if err != nil {
		return "", fmt.Errorf("failed to get remote %s: %w", remoteName, err)
	}

	// 1. Audit: List refs
	refs, err := listRemoteRefs(ctx, remote, auth)
	if err != nil {
		return "", err
	}
	if len(refs) == 0 && parent == "" {
		return "", nil
	}

	// 2. Analyze: Find default branch and check target existence
	headFull, headShort, targetExists := analyzeRemoteRefs(ctx, refs, target.String())

	defaultFull, defaultShort, err := fallbackBranch(refs, parent, headFull, headShort, targetExists)
	if err != nil {
		return "", err
	}

	// 3. Plan: Build RefSpecs based on analysis
	refSpecs := buildSmartRefSpecs(remoteName, defaultFull, defaultShort, target, targetExists)

	// Determine Result (The return value)
	var result plumbing.ReferenceName
	switch {
	case targetExists:
		result = target
	case defaultFull != "":
		result = plumbing.ReferenceName(defaultFull)
	default:
		return "", nil
	}

	// 4. Execute: Fetch
	if len(refSpecs) > 0 {
		if err := fetchBounded(ctx, repo, remoteName, refSpecs, auth); err != nil {
			return "", err
		}
	}

	// 5. Repair: Fix local symbolic HEAD. It mirrors the remote's HEAD, never a configured parent.
	repairRemoteSymbolicHead(repo, remoteName, headShort)

	return result, nil
}

// fallbackBranch is the branch to fall back on, and to fetch as the safety net: the configured
// parent, or the remote's default branch when none is configured. A parent the remote does not
// carry is an error only while the target is absent too: a configured one is
// ErrParentBranchNotFound, and an unresolved default branch in a repository that is not empty is
// ErrDefaultBranchUnresolved, because an unborn start there would be an orphan beside the refs it
// ignored.
func fallbackBranch(
	refs []*plumbing.Reference, parent, headFull, headShort string, targetExists bool,
) (string, string, error) {
	if parent == "" {
		if headFull == "" && !targetExists && !advertisesNoRefs(refs) {
			return "", "", &DefaultBranchUnresolvedError{Head: symbolicHeadTarget(refs)}
		}
		return headFull, headShort, nil
	}
	full, short := configuredParent(refs, parent)
	if full == "" && !targetExists {
		return "", "", fmt.Errorf("%w: %q", ErrParentBranchNotFound, parent)
	}
	return full, short, nil
}

// configuredParent finds a configured parent branch in the advertisement, or returns empty names.
func configuredParent(refs []*plumbing.Reference, parent string) (string, string) {
	full := plumbing.NewBranchReferenceName(parent)
	for _, ref := range refs {
		if ref.Name() == full && ref.Type() == plumbing.HashReference {
			return full.String(), parent
		}
	}
	return "", ""
}

// listRemoteRefs reads the remote's advertisement, bounded by gitCallTimeout: see network_bound.go.
func listRemoteRefs(
	ctx context.Context, remote *git.Remote, auth []gitclient.Option,
) ([]*plumbing.Reference, error) {
	ctx, cancel := boundGitCall(ctx)
	defer cancel()
	refs, err := remote.ListContext(ctx, &git.ListOptions{ClientOptions: boundToContext(ctx, auth)})
	if errors.Is(err, transport.ErrEmptyRemoteRepository) {
		return nil, nil // Valid state, not an error
	}
	if err != nil {
		return nil, fmt.Errorf("failed to list remote refs: %w", boundedCallError(ctx, err))
	}
	return refs, nil
}

// fetchBounded is SmartFetch's transfer, bounded by gitCallTimeout: see network_bound.go.
func fetchBounded(
	ctx context.Context, repo *git.Repository, remoteName string, refSpecs []config.RefSpec,
	auth []gitclient.Option,
) error {
	ctx, cancel := boundGitCall(ctx)
	defer cancel()
	err := repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName:    remoteName,
		ClientOptions: boundToContext(ctx, auth),
		RefSpecs:      refSpecs,
		Depth:         1,
		Force:         true,
		Prune:         true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return fmt.Errorf("smart fetch failed: %w", boundedCallError(ctx, err))
	}
	return nil
}

// analyzeRemoteRefs scans the reference list to find the default branch and check if the target exists.
func analyzeRemoteRefs(ctx context.Context, refs []*plumbing.Reference, targetFullStr string) (string, string, bool) {
	logger := log.FromContext(ctx)

	var defaultFull, defaultShort string
	var targetExists bool

	// Map existing refs for O(1) lookup validation
	existingRefs := make(map[string]bool, len(refs))
	for _, ref := range refs {
		existingRefs[ref.Name().String()] = true
	}

	for _, ref := range refs {
		name := ref.Name().String()

		// Check for Default Branch (HEAD)
		if name == "HEAD" && ref.Type() == plumbing.SymbolicReference {
			target := ref.Target().String()
			if existingRefs[target] {
				defaultFull = target
				defaultShort = cleanBranchName(ref.Target().Short())
			} else {
				logger.Info("Remote HEAD is broken (points to missing ref)", "target", target)
			}
		}

		// Check for Target
		if name == targetFullStr {
			targetExists = true
		}
	}

	return defaultFull, defaultShort, targetExists
}

func buildSmartRefSpecs(
	remoteName, defaultFull, defaultShort string,
	target plumbing.ReferenceName,
	targetExists bool,
) []config.RefSpec {
	var refSpecs []config.RefSpec

	// A. Always fetch Default (Safety Net)
	if defaultFull != "" {
		spec := config.RefSpec(fmt.Sprintf("+%s:refs/remotes/%s/%s", defaultFull, remoteName, defaultShort))
		refSpecs = append(refSpecs, spec)
	}

	// B. Fetch Target (If valid and different)
	if targetExists {
		targetFullStr := target.String()
		if defaultFull != targetFullStr {
			spec := config.RefSpec(fmt.Sprintf("+%s:refs/remotes/%s/%s", targetFullStr, remoteName, target.Short()))
			refSpecs = append(refSpecs, spec)
		}
	}
	return refSpecs
}

func repairRemoteSymbolicHead(repo *git.Repository, remoteName, defaultShort string) {
	if defaultShort == "" {
		return
	}
	symRef := plumbing.NewSymbolicReference(
		plumbing.NewRemoteReferenceName(remoteName, "HEAD"),
		plumbing.NewRemoteReferenceName(remoteName, defaultShort),
	)
	_ = repo.Storer.SetReference(symRef)
}

// cleanBranchName handles the edge case where .Short() returns "origin/main" instead of "main".
func cleanBranchName(name string) string {
	if len(name) > 7 && name[:7] == "origin/" {
		return name[7:]
	}
	return name
}
