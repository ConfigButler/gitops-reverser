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

// InitialSnapshotsSettled reports whether every collection the GitTarget's current plan watches has
// a settled snapshot: the branch worker applied, refused or failed one of it (markSnapshotSettled).
// The worker adds that it holds no resync of the GitTarget still to publish.
//
// A GitTarget with no planned collection has not settled them: its WatchRule may not be resolved
// yet (a CRD not discovered, a rule not reconciled), and its folder holds nothing of the cluster. It
// holds its applier back as a Blocked watch does, rather than let it apply a folder nothing has
// mirrored into.
//
// It is tracked per collection, not read from the stream's state: a stream streams once the worker
// took its snapshot, which may still be discarded.
func (m *Manager) InitialSnapshotsSettled(gitDest types.ResourceReference) bool {
	table, ok := m.watchedTypeTableForGitDest(gitDest)
	if !ok {
		return false
	}
	collections := collectionsForWatchKeys(targetWatchKeys(table))
	return len(collections) > 0 && m.snapshotsSettled(gitDest, collections)
}

// markSnapshotSettled records that a snapshot of the collection settled: a later one of it is no
// longer initial (git.ResyncRequest.Initial).
func (m *Manager) markSnapshotSettled(gitDest types.ResourceReference, collection types.CollectionKey) {
	m.snapshotsMu.Lock()
	defer m.snapshotsMu.Unlock()
	if m.settledSnapshots == nil {
		m.settledSnapshots = map[string]map[types.CollectionKey]bool{}
	}
	settled := m.settledSnapshots[gitDest.Key()]
	if settled == nil {
		settled = map[types.CollectionKey]bool{}
		m.settledSnapshots[gitDest.Key()] = settled
	}
	settled[collection] = true
}

// snapshotsSettled reports whether a snapshot of every one of the collections settled.
func (m *Manager) snapshotsSettled(gitDest types.ResourceReference, collections []types.CollectionKey) bool {
	m.snapshotsMu.Lock()
	defer m.snapshotsMu.Unlock()
	settled := m.settledSnapshots[gitDest.Key()]
	for _, collection := range collections {
		if !settled[collection] {
			return false
		}
	}
	return true
}

// forgetSettledSnapshots drops a torn-down GitTarget's record, so a same-name recreation starts with
// initial snapshots again.
func (m *Manager) forgetSettledSnapshots(gitDest types.ResourceReference) {
	m.snapshotsMu.Lock()
	defer m.snapshotsMu.Unlock()
	delete(m.settledSnapshots, gitDest.Key())
}
