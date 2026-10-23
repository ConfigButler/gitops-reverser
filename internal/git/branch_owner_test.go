// SPDX-License-Identifier: Apache-2.0

package git

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8stypes "k8s.io/apimachinery/pkg/types"
)

var claimEpoch = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// claimant is a GitBranch namespace/name created age ago, with a UID of its name.
func claimant(namespace, name string, age time.Duration) BranchClaimant {
	return BranchClaimant{
		GitBranch: k8stypes.NamespacedName{Namespace: namespace, Name: name},
		UID:       k8stypes.UID(namespace + "/" + name),
		Created:   claimEpoch.Add(-age),
	}
}

// TestBranchClaims_AnUnprovenGitBranchClaimsNothing is rule 1: a GitBranch with a bad credential,
// created first, does not stop a valid one created later from claiming the branch.
func TestBranchClaims_AnUnprovenGitBranchClaimsNothing(t *testing.T) {
	claims := NewBranchClaims()
	key := BranchKeyFor("https://github.com/acme/platform.git", "main")
	squatter := claimant("team-x", "main", 2*time.Hour)
	owner := claimant("platform", "main", time.Hour)

	_, held := claims.Observe(key, squatter, false)
	assert.False(t, held, "an unproven GitBranch holds nothing")

	holder, held := claims.Observe(key, owner, true)
	require.True(t, held)
	assert.Equal(t, owner, holder)
}

// TestBranchClaims_TheOldestCandidateHoldsTheBranch is rule 2, across two spellings of one
// repository: the younger GitBranch learns who holds it, whichever reconciles first.
func TestBranchClaims_TheOldestCandidateHoldsTheBranch(t *testing.T) {
	claims := NewBranchClaims()
	https := BranchKeyFor("https://github.com/acme/platform.git", "main")
	ssh := BranchKeyFor("git@github.com:acme/platform", "main")
	require.Equal(t, https, ssh, "one repository, spelled two ways, is one key")
	older := claimant("team-a", "main", 2*time.Hour)
	younger := claimant("team-b", "main", time.Hour)

	holder, _ := claims.Observe(ssh, younger, true)
	assert.Equal(t, younger, holder, "alone, the younger GitBranch holds the branch")
	holder, _ = claims.Observe(https, older, true)
	assert.Equal(t, older, holder, "an older candidate takes the branch over")
	holder, _ = claims.Observe(ssh, younger, true)
	assert.Equal(t, older, holder)

	other := BranchKeyFor("https://github.com/acme/platform.git", "release")
	holder, _ = claims.Observe(other, claimant("team-b", "release", time.Hour), true)
	assert.Equal(t, "release", holder.GitBranch.Name, "another branch is another claim")
}

// TestBranchClaims_TheHolderKeepsTheBranchThroughAFailedProof is rule 3: a holder whose credential
// starts failing keeps the branch, and does not hand it to a younger candidate. A candidate that is
// not the holder stops being one when its proof fails.
func TestBranchClaims_TheHolderKeepsTheBranchThroughAFailedProof(t *testing.T) {
	claims := NewBranchClaims()
	key := BranchKeyFor("https://github.com/acme/platform.git", "main")
	holder := claimant("platform", "main", 2*time.Hour)
	younger := claimant("team-b", "main", time.Hour)
	claims.Observe(key, holder, true)
	claims.Observe(key, younger, true)

	got, held := claims.Observe(key, holder, false)
	require.True(t, held)
	assert.Equal(t, holder, got, "the holder keeps the branch")

	claims.Observe(key, younger, false)
	claims.Release(holder.UID)
	_, held = claims.Holder(key)
	assert.False(t, held, "a candidate whose proof failed does not inherit the branch")

	got, held = claims.Observe(key, younger, true)
	require.True(t, held)
	assert.Equal(t, younger, got)
}

// TestBranchClaims_ReleaseHandsTheBranchToNobody is rule 5: a holder giving the branch up elects no
// successor. It returns the GitBranches waiting, oldest first, and the first of them to confirm again
// holds the branch; an older one confirming later takes it over by rule 2.
func TestBranchClaims_ReleaseHandsTheBranchToNobody(t *testing.T) {
	claims := NewBranchClaims()
	key := BranchKeyFor("https://github.com/acme/platform.git", "main")
	first := claimant("platform", "main", 3*time.Hour)
	second := claimant("team-a", "main", 2*time.Hour)
	third := claimant("team-b", "main", time.Hour)
	for _, c := range []BranchClaimant{third, first, second} {
		claims.Observe(key, c, true)
	}

	assert.Empty(t, claims.Release(third.UID), "a candidate that does not hold the branch hands nothing on")
	claims.Observe(key, third, true)

	waiting := claims.Release(first.UID)
	assert.Equal(t, []BranchClaimant{second, third}, waiting)
	_, held := claims.Holder(key)
	assert.False(t, held, "nobody holds the branch until a waiting GitBranch confirms itself")
	_, _, held = claims.HeldBy(first.GitBranch)
	assert.False(t, held, "a released GitBranch holds nothing")

	got, held := claims.Observe(key, third, true)
	require.True(t, held)
	assert.Equal(t, third, got)
	got, _ = claims.Observe(key, second, true)
	assert.Equal(t, second, got, "the older candidate takes the branch over once it confirms")
}

// TestBranchClaims_AGitBranchThatNamesAnotherRepositoryGivesTheOldOneUp: its GitProvider was
// recreated against another URL, so its claim moves with it and the old branch is free again.
func TestBranchClaims_AGitBranchThatNamesAnotherRepositoryGivesTheOldOneUp(t *testing.T) {
	claims := NewBranchClaims()
	before := BranchKeyFor("https://github.com/acme/old.git", "main")
	after := BranchKeyFor("https://github.com/acme/new.git", "main")
	branch := claimant("platform", "main", time.Hour)
	claims.Observe(before, branch, true)

	claims.Observe(after, branch, true)
	_, held := claims.Holder(before)
	assert.False(t, held)
	got, held := claims.Holder(after)
	require.True(t, held)
	assert.Equal(t, branch, got)
}

// TestBranchClaims_ARecreatedGitBranchStartsWithoutTheClaim: the UID holds the claim, so an object
// recreated under the same name holds nothing until its own proof.
func TestBranchClaims_ARecreatedGitBranchStartsWithoutTheClaim(t *testing.T) {
	claims := NewBranchClaims()
	key := BranchKeyFor("https://github.com/acme/platform.git", "main")
	old := claimant("platform", "main", time.Hour)
	claims.Observe(key, old, true)
	claims.Release(old.UID)

	recreated := old
	recreated.UID = "platform/main-2"
	_, _, held := claims.HeldBy(recreated.GitBranch)
	assert.False(t, held)
	got, held := claims.Observe(key, recreated, true)
	require.True(t, held)
	assert.Equal(t, recreated.UID, got.UID)
}
