// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// DeclareForGitTarget ensures the GitTarget's watch-first data plane is running against the
// source cluster it mirrors from. clusterID is (api/v1alpha3).GitTarget.SourceCluster() — the
// referenced ClusterProvider's name, "default" for the cluster the operator runs in. It is
// captured here, the same capture-on-Declare pattern as the UID: because spec.clusterProviderRef
// is immutable it is learned once and never changes, so there is no per-rule propagation and no
// cross-rule disagreement window.
//
// pruneMode is (api/v1alpha3).GitTarget.EffectivePruneMode(). Unlike the other two it is mutable,
// and widening it to a sweeping mode forces a fresh replay — see prune_declaration.go for why the
// edge, and only that edge, has to be the trigger.
func (m *Manager) DeclareForGitTarget(
	ctx context.Context,
	gitDest types.ResourceReference,
	clusterID string,
	pruneMode v1alpha3.PruneMode,
	forceRecheck ...bool,
) error {
	// Capture the UID and the source cluster before starting watches: the data plane keys its
	// resume cursors by GitTarget UID, and resolves rules/opens watches against the captured
	// cluster's context — neither of which the rule-derived watch tables carry.
	m.rememberGitTargetUID(gitDest)
	m.rememberGitTargetCluster(gitDest, clusterID)
	force := (len(forceRecheck) > 0 && forceRecheck[0]) || m.pruneModeRequiresReplay(gitDest, pruneMode)
	if err := m.EnsureGitTargetWatches(ctx, gitDest, force); err != nil {
		m.Log.Info("watch-first declare skipped; surface not observable",
			"gitDest", gitDest.String(), "clusterID", describeCluster(clusterID), "err", err.Error())
		return err
	}
	// Only once the watches are actually in place: a failed declare must leave the pending force
	// standing for the next reconcile rather than consuming it.
	m.rememberGitTargetPruneMode(gitDest, pruneMode)
	return nil
}

// ForgetGitTargetDeclaration drops in-memory watch state for a deleted GitTarget, and tears
// down its source cluster's context when it was the last GitTarget mirroring from it.
func (m *Manager) ForgetGitTargetDeclaration(gitDest types.ResourceReference) {
	m.forgetGitTargetWatches(gitDest)
	m.forgetGitTargetUID(gitDest)
	m.forgetGitTargetCluster(gitDest)
	m.forgetGitTargetPruneMode(gitDest)
	m.declaredGVRsMu.Lock()
	defer m.declaredGVRsMu.Unlock()
	delete(m.declaredGVRs, gitDest.String())
}
