// SPDX-License-Identifier: Apache-2.0

package applier

import (
	"context"
	"slices"
	"testing"

	fluxmeta "github.com/fluxcd/pkg/apis/meta"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
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

// helmChartObj is a HelmChart built from the GitRepository sourceName in its own namespace.
func helmChartObj(namespace, name, sourceName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1", "kind": kindHelmChart,
		"metadata": map[string]any{"namespace": namespace, "name": name},
		"spec": map[string]any{
			"chart": "./charts/app", "sourceRef": map[string]any{"kind": kindGitRepository, "name": sourceName},
		},
	}}
}

// chartRefRelease is a HelmRelease whose spec.chartRef names an object of kind, in its own
// namespace unless refNamespace is set.
func chartRefRelease(namespace, name, kind, refName, refNamespace string) *unstructured.Unstructured {
	chartRef := map[string]any{"kind": kind, "name": refName}
	if refNamespace != "" {
		chartRef["namespace"] = refNamespace
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "helm.toolkit.fluxcd.io/v2", "kind": kindHelmRelease,
		"metadata": map[string]any{"namespace": namespace, "name": name},
		"spec":     map[string]any{"chartRef": chartRef},
	}}
}

// helmClient is a source cluster that serves HelmReleases and HelmCharts at every version Flux
// served them at, except those listed in unserved, which answer a list with NotFound as an API
// server does for a version it does not serve.
func helmClient(unserved []schema.GroupVersionResource, objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	listKinds := map[schema.GroupVersionResource]string{kustomizationGVR(): "KustomizationList"}
	for _, gvr := range everyVersion(helmReleaseGVR(), olderHelmReleaseVersions()) {
		listKinds[gvr] = "HelmReleaseList"
	}
	for _, gvr := range everyVersion(helmChartGVR(), olderHelmChartVersions()) {
		listKinds[gvr] = "HelmChartList"
	}
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, objs...)
	client.PrependReactor("list", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if slices.Contains(unserved, action.GetResource()) {
			return true, nil, apierrors.NewNotFound(action.GetResource().GroupResource(), "")
		}
		return false, nil, nil
	})
	return client
}

// everyVersion is gvr at its own version and at each of older.
func everyVersion(gvr schema.GroupVersionResource, older []string) []schema.GroupVersionResource {
	gvrs := []schema.GroupVersionResource{gvr}
	for _, version := range older {
		gvr.Version = version
		gvrs = append(gvrs, gvr)
	}
	return gvrs
}

// TestFluxConsumers_AHelmReleaseThroughAHelmChart is the fourth review's finding J: a HelmRelease
// whose spec.chartRef names a HelmChart built from the source applies it too.
func TestFluxConsumers_AHelmReleaseThroughAHelmChart(t *testing.T) {
	client := helmClient(nil,
		helmChartObj(fluxSystemNS, "podinfo", "platform"),
		helmChartObj(fluxSystemNS, "elsewhere", "another"),
		chartRefRelease("apps", "podinfo", kindHelmChart, "podinfo", fluxSystemNS),
		chartRefRelease(fluxSystemNS, "local", kindHelmChart, "podinfo", ""),
		chartRefRelease("apps", "other-source", kindHelmChart, "elsewhere", fluxSystemNS),
		chartRefRelease("apps", "missing-chart", kindHelmChart, "podinfo", ""),
		chartRefRelease("apps", "oci", "OCIRepository", "podinfo", fluxSystemNS),
	)
	consumers, err := pinnedFlux(client).Consumers(context.Background(), platformKey)
	require.NoError(t, err)
	assert.Equal(t, []fluxmeta.NamespacedObjectKindReference{
		{Kind: kindHelmRelease, Namespace: "apps", Name: "podinfo"},
		{Kind: kindHelmRelease, Namespace: fluxSystemNS, Name: "local"},
	}, consumers)
}

// TestFluxConsumers_AClusterServingOnlyAnOlderHelmRelease: a Flux that serves HelmReleases only at
// v2beta2 has them listed there, not read as having none.
func TestFluxConsumers_AClusterServingOnlyAnOlderHelmRelease(t *testing.T) {
	release := helmReleaseObj("apps", "podinfo", "platform", fluxSystemNS)
	release.SetAPIVersion("helm.toolkit.fluxcd.io/v2beta2")
	client := helmClient([]schema.GroupVersionResource{helmReleaseGVR()}, release)

	consumers, err := pinnedFlux(client).Consumers(context.Background(), platformKey)
	require.NoError(t, err)
	assert.Equal(t, []fluxmeta.NamespacedObjectKindReference{
		{Kind: kindHelmRelease, Namespace: "apps", Name: "podinfo"},
	}, consumers)
}

// TestFluxConsumers_NoHelmReleaseAPIIsNoneOnlyWithoutAChart: a cluster that serves HelmReleases at
// no version has none, unless a HelmChart is built from the source: then a release at some other
// version may apply it, and the consumers are unknown.
func TestFluxConsumers_NoHelmReleaseAPIIsNoneOnlyWithoutAChart(t *testing.T) {
	unserved := everyVersion(helmReleaseGVR(), olderHelmReleaseVersions())
	ks := kustomizationObj(fluxSystemNS, "checkout", gitRepoSourceRef("platform"))

	consumers, err := pinnedFlux(helmClient(unserved, ks)).Consumers(context.Background(), platformKey)
	require.NoError(t, err, "no Helm at all: none")
	assert.Equal(t, []fluxmeta.NamespacedObjectKindReference{
		{Kind: kindKustomization, Namespace: fluxSystemNS, Name: "checkout"},
	}, consumers)

	_, err = pinnedFlux(helmClient(unserved, ks, helmChartObj(fluxSystemNS, "podinfo", "platform"))).
		Consumers(context.Background(), platformKey)
	require.Error(t, err, "a chart is built from the source, by a release that cannot be listed")
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
