// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestClusterProvider_AuditRoute covers the defaulting rule the whole feature rests on: an unset
// auditRoute resolves to the provider's own name, which is what every install already partitions its
// facts by, so adding the field changes nothing until someone sets it.
func TestClusterProvider_AuditRoute(t *testing.T) {
	tests := []struct {
		name     string
		provider ClusterProvider
		want     string
	}{
		{
			name:     "no attribution block falls back to the object name",
			provider: ClusterProvider{ObjectMeta: metav1.ObjectMeta{Name: "prod-eu-1"}},
			want:     "prod-eu-1",
		},
		{
			name: "an empty auditRoute falls back to the object name",
			provider: ClusterProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "prod-eu-1"},
				Spec:       ClusterProviderSpec{Attribution: &ClusterProviderAttribution{}},
			},
			want: "prod-eu-1",
		},
		{
			name: "a declared route wins over the object name",
			provider: ClusterProvider{
				ObjectMeta: metav1.ObjectMeta{Name: "tenant-acme-delegating"},
				Spec: ClusterProviderSpec{
					Attribution: &ClusterProviderAttribution{AuditRoute: "prod-eu-1"},
				},
			},
			want: "prod-eu-1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.provider.AuditRoute())
		})
	}
}

// TestClusterProvider_AuditRoute_SharedBySeveralProviders is the reported bug, stated as an API
// invariant. An API server has one audit webhook backend and posts under one route, so two
// ClusterProviders naming one physical cluster can only both resolve authors if they agree on that
// route. Before this field they could not: each read under its own name, and every commit through
// the provider the API server did NOT post under was authored attribution-unresolved.
//
// Locality is deliberately not involved. Both providers here omit kubeConfig, which is what makes
// them in-cluster, and it is the declared route rather than that fact which joins them.
func TestClusterProvider_AuditRoute_SharedBySeveralProviders(t *testing.T) {
	fedByTheAPIServer := ClusterProvider{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	delegating := ClusterProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "srcns-delegating"},
		Spec: ClusterProviderSpec{
			AllowSourceNamespaceOverride: true,
			Attribution:                  &ClusterProviderAttribution{AuditRoute: "default"},
		},
	}

	assert.True(t, fedByTheAPIServer.IsInCluster())
	assert.True(t, delegating.IsInCluster())
	assert.Equal(t, fedByTheAPIServer.AuditRoute(), delegating.AuditRoute(),
		"two providers on one cluster must read one partition, or only the routed one is attributed")

	// A provider on a genuinely different cluster keeps its own partition, so the join can never
	// cross-credit. This is what a declared route buys that keying by object UID alone would not:
	// an etcd-snapshot clone reproduces object UIDs exactly, and only a human-chosen name separates
	// the copy from its origin.
	clone := ClusterProvider{ObjectMeta: metav1.ObjectMeta{Name: "prod-restored"}}
	assert.NotEqual(t, fedByTheAPIServer.AuditRoute(), clone.AuditRoute())
}
