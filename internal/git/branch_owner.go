// SPDX-License-Identifier: Apache-2.0

package git

import (
	k8stypes "k8s.io/apimachinery/pkg/types"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// One GitProvider per repository branch.
//
// A branch worker is keyed by its repository and branch (BranchKey), and pushes with one
// credential. So every GitTarget that writes one repository branch must name the same
// GitProvider; a GitTarget in another namespace reaches a shared one through
// spec.gitProviderRef.namespace, which the provider's spec.accessFrom admits. When GitTargets on
// one repository branch name different GitProviders anyway, the oldest GitTarget's GitProvider
// owns the branch, and the others are refused (TargetConflict) and are written by no worker. The
// GitTarget reconciler, the worker sweep and EnsureWorker all decide it here, so they cannot
// disagree.

// BranchOwner is the GitProvider that writes a repository branch, and the GitTarget that decided
// it: the oldest one writing that branch.
type BranchOwner struct {
	Provider k8stypes.NamespacedName
	Target   k8stypes.NamespacedName
}

// BranchOwners decides the owner of every repository branch the targets write. urlOf resolves a
// GitProvider to its spec.url; a GitTarget whose GitProvider it cannot resolve writes nothing yet,
// and decides nothing. A GitTarget being deleted decides nothing either, so the next one takes the
// branch over at once. Nor does one admitted reports refused (authz.GitTargetAdmitted: its
// ClusterProvider or its GitProvider does not admit its namespace): it may not write, so it must not
// keep the branch from a GitTarget that may. An error from admitted is returned, never read as a
// refusal, so a read that fails moves no branch.
func BranchOwners(
	targets []v1alpha3.GitTarget,
	urlOf func(provider k8stypes.NamespacedName) (string, bool),
	admitted func(target *v1alpha3.GitTarget) (bool, error),
) (map[BranchKey]BranchOwner, error) {
	owners := map[BranchKey]BranchOwner{}
	oldest := map[BranchKey]*v1alpha3.GitTarget{}
	for i := range targets {
		target := &targets[i]
		if !target.DeletionTimestamp.IsZero() {
			continue
		}
		url, ok := urlOf(target.GitProviderKey())
		if !ok {
			continue
		}
		allowed, err := admitted(target)
		if err != nil {
			return nil, err
		}
		if !allowed {
			continue
		}
		key := BranchKeyFor(url, target.Spec.Branch)
		if key == (BranchKey{}) {
			continue
		}
		if current, held := oldest[key]; held && !OlderGitTarget(target, current) {
			continue
		}
		oldest[key] = target
		owners[key] = BranchOwner{
			Provider: target.GitProviderKey(),
			Target:   k8stypes.NamespacedName{Namespace: target.Namespace, Name: target.Name},
		}
	}
	return owners, nil
}

// OlderGitTarget reports whether a was created before b, ties broken by namespace and name, so every
// caller orders two GitTargets the same way.
func OlderGitTarget(a, b *v1alpha3.GitTarget) bool {
	at, bt := a.CreationTimestamp.Time, b.CreationTimestamp.Time
	if !at.Equal(bt) {
		return at.Before(bt)
	}
	return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
}
