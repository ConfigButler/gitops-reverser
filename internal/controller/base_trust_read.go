// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"sync"
	"time"

	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// baseTrustReadTracker remembers, per GitTarget, when it last forced a re-read of its own folder
// and which base-trust epoch that re-read answered.
//
// It exists because a branch worker is shared by every GitTarget on its (provider, branch) while
// the trust it guards is one boolean with one timestamp. Two separate things can go stale, and
// only one of them is the worker's:
//
//   - The CHECKOUT. It expires on the shared timestamp, which every successful push renews. The
//     epoch counts those expiries, and the epoch arm below fans one expiry out to every target on
//     the worker — without it, the first target to reconcile consumed the transition and its
//     siblings saw an already-cleared flag.
//   - The TARGET's own observation: what its folder holds, what the acceptance gate says about it,
//     where its documents are placed. A fetch a sibling earned updates the checkout and says
//     nothing about this. A review found the gap that leaves: one busy target publishing inside
//     the maximum age renews the shared timestamp forever, so the expiry never fires, so a quiet
//     target beside it is never re-evaluated — indefinitely postponed rather than merely late.
//     The age arm below bounds that directly, per target, whether or not anything expired.
//
// It is the same shape as reconcileRequestTracker, and deliberately so: both turn a signal into
// at-most-once work per object.
type baseTrustReadTracker struct {
	mu   sync.Mutex
	seen map[string]baseTrustRead
}

// baseTrustRead is one target's last forced re-read: the epoch it answered, and when it happened.
type baseTrustRead struct {
	epoch uint64
	at    time.Time
}

// take reports whether this target owes a folder re-read — because the shared checkout's trust
// expired on an epoch it has not acted on, or because its own last re-read is older than maxAge —
// and records it when it does.
//
// A target seen for the first time starts its clock at now rather than at the zero time, so a
// freshly created GitTarget does not force a re-read on top of the read it is about to do anyway.
// It is still owed the current epoch, which is what a sibling arriving after an expiry needs.
func (t *baseTrustReadTracker) take(
	target types.ResourceReference, epoch uint64, maxAge time.Duration, now time.Time,
) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seen == nil {
		t.seen = map[string]baseTrustRead{}
	}
	key := target.String()
	last, known := t.seen[key]
	if !known {
		last = baseTrustRead{at: now}
		t.seen[key] = last
	}
	expired := epoch > last.epoch
	stale := maxAge > 0 && now.Sub(last.at) >= maxAge
	if !expired && !stale {
		return false
	}
	t.seen[key] = baseTrustRead{epoch: epoch, at: now}
	return true
}

// forget drops a target's record when it goes away, so the map does not grow with every GitTarget
// the cluster has ever had.
func (t *baseTrustReadTracker) forget(target types.ResourceReference) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.seen, target.String())
}
