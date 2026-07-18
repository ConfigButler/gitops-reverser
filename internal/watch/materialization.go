// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// DeclareForGitTarget ensures the GitTarget's watch-first data plane is running against the
// source cluster it mirrors from. clusterID is (api/v1alpha3).GitTarget.SourceCluster() — the
// referenced ClusterProvider's name, "default" for the cluster the operator runs in. It is
// captured here, the same capture-on-Declare pattern as the UID: because spec.clusterProviderRef
// is immutable it is learned once and never changes, so there is no per-rule propagation and no
// cross-rule disagreement window.
func (m *Manager) DeclareForGitTarget(
	ctx context.Context,
	gitDest types.ResourceReference,
	clusterID string,
	forceRecheck ...bool,
) error {
	// Capture the UID and the source cluster before starting watches: the data plane keys its
	// resume cursors by GitTarget UID, and resolves rules/opens watches against the captured
	// cluster's context — neither of which the rule-derived watch tables carry.
	m.rememberGitTargetUID(gitDest)
	m.rememberGitTargetCluster(gitDest, clusterID)
	force := len(forceRecheck) > 0 && forceRecheck[0]
	if err := m.EnsureGitTargetWatches(ctx, gitDest, force); err != nil {
		m.Log.Info("watch-first declare skipped; surface not observable",
			"gitDest", gitDest.String(), "clusterID", describeCluster(clusterID), "err", err.Error())
		return err
	}
	return nil
}

// ForgetGitTargetDeclaration drops in-memory watch state for a deleted GitTarget, and tears
// down its source cluster's context when it was the last GitTarget mirroring from it.
func (m *Manager) ForgetGitTargetDeclaration(gitDest types.ResourceReference) {
	m.forgetGitTargetWatches(gitDest)
	m.forgetGitTargetUID(gitDest)
	m.forgetGitTargetCluster(gitDest)
	m.declaredGVRsMu.Lock()
	defer m.declaredGVRsMu.Unlock()
	delete(m.declaredGVRs, gitDest.String())
}
