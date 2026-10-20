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

// InitialSnapshotsDelivered reports whether every collection the GitTarget's current plan watches has
// delivered its snapshot to the branch worker: its stream is Streaming, which it reaches only once the
// worker took its snapshot. A GitTarget declared with no collection has none to deliver. One not
// declared yet, or whose plan holds a collection not streaming, has not delivered them.
//
// The branch worker latches a GitTarget once this holds and it has published every resync it holds
// for it, so a later replay does not take the answer back.
func (m *Manager) InitialSnapshotsDelivered(gitDest types.ResourceReference) bool {
	table, ok := m.watchedTypeTableForGitDest(gitDest)
	if !ok {
		return m.declared(gitDest)
	}
	return snapshotsDelivered(collectionsForWatchKeys(targetWatchKeys(table)), m.watchPlane().streams[gitDest.Key()])
}

// snapshotsDelivered reports whether every expected collection's stream is Streaming.
func snapshotsDelivered(expected []types.CollectionKey, states map[types.CollectionKey]targetStreamStatus) bool {
	for _, collection := range deduplicateCollections(expected) {
		if status, seen := states[collection]; !seen || status.state != StreamStateStreaming {
			return false
		}
	}
	return true
}

// declared reports whether the GitTarget has declared itself to the watch plane.
func (m *Manager) declared(gitDest types.ResourceReference) bool {
	t := m.triggers()
	t.mu.Lock()
	defer t.mu.Unlock()
	_, declared := t.declares[gitDest.Key()]
	return declared
}
