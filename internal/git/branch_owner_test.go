// SPDX-License-Identifier: Apache-2.0

package git

import (
	"strings"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// ownerTarget is a GitTarget in namespace writing main through the GitProvider provider
// ("namespace/name", or a bare name for its own namespace), created age ago.
func ownerTarget(namespace, name, provider string, age time.Duration) v1alpha3.GitTarget {
	ref := meta.NamespacedObjectReference{Name: provider}
	if ns, n, found := strings.Cut(provider, "/"); found {
		ref = meta.NamespacedObjectReference{Namespace: ns, Name: n}
	}
	return v1alpha3.GitTarget{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name,
			CreationTimestamp: metav1.NewTime(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC).Add(-age)),
		},
		Spec: v1alpha3.GitTargetSpec{GitProviderRef: ref, Branch: "main", Path: name},
	}
}

// urlsOf resolves the GitProviders the ownership tests name.
func urlsOf(urls map[string]string) func(k8stypes.NamespacedName) (string, bool) {
	return func(provider k8stypes.NamespacedName) (string, bool) {
		url, ok := urls[provider.String()]
		return url, ok
	}
}

// TestBranchOwners_TheOldestGitTargetsGitProviderOwnsTheBranch: GitTargets on one repository branch
// through two GitProviders, spelled two ways. One worker writes the branch, with one credential, so
// the oldest GitTarget decides whose; a GitTarget sharing that GitProvider from another namespace is
// on the owner's side.
func TestBranchOwners_TheOldestGitTargetsGitProviderOwnsTheBranch(t *testing.T) {
	urls := urlsOf(map[string]string{
		"team-a/monorepo": "https://github.com/acme/platform.git",
		"team-b/monorepo": "git@github.com:acme/platform",
	})
	targets := []v1alpha3.GitTarget{
		ownerTarget("team-b", "late", "monorepo", time.Hour),
		ownerTarget("team-a", "first", "monorepo", 2*time.Hour),
		ownerTarget("team-c", "shared", "team-a/monorepo", 3*time.Minute),
	}

	owners := BranchOwners(targets, urls)

	key := BranchKeyFor("https://github.com/acme/platform", "main")
	assert.Len(t, owners, 1, "two spellings of one repository are one branch")
	assert.Equal(t, BranchOwner{
		Provider: k8stypes.NamespacedName{Namespace: "team-a", Name: "monorepo"},
		Target:   k8stypes.NamespacedName{Namespace: "team-a", Name: "first"},
	}, owners[key])
}

// TestBranchOwners_ADeletedOrUnresolvedGitTargetDecidesNothing: a GitTarget being deleted hands the
// branch to the next one at once, and one whose GitProvider cannot be read writes nothing yet.
func TestBranchOwners_ADeletedOrUnresolvedGitTargetDecidesNothing(t *testing.T) {
	urls := urlsOf(map[string]string{
		"team-a/monorepo": "https://github.com/acme/platform",
		"team-b/monorepo": "https://github.com/acme/platform",
	})
	leaving := ownerTarget("team-a", "leaving", "monorepo", 2*time.Hour)
	leaving.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	targets := []v1alpha3.GitTarget{
		leaving,
		ownerTarget("team-x", "unread", "gone", 3*time.Hour),
		ownerTarget("team-b", "next", "monorepo", time.Hour),
	}

	owner := BranchOwners(targets, urls)[BranchKeyFor("https://github.com/acme/platform", "main")]

	assert.Equal(t, k8stypes.NamespacedName{Namespace: "team-b", Name: "monorepo"}, owner.Provider)
}

// TestOlderGitTarget_BreaksATieByIdentity: two GitTargets created in the same second are still
// ordered, the same way by every caller.
func TestOlderGitTarget_BreaksATieByIdentity(t *testing.T) {
	a := ownerTarget("team-a", "x", "p", time.Hour)
	b := ownerTarget("team-b", "x", "p", time.Hour)
	assert.True(t, OlderGitTarget(&a, &b))
	assert.False(t, OlderGitTarget(&b, &a))
	assert.False(t, OlderGitTarget(&a, &a))
}
