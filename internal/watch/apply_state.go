// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"context"

	"github.com/ConfigButler/gitops-reverser/internal/git"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// The watch side's half of docs/design/applier-echoes.md: the branch worker judges a snapshot by
// what its appliers were doing when the snapshot's collection started (F3), and asks no applier for
// anything before a GitTarget's initial snapshots are in Git (S2). Both need what only the streams
// know.

// applyStateAtCollectionStart reads the ApplyState of the GitTarget's branch worker, for a snapshot
// whose collection is about to start. Nil without a router, or when the worker cannot be resolved:
// the worker then judges the snapshot only by its state on arrival.
func (m *Manager) applyStateAtCollectionStart(ctx context.Context, gitDest types.ResourceReference) *git.ApplyState {
	if m.EventRouter == nil {
		return nil
	}
	return m.EventRouter.applyState(ctx, gitDest)
}

// InitialSnapshotsApplied reports whether every collection the GitTarget's current plan watches has
// had a snapshot applied by the branch worker (markSnapshotApplied). A GitTarget declared with no
// collection has none to apply. One not declared yet, or whose plan holds a collection no snapshot
// was applied for, has not applied them. The worker adds that it holds no resync of the GitTarget
// still to publish.
//
// It is tracked per collection, not read from the stream's state: a stream streams once the worker
// took its snapshot, which may still be discarded, and a GitTarget declared before its WatchRule's
// collection is planned would answer for a collection it does not know of yet.
func (m *Manager) InitialSnapshotsApplied(gitDest types.ResourceReference) bool {
	table, ok := m.watchedTypeTableForGitDest(gitDest)
	if !ok {
		return m.declared(gitDest)
	}
	return m.snapshotsApplied(gitDest, collectionsForWatchKeys(targetWatchKeys(table)))
}

// markSnapshotApplied records that the branch worker applied a snapshot of the collection: a later
// one of it is no longer initial (git.ResyncRequest.Initial).
func (m *Manager) markSnapshotApplied(gitDest types.ResourceReference, collection types.CollectionKey) {
	m.snapshotsMu.Lock()
	defer m.snapshotsMu.Unlock()
	if m.appliedSnapshots == nil {
		m.appliedSnapshots = map[string]map[types.CollectionKey]bool{}
	}
	applied := m.appliedSnapshots[gitDest.Key()]
	if applied == nil {
		applied = map[types.CollectionKey]bool{}
		m.appliedSnapshots[gitDest.Key()] = applied
	}
	applied[collection] = true
}

// snapshotsApplied reports whether a snapshot was applied for every one of the collections.
func (m *Manager) snapshotsApplied(gitDest types.ResourceReference, collections []types.CollectionKey) bool {
	m.snapshotsMu.Lock()
	defer m.snapshotsMu.Unlock()
	applied := m.appliedSnapshots[gitDest.Key()]
	for _, collection := range collections {
		if !applied[collection] {
			return false
		}
	}
	return true
}

// forgetAppliedSnapshots drops a torn-down GitTarget's record, so a same-name recreation starts with
// initial snapshots again.
func (m *Manager) forgetAppliedSnapshots(gitDest types.ResourceReference) {
	m.snapshotsMu.Lock()
	defer m.snapshotsMu.Unlock()
	delete(m.appliedSnapshots, gitDest.Key())
}

// declared reports whether the GitTarget has declared itself to the watch plane.
func (m *Manager) declared(gitDest types.ResourceReference) bool {
	t := m.triggers()
	t.mu.Lock()
	defer t.mu.Unlock()
	_, declared := t.declares[gitDest.Key()]
	return declared
}
