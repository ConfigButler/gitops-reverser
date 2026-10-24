// SPDX-License-Identifier: Apache-2.0

package controller

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	configbutleraiv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
)

// logDependencyListError records a List failure from inside a cross-kind watch
// map function. Those map functions intentionally degrade to "enqueue nothing"
// on error — the affected resources still recover on their periodic requeue —
// so without this log the failure would be entirely invisible.
func logDependencyListError(ctx context.Context, err error, listKind string, trigger client.Object) {
	logf.FromContext(ctx).Error(err,
		"dependency watch: failed to list "+listKind+" for a dependency event; "+
			"affected resources will recover on the periodic requeue",
		"trigger", client.ObjectKeyFromObject(trigger))
}

// GitBranchProviderIndex indexes GitBranches by the GitProvider they claim through, its namespace
// resolved (namespace/name). A GitBranch may name a GitProvider in another namespace, so the
// GitBranches of a provider cannot be found by listing the provider's namespace.
const GitBranchProviderIndex = "configbutler.ai/gitbranch-provider"

// IndexGitBranchProvider is the GitBranchProviderIndex value of a GitBranch.
func IndexGitBranchProvider(obj client.Object) []string {
	branch, ok := obj.(*configbutleraiv1alpha3.GitBranch)
	if !ok || branch.Spec.GitProviderRef.Name == "" {
		return nil
	}
	return []string{branch.GitProviderKey().String()}
}

// IndexGitBranches registers GitBranchProviderIndex. The GitBranch, GitTarget and applier claim
// controllers all find a provider's GitBranches through it, so it is registered once, before any of
// them is set up.
func IndexGitBranches(ctx context.Context, indexer client.FieldIndexer) error {
	return indexer.IndexField(ctx, &configbutleraiv1alpha3.GitBranch{}, GitBranchProviderIndex,
		IndexGitBranchProvider)
}

// gitBranchesOf lists the GitBranches, from any namespace, that claim through the GitProvider provider.
func gitBranchesOf(
	ctx context.Context, reader client.Reader, provider client.Object,
) ([]configbutleraiv1alpha3.GitBranch, error) {
	var list configbutleraiv1alpha3.GitBranchList
	if err := reader.List(ctx, &list,
		client.MatchingFields{GitBranchProviderIndex: client.ObjectKeyFromObject(provider).String()}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

// requestsFor is a reconcile request for each object in a list.
func requestsFor[T any, PT interface {
	*T
	client.Object
}](objects []T) []ctrlreconcile.Request {
	requests := make([]ctrlreconcile.Request, 0, len(objects))
	for i := range objects {
		requests = append(requests, ctrlreconcile.Request{NamespacedName: client.ObjectKeyFromObject(PT(&objects[i]))})
	}
	return requests
}

// ruleDependencyWatches adds the watches a WatchRule and a ClusterWatchRule share: their GitTarget,
// its KubeProvider and the Namespace whose labels its grants read.
//
//   - The rule MIRRORS the GitTarget's readiness into its own GitTargetReady, so the watch has to see
//     the status-only update that carries a heal; GenerationChangedPredicate would drop exactly that.
//     gitTargetReadyProjectionChanged fires on a spec change or on a move in the mirrored verdict.
//   - A KubeProvider's grants changing. The GitTarget edge cannot carry it: a provider change reaches
//     the GitTarget as a status update that need not move its readiness, so without this edge a
//     revocation would leave the compiled rule resident until the periodic requeue.
//   - A Namespace's labels changing. A Selector grant reads them, and a namespace matched by two grants
//     can lose the one granting a wider scope while its GitTarget stays admitted, which moves nothing
//     the GitTarget edge sees. LabelChangedPredicate ignores unrelated namespace churn.
func ruleDependencyWatches(
	b *builder.Builder, gitTargets, kubeProviders, namespaces handler.MapFunc,
) *builder.Builder {
	return b.
		Watches(&configbutleraiv1alpha3.GitTarget{}, handler.EnqueueRequestsFromMapFunc(gitTargets),
			builder.WithPredicates(gitTargetReadyProjectionChanged())).
		Watches(&configbutleraiv1alpha3.KubeProvider{}, handler.EnqueueRequestsFromMapFunc(kubeProviders),
			builder.WithPredicates(kubeProviderReadyOrSpecChanged())).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(namespaces),
			builder.WithPredicates(predicate.LabelChangedPredicate{}))
}
