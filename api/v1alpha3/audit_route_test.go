// SPDX-License-Identifier: Apache-2.0

package v1alpha3

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// TestKubeProvider_AuditRoute_SharedBySeveralProviders is the reported bug, stated as an API
// invariant. An API server has one audit webhook backend and posts under one route, so two
// KubeProviders naming one physical cluster can only both resolve authors if they agree on that
// route. Each defaults to its own <namespace>.<name>, so without a declared route every commit
// through the provider the API server did NOT post under is authored attribution-unresolved.
//
// Locality is deliberately not involved. Both providers here set spec.inCluster, and it is the
// declared route rather than that fact which joins them. (The default route defaulting itself is
// TestKubeProvider_AuditRoute.)
func TestKubeProvider_AuditRoute_SharedBySeveralProviders(t *testing.T) {
	fedByTheAPIServer := KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "local"},
		Spec:       KubeProviderSpec{InCluster: &KubeProviderInCluster{}},
	}
	delegating := KubeProvider{
		ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "srcns-delegating"},
		Spec: KubeProviderSpec{
			InCluster: &KubeProviderInCluster{},
			AllowedGitTargets: []SourceGrant{
				{From: GrantFromSame, SourceNamespaces: []ScopeNamespace{"*"}},
			},
			Attribution: &KubeProviderAttribution{AuditRoute: "platform.local"},
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
	clone := KubeProvider{ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: "prod-restored"}}
	assert.NotEqual(t, fedByTheAPIServer.AuditRoute(), clone.AuditRoute())

	// One provider name in another namespace is another partition by default.
	sameName := KubeProvider{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-a", Name: "local"}}
	assert.NotEqual(t, fedByTheAPIServer.AuditRoute(), sameName.AuditRoute())
}
