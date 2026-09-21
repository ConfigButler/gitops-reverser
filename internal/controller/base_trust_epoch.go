// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"sync"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// baseTrustEpochTracker remembers, per GitTarget, the base-trust epoch it last forced a re-read
// for.
//
// It exists because a branch worker is shared by every GitTarget on its (provider, branch) while
// the trust it guards is one boolean. Without this, the first target to reconcile after an expiry
// consumed the transition and its siblings never re-evaluated their own folders: they saw either
// an already-cleared flag or the fresh timestamp from that target's fetch. A quiet sibling beside
// a busy one could stay stale for as long as the busy one kept renewing the shared trust.
//
// It is the same shape as reconcileRequestTracker, and deliberately so: both turn one signal into
// at-most-once work per object.
type baseTrustEpochTracker struct {
	mu   sync.Mutex
	seen map[string]uint64
}

// take reports whether this target still owes a re-read for the given epoch, and records that it
// has been taken. It returns false for an epoch the target has already acted on, so a steady
// stream of reconciles does not force a fetch on every pass.
func (t *baseTrustEpochTracker) take(target types.ResourceReference, epoch uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen == nil {
		t.seen = map[string]uint64{}
	}
	key := target.String()
	if t.seen[key] >= epoch {
		return false
	}
	t.seen[key] = epoch
	return true
}

// forget drops a target's record when it goes away, so the map does not grow with every GitTarget
// the cluster has ever had.
func (t *baseTrustEpochTracker) forget(target types.ResourceReference) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.seen, target.String())
}
