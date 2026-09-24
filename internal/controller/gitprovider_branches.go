// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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
// A GitProvider is referenced only from its own namespace, so the list is namespace-scoped.
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
	if len(counts) == 0 {
		return nil
	}
	branches := make([]configbutleraiv1alpha3.GitProviderBranchStatus, 0, len(counts))
	for name, count := range counts {
		branches = append(branches, configbutleraiv1alpha3.GitProviderBranchStatus{Name: name, GitTargets: count})
	}
	sort.Slice(branches, func(i, j int) bool { return branches[i].Name < branches[j].Name })
	return branches
}

// gitTargetToGitProvider maps a GitTarget event to the GitProvider it names, so the inventory
// follows the configuration instead of lagging up to one steady interval behind it.
//
// The GitProvider's own For() takes GenerationChangedPredicate, which is right for the
// connectivity check — nothing about a GitTarget changes whether the credential works — but it
// means nothing else would re-run this reconcile when a GitTarget is created, deleted, or
// repointed at another branch.
func (r *GitProviderReconciler) gitTargetToGitProvider(_ context.Context, obj client.Object) []reconcile.Request {
	target, ok := obj.(*configbutleraiv1alpha3.GitTarget)
	if !ok || target.Spec.GitProviderRef.Name == "" {
		return nil
	}
	return []reconcile.Request{{
		NamespacedName: client.ObjectKey{Namespace: target.Namespace, Name: target.Spec.GitProviderRef.Name},
	}}
}

// gitTargetInventoryChanged passes the GitTarget events that can move the inventory: a create, a
// delete, or a spec change that names another provider or another branch. Status-only updates —
// which every GitTarget writes on its own tick, several per minute across a busy namespace — move
// nothing here, and a GenerationChangedPredicate would have let the deletion timestamp through
// unexamined while still admitting every unrelated spec edit.
func gitTargetInventoryChanged() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc:  func(event.CreateEvent) bool { return true },
		DeleteFunc:  func(event.DeleteEvent) bool { return true },
		GenericFunc: func(event.GenericEvent) bool { return false },
		UpdateFunc: func(e event.UpdateEvent) bool {
			before, ok1 := e.ObjectOld.(*configbutleraiv1alpha3.GitTarget)
			after, ok2 := e.ObjectNew.(*configbutleraiv1alpha3.GitTarget)
			if !ok1 || !ok2 {
				return true
			}
			return before.Spec.GitProviderRef.Name != after.Spec.GitProviderRef.Name ||
				before.Spec.Branch != after.Spec.Branch ||
				before.DeletionTimestamp.IsZero() != after.DeletionTimestamp.IsZero()
		},
	}
}
