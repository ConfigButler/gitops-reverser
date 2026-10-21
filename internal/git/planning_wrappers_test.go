// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"

	gogit "github.com/go-git/go-git/v6"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
)

// The planners report per event, or with the actions a resync decided; most tests ask only whether
// the tree changed. These are those narrower answers.

// flushEventsToWorktree is planEventsToWorktree without the per-event answer.
func (w *BranchWorker) flushEventsToWorktree(
	ctx context.Context,
	worktree *gogit.Worktree,
	base string,
	events []Event,
	policy *manifestanalyzer.PlacementPolicy,
	namespaces namespacePolicy,
	pruneMode v1alpha3.PruneMode,
) (bool, error) {
	changed, _, err := w.planEventsToWorktree(ctx, worktree, base, events, policy, namespaces, pruneMode)
	return changed, err
}

// applyPendingWriteEvents is planPendingWriteEvents without the per-event answer.
func (w *BranchWorker) applyPendingWriteEvents(
	ctx context.Context,
	repo *gogit.Repository,
	worktree *gogit.Worktree,
	events []Event,
	targets map[pendingTargetKey]ResolvedTargetMetadata,
) (bool, error) {
	changed, _, err := w.planPendingWriteEvents(ctx, repo, worktree, events, targets)
	return changed, err
}

// applyResyncToWorktree is planResyncToWorktree without the decided actions, for a GitTarget no
// other applier shares.
func (w *BranchWorker) applyResyncToWorktree(
	ctx context.Context,
	worktree *gogit.Worktree,
	base string,
	target ResolvedTargetMetadata,
	desired []manifestanalyzer.DesiredResource,
	scope *ResyncScope,
) (ResyncStats, bool, error) {
	stats, changed, _, err := w.planResyncToWorktree(ctx, worktree, base, target, desired, scope, nil)
	return stats, changed, err
}

// groupEventsByBase is groupEventsByBaseWithIndex without the indexes.
func groupEventsByBase(events []Event) map[string][]Event {
	byBase, _ := groupEventsByBaseWithIndex(events)
	return byBase
}
