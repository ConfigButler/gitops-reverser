// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"time"

	v1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// DeclareForGitTarget submits the GitTarget's declaration to the watch-plane owner and returns.
// It does no work on the caller's goroutine.
//
// clusterID is (api/v1alpha3).GitTarget.SourceCluster() — the referenced ClusterProvider's name,
// "default" for the cluster the operator runs in. It is captured with the UID and the audit
// route, because none of the three can be derived from the rule-derived watch tables the owner
// plans from.
//
// pruneMode is (api/v1alpha3).GitTarget.EffectivePruneMode(). Unlike the other two it is mutable,
// and widening it to a sweeping mode forces a fresh replay — see prune_declaration.go for why the
// edge, and only that edge, has to be the trigger.
//
// It returns no error, and that is the contract: the pass runs after the target has been quiet
// for the settle window, so there is no result to hand back here. The controller reads
// DeclareStatusForGitTarget for whether the declaration has landed and how the last pass ended.
// A GitTarget applied together with its WatchRules is ONE piece of configuration, and waiting for
// the silence window is what makes the first plan it declares the one the user wrote rather than
// an empty one superseded four times as each rule arrives.
func (m *Manager) DeclareForGitTarget(
	gitDest types.ResourceReference,
	clusterID string,
	auditRoute string,
	pruneMode v1alpha3.PruneMode,
	forceRecheck ...bool,
) {
	force := len(forceRecheck) > 0 && forceRecheck[0]
	m.declareIntentFor(gitDest, clusterID, auditRoute, pruneMode, force)
}

// RequestRecheckForGitTarget forces this target's next pass to re-anchor its streams, which is
// what makes each one replay and drive the mark-and-sweep that rebuilds its folder.
//
// It exists for the branch worker replacement. The declaration is level-triggered and its inputs
// do not change when the repository underneath it does, so without this the pass is a no-op: the
// streams keep running, nothing replays, and an idle target would leave the new repository empty
// indefinitely. The force flag is the same one a refused Git path raises, and it is sticky on the
// intent, so it survives until a pass actually consumes it.
//
// A target that has not declared yet needs nothing: its first pass starts its streams, and a
// stream that starts replays.
func (m *Manager) RequestRecheckForGitTarget(gitDest types.ResourceReference) {
	t := m.triggers()
	t.mu.Lock()
	prior, declared := t.declares[gitDest.Key()]
	if declared {
		prior.forceRequests++
		t.markDirtyLocked(gitDest, TriggerReasonWorkerReplaced, time.Now())
	}
	t.mu.Unlock()
	if !declared {
		return
	}
	t.signal()
	// The controller owns status, and what it reads back about this target (its render fidelity,
	// its remote revision) was proved against the repository that is no longer there.
	m.enqueueGitTargetReconcile(gitDest)
}

// ForcedRecheckTargetsForTest lists the declared GitTargets whose next pass will re-anchor their
// streams. It exists so a test in another package can assert which targets a recovery reached;
// nothing in production calls it.
func (m *Manager) ForcedRecheckTargetsForTest() []string {
	t := m.triggers()
	t.mu.Lock()
	defer t.mu.Unlock()
	forced := make([]string, 0, len(t.declares))
	for key, intent := range t.declares {
		if intent.forcePending() {
			forced = append(forced, key)
		}
	}
	return forced
}

// ForgetGitTargetDeclaration drops in-memory watch state for a deleted GitTarget, and tears down
// its source cluster's context when it was the last GitTarget mirroring from it.
//
// The pending trigger is dropped synchronously, in the same step that queues the teardown: a
// trigger that outlived the delete would have the next pass re-create what the delete tore down.
// The teardown itself carries the UID it was issued for, so a GitTarget deleted and recreated
// under the same namespace and name cannot have its successor's watches torn down by its
// predecessor's delete.
func (m *Manager) ForgetGitTargetDeclaration(gitDest types.ResourceReference) {
	m.forgetIntent(gitDest)
}

// tearDownGitTarget is the owner's side of a deletion: stop the streams, drop every projection,
// and release the source cluster if this was the last GitTarget mirroring from it.
func (m *Manager) tearDownGitTarget(gitDest types.ResourceReference) {
	m.forgetGitTargetWatches(gitDest)
	m.forgetGitTargetUID(gitDest)
	m.forgetGitTargetCluster(gitDest)
	m.forgetGitTargetPruneMode(gitDest)
	m.mutateWatchPlane(func(s *watchPlaneState) bool {
		_, hadPass := s.passes[gitDest.Key()]
		_, hadLayout := s.layouts[gitDest.Key()]
		_, hadRemote := s.remotes[gitDest.Key()]
		if !hadPass && !hadLayout && !hadRemote {
			return false
		}
		delete(s.passes, gitDest.Key())
		delete(s.layouts, gitDest.Key())
		delete(s.remotes, gitDest.Key())
		return true
	})
}
