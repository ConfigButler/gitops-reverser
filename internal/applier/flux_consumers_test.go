// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"testing"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/applier/fluxsim"
	"github.com/ConfigButler/gitops-reverser/internal/git"
)

// helmReleaseObj is a HelmRelease whose chart comes from the GitRepository sourceName, in its
// namespace unless sourceNamespace is set.
func helmReleaseObj(namespace, name, sourceName, sourceNamespace string) *unstructured.Unstructured {
	sourceRef := map[string]any{"kind": kindGitRepository, "name": sourceName}
	if sourceNamespace != "" {
		sourceRef["namespace"] = sourceNamespace
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": kindHelmRelease,
		"metadata": map[string]any{"namespace": namespace, "name": name},
		"spec": map[string]any{"chart": map[string]any{"spec": map[string]any{
			"chart": "./charts/app", "sourceRef": sourceRef,
		}}},
	}}
}

// TestFluxConsumers_ListsEveryObjectApplyingTheSource: Kustomizations and HelmReleases, their source
// namespace defaulted to their own, and nothing that applies another source.
func TestFluxConsumers_ListsEveryObjectApplyingTheSource(t *testing.T) {
	client := newSourceClient(
		kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")),
		kustomizationObj("apps", "elsewhere", gitRepoSourceRef("platform")),
		kustomizationObj("apps", "across", map[string]any{
			"kind": kindGitRepository, "name": "platform", "namespace": fluxSystemNS,
		}),
		kustomizationObj(fluxSystemNS, "other", gitRepoSourceRef("another")),
		helmReleaseObj("apps", "podinfo", "platform", fluxSystemNS),
		helmReleaseObj(fluxSystemNS, "local", "platform", ""),
	)
	consumers, err := pinnedFlux(client).Consumers(context.Background(), platformKey)
	require.NoError(t, err)
	assert.Equal(t, []fluxmeta.NamespacedObjectKindReference{
		{Kind: kindHelmRelease, Namespace: "apps", Name: "podinfo"},
		{Kind: kindHelmRelease, Namespace: fluxSystemNS, Name: "local"},
		{Kind: kindKustomization, Namespace: "apps", Name: "across"},
		{Kind: kindKustomization, Namespace: fluxSystemNS, Name: "checkout"},
	}, consumers)
}

// TestFluxProbe_ASharedSourceAppliesOnlyTheConsumerAsked is the third review's third finding: A and
// B share a GitRepository, and A's request fetched it, which woke B too, outside any window. With
// both suspended, A's fetch and apply leave B alone: an edit to B's objects keeps its value.
func TestFluxProbe_ASharedSourceAppliesOnlyTheConsumerAsked(t *testing.T) {
	a := fluxsim.OwnSuspend(t, kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform")), true,
		SuspendFieldManager, metav1.ManagedFieldsOperationApply)
	b := fluxsim.OwnSuspend(t, kustomizationObj(fluxSystemNS, "infra", gitRepoSourceRef("platform")), true,
		SuspendFieldManager, metav1.ManagedFieldsOperationApply)
	sim := fluxsim.New(t, newSourceClient(a, b,
		gitRepositoryObj(fluxSystemNS, "platform", testRepoURL, branchRef(testBranch), "main@sha1:"+olderCommit)))
	sim.Fetches(platformKey, "main@sha1:"+testCommit)
	flux := simFlux(sim)

	stamped, err := flux.Request(context.Background(), checkoutKey, ask(v1alpha3.ApplyReasonRefusal))
	require.NoError(t, err)
	follow(t, flux, &stamped)

	assert.Equal(t, []string{"flux-system/checkout main@sha1:" + testCommit}, sim.Applied(),
		"B ignored the artifact A's request fetched")
	assert.True(t, suspended(sim.Kustomization(checkoutKey)))
}

// prefetchFixture is a dispatcher whose FluxTrigger "checkout" publishes source flux-system/platform
// with prefetch, over a cluster whose GitRepository is behind.
func prefetchFixture(t *testing.T, prefetch bool, extra ...client.Object) *dispatcherFixture {
	t.Helper()
	f := newDispatcherFixture(t, []client.Object{gitTarget("a", checkoutRef(), "")}, behindSource()...)
	setTriggerSource(t, f, "checkout", prefetch, nil)
	for _, obj := range extra {
		require.NoError(t, f.control.Create(context.Background(), obj))
	}
	return f
}

// setTriggerSource writes a FluxTrigger's published source, and its operation.
func setTriggerSource(t *testing.T, f *dispatcherFixture, name string, prefetch bool,
	operation *v1alpha3.ApplyOperation) {
	t.Helper()
	trigger := &v1alpha3.FluxTrigger{}
	require.NoError(t, f.control.Get(context.Background(), client.ObjectKey{Namespace: targetNS, Name: name}, trigger))
	trigger.Status.Source = &v1alpha3.FluxTriggerSource{
		GitRepository: fluxmeta.NamespacedObjectReference{Namespace: fluxSystemNS, Name: "platform"},
		URL:           testRepoURL, Branch: testBranch, Prefetch: prefetch,
	}
	trigger.Status.Operation = operation
	require.NoError(t, f.control.Update(context.Background(), trigger))
}

func prefetchRequest(commit string) git.ApplyRequest {
	return git.ApplyRequest{Branch: testBranchKey, Applier: checkout, Commit: commit}
}

// TestDispatcher_PrefetchFetchesTheSourceOnlyWhenItMay: the source is stamped when every consumer is
// held and nothing on it is in flight, and left alone otherwise.
func TestDispatcher_PrefetchFetchesTheSourceOnlyWhenItMay(t *testing.T) {
	f := prefetchFixture(t, true)
	require.NoError(t, f.dispatcher.prefetch(context.Background(), prefetchRequest(testCommit)))
	assert.Equal(t, []string{"gitrepositories " + fluxSystemNS}, f.patches(), "fetched; nothing applied")

	held := prefetchFixture(t, false)
	require.ErrorIs(t, held.dispatcher.prefetch(context.Background(), prefetchRequest(testCommit)), errPrefetchHeld,
		"a source another consumer applies on its own is never prefetched")
	assert.Empty(t, held.patches())

	busy := prefetchFixture(t, true, &v1alpha3.FluxTrigger{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "infra"},
		Spec: v1alpha3.FluxTriggerSpec{
			ClusterProviderRef: &fluxmeta.LocalObjectReference{Name: v1alpha3.DefaultClusterProviderName},
			KustomizationRef:   fluxmeta.NamespacedObjectReference{Namespace: fluxSystemNS, Name: "infra"},
		},
		Status: v1alpha3.FluxTriggerStatus{
			Source: &v1alpha3.FluxTriggerSource{
				GitRepository: fluxmeta.NamespacedObjectReference{Namespace: fluxSystemNS, Name: "platform"},
			},
			Operation: &v1alpha3.ApplyOperation{Phase: v1alpha3.ApplyOperationRunning, Token: "T1"},
		},
	})
	require.ErrorIs(t, busy.dispatcher.prefetch(context.Background(), prefetchRequest(testCommit)), errPrefetchHeld,
		"not while a consumer of the source is resumed")
	assert.Empty(t, busy.patches())

	current := prefetchFixture(t, true)
	require.ErrorIs(t, current.dispatcher.prefetch(context.Background(), prefetchRequest(olderCommit)),
		errNotPrefetched, "the artifact already holds it")
	assert.Empty(t, current.patches())
}

var _ runtime.Object = (*unstructured.Unstructured)(nil)
