// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"sort"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/ConfigButler/gitops-reverser/internal/manifestanalyzer"
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// LayoutReport is one scan's answer to "what does this folder's shape imply about where new
// documents go" — the resolution plus the revision and moment it was read at, so a consumer
// can tell a fresh answer from one taken before the folder changed.
type LayoutReport struct {
	manifestanalyzer.LayoutResolution

	// ByTypeEntries is how many placement.byType templates the target declares. It comes from
	// the spec rather than the scan, and is carried here so status.placement has one source.
	ByTypeEntries int
	// Revision is the Git revision the scan read; ObservedTime is when it ran.
	Revision     string
	ObservedTime time.Time
}

// LayoutReporter publishes a folder's resolved layout to the layer that owns GitTarget status.
//
// It is the twin of PathRefusalReporter, and it exists for the same structural reason: the
// scan happens on a branch-worker goroutine with no result channel back to the controller, so
// without a hook the resolution would be computed and dropped. The watch Manager supplies it
// (WorkerManager.SetLayoutReporter), which is where the projection onto status lives.
//
// Every scan reports, including one that changes nothing and one on a SUSPENDED target — a
// suspended target that publishes what it resolved is the whole point of suspend, and it can
// only do that if the report is independent of anything being written. The consumer is
// responsible for republishing only on a transition.
type LayoutReporter func(target itypes.ResourceReference, report LayoutReport)

// reportLayout resolves the layout over a batch's store and publishes it.
//
// The store is the same one the batch resolves placements against, so the reported layout is
// the one this write actually used rather than a second opinion computed from a second scan.
// An unattributable batch (either half of the target reference empty — the CLI, and tests)
// resolves nothing and publishes nothing: the projection is keyed by "namespace/name", so an
// empty half would file the report under a key no GitTarget reads.
func (w *BranchWorker) reportLayout(ctx context.Context, batch *writeBatch, revision string) {
	if w.layoutReporter == nil || batch.target.name == "" || batch.target.namespace == "" {
		return
	}
	// Re-resolved with the target's declared types so the EXAMPLES illustrate them; the verdict
	// is identical to batch.layout's, which is resolved without them at construction.
	resolution := manifestanalyzer.ResolveLayout(
		batch.store, batch.policy, batch.writeSubdir, declaredTypeKeys(batch.policy))
	log.FromContext(ctx).V(1).Info("GitTarget layout resolved",
		"gitTarget", batch.target.namespace+"/"+batch.target.name,
		"reason", resolution.Reason, "renderRoot", resolution.RenderRoot, "revision", revision)
	w.layoutReporter(
		itypes.NewResourceReference(batch.target.name, batch.target.namespace),
		LayoutReport{
			LayoutResolution: resolution,
			ByTypeEntries:    byTypeEntryCount(batch.policy),
			Revision:         revision,
			ObservedTime:     time.Now(),
		})
}

// scanLayout resolves a folder's layout, publishes it, and arms the batch with the verdict.
//
// It never refuses on its own. Ambiguity is a PLACEMENT problem — a new document has no single
// root to go into — and an existing document is edited where it already lives, whatever the
// folder covers. Refusing the whole flush here would also pre-empt the file-level write-boundary
// preconditions (L1 and L2), whose messages name the offending file rather than the folder, so
// the specific answer would be replaced by a general one. The refusal is raised in createNew,
// where the problem actually is.
func (w *BranchWorker) scanLayout(ctx context.Context, batch *writeBatch, worktree *gogit.Worktree) {
	w.reportLayout(ctx, batch, worktreeRevision(worktree))
}

// declaredTypeKeys is which types the placement examples illustrate: the ones the target
// actually declared a template for, in a stable order.
//
// A declared type is the interesting one — it is the answer a user asked for and the one they
// can get wrong — and ordering by key rather than by map iteration keeps the published stanza
// from churning between reconciles for no reason. With no declared types ResolveLayout falls
// back to illustrating the fallback rung, which is the whole answer for that folder anyway.
func declaredTypeKeys(policy *manifestanalyzer.PlacementPolicy) []string {
	if policy == nil {
		return nil
	}
	keys := make([]string, 0, len(policy.ByType))
	for key := range policy.ByType {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func byTypeEntryCount(policy *manifestanalyzer.PlacementPolicy) int {
	if policy == nil {
		return 0
	}
	return len(policy.ByType)
}

// worktreeRevision is the commit the scan read, or "" when the branch has no commit yet (a
// freshly bootstrapped folder). It is read from the worktree rather than from the worker's
// cached remote metadata, because the cached SHA describes the REMOTE branch and the scan
// describes what is on disk — and a placement stanza that claims a revision it did not read is
// worse than one that claims none.
func worktreeRevision(worktree *gogit.Worktree) string {
	repo, err := gogit.PlainOpen(worktree.Filesystem().Root())
	if err != nil {
		return ""
	}
	head, err := repo.Head()
	if err != nil {
		return ""
	}
	return head.Hash().String()
}
