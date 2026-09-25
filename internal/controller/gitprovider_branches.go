// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// publishBranchInventory writes status.branches: the branches this repository's GitTargets are
// configured to write, and how many reference each.
//
// It is a question about CONFIGURATION, so it is answered from the API and not from the data
// plane. A blocked or suspended GitTarget still says what the repository is for; a branch worker
// that happens to be running says only what has been reached so far, and a repository nothing has
// published to yet has no worker at all. That is the whole point of the field: "Ready=True with no
// branches" is configured-and-unused, which looks nothing like broken, and telling the two apart
// used to take a GitTarget listing.
//
// A GitProvider is referenced only from its own namespace, so the list is namespace-scoped, and
// it is read from the cache on the provider's own steady tick. There is deliberately no watch on
// GitTarget: this reconcile also proves the credential against the remote, so an edge per
// GitTarget edit would spend a network round trip and advance status.lastVerifiedAt to answer a
// question about configuration. The inventory lags a GitTarget edit by up to one steady interval,
// which is the same freshness every other configuration projection here has.
//
// A List that fails leaves what is published standing rather than emptying it: an inventory that
// collapses to nothing whenever the cache hiccups would report "configured and unused" about a
// repository with ten GitTargets on it.
func (r *GitProviderReconciler) publishBranchInventory(
	ctx context.Context,
	gitProvider *configbutleraiv1alpha3.GitProvider,
) error {
	var targets configbutleraiv1alpha3.GitTargetList
	if err := r.List(ctx, &targets, client.InNamespace(gitProvider.Namespace)); err != nil {
		return err
	}
	gitProvider.Status.Branches = branchInventory(gitProvider.Name, targets.Items)
	return nil
}

// branchInventory counts the live GitTargets that name provider, by branch, sorted by name.
//
// Sorting is not cosmetic. The status patch is computed against what is published, so an order
// that followed the list's arrival order would rewrite the field — and wake every watcher of the
// type — on a change that moved nothing.
func branchInventory(
	provider string,
	targets []configbutleraiv1alpha3.GitTarget,
) []configbutleraiv1alpha3.GitProviderBranchStatus {
	counts := map[string]int32{}
	for i := range targets {
		target := &targets[i]
		// A target being deleted is not what the repository is for any more: its worker is
		// retired by the sweep the delete drives, and counting it would leave a branch in the
		// inventory that nothing writes.
		if !target.DeletionTimestamp.IsZero() || target.Spec.GitProviderRef.Name != provider {
			continue
		}
		counts[target.Spec.Branch]++
	}
	// EMPTY rather than nil: "this repository is configured and unused" is a measurement, and a
	// nil slice would serialize the same way as "nothing has read the GitTargets yet", which is
	// what the field is absent for.
	branches := make([]configbutleraiv1alpha3.GitProviderBranchStatus, 0, len(counts))
	for name, count := range counts {
		branches = append(branches, configbutleraiv1alpha3.GitProviderBranchStatus{Name: name, GitTargetCount: count})
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].Name < branches[j].Name })
	return branches
}
