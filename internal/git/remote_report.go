// SPDX-License-Identifier: Apache-2.0

package git

import (
	itypes "github.com/ConfigButler/gitops-reverser/internal/types"
)

// RemoteReporter publishes a confirmed observation of a branch's remote state to the layer that
// owns GitTarget status.
//
// It is the twin of LayoutReporter, and it exists for the same structural reason: the observation
// is made on a branch-worker goroutine with no result channel back to the controller, so without
// a hook it would be learned and dropped. The watch Manager supplies it
// (WorkerManager.SetRemoteReporter), which is where the projection onto status lives.
//
// One difference from layouts is worth stating, because it is the only place this design could
// have grown a registry. A worker serves EVERY GitTarget on its (provider, branch), while the
// observation is about the branch; the worker keeps no list of the targets it serves, and does
// not need one. It reports only against a target already in hand — the ones a push's writes
// named, or the one a refresh request names — and every target reaches the second case on its own
// reconcile tick, so nothing has to be enumerated and no target goes unreported.
type RemoteReporter func(target itypes.ResourceReference, observed RemoteObservation)

// reportRemoteObservation publishes one observation against every GitTarget named by the writes
// that produced it.
//
// An unattributable write (either half of the reference empty — the CLI, and tests) publishes
// nothing: the projection is keyed by "namespace/name", so an empty half would file the report
// under a key no GitTarget reads.
func (w *BranchWorker) reportRemoteObservation(targets []itypes.ResourceReference, observed RemoteObservation) {
	if w.remoteReporter == nil {
		return
	}
	for _, target := range targets {
		if target.Name == "" || target.Namespace == "" {
			continue
		}
		w.remoteReporter(target, observed)
	}
}

// pendingWriteTargets is the distinct set of GitTargets a batch of pending writes was written
// for, in a stable order.
func pendingWriteTargets(writes []PendingWrite) []itypes.ResourceReference {
	seen := map[string]struct{}{}
	targets := make([]itypes.ResourceReference, 0, len(writes))
	add := func(name, namespace string) {
		if name == "" || namespace == "" {
			return
		}
		ref := itypes.NewResourceReference(name, namespace)
		if _, had := seen[ref.Key()]; had {
			return
		}
		seen[ref.Key()] = struct{}{}
		targets = append(targets, ref)
	}
	for _, write := range writes {
		add(write.GitTargetName, write.GitTargetNamespace)
		for key := range write.Targets {
			add(key.Name, key.Namespace)
		}
	}
	return targets
}
