// SPDX-License-Identifier: Apache-2.0

package git

import (
	"sort"
	"sync"
	"time"

	k8stypes "k8s.io/apimachinery/pkg/types"
)

// One GitBranch per repository branch.
//
// A branch worker is keyed by its repository and branch (BranchKey) and pushes with one credential,
// so exactly one GitBranch may write a repository branch: the one holding its claim. The rules are
// the plan's (docs/design/gitbranch-and-kubeprovider-plan.md):
//
//  1. A GitBranch becomes a candidate once its credential opens a receive-pack session on the
//     remote (ProvePushSession). An unproven GitBranch claims nothing, so creating one first
//     cannot block anybody.
//  2. Among the candidates for one key, the oldest by creationTimestamp, then UID, holds the claim.
//  3. The holder stays a candidate through a later credential or network failure, so it keeps the
//     claim and does not hand the branch to another GitBranch. Any other candidate whose proof
//     fails stops being one.
//  4. The claim lives here, in the manager's memory. After a restart it is recomputed by 1 and 2.
//  5. A holder giving the branch up hands it to nobody. Every other candidate becomes one again only
//     through its own GitBranch reconcile, which checks its grants before its proof, so a candidate
//     whose grant was withdrawn since it last reconciled never holds the branch, and never gets a
//     worker. The candidates waiting are stalled on BranchConflict, so they reconcile again soon on
//     their own; the GitBranch reconciler also wakes them at once (Release returns them).
//
// The GitBranch reconciler feeds it (Observe, Release); the GitTarget reconciler, the worker sweep
// and EnsureWorker read it, so they cannot disagree about who writes a branch. The grants are checked
// in one place only, the GitBranch reconcile, before it calls Observe.

// BranchClaimant is a GitBranch as the claim sees it: which object, and how old.
type BranchClaimant struct {
	GitBranch k8stypes.NamespacedName
	UID       k8stypes.UID
	Created   time.Time
}

// olderThan orders two claimants: creationTimestamp first, then UID, so every caller and every
// restart elects the same one.
func (c BranchClaimant) olderThan(other BranchClaimant) bool {
	if !c.Created.Equal(other.Created) {
		return c.Created.Before(other.Created)
	}
	return c.UID < other.UID
}

type claimEntry struct {
	claimant  BranchClaimant
	key       BranchKey
	candidate bool
}

// BranchClaims records which GitBranch holds each repository branch. Its zero value is not usable;
// use NewBranchClaims.
type BranchClaims struct {
	mu      sync.RWMutex
	entries map[k8stypes.UID]*claimEntry
	holders map[BranchKey]k8stypes.UID
}

// NewBranchClaims returns an empty claim registry.
func NewBranchClaims() *BranchClaims {
	return &BranchClaims{
		entries: map[k8stypes.UID]*claimEntry{},
		holders: map[BranchKey]k8stypes.UID{},
	}
}

// Observe records what a GitBranch's reconcile found: the key it names now and whether its proof
// succeeded. It returns who holds the key afterwards. A GitBranch that names another key than it
// did before (its GitProvider was recreated against another repository) gives up the old one first.
func (c *BranchClaims) Observe(key BranchKey, claimant BranchClaimant, proven bool) (BranchClaimant, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, known := c.entries[claimant.UID]
	if known && entry.key != key {
		// The GitBranches waiting for the old key are stalled on BranchConflict, and reconcile again
		// on their own.
		c.releaseLocked(claimant.UID)
		known = false
	}
	if !known {
		entry = &claimEntry{claimant: claimant, key: key}
		c.entries[claimant.UID] = entry
	}
	// Rule 3: the holder's candidacy survives a failed proof. Everyone else's follows the proof.
	if c.holders[key] != claimant.UID {
		entry.candidate = proven
	}
	c.electLocked(key)
	return c.holderLocked(key)
}

// Release forgets a GitBranch: it is being deleted, or no grant admits it any more. If it held its
// key, nobody holds the key afterwards (rule 5), and Release returns the GitBranches that wait for it,
// oldest first: they are candidates again once their own reconcile has checked their grants.
func (c *BranchClaims) Release(uid k8stypes.UID) []BranchClaimant {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.releaseLocked(uid)
}

func (c *BranchClaims) releaseLocked(uid k8stypes.UID) []BranchClaimant {
	entry, known := c.entries[uid]
	if !known {
		return nil
	}
	delete(c.entries, uid)
	if c.holders[entry.key] != uid {
		return nil
	}
	delete(c.holders, entry.key)
	var waiting []BranchClaimant
	for _, other := range c.entries {
		if other.key == entry.key {
			other.candidate = false
			waiting = append(waiting, other.claimant)
		}
	}
	sort.Slice(waiting, func(i, j int) bool { return waiting[i].olderThan(waiting[j]) })
	return waiting
}

// electLocked applies rule 2: the oldest candidate for key holds it. A holder is always a candidate,
// so an older GitBranch that proves itself later takes the key over; that is the rule, and it is
// what makes the outcome the same after a restart.
func (c *BranchClaims) electLocked(key BranchKey) {
	var oldest *claimEntry
	for _, entry := range c.entries {
		if entry.key != key || !entry.candidate {
			continue
		}
		if oldest == nil || entry.claimant.olderThan(oldest.claimant) {
			oldest = entry
		}
	}
	if oldest == nil {
		delete(c.holders, key)
		return
	}
	c.holders[key] = oldest.claimant.UID
}

// Holder returns the GitBranch holding key.
func (c *BranchClaims) Holder(key BranchKey) (BranchClaimant, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.holderLocked(key)
}

func (c *BranchClaims) holderLocked(key BranchKey) (BranchClaimant, bool) {
	uid, held := c.holders[key]
	if !held {
		return BranchClaimant{}, false
	}
	return c.entries[uid].claimant, true
}

// HeldBy returns the key the named GitBranch holds, if it holds one. GitTargets reference a
// GitBranch by name; a recreated object with that name is a new UID, and it holds nothing until it
// has proved itself (Observe), because Release forgot the old one before its finalizer went.
func (c *BranchClaims) HeldBy(gitBranch k8stypes.NamespacedName) (BranchKey, BranchClaimant, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for key, uid := range c.holders {
		if claimant := c.entries[uid].claimant; claimant.GitBranch == gitBranch {
			return key, claimant, true
		}
	}
	return BranchKey{}, BranchClaimant{}, false
}
