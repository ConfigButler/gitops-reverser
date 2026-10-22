// SPDX-License-Identifier: Apache-2.0

package watch

import (
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8stypes "k8s.io/apimachinery/pkg/types"

	configv1alpha3 "github.com/ConfigButler/gitops-reverser/api/v1alpha3"
	"github.com/ConfigButler/gitops-reverser/internal/types"
)

// TestManager_AuditRouteForCluster covers the read side of the Declare-time capture. The watch
// manager holds no Kubernetes client for KubeProviders, so the controller hands it the route and
// this is where an event's author lookup picks it up.
func TestManager_AuditRouteForCluster(t *testing.T) {
	m := &Manager{}
	const delegating, prod = "platform/srcns-delegating", "team-a/prod-eu-1"

	// Nothing captured yet: a lookup racing the first Declare must read the provider's default route,
	// <namespace>.<name>, which is exactly what AuditRoute() resolves when none is set. Anything else
	// would key the read to a partition the handler never writes.
	assert.Equal(t, "platform.srcns-delegating", m.auditRouteForCluster(delegating))

	m.rememberClusterAuditRoute(delegating, "platform.local")
	assert.Equal(t, "platform.local", m.auditRouteForCluster(delegating))

	// An empty route never overwrites a captured one: a Declare that could not resolve the provider
	// passes the fallback, and that must not clobber a route already learned.
	m.rememberClusterAuditRoute(delegating, "")
	assert.Equal(t, "platform.local", m.auditRouteForCluster(delegating))

	// A different cluster keeps its own route, so the partition still separates clusters.
	m.rememberClusterAuditRoute(prod, "prod-eu-1")
	assert.Equal(t, "prod-eu-1", m.auditRouteForCluster(prod))
}

// TestManager_AuditRouteForCluster_FallbackSeparatesNamespaces: two providers sharing a name in two
// namespaces are two clusters, and their uncaptured default routes differ too, so neither tenant
// reads the other's facts while a Declare is still in flight.
func TestManager_AuditRouteForCluster_FallbackSeparatesNamespaces(t *testing.T) {
	m := &Manager{}
	teamA := SourceClusterID(k8stypes.NamespacedName{Namespace: "team-a", Name: "prod"})
	teamB := SourceClusterID(k8stypes.NamespacedName{Namespace: "team-b", Name: "prod"})

	assert.Equal(t, "team-a.prod", m.auditRouteForCluster(teamA))
	assert.Equal(t, "team-b.prod", m.auditRouteForCluster(teamB))
	assert.Equal(t,
		configv1alpha3.DefaultAuditRoute(k8stypes.NamespacedName{Namespace: "team-a", Name: "prod"}),
		m.auditRouteForCluster(teamA), "the fallback is the API's own default")
}

// TestManager_AuditRouteDiesWithItsCluster pins the teardown: a route captured for a cluster is
// dropped when the last GitTarget mirroring from it goes away, so a provider recreated against a
// different route cannot inherit its predecessor's partition.
func TestManager_AuditRouteDiesWithItsCluster(t *testing.T) {
	m := &Manager{Log: logr.Discard()}
	gitDest := types.NewResourceReference("target", "ns").WithUID("uid-1")
	const cluster = "team-a/prod-eu-1"

	m.rememberGitTargetCluster(gitDest, cluster)
	m.rememberClusterAuditRoute(cluster, "shared-route")
	require.Equal(t, "shared-route", m.auditRouteForCluster(cluster))

	m.forgetGitTargetCluster(gitDest)
	assert.Equal(t, "team-a.prod-eu-1", m.auditRouteForCluster(cluster),
		"the captured route must die with its cluster, leaving the provider's default route")
}
