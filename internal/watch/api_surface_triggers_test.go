// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// aggregatingDiscovery is the common test surface plus a served apiregistration.k8s.io/v1,
// i.e. an ordinary kube-apiserver rather than a control plane without an aggregation layer.
func aggregatingDiscovery() staticCatalogDiscovery {
	disco := newCommonTestDiscovery()
	disco.groups = append(disco.groups, testAPIGroup("apiregistration.k8s.io", "v1"))
	disco.resources = append(disco.resources, &metav1.APIResourceList{
		GroupVersion: "apiregistration.k8s.io/v1",
		APIResources: []metav1.APIResource{
			{Name: "apiservices", Kind: "APIService", Verbs: metav1.Verbs{"get", "list", "watch"}},
		},
	})
	return disco
}

func TestAPIResourceCatalog_ServesWatchable(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		discovery staticCatalogDiscovery
		gvr       schema.GroupVersionResource
		want      bool
	}{
		"crd served on a stock apiserver": {
			discovery: newCommonTestDiscovery(),
			gvr:       crdTriggerGVR(),
			want:      true,
		},
		"apiservices absent without an aggregation layer": {
			discovery: newCommonTestDiscovery(),
			gvr:       apiServiceTriggerGVR(),
			want:      false,
		},
		"apiservices served when the group is aggregated": {
			discovery: aggregatingDiscovery(),
			gvr:       apiServiceTriggerGVR(),
			want:      true,
		},
		"served but not watchable": {
			discovery: staticCatalogDiscovery{
				groups: []*metav1.APIGroup{testAPIGroup("apiregistration.k8s.io", "v1")},
				resources: []*metav1.APIResourceList{{
					GroupVersion: "apiregistration.k8s.io/v1",
					APIResources: []metav1.APIResource{
						{Name: "apiservices", Kind: "APIService", Verbs: metav1.Verbs{"get", "list"}},
					},
				}},
			},
			gvr:  apiServiceTriggerGVR(),
			want: false,
		},
		"unknown resource": {
			discovery: newCommonTestDiscovery(),
			gvr:       schema.GroupVersionResource{Group: "nope.example.com", Version: "v1", Resource: "widgets"},
			want:      false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			catalog := NewAPIResourceCatalog()
			_, err := catalog.Refresh(tc.discovery)
			require.NoError(t, err)
			require.Equal(t, tc.want, catalog.ServesWatchable(tc.gvr))
		})
	}
}

func TestAPIResourceCatalog_ServesWatchable_BeforeFirstScan(t *testing.T) {
	t.Parallel()
	// A catalog that has never accepted a trusted scan must not claim anything is
	// watchable, or the manager would open an informer on an API surface it has not seen.
	require.False(t, NewAPIResourceCatalog().ServesWatchable(crdTriggerGVR()))
}

func TestSelectAPISurfaceTriggers_SkipsUnservedAPIServices(t *testing.T) {
	t.Parallel()

	catalog := newCommonTestCatalog(t)
	start, unserved := selectAPISurfaceTriggers(catalog, map[schema.GroupVersionResource]struct{}{})

	require.Equal(t, []schema.GroupVersionResource{crdTriggerGVR()}, start,
		"only the served CRD trigger should start")
	require.Equal(t, []schema.GroupVersionResource{apiServiceTriggerGVR()}, unserved,
		"apiservices must be reported unserved, not started blindly")
}

func TestSelectAPISurfaceTriggers_StartsBothWhenAggregationIsServed(t *testing.T) {
	t.Parallel()

	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(aggregatingDiscovery())
	require.NoError(t, err)

	start, unserved := selectAPISurfaceTriggers(catalog, map[schema.GroupVersionResource]struct{}{})

	require.ElementsMatch(t, apiSurfaceTriggerGVRs(), start)
	require.Empty(t, unserved)
}

func TestSelectAPISurfaceTriggers_NeverRestartsARunningInformer(t *testing.T) {
	t.Parallel()

	catalog := NewAPIResourceCatalog()
	_, err := catalog.Refresh(aggregatingDiscovery())
	require.NoError(t, err)

	started := map[schema.GroupVersionResource]struct{}{crdTriggerGVR(): {}}
	start, unserved := selectAPISurfaceTriggers(catalog, started)

	require.Equal(t, []schema.GroupVersionResource{apiServiceTriggerGVR()}, start)
	require.Empty(t, unserved)
}

// An aggregation layer installed after startup must be picked up on the next catalog
// refresh, without restarting the operator.
func TestSelectAPISurfaceTriggers_PicksUpAggregationInstalledLater(t *testing.T) {
	t.Parallel()

	catalog := newCommonTestCatalog(t)
	started := map[schema.GroupVersionResource]struct{}{}

	start, unserved := selectAPISurfaceTriggers(catalog, started)
	require.Equal(t, []schema.GroupVersionResource{crdTriggerGVR()}, start)
	require.Equal(t, []schema.GroupVersionResource{apiServiceTriggerGVR()}, unserved)
	for _, gvr := range start {
		started[gvr] = struct{}{}
	}

	_, err := catalog.Refresh(aggregatingDiscovery())
	require.NoError(t, err)

	start, unserved = selectAPISurfaceTriggers(catalog, started)
	require.Equal(t, []schema.GroupVersionResource{apiServiceTriggerGVR()}, start)
	require.Empty(t, unserved)
}

func TestEnsureAPISurfaceTriggerInformers_NoOpBeforeStart(t *testing.T) {
	t.Parallel()

	// triggerCtx is nil until Start runs. Calling in through a controller-driven catalog
	// refresh before then must not create a factory bound to a dead context.
	m := &Manager{Log: logr.Discard()}
	m.ensureAPISurfaceTriggerInformers(m.Log)

	require.Nil(t, m.triggerFactory)
	require.Empty(t, m.triggersStarted)
}
